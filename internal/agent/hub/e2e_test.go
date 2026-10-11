package hub_test

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"k_cockpit/internal/agent"
	agenthub "k_cockpit/internal/agent/hub"
	agentproto "k_cockpit/internal/agentproto"
	"k_cockpit/internal/model"
	"k_cockpit/internal/nodeagent"

	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	noderepo "k_cockpit/internal/repository/node"
)

// fakeRegistrar 模拟 node.Service.Register 的最小语义：校验通过后把
// agent 身份写到节点行（真实实现还记审计，与本测试无关）。
type fakeRegistrar struct {
	repo *noderepo.Repo
}

func (f fakeRegistrar) Register(
	ctx context.Context, token, agentID, agentVersion string,
) (int64, string, string, error) {
	if token != "test-token" {
		return 0, "", "", errors.New("注册令牌无效或已被使用")
	}
	if err := f.repo.UpdateNode(ctx, 1, map[string]any{"agent_id": agentID}); err != nil {
		return 0, "", "", err
	}
	return 1, "test-node", agentID, nil
}

// TestGatewayEndToEnd 走完 agent 的完整生命周期：
// enroll（令牌换证书）→ Channel 上线（心跳落库）→ Execute（阶段透传 +
// 类型化解码）→ 断线（在途操作得到「未送达」语义）。
//
// 这是对 P0 验收标准（面板 online/offline/指令往返）的进程内全真演练：
// 真实 TLS（自签 CA）、真实注册 RPC、真实双向流，只有 libvirt 不在场。
func TestGatewayEndToEnd(t *testing.T) {
	db := openTestDB(t)
	repo := noderepo.NewRepo(db)
	// 操作员先在面板创建待接入节点（真实流程的第一步）。
	if err := db.Create(&model.Node{ID: 1, Name: "test-node", EnrollState: model.NodeEnrollPending}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	pkiDir := filepath.Join(t.TempDir(), "pki")
	h, err := agenthub.New(agenthub.Deps{
		Repo:      repo,
		Registrar: fakeRegistrar{repo: repo},
		TLSDir:    pkiDir,
	})
	if err != nil {
		t.Fatalf("构造 hub: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	defer ln.Close()
	go func() { _ = h.Serve(ln) }()

	// --- agent 侧：enroll ---
	sd := nodeagent.StateDir{Dir: t.TempDir()}
	res, err := nodeagent.Enroll(context.Background(), nodeagent.EnrollInput{
		Server:  ln.Addr().String(),
		Token:   "test-token",
		AgentID: "11111111-1111-4111-8111-111111111111",
		Version: "test-1.0",
		TLS:     sd.EnrollTLS(),
	})
	if err != nil {
		t.Fatalf("enroll 失败: %v", err)
	}
	if res.State.AgentID == "" || len(res.CertPEM) == 0 || len(res.CaPEM) == 0 {
		t.Fatalf("enroll 材料不完整: %+v", res.State)
	}
	if err := sd.SaveEnrollment(&res.State, res.CaPEM, res.CertPEM, res.KeyPEM); err != nil {
		t.Fatalf("保存身份: %v", err)
	}
	// 错误令牌应被拒绝（防枚举文案由 service 层负责，这里验证拒绝本身）。
	if _, err := nodeagent.Enroll(context.Background(), nodeagent.EnrollInput{
		Server: ln.Addr().String(), Token: "wrong", AgentID: "x", Version: "v", TLS: sd.EnrollTLS(),
	}); err == nil {
		t.Fatal("错误令牌应当被拒绝")
	}

	// --- agent 侧：常驻通道 ---
	clientTLS, err := sd.ClientTLS()
	if err != nil {
		t.Fatalf("构造客户端 TLS: %v", err)
	}
	registry := nodeagent.Registry{
		"test.op": func(ctx context.Context, op agent.Operation, report nodeagent.Reporter) (*agent.Result, error) {
			report(nodeagent.Stage{Key: "step_one", Name: "第一步", Index: 1, Total: 2})
			report(nodeagent.Stage{Key: "step_two", Name: "第二步", Index: 2, Total: 2})
			return &agent.Result{
				Success: true,
				Message: "完成",
				Data:    map[string]any{"status": "running", "count": 3},
			}, nil
		},
	}
	dctx, stopDaemon := context.WithCancel(context.Background())
	defer stopDaemon()
	daemon := &nodeagent.Daemon{
		Server: ln.Addr().String(), AgentID: res.State.AgentID,
		AgentVersion: "test-1.0", Hostname: "test-host",
		TLS: clientTLS, Registry: registry,
	}
	go func() { _ = daemon.Run(dctx) }()

	// 通道建立（首帧心跳随即落库）。
	waitFor(t, "节点上线", func() bool { return h.Online(1) })
	waitFor(t, "心跳落库", func() bool {
		snap, err := h.Snapshot(context.Background(), 1)
		return err == nil && snap.Status == agent.StatusOnline
	})

	// --- Execute 全链路 ---
	var gotStages []agent.Stage
	res2, err := h.Execute(context.Background(), agent.Operation{
		Kind: "test.op", NodeID: 1, Target: "vm-1",
		OnStage: func(s agent.Stage) { gotStages = append(gotStages, s) },
	})
	if err != nil {
		t.Fatalf("Execute 失败: %v", err)
	}
	if !res2.Success || res2.Message != "完成" {
		t.Fatalf("结果不符合: %+v", res2)
	}
	// 类型化解码：结构键（此例未登记 typedDecoders，走通用 JSON）。
	if s, _ := res2.Data["status"].(string); s != "running" {
		t.Errorf("Data[status] 应为 running，实际 %v", res2.Data["status"])
	}
	if f, _ := res2.Data["count"].(float64); f != 3 {
		t.Errorf("Data[count] 应为 3（JSON 数字），实际 %v", res2.Data["count"])
	}
	if len(gotStages) != 2 || gotStages[0].Key != "step_one" || gotStages[1].Index != 2 {
		t.Errorf("阶段透传不符: %+v", gotStages)
	}

	// 未实现的操作得到明确失败（不是「未送达」）。
	res3, err := h.Execute(context.Background(), agent.Operation{Kind: "vm.create", NodeID: 1})
	if err != nil {
		t.Fatalf("未实现操作不应返回传输错误: %v", err)
	}
	if res3.Success || res3.Message == "" {
		t.Errorf("未实现操作应返回失败的 Result，实际 %+v", res3)
	}

	// --- 断线：在途与后续操作回到「未送达」语义 ---
	stopDaemon()
	waitFor(t, "节点下线（连接注销）", func() bool { return !h.Online(1) })
	_, err = h.Execute(context.Background(), agent.Operation{Kind: "test.op", NodeID: 1})
	if err == nil {
		t.Fatal("节点离线后 Execute 应返回错误（未送达）")
	}
}

// TestRegisterRequiresToken 无证书直连 Channel 被拒：鉴权两态的正确侧。
func TestRegisterRequiresToken(t *testing.T) {
	db := openTestDB(t)
	repo := noderepo.NewRepo(db)
	pkiDir := filepath.Join(t.TempDir(), "pki")
	h, err := agenthub.New(agenthub.Deps{Repo: repo, Registrar: fakeRegistrar{repo}, TLSDir: pkiDir})
	if err != nil {
		t.Fatalf("构造 hub: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听: %v", err)
	}
	defer ln.Close()
	go func() { _ = h.Serve(ln) }()

	// 无证书客户端直接开 Channel：连接应被服务端拒绝（Unauthenticated）。
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpcDialNoCert(dialCtx, ln.Addr().String())
	if err != nil {
		t.Fatalf("拨号: %v", err)
	}
	defer func() { _ = conn.Close() }()
	stream, err := agentproto.NewAgentGatewayClient(conn).Channel(dialCtx)
	if err != nil {
		t.Fatalf("打开流: %v", err)
	}
	err = stream.Send(&agentproto.AgentToServer{Payload: &agentproto.AgentToServer_Heartbeat{
		Heartbeat: &agentproto.Heartbeat{AgentVersion: "v", ProtocolVersion: 1},
	}})
	if err == nil {
		_ = stream.CloseSend()
		_, err = stream.Recv()
	}
	if err == nil {
		t.Fatal("无客户端证书的 Channel 应被拒绝")
	}
}

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "hub.db"),
		MaxOpenConns: 1, MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.Node{}, &model.AuditLog{}); err != nil {
		t.Fatalf("建表: %v", err)
	}
	return db
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

// grpcDialNoCert 供测试用的「TLS 但无客户端证书」拨号——专测鉴权两态中
// 「连接进了网关但没有身份」的那一支（服务端必须拒绝业务方法）。
func grpcDialNoCert(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	return grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			InsecureSkipVerify: true, MinVersion: tls.VersionTLS12,
		})),
		grpc.WithBlock())
}
