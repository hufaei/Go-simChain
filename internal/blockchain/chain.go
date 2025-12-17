package blockchain

import (
	"errors"
	"fmt"
	"math/big"
	"sync"

	"simchain-go/internal/crypto"
	"simchain-go/internal/types"
)

type Reorg struct {
	CommonAncestor types.Hash
	Removed        []*types.Block // old main chain blocks removed, oldest->newest
	Added          []*types.Block // new main chain blocks added, oldest->newest
}

type AddResult struct {
	Added    bool
	IsNewTip bool
	OldTip   types.Hash
	NewTip   types.Hash
	Reorg    *Reorg
}

type Blockchain struct {
	mu         sync.Mutex
	difficulty uint32

	blocks  map[types.Hash]*types.Block
	parent  map[types.Hash]types.Hash
	cumWork map[types.Hash]*big.Int
	height  map[types.Hash]uint64

	tip     types.Hash
	genesis types.Hash

	orphans map[types.Hash]*types.Block
}

// NewBlockchain initializes a chain with a shared deterministic genesis block.
func NewBlockchain(genesis *types.Block, difficulty uint32) (*Blockchain, error) {
	if genesis == nil {
		return nil, errors.New("nil genesis")
	}

	g := cloneBlock(genesis)
	g.Header.TxRoot = crypto.HashTransactions(g.Txs)
	h, err := crypto.HashHeaderNonce(g.Header, g.Nonce)
	if err != nil {
		return nil, err
	}
	g.Hash = h

	bc := &Blockchain{
		difficulty: difficulty,
		blocks:     make(map[types.Hash]*types.Block),
		parent:     make(map[types.Hash]types.Hash),
		cumWork:    make(map[types.Hash]*big.Int),
		height:     make(map[types.Hash]uint64),
		orphans:    make(map[types.Hash]*types.Block),
	}

	bc.blocks[h] = g
	bc.parent[h] = types.Hash{}
	bc.height[h] = 0
	bc.cumWork[h] = big.NewInt(0)
	bc.tip = h
	bc.genesis = h

	return bc, nil
}

func (bc *Blockchain) Difficulty() uint32 {
	return bc.difficulty
}

func (bc *Blockchain) TipHash() types.Hash {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.tip
}

func (bc *Blockchain) TipHeight() uint64 {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.height[bc.tip]
}

func (bc *Blockchain) GetTip() *types.Block {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.blocks[bc.tip]
}

func (bc *Blockchain) TipCumWork() *big.Int {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return new(big.Int).Set(bc.cumWork[bc.tip])
}

func (bc *Blockchain) BlockByHash(h types.Hash) (*types.Block, bool) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	b, ok := bc.blocks[h]
	return b, ok
}

func (bc *Blockchain) HasBlock(h types.Hash) bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	_, ok := bc.blocks[h]
	return ok
}

// AddBlock validates and adds a block. If its parent is unknown, it is stored as orphan.
func (bc *Blockchain) AddBlock(b *types.Block) (AddResult, error) {
	bc.mu.Lock()
	defer bc.mu.Unlock()

	res, err := bc.addBlockUnlocked(b)
	if err != nil {
		return res, err
	}

	bc.tryConnectOrphansUnlocked()
	return res, nil
}

