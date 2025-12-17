package node

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"simchain-go/internal/blockchain"
	"simchain-go/internal/crypto"
	"simchain-go/internal/mempool"
	"simchain-go/internal/syncer"
	"simchain-go/internal/transport"
	"simchain-go/internal/types"
)

type Config struct {
	Difficulty    uint32
	MaxTxPerBlock int
	MinerSleep    time.Duration

	// SyncTimeout bounds request/response waits during initial sync.
	SyncTimeout time.Duration
	// MaxHeaders limits headers returned per GetHeaders request.
	MaxHeaders int
	// GetBlocksBatchSize controls how many block hashes are requested per GetBlocks call.
	GetBlocksBatchSize int

	// OnTipChange is called after this node's main-chain tip changes and mempool has been adjusted.
	// Intended for debugging/observability from the runner.
	OnTipChange func(ev TipChangeEvent)
}

type TipChangeEvent struct {
	NodeID   string
	FromPeer string
	Height   uint64
	TipHash  types.Hash
	Reorg    *blockchain.Reorg
}

type DebugBlock struct {
	Height  uint64
	Hash    types.Hash
	TxCount int
	TxIDs   []string
}

type DebugState struct {
	NodeID       string
	TipHeight    uint64
	TipHash      types.Hash
	MempoolSize  int
	MempoolTxIDs []string
	MainChain    []DebugBlock // oldest->newest, truncated by requested depth
}

type Node struct {
	id           string
	chain        *blockchain.Blockchain
	mempool      *mempool.Mempool
	tr           transport.Transport
	difficulty   uint32
	maxTxPerBlk  int
	minerSleep   time.Duration
	syncTimeout  time.Duration
	maxHeaders   int
	blocksBatch  int
	onTipChange  func(ev TipChangeEvent)
	syncer       *syncer.Syncer
	stopCh       chan struct{}
	doneCh       chan struct{}
	stopOnce     sync.Once
	minedBlocks  uint64
	recvBlocks   uint64
	hashAttempts uint64

	mu            sync.Mutex
	knownTxIDs    map[string]struct{}
	txStore       map[string]types.Transaction
	inflightTx    map[string]struct{}
	inflightBlock map[types.Hash]struct{}

	pendingRPC map[string]chan types.Message
	rpcSeq     uint64
}

func NewNode(id string, chain *blockchain.Blockchain, mp *mempool.Mempool, tr transport.Transport, cfg Config) *Node {
	if mp == nil {
		mp = mempool.NewMempool()
	}
	n := &Node{
		id:            id,
		chain:         chain,
		mempool:       mp,
		tr:            tr,
		difficulty:    cfg.Difficulty,
		maxTxPerBlk:   cfg.MaxTxPerBlock,
		minerSleep:    cfg.MinerSleep,
		syncTimeout:   cfg.SyncTimeout,
		maxHeaders:    cfg.MaxHeaders,
		blocksBatch:   cfg.GetBlocksBatchSize,
		onTipChange:   cfg.OnTipChange,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
		knownTxIDs:    make(map[string]struct{}),
		txStore:       make(map[string]types.Transaction),
		inflightTx:    make(map[string]struct{}),
		inflightBlock: make(map[types.Hash]struct{}),
		pendingRPC:    make(map[string]chan types.Message),
	}
	if n.maxTxPerBlk <= 0 {
		n.maxTxPerBlk = 50
	}
	if n.syncTimeout <= 0 {
		n.syncTimeout = 2 * time.Second
	}
	if n.maxHeaders <= 0 {
		n.maxHeaders = 2000
	}
	if n.blocksBatch <= 0 {
		n.blocksBatch = 64
	}
	if n.tr != nil {
		n.tr.Register(n.id, n.HandleMessage)
	}

	// V3-A：把“追链同步”抽成独立 Syncer。当前先迁移 initial sync，
	// 后续再把 inv/get 的收发策略也逐步迁出 Node。
	n.syncer = syncer.New(syncer.Config{
		NodeID:          n.id,
		Transport:       n.tr,
		Chain:           n.chain,
		SyncTimeout:     n.syncTimeout,
		MaxHeaders:      n.maxHeaders,
		BlocksBatchSize: n.blocksBatch,
		RPC:             n.rpc,
		OnBlock:         n.onNewBlock,
	})

	return n
}

func (n *Node) ID() string { return n.id }

func (n *Node) StartMining() {
	go n.minerLoop()
}

func (n *Node) StopMining() {
	n.stopOnce.Do(func() {
		close(n.stopCh)
	})
	<-n.doneCh
}

