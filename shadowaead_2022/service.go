package shadowaead_2022

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"io"
	"math"
	"net"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-shadowsocks"
	"github.com/sagernet/sing-shadowsocks/shadowaead"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/cache"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/replay"
	"github.com/sagernet/sing/common/rw"
	"github.com/sagernet/sing/common/udpnat"
	wgReplay "golang.zx2c4.com/wireguard/replay"
)

var (
	ErrSaltNotUnique = E.New("bad request: salt not unique")
	ErrNoPadding     = E.New("bad request: missing payload or padding")
	ErrBadPadding    = E.New("bad request: damaged padding")
)

type Service struct {
	name          string
	keySaltLength int
	handler       shadowsocks.Handler

	constructor      func(key []byte) cipher.AEAD
	blockConstructor func(key []byte) cipher.Block
	udpCipher        cipher.AEAD
	udpBlockCipher   cipher.Block
	psk              []byte

	replayFilter replay.Filter
	udpNat       *udpnat.Service[uint64]
	udpSessions  *cache.LruCache[uint64, *serverUDPSession]
}

func NewServiceWithPassword(method string, password string, udpTimeout int64, handler shadowsocks.Handler) (shadowsocks.Service, error) {
	if password == "" {
		return nil, ErrMissingPSK
	}
	psk, err := base64.StdEncoding.DecodeString(password)
	if err != nil {
		return nil, E.Cause(err, "decode psk")
	}
	return NewService(method, psk, udpTimeout, handler)
}

func NewService(method string, psk []byte, udpTimeout int64, handler shadowsocks.Handler) (shadowsocks.Service, error) {
	s := &Service{
		name:    method,
		handler: handler,

		replayFilter: replay.NewSimple(60 * time.Second),
		udpNat:       udpnat.New[uint64](udpTimeout, handler),
		udpSessions: cache.New[uint64, *serverUDPSession](
			cache.WithAge[uint64, *serverUDPSession](udpTimeout),
			cache.WithUpdateAgeOnGet[uint64, *serverUDPSession](),
		),
	}

	switch method {
	case "2022-blake3-aes-128-gcm":
		s.keySaltLength = 16
		s.constructor = newAESGCM
		s.blockConstructor = newAES
	case "2022-blake3-aes-256-gcm":
		s.keySaltLength = 32
		s.constructor = newAESGCM
		s.blockConstructor = newAES
	case "2022-blake3-chacha20-poly1305":
		s.keySaltLength = 32
		s.constructor = newChacha20Poly1305
	default:
		return nil, os.ErrInvalid
	}

	if len(psk) != s.keySaltLength {
		if len(psk) < s.keySaltLength {
			return nil, shadowsocks.ErrBadKey
		} else if len(psk) > s.keySaltLength {
			psk = Key(psk, s.keySaltLength)
		} else {
			return nil, ErrMissingPSK
		}
	}

	switch method {
	case "2022-blake3-aes-128-gcm", "2022-blake3-aes-256-gcm":
		s.udpBlockCipher = newAES(psk)
	case "2022-blake3-chacha20-poly1305":
		s.udpCipher = newXChacha20Poly1305(psk)
	}

	s.psk = psk
	return s, nil
}

func (s *Service) NewConnection(ctx context.Context, conn net.Conn, metadata M.Metadata) error {
	err := s.newConnection(ctx, conn, metadata)
	if err != nil {
		err = &shadowsocks.ServerConnError{Conn: conn, Source: metadata.Source, Cause: err}
	}
	return err
}

