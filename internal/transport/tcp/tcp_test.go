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
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("close a: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("close b: %v", err)
		}
	})

	want := types.Message{
		Type:      types.MsgInvTx,
		From:      "x",
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.InvTxPayload{TxID: "tx-1"},
	}
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- WriteFrame(a, want, 1<<20, 2*time.Second)
	}()

	got, err := ReadFrame(b, 1<<20, 2*time.Second)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatalf("write: %v", err)
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

	initTr, err := New(Config{ListenAddr: "127.0.0.1:7001", Magic: "m", Version: 1, Identity: initID, ReadTimeout: 800 * time.Millisecond, WriteTimeout: 800 * time.Millisecond})
	if err != nil {
		t.Fatalf("init tr: %v", err)
	}
	respTr, err := New(Config{ListenAddr: "127.0.0.1:7002", Magic: "m", Version: 1, Identity: respID, ReadTimeout: 800 * time.Millisecond, WriteTimeout: 800 * time.Millisecond})
	if err != nil {
		t.Fatalf("resp tr: %v", err)
	}

	a, b := net.Pipe()
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("close a: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("close b: %v", err)
		}
	})

	type res struct {
		peerID string
		err    error
	}
	done := make(chan res, 1)
	go func() {
		pid, _, _, err := HandshakeInbound(
			b, respTr.localID, respTr.cfg.Identity.PrivateKey, respTr.cfg.Identity.PublicKey,
			respTr.cfg.ListenAddr, respTr.cfg.Magic, respTr.cfg.Version,
			respTr.cfg.MaxMessageSize, respTr.cfg.ReadTimeout, respTr.cfg.WriteTimeout,
		)
		done <- res{peerID: pid, err: err}
	}()

	outPeerID, _, _, err := HandshakeOutbound(
		a, initTr.localID, initTr.cfg.Identity.PrivateKey, initTr.cfg.Identity.PublicKey,
		initTr.cfg.ListenAddr, initTr.cfg.Magic, initTr.cfg.Version,
		initTr.cfg.MaxMessageSize, initTr.cfg.ReadTimeout, initTr.cfg.WriteTimeout,
	)
	if err != nil {
		t.Fatalf("outbound handshake: %v", err)
	}
	in := <-done
	if in.err != nil {
		t.Fatalf("inbound handshake: %v", in.err)
	}
	if outPeerID != respID.NodeID {
		t.Fatalf("out peer mismatch: got %q want %q", outPeerID, respID.NodeID)
	}
	if in.peerID != initID.NodeID {
		t.Fatalf("in peer mismatch: got %q want %q", in.peerID, initID.NodeID)
	}
}

func TestHandshakeRejectsMagicMismatch(t *testing.T) {
	initID := newTestIdentity(t)
	respID := newTestIdentity(t)

	initTr, err := New(Config{ListenAddr: "127.0.0.1:7001", Magic: "m1", Version: 1, Identity: initID, ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("init tr: %v", err)
	}
	respTr, err := New(Config{ListenAddr: "127.0.0.1:7002", Magic: "m2", Version: 1, Identity: respID, ReadTimeout: 500 * time.Millisecond, WriteTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("resp tr: %v", err)
	}

	a, b := net.Pipe()
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("close a: %v", err)
		}
	})
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Errorf("close b: %v", err)
		}
	})

	done := make(chan error, 1)
	go func() {
		_, _, _, err := HandshakeInbound(
			b, respTr.localID, respTr.cfg.Identity.PrivateKey, respTr.cfg.Identity.PublicKey,
			respTr.cfg.ListenAddr, respTr.cfg.Magic, respTr.cfg.Version,
			respTr.cfg.MaxMessageSize, respTr.cfg.ReadTimeout, respTr.cfg.WriteTimeout,
		)
		// 主动关闭 b，避免对端一直卡在 read。
		_ = b.Close()
		done <- err
	}()

	_, _, _, err = HandshakeOutbound(
		a, initTr.localID, initTr.cfg.Identity.PrivateKey, initTr.cfg.Identity.PublicKey,
		initTr.cfg.ListenAddr, initTr.cfg.Magic, initTr.cfg.Version,
		initTr.cfg.MaxMessageSize, initTr.cfg.ReadTimeout, initTr.cfg.WriteTimeout,
	)
	if err == nil {
		t.Fatalf("expected outbound handshake error")
	}
	if inErr := <-done; inErr == nil {
		t.Fatalf("expected inbound handshake error")
	}
}
