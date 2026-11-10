// gRPC 网关：AgentGateway 服务的控制面实现。
//
// 鉴权模型（两态，见 proto 注释）：
//   - 无客户端证书：只允许 Register（一次性令牌换证书）；
//   - 有客户端证书：CN 即 agent_id，全部方法可用，Channel 的身份解析以它为准。
package hub

import (
	"context"
	"crypto/x509"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	agentproto "k_cockpit/internal/agentproto"
)

// gateway 是 AgentGateway 的服务端实现。方法集很小：真正的路由逻辑在
// Hub（hub.go），这里只做「TLS 身份 → 节点」与「线上消息 → 内存消息」的翻译。
type gateway struct {
	agentproto.UnimplementedAgentGatewayServer
	h *Hub
}

// Register 处理一次性注册：校验令牌（业务在 node.Service）、签发客户端证书。
//
// 令牌是 48 位十六进制的高熵随机值，暴力枚举不可行，因此这里不做限流——
// 与面板登录（人类密码，可枚举）的威胁模型不同。
func (g *gateway) Register(ctx context.Context, req *agentproto.RegisterRequest) (*agentproto.RegisterResponse, error) {
	if req.Token == "" {
		return nil, status.Error(codes.InvalidArgument, "注册令牌不能为空")
	}
	// 协议版本必须在签发证书之前校验：让一个说不清自己版本的 agent 拿到
	// 长期有效的 mTLS 凭据，之后只能靠吊销来收拾。
	if !protocolVersionSupported(req.ProtocolVersion) {
		return nil, status.Error(codes.FailedPrecondition,
			"协议版本不受支持（当前支持 1~"+strconv.Itoa(agentProtocolVersion)+"），请对齐控制面与 agent 版本")
	}
	agentID := req.AgentId
	if agentID == "" {
		// agent 首次 enroll 未携带身份时由控制面生成；此后 agent 持久化它。
		agentID = newAgentID()
	}
	nodeID, nodeName, confirmed, err := g.h.registrar.Register(ctx, req.Token, agentID, req.AgentVersion)
	if err != nil {
		// 注册失败的常见原因（令牌无效/过期/已用）由 service 层给出可执行文案。
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	// requested_name 只在 agent 显式传了 -name 时才有值（默认不发），因此
	// 重复 enroll 不会覆盖管理员改过的节点名。名字只作展示，写失败不阻断
	// 注册，但必须记日志——否则运维会以为改名成功了。
	if req.RequestedName != "" {
		if !validNodeName(req.RequestedName) {
			return nil, status.Error(codes.InvalidArgument,
				"请求的节点名不合法（字母或数字开头，仅允许 ._-，最长 64 字符）")
		}
		if err := g.h.repo.UpdateNode(ctx, nodeID, map[string]any{"name": req.RequestedName}); err != nil {
			log.Printf("[agent-hub] 节点 %d 采用注册名 %q 失败（保留原名称）: %v", nodeID, req.RequestedName, err)
		} else {
			nodeName = req.RequestedName
		}
	}
	certPEM, keyPEM, err := g.h.certs.IssueClientCert(confirmed)
	if err != nil {
		log.Printf("[agent-hub] 节点 %d 客户端证书签发失败: %v", nodeID, err)
		return nil, status.Error(codes.Internal, "证书签发失败")
	}
	return &agentproto.RegisterResponse{
		AgentId:    confirmed,
		CaCert:     g.h.certs.caPEM,
		ClientCert: certPEM,
		ClientKey:  keyPEM,
		NodeName:   nodeName,
	}, nil
}

// Channel 是每条 agent 连接的主循环。
//
// goroutine 形状：
//   - 当前 goroutine 做**上行读取**（心跳/阶段/结果），流结束（agent 断开）
//     时清理注册表并终止 writer；
//   - writer goroutine 独占**下行发送**（gRPC 流不允许并发 Send）。
func (g *gateway) Channel(stream agentproto.AgentGateway_ChannelServer) error {
	agentID := peerAgentID(certChain(stream.Context()))
	if agentID == "" {
		return status.Error(codes.Unauthenticated, "缺少客户端证书（请先 enroll）")
	}
	node, err := g.h.repo.GetNodeByAgentID(stream.Context(), agentID)
	if err != nil {
		// 证书有效但控制面不认识这个身份：节点记录可能被删除。
		// 让 agent 重新走 enroll（它会拿到新身份或重绑旧节点）。
		return status.Error(codes.PermissionDenied, "未知 agent 身份，请重新注册")
	}

	c := &conn{
		nodeID:     node.ID,
		agentID:    agentID,
		outbox:     make(chan *serverToAgent, 32),
		pending:    map[string]*pendingOp{},
		superseded: make(chan struct{}),
	}
	g.h.attach(c)
	defer g.h.detach(c)
	log.Printf("[agent-hub] 节点 %d（%s）通道建立", node.ID, node.Name)

	// 下行 writer。
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-stream.Context().Done():
				return
			case <-c.superseded:
				return
			case m := <-c.outbox:
				if err := stream.Send(m); err != nil {
					return
				}
			}
		}
	}()

	// 上行读取放进独立 goroutine：主循环要能在「被新连接顶替」时退出。
	// Recv 是阻塞调用，没法 select，只能这样把它变成可取消的。
	//
	// 结束信号用 **close(recvDone)**，不是往 channel 里发一个带 err 的结果：
	// 客户端断开时 stream.Context().Done() 与「把失败结果送出去」会同时就绪，
	// select 随机选一个——选中 Done 就等于把断开事件吞掉，主循环永远等不到，
	// 这条连接在 conns 里变成僵尸（Online 一直为真，Execute 把指令投进没人
	// 读的 outbox，直到超时）。close 不会被 select 丢掉。
	recvCh := make(chan *agentproto.AgentToServer, 1)
	recvDone := make(chan struct{})
	// recvErr 只由读 goroutine 写、在 recvDone 关闭之后由主循环读，
	// close 建立 happens-before，因此不需要额外加锁。
	var recvErr error
	go func() {
		defer close(recvDone)
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr = err
				return
			}
			select {
			case recvCh <- msg:
			case <-stream.Context().Done():
				recvErr = stream.Context().Err()
				return
			}
		}
	}()

	for {
		select {
		case <-c.superseded:
			<-writerDone
			log.Printf("[agent-hub] 节点 %d（%s）通道被新连接顶替，旧通道关闭", node.ID, node.Name)
			// 返回非 nil 会让 gRPC 立刻结束这条流，agent 侧看到错误后重连，
			// 从而由**真正活着的那一端**重新登记进 conns。
			return status.Error(codes.Aborted, "该节点已建立新的通道，旧通道关闭")
		case <-recvDone:
			// 先把已到达的消息处理完再退出：它们是已经收到的，丢掉会让
			// 最后一次结果（例如关机的最终状态）凭空消失。
			g.drainInbound(stream.Context(), c, recvCh)
			<-writerDone
			log.Printf("[agent-hub] 节点 %d（%s）通道断开: %v", node.ID, node.Name, recvErr)
			return nil // 断开是常态（网络抖动/agent 重启），不是错误
		case msg := <-recvCh:
			g.dispatchInbound(stream.Context(), c, msg)
		}
	}
}

