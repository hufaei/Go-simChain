package syncer

import (
	"math/big"
	"sync"
	"time"

	"simchain-go/internal/blockchain"
	"simchain-go/internal/crypto"
	"simchain-go/internal/peer"
	"simchain-go/internal/transport"
	"simchain-go/internal/types"
)

type syncState uint8

const (
	syncIdle syncState = iota
	syncingHeaders
	syncingBlocks
	syncBackoff
)

type blockReqKind uint8

const (
	blkSync blockReqKind = iota
	blkInv
	blkParent
)

type inflightBlockReq struct {
	peer     string
	kind     blockReqKind
	attempts int
	deadline time.Time
}

type headersResult struct {
	peer  string
	ok    bool
	metas []types.BlockMeta
}

// RPCFunc is a single-request/single-response helper used by Syncer.
// It stays intentionally small: most sync logic lives in Syncer; Node only provides routing + final AddBlock.
type RPCFunc func(to string, typ types.MessageType, payload any, timeout time.Duration) (types.Message, bool)

// OnBlockFunc is called for each downloaded full block during sync.
// The node should validate/add it via chain.AddBlock and do any mempool adjustments.
type OnBlockFunc func(b *types.Block, from string)

type Config struct {
	NodeID string

	Transport transport.Transport
	Chain     *blockchain.Blockchain
	Peers     *peer.Manager
	HasTx     func(txid string) bool
	OnTx      func(tx types.Transaction, fromPeer string) bool

	SyncTimeout     time.Duration
	MaxHeaders      int
	BlocksBatchSize int
	ProbeInterval   time.Duration

	// V3-A: headers-first catch-up 的状态机参数
	// MaxInflightBlocks：同时在路上的 GetBlock 数量上限（窗口大小）
	// BlockRetryLimit：单个区块 GetBlock 超时后最多重试次数（含首次）
	// HeaderRetryLimit：GetHeaders 超时后最多重试次数（含首次）
	// BackoffDuration：同步失败后的冷却时间（避免一直打同一个 peer）
	MaxInflightBlocks int
	BlockRetryLimit   int
	HeaderRetryLimit  int
	BackoffDuration   time.Duration

	RPC     RPCFunc
	OnBlock OnBlockFunc
}

// Syncer is responsible for keeping a node's chain up to date.
//
// V3-A：Syncer 负责长期追链（headers-first + window + retry + peer backoff），
// 同时接管 inv/get 的“拉取/重试/向谁拉”的策略；Node 只做路由 + 最终 AddBlock。
type Syncer struct {
	cfg Config

	mu sync.Mutex

	// -------- block fetch (inv + sync 共用) --------
	inflightBlocks map[types.Hash]*inflightBlockReq
	inflightTx     map[string]struct{}

	queueParent []types.Hash
	queueSync   []types.Hash
	queueInv    []types.Hash

	queuedParent map[types.Hash]string
	queuedSync   map[types.Hash]string
	queuedInv    map[types.Hash]string

	// -------- headers-first catch-up 状态机 --------
	state        syncState
	syncPeer     string
	backoffUntil time.Time

	headerReqInFlight bool
	headerRetries     int
	headersCh         chan headersResult

	// -------- V3-C: peer performance metrics --------
	metrics *metricsTracker

	stopCh  chan struct{}
	doneCh  chan struct{}
	startMu sync.Mutex
	started bool
}

func New(cfg Config) *Syncer {
	if cfg.MaxHeaders <= 0 {
		cfg.MaxHeaders = 2000
	}
	if cfg.BlocksBatchSize <= 0 {
		cfg.BlocksBatchSize = 64
	}
	if cfg.SyncTimeout <= 0 {
		cfg.SyncTimeout = 2 * time.Second
	}
	if cfg.ProbeInterval <= 0 {
		cfg.ProbeInterval = 900 * time.Millisecond
	}
	if cfg.MaxInflightBlocks <= 0 {
		cfg.MaxInflightBlocks = 16
	}
	if cfg.BlockRetryLimit <= 0 {
		cfg.BlockRetryLimit = 3
	}
	if cfg.HeaderRetryLimit <= 0 {
		cfg.HeaderRetryLimit = 2
	}
	if cfg.BackoffDuration <= 0 {
		cfg.BackoffDuration = 1200 * time.Millisecond
	}
	if cfg.Peers == nil {
		cfg.Peers = peer.NewManager(800 * time.Millisecond)
	}
	return &Syncer{
		cfg: cfg,

		inflightBlocks: make(map[types.Hash]*inflightBlockReq),
		inflightTx:     make(map[string]struct{}),

		queuedParent: make(map[types.Hash]string),
		queuedSync:   make(map[types.Hash]string),
		queuedInv:    make(map[types.Hash]string),

		metrics: newMetricsTracker(), // V3-C: initialize peer performance tracker

		headersCh: make(chan headersResult, 1),
	}
}

