package transport

import (
	"time"

	"simchain-go/internal/types"
)

// Handler handles an incoming message for a registered node ID.
type Handler func(msg types.Message)

// Transport abstracts message delivery between nodes.
//
// V3-A 只实现 inproc（单进程内存网络）。后续如果做 V3-B（TCP 多进程），只需要替换实现，
// 上层的 peer/sync/mempool/miner/chain 逻辑应尽量不改。
type Transport interface {
	Register(nodeID string, handler Handler)
	Unregister(nodeID string)
	Peers() []string

	// Broadcast 用于 inv 类公告（扇出）。
	Broadcast(from string, msg types.Message)
	// Send 用于 Get*/* 的按需拉取（定向请求/响应）。
	Send(to string, msg types.Message)

	// 故障注入（inproc 模拟网络用）。
	SetDelay(d time.Duration)
	SetDropRate(p float64)
}