func (n *Node) Stats() (mined uint64, received uint64, height uint64) {
	mined = atomic.LoadUint64(&n.minedBlocks)
	received = atomic.LoadUint64(&n.recvBlocks)
	if n.chain != nil {
		height = n.chain.TipHeight()
	}
	return
}

func (n *Node) HashAttempts() uint64 {
	return atomic.LoadUint64(&n.hashAttempts)
}

func (n *Node) SubmitTransaction(payload string) {
	tx := types.NewTransaction(payload)
	if !n.mempool.Add(tx) {
		return
	}
	n.rememberTx(tx)
	log.Printf("TX_ACCEPTED node=%s tx=%s", n.id, tx.ID)
	if n.tr == nil {
		return
	}
	msg := types.Message{
		Type:      types.MsgInvTx,
		From:      n.id,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.InvTxPayload{TxID: tx.ID},
	}
	// V2/V3：只广播 txid（InvTx），完整交易通过 GetTx/Tx 按需拉取。
	n.tr.Broadcast(n.id, msg)
}

func (n *Node) HandleMessage(msg types.Message) {
	if n.tryDeliverRPC(msg) {
		return
	}

	switch msg.Type {
	case types.MsgInvTx:
		pl, ok := msg.Payload.(types.InvTxPayload)
		if !ok {
			return
		}
		n.onInvTx(pl.TxID, msg.From)
	case types.MsgGetTx:
		pl, ok := msg.Payload.(types.GetTxPayload)
		if !ok {
			return
		}
		n.onGetTx(pl.TxID, msg.From)
	case types.MsgTx:
		pl, ok := msg.Payload.(types.TxPayload)
		if !ok {
			return
		}
		n.onTx(pl.Tx, msg.From)
	case types.MsgInvBlock:
		pl, ok := msg.Payload.(types.InvBlockPayload)
		if !ok {
			return
		}
		n.onInvBlock(pl.Meta, msg.From)
	case types.MsgGetBlock:
		pl, ok := msg.Payload.(types.GetBlockPayload)
		if !ok {
			return
		}
		n.onGetBlock(pl.Hash, msg.From)
	case types.MsgBlock:
		pl, ok := msg.Payload.(types.BlockPayload)
		if !ok || pl.Block == nil {
			return
		}
		n.onBlock(pl.Block, msg.From)
	case types.MsgGetTip:
		n.onGetTip(msg.From, msg.TraceID)
	case types.MsgGetHeaders:
		pl, ok := msg.Payload.(types.GetHeadersPayload)
		if !ok {
			return
		}
		n.onGetHeaders(pl, msg.From, msg.TraceID)
	case types.MsgGetBlocks:
		pl, ok := msg.Payload.(types.GetBlocksPayload)
		if !ok {
			return
		}
		n.onGetBlocks(pl, msg.From, msg.TraceID)
	default:
	}
}

func (n *Node) onNewBlock(b *types.Block, from string) {
	res, err := n.chain.AddBlock(b)
	if err != nil {
		return
	}
	if !res.Added {
		return
	}

	atomic.AddUint64(&n.recvBlocks, 1)
	h := b.Hash
	if h.IsZero() {
		if computed, err := crypto.HashHeaderNonce(b.Header, b.Nonce); err == nil {
			h = computed
		}
	}
	log.Printf("BLOCK_RECEIVED node=%s from=%s h=%d hash=%s", n.id, from, b.Header.Height, h.String())

	if res.Reorg != nil {
		n.applyReorg(res.Reorg)
	} else if res.IsNewTip {
		// Only remove txs for blocks that become part of the main chain.
		n.removeTxsFromBlock(b)
	}

	// Announce new tip blocks as inventory (not full data).
	if res.IsNewTip && n.tr != nil {
		meta := types.BlockMeta{Header: b.Header, Nonce: b.Nonce, Hash: h}
		n.tr.Broadcast(n.id, types.Message{
			Type:      types.MsgInvBlock,
			From:      n.id,
			Timestamp: time.Now().UnixMilli(),
			Payload:   types.InvBlockPayload{Meta: meta},
		})
	}

	if res.IsNewTip && n.onTipChange != nil {
		n.onTipChange(TipChangeEvent{
			NodeID:   n.id,
			FromPeer: from,
			Height:   n.chain.TipHeight(),
			TipHash:  n.chain.TipHash(),
			Reorg:    res.Reorg,
		})
	}
}

