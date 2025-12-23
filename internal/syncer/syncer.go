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

// RPCFunc is a single-request/single-response helper used by InitialSync.
// It is intentionally small: V3-A will later evolve this into a persistent Syncer state machine.
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

	RPC     RPCFunc
	OnBlock OnBlockFunc
}

// Syncer is responsible for keeping a node's chain up to date.
//
// 当前只先迁移 V2 的 InitialSync（late join 追链逻辑）到这里；
// 后续 V3-A 会把 inv/get 的长期同步与 retry/window 也逐步搬入 Syncer。
type Syncer struct {
	cfg Config

	mu            sync.Mutex
	inflightBlock map[types.Hash]struct{}
	inflightTx    map[string]struct{}

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
	if cfg.Peers == nil {
		cfg.Peers = peer.NewManager(800 * time.Millisecond)
	}
	return &Syncer{
		cfg:           cfg,
		inflightBlock: make(map[types.Hash]struct{}),
		inflightTx:    make(map[string]struct{}),
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
		s.requestBlock(fromPeer, meta.Header.PrevHash)
	}
	s.requestBlock(fromPeer, meta.Hash)
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
	s.mu.Lock()
	delete(s.inflightBlock, h)
	s.mu.Unlock()

	s.cfg.OnBlock(b, fromPeer)
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

func (s *Syncer) requestBlock(to string, h types.Hash) {
	if s.cfg.Transport == nil || s.cfg.Chain == nil {
		return
	}
	if to == "" || h.IsZero() {
		return
	}
	if s.cfg.Chain.HasBlock(h) {
		return
	}

	s.mu.Lock()
	if _, ok := s.inflightBlock[h]; ok {
		s.mu.Unlock()
		return
	}
	s.inflightBlock[h] = struct{}{}
	s.mu.Unlock()

	// 超时后释放 inflight，允许后续重试（窗口/更复杂重试策略留到下一步状态机增强）。
	time.AfterFunc(s.cfg.SyncTimeout, func() {
		s.mu.Lock()
		delete(s.inflightBlock, h)
		s.mu.Unlock()
	})

	s.cfg.Transport.Send(to, types.Message{
		Type:      types.MsgGetBlock,
		From:      s.cfg.NodeID,
		To:        to,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.GetBlockPayload{Hash: h},
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
}

func (s *Syncer) Stop() {
	s.startMu.Lock()
	if !s.started {
		s.startMu.Unlock()
		return
	}
	ch := s.stopCh
	done := s.doneCh
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
	for _, p := range peers {
		if p == "" || p == s.cfg.NodeID {
			continue
		}
		s.cfg.Peers.Upsert(p)

		start := time.Now()
		resp, ok := s.cfg.RPC(p, types.MsgGetTip, types.GetTipPayload{}, s.cfg.SyncTimeout)
		rtt := time.Since(start)
		if !ok || resp.Type != types.MsgTip {
			s.cfg.Peers.ReportTimeout(p)
			continue
		}
		pl, ok := resp.Payload.(types.TipPayload)
		if !ok {
			s.cfg.Peers.ReportError(p)
			continue
		}
		w := new(big.Int)
		if _, ok := w.SetString(pl.CumWork, 10); !ok {
			s.cfg.Peers.ReportError(p)
			continue
		}
		s.cfg.Peers.UpdateTip(p, pl.TipHeight, pl.TipHash, w, rtt)
	}
}

func (s *Syncer) catchUpIfBehind() {
	if s.cfg.Chain == nil {
		return
	}
	now := time.Now()
	bestPeer, bestHeight, _, bestWork := s.cfg.Peers.BestPeer(now)
	if bestPeer == "" || bestWork == nil {
		return
	}
	localWork := s.cfg.Chain.TipCumWork()
	if localWork.Cmp(bestWork) >= 0 && s.cfg.Chain.TipHeight() >= bestHeight {
		return
	}
	// Do a catch-up pass. If it fails, PeerManager will backoff this peer via report calls.
	s.InitialSyncFrom(bestPeer, bestHeight, bestWork)
}

// InitialSync pulls missing headers/blocks from the best available peer.
// It is intended for "late join" nodes that start with only genesis or lag behind.
func (s *Syncer) InitialSync() {
	if s.cfg.Transport == nil || s.cfg.Chain == nil || s.cfg.RPC == nil || s.cfg.OnBlock == nil {
		return
	}

	peers := s.cfg.Transport.Peers()
	candidates := make([]string, 0, len(peers))
	for _, p := range peers {
		if p != s.cfg.NodeID {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return
	}

	type tipInfo struct {
		peer   string
		height uint64
		work   *big.Int
		hash   types.Hash
	}

	var best tipInfo
	best.work = big.NewInt(-1)
	for _, p := range candidates {
		resp, ok := s.cfg.RPC(p, types.MsgGetTip, types.GetTipPayload{}, s.cfg.SyncTimeout)
		if !ok || resp.Type != types.MsgTip {
			continue
		}
		pl, ok := resp.Payload.(types.TipPayload)
		if !ok {
			continue
		}
		w := new(big.Int)
		if _, ok := w.SetString(pl.CumWork, 10); !ok {
			continue
		}
		if best.peer == "" || w.Cmp(best.work) > 0 || (w.Cmp(best.work) == 0 && pl.TipHeight > best.height) {
			best = tipInfo{peer: p, height: pl.TipHeight, work: w, hash: pl.TipHash}
		}
	}

	if best.peer == "" {
		return
	}

	s.InitialSyncFrom(best.peer, best.height, best.work)
}

// InitialSyncFrom performs a headers-first catch up from a specific peer snapshot.
func (s *Syncer) InitialSyncFrom(peerID string, peerHeight uint64, peerWork *big.Int) {
	if peerID == "" || s.cfg.Transport == nil || s.cfg.Chain == nil || s.cfg.RPC == nil || s.cfg.OnBlock == nil {
		return
	}
	if peerWork == nil {
		peerWork = big.NewInt(0)
	}

	for {
		// Allow Stop() to interrupt long sync runs.
		select {
		case <-s.stopCh:
			return
		default:
		}

		localWork := s.cfg.Chain.TipCumWork()
		if localWork.Cmp(peerWork) >= 0 && s.cfg.Chain.TipHeight() >= peerHeight {
			return
		}

		locator := s.cfg.Chain.Locator(32)
		resp, ok := s.cfg.RPC(peerID, types.MsgGetHeaders, types.GetHeadersPayload{Locator: locator, Max: s.cfg.MaxHeaders}, s.cfg.SyncTimeout)
		if !ok || resp.Type != types.MsgHeaders {
			s.cfg.Peers.ReportTimeout(peerID)
			return
		}
		hpl, ok := resp.Payload.(types.HeadersPayload)
		if !ok || len(hpl.Metas) == 0 {
			return
		}

		hashes := make([]types.Hash, 0, len(hpl.Metas))
		for _, m := range hpl.Metas {
			if m.Hash.IsZero() || s.cfg.Chain.HasBlock(m.Hash) {
				continue
			}
			hashes = append(hashes, m.Hash)
		}

		for i := 0; i < len(hashes); i += s.cfg.BlocksBatchSize {
			end := i + s.cfg.BlocksBatchSize
			if end > len(hashes) {
				end = len(hashes)
			}
			bresp, ok := s.cfg.RPC(peerID, types.MsgGetBlocks, types.GetBlocksPayload{Hashes: hashes[i:end]}, s.cfg.SyncTimeout)
			if !ok || bresp.Type != types.MsgBlocks {
				s.cfg.Peers.ReportTimeout(peerID)
				return
			}
			bpl, ok := bresp.Payload.(types.BlocksPayload)
			if !ok {
				s.cfg.Peers.ReportError(peerID)
				return
			}
			for _, b := range bpl.Blocks {
				if b != nil {
					s.cfg.OnBlock(b, peerID)
				}
			}
		}
	}
}
