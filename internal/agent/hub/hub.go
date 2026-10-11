// Package hub 是控制面侧的 agent 连接枢纽（f-6-01 §5.2 的服务端半边）。
//
// 职责只有三件：
//  1. 维护「节点 → 在线连接」的注册表，把 agent.Client 的领域操作路由到
//     对应节点的反向长连接上（Execute）；
//  2. 把心跳写进节点行，让离线判定（40s 阈值）与面板展示有真数据
//     （SnapshotProvider + gRPC handler 内的落库）；
//  3. 注册时给 agent 签发客户端证书（certs.go）。
//
// 它实现 agent.Client / agent.SnapshotProvider / agent.StreamOpener 三个
// 接口——与 MockClient 完全同构，因此切换运输层时业务代码零改动（ADR-0007）。
package hub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"

	"k_cockpit/internal/agent"
	agentproto "k_cockpit/internal/agentproto"
	noderepo "k_cockpit/internal/repository/node"
)

// 线上消息类型的本地别名：hub 代码里只关心「下行/上行」两个方向。
type (
	serverToAgent   = agentproto.ServerToAgent
	executeRequest  = agentproto.ExecuteRequest
	stageUpdate     = agentproto.StageUpdate
	executeResult   = agentproto.ExecuteResult
	heartbeat       = agentproto.Heartbeat
	streamOpenedMsg = agentproto.StreamOpened
)

// offlineThreshold 与 service/node 的 OfflineThreshold 同值（f-6-01 R-005：
// 心跳 10s、离线阈值 40s）。刻意不跨包引用：hub 是 agent 传输层，依赖
// service 层只为一个常量会颠倒依赖方向。
const offlineThreshold = 40 * time.Second

// Registrar 是注册业务的最小接口，由 node.Service 适配。
//
// hub 不直接依赖 service 层：它只需要「令牌 → 节点」这一步，宽接口会让
// 传输层长出一堆用不到的依赖。
type Registrar interface {
	// Register 校验一次性令牌、写入 agent 身份并记审计（f-6-01 的注册语义）。
	// 返回节点 ID、节点名（回显给 agent）与确认后的 agent_id。
	Register(ctx context.Context, token, agentID, agentVersion string) (nodeID int64, nodeName, confirmedAgentID string, err error)
}

// Deps 是 hub 的装配依赖。
type Deps struct {
	Repo      *noderepo.Repo
	Registrar Registrar
	// TLSDir 是私有 CA 与服务端证书目录（缺文件时自动生成）。
	TLSDir string
}

// Hub 实现 agent 三接口并承载 gRPC 网关（见 grpc.go）。
type Hub struct {
	repo      *noderepo.Repo
	registrar Registrar
	certs     *certManager

	mu    sync.Mutex
	conns map[int64]*conn
	// gs 是网关的 gRPC server（Serve 时登记，Stop 时优雅关闭）。
	gs *grpc.Server

	opSeq atomic.Uint64
}

// New 构造 hub 并加载（或生成）PKI 材料。
func New(deps Deps) (*Hub, error) {
	certs, err := loadOrCreateCA(deps.TLSDir)
	if err != nil {
		return nil, err
	}
	return &Hub{
		repo:      deps.Repo,
		registrar: deps.Registrar,
		certs:     certs,
		conns:     map[int64]*conn{},
	}, nil
}

// TLSConfig 暴露网关监听用的 TLS 配置。
func (h *Hub) TLSConfig() *tls.Config { return h.certs.TLSConfig() }

// conn 是一条在线 agent 连接：Channel 双向流 + 该流上未决的 Execute。
type conn struct {
	nodeID  int64
	agentID string
	// outbox 是下行发送队列，由 Channel 的 writer goroutine 独占消费——
	// gRPC 流的 SendMsg 不允许并发调用，队列是唯一合法的写入点。
	outbox chan *serverToAgent

	mu      sync.Mutex
	pending map[string]*pendingOp
	closed  bool
}

