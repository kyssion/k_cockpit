// daemon.go 是 agent 的常驻循环：拨号控制面 → 打开 Channel → 心跳 +
// 接收执行指令 → 流断开按退避重连。
package nodeagent

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"k_cockpit/internal/agent"
	agentproto "k_cockpit/internal/agentproto"
)

// 通道参数（f-6-01 §5.2 的 agent 侧执行口径）。
const (
	// heartbeatInterval 是心跳周期；离线判定在控制面按 40s 阈值推导，
	// agent 侧只需要守周期。
	heartbeatInterval = 10 * time.Second
	// ProtocolVersion 是本实现理解的协议版本（兼容承诺：字段只增不改）。
	ProtocolVersion = 1
	// runAgentName 是日志组件名。
	runAgentName = "k_cockpit-agent"
)

// Daemon 是一次「agent 运行期」的载体。
type Daemon struct {
	// Server 是控制面网关地址（host:port）。
	Server string
	// AgentID / AgentVersion 随每次心跳与注册上报。
	AgentID      string
	AgentVersion string
	// Hostname 仅作展示。
	Hostname string
	// TLS 是数据通道的客户端 TLS 配置（由 StateDir.ClientTLS 构造）。
	TLS *tls.Config
	// Registry 是本机已实现的执行器。
	Registry Registry
	// Now 供测试注入。
	Now func() time.Time

	// conn 是当前会话的 gRPC 连接（StreamData 回拨需要与通道同一条 mTLS
	// 连接；由 session 建立时写入）。
	conn *grpc.ClientConn
}

func (d *Daemon) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// AgentVersionOf 返回构建版本；未注入时如实写 dev 而不是编一个版本号。
// 由 cmd/agent 通过 -ldflags 注入（与控制面 version 包同一套变量名约定）。
var AgentVersionOf = func() string {
	if buildVersion != "" {
		return buildVersion
	}
	return "dev"
}

// buildVersion 由 -ldflags 注入。
var buildVersion string

// Run 阻塞运行直到 ctx 取消。返回值仅表示主循环为何退出。
func (d *Daemon) Run(ctx context.Context) error {
	// 镜像看门狗的定时器在内存里，上个进程退出即丢——先按 /run 落盘的
	// 到期时刻补上（已到期的立刻补撤销），再进入主循环。
	rearmMirrorWatchdogs()

	failures := 0
	var lastSession time.Duration

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		start := d.now()
		err := d.session(ctx)
		lastSession = d.now().Sub(start)

		if ctx.Err() != nil {
			return ctx.Err()
		}
		failures++
		wait := nextBackoff(failures, lastSession)
		log.Printf("[%s] 通道会话结束（存活 %s），%s 后第 %d 次重连: %v",
			runAgentName, lastSession.Round(time.Second), wait.Round(time.Millisecond), failures, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		if lastSession >= stableAfter {
			failures = 0
		}
	}
}

