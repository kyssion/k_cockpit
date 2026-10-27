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
	"strings"

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
		nodeID:  node.ID,
		agentID: agentID,
		outbox:  make(chan *serverToAgent, 32),
		pending: map[string]*pendingOp{},
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
			case m := <-c.outbox:
				if err := stream.Send(m); err != nil {
					return
				}
			}
		}
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			<-writerDone
			log.Printf("[agent-hub] 节点 %d（%s）通道断开: %v", node.ID, node.Name, err)
			return nil // 断开是常态（网络抖动/agent 重启），不是错误
		}
		switch p := msg.Payload.(type) {
		case *agentproto.AgentToServer_Heartbeat:
			g.h.onHeartbeat(stream.Context(), node.ID, p.Heartbeat)
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

// Stop 优雅停止网关：停止接收新连接，等待在途 RPC 收尾。
func (h *Hub) Stop() {
	h.mu.Lock()
	gs := h.gs
	h.mu.Unlock()
	if gs != nil {
		gs.GracefulStop()
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