// dispatchInbound 把一条上行消息交给对应的处理器。
func (g *gateway) dispatchInbound(ctx context.Context, c *conn, msg *agentproto.AgentToServer) {
	switch p := msg.Payload.(type) {
	case *agentproto.AgentToServer_Heartbeat:
		g.h.onHeartbeat(ctx, c.nodeID, p.Heartbeat)
	case *agentproto.AgentToServer_Stage:
		g.h.onStage(c, p.Stage)
	case *agentproto.AgentToServer_Result:
		g.h.onResult(c, p.Result)
	case *agentproto.AgentToServer_StreamOpened:
		c.mu.Lock()
		ch := c.pendingStreams[p.StreamOpened.StreamId]
		c.mu.Unlock()
		if ch != nil {
			ch <- p.StreamOpened
		}
	default:
		// 未知消息忽略不报错：协议承诺「未知字段/消息不破坏兼容」。
	}
}

// drainInbound 取走缓冲区里已到达的消息并分发，返回时缓冲区已空。
func (g *gateway) drainInbound(ctx context.Context, c *conn, ch chan *agentproto.AgentToServer) {
	for {
		select {
		case msg := <-ch:
			g.dispatchInbound(ctx, c, msg)
		default:
			return
		}
	}
}

// StreamData 承载节点回传的字节流（VNC 等）。流标识经请求头携带，
// 转发与桥接逻辑在 stream.go。
func (g *gateway) StreamData(stream agentproto.AgentGateway_StreamDataServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	id := streamIDFromContext(md)
	if id == "" {
		return status.Error(codes.InvalidArgument, "缺少 x-kc-stream-id")
	}
	g.h.serveAgentStream(id, stream)
	return nil
}

// Serve 在 ln 上启动 gRPC 网关（阻塞）。TLS 与客户端证书校验见 certManager。
func (h *Hub) Serve(ln net.Listener) error {
	// 32MB：导出分片（8MB）与截帧等大负载需要显式余量——gRPC 默认 4MB
	// 收包上限会把任何真实规模的导出分片直接拦下（真机验证前的静态推演）。
	gs := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(h.certs.TLSConfig())),
		grpc.MaxRecvMsgSize(32<<20), grpc.MaxSendMsgSize(32<<20),
	)
	agentproto.RegisterAgentGatewayServer(gs, &gateway{h: h})
	h.mu.Lock()
	h.gs = gs
	h.mu.Unlock()
	return gs.Serve(ln)
}