// session 是一次完整的「拨号 → Channel → 断开」。
func (d *Daemon) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, d.Server,
		grpc.WithTransportCredentials(credentials.NewTLS(d.TLS)),
		grpc.WithBlock(),
		// 与服务端同档：导出分片等大负载（默认 4MB 收包上限会拦）。
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(32<<20), grpc.MaxCallSendMsgSize(32<<20)),
	)
	if err != nil {
		return fmt.Errorf("连接控制面: %w", err)
	}
	defer func() { _ = conn.Close() }()
	d.conn = conn

	stream, err := agentproto.NewAgentGatewayClient(conn).Channel(ctx)
	if err != nil {
		return fmt.Errorf("打开通道: %w", err)
	}

	// 首帧即心跳：让控制面在通道建立的瞬间就能把节点标成在线，
	// 而不是等第一个 10s 周期。
	if err := stream.Send(d.heartbeatMsg()); err != nil {
		return fmt.Errorf("发送首帧心跳: %w", err)
	}

	// sendPump：心跳定时器 + 本地下行队列（执行结果/阶段上行也走这条流，
	// Send 不允许并发，全部经 outbox 串行化）。
	outbox := make(chan *agentproto.AgentToServer, 64)
	sendDone := make(chan error, 1)
	go func() {
		defer close(sendDone)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := stream.Send(d.heartbeatMsg()); err != nil {
					sendDone <- err
					return
				}
			case m := <-outbox:
				if err := stream.Send(m); err != nil {
					sendDone <- err
					return
				}
			}
		}
	}()

	reporter := func(opID string) Reporter {
		return func(s Stage) {
			// **非阻塞上报**：断线窗口里 sendPump 已退出、outbox 会堆满——
			// 阻塞发送会把执行 goroutine 永久卡死（channel 属于已结束的
			// 会话，无人再消费），每个在途操作泄漏一份。阶段只是进度展示，
			// 满了丢弃无妨。
			select {
			case outbox <- &agentproto.AgentToServer{Payload: &agentproto.AgentToServer_Stage{
				Stage: &agentproto.StageUpdate{
					OpId: opID, Key: s.Key, Name: s.Name, Index: int32(s.Index), Total: int32(s.Total),
				},
			}}:
			default:
			}
		}
	}

	// 接收循环：执行指令分发。任何错误（含 ctx 取消）都结束会话，由 Run
	// 决定重连。
	for {
		msg, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("通道读取: %w", err)
		}
		switch p := msg.Payload.(type) {
		case *agentproto.ServerToAgent_Execute:
			req := p.Execute
			op := agent.Operation{
				Kind: agent.OpKind(req.Kind), Target: req.Target,
			}
			if req.ParamsJson != "" {
				if err := json.Unmarshal([]byte(req.ParamsJson), &op.Params); err != nil {
					outbox <- resultMsg(req.OpId, &Outcome{
						Success: false, Message: "操作参数不是合法 JSON: " + err.Error(),
					})
					continue
				}
			}
			// 执行在独立 goroutine：一个慢操作不能阻塞后续指令的接收
			// （控制面侧任务队列按资源锁串行化同资源操作，这里无需再排队）。
			go func(opID string, op agent.Operation) {
				out := d.Registry.dispatch(ctx, op, reporter(opID))
				select {
				case outbox <- resultMsg(opID, out):
				case <-sendDone:
					// 通道已死：丢弃结果（控制面任务侧自会按未送达处理）。
				case <-ctx.Done():
				}
			}(req.OpId, op)
		case *agentproto.ServerToAgent_OpenStream:
			// dial 限 3s：VNC 端口异常时不能让接收循环整体卡住
			//（心跳与后续指令都被压在后面）。
			dialCtx, dialCancel := context.WithTimeout(ctx, 3*time.Second)
			if out := d.handleOpenStream(dialCtx, p.OpenStream); out != nil {
				select {
				case outbox <- out:
				default:
					_ = dialCancel
				}
			}
			dialCancel()
		default:
			// 未知下行消息忽略（协议兼容承诺）。
		}
	}
}

func (d *Daemon) heartbeatMsg() *agentproto.AgentToServer {
	return &agentproto.AgentToServer{Payload: &agentproto.AgentToServer_Heartbeat{
		Heartbeat: &agentproto.Heartbeat{
			AgentVersion:    d.AgentVersion,
			ProtocolVersion: ProtocolVersion,
			Capabilities:    d.Registry.Capabilities(),
			Hostname:        d.Hostname,
		},
	}}
}

func resultMsg(opID string, out *Outcome) *agentproto.AgentToServer {
	dataJSON := ""
	if len(out.Data) > 0 {
		if b, err := json.Marshal(out.Data); err == nil {
			dataJSON = string(b)
		}
	}
	return &agentproto.AgentToServer{Payload: &agentproto.AgentToServer_Result{
		Result: &agentproto.ExecuteResult{
			OpId: opID, Success: out.Success, Message: out.Message, DataJson: dataJSON,
		},
	}}
}
