// Package network 提供“单进程内存网络”的模拟能力（发布/订阅总线）。
//
// 说明：
// - 仅用于本项目的 inproc 模式（单进程多节点模拟）。
// - 可注入延迟与丢包，用于观察同步/收敛行为。
package network

import (
	"math/rand"
	"sync"
	"time"

	"simchain-go/internal/types"
)

// Handler 是节点注册到 NetworkBus 的回调函数。
type Handler func(msg types.Message)

// NetworkBus 是内存中的 pub/sub 总线，用于模拟“网络投递”。
//
// 约定（V2）：
// - Broadcast：用于“inv 公告”类的扇出广播（告诉别人“我有 tx/block 的摘要”）。
// - Send：用于 Get*/* 这种“按需拉取”的定向请求/响应。
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
	// Broadcast 是“扇出”：除发送者外的所有节点都会收到。
	// 延迟/丢包在这里注入（模拟真实网络的不确定性）。
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
	// Send 是“定向投递”：只发给指定节点。
	// V2 的 Get*/* 交换依赖它来模拟“按需拉取”的请求-响应。
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