func (bc *Blockchain) addBlockUnlocked(b *types.Block) (AddResult, error) {
	res := AddResult{OldTip: bc.tip, NewTip: bc.tip}
	if b == nil {
		return res, errors.New("nil block")
	}

	// Compute hash if missing.
	h := b.Hash
	if h.IsZero() {
		var err error
		h, err = crypto.HashHeaderNonce(b.Header, b.Nonce)
		if err != nil {
			return res, err
		}
	}

	// Reject unknown genesis blocks.
	if b.Header.Height == 0 && h != bc.genesis {
		return res, fmt.Errorf("unexpected genesis block: %s", h.String())
	}

	if _, ok := bc.blocks[h]; ok {
		return res, nil
	}

	if b.Header.Height > 0 {
		parentHash := b.Header.PrevHash
		parentBlock, ok := bc.blocks[parentHash]
		if !ok {
			bc.orphans[h] = cloneBlock(b)
			return res, nil
		}

		if err := bc.validateBlockUnlocked(b, parentBlock, h); err != nil {
			return res, err
		}

		stored := cloneBlock(b)
		stored.Hash = h
		bc.blocks[h] = stored
		bc.parent[h] = parentHash
		bc.height[h] = b.Header.Height

		work := crypto.WorkForDifficulty(b.Header.Difficulty)
		bc.cumWork[h] = new(big.Int).Add(bc.cumWork[parentHash], work)

		if bc.cumWork[h].Cmp(bc.cumWork[bc.tip]) > 0 {
			oldTip := bc.tip
			bc.tip = h
			res.Added = true
			res.IsNewTip = true
			res.OldTip = oldTip
			res.NewTip = h
			reorg := bc.computeReorgUnlocked(oldTip, h)
			// Treat a pure tip extension as non-reorg (Removed is empty). Reorg is reserved
			// for actual chain switches that remove blocks from the previous main chain.
			if reorg != nil && len(reorg.Removed) == 0 {
				reorg = nil
			}
			res.Reorg = reorg
			return res, nil
		}

		res.Added = true
		res.NewTip = bc.tip
		return res, nil
	}

	// Height == 0 (known genesis) handled above.
	return res, nil
}

func (bc *Blockchain) validateBlockUnlocked(b *types.Block, parent *types.Block, h types.Hash) error {
	if b.Header.Difficulty != bc.difficulty {
		return fmt.Errorf("difficulty mismatch: got %d want %d", b.Header.Difficulty, bc.difficulty)
	}
	if b.Header.Height != parent.Header.Height+1 {
		return fmt.Errorf("height mismatch: got %d want %d", b.Header.Height, parent.Header.Height+1)
	}
	if b.Header.PrevHash != parent.Hash {
		return fmt.Errorf("prev hash mismatch")
	}
	if b.Header.Timestamp < parent.Header.Timestamp {
		return fmt.Errorf("timestamp regression")
	}

	txRoot := crypto.HashTransactions(b.Txs)
	if txRoot != b.Header.TxRoot {
		return fmt.Errorf("tx root mismatch")
	}

	computed, err := crypto.HashHeaderNonce(b.Header, b.Nonce)
	if err != nil {
		return err
	}
	if computed != h {
		return fmt.Errorf("hash mismatch")
	}
	if !crypto.MeetsDifficulty(h, bc.difficulty) {
		return fmt.Errorf("pow does not meet difficulty")
	}

	return nil
}

func (bc *Blockchain) tryConnectOrphansUnlocked() {
	for {
		progressed := false
		for oh, ob := range bc.orphans {
			if _, ok := bc.blocks[ob.Header.PrevHash]; !ok {
				continue
			}
			delete(bc.orphans, oh)
			_, err := bc.addBlockUnlocked(ob)
			if err == nil {
				progressed = true
			}
		}
		if !progressed {
			return
		}
	}
}

func (bc *Blockchain) computeReorgUnlocked(oldTip, newTip types.Hash) *Reorg {
	// computeReorgUnlocked 只负责计算“差异段”：
	// - CommonAncestor：两条链的共同祖先
	// - Removed：旧主链上会被移除的那段块
	// - Added：新主链上会被接入的那段块
	// 具体如何修正 mempool 由 Node 层处理（退回 Removed 的交易，删除 Added 的交易）。
	if oldTip == newTip {
		return nil
	}

	oh := bc.height[oldTip]
	nh := bc.height[newTip]
	o := oldTip
	n := newTip

	for oh > nh {
		o = bc.parent[o]
		oh--
	}
	for nh > oh {
		n = bc.parent[n]
		nh--
	}
	for o != n {
		o = bc.parent[o]
		n = bc.parent[n]
	}
	common := o

	removed := make([]*types.Block, 0)
	for h := oldTip; h != common; h = bc.parent[h] {
		removed = append(removed, bc.blocks[h])
	}
	added := make([]*types.Block, 0)
	for h := newTip; h != common; h = bc.parent[h] {
		added = append(added, bc.blocks[h])
	}

	reverseBlocks(removed)
	reverseBlocks(added)

	return &Reorg{
		CommonAncestor: common,
		Removed:        removed,
		Added:          added,
	}
}

