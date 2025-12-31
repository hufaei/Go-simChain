package tcp

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"simchain-go/internal/identity"
	"simchain-go/internal/transport"
	"simchain-go/internal/types"
)

type Config struct {
	ListenAddr string
	Seeds      []string

	Magic   string
	Version int

	Identity *identity.Identity

	MaxPeers       int
	OutboundPeers  int
	MaxMessageSize int

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	PeerRequestInterval time.Duration
}

type Transport struct {
	cfg Config

	mu      sync.RWMutex
	handler transport.Handler
	localID string

	ln net.Listener

	peers    map[string]*peerConn
	addrMu   sync.Mutex
	addrBook map[string]time.Time

	stopCh chan struct{}
	doneCh chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
}

func New(cfg Config) (*Transport, error) {
	if cfg.Identity == nil {
		return nil, errors.New("missing identity")
	}
	if cfg.ListenAddr == "" {
		return nil, errors.New("missing listen addr")
	}
	if cfg.Magic == "" {
		cfg.Magic = "simchain-go"
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	if cfg.MaxMessageSize <= 0 {
		cfg.MaxMessageSize = 2 << 20 // 2MB
	}
	if cfg.MaxPeers <= 0 {
		cfg.MaxPeers = 8
	}
	if cfg.OutboundPeers <= 0 {
		cfg.OutboundPeers = 4
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 4 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 4 * time.Second
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 20 * time.Second
	}
	if cfg.PeerRequestInterval <= 0 {
		cfg.PeerRequestInterval = 4 * time.Second
	}
	return &Transport{
		cfg:      cfg,
		localID:  cfg.Identity.NodeID,
		peers:    make(map[string]*peerConn),
		addrBook: make(map[string]time.Time),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}, nil
}

func (t *Transport) Start() error {
	var startErr error
	t.startOnce.Do(func() {
		if err := t.ensureLoopbackListenAddr(); err != nil {
			startErr = err
			return
		}
		ln, err := net.Listen("tcp", t.cfg.ListenAddr)
		if err != nil {
			startErr = err
			return
		}
		t.ln = ln
		go t.acceptLoop()
		go t.dialLoop()
		go t.peerDiscoveryLoop()
	})
	return startErr
}

func (t *Transport) Stop() {
	t.stopOnce.Do(func() {
		close(t.stopCh)
		if t.ln != nil {
			_ = t.ln.Close()
		}
		t.mu.Lock()
		for _, p := range t.peers {
			p.close()
		}
		t.peers = make(map[string]*peerConn)
		t.mu.Unlock()
		<-t.doneCh
	})
}

func (t *Transport) Register(nodeID string, handler transport.Handler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handler = handler
	if nodeID != "" {
		t.localID = nodeID
	}
}

func (t *Transport) Unregister(nodeID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if nodeID != "" && nodeID == t.localID {
		t.handler = nil
	}
}

func (t *Transport) Peers() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.peers))
	for id := range t.peers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (t *Transport) Broadcast(from string, msg types.Message) {
	t.mu.RLock()
	peers := make([]*peerConn, 0, len(t.peers))
	for _, p := range t.peers {
		peers = append(peers, p)
	}
	t.mu.RUnlock()

	if msg.From == "" {
		msg.From = from
	}
	raw, err := encodeFrame(msg, t.cfg.MaxMessageSize)
	if err != nil {
		return
	}
	for _, p := range peers {
		p.sendRaw(raw)
	}
}

func (t *Transport) Send(to string, msg types.Message) {
	if to == "" {
		return
	}
	t.mu.RLock()
	p := t.peers[to]
	t.mu.RUnlock()
	if p == nil {
		return
	}
	if msg.To == "" {
		msg.To = to
	}
	raw, err := encodeFrame(msg, t.cfg.MaxMessageSize)
	if err != nil {
		return
	}
	p.sendRaw(raw)
}