func (s *Service) newConnection(ctx context.Context, conn net.Conn, metadata M.Metadata) error {
	header := buf.Make(s.keySaltLength + shadowaead.Overhead + RequestHeaderFixedChunkLength)

	n, err := conn.Read(header)
	if err != nil {
		return E.Cause(err, "read header")
	} else if n < len(header) {
		return shadowaead.ErrBadHeader
	}

	requestSalt := header[:s.keySaltLength]

	if !s.replayFilter.Check(requestSalt) {
		return ErrSaltNotUnique
	}

	requestKey := SessionKey(s.psk, requestSalt, s.keySaltLength)
	reader := shadowaead.NewReader(
		conn,
		s.constructor(common.Dup(requestKey)),
		MaxPacketSize,
	)
	runtime.KeepAlive(requestKey)

	err = reader.ReadChunk(header[s.keySaltLength:])
	if err != nil {
		return err
	}

	headerType, err := reader.ReadByte()
	if err != nil {
		return E.Cause(err, "read header")
	}

	if headerType != HeaderTypeClient {
		return E.Extend(ErrBadHeaderType, "expected ", HeaderTypeClient, ", got ", headerType)
	}

	var epoch uint64
	err = binary.Read(reader, binary.BigEndian, &epoch)
	if err != nil {
		return err
	}

	diff := int(math.Abs(float64(time.Now().Unix() - int64(epoch))))
	if diff > 30 {
		return E.Extend(ErrBadTimestamp, "received ", epoch, ", diff ", diff, "s")
	}

	var length uint16
	err = binary.Read(reader, binary.BigEndian, &length)
	if err != nil {
		return err
	}

	err = reader.ReadWithLength(length)
	if err != nil {
		return err
	}

	destination, err := M.SocksaddrSerializer.ReadAddrPort(reader)
	if err != nil {
		return err
	}

	var paddingLen uint16
	err = binary.Read(reader, binary.BigEndian, &paddingLen)
	if err != nil {
		return err
	}

	if uint16(reader.Cached()) < paddingLen {
		return ErrNoPadding
	}

	if paddingLen > 0 {
		err = reader.Discard(int(paddingLen))
		if err != nil {
			return E.Cause(err, "discard padding")
		}
	} else if reader.Cached() == 0 {
		return ErrNoPadding
	}

	metadata.Protocol = "shadowsocks"
	metadata.Destination = destination
	return s.handler.NewConnection(ctx, &serverConn{
		Service:     s,
		Conn:        conn,
		uPSK:        s.psk,
		reader:      reader,
		requestSalt: requestSalt,
	}, metadata)
}

type serverConn struct {
	*Service
	net.Conn
	uPSK        []byte
	access      sync.Mutex
	reader      *shadowaead.Reader
	writer      *shadowaead.Writer
	requestSalt []byte
}

func (c *serverConn) writeResponse(payload []byte) (n int, err error) {
	_salt := buf.Make(c.keySaltLength)
	salt := common.Dup(_salt[:])
	common.Must1(io.ReadFull(rand.Reader, salt))
	key := SessionKey(c.uPSK, salt, c.keySaltLength)
	runtime.KeepAlive(_salt)
	writer := shadowaead.NewWriter(
		c.Conn,
		c.constructor(common.Dup(key)),
		MaxPacketSize,
	)
	runtime.KeepAlive(key)
	header := writer.Buffer()
	header.Write(salt)

	_headerFixedChunk := buf.Make(1 + 8 + c.keySaltLength + 2)
	headerFixedChunk := buf.With(common.Dup(_headerFixedChunk))
	common.Must(headerFixedChunk.WriteByte(HeaderTypeServer))
	common.Must(binary.Write(headerFixedChunk, binary.BigEndian, uint64(time.Now().Unix())))
	common.Must1(headerFixedChunk.Write(c.requestSalt))
	common.Must(binary.Write(headerFixedChunk, binary.BigEndian, uint16(len(payload))))

	writer.WriteChunk(header, headerFixedChunk.Slice())
	runtime.KeepAlive(_headerFixedChunk)
	c.requestSalt = nil

	if len(payload) > 0 {
		writer.WriteChunk(header, payload)
	}

	err = writer.BufferedWriter(header.Len()).Flush()
	if err != nil {
		return
	}

	c.writer = writer
	n = len(payload)
	return
}