func reverseBlocks(bs []*types.Block) {
	for i, j := 0, len(bs)-1; i < j; i, j = i+1, j-1 {
		bs[i], bs[j] = bs[j], bs[i]
	}
}

func cloneBlock(b *types.Block) *types.Block {
	if b == nil {
		return nil
	}
	cp := *b
	if b.Txs != nil {
		cp.Txs = append([]types.Transaction(nil), b.Txs...)
	}
	return &cp
}

func (bc *Blockchain) Locator(max int) []types.Hash {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.locatorUnlocked(max)
}

func (bc *Blockchain) locatorUnlocked(max int) []types.Hash {
	// locator 是 headers-first 同步的常用技巧：
	// 从 tip 往回取若干个 hash，前面步长小、后面指数增大，
	// 这样既能快速定位共同祖先，又不会传输过多数据。
	if max <= 0 {
		max = 32
	}
	out := make([]types.Hash, 0, max)
	h := bc.tip
	height := bc.height[h]
	step := uint64(1)
	for len(out) < max {
		out = append(out, h)
		if h == bc.genesis || height == 0 {
			break
		}
		for i := uint64(0); i < step && height > 0; i++ {
			h = bc.parent[h]
			height--
		}
		if len(out) >= 10 {
			step *= 2
		}
	}
	return out
}

func (bc *Blockchain) MainChainMetasFromLocator(locator []types.Hash, max int) []types.BlockMeta {
	// 给定对方的 locator，返回“共同祖先之后”的主链 BlockMeta（header+nonce+hash）。
	// 这相当于极简版的 headers-first：对方用 metas 知道自己缺哪些 blockhash，再去拉完整区块。
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if max <= 0 {
		max = 2000
	}

	main := bc.mainChainHashesUnlocked()
	if len(main) == 0 {
		return nil
	}

	index := make(map[types.Hash]int, len(main))
	for i, h := range main {
		index[h] = i
	}

	commonIndex := 0 // default to genesis
	for _, h := range locator {
		if i, ok := index[h]; ok {
			commonIndex = i
			break
		}
	}

	start := commonIndex + 1
	if start >= len(main) {
		return nil
	}
	end := start + max
	if end > len(main) {
		end = len(main)
	}

	metas := make([]types.BlockMeta, 0, end-start)
	for _, h := range main[start:end] {
		b := bc.blocks[h]
		if b == nil {
			continue
		}
		metas = append(metas, types.BlockMeta{Header: b.Header, Nonce: b.Nonce, Hash: b.Hash})
	}
	return metas
}

func (bc *Blockchain) mainChainHashesUnlocked() []types.Hash {
	if bc.tip.IsZero() {
		return nil
	}
	reversed := make([]types.Hash, 0, 64)
	for h := bc.tip; ; h = bc.parent[h] {
		reversed = append(reversed, h)
		if h == bc.genesis {
			break
		}
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

// MainChainBlocks returns up to limit blocks from the current main chain, oldest->newest.
// If limit <= 0, it returns the full main chain.
func (bc *Blockchain) MainChainBlocks(limit int) []*types.Block {
	// 调试/可观测性用途：返回当前主链上的区块（带交易），用于打印“确认后的数据”。
	bc.mu.Lock()
	defer bc.mu.Unlock()

	hashes := bc.mainChainHashesUnlocked()
	if len(hashes) == 0 {
		return nil
	}
	if limit > 0 && limit < len(hashes) {
		hashes = hashes[len(hashes)-limit:]
	}
	out := make([]*types.Block, 0, len(hashes))
	for _, h := range hashes {
		if b := bc.blocks[h]; b != nil {
			out = append(out, cloneBlock(b))
		}
	}
	return out
}
