package tcp

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"simchain-go/internal/identity"
	"simchain-go/internal/transport"
	"simchain-go/internal/types"

	"golang.org/x/time/rate"
)

type Config struct {
	// ListenAddr 是本地监听地址（V3-B 仅支持 127.0.0.1/localhost）。
	ListenAddr string
	// Seeds 是可选的 seed 列表（用于初始发现 peers）。
	Seeds []string

	// Magic/Version 用于避免“串网”，握手时必须一致。
	Magic   string
	Version int

	// Identity 用于握手签名与 nodeID 推导（最小身份绑定）。
	Identity *identity.Identity

	// MaxPeers 是最大连接数（总入站+出站）。
	MaxPeers int
	// OutboundPeers 是目标出站连接数。
	OutboundPeers int
	// MaxConnsPerIP 限制每个 IP 的并发连接数 (默认 2)。
	MaxConnsPerIP int

	// MaxMessageSize 是单条消息最大字节数（用于 framing 限制）。
	MaxMessageSize int

	// RateLimit 是单连接每秒入站字节数限制 (默认 1MB).
	RateLimit int
	// RateBurst 是单连接入站突发字节数限制 (默认 2MB).
	RateBurst int

	// ReadTimeout/WriteTimeout 是单次读写的超时上限。
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	// IdleTimeout 是空闲读超时（用于防止连接被慢速占用）。
	IdleTimeout time.Duration

	// PeerRequestInterval 是向 peers 请求地址列表的周期。
	PeerRequestInterval time.Duration
}

// Transport 是 V3-B 的 TCP 传输实现：
// - 仅支持本机回环（loopback）
// - 使用 length-prefix framing 处理粘包/拆包
// - 握手阶段做最小身份绑定（ed25519 challenge-response）
// - 提供简单的 seed 发现（GetPeers/Peers）
// - V3-C: 集成连接数限制和速率限制
type Transport struct {
	cfg Config

	mu      sync.RWMutex
	handler transport.Handler
	localID string

	ln net.Listener

	peers map[string]*peerConn

	dialMu       sync.Mutex
	dialInFlight map[string]struct{}

	addrMu   sync.Mutex
	addrBook map[string]addrInfo

	// ipCountMu protects ipCount
	ipCountMu sync.Mutex
	ipCount   map[string]int

	rngMu sync.Mutex
	rng   *mrand.Rand

	stopCh chan struct{}
	doneCh chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
}