// HandleInvBlock processes an incoming block announcement (meta).
// It performs a cheap PoW check (header+nonce -> hash) and requests the full block only if needed.
//
// 说明：V3-A 先把 “是否拉取、向谁拉取、避免重复拉取” 迁到 Syncer，
// 区块的完整校验与接入仍然由 Node/Chain 负责（通过 cfg.OnBlock 回调）。
func (s *Syncer) HandleInvBlock(meta types.BlockMeta, fromPeer string) {
	if s.cfg.Transport == nil || s.cfg.Chain == nil {
		return
	}
	if fromPeer == "" || meta.Hash.IsZero() {
		return
	}
	if s.cfg.Chain.HasBlock(meta.Hash) {
		return
	}
	if meta.Header.Difficulty != s.cfg.Chain.Difficulty() {
		return
	}

	// 轻校验 PoW：不下载 block body 的前提下先拒绝明显无效的公告。
	computed, err := crypto.HashHeaderNonce(meta.Header, meta.Nonce)
	if err != nil || computed != meta.Hash || !crypto.MeetsDifficulty(meta.Hash, s.cfg.Chain.Difficulty()) {
		return
	}

	// 若父块未知，先尝试请求父块（best-effort）。
	if meta.Header.Height > 0 && !meta.Header.PrevHash.IsZero() && !s.cfg.Chain.HasBlock(meta.Header.PrevHash) {
		s.enqueueBlock(meta.Header.PrevHash, fromPeer, blkParent)
	}
	s.enqueueBlock(meta.Hash, fromPeer, blkInv)
	// 收到 inv 后尽量立刻发出请求（受窗口限制）
	s.dispatchQueuedBlocks(time.Now())
}

// HandleBlock processes a full block response to a previous GetBlock request.
// It clears inflight state and forwards the block to the chain via cfg.OnBlock.
func (s *Syncer) HandleBlock(b *types.Block, fromPeer string) {
	if b == nil || s.cfg.OnBlock == nil {
		return
	}
	h := b.Hash
	if h.IsZero() {
		if computed, err := crypto.HashHeaderNonce(b.Header, b.Nonce); err == nil {
			h = computed
		}
	}

	// V3-C: Record successful block response for peer metrics
	s.mu.Lock()
	req, ok := s.inflightBlocks[h]
	s.mu.Unlock()
	if ok && req != nil && fromPeer != "" {
		startTime := req.deadline.Add(-s.cfg.SyncTimeout)
		s.metrics.recordRequest(fromPeer, startTime, true)
	}

	s.onBlockArrived(h)
	s.cfg.OnBlock(b, fromPeer)
}

// HandleBlocks processes a batch block response to a previous GetBlocks request.
// It clears inflight state for each received block and forwards blocks to the chain via cfg.OnBlock.
func (s *Syncer) HandleBlocks(blocks []*types.Block, fromPeer string) {
	if len(blocks) == 0 || s.cfg.OnBlock == nil {
		return
	}

	// V3-C: Record successful batch response for peer metrics
	// Use the first block to estimate request start time
	if fromPeer != "" && len(blocks) > 0 {
		firstHash := blocks[0].Hash
		if firstHash.IsZero() {
			if computed, err := crypto.HashHeaderNonce(blocks[0].Header, blocks[0].Nonce); err == nil {
				firstHash = computed
			}
		}
		s.mu.Lock()
		req, ok := s.inflightBlocks[firstHash]
		s.mu.Unlock()
		if ok && req != nil {
			startTime := req.deadline.Add(-s.cfg.SyncTimeout)
			s.metrics.recordRequest(fromPeer, startTime, true)
		}
	}

	for _, b := range blocks {
		if b == nil {
			continue
		}
		h := b.Hash
		if h.IsZero() {
			if computed, err := crypto.HashHeaderNonce(b.Header, b.Nonce); err == nil {
				h = computed
			}
		}
		s.onBlockArrived(h)
		s.cfg.OnBlock(b, fromPeer)
	}
}

