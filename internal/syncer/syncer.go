package syncer

import (
	"math/big"
	"time"

	"simchain-go/internal/blockchain"
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

	SyncTimeout     time.Duration
	MaxHeaders      int
	BlocksBatchSize int

	RPC     RPCFunc
	OnBlock OnBlockFunc
}

// Syncer is responsible for keeping a node's chain up to date.
//
// 当前只先迁移 V2 的 InitialSync（late join 追链逻辑）到这里；
// 后续 V3-A 会把 inv/get 的长期同步与 retry/window 也逐步搬入 Syncer。
type Syncer struct {
	cfg Config
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
	return &Syncer{cfg: cfg}
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

	for {
		localWork := s.cfg.Chain.TipCumWork()
		if localWork.Cmp(best.work) >= 0 && s.cfg.Chain.TipHeight() >= best.height {
			break
		}

		locator := s.cfg.Chain.Locator(32)
		resp, ok := s.cfg.RPC(best.peer, types.MsgGetHeaders, types.GetHeadersPayload{Locator: locator, Max: s.cfg.MaxHeaders}, s.cfg.SyncTimeout)
		if !ok || resp.Type != types.MsgHeaders {
			break
		}
		hpl, ok := resp.Payload.(types.HeadersPayload)
		if !ok || len(hpl.Metas) == 0 {
			break
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
			bresp, ok := s.cfg.RPC(best.peer, types.MsgGetBlocks, types.GetBlocksPayload{Hashes: hashes[i:end]}, s.cfg.SyncTimeout)
			if !ok || bresp.Type != types.MsgBlocks {
				continue
			}
			bpl, ok := bresp.Payload.(types.BlocksPayload)
			if !ok {
				continue
			}
			for _, b := range bpl.Blocks {
				if b != nil {
					s.cfg.OnBlock(b, best.peer)
				}
			}
		}
	}
}