type addrInfo struct {
	lastSeen time.Time
	nextDial time.Time
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
	if cfg.MaxConnsPerIP <= 0 {
		cfg.MaxConnsPerIP = 2
	}
	if cfg.RateLimit <= 0 { // Default 1MB/s
		cfg.RateLimit = 1 << 20
	}
	if cfg.RateBurst <= 0 { // Default 2MB Burst
		cfg.RateBurst = 2 << 20
	}
	// Burst must be at least MaxMessageSize, otherwise large messages are impossible to read
	if cfg.RateBurst < cfg.MaxMessageSize {
		cfg.RateBurst = cfg.MaxMessageSize
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
		cfg:          cfg,
		localID:      cfg.Identity.NodeID,
		peers:        make(map[string]*peerConn),
		dialInFlight: make(map[string]struct{}),
		addrBook:     make(map[string]addrInfo),
		ipCount:      make(map[string]int),
		rng:          mrand.New(mrand.NewSource(time.Now().UnixNano())),
		stopCh:       make(chan struct{}),
		doneCh:       make(chan struct{}),
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

// SetDelay / SetDropRate 用于 inproc 的故障注入；tcp 模式下暂不支持（no-op）。
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

		// V3-C: Immediate IP limit check (Anti-DoS)
		host, _, err := net.SplitHostPort(conn.RemoteAddr().String())
		if err == nil && !t.trackIP(host) {
			_ = conn.Close()
			continue
		}

		if !isLoopbackConn(conn) {
			t.untrackIP(host) // Closed, so untrack
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
	host, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	// Ensure cleanup if early return
	defer func() {
		// If addPeer failed, we must untrack.
		// If addPeer succeeded, removePeer will handle untrack.
		// Use a clever tracking mechanism or just check if it's in peers?
		// Better: peerConn handles cleanup? No, Transport manages ipCount.
		// To keep it simple: if not added to peers, untrack.
	}()

	peerID, peerListen, peerPub, err := t.handshakeInbound(conn)
	if err != nil {
		t.untrackIP(host)
		_ = conn.Close()
		return
	}

	// Create limiter
	limiter := rate.NewLimiter(rate.Limit(t.cfg.RateLimit), t.cfg.RateBurst)
	pc := newPeerConn(peerID, peerListen, peerPub, conn, false, t.cfg.WriteTimeout, limiter)

	if !t.addPeer(pc) {
		t.untrackIP(host)
		pc.close()
		return
	}
	t.observeAddr(peerListen)
	log.Printf("TCP_PEER_CONNECTED dir=in peer=%s addr=%s listen=%s", peerID, conn.RemoteAddr().String(), peerListen)
	pc.runReadLoop(t, t.cfg.MaxMessageSize, t.cfg.IdleTimeout, t.cfg.ReadTimeout)
}

func (t *Transport) handleOutbound(addr string) {
	defer t.clearDialInFlight(addr)

	// V3-C: Check IP limit before dialing
	host, _, _ := net.SplitHostPort(addr)
	if !t.trackIP(host) {
		return
	}

	d := net.Dialer{Timeout: t.cfg.ReadTimeout}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		t.untrackIP(host)
		t.backoffAddrDial(addr, false)
		return
	}
	if !isLoopbackConn(conn) {
		t.untrackIP(host)
		_ = conn.Close()
		t.backoffAddrDial(addr, true)
		return
	}
	peerID, peerListen, peerPub, err := t.handshakeOutbound(conn)
	if err != nil {
		t.untrackIP(host)
		_ = conn.Close()
		t.backoffAddrDial(addr, false)
		return
	}

	limiter := rate.NewLimiter(rate.Limit(t.cfg.RateLimit), t.cfg.RateBurst)
	pc := newPeerConn(peerID, peerListen, peerPub, conn, true, t.cfg.WriteTimeout, limiter)
	if !t.addPeer(pc) {
		t.untrackIP(host)
		pc.close()
		t.backoffAddrDial(addr, true)
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

func (t *Transport) trackIP(host string) bool {
	t.ipCountMu.Lock()
	defer t.ipCountMu.Unlock()
	if t.ipCount[host] >= t.cfg.MaxConnsPerIP {
		return false
	}
	t.ipCount[host]++
	return true
}

func (t *Transport) untrackIP(host string) {
	t.ipCountMu.Lock()
	defer t.ipCountMu.Unlock()
	if t.ipCount[host] > 0 {
		t.ipCount[host]--
		if t.ipCount[host] == 0 {
			delete(t.ipCount, host)
		}
	}
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
	maxOut := t.maxOutboundLocked()
	if pc.outbound && t.outboundCountLocked() >= maxOut {
		return false
	}
	maxIn := t.cfg.MaxPeers - maxOut
	if !pc.outbound && maxIn >= 0 && t.inboundCountLocked() >= maxIn {
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
		// V3-C: Untrack IP when peer is removed
		host, _, _ := net.SplitHostPort(pc.conn.RemoteAddr().String())
		t.untrackIP(host)
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
	if !validTCPAddr(addr) {
		return
	}
	t.addrMu.Lock()
	info := t.addrBook[addr]
	info.lastSeen = time.Now()
	t.addrBook[addr] = info
	t.addrMu.Unlock()
}

func (t *Transport) addSeedAddrs(addrs []string) {
	for _, a := range addrs {
		t.observeAddr(a)
	}
}

func (t *Transport) maybeDialMore() {
	targetOut := t.maxOutbound()
	if t.outboundCount() >= targetOut {
		return
	}
	candidates := t.addrCandidates()
	for _, addr := range candidates {
		if t.outboundCount() >= targetOut {
			return
		}
		if !t.markDialInFlight(addr) {
			continue
		}
		if t.isConnectedToAddr(addr) {
			t.clearDialInFlight(addr)
			continue
		}
		if !t.addrDialAllowed(addr) {
			t.clearDialInFlight(addr)
			continue
		}
		go t.handleOutbound(addr)
		// throttle a bit
		time.Sleep(50 * time.Millisecond)
	}
}

func (t *Transport) outboundCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.outboundCountLocked()
}

func (t *Transport) outboundCountLocked() int {
	n := 0
	for _, p := range t.peers {
		if p != nil && p.outbound {
			n++
		}
	}
	return n
}

func (t *Transport) inboundCountLocked() int {
	n := 0
	for _, p := range t.peers {
		if p != nil && !p.outbound {
			n++
		}
	}
	return n
}

func (t *Transport) maxOutbound() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.maxOutboundLocked()
}

func (t *Transport) maxOutboundLocked() int {
	maxOut := t.cfg.OutboundPeers
	if maxOut < 0 {
		maxOut = 0
	}
	if maxOut > t.cfg.MaxPeers {
		maxOut = t.cfg.MaxPeers
	}
	return maxOut
}

func (t *Transport) addrCandidates() []string {
	t.addrMu.Lock()
	defer t.addrMu.Unlock()
	now := time.Now()
	out := make([]string, 0, len(t.addrBook))
	for a, info := range t.addrBook {
		if a == t.cfg.ListenAddr {
			continue
		}
		if !info.nextDial.IsZero() && info.nextDial.After(now) {
			continue
		}
		if !info.lastSeen.IsZero() && now.Sub(info.lastSeen) > 10*time.Minute {
			delete(t.addrBook, a)
			continue
		}
		out = append(out, a)
	}
	t.shuffleLocked(out)
	return out
}

func (t *Transport) shuffleLocked(ss []string) {
	t.rngMu.Lock()
	defer t.rngMu.Unlock()
	t.rng.Shuffle(len(ss), func(i, j int) { ss[i], ss[j] = ss[j], ss[i] })
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
	peerID := ids[t.randIntn(len(ids))]
	t.Send(peerID, types.Message{
		Type:      types.MsgGetPeers,
		From:      t.localID,
		To:        peerID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.GetPeersPayload{Max: 32},
	})
}

func (t *Transport) randIntn(n int) int {
	if n <= 1 {
		return 0
	}
	t.rngMu.Lock()
	defer t.rngMu.Unlock()
	return t.rng.Intn(n)
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

func validTCPAddr(addr string) bool {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	return err == nil && port > 0 && port <= 65535
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
	raw, err := json.Marshal(msg)
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
	return readFrameWithLimit(r, max, timeout, nil)
}

func readFrameWithLimit(r io.Reader, max int, timeout time.Duration, limiter *rate.Limiter) (types.Message, error) {
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

	// V3-C: Apply rate limiting if limiter is provided
	if limiter != nil {
		ctx := context.Background()
		if timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
		if err := limiter.WaitN(ctx, n); err != nil {
			return types.Message{}, err
		}
		// Reset deadline because WaitN might have consumed time
		if c, ok := r.(net.Conn); ok && timeout > 0 {
			_ = c.SetReadDeadline(time.Now().Add(timeout))
		}
	}

	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return types.Message{}, err
	}
	var msg types.Message
	if err := json.Unmarshal(buf, &msg); err != nil {
		return types.Message{}, err
	}
	return msg, nil
}

func (t *Transport) markDialInFlight(addr string) bool {
	t.dialMu.Lock()
	defer t.dialMu.Unlock()
	if _, ok := t.dialInFlight[addr]; ok {
		return false
	}
	t.dialInFlight[addr] = struct{}{}
	return true
}

func (t *Transport) clearDialInFlight(addr string) {
	t.dialMu.Lock()
	delete(t.dialInFlight, addr)
	t.dialMu.Unlock()
}

func (t *Transport) addrDialAllowed(addr string) bool {
	t.addrMu.Lock()
	defer t.addrMu.Unlock()
	info, ok := t.addrBook[addr]
	if !ok {
		return true
	}
	if info.nextDial.IsZero() {
		return true
	}
	return time.Now().After(info.nextDial)
}

func (t *Transport) backoffAddrDial(addr string, permanent bool) {
	t.addrMu.Lock()
	defer t.addrMu.Unlock()
	info := t.addrBook[addr]
	backoff := 900 * time.Millisecond
	if permanent {
		backoff = 2 * time.Second
	}
	jitter := time.Duration(t.randIntn(250)) * time.Millisecond
	info.nextDial = time.Now().Add(backoff + jitter)
	t.addrBook[addr] = info
}