// pendingOp 是一次在途的 Execute。
type pendingOp struct {
	// kind 是操作类型：结果 Data 的类型解码依赖它（同一键在不同操作下
	// 形状不同）。
	kind    agent.OpKind
	onStage func(agent.Stage)
	done    chan *agent.Result
}

func (c *conn) track(opID string, p *pendingOp) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[opID] = p
}

func (c *conn) untrack(opID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, opID)
}

// deliver 把一条上行消息派发给在途操作；返回 false 表示没有等待者
// （操作已超时或连接刚断），消息只能丢弃。
func (c *conn) deliver(opID string, apply func(*pendingOp)) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[opID]
	if !ok {
		return false
	}
	apply(p)
	return true
}

func (c *conn) closeAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	// 所有在途操作以「未送达/无法判定」收场：这是 error 语义（Result 与
	// error 的区分见 agent.Client 注释），等待方据此把任务标记失败。
	for _, p := range c.pending {
		p.done <- &agent.Result{Success: false, Message: "与节点的连接已断开，操作结果未知"}
	}
	c.pending = map[string]*pendingOp{}
}

// attach / detach 维护在线注册表。同一节点重复连接（旧连接未断又起新连接）
// 时新连接顶替旧连接：旧连接的 Channel 流会随 detach 关闭。
func (h *Hub) attach(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.conns[c.nodeID]; ok && old != c {
		old.closeAll()
	}
	h.conns[c.nodeID] = c
}

func (h *Hub) detach(c *conn) {
	c.closeAll()
	h.mu.Lock()
	defer h.mu.Unlock()
	if cur, ok := h.conns[c.nodeID]; ok && cur == c {
		delete(h.conns, c.nodeID)
	}
}

func (h *Hub) connOf(nodeID int64) *conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[nodeID]
}

// Online 报告节点是否有一条活跃连接。仅供运维排查，不参与离线判定
// （那由心跳时间推导）。
func (h *Hub) Online(nodeID int64) bool { return h.connOf(nodeID) != nil }

// Execute 实现 agent.Client：把操作路由到对应节点的连接。
//
// 节点不在线返回 error（指令未送达），执行失败返回 Success=false 的 Result
// ——两者的区分是 Client 契约的一部分，handler 层据此给出不同的用户文案。
func (h *Hub) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	c := h.connOf(op.NodeID)
	if c == nil {
		return nil, fmt.Errorf("节点 %d 不在线，指令未送达", op.NodeID)
	}

	paramsJSON := "{}"
	if op.Params != nil {
		b, err := json.Marshal(op.Params)
		if err != nil {
			return nil, fmt.Errorf("序列化操作参数: %w", err)
		}
		paramsJSON = string(b)
	}
	opID := fmt.Sprintf("op-%d-%06d", time.Now().UnixNano(), h.opSeq.Add(1))

	w := &pendingOp{kind: op.Kind, onStage: op.OnStage, done: make(chan *agent.Result, 1)}
	c.track(opID, w)
	defer c.untrack(opID)

	err := c.send(ctx, &serverToAgent{Payload: &agentproto.ServerToAgent_Execute{
		Execute: &executeRequest{
			OpId: opID, Kind: string(op.Kind), Target: op.Target, ParamsJson: paramsJSON,
		},
	}})
	if err != nil {
		return nil, fmt.Errorf("指令未送达（节点连接中断）: %w", err)
	}

	select {
	case r := <-w.done:
		return r, nil
	case <-ctx.Done():
		// 注意语义：这里返回 error（无法判定结果），**不**通知 agent 停止——
		// 通道上没有取消指令的形状，节点侧操作继续执行并在结果到达时被
		// 丢弃（untrack 之后 deliver 找不到等待者）。任务层已有自己的取消
		// 与对账语义，这里不发明第二套。
		return nil, fmt.Errorf("等待节点 %d 执行 %s 超时或被取消", op.NodeID, op.Kind)
	}
}

