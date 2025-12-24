package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"simchain-go/internal/blockchain"
	"simchain-go/internal/crypto"
	"simchain-go/internal/types"
)

type Manifest struct {
	Version    int        `json:"version"`
	Difficulty uint32     `json:"difficulty"`
	TipHeight  uint64     `json:"tipHeight"`
	TipHash    types.Hash `json:"tipHash"`
	UpdatedAt  int64      `json:"updatedAt"` // unix ms
}

// Store persists only the current main chain (V3-A):
// - blocks are stored by height
// - forks/orphans are NOT persisted
// - mempool is NOT persisted
type Store struct {
	dir          string
	blocksDir    string
	manifestPath string

	mu       sync.Mutex
	manifest Manifest
}

func Open(dir string, difficulty uint32, genesis *types.Block) (*Store, error) {
	s := &Store{
		dir:          dir,
		blocksDir:    filepath.Join(dir, "blocks"),
		manifestPath: filepath.Join(dir, "manifest.json"),
	}

	if err := os.MkdirAll(s.blocksDir, 0o755); err != nil {
		return nil, err
	}

	if _, err := os.Stat(s.manifestPath); err == nil {
		if err := s.loadManifest(); err != nil {
			return nil, err
		}
		if s.manifest.Difficulty != difficulty {
			return nil, fmt.Errorf("store difficulty mismatch: got %d want %d", s.manifest.Difficulty, difficulty)
		}
		return s, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if err := s.initNew(difficulty, genesis); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) loadManifest() error {
	raw, err := os.ReadFile(s.manifestPath)
	if err != nil {
		return err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	s.manifest = m
	return nil
}

func (s *Store) initNew(difficulty uint32, genesis *types.Block) error {
	g := cloneBlock(genesis)
	g.Header.TxRoot = crypto.HashTransactions(g.Txs)
	h, err := crypto.HashHeaderNonce(g.Header, g.Nonce)
	if err != nil {
		return err
	}
	g.Hash = h

	if err := s.writeBlockFile(g); err != nil {
		return err
	}

	s.manifest = Manifest{
		Version:    1,
		Difficulty: difficulty,
		TipHeight:  0,
		TipHash:    g.Hash,
		UpdatedAt:  time.Now().UnixMilli(),
	}
	return s.saveManifest()
}

func (s *Store) saveManifest() error {
	s.manifest.UpdatedAt = time.Now().UnixMilli()
	raw, err := json.MarshalIndent(s.manifest, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.manifestPath, raw, 0o644)
}

// LoadMainChain loads the persisted main chain blocks from height 0..tipHeight (inclusive),
// ordered oldest->newest.
func (s *Store) LoadMainChain() ([]*types.Block, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*types.Block, 0, s.manifest.TipHeight+1)
	for h := uint64(0); h <= s.manifest.TipHeight; h++ {
		b, err := s.readBlockFile(h)
		if err != nil {
			// 容错：若磁盘上缺失某个高度的块文件（例如上次写入中断），
			// 则按“已存在的最后连续高度”截断，并修正 manifest，剩余部分通过网络同步补齐。
			if os.IsNotExist(err) {
				if h == 0 {
					return nil, err
				}
				last := h - 1
				s.manifest.TipHeight = last
				if len(out) > 0 && out[len(out)-1] != nil {
					s.manifest.TipHash = out[len(out)-1].Hash
				}
				_ = s.saveManifest()
				break
			}
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ApplyTipChange persists a main-chain tip change.
// For a pure extension (reorg==nil), it appends the new tip block.
// For a reorg, it rolls back to the common ancestor height, then appends the new main-chain segment.
func (s *Store) ApplyTipChange(chain *blockchain.Blockchain, tipHash types.Hash, reorg *blockchain.Reorg) error {
	if chain == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if reorg == nil {
		b, ok := chain.BlockByHash(tipHash)
		if !ok || b == nil {
			return nil
		}
		if b.Header.Height <= s.manifest.TipHeight {
			// Idempotency: ignore duplicate events.
			return nil
		}
		if err := s.writeBlockFile(b); err != nil {
			return err
		}
		s.manifest.TipHeight = b.Header.Height
		s.manifest.TipHash = tipHash
		return s.saveManifest()
	}

	ancestor, ok := chain.BlockByHash(reorg.CommonAncestor)
	if !ok || ancestor == nil {
		return fmt.Errorf("missing common ancestor: %s", reorg.CommonAncestor.String())
	}
	ancestorHeight := ancestor.Header.Height

	if err := s.rollbackTo(ancestorHeight); err != nil {
		return err
	}
	for _, b := range reorg.Added {
		if b == nil {
			continue
		}
		if err := s.writeBlockFile(b); err != nil {
			return err
		}
		s.manifest.TipHeight = b.Header.Height
		s.manifest.TipHash = b.Hash
	}
	return s.saveManifest()
}

func (s *Store) rollbackTo(height uint64) error {
	for h := s.manifest.TipHeight; h > height; h-- {
		p := s.blockPath(h)
		_ = os.Remove(p)
	}
	s.manifest.TipHeight = height
	if b, err := s.readBlockFile(height); err == nil && b != nil {
		s.manifest.TipHash = b.Hash
	}
	return nil
}

func (s *Store) blockPath(height uint64) string {
	return filepath.Join(s.blocksDir, fmt.Sprintf("%020d.json", height))
}

func (s *Store) readBlockFile(height uint64) (*types.Block, error) {
	raw, err := os.ReadFile(s.blockPath(height))
	if err != nil {
		return nil, err
	}
	var b types.Block
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) writeBlockFile(b *types.Block) error {
	if b == nil {
		return nil
	}
	cp := cloneBlock(b)
	// Ensure hash is present for persistence/debugging.
	if cp.Hash.IsZero() {
		h, err := crypto.HashHeaderNonce(cp.Header, cp.Nonce)
		if err != nil {
			return err
		}
		cp.Hash = h
	}
	raw, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.blockPath(cp.Header.Height), raw, 0o644)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, fmt.Sprintf(".tmp-%d", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
