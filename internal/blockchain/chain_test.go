package blockchain

import (
	"testing"

	"simchain-go/internal/crypto"
	"simchain-go/internal/types"
)

func mineBlock(parent *types.Block, height uint64, difficulty uint32, minerID string) *types.Block {
	header := types.BlockHeader{
		Height:     height,
		PrevHash:   parent.Hash,
		Timestamp:  parent.Header.Timestamp + 1,
		Difficulty: difficulty,
		MinerID:    minerID,
		TxRoot:     crypto.HashTransactions(nil),
	}
	var nonce uint64
	for {
		h, _ := crypto.HashHeaderNonce(header, nonce)
		if crypto.MeetsDifficulty(h, difficulty) {
			return &types.Block{Header: header, Nonce: nonce, Hash: h}
		}
		nonce++
	}
}

func TestForkSelectionByCumWork(t *testing.T) {
	genesis := types.NewGenesisBlock()
	diff := uint32(4)
	bc, err := NewBlockchain(genesis, diff)
	if err != nil {
		t.Fatalf("init chain: %v", err)
	}

	// main chain: g -> b1 -> b2a
	b1 := mineBlock(bc.GetTip(), 1, diff, "A")
	if _, err := bc.AddBlock(b1); err != nil {
		t.Fatalf("add b1: %v", err)
	}
	if bc.TipHeight() != 1 {
		t.Fatalf("expected height 1, got %d", bc.TipHeight())
	}

	mainTip := bc.GetTip()
	b2a := mineBlock(mainTip, 2, diff, "A")
	if _, err := bc.AddBlock(b2a); err != nil {
		t.Fatalf("add b2a: %v", err)
	}
	if bc.GetTip().Hash != b2a.Hash {
		t.Fatalf("expected tip b2a")
	}

	// side chain: g -> b1 -> b2b -> b3b (should win)
	b2b := mineBlock(mainTip, 2, diff, "B")
	if _, err := bc.AddBlock(b2b); err != nil {
		t.Fatalf("add b2b: %v", err)
	}
	if bc.GetTip().Hash != b2a.Hash {
		t.Fatalf("expected tip still b2a after equal cumwork fork")
	}

	b3b := mineBlock(b2b, 3, diff, "B")
	res, err := bc.AddBlock(b3b)
	if err != nil {
		t.Fatalf("add b3b: %v", err)
	}
	if !res.IsNewTip || res.Reorg == nil {
		t.Fatalf("expected reorg")
	}
	if bc.GetTip().Hash != b3b.Hash {
		t.Fatalf("expected tip b3b after heavier chain")
	}
	if len(res.Reorg.Removed) != 1 || len(res.Reorg.Added) != 2 {
		t.Fatalf("unexpected reorg sizes: removed=%d added=%d", len(res.Reorg.Removed), len(res.Reorg.Added))
	}
}

func TestMainChainMetasFromLocator(t *testing.T) {
	genesis := types.NewGenesisBlock()
	diff := uint32(4)

	peer, err := NewBlockchain(genesis, diff)
	if err != nil {
		t.Fatalf("init peer: %v", err)
	}
	local, err := NewBlockchain(genesis, diff)
	if err != nil {
		t.Fatalf("init local: %v", err)
	}

	blocks := make([]*types.Block, 0, 8)
	for i := 1; i <= 8; i++ {
		b := mineBlock(peer.GetTip(), uint64(i), diff, "P")
		if _, err := peer.AddBlock(b); err != nil {
			t.Fatalf("peer add b%d: %v", i, err)
		}
		blocks = append(blocks, b)
		if i <= 3 {
			if _, err := local.AddBlock(b); err != nil {
				t.Fatalf("local add b%d: %v", i, err)
			}
		}
	}

	locator := local.Locator(32)
	metas := peer.MainChainMetasFromLocator(locator, 2000)
	if len(metas) != 5 {
		t.Fatalf("expected 5 metas, got %d", len(metas))
	}
	for i := range metas {
		want := blocks[i+3] // heights 4..8
		got := metas[i]
		if got.Hash != want.Hash {
			t.Fatalf("meta[%d] hash mismatch: got %s want %s", i, got.Hash.String(), want.Hash.String())
		}
		if got.Header.Height != want.Header.Height {
			t.Fatalf("meta[%d] height mismatch: got %d want %d", i, got.Header.Height, want.Header.Height)
		}
		if got.Nonce != want.Nonce {
			t.Fatalf("meta[%d] nonce mismatch: got %d want %d", i, got.Nonce, want.Nonce)
		}
	}
}