// Snapshot 实现 agent.SnapshotProvider：从节点行读最近心跳与能力。
//
// 与 mock（恒返 online）不同，这里的状态也由心跳时间推导——控制面重启后
// 内存连接表为空，节点行的 last_heartbeat_at 是唯一幸存的事实。
func (h *Hub) Snapshot(ctx context.Context, nodeID int64) (*agent.Snapshot, error) {
	row, err := h.repo.GetNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	snap := &agent.Snapshot{
		ProtocolVersion: row.ProtocolVersion,
		LastError:       deref(row.LastError),
	}
	if row.LastHeartbeatAt != nil {
		snap.LastHeartbeat = *row.LastHeartbeatAt
		if time.Since(*row.LastHeartbeatAt) <= offlineThreshold {
			snap.Status = agent.StatusOnline
		} else {
			snap.Status = agent.StatusOffline
		}
	} else {
		snap.Status = agent.StatusUnknown
	}
	if row.AgentVersion != nil {
		snap.AgentVersion = *row.AgentVersion
	}
	if row.Capabilities != nil && *row.Capabilities != "" {
		if err := json.Unmarshal([]byte(*row.Capabilities), &snap.Capabilities); err != nil {
			log.Printf("[agent-hub] 解析节点 %d 能力清单失败: %v", nodeID, err)
		}
	}
	if row.CapabilitiesAt != nil {
		snap.CapabilitiesAt = *row.CapabilitiesAt
	}
	return snap, nil
}

// OpenStream 实现 agent.StreamOpener。
//
// P0 仅打通通道（Register/Channel/Execute），字节流在 P2 随控制台一起接入；
// 返回哨兵错误让服务层走「不支持」分支——那是为 mock 设计的同一出口，
// 前端会显示「当前节点不支持控制台通道」，如实反映 P0 阶段的能力。
func (h *Hub) OpenStream(_ context.Context, _ agent.StreamKind, _ int64, _ string) (agent.Stream, error) {
	return nil, agent.ErrStreamUnsupported
}

// send 把下行消息投进连接的发送队列；连接关闭或 ctx 取消时返回错误。
func (c *conn) send(ctx context.Context, m *serverToAgent) error {
	select {
	case c.outbox <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// onHeartbeat 把心跳落库。失败只记日志：心跳丢失一个周期只会让状态显示
// 退化到数据库缓存值，下一次心跳就会覆盖——为它让 gRPC 流报错得不偿失。
func (h *Hub) onHeartbeat(ctx context.Context, nodeID int64, hb *heartbeat) {
	now := time.Now()
	capsJSON := "[]"
	if len(hb.Capabilities) > 0 {
		if b, err := json.Marshal(hb.Capabilities); err == nil {
			capsJSON = string(b)
		}
	}
	err := h.repo.UpdateNode(ctx, nodeID, map[string]any{
		"last_heartbeat_at": now,
		"last_seen_at":      now,
		"agent_version":     hb.AgentVersion,
		"protocol_version":  int(hb.ProtocolVersion),
		"capabilities":      capsJSON,
		"capabilities_at":   now,
		"status":            agent.StatusOnline,
	})
	if err != nil {
		log.Printf("[agent-hub] 节点 %d 心跳落库失败: %v", nodeID, err)
	}
}

// onStage / onResult 把上行消息派发给在途 Execute。
func (h *Hub) onStage(c *conn, s *stageUpdate) {
	c.deliver(s.OpId, func(p *pendingOp) {
		if p.onStage != nil {
			p.onStage(agent.Stage{Key: s.Key, Name: s.Name, Index: int(s.Index), Total: int(s.Total)})
		}
	})
}

func (h *Hub) onResult(c *conn, r *executeResult) {
	c.deliver(r.OpId, func(p *pendingOp) {
		data, err := decodeResultData(p.kind, r.DataJson)
		if err != nil {
			log.Printf("[agent-hub] 节点 %d 操作 %s 结果解码失败: %v", c.nodeID, r.OpId, err)
		}
		p.done <- &agent.Result{Success: r.Success, Message: r.Message, Data: data}
	})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
