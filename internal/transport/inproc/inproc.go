// Package inproc 提供 Transport 的单进程实现：通过内存总线投递消息。
package inproc

import (
	"time"

	"simchain-go/internal/network"
	"simchain-go/internal/transport"
	"simchain-go/internal/types"
)

// InprocTransport 把内存网络（NetworkBus）适配成 Transport 接口。
//
// 说明：
// - 它只用于“单进程多节点”的模拟模式，方便注入延迟/丢包并做调试观察。
type InprocTransport struct {
	bus *network.NetworkBus
}

func New(seed int64) *InprocTransport {
	return &InprocTransport{bus: network.NewNetworkBus(seed)}
}

func (t *InprocTransport) Register(nodeID string, handler transport.Handler) {
	t.bus.Register(nodeID, network.Handler(handler))
}

func (t *InprocTransport) Unregister(nodeID string) {
	t.bus.Unregister(nodeID)
}

func (t *InprocTransport) Peers() []string {
	return t.bus.PeerIDs()
}

func (t *InprocTransport) Broadcast(from string, msg types.Message) {
	t.bus.Broadcast(from, msg)
}

func (t *InprocTransport) Send(to string, msg types.Message) {
	t.bus.Send(to, msg)
}

func (t *InprocTransport) SetDelay(d time.Duration) {
	t.bus.SetDelay(d)
}

func (t *InprocTransport) SetDropRate(p float64) {
	t.bus.SetDropRate(p)
}