func (n *Node) minerLoop() {
	defer close(n.doneCh)
	log.Printf("MINER_START node=%s difficulty=%d", n.id, n.difficulty)

	for {
		select {
		case <-n.stopCh:
			return
		default:
		}

		tipHash := n.chain.TipHash()
		tipHeight := n.chain.TipHeight()
		txs := n.mempool.Pop(n.maxTxPerBlk)

		header := types.BlockHeader{
			Height:     tipHeight + 1,
			PrevHash:   tipHash,
			Timestamp:  time.Now().UnixMilli(),
			Difficulty: n.difficulty,
			MinerID:    n.id,
			TxRoot:     crypto.HashTransactions(txs),
		}

		var nonce uint64
		var attempts uint64
		var stopping bool
		var minedBlock *types.Block
		var minedRes blockchain.AddResult
		for {
			select {
			case <-n.stopCh:
				stopping = true
			default:
			}
			if stopping {
				break
			}
			attempts++
			h, err := crypto.HashHeaderNonce(header, nonce)
			if err != nil {
				nonce++
				continue
			}
			if crypto.MeetsDifficulty(h, n.difficulty) {
				// Stale work check.
				if n.chain.TipHash() != tipHash {
					n.mempool.Return(txs)
					break
				}

				block := &types.Block{
					Header: header,
					Nonce:  nonce,
					Hash:   h,
					Txs:    txs,
				}

				res, err := n.chain.AddBlock(block)
				if err != nil || !res.Added {
					n.mempool.Return(txs)
					break
				}

				if res.Reorg != nil {
					n.applyReorg(res.Reorg)
				}
				if res.IsNewTip {
					n.removeTxsFromBlock(block)
				}

				minedBlock = block
				minedRes = res
				atomic.AddUint64(&n.minedBlocks, 1)
				log.Printf("BLOCK_MINED node=%s h=%d hash=%s nonce=%d txs=%d", n.id, header.Height, block.Hash.String(), nonce, len(txs))

				if res.IsNewTip && n.onTipChange != nil {
					n.onTipChange(TipChangeEvent{
						NodeID:   n.id,
						FromPeer: n.id,
						Height:   n.chain.TipHeight(),
						TipHash:  n.chain.TipHash(),
						Reorg:    res.Reorg,
					})
				}

				break
			}
			nonce++
		}

		if attempts > 0 {
			atomic.AddUint64(&n.hashAttempts, attempts)
		}
		if stopping {
			n.mempool.Return(txs)
			return
		}

		// Announce newly mined tip blocks (after counting attempts and handling stop paths).
		if minedBlock != nil && minedRes.IsNewTip && n.tr != nil {
			meta := types.BlockMeta{Header: minedBlock.Header, Nonce: minedBlock.Nonce, Hash: minedBlock.Hash}
			n.tr.Broadcast(n.id, types.Message{
				Type:      types.MsgInvBlock,
				From:      n.id,
				Timestamp: time.Now().UnixMilli(),
				Payload:   types.InvBlockPayload{Meta: meta},
			})
		}

		if n.minerSleep > 0 {
			time.Sleep(n.minerSleep)
		}
	}
}

func (n *Node) applyReorg(r *blockchain.Reorg) {
	if r == nil {
		return
	}
	for _, b := range r.Removed {
		n.mempool.Return(b.Txs)
	}
	ids := make(map[string]struct{})
	for _, b := range r.Added {
		for _, tx := range b.Txs {
			ids[tx.ID] = struct{}{}
		}
	}
	n.mempool.RemoveByID(ids)
	oldTip := ""
	newTip := ""
	if len(r.Removed) > 0 {
		oldTip = r.Removed[len(r.Removed)-1].Hash.String()
	}
	if len(r.Added) > 0 {
		newTip = r.Added[len(r.Added)-1].Hash.String()
	}
	log.Printf("CHAIN_SWITCH node=%s oldTip=%s newTip=%s forkDepth=%d", n.id, oldTip, newTip, len(r.Removed))
}

func (n *Node) removeTxsFromBlock(b *types.Block) {
	if b == nil || len(b.Txs) == 0 {
		return
	}
	ids := make(map[string]struct{}, len(b.Txs))
	for _, tx := range b.Txs {
		ids[tx.ID] = struct{}{}
	}
	n.mempool.RemoveByID(ids)
}

