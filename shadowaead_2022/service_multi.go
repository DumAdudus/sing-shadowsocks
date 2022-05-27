package shadowaead_2022

import (
	"context"
	"crypto/aes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"math"
	"net"
	"os"
	"runtime"
	"time"

	"github.com/sagernet/sing-shadowsocks"
	"github.com/sagernet/sing-shadowsocks/shadowaead"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/buf"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/rw"
	"lukechampine.com/blake3"
)

type MultiService[U comparable] struct {
	*Service

	uPSK      map[U][]byte
	uPSKHash  map[U][aes.BlockSize]byte
	uPSKHashR map[[aes.BlockSize]byte]U
}

func NewMultiServiceWithPassword[U comparable](method string, password string, udpTimeout int64, handler shadowsocks.Handler) (*MultiService[U], error) {
	if password == "" {
		return nil, ErrMissingPSK
	}
	iPSK, err := base64.StdEncoding.DecodeString(password)
	if err != nil {
		return nil, E.Cause(err, "decode psk")
	}
	return NewMultiService[U](method, iPSK, udpTimeout, handler)
}

func NewMultiService[U comparable](method string, iPSK []byte, udpTimeout int64, handler shadowsocks.Handler) (*MultiService[U], error) {
	switch method {
	case "2022-blake3-aes-128-gcm":
	case "2022-blake3-aes-256-gcm":
	default:
		return nil, os.ErrInvalid
	}

	ss, err := NewService(method, iPSK, udpTimeout, handler)
	if err != nil {
		return nil, err
	}

	s := &MultiService[U]{
		Service: ss.(*Service),

		uPSK:      make(map[U][]byte),
		uPSKHash:  make(map[U][aes.BlockSize]byte),
		uPSKHashR: make(map[[aes.BlockSize]byte]U),
	}
	return s, nil
}

func (s *MultiService[U]) AddUser(user U, key []byte) error {
	if len(key) < s.keySaltLength {
		return shadowsocks.ErrBadKey
	} else if len(key) > s.keySaltLength {
		key = Key(key, s.keySaltLength)
	}

	var uPSKHash [aes.BlockSize]byte
	hash512 := blake3.Sum512(key)
	copy(uPSKHash[:], hash512[:])

	if oldHash, loaded := s.uPSKHash[user]; loaded {
		delete(s.uPSKHashR, oldHash)
	}

	s.uPSKHash[user] = uPSKHash
	s.uPSKHashR[uPSKHash] = user
	s.uPSK[user] = key

	return nil
}

func (s *MultiService[U]) AddUserWithPassword(user U, password string) error {
	if password == "" {
		return shadowsocks.ErrMissingPassword
	}
	psk, err := base64.StdEncoding.DecodeString(password)
	if err != nil {
		return E.Cause(err, "decode psk")
	}
	return s.AddUser(user, psk)
}

func (s *MultiService[U]) RemoveUser(user U) {
	if hash, loaded := s.uPSKHash[user]; loaded {
		delete(s.uPSKHashR, hash)
	}
	delete(s.uPSK, user)
	delete(s.uPSKHash, user)
}

func (s *MultiService[U]) NewConnection(ctx context.Context, conn net.Conn, metadata M.Metadata) error {
	err := s.newConnection(ctx, conn, metadata)
	if err != nil {
		err = &shadowsocks.ServerConnError{Conn: conn, Source: metadata.Source, Cause: err}
	}
	return err
}

