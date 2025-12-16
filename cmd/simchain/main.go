package main

import (
	"flag"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"simchain-go/internal/blockchain"
	"simchain-go/internal/mempool"
	"simchain-go/internal/network"
	"simchain-go/internal/node"
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
	flag.Parse()

	if *nodesFlag < 1 {
		log.Fatal("nodes must be >= 1")
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	genesis := types.NewGenesisBlock()
	bus := network.NewNetworkBus(*seedFlag)
	if *netDelayFlag > 0 {
		bus.SetDelay(*netDelayFlag)
	}
	if *dropRateFlag > 0 {
		bus.SetDropRate(*dropRateFlag)
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

	newNode := func(i int) *node.Node {
		chain, err := blockchain.NewBlockchain(genesis, diff)
		if err != nil {
			log.Fatalf("init chain: %v", err)
		}
		mp := mempool.NewMempool()
		return node.NewNode(
			fmt.Sprintf("node-%d", i+1),
			chain,
			mp,
			bus,
			node.Config{
				Difficulty:    diff,
				MaxTxPerBlock: *maxTxPerBlockFlag,
				MinerSleep:    *minerSleepFlag,
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

	for _, n := range snapshotNodes() {
		mined, received, height := n.Stats()
		log.Printf("STATS node=%s mined=%d received=%d height=%d", n.ID(), mined, received, height)
	}
}