func (n *Node) DebugState(chainDepth int, mempoolMax int, blockTxMax int) DebugState {
	out := DebugState{
		NodeID:      n.id,
		MempoolSize: n.mempool.Size(),
	}
	if n.chain != nil {
		out.TipHeight = n.chain.TipHeight()
		out.TipHash = n.chain.TipHash()
	}

	txs := n.mempool.Snapshot(mempoolMax)
	out.MempoolTxIDs = make([]string, 0, len(txs))
	for _, tx := range txs {
		out.MempoolTxIDs = append(out.MempoolTxIDs, tx.ID)
	}

	if n.chain != nil && chainDepth != 0 {
		blocks := n.chain.MainChainBlocks(chainDepth)
		out.MainChain = make([]DebugBlock, 0, len(blocks))
		for _, b := range blocks {
			if b == nil {
				continue
			}
			db := DebugBlock{Height: b.Header.Height, Hash: b.Hash}
			db.TxCount = len(b.Txs)
			if len(b.Txs) > 0 && blockTxMax != 0 {
				txMax := blockTxMax
				if txMax < 0 || txMax > len(b.Txs) {
					txMax = len(b.Txs)
				}
				db.TxIDs = make([]string, 0, txMax)
				for i := 0; i < txMax; i++ {
					db.TxIDs = append(db.TxIDs, b.Txs[i].ID)
				}
			}
			out.MainChain = append(out.MainChain, db)
		}
	}

	return out
}

func (n *Node) rememberTx(tx types.Transaction) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.knownTxIDs[tx.ID] = struct{}{}
	n.txStore[tx.ID] = tx
}

func (n *Node) hasTx(txid string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.knownTxIDs[txid]
	return ok
}

func (n *Node) markInflightTx(txid string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.inflightTx[txid]; ok {
		return false
	}
	n.inflightTx[txid] = struct{}{}
	return true
}

func (n *Node) clearInflightTx(txid string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.inflightTx, txid)
}

func (n *Node) markInflightBlock(h types.Hash) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.inflightBlock[h]; ok {
		return false
	}
	n.inflightBlock[h] = struct{}{}
	return true
}

func (n *Node) clearInflightBlock(h types.Hash) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.inflightBlock, h)
}

func (n *Node) onInvTx(txid string, from string) {
	if txid == "" || from == "" || n.tr == nil {
		return
	}
	if n.hasTx(txid) {
		return
	}
	if !n.markInflightTx(txid) {
		return
	}
	time.AfterFunc(n.syncTimeout, func() {
		n.clearInflightTx(txid)
	})
	n.tr.Send(from, types.Message{
		Type:      types.MsgGetTx,
		From:      n.id,
		To:        from,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.GetTxPayload{TxID: txid},
	})
}

func (n *Node) onGetTx(txid string, requester string) {
	if txid == "" || requester == "" || n.tr == nil {
		return
	}
	n.mu.Lock()
	tx, ok := n.txStore[txid]
	n.mu.Unlock()
	if !ok {
		return
	}
	n.tr.Send(requester, types.Message{
		Type:      types.MsgTx,
		From:      n.id,
		To:        requester,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.TxPayload{Tx: tx},
	})
}

func (n *Node) onTx(tx types.Transaction, from string) {
	if tx.ID == "" {
		return
	}
	n.clearInflightTx(tx.ID)
	if n.hasTx(tx.ID) {
		return
	}
	if !n.mempool.Add(tx) {
		return
	}
	n.rememberTx(tx)
	log.Printf("TX_ACCEPTED node=%s tx=%s (from=%s)", n.id, tx.ID, from)
	if n.tr == nil {
		return
	}
	n.tr.Broadcast(n.id, types.Message{
		Type:      types.MsgInvTx,
		From:      n.id,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.InvTxPayload{TxID: tx.ID},
	})
}

func (n *Node) onInvBlock(meta types.BlockMeta, from string) {
	if from == "" || n.tr == nil || n.chain == nil {
		return
	}
	if meta.Hash.IsZero() {
		return
	}
	if n.chain.HasBlock(meta.Hash) {
		return
	}
	if meta.Header.Difficulty != n.difficulty {
		return
	}
	// Validate PoW from header+nonce without downloading full block.
	computed, err := crypto.HashHeaderNonce(meta.Header, meta.Nonce)
	if err != nil || computed != meta.Hash || !crypto.MeetsDifficulty(meta.Hash, n.difficulty) {
		return
	}

	// If parent is missing, request it first (best-effort).
	if meta.Header.Height > 0 && !n.chain.HasBlock(meta.Header.PrevHash) && !meta.Header.PrevHash.IsZero() {
		n.requestBlock(from, meta.Header.PrevHash)
	}
	n.requestBlock(from, meta.Hash)
}

func (n *Node) requestBlock(to string, h types.Hash) {
	if to == "" || n.tr == nil || h.IsZero() {
		return
	}
	if !n.markInflightBlock(h) {
		return
	}
	time.AfterFunc(n.syncTimeout, func() {
		n.clearInflightBlock(h)
	})
	n.tr.Send(to, types.Message{
		Type:      types.MsgGetBlock,
		From:      n.id,
		To:        to,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.GetBlockPayload{Hash: h},
	})
}