func (c *serverConn) Write(p []byte) (n int, err error) {
	if c.writer != nil {
		return c.writer.Write(p)
	}
	c.access.Lock()
	if c.writer != nil {
		c.access.Unlock()
		return c.writer.Write(p)
	}
	defer c.access.Unlock()
	return c.writeResponse(p)
}

func (c *serverConn) ReadFrom(r io.Reader) (n int64, err error) {
	if c.writer == nil {
		return rw.ReadFrom0(c, r)
	}
	return c.writer.ReadFrom(r)
}

func (c *serverConn) WriteTo(w io.Writer) (n int64, err error) {
	return c.reader.WriteTo(w)
}

func (c *serverConn) Upstream() any {
	return c.Conn
}

func (s *Service) NewPacket(ctx context.Context, conn N.PacketConn, buffer *buf.Buffer, metadata M.Metadata) error {
	err := s.newPacket(ctx, conn, buffer, metadata)
	if err != nil {
		err = &shadowsocks.ServerPacketError{Source: metadata.Source, Cause: err}
	}
	return err
}

func (s *Service) newPacket(ctx context.Context, conn N.PacketConn, buffer *buf.Buffer, metadata M.Metadata) error {
	var packetHeader []byte
	if s.udpCipher != nil {
		_, err := s.udpCipher.Open(buffer.Index(PacketNonceSize), buffer.To(PacketNonceSize), buffer.From(PacketNonceSize), nil)
		if err != nil {
			return E.Cause(err, "decrypt packet header")
		}
		buffer.Advance(PacketNonceSize)
		buffer.Truncate(buffer.Len() - shadowaead.Overhead)
	} else {
		packetHeader = buffer.To(aes.BlockSize)
		s.udpBlockCipher.Decrypt(packetHeader, packetHeader)
	}

	var sessionId, packetId uint64
	err := binary.Read(buffer, binary.BigEndian, &sessionId)
	if err != nil {
		return err
	}
	err = binary.Read(buffer, binary.BigEndian, &packetId)
	if err != nil {
		return err
	}

	session, loaded := s.udpSessions.LoadOrStore(sessionId, s.newUDPSession)
	if !loaded {
		session.remoteSessionId = sessionId
		if packetHeader != nil {
			key := SessionKey(s.psk, packetHeader[:8], s.keySaltLength)
			session.remoteCipher = s.constructor(common.Dup(key))
			runtime.KeepAlive(key)
		}
	}
	goto process

returnErr:
	if !loaded {
		s.udpSessions.Delete(sessionId)
	}
	return err

process:
	if !session.filter.ValidateCounter(packetId, math.MaxUint64) {
		err = ErrPacketIdNotUnique
		goto returnErr
	}

	if packetHeader != nil {
		_, err = session.remoteCipher.Open(buffer.Index(0), packetHeader[4:16], buffer.Bytes(), nil)
		if err != nil {
			err = E.Cause(err, "decrypt packet")
			goto returnErr
		}
		buffer.Truncate(buffer.Len() - shadowaead.Overhead)
	}

	var headerType byte
	headerType, err = buffer.ReadByte()
	if err != nil {
		err = E.Cause(err, "decrypt packet")
		goto returnErr
	}
	if headerType != HeaderTypeClient {
		err = E.Extend(ErrBadHeaderType, "expected ", HeaderTypeClient, ", got ", headerType)
		goto returnErr
	}

	var epoch uint64
	err = binary.Read(buffer, binary.BigEndian, &epoch)
	if err != nil {
		goto returnErr
	}
	diff := int(math.Abs(float64(time.Now().Unix() - int64(epoch))))
	if diff > 30 {
		err = E.Extend(ErrBadTimestamp, "received ", epoch, ", diff ", diff, "s")
		goto returnErr
	}

	var paddingLength uint16
	err = binary.Read(buffer, binary.BigEndian, &paddingLength)
	if err != nil {
		err = E.Cause(err, "read padding length")
		goto returnErr
	}
	buffer.Advance(int(paddingLength))

	destination, err := M.SocksaddrSerializer.ReadAddrPort(buffer)
	if err != nil {
		goto returnErr
	}
	metadata.Destination = destination

	session.remoteAddr = metadata.Source
	s.udpNat.NewPacket(ctx, sessionId, func() N.PacketWriter {
		return &serverPacketWriter{s, conn, session}
	}, buffer, metadata)
	return nil
}