// HandleInvTx processes a tx announcement.
// If we don't have the tx yet, request it from the announcing peer.
func (s *Syncer) HandleInvTx(txid string, fromPeer string) {
	if s.cfg.Transport == nil || txid == "" || fromPeer == "" {
		return
	}
	if s.cfg.HasTx != nil && s.cfg.HasTx(txid) {
		return
	}

	s.mu.Lock()
	if _, ok := s.inflightTx[txid]; ok {
		s.mu.Unlock()
		return
	}
	s.inflightTx[txid] = struct{}{}
	s.mu.Unlock()

	time.AfterFunc(s.cfg.SyncTimeout, func() {
		s.mu.Lock()
		delete(s.inflightTx, txid)
		s.mu.Unlock()
	})

	s.cfg.Transport.Send(fromPeer, types.Message{
		Type:      types.MsgGetTx,
		From:      s.cfg.NodeID,
		To:        fromPeer,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.GetTxPayload{TxID: txid},
	})
}

// HandleTx processes a full transaction response.
// The actual mempool insert / dedupe is delegated to cfg.OnTx.
func (s *Syncer) HandleTx(tx types.Transaction, fromPeer string) {
	if tx.ID == "" {
		return
	}
	s.mu.Lock()
	delete(s.inflightTx, tx.ID)
	s.mu.Unlock()

	if s.cfg.OnTx == nil {
		return
	}
	added := s.cfg.OnTx(tx, fromPeer)
	if !added || s.cfg.Transport == nil {
		return
	}
	// Gossip the txid further. Peers that already know it will ignore.
	s.cfg.Transport.Broadcast(s.cfg.NodeID, types.Message{
		Type:      types.MsgInvTx,
		From:      s.cfg.NodeID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.InvTxPayload{TxID: tx.ID},
	})
}

func (s *Syncer) enqueueBlock(h types.Hash, preferredPeer string, kind blockReqKind) {
	if s.cfg.Transport == nil || s.cfg.Chain == nil || h.IsZero() {
		return
	}
	if s.cfg.Chain.HasBlock(h) {
		return
	}

	s.mu.Lock()
	if _, ok := s.inflightBlocks[h]; ok {
		s.mu.Unlock()
		return
	}
	switch kind {
	case blkParent:
		if _, ok := s.queuedParent[h]; ok {
			s.mu.Unlock()
			return
		}
		s.queuedParent[h] = preferredPeer
		s.queueParent = append(s.queueParent, h)
	case blkSync:
		if _, ok := s.queuedSync[h]; ok {
			s.mu.Unlock()
			return
		}
		s.queuedSync[h] = preferredPeer
		s.queueSync = append(s.queueSync, h)
	default:
		if _, ok := s.queuedInv[h]; ok {
			s.mu.Unlock()
			return
		}
		s.queuedInv[h] = preferredPeer
		s.queueInv = append(s.queueInv, h)
	}
	s.mu.Unlock()
}

func (s *Syncer) popNextQueuedLocked() (h types.Hash, peerID string, kind blockReqKind, ok bool) {
	for len(s.queueParent) > 0 {
		h = s.queueParent[0]
		s.queueParent = s.queueParent[1:]
		peerID, ok = s.queuedParent[h]
		if !ok {
			continue
		}
		delete(s.queuedParent, h)
		return h, peerID, blkParent, true
	}
	for len(s.queueSync) > 0 {
		h = s.queueSync[0]
		s.queueSync = s.queueSync[1:]
		peerID, ok = s.queuedSync[h]
		if !ok {
			continue
		}
		delete(s.queuedSync, h)
		return h, peerID, blkSync, true
	}
	for len(s.queueInv) > 0 {
		h = s.queueInv[0]
		s.queueInv = s.queueInv[1:]
		peerID, ok = s.queuedInv[h]
		if !ok {
			continue
		}
		delete(s.queuedInv, h)
		return h, peerID, blkInv, true
	}
	return types.Hash{}, "", 0, false
}