// gatewayStopGrace 是「等 agent 连接自己收尾」的上限。
//
// 取 3s 而不是 main 里 HTTP 那 5s 的原因：这里要等的对象只有 agent 的
// **长连接双向流**，它们的正常寿命是整个进程；再怎么等也不会自己结束，
// 超过几秒只会让运维看到「收到 SIGTERM 了但进程还活着」。3s 足够让正在
// 发送中的下行消息写出去，又短到不会让人以为停机卡死。
const gatewayStopGrace = 3 * time.Second

// Stop 停止网关并关闭全部节点连接。
//
// 为什么不能只调 GracefulStop：grpc.Server.GracefulStop 会等所有在途 RPC
// **自然结束**，而 agent 的 Channel（见本文件 Channel 方法）是长连接双向
// 流——只要节点在线，它的 Recv 就永远阻塞着。于是「等它结束」等于「等到
// 世界末日」：实机上进程收到 SIGTERM 后存活 22 分钟不退出，继续持有
// 192.168.64.x:18443 的 ESTABLISHED 连接，并且还在把心跳写进共享库，
// 造成「节点列表显示在线、新实例的连接表却是空的、所有指令送不达」。
//
// 所以这里做的是「有限等待 + 强制停止」：给在途传输一点收尾窗口，超时就
// 断连接。断连接会依次触发 Channel 的 Recv 返回错误 → defer detach →
// conn.closeAll，把每个在途 Execute 以「结果未知」收场（见 closeAll 注释）。
func (h *Hub) Stop() {
	h.mu.Lock()
	gs := h.gs
	// 连接数只用于日志，取一次快照即可；强制停止过程中它必然在变。
	online := len(h.conns)
	h.mu.Unlock()
	if gs == nil {
		return
	}
	if stopGateway(gs, gatewayStopGrace) {
		log.Printf("[agent-hub] 优雅停止超时（%s），强制关闭 %d 条节点连接："+
			"agent 的 Channel 是长连接，GracefulStop 会一直等它自然结束", gatewayStopGrace, online)
	}
}

// grpcStopper 只取 grpc.Server 与测试替身共有的两个停止方法。
//
// 收窄到这两方法是为了让「超时后强制停止」这条路径能在单测里被抓住：
// 直接依赖 *grpc.Server 的话，唯一可行的验证方式是起真实 mTLS 服务并
// 伪造一条永不结束的流——太重，于是没人写，缺陷也就一直留着。
type grpcStopper interface {
	GracefulStop()
	Stop()
}

// stopGateway 先尝试优雅停止，超时后强制停止。返回值表示是否走了强制停止。
//
// 并发安全的前提（grpc-go v1.84 server.go 的 stop 实现，已核对）：
//   - GracefulStop 与 Stop 都只 Fire 同一个 quit/done 事件，Fire 是 CAS 的，
//     重复与并发调用都安全；
//   - GracefulStop 在 "等待连接数归零" 处阻塞时用的是条件变量（已释放
//     s.mu），因此并发的 Stop 能拿到锁、关闭所有传输、把连接数打到 0，
//     从而让那条 GracefulStop 也随之返回。
//
// gs 不能为 nil（nil 判断在调用方）：helper 只负责按时序决策，不替调用方
// 兜空值，避免「传空就静默成功」这种最难查的假象。
func stopGateway(gs grpcStopper, grace time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		gs.GracefulStop()
	}()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		return false
	case <-timer.C:
		// 在途 RPC 里有永不结束的长连接流：Stop 会立刻关闭监听与全部
		// 连接，使 Serve 返回、Channel 的 Recv 报错退出。
		gs.Stop()
		return true
	}
}

func newAgentID() string {
	// uuid v4（github.com/google/uuid 已在依赖图中）。
	return uuid.NewString()
}

// peerAgentID 从 TLS 对端证书取 CN（= agent_id）。无证书返回空串。
func peerAgentID(certs []*x509.Certificate) string {
	if len(certs) == 0 {
		return ""
	}
	return strings.TrimSpace(certs[0].Subject.CommonName)
}

// certChain 取 gRPC 对端的证书链；无 TLS 对端时返回 nil。
func certChain(ctx context.Context) []*x509.Certificate {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return nil
	}
	if tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo); ok {
		return tlsInfo.State.PeerCertificates
	}
	return nil
}