func (n *Node) onGetBlock(h types.Hash, requester string) {
	if requester == "" || n.tr == nil || n.chain == nil {
		return
	}
	b, ok := n.chain.BlockByHash(h)
	if !ok || b == nil {
		return
	}
	cp := *b
	if b.Txs != nil {
		cp.Txs = append([]types.Transaction(nil), b.Txs...)
	}
	n.tr.Send(requester, types.Message{
		Type:      types.MsgBlock,
		From:      n.id,
		To:        requester,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.BlockPayload{Block: &cp},
	})
}

func (n *Node) onBlock(b *types.Block, from string) {
	if b == nil || n.chain == nil {
		return
	}
	n.clearInflightBlock(b.Hash)
	n.onNewBlock(b, from)
}

func (n *Node) onGetTip(requester string, traceID string) {
	if requester == "" || traceID == "" || n.tr == nil || n.chain == nil {
		return
	}
	n.tr.Send(requester, types.Message{
		Type:      types.MsgTip,
		From:      n.id,
		To:        requester,
		TraceID:   traceID,
		Timestamp: time.Now().UnixMilli(),
		Payload: types.TipPayload{
			TipHash:   n.chain.TipHash(),
			TipHeight: n.chain.TipHeight(),
			CumWork:   n.chain.TipCumWork().String(),
		},
	})
}

func (n *Node) onGetHeaders(req types.GetHeadersPayload, requester string, traceID string) {
	if requester == "" || traceID == "" || n.tr == nil || n.chain == nil {
		return
	}
	m := req.Max
	if m <= 0 || m > n.maxHeaders {
		m = n.maxHeaders
	}
	metas := n.chain.MainChainMetasFromLocator(req.Locator, m)
	n.tr.Send(requester, types.Message{
		Type:      types.MsgHeaders,
		From:      n.id,
		To:        requester,
		TraceID:   traceID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.HeadersPayload{Metas: metas},
	})
}

func (n *Node) onGetBlocks(req types.GetBlocksPayload, requester string, traceID string) {
	if requester == "" || traceID == "" || n.tr == nil || n.chain == nil {
		return
	}
	blocks := make([]*types.Block, 0, len(req.Hashes))
	for _, h := range req.Hashes {
		b, ok := n.chain.BlockByHash(h)
		if !ok || b == nil {
			continue
		}
		cp := *b
		if b.Txs != nil {
			cp.Txs = append([]types.Transaction(nil), b.Txs...)
		}
		blocks = append(blocks, &cp)
	}
	n.tr.Send(requester, types.Message{
		Type:      types.MsgBlocks,
		From:      n.id,
		To:        requester,
		TraceID:   traceID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   types.BlocksPayload{Blocks: blocks},
	})
}

func (n *Node) tryDeliverRPC(msg types.Message) bool {
	if msg.TraceID == "" {
		return false
	}
	n.mu.Lock()
	ch, ok := n.pendingRPC[msg.TraceID]
	if ok {
		delete(n.pendingRPC, msg.TraceID)
	}
	n.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- msg:
	default:
	}
	return true
}

func (n *Node) rpc(to string, typ types.MessageType, payload any, timeout time.Duration) (types.Message, bool) {
	if n.tr == nil || to == "" {
		return types.Message{}, false
	}
	if timeout <= 0 {
		timeout = n.syncTimeout
	}
	traceID := fmt.Sprintf("%s-%d", n.id, atomic.AddUint64(&n.rpcSeq, 1))
	ch := make(chan types.Message, 1)
	n.mu.Lock()
	n.pendingRPC[traceID] = ch
	n.mu.Unlock()

	n.tr.Send(to, types.Message{
		Type:      typ,
		From:      n.id,
		To:        to,
		TraceID:   traceID,
		Timestamp: time.Now().UnixMilli(),
		Payload:   payload,
	})

	select {
	case resp := <-ch:
		return resp, true
	case <-time.After(timeout):
		n.mu.Lock()
		delete(n.pendingRPC, traceID)
		n.mu.Unlock()
		return types.Message{}, false
	}
}

// InitialSync pulls missing headers/blocks from the best available peer.
// It is intended for "late join" nodes that start with only genesis.
func (n *Node) InitialSync() {
	// V3-A：initial sync 由 Syncer 负责，Node 只做编排。
	if n.syncer == nil {
		return
	}
	log.Printf("SYNC_START node=%s", n.id)
	n.syncer.InitialSync()
	log.Printf("SYNC_DONE node=%s height=%d tip=%s", n.id, n.chain.TipHeight(), n.chain.TipHash().String())
}
