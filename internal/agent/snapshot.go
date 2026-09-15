package agent

import (
	"context"
	"time"
)

// 节点运行态取值。
//
// 状态由**心跳时间**推导（f-6-01 R-005：心跳周期 10s，离线阈值 40s），
// 而不是由 agent 直接上报一个字符串——后者会在 agent 宕机时停在上报的
// 最后状态上，把「失联」显示成「在线」。
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
	StatusUnknown = "unknown"
)

// Snapshot 是节点的运行态快照。
//
// 它是**节点自己报上来的数据**，与 node 表中的元数据（名称、注册信息）
// 是两类东西：元数据由控制面管理，运行态只可能来自 agent。因此开发期
// 由 mock 提供（ADR-0007），业务代码不区分二者来源。
type Snapshot struct {
	Status          string
	LastHeartbeat   time.Time
	AgentVersion    string
	ProtocolVersion int
	// Capabilities 是 agent 自报的能力清单（声明式，见 f-6-01 R-014）。
	Capabilities   []string
	CapabilitiesAt time.Time
	// LastError 是 agent 侧最近一次错误的摘要，供排障展示。
	LastError string
}

// SnapshotProvider 由 Client 实现，用于获取节点运行态。
//
// 单独声明是为了让业务代码可以只依赖它——只需要读运行态的调用方
// 不必拿到「下发指令」的能力。
type SnapshotProvider interface {
	// Snapshot 返回指定节点的运行态快照。
	Snapshot(ctx context.Context, nodeID int64) (*Snapshot, error)
}
