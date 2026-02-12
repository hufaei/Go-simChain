package tcp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"time"

	"simchain-go/internal/identity"
	"simchain-go/internal/types"
)

// HandshakeOutbound initiates a handshake with a remote peer.
// It returns the peer's ID, ListenAddr, and PublicKey if successful.
func HandshakeOutbound(
	conn net.Conn,
	localID string,
	localPriv ed25519.PrivateKey,
	localPub ed25519.PublicKey,
	localListenAddr string,
	magic string,
	version int,
	maxMsgSize int,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (peerID string, peerListen string, peerPub ed25519.PublicKey, err error) {
	nonceA := make([]byte, 32)
	if _, err := rand.Read(nonceA); err != nil {
		return "", "", nil, err
	}
	if err := WriteFrame(conn, types.Message{
		Type:      types.MsgHello,
		From:      localID,
		Timestamp: time.Now().UnixMilli(),
		Payload: types.HelloPayload{
			Magic:      magic,
			Version:    version,
			PubKey:     []byte(localPub),
			ListenAddr: localListenAddr,
			Nonce:      nonceA,
			Timestamp:  time.Now().UnixMilli(),
		},
	}, maxMsgSize, writeTimeout); err != nil {
		return "", "", nil, err
	}
	msg, err := ReadFrame(conn, maxMsgSize, readTimeout)
	if err != nil {
		return "", "", nil, err
	}
	if msg.Type != types.MsgHelloAck {
		return "", "", nil, errors.New("expected HelloAck")
	}
	ack, ok := msg.Payload.(types.HelloAckPayload)
	if !ok {
		return "", "", nil, errors.New("invalid HelloAck payload")
	}
	if ack.Magic != magic || ack.Version != version {
		return "", "", nil, errors.New("magic/version mismatch")
	}
	if !bytesEq(ack.EchoNonce, nonceA) {
		return "", "", nil, errors.New("hello echo nonce mismatch")
	}
	if len(ack.PubKey) != ed25519.PublicKeySize || len(ack.Nonce) == 0 {
		return "", "", nil, errors.New("invalid responder pubkey/nonce")
	}
	peerPub = ed25519.PublicKey(append([]byte(nil), ack.PubKey...))
	if !ed25519.Verify(peerPub, handshakeSigMsg(nonceA, magic, version), ack.SigOverEcho) {
		return "", "", nil, errors.New("responder signature invalid")
	}
	peerID, err = identity.NodeIDFromPublicKey(peerPub)
	if err != nil {
		return "", "", nil, err
	}
	peerListen = ack.ListenAddr

	sig := ed25519.Sign(localPriv, handshakeSigMsg(ack.Nonce, magic, version))
	if err := WriteFrame(conn, types.Message{
		Type:      types.MsgAuth,
		From:      localID,
		To:        peerID,
		Timestamp: time.Now().UnixMilli(),
		Payload: types.AuthPayload{
			EchoNonce: ack.Nonce,
			Sig:       sig,
			Timestamp: time.Now().UnixMilli(),
		},
	}, maxMsgSize, writeTimeout); err != nil {
		return "", "", nil, err
	}
	return peerID, peerListen, peerPub, nil
}

// HandshakeInbound handles an incoming handshake from a remote peer.
func HandshakeInbound(
	conn net.Conn,
	localID string,
	localPriv ed25519.PrivateKey,
	localPub ed25519.PublicKey,
	localListenAddr string,
	magic string,
	version int,
	maxMsgSize int,
	readTimeout time.Duration,
	writeTimeout time.Duration,
) (peerID string, peerListen string, peerPub ed25519.PublicKey, err error) {
	msg, err := ReadFrame(conn, maxMsgSize, readTimeout)
	if err != nil {
		return "", "", nil, err
	}
	if msg.Type != types.MsgHello {
		return "", "", nil, errors.New("expected Hello")
	}
	hello, ok := msg.Payload.(types.HelloPayload)
	if !ok {
		return "", "", nil, errors.New("invalid Hello payload")
	}
	if hello.Magic != magic || hello.Version != version {
		return "", "", nil, errors.New("magic/version mismatch")
	}
	if len(hello.PubKey) != ed25519.PublicKeySize || len(hello.Nonce) == 0 {
		return "", "", nil, errors.New("invalid initiator pubkey/nonce")
	}
	peerPub = ed25519.PublicKey(append([]byte(nil), hello.PubKey...))
	peerID, err = identity.NodeIDFromPublicKey(peerPub)
	if err != nil {
		return "", "", nil, err
	}
	peerListen = hello.ListenAddr

	nonceB := make([]byte, 32)
	if _, err := rand.Read(nonceB); err != nil {
		return "", "", nil, err
	}
	sigB := ed25519.Sign(localPriv, handshakeSigMsg(hello.Nonce, magic, version))
	if err := WriteFrame(conn, types.Message{
		Type:      types.MsgHelloAck,
		From:      localID,
		To:        peerID,
		Timestamp: time.Now().UnixMilli(),
		Payload: types.HelloAckPayload{
			Magic:       magic,
			Version:     version,
			PubKey:      []byte(localPub),
			ListenAddr:  localListenAddr,
			Nonce:       nonceB,
			EchoNonce:   hello.Nonce,
			SigOverEcho: sigB,
			Timestamp:   time.Now().UnixMilli(),
		},
	}, maxMsgSize, writeTimeout); err != nil {
		return "", "", nil, err
	}

	authMsg, err := ReadFrame(conn, maxMsgSize, readTimeout)
	if err != nil {
		return "", "", nil, err
	}
	if authMsg.Type != types.MsgAuth {
		return "", "", nil, errors.New("expected Auth")
	}
	auth, ok := authMsg.Payload.(types.AuthPayload)
	if !ok {
		return "", "", nil, errors.New("invalid Auth payload")
	}
	if !bytesEq(auth.EchoNonce, nonceB) {
		return "", "", nil, errors.New("auth echo nonce mismatch")
	}
	if !ed25519.Verify(peerPub, handshakeSigMsg(nonceB, magic, version), auth.Sig) {
		return "", "", nil, errors.New("initiator signature invalid")
	}
	return peerID, peerListen, peerPub, nil
}

func handshakeSigMsg(nonce []byte, magic string, version int) []byte {
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], uint32(version))
	b := make([]byte, 0, len(nonce)+len(magic)+len(v))
	b = append(b, nonce...)
	b = append(b, []byte(magic)...)
	b = append(b, v[:]...)
	return b
}

func bytesEq(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Ensure readFrame/writeFrame are available or move them here?
// They are in tcp.go. Since they are private in package tcp, they are visible here.
// But context/rate imports in this file suggest I need them?
// readFrameWithLimit uses rate.Limiter which uses context.
// But Handshake functions don't use limiter (nil).
// So I don't need rate/context if I call readFrame(..., nil).
// readFrame is in tcp.go.
