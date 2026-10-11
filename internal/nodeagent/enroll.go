// enroll.go：一次性注册的 agent 侧实现。
package nodeagent

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentproto "k_cockpit/internal/agentproto"
)

// EnrollInput 是一次注册请求。
type EnrollInput struct {
	Server  string
	Token   string
	AgentID string
	Version string
	// TLS 是 enroll 连接的 TLS 配置（StateDir.EnrollTLS：无客户端证书）。
	TLS *tls.Config
}

// EnrollResult 是注册成功后的全部材料，调用方负责持久化（StateDir.SaveEnrollment）。
type EnrollResult struct {
	State    State
	CaPEM    []byte
	CertPEM  []byte
	KeyPEM   []byte
	NodeName string
}

// Enroll 用一次性令牌换取客户端证书。
//
// 注册语义（令牌校验、状态翻转、审计）在**控制面**的 node.Service 里；
// agent 侧只负责把请求送上去、把材料保存下来。
func Enroll(ctx context.Context, in EnrollInput) (*EnrollResult, error) {
	if in.Server == "" || in.Token == "" {
		return nil, fmt.Errorf("server 与 token 必填")
	}
	if in.TLS == nil {
		return nil, fmt.Errorf("未配置 enroll TLS")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(dialCtx, in.Server,
		grpc.WithTransportCredentials(credentials.NewTLS(in.TLS)),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("连接控制面: %w", err)
	}
	defer func() { _ = conn.Close() }()

	resp, err := agentproto.NewAgentGatewayClient(conn).Register(dialCtx, &agentproto.RegisterRequest{
		Token:           in.Token,
		AgentId:         in.AgentID,
		AgentVersion:    in.Version,
		ProtocolVersion: ProtocolVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("注册被拒绝: %w", err)
	}
	if resp.AgentId == "" {
		return nil, fmt.Errorf("控制面未返回 agent 身份")
	}
	return &EnrollResult{
		State: State{
			AgentID: resp.AgentId,
			Server:  in.Server,
		},
		CaPEM:    resp.CaCert,
		CertPEM:  resp.ClientCert,
		KeyPEM:   resp.ClientKey,
		NodeName: resp.NodeName,
	}, nil
}

// NewRegistry 返回本构建的执行器注册表。
//
// P0：通道骨架，无领域执行器——能力清单为空是**诚实**的（面板显示该
// 节点还没有可执行能力），任何操作都会得到明确的「未实现」结果。P1 起
// 在这里逐个注册探测类执行器。
func NewRegistry() Registry {
	return Registry{}
}
