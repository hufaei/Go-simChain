package tcp

import (
	"crypto/ed25519"
	"net"
	"sync"
	"time"

	"simchain-go/internal/types"
)

type peerConn struct {
	id         string
	listenAddr string
	pubKey     ed25519.PublicKey
	outbound   bool

	conn net.Conn

	writeTimeout time.Duration
	writeCh      chan []byte

	closeOnce sync.Once
	closedCh  chan struct{}
}

func newPeerConn(id string, listenAddr string, pub ed25519.PublicKey, conn net.Conn, outbound bool, writeTimeout time.Duration) *peerConn {
	pc := &peerConn{
		id:           id,
		listenAddr:   listenAddr,
		pubKey:       pub,
		outbound:     outbound,
		conn:         conn,
		writeTimeout: writeTimeout,
		writeCh:      make(chan []byte, 128),
		closedCh:     make(chan struct{}),
	}
	go pc.writeLoop()
	return pc
}

func (p *peerConn) close() {
	p.closeOnce.Do(func() {
		close(p.closedCh)
		_ = p.conn.Close()
	})
}

func (p *peerConn) sendRaw(frame []byte) {
	if p == nil || len(frame) == 0 {
		return
	}
	select {
	case p.writeCh <- frame:
	default:
		// Drop if the peer is too slow.
	}
}

func (p *peerConn) send(msg types.Message, maxSize int) {
	raw, err := encodeFrame(msg, maxSize)
	if err != nil {
		return
	}
	p.sendRaw(raw)
}

func (p *peerConn) writeLoop() {
	for {
		select {
		case <-p.closedCh:
			return
		case frame := <-p.writeCh:
			if frame == nil {
				continue
			}
			if p.writeTimeout > 0 {
				_ = p.conn.SetWriteDeadline(time.Now().Add(p.writeTimeout))
			}
			if _, err := p.conn.Write(frame); err != nil {
				p.close()
				return
			}
		}
	}
}

func (p *peerConn) runReadLoop(t *Transport, maxSize int, idleTimeout time.Duration, readTimeout time.Duration) {
	defer t.removePeer(p.id)
	for {
		select {
		case <-t.stopCh:
			return
		case <-p.closedCh:
			return
		default:
		}

		timeout := idleTimeout
		if timeout <= 0 {
			timeout = readTimeout
		}
		msg, err := readFrame(p.conn, maxSize, timeout)
		if err != nil {
			p.close()
			return
		}

		switch msg.Type {
		case types.MsgGetPeers:
			pl, ok := msg.Payload.(types.GetPeersPayload)
			if !ok {
				pl = types.GetPeersPayload{Max: 32}
			}
			addrs := t.addrCandidates()
			if pl.Max > 0 && pl.Max < len(addrs) {
				addrs = addrs[:pl.Max]
			}
			p.send(types.Message{
				Type:      types.MsgPeers,
				From:      t.localID,
				To:        p.id,
				Timestamp: time.Now().UnixMilli(),
				Payload:   types.PeersPayload{Addrs: addrs},
			}, maxSize)
		case types.MsgPeers:
			pl, ok := msg.Payload.(types.PeersPayload)
			if ok {
				t.onPeers(p.id, pl.Addrs)
			}
		default:
			t.deliverFromPeer(p.id, msg)
		}
	}
}