func (s *Syncer) dispatchQueuedBlocks(now time.Time) {
	if s.cfg.Transport == nil || s.cfg.Chain == nil {
		return
	}
	bestPeer, _, _, _ := s.cfg.Peers.BestPeer(now)

	type sendReq struct {
		to string
		h  types.Hash
	}
	sends := make([]sendReq, 0, 8)
	type sendBatch struct {
		to     string
		hashes []types.Hash
	}
	batches := make([]sendBatch, 0, 4)

	s.mu.Lock()
	// 父块优先：单块 GetBlock
	for len(s.inflightBlocks) < s.cfg.MaxInflightBlocks {
		h, peerID, kind, ok := s.popNextQueuedLocked()
		if !ok {
			break
		}
		// 把 popNextQueuedLocked 的优先级保持不变，但我们要在这里对 blkSync 做批量，
		// 所以如果取到了 blkSync，就先“放回去”并跳出，让后面专门的 sync 批量逻辑处理。
		if kind == blkSync {
			// 放回队首（保持顺序）
			s.queueSync = append([]types.Hash{h}, s.queueSync...)
			s.queuedSync[h] = peerID
			break
		}
		if s.cfg.Chain.HasBlock(h) {
			continue
		}
		if _, ok := s.inflightBlocks[h]; ok {
			continue
		}
		if peerID == "" {
			peerID = bestPeer
		}
		if peerID == "" || peerID == s.cfg.NodeID {
			continue
		}
		s.inflightBlocks[h] = &inflightBlockReq{
			peer:     peerID,
			kind:     kind,
			attempts: 1,
			deadline: now.Add(s.cfg.SyncTimeout),
		}
		sends = append(sends, sendReq{to: peerID, h: h})
	}

	// 同步块：批量 GetBlocks（每个 hash 仍然占用窗口 1 个单位）
	syncPeer := s.syncPeer
	if syncPeer == "" {
		syncPeer = bestPeer
	}
	for len(s.inflightBlocks) < s.cfg.MaxInflightBlocks && len(s.queueSync) > 0 {
		remaining := s.cfg.MaxInflightBlocks - len(s.inflightBlocks)
		bs := s.cfg.BlocksBatchSize
		if bs <= 0 {
			bs = 1
		}
		if bs > remaining {
			bs = remaining
		}
		if bs > len(s.queueSync) {
			bs = len(s.queueSync)
		}
		if bs <= 0 {
			break
		}
		if syncPeer == "" || syncPeer == s.cfg.NodeID {
			break
		}
		hashes := make([]types.Hash, 0, bs)
		for i := 0; i < bs; i++ {
			h := s.queueSync[0]
			s.queueSync = s.queueSync[1:]
			delete(s.queuedSync, h)
			if h.IsZero() || s.cfg.Chain.HasBlock(h) {
				continue
			}
			if _, ok := s.inflightBlocks[h]; ok {
				continue
			}
			s.inflightBlocks[h] = &inflightBlockReq{
				peer:     syncPeer,
				kind:     blkSync,
				attempts: 1,
				deadline: now.Add(s.cfg.SyncTimeout),
			}
			hashes = append(hashes, h)
		}
		if len(hashes) > 0 {
			batches = append(batches, sendBatch{to: syncPeer, hashes: hashes})
		}
	}

	// inv：单块 GetBlock
	for len(s.inflightBlocks) < s.cfg.MaxInflightBlocks && len(s.queueInv) > 0 {
		h := s.queueInv[0]
		s.queueInv = s.queueInv[1:]
		peerID := s.queuedInv[h]
		delete(s.queuedInv, h)
		if h.IsZero() || s.cfg.Chain.HasBlock(h) {
			continue
		}
		if _, ok := s.inflightBlocks[h]; ok {
			continue
		}
		if peerID == "" {
			peerID = bestPeer
		}
		if peerID == "" || peerID == s.cfg.NodeID {
			continue
		}
		s.inflightBlocks[h] = &inflightBlockReq{
			peer:     peerID,
			kind:     blkInv,
			attempts: 1,
			deadline: now.Add(s.cfg.SyncTimeout),
		}
		sends = append(sends, sendReq{to: peerID, h: h})
	}
	s.mu.Unlock()

	for _, r := range sends {
		s.cfg.Transport.Send(r.to, types.Message{
			Type:      types.MsgGetBlock,
			From:      s.cfg.NodeID,
			To:        r.to,
			Timestamp: time.Now().UnixMilli(),
			Payload:   types.GetBlockPayload{Hash: r.h},
		})
	}

	for _, b := range batches {
		s.sendGetBlocks(b.to, b.hashes)
	}
}

