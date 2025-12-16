package network

import (
	"math/rand"
	"sync"
	"time"

	"simchain-go/internal/types"
)

type Handler func(msg types.Message)

// NetworkBus is an in-memory pub/sub bus simulating a network.
type NetworkBus struct {
	mu       sync.RWMutex
	handlers map[string]Handler

	delay    time.Duration
	dropRate float64

	rngMu sync.Mutex
	rng   *rand.Rand
}

func NewNetworkBus(seed int64) *NetworkBus {
	return &NetworkBus{
		handlers: make(map[string]Handler),
		rng:      rand.New(rand.NewSource(seed)),
	}
}

func (b *NetworkBus) Register(nodeID string, handler Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[nodeID] = handler
}

func (b *NetworkBus) Unregister(nodeID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.handlers, nodeID)
}

func (b *NetworkBus) PeerIDs() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.handlers))
	for id := range b.handlers {
		out = append(out, id)
	}
	return out
}

func (b *NetworkBus) SetDelay(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.delay = d
}

func (b *NetworkBus) SetDropRate(p float64) {
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dropRate = p
}

func (b *NetworkBus) Broadcast(from string, msg types.Message) {
	b.mu.RLock()
	delay := b.delay
	dropRate := b.dropRate
	recipients := make([]Handler, 0, len(b.handlers))
	for id, h := range b.handlers {
		if id == from {
			continue
		}
		recipients = append(recipients, h)
	}
	b.mu.RUnlock()

	for _, h := range recipients {
		if dropRate > 0 && b.shouldDrop(dropRate) {
			continue
		}
		handler := h
		go func() {
			if delay > 0 {
				time.Sleep(delay)
			}
			handler(msg)
		}()
	}
}

func (b *NetworkBus) Send(to string, msg types.Message) {
	b.mu.RLock()
	delay := b.delay
	dropRate := b.dropRate
	handler, ok := b.handlers[to]
	b.mu.RUnlock()
	if !ok {
		return
	}
	if dropRate > 0 && b.shouldDrop(dropRate) {
		return
	}
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		handler(msg)
	}()
}

func (b *NetworkBus) shouldDrop(p float64) bool {
	b.rngMu.Lock()
	defer b.rngMu.Unlock()
	return b.rng.Float64() < p
}