type serverPacketWriter struct {
	*Service
	N.PacketConn
	session *serverUDPSession
}

func (w *serverPacketWriter) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	var hdrLen int
	if w.udpCipher != nil {
		hdrLen = PacketNonceSize
	}
	hdrLen += 16 // packet header
	hdrLen += 1  // header type
	hdrLen += 8  // timestamp
	hdrLen += 8  // remote session id
	hdrLen += 2  // padding length
	hdrLen += M.SocksaddrSerializer.AddrPortLen(destination)
	header := buf.With(buffer.ExtendHeader(hdrLen))

	var dataIndex int
	if w.udpCipher != nil {
		common.Must1(header.ReadFullFrom(w.session.rng, PacketNonceSize))
		dataIndex = PacketNonceSize
	} else {
		dataIndex = aes.BlockSize
	}

	common.Must(
		binary.Write(header, binary.BigEndian, w.session.sessionId),
		binary.Write(header, binary.BigEndian, w.session.nextPacketId()),
		header.WriteByte(HeaderTypeServer),
		binary.Write(header, binary.BigEndian, uint64(time.Now().Unix())),
		binary.Write(header, binary.BigEndian, w.session.remoteSessionId),
		binary.Write(header, binary.BigEndian, uint16(0)), // padding length
	)

	err := M.SocksaddrSerializer.WriteAddrPort(header, destination)
	if err != nil {
		return err
	}

	if w.udpCipher != nil {
		w.udpCipher.Seal(buffer.Index(dataIndex), buffer.To(dataIndex), buffer.From(dataIndex), nil)
		buffer.Extend(shadowaead.Overhead)
	} else {
		packetHeader := buffer.To(aes.BlockSize)
		w.session.cipher.Seal(buffer.Index(dataIndex), packetHeader[4:16], buffer.From(dataIndex), nil)
		buffer.Extend(shadowaead.Overhead)
		w.udpBlockCipher.Encrypt(packetHeader, packetHeader)
	}
	return w.PacketConn.WritePacket(buffer, w.session.remoteAddr)
}

type serverUDPSession struct {
	sessionId       uint64
	remoteSessionId uint64
	remoteAddr      M.Socksaddr
	packetId        uint64
	cipher          cipher.AEAD
	remoteCipher    cipher.AEAD
	filter          wgReplay.Filter
	rng             io.Reader
}

func (s *serverUDPSession) nextPacketId() uint64 {
	return atomic.AddUint64(&s.packetId, 1)
}

func (m *Service) newUDPSession() *serverUDPSession {
	session := &serverUDPSession{}
	if m.udpCipher != nil {
		session.rng = Blake3KeyedHash(rand.Reader)
		common.Must(binary.Read(session.rng, binary.BigEndian, &session.sessionId))
	} else {
		common.Must(binary.Read(rand.Reader, binary.BigEndian, &session.sessionId))
	}
	session.packetId--
	if m.udpCipher == nil {
		sessionId := make([]byte, 8)
		binary.BigEndian.PutUint64(sessionId, session.sessionId)
		key := SessionKey(m.psk, sessionId, m.keySaltLength)
		session.cipher = m.constructor(common.Dup(key))
		runtime.KeepAlive(key)
	}
	return session
}