func (s *Syncer) onBlockArrived(h types.Hash) {
	if h.IsZero() {
		return
	}
	s.mu.Lock()
	delete(s.inflightBlocks, h)
	s.mu.Unlock()
}

func (s *Syncer) sendGetBlocks(to string, hashes []types.Hash) {
	if s.cfg.Transport == nil || to == "" || to == s.cfg.NodeID || len(hashes) == 0 {
		return
	}
	s.cfg.Transport.Send(to, types.Message{
		Type:      types.MsgGetBlocks,
		From:      s.cfg.NodeID,
		To:        to,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.GetBlocksPayload{Hashes: hashes},
	})
}

// Start runs a background loop that:
// - probes peer tips periodically
// - selects the best peer
// - performs a headers-first catch-up when behind
func (s *Syncer) Start() {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if s.started {
		return
	}
	s.started = true
	s.stopCh = make(chan struct{})
	s.doneCh = make(chan struct{})
	go s.loop()

	// 尽快推进一次：新节点加入时不必等待第一个 ticker tick 才开始探测/追链。
	go func() {
		s.probeTips()
		s.catchUpIfBehind()
	}()
}

func (s *Syncer) Stop() {
	s.startMu.Lock()
	if !s.started {
		s.startMu.Unlock()
		return
	}
	ch := s.stopCh
	done := s.doneCh
	// Make Stop idempotent: mark not started before closing stopCh to avoid double-close panics.
	s.started = false
	s.startMu.Unlock()

	close(ch)
	<-done
}

func (s *Syncer) loop() {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.cfg.ProbeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.probeTips()
			s.catchUpIfBehind()
		}
	}
}

func (s *Syncer) probeTips() {
	if s.cfg.Transport == nil || s.cfg.Chain == nil || s.cfg.RPC == nil {
		return
	}
	peers := s.cfg.Transport.Peers()
	type tipRes struct {
		peer   string
		ok     bool
		height uint64
		hash   types.Hash
		work   *big.Int
		rtt    time.Duration
	}
	ch := make(chan tipRes, len(peers))
	n := 0
	for _, p := range peers {
		if p == "" || p == s.cfg.NodeID {
			continue
		}
		n++
		s.cfg.Peers.Upsert(p)
		go func(peerID string) {
			start := time.Now()
			resp, ok := s.cfg.RPC(peerID, types.MsgGetTip, types.GetTipPayload{}, s.cfg.SyncTimeout)
			rtt := time.Since(start)
			if !ok || resp.Type != types.MsgTip {
				ch <- tipRes{peer: peerID, ok: false}
				return
			}
			pl, ok := resp.Payload.(types.TipPayload)
			if !ok {
				ch <- tipRes{peer: peerID, ok: false}
				return
			}
			w := new(big.Int)
			if _, ok := w.SetString(pl.CumWork, 10); !ok {
				ch <- tipRes{peer: peerID, ok: false}
				return
			}
			ch <- tipRes{peer: peerID, ok: true, height: pl.TipHeight, hash: pl.TipHash, work: w, rtt: rtt}
		}(p)
	}
	for i := 0; i < n; i++ {
		r := <-ch
		if !r.ok || r.work == nil {
			s.cfg.Peers.ReportTimeout(r.peer)
			continue
		}
		s.cfg.Peers.UpdateTip(r.peer, r.height, r.hash, r.work, r.rtt)
	}
}

