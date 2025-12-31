package tcp

import (
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"testing"
	"time"

	"simchain-go/internal/identity"
	"simchain-go/internal/types"
)

func newTestIdentity(t *testing.T) *identity.Identity {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	id, err := identity.NodeIDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("node id: %v", err)
	}
	return &identity.Identity{NodeID: id, PublicKey: pub, PrivateKey: priv}
}

func TestFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	want := types.Message{
		Type:      types.MsgInvTx,
		From:      "x",
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.InvTxPayload{TxID: "tx-1"},
	}
	go func() {
		_ = writeFrame(a, want, 1<<20, 2*time.Second)
	}()

	got, err := readFrame(b, 1<<20, 2*time.Second)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Type != want.Type {
		t.Fatalf("type mismatch: got %q want %q", got.Type, want.Type)
	}
	pl, ok := got.Payload.(types.InvTxPayload)
	if !ok {
		t.Fatalf("payload type: %T", got.Payload)
	}
	if pl.TxID != "tx-1" {
		t.Fatalf("txid mismatch: %q", pl.TxID)
	}
}

func TestHandshakePipeSuccess(t *testing.T) {
	initID := newTestIdentity(t)
	respID := newTestIdentity(t)

	initTr, err := New(Config{ListenAddr: "127.0.0.1:7001", Magic: "m", Version: 1, Identity: initID})
	if err != nil {
		t.Fatalf("init tr: %v", err)
	}
	respTr, err := New(Config{ListenAddr: "127.0.0.1:7002", Magic: "m", Version: 1, Identity: respID})
	if err != nil {
		t.Fatalf("resp tr: %v", err)
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	done := make(chan struct{})
	var inPeerID string
	go func() {
		defer close(done)
		pid, _, _, err := respTr.handshakeInbound(b)
		if err != nil {
			t.Errorf("inbound handshake: %v", err)
			return
		}
		inPeerID = pid
	}()

	outPeerID, _, _, err := initTr.handshakeOutbound(a)
	if err != nil {
		t.Fatalf("outbound handshake: %v", err)
	}
	<-done
	if outPeerID != respID.NodeID {
		t.Fatalf("out peer mismatch: got %q want %q", outPeerID, respID.NodeID)
	}
	if inPeerID != initID.NodeID {
		t.Fatalf("in peer mismatch: got %q want %q", inPeerID, initID.NodeID)
	}
}

func TestHandshakeRejectsMagicMismatch(t *testing.T) {
	initID := newTestIdentity(t)
	respID := newTestIdentity(t)

	initTr, err := New(Config{ListenAddr: "127.0.0.1:7001", Magic: "m1", Version: 1, Identity: initID})
	if err != nil {
		t.Fatalf("init tr: %v", err)
	}
	respTr, err := New(Config{ListenAddr: "127.0.0.1:7002", Magic: "m2", Version: 1, Identity: respID})
	if err != nil {
		t.Fatalf("resp tr: %v", err)
	}

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		_, _, _, err := respTr.handshakeInbound(b)
		done <- err
	}()

	_, _, _, err = initTr.handshakeOutbound(a)
	if err == nil {
		t.Fatalf("expected outbound handshake error")
	}
	<-done
}