func (t *Transport) SetDelay(d time.Duration) {}
func (t *Transport) SetDropRate(p float64)    {}

func (t *Transport) acceptLoop() {
	defer close(t.doneCh)
	for {
		conn, err := t.ln.Accept()
		if err != nil {
			select {
			case <-t.stopCh:
				return
			default:
			}
			continue
		}
		if !isLoopbackConn(conn) {
			_ = conn.Close()
			continue
		}
		go t.handleInbound(conn)
	}
}

func (t *Transport) dialLoop() {
	t.addSeedAddrs(t.cfg.Seeds)

	// Keep attempting to reach the outbound peer target.
	ticker := time.NewTicker(700 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.maybeDialMore()
		}
	}
}

func (t *Transport) peerDiscoveryLoop() {
	ticker := time.NewTicker(t.cfg.PeerRequestInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.stopCh:
			return
		case <-ticker.C:
			t.requestPeersFromSomePeer()
		}
	}
}

func (t *Transport) handleInbound(conn net.Conn) {
	peerID, peerListen, peerPub, err := t.handshakeInbound(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	pc := newPeerConn(peerID, peerListen, peerPub, conn, t.cfg.WriteTimeout)
	if !t.addPeer(pc) {
		pc.close()
		return
	}
	t.observeAddr(peerListen)
	log.Printf("TCP_PEER_CONNECTED dir=in peer=%s addr=%s listen=%s", peerID, conn.RemoteAddr().String(), peerListen)
	pc.runReadLoop(t, t.cfg.MaxMessageSize, t.cfg.IdleTimeout, t.cfg.ReadTimeout)
}

func (t *Transport) handleOutbound(addr string) {
	d := net.Dialer{Timeout: t.cfg.ReadTimeout}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return
	}
	if !isLoopbackConn(conn) {
		_ = conn.Close()
		return
	}
	peerID, peerListen, peerPub, err := t.handshakeOutbound(conn)
	if err != nil {
		_ = conn.Close()
		return
	}
	pc := newPeerConn(peerID, peerListen, peerPub, conn, t.cfg.WriteTimeout)
	if !t.addPeer(pc) {
		pc.close()
		return
	}
	t.observeAddr(peerListen)
	log.Printf("TCP_PEER_CONNECTED dir=out peer=%s addr=%s listen=%s", peerID, addr, peerListen)
	// Ask for peers once, best-effort.
	pc.send(types.Message{
		Type:      types.MsgGetPeers,
		From:      t.localID,
		To:        peerID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.GetPeersPayload{Max: 32},
	}, t.cfg.MaxMessageSize)
	pc.runReadLoop(t, t.cfg.MaxMessageSize, t.cfg.IdleTimeout, t.cfg.ReadTimeout)
}

func (t *Transport) addPeer(pc *peerConn) bool {
	if pc == nil || pc.id == "" || pc.id == t.localID {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.peers) >= t.cfg.MaxPeers {
		return false
	}
	if _, ok := t.peers[pc.id]; ok {
		return false
	}
	t.peers[pc.id] = pc
	return true
}

func (t *Transport) removePeer(peerID string) {
	t.mu.Lock()
	pc := t.peers[peerID]
	delete(t.peers, peerID)
	t.mu.Unlock()
	if pc != nil {
		pc.close()
	}
}

func (t *Transport) deliverFromPeer(peerID string, msg types.Message) {
	// Enforce sender identity.
	msg.From = peerID
	if msg.To != "" && msg.To != t.localID {
		return
	}
	t.mu.RLock()
	h := t.handler
	t.mu.RUnlock()
	if h == nil {
		return
	}
	h(msg)
}

func (t *Transport) onPeers(peerID string, addrs []string) {
	for _, a := range addrs {
		t.observeAddr(a)
	}
	t.maybeDialMore()
}

