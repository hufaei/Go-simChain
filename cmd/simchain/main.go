package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"simchain-go/internal/blockchain"
	"simchain-go/internal/mempool"
	"simchain-go/internal/node"
	"simchain-go/internal/store"
	"simchain-go/internal/transport/inproc"
	"simchain-go/internal/types"
)

func main() {
	nodesFlag := flag.Int("nodes", 2, "number of nodes to simulate")
	difficultyFlag := flag.Uint("difficulty", 16, "PoW difficulty in leading zero bits")
	durationFlag := flag.Duration("duration", 30*time.Second, "simulation duration")
	maxTxPerBlockFlag := flag.Int("max-tx-per-block", 50, "max txs per mined block")
	txIntervalFlag := flag.Duration("tx-interval", 1*time.Second, "interval for injecting transactions")
	minerSleepFlag := flag.Duration("miner-sleep", 10*time.Millisecond, "sleep between mining rounds")
	seedFlag := flag.Int64("seed", time.Now().UnixNano(), "random seed")
	bootstrapNodesFlag := flag.Int("bootstrap-nodes", 0, "nodes to start immediately (default: all)")
	joinDelayFlag := flag.Duration("join-delay", 0, "delay before starting each late-join node")
	netDelayFlag := flag.Duration("net-delay", 0, "simulated network delay")
	dropRateFlag := flag.Float64("drop-rate", 0, "simulated network drop rate [0..1]")
	debugDumpOnTipFlag := flag.Bool("debug-dump-on-tip", true, "dump all nodes' mempool + recent main-chain blocks whenever any node's tip changes (set to false to disable)")
	debugDumpFormatFlag := flag.String("debug-dump-format", "pretty", "debug dump format: pretty|json")
	debugChainDepthFlag := flag.Int("debug-chain-depth", 8, "how many recent main-chain blocks to print per node when dumping (0 disables chain dump)")
	debugMempoolMaxFlag := flag.Int("debug-mempool-max", 20, "how many mempool txids to print per node when dumping (0 disables mempool listing)")
	debugBlockTxMaxFlag := flag.Int("debug-block-tx-max", 20, "how many txids to print per main-chain block when dumping (0 disables tx listing)")
	flag.Parse()

	if *nodesFlag < 1 {
		log.Fatal("nodes must be >= 1")
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	genesis := types.NewGenesisBlock()
	tr := inproc.New(*seedFlag)
	if *netDelayFlag > 0 {
		tr.SetDelay(*netDelayFlag)
	}
	if *dropRateFlag > 0 {
		tr.SetDropRate(*dropRateFlag)
	}

	diff := uint32(*difficultyFlag)

	bootstrap := *bootstrapNodesFlag
	if bootstrap <= 0 || bootstrap > *nodesFlag {
		bootstrap = *nodesFlag
	}
	if bootstrap < 1 {
		bootstrap = 1
	}

	var nodesMu sync.RWMutex
	nodes := make([]*node.Node, 0, *nodesFlag)
	addNode := func(n *node.Node) {
		nodesMu.Lock()
		nodes = append(nodes, n)
		nodesMu.Unlock()
	}
	snapshotNodes := func() []*node.Node {
		nodesMu.RLock()
		defer nodesMu.RUnlock()
		return append([]*node.Node(nil), nodes...)
	}

	var dumpMu sync.Mutex
	shortHash := func(h types.Hash) string {
		s := h.String()
		if len(s) <= 12 {
			return s
		}
		return s[:12]
	}

	logBlock := func(title string, lines []string) {
		// 为了更直观，把一组相关信息聚合成一次 log 输出（多行一块），便于在控制台阅读/复制。
		var b strings.Builder
		b.WriteString(title)
		for _, line := range lines {
			b.WriteString("\n")
			b.WriteString(line)
		}
		log.Print(b.String())
	}

	debugStatePrettyLines := func(st node.DebugState) []string {
		lines := make([]string, 0, 3+len(st.MainChain))
		lines = append(lines, fmt.Sprintf("  node=%s tipHeight=%d tip=%s", st.NodeID, st.TipHeight, shortHash(st.TipHash)))
		if *debugMempoolMaxFlag == 0 {
			lines = append(lines, fmt.Sprintf("  mempool size=%d", st.MempoolSize))
		} else {
			lines = append(lines, fmt.Sprintf("  mempool size=%d txids=[%s]", st.MempoolSize, strings.Join(st.MempoolTxIDs, ", ")))
		}
		if *debugChainDepthFlag != 0 {
			lines = append(lines, "  mainchain:")
			for _, b := range st.MainChain {
				txSuffix := ""
				if *debugBlockTxMaxFlag != 0 && len(b.TxIDs) > 0 {
					txSuffix = " txids=[" + strings.Join(b.TxIDs, ", ") + "]"
				}
				lines = append(lines, fmt.Sprintf("    - h=%d hash=%s txs=%d%s", b.Height, shortHash(b.Hash), b.TxCount, txSuffix))
			}
		}
		return lines
	}

	dumpAll := func(trigger node.TipChangeEvent) {
		dumpMu.Lock()
		defer dumpMu.Unlock()

		// 注意：这里是“可观测性/调试”输出，和共识逻辑无关。
		// 每当任意节点 tip 变化，就把所有节点的 mempool + 最近主链块打印出来。
		nodesSnap := snapshotNodes()
		if strings.EqualFold(*debugDumpFormatFlag, "json") {
			type dump struct {
				Trigger node.TipChangeEvent `json:"trigger"`
				Nodes   []node.DebugState   `json:"nodes"`
			}
			payload := dump{Trigger: trigger, Nodes: make([]node.DebugState, 0, len(nodesSnap))}
			for _, n := range nodesSnap {
				payload.Nodes = append(payload.Nodes, n.DebugState(*debugChainDepthFlag, *debugMempoolMaxFlag, *debugBlockTxMaxFlag))
			}
			// Stable ordering for diffing.
			sort.Slice(payload.Nodes, func(i, j int) bool { return payload.Nodes[i].NodeID < payload.Nodes[j].NodeID })
			raw, err := jsonMarshalIndent(payload)
			if err != nil {
				log.Printf("STATE_DUMP_ERR err=%v", err)
				return
			}
			log.Print("STATE_DUMP_JSON\n" + string(raw))
			return
		}

		lines := []string{
			fmt.Sprintf("trigger node=%s from=%s height=%d tip=%s reorg=%t", trigger.NodeID, trigger.FromPeer, trigger.Height, shortHash(trigger.TipHash), trigger.Reorg != nil),
		}
		states := make([]node.DebugState, 0, len(nodesSnap))
		for _, n := range nodesSnap {
			states = append(states, n.DebugState(*debugChainDepthFlag, *debugMempoolMaxFlag, *debugBlockTxMaxFlag))
		}
		sort.Slice(states, func(i, j int) bool { return states[i].NodeID < states[j].NodeID })
		for _, st := range states {
			lines = append(lines, debugStatePrettyLines(st)...)
		}
		logBlock("STATE_DUMP", lines)
	}

	newNode := func(i int) *node.Node {
		nodeID := fmt.Sprintf("node-%d", i+1)
		dataDir := filepath.Join("data", nodeID)
		st, err := store.Open(dataDir, diff, genesis)
		if err != nil {
			log.Fatalf("init store: %v", err)
		}
		blocks, err := st.LoadMainChain()
		if err != nil {
			log.Fatalf("load chain: %v", err)
		}
		if len(blocks) == 0 {
			log.Fatalf("load chain: empty")
		}

		chain, err := blockchain.NewBlockchain(blocks[0], diff)
		if err != nil {
			log.Fatalf("init chain: %v", err)
		}
		for _, b := range blocks[1:] {
			if _, err := chain.AddBlock(b); err != nil {
				log.Fatalf("restore chain: %v", err)
			}
		}

		mp := mempool.NewMempool()
		return node.NewNode(
			nodeID,
			chain,
			mp,
			tr,
			node.Config{
				Difficulty:    diff,
				MaxTxPerBlock: *maxTxPerBlockFlag,
				MinerSleep:    *minerSleepFlag,
				OnTipChange: func(ev node.TipChangeEvent) {
					if err := st.ApplyTipChange(chain, ev.TipHash, ev.Reorg); err != nil {
						log.Printf("STORE_ERR node=%s err=%v", nodeID, err)
					}
					if *debugDumpOnTipFlag {
						dumpAll(ev)
					}
				},
			},
		)
	}

	for i := 0; i < bootstrap; i++ {
		n := newNode(i)
		addNode(n)
		n.StartMining()
	}

	rng := rand.New(rand.NewSource(*seedFlag))
	txTicker := time.NewTicker(*txIntervalFlag)
	defer txTicker.Stop()
	end := time.NewTimer(*durationFlag)
	defer end.Stop()

	stopJoin := make(chan struct{})
	if bootstrap < *nodesFlag {
		go func() {
			for i := bootstrap; i < *nodesFlag; i++ {
				if *joinDelayFlag > 0 {
					select {
					case <-time.After(*joinDelayFlag):
					case <-stopJoin:
						return
					}
				}
				n := newNode(i)
				addNode(n)
				n.InitialSync()
				n.StartMining()
				log.Printf("NODE_JOINED node=%s", n.ID())
			}
		}()
	}

	log.Printf("SIMULATION_START nodes=%d bootstrap=%d difficulty=%d duration=%s txInterval=%s netDelay=%s dropRate=%.3f",
		*nodesFlag, bootstrap, diff, durationFlag.String(), txIntervalFlag.String(), netDelayFlag.String(), *dropRateFlag)

	txCount := 0
loop:
	for {
		select {
		case <-txTicker.C:
			nodesMu.RLock()
			if len(nodes) == 0 {
				nodesMu.RUnlock()
				continue
			}
			target := nodes[txCount%len(nodes)]
			nodesMu.RUnlock()
			payload := fmt.Sprintf("tx-%d rnd=%d", txCount+1, rng.Int63())
			target.SubmitTransaction(payload)
			txCount++
		case <-end.C:
			break loop
		}
	}

	log.Printf("SIMULATION_STOP txs=%d", txCount)
	close(stopJoin)
	for _, n := range snapshotNodes() {
		n.StopMining()
	}

	nodesSnap := snapshotNodes()

	var totalHashes uint64
	var totalMined uint64
	for _, n := range nodesSnap {
		totalHashes += n.HashAttempts()
		mined, _, _ := n.Stats()
		totalMined += mined
	}
	type nodeRow struct {
		ID         string
		Mined      uint64
		Received   uint64
		Height     uint64
		Hashes     uint64
		HashShare  float64
		MinedShare float64
	}
	rows := make([]nodeRow, 0, len(nodesSnap))
	for _, n := range nodesSnap {
		mined, received, height := n.Stats()
		hashes := n.HashAttempts()
		hashShare := 0.0
		minedShare := 0.0
		if totalHashes > 0 {
			hashShare = float64(hashes) / float64(totalHashes)
		}
		if totalMined > 0 {
			minedShare = float64(mined) / float64(totalMined)
		}
		rows = append(rows, nodeRow{
			ID:         n.ID(),
			Mined:      mined,
			Received:   received,
			Height:     height,
			Hashes:     hashes,
			HashShare:  hashShare,
			MinedShare: minedShare,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	lines := []string{
		fmt.Sprintf("txs=%d totalHashes=%d totalMined=%d", txCount, totalHashes, totalMined),
	}
	for _, r := range rows {
		lines = append(lines, fmt.Sprintf("node=%s height=%d mined=%d(%.2f%%) received=%d hashes=%d(%.2f%%)",
			r.ID, r.Height, r.Mined, r.MinedShare*100, r.Received, r.Hashes, r.HashShare*100))
	}
	logBlock("SIMULATION_SUMMARY", lines)
}

func jsonMarshalIndent(v any) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}
