package inproc

import (
	"time"

	"simchain-go/internal/network"
	"simchain-go/internal/transport"
	"simchain-go/internal/types"
)

// InprocTransport adapts NetworkBus to the Transport interface.
// It keeps the current single-process simulation model.
type InprocTransport struct {
	bus *network.NetworkBus
}

func New(seed int64) *InprocTransport {
	return &InprocTransport{bus: network.NewNetworkBus(seed)}
}

func FromBus(bus *network.NetworkBus) *InprocTransport {
	return &InprocTransport{bus: bus}
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
