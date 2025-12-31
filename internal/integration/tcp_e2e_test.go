package integration

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"simchain-go/internal/blockchain"
	"simchain-go/internal/identity"
	"simchain-go/internal/mempool"
	"simchain-go/internal/node"
	"simchain-go/internal/store"
	tcptransport "simchain-go/internal/transport/tcp"
	"simchain-go/internal/types"
)

func TestTCPE2E_TxPropagation_LateJoin_Restart(t *testing.T) {
	// This test intentionally uses real TCP sockets on loopback to validate:
	// - framing + JSON message decoding
	// - handshake identity binding
	// - seed discovery (GetPeers/Peers)
	// - inv/get propagation for tx and blocks
	// - store-based restart recovery
	//
	// It does not attempt to validate reorg behavior deterministically.

	oldOut := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(oldOut) })

	tmp := t.TempDir()
	genesis := types.NewGenesisBlock()

	seedPort := mustFreePort(t)
	node1Port := mustFreePort(t)
	minerPort := mustFreePort(t)
	latePort := mustFreePort(t)
	seedAddr := "127.0.0.1:" + seedPort

	type running struct {
		id   *identity.Identity
		tr   *tcptransport.Transport
		n    *node.Node
		dir  string
		addr string
		mine bool
	}
	stopAll := func(nodes ...*running) {
		for _, r := range nodes {
			if r == nil {
				continue
			}
			if r.n != nil {
				r.n.StopSync()
				if r.mine {
					r.n.StopMining()
				}
			}
			if r.tr != nil {
				r.tr.Stop()
			}
		}
	}

	startNode := func(t *testing.T, listenAddr string, seeds []string, dataDir string, mine bool) *running {
		t.Helper()

		id, err := identity.LoadOrCreate(dataDir)
		if err != nil {
			t.Fatalf("identity: %v", err)
		}

		diff := uint32(6) // fast for tests (expected ~64 hashes per block)
		st, err := store.Open(dataDir, diff, genesis)
		if err != nil {
			t.Fatalf("store open: %v", err)
		}
		blocks, err := st.LoadMainChain()
		if err != nil {
			t.Fatalf("store load: %v", err)
		}
		chain, err := blockchain.NewBlockchain(blocks[0], diff)
		if err != nil {
			t.Fatalf("chain init: %v", err)
		}
		for _, b := range blocks[1:] {
			if _, err := chain.AddBlock(b); err != nil {
				t.Fatalf("restore chain: %v", err)
			}
		}

		tr, err := tcptransport.New(tcptransport.Config{
			ListenAddr:     listenAddr,
			Seeds:          seeds,
			Magic:          "simchain-go",
			Version:        1,
			Identity:       id,
			MaxPeers:       8,
			OutboundPeers:  4,
			MaxMessageSize: 2 << 20,
		})
		if err != nil {
			t.Fatalf("tcp new: %v", err)
		}

		mp := mempool.NewMempool()
		n := node.NewNode(
			id.NodeID,
			chain,
			mp,
			tr,
			node.Config{
				Difficulty:    diff,
				MaxTxPerBlock: 50,
				MinerSleep:    5 * time.Millisecond,
				OnTipChange: func(ev node.TipChangeEvent) {
					_ = st.ApplyTipChange(chain, ev.TipHash, ev.Reorg)
				},
			},
		)

		// Register node handler before transport starts.
		if err := tr.Start(); err != nil {
			t.Fatalf("tcp start: %v", err)
		}
		n.StartSync()
		if mine {
			n.StartMining()
		}

		return &running{id: id, tr: tr, n: n, dir: dataDir, addr: listenAddr, mine: mine}
	}

	seed := startNode(t, seedAddr, nil, filepath.Join(tmp, "seed"), false)
	node1 := startNode(t, "127.0.0.1:"+node1Port, []string{seedAddr}, filepath.Join(tmp, "node1"), false)
	t.Cleanup(func() { stopAll(node1, seed) })

	waitFor(t, 8*time.Second, func() bool {
		return len(node1.tr.Peers()) > 0 && len(seed.tr.Peers()) > 0
	}, "seed/node1 connect")

	// Tx propagation: node1 submits, seed should fetch full tx via GetTx/Tx and accept it.
	node1.n.SubmitTransaction("hello-tx")
	waitFor(t, 6*time.Second, func() bool {
		return seed.n.DebugState(0, 10, 0).MempoolSize > 0
	}, "tx propagate to seed mempool")

	// Start a dedicated miner node after tx propagation is confirmed (to avoid mempool draining races).
	miner := startNode(t, "127.0.0.1:"+minerPort, []string{seedAddr}, filepath.Join(tmp, "miner"), true)
	t.Cleanup(func() { stopAll(miner) })

	// Let miner mine a few blocks; others should follow via inv/get and/or headers-first.
	waitFor(t, 12*time.Second, func() bool { return miner.n.DebugState(0, 0, 0).TipHeight >= 3 }, "miner mines blocks")
	targetHeight := miner.n.DebugState(0, 0, 0).TipHeight
	waitFor(t, 10*time.Second, func() bool { return node1.n.DebugState(0, 0, 0).TipHeight >= targetHeight }, "node1 catches up")
	waitFor(t, 10*time.Second, func() bool { return seed.n.DebugState(0, 0, 0).TipHeight >= targetHeight }, "seed catches up")

	// Stabilize the chain height for the remainder of the test.
	miner.n.StopMining()
	miner.mine = false

	// Late join: node starts after the network has progressed.
	lateDir := filepath.Join(tmp, "late")
	late := startNode(t, "127.0.0.1:"+latePort, []string{seedAddr}, lateDir, false)
	t.Cleanup(func() { stopAll(late) })

	waitFor(t, 12*time.Second, func() bool { return late.n.DebugState(0, 0, 0).TipHeight >= targetHeight }, "late-join catches up")
	lateHeight := late.n.DebugState(0, 0, 0).TipHeight
	waitFor(t, 6*time.Second, func() bool { return readManifestTipHeight(t, lateDir) >= lateHeight }, "late node persists tip")
	persistedHeight := readManifestTipHeight(t, lateDir)

	// Restart recovery: stop late node, then start again with the same data dir on a new port and verify it restores height.
	late.n.StopSync()
	late.tr.Stop()

	waitFor(t, 8*time.Second, func() bool {
		for _, id := range seed.tr.Peers() {
			if id == late.id.NodeID {
				return false
			}
		}
		return true
	}, "seed observes late node disconnect")

	restartPort := mustFreePort(t)
	lateRestart := startNode(t, "127.0.0.1:"+restartPort, []string{seedAddr}, lateDir, false)
	t.Cleanup(func() { stopAll(lateRestart) })

	restoredHeight := lateRestart.n.DebugState(0, 0, 0).TipHeight
	if restoredHeight < persistedHeight {
		t.Fatalf("restart did not restore persisted chain height: got %d want >= %d", restoredHeight, persistedHeight)
	}
	waitFor(t, 12*time.Second, func() bool { return lateRestart.n.DebugState(0, 0, 0).TipHeight >= targetHeight }, "late restart catches up")
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func mustFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	return port
}

func readManifestTipHeight(t *testing.T, dir string) uint64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return 0
	}
	var m struct {
		TipHeight uint64 `json:"tipHeight"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0
	}
	return m.TipHeight
}