func (s *Syncer) catchUpIfBehind() {
	if s.cfg.Chain == nil {
		return
	}
	now := time.Now()

	// 先处理 inflight 的超时重试（不依赖当前同步状态）
	s.tickInflight(now)
	// 再把队列尽量打满窗口（parent > sync > inv）
	s.dispatchQueuedBlocks(now)
	// 处理 headers RPC 的异步结果（会把状态从 syncingHeaders 推进到 syncingBlocks）
	s.drainHeaderResults(now)

	s.mu.Lock()
	if s.state == syncBackoff && now.After(s.backoffUntil) {
		s.state = syncIdle
		s.syncPeer = ""
		s.headerRetries = 0
		s.headerReqInFlight = false
	}
	s.mu.Unlock()

	bestPeer, bestHeight, _, bestWork := s.cfg.Peers.BestPeer(now)
	if bestPeer == "" || bestWork == nil {
		return
	}
	localWork := s.cfg.Chain.TipCumWork()
	behind := localWork.Cmp(bestWork) < 0 || s.cfg.Chain.TipHeight() < bestHeight
	if !behind {
		s.resetSyncState()
		return
	}

	s.mu.Lock()
	if s.state == syncIdle {
		s.state = syncingHeaders
		s.syncPeer = bestPeer
		s.headerRetries = 0
	}
	state := s.state
	peerID := s.syncPeer
	s.mu.Unlock()
	if peerID == "" {
		peerID = bestPeer
	}

	switch state {
	case syncingHeaders:
		s.maybeStartHeaderRequest(peerID)
	case syncingBlocks:
		if s.syncWorkDone() {
			s.mu.Lock()
			if s.state == syncingBlocks {
				s.state = syncingHeaders
			}
			s.mu.Unlock()
		}
	default:
	}
}

func (s *Syncer) resetSyncState() {
	s.mu.Lock()
	s.state = syncIdle
	s.syncPeer = ""
	s.headerRetries = 0
	s.headerReqInFlight = false
	// 只清理“同步用”的工作，不影响 inv/parent 的下载。
	s.queueSync = nil
	s.queuedSync = make(map[types.Hash]string)
	for h, req := range s.inflightBlocks {
		if req != nil && req.kind == blkSync {
			delete(s.inflightBlocks, h)
		}
	}
	s.mu.Unlock()
}

func (s *Syncer) syncWorkDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queuedSync) > 0 {
		return false
	}
	for _, req := range s.inflightBlocks {
		if req != nil && req.kind == blkSync {
			return false
		}
	}
	return true
}

func (s *Syncer) maybeStartHeaderRequest(peerID string) {
	if s.cfg.RPC == nil || s.cfg.Chain == nil || peerID == "" || peerID == s.cfg.NodeID {
		return
	}
	s.mu.Lock()
	if s.state != syncingHeaders || s.headerReqInFlight {
		s.mu.Unlock()
		return
	}
	s.headerReqInFlight = true
	if s.syncPeer == "" {
		s.syncPeer = peerID
	}
	s.mu.Unlock()

	locator := s.cfg.Chain.Locator(32)
	payload := types.GetHeadersPayload{Locator: locator, Max: s.cfg.MaxHeaders}
	timeout := s.cfg.SyncTimeout

	go func() {
		resp, ok := s.cfg.RPC(peerID, types.MsgGetHeaders, payload, timeout)
		if !ok || resp.Type != types.MsgHeaders {
			select {
			case s.headersCh <- headersResult{peer: peerID, ok: false}:
			case <-s.stopCh:
			}
			return
		}
		pl, ok := resp.Payload.(types.HeadersPayload)
		if !ok {
			select {
			case s.headersCh <- headersResult{peer: peerID, ok: false}:
			case <-s.stopCh:
			}
			return
		}
		select {
		case s.headersCh <- headersResult{peer: peerID, ok: true, metas: pl.Metas}:
		case <-s.stopCh:
		}
	}()
}