func (s *MultiService[U]) newConnection(ctx context.Context, conn net.Conn, metadata M.Metadata) error {
	requestHeader := make([]byte, s.keySaltLength+aes.BlockSize+shadowaead.Overhead+RequestHeaderFixedChunkLength)
	n, err := conn.Read(requestHeader)
	if err != nil {
		return err
	} else if n < len(requestHeader) {
		return shadowaead.ErrBadHeader
	}
	requestSalt := requestHeader[:s.keySaltLength]
	if !s.replayFilter.Check(requestSalt) {
		return ErrSaltNotUnique
	}

	var _eiHeader [aes.BlockSize]byte
	eiHeader := common.Dup(_eiHeader[:])
	copy(eiHeader, requestHeader[s.keySaltLength:s.keySaltLength+aes.BlockSize])

	keyMaterial := buf.Make(s.keySaltLength * 2)
	copy(keyMaterial, s.psk)
	copy(keyMaterial[s.keySaltLength:], requestSalt)
	_identitySubkey := buf.Make(s.keySaltLength)
	identitySubkey := common.Dup(_identitySubkey)
	blake3.DeriveKey(identitySubkey, "shadowsocks 2022 identity subkey", keyMaterial)
	b, err := s.blockConstructor(identitySubkey)
	if err != nil {
		return err
	}
	b.Decrypt(eiHeader, eiHeader)
	runtime.KeepAlive(_identitySubkey)

	var user U
	var uPSK []byte
	if u, loaded := s.uPSKHashR[_eiHeader]; loaded {
		user = u
		uPSK = s.uPSK[u]
	} else {
		return E.New("invalid request")
	}
	runtime.KeepAlive(_eiHeader)

	requestKey := SessionKey(uPSK, requestSalt, s.keySaltLength)
	readCipher, err := s.constructor(common.Dup(requestKey))
	if err != nil {
		return err
	}
	reader := shadowaead.NewReader(
		conn,
		readCipher,
		MaxPacketSize,
	)

	err = reader.ReadChunk(requestHeader[s.keySaltLength+aes.BlockSize:])
	if err != nil {
		return err
	}

	headerType, err := rw.ReadByte(reader)
	if err != nil {
		return E.Cause(err, "read header")
	}

	if headerType != HeaderTypeClient {
		return E.Extend(ErrBadHeaderType, "expected ", HeaderTypeClient, ", got ", headerType)
	}

	var epoch uint64
	err = binary.Read(reader, binary.BigEndian, &epoch)
	if err != nil {
		return E.Cause(err, "read timestamp")
	}
	diff := int(math.Abs(float64(time.Now().Unix() - int64(epoch))))
	if diff > 30 {
		return E.Extend(ErrBadTimestamp, "received ", epoch, ", diff ", diff, "s")
	}
	var length uint16
	err = binary.Read(reader, binary.BigEndian, &length)
	if err != nil {
		return E.Cause(err, "read length")
	}

	err = reader.ReadWithLength(length)
	if err != nil {
		return err
	}

	destination, err := M.SocksaddrSerializer.ReadAddrPort(reader)
	if err != nil {
		return E.Cause(err, "read destination")
	}

	var paddingLen uint16
	err = binary.Read(reader, binary.BigEndian, &paddingLen)
	if err != nil {
		return E.Cause(err, "read padding length")
	}

	if reader.Cached() < int(paddingLen) {
		return ErrBadPadding
	} else if paddingLen > 0 {
		err = reader.Discard(int(paddingLen))
		if err != nil {
			return E.Cause(err, "discard padding")
		}
	} else if reader.Cached() == 0 {
		return ErrNoPadding
	}

	var userCtx shadowsocks.UserContext[U]
	userCtx.Context = ctx
	userCtx.User = user

	metadata.Protocol = "shadowsocks"
	metadata.Destination = destination
	return s.handler.NewConnection(&userCtx, &serverConn{
		Service:     s.Service,
		Conn:        conn,
		uPSK:        uPSK,
		reader:      reader,
		requestSalt: requestSalt,
	}, metadata)
}

func (s *MultiService[U]) NewPacket(ctx context.Context, conn N.PacketConn, buffer *buf.Buffer, metadata M.Metadata) error {
	err := s.newPacket(ctx, conn, buffer, metadata)
	if err != nil {
		err = &shadowsocks.ServerPacketError{Source: metadata.Source, Cause: err}
	}
	return err
}

func (s *MultiService[U]) newPacket(ctx context.Context, conn N.PacketConn, buffer *buf.Buffer, metadata M.Metadata) error {
	packetHeader := buffer.To(aes.BlockSize)
	s.udpBlockCipher.Decrypt(packetHeader, packetHeader)

	var _eiHeader [aes.BlockSize]byte
	eiHeader := common.Dup(_eiHeader[:])
	s.udpBlockCipher.Decrypt(eiHeader, buffer.Range(aes.BlockSize, 2*aes.BlockSize))

	for i := range eiHeader {
		eiHeader[i] = eiHeader[i] ^ packetHeader[i]
	}

	var user U
	var uPSK []byte
	if u, loaded := s.uPSKHashR[_eiHeader]; loaded {
		user = u
		uPSK = s.uPSK[u]
	} else {
		return E.New("invalid request")
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

	session, loaded := s.udpSessions.LoadOrStore(sessionId, func() *serverUDPSession {
		return s.newUDPSession(uPSK)
	})
	if !loaded {
		session.remoteSessionId = sessionId
		key := SessionKey(uPSK, packetHeader[:8], s.keySaltLength)
		session.remoteCipher, err = s.constructor(common.Dup(key))
		if err != nil {
			return err
		}
		runtime.KeepAlive(key)
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

	s.udpNat.NewContextPacket(ctx, sessionId, func() (context.Context, N.PacketWriter) {
		return &shadowsocks.UserContext[U]{
			ctx,
			user,
		}, &serverPacketWriter{s.Service, conn, session}
	}, buffer, metadata)
	return nil
}

func (s *MultiService[U]) newUDPSession(uPSK []byte) *serverUDPSession {
	session := &serverUDPSession{}
	if s.udpCipher != nil {
		session.rng = Blake3KeyedHash(rand.Reader)
		common.Must(binary.Read(session.rng, binary.BigEndian, &session.sessionId))
	} else {
		common.Must(binary.Read(rand.Reader, binary.BigEndian, &session.sessionId))
	}
	session.packetId--
	sessionId := make([]byte, 8)
	binary.BigEndian.PutUint64(sessionId, session.sessionId)
	key := SessionKey(uPSK, sessionId, s.keySaltLength)
	var err error
	session.cipher, err = s.constructor(common.Dup(key))
	common.Must(err)
	runtime.KeepAlive(key)
	return session
}