func (t *Transport) observeAddr(addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return
	}
	if !isLoopbackAddr(addr) {
		return
	}
	t.addrMu.Lock()
	t.addrBook[addr] = time.Now()
	t.addrMu.Unlock()
}

func (t *Transport) addSeedAddrs(addrs []string) {
	for _, a := range addrs {
		t.observeAddr(a)
	}
}

func (t *Transport) maybeDialMore() {
	if t.outboundCount() >= t.cfg.OutboundPeers {
		return
	}
	candidates := t.addrCandidates()
	for _, addr := range candidates {
		if t.outboundCount() >= t.cfg.OutboundPeers {
			return
		}
		if t.isConnectedToAddr(addr) {
			continue
		}
		go t.handleOutbound(addr)
		// throttle a bit
		time.Sleep(50 * time.Millisecond)
	}
}

func (t *Transport) outboundCount() int {
	// We don't track inbound/outbound separately; approximate via total peers.
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.peers)
}

func (t *Transport) addrCandidates() []string {
	t.addrMu.Lock()
	defer t.addrMu.Unlock()
	out := make([]string, 0, len(t.addrBook))
	for a := range t.addrBook {
		if a == t.cfg.ListenAddr {
			continue
		}
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

func (t *Transport) isConnectedToAddr(addr string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, p := range t.peers {
		if p != nil && p.listenAddr == addr {
			return true
		}
	}
	return false
}

func (t *Transport) requestPeersFromSomePeer() {
	ids := t.Peers()
	if len(ids) == 0 {
		return
	}
	peerID := ids[time.Now().UnixNano()%int64(len(ids))]
	t.Send(peerID, types.Message{
		Type:      types.MsgGetPeers,
		From:      t.localID,
		To:        peerID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.GetPeersPayload{Max: 32},
	})
}

func (t *Transport) ensureLoopbackListenAddr() error {
	if !isLoopbackAddr(t.cfg.ListenAddr) {
		return errors.New("tcp transport only supports loopback listen addr in V3-B")
	}
	return nil
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

func (t *Transport) handshakeOutbound(conn net.Conn) (peerID string, peerListen string, peerPub ed25519.PublicKey, err error) {
	nonceA := make([]byte, 32)
	if _, err := rand.Read(nonceA); err != nil {
		return "", "", nil, err
	}
	if err := writeFrame(conn, types.Message{
		Type:      types.MsgHello,
		From:      t.localID,
		Timestamp: time.Now().UnixMilli(),
		Payload: types.HelloPayload{
			Magic:      t.cfg.Magic,
			Version:    t.cfg.Version,
			PubKey:     []byte(t.cfg.Identity.PublicKey),
			ListenAddr: t.cfg.ListenAddr,
			Nonce:      nonceA,
			Timestamp:  time.Now().UnixMilli(),
		},
	}, t.cfg.MaxMessageSize, t.cfg.WriteTimeout); err != nil {
		return "", "", nil, err
	}
	msg, err := readFrame(conn, t.cfg.MaxMessageSize, t.cfg.ReadTimeout)
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
	if ack.Magic != t.cfg.Magic || ack.Version != t.cfg.Version {
		return "", "", nil, errors.New("magic/version mismatch")
	}
	if !bytesEq(ack.EchoNonce, nonceA) {
		return "", "", nil, errors.New("hello echo nonce mismatch")
	}
	if len(ack.PubKey) != ed25519.PublicKeySize || len(ack.Nonce) == 0 {
		return "", "", nil, errors.New("invalid responder pubkey/nonce")
	}
	peerPub = ed25519.PublicKey(append([]byte(nil), ack.PubKey...))
	if !ed25519.Verify(peerPub, handshakeSigMsg(nonceA, t.cfg.Magic, t.cfg.Version), ack.SigOverEcho) {
		return "", "", nil, errors.New("responder signature invalid")
	}
	peerID, err = identity.NodeIDFromPublicKey(peerPub)
	if err != nil {
		return "", "", nil, err
	}
	peerListen = ack.ListenAddr

	sig := ed25519.Sign(t.cfg.Identity.PrivateKey, handshakeSigMsg(ack.Nonce, t.cfg.Magic, t.cfg.Version))
	if err := writeFrame(conn, types.Message{
		Type:      types.MsgAuth,
		From:      t.localID,
		To:        peerID,
		Timestamp: time.Now().UnixMilli(),
		Payload: types.AuthPayload{
			EchoNonce: ack.Nonce,
			Sig:       sig,
			Timestamp: time.Now().UnixMilli(),
		},
	}, t.cfg.MaxMessageSize, t.cfg.WriteTimeout); err != nil {
		return "", "", nil, err
	}
	return peerID, peerListen, peerPub, nil
}

func (t *Transport) handshakeInbound(conn net.Conn) (peerID string, peerListen string, peerPub ed25519.PublicKey, err error) {
	msg, err := readFrame(conn, t.cfg.MaxMessageSize, t.cfg.ReadTimeout)
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
	if hello.Magic != t.cfg.Magic || hello.Version != t.cfg.Version {
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
	sigB := ed25519.Sign(t.cfg.Identity.PrivateKey, handshakeSigMsg(hello.Nonce, t.cfg.Magic, t.cfg.Version))
	if err := writeFrame(conn, types.Message{
		Type:      types.MsgHelloAck,
		From:      t.localID,
		To:        peerID,
		Timestamp: time.Now().UnixMilli(),
		Payload: types.HelloAckPayload{
			Magic:       t.cfg.Magic,
			Version:     t.cfg.Version,
			PubKey:      []byte(t.cfg.Identity.PublicKey),
			ListenAddr:  t.cfg.ListenAddr,
			Nonce:       nonceB,
			EchoNonce:   hello.Nonce,
			SigOverEcho: sigB,
			Timestamp:   time.Now().UnixMilli(),
		},
	}, t.cfg.MaxMessageSize, t.cfg.WriteTimeout); err != nil {
		return "", "", nil, err
	}

	authMsg, err := readFrame(conn, t.cfg.MaxMessageSize, t.cfg.ReadTimeout)
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
	if !ed25519.Verify(peerPub, handshakeSigMsg(nonceB, t.cfg.Magic, t.cfg.Version), auth.Sig) {
		return "", "", nil, errors.New("initiator signature invalid")
	}
	return peerID, peerListen, peerPub, nil
}

func isLoopbackConn(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

func encodeFrame(msg types.Message, max int) ([]byte, error) {
	raw, err := jsonMarshal(msg)
	if err != nil {
		return nil, err
	}
	if len(raw) > max {
		return nil, errors.New("message too large")
	}
	out := make([]byte, 4+len(raw))
	binary.BigEndian.PutUint32(out[:4], uint32(len(raw)))
	copy(out[4:], raw)
	return out, nil
}

func writeFrame(w io.Writer, msg types.Message, max int, timeout time.Duration) error {
	if c, ok := w.(net.Conn); ok && timeout > 0 {
		_ = c.SetWriteDeadline(time.Now().Add(timeout))
	}
	raw, err := encodeFrame(msg, max)
	if err != nil {
		return err
	}
	_, err = w.Write(raw)
	return err
}

func readFrame(r io.Reader, max int, timeout time.Duration) (types.Message, error) {
	if c, ok := r.(net.Conn); ok && timeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(timeout))
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return types.Message{}, err
	}
	n := int(binary.BigEndian.Uint32(lenBuf[:]))
	if n <= 0 || n > max {
		return types.Message{}, errors.New("invalid message length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return types.Message{}, err
	}
	var msg types.Message
	if err := jsonUnmarshal(buf, &msg); err != nil {
		return types.Message{}, err
	}
	return msg, nil
}

func jsonMarshal(v any) ([]byte, error) {
	// keep centralized to allow future swap.
	return json.Marshal(v)
}

func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