func (s *Syncer) drainHeaderResults(now time.Time) {
	for {
		select {
		case r := <-s.headersCh:
			s.mu.Lock()
			s.headerReqInFlight = false
			s.mu.Unlock()

			if !r.ok {
				if r.peer != "" {
					s.cfg.Peers.ReportTimeout(r.peer)
				}
				shouldBackoff := false
				s.mu.Lock()
				s.headerRetries++
				if s.headerRetries >= s.cfg.HeaderRetryLimit {
					shouldBackoff = true
				}
				s.mu.Unlock()
				if shouldBackoff {
					s.enterBackoff(now)
				}
				continue
			}

			// 成功拿到 headers：把缺失块放进 sync 队列（具体拉取由窗口/重试机制统一处理）
			s.mu.Lock()
			s.headerRetries = 0
			s.mu.Unlock()

			queuedAny := false
			for _, m := range r.metas {
				if m.Hash.IsZero() {
					continue
				}
				if s.cfg.Chain != nil && s.cfg.Chain.HasBlock(m.Hash) {
					continue
				}
				s.enqueueBlock(m.Hash, r.peer, blkSync)
				queuedAny = true
			}
			if queuedAny {
				s.mu.Lock()
				if s.state == syncingHeaders {
					s.state = syncingBlocks
				}
				if s.syncPeer == "" {
					s.syncPeer = r.peer
				}
				s.mu.Unlock()
				s.dispatchQueuedBlocks(now)
			}
		default:
			return
		}
	}
}

func (s *Syncer) tickInflight(now time.Time) {
	if s.cfg.Transport == nil || s.cfg.Chain == nil || len(s.inflightBlocks) == 0 {
		return
	}
	bestPeer, _, _, _ := s.cfg.Peers.BestPeer(now)

	type sendReq struct {
		to string
		h  types.Hash
	}
	sends := make([]sendReq, 0, 8)
	retrySync := make(map[string][]types.Hash)
	timeouts := make([]string, 0, 8)
	syncFailed := false

	s.mu.Lock()
	for h, req := range s.inflightBlocks {
		if req == nil || now.Before(req.deadline) {
			continue
		}
		if req.peer != "" {
			timeouts = append(timeouts, req.peer)
			// V3-C: Record timeout failure for peer metrics
			startTime := req.deadline.Add(-s.cfg.SyncTimeout)
			s.metrics.recordRequest(req.peer, startTime, false)
		}
		if req.attempts >= s.cfg.BlockRetryLimit {
			if req.kind == blkSync && (s.state == syncingBlocks || s.state == syncingHeaders) {
				syncFailed = true
			}
			delete(s.inflightBlocks, h)
			continue
		}
		peerID := req.peer
		if bestPeer != "" {
			peerID = bestPeer
		}
		req.peer = peerID
		req.attempts++
		req.deadline = now.Add(s.cfg.SyncTimeout)
		if peerID != "" && peerID != s.cfg.NodeID {
			if req.kind == blkSync {
				retrySync[peerID] = append(retrySync[peerID], h)
			} else {
				sends = append(sends, sendReq{to: peerID, h: h})
			}
		}
	}
	s.mu.Unlock()

	for _, p := range timeouts {
		s.cfg.Peers.ReportTimeout(p)
	}
	for _, r := range sends {
		s.cfg.Transport.Send(r.to, types.Message{
			Type:      types.MsgGetBlock,
			From:      s.cfg.NodeID,
			To:        r.to,
			Timestamp: time.Now().UnixMilli(),
			Payload:   types.GetBlockPayload{Hash: r.h},
		})
	}
	for peerID, hashes := range retrySync {
		if len(hashes) == 0 {
			continue
		}
		bs := s.cfg.BlocksBatchSize
		if bs <= 0 {
			bs = 1
		}
		for i := 0; i < len(hashes); i += bs {
			end := i + bs
			if end > len(hashes) {
				end = len(hashes)
			}
			s.sendGetBlocks(peerID, hashes[i:end])
		}
	}
	if syncFailed {
		s.enterBackoff(now)
	}
}

func (s *Syncer) enterBackoff(now time.Time) {
	s.mu.Lock()
	s.state = syncBackoff
	s.backoffUntil = now.Add(s.cfg.BackoffDuration)
	s.syncPeer = ""
	s.headerRetries = 0
	s.headerReqInFlight = false

	// 清掉同步队列与同步 inflight，让下一轮能换 peer 重新来。
	s.queueSync = nil
	s.queuedSync = make(map[types.Hash]string)
	for h, req := range s.inflightBlocks {
		if req != nil && req.kind == blkSync {
			delete(s.inflightBlocks, h)
		}
	}
	s.mu.Unlock()
}
