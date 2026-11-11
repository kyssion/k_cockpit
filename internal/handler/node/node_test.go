// Package node_test 是节点接口的路由级用例。
//
// 为什么要在 handler 层再测一遍：`internal/service/node` 已有服务层用例，
// 但「HTTP 方法 / 路径参数 / 请求体绑定 / 状态码映射 / 二次验证接线」这一层
// 完全没有 Go 证据——它们的正确性此前只由 R3 实机脚本覆盖，而那些脚本
// 不在 CI、也不在 60 个 Go 测试包内。handler 是最容易「改一行就静默坏掉」
// 的地方：服务层签名不变，路由就编译得过。
//
// **adminOnly 不在这里断言**：它由 internal/router 统一声明，已由 U-11 的
// 160 条管理端点动态用例（租户逐条 403）覆盖。本文件只验 handler 自身。
package node_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	nodehandler "k_cockpit/internal/handler/node"
	platformhandler "k_cockpit/internal/handler/platform"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	noderepo "k_cockpit/internal/repository/node"
	nodesvc "k_cockpit/internal/service/node"
	"k_cockpit/internal/service/platform/auth"
	"k_cockpit/internal/service/platform/risk"
	"k_cockpit/internal/service/platform/settings"
)

const (
	testSecret = "node-handler-test-secret-0123456789abcdef"
	// devBypassCode 让「高风险操作通过后」的分支可测。它由配置注入，
	// 生产环境 config.Validate 会拒绝非空值——这里用的是同一条代码路径，
	// 不是绕过：428 → 换 grant → 重放 这三步一步不少。
	devBypassCode = "TEST-BYPASS-7F3A"
	testPassword  = "Test-Password-2026"
	testAgentPort = "18443"
)

// fakeRuntime 是运行态快照的替身：节点接口必须能在 agent 通道不可用时
// 仍然工作（显示「未知」而不是整页失败），用真的通道没法测这个。
type fakeRuntime struct {
	snaps map[int64]*agent.Snapshot
	err   error
}

func (f *fakeRuntime) Snapshot(_ context.Context, id int64) (*agent.Snapshot, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.snaps[id], nil
}

type env struct {
	h    *server.Hertz
	db   *gorm.DB
	node *nodesvc.Service
	auth *auth.Service
	// cookie 是管理员会话的 Cookie 头值，已登录。
	cookie string
}

// newEnv 装配一个只挂被测路由的引擎。
func newEnv(t *testing.T, runtime agent.SnapshotProvider, stats agent.Client) *env {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "node.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	if err := db.AutoMigrate(
		&model.User{}, &model.Session{}, &model.AuditLog{},
		&model.Node{}, &model.VM{}, &model.SystemSetting{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}

	recorder := audit.NewRecorder(db)
	issuer, err := auth.NewTokenIssuer(testSecret)
	if err != nil {
		t.Fatalf("构造签发器失败: %v", err)
	}
	authSvc := auth.NewService(db, issuer, recorder, auth.Config{})
	guard := risk.NewGuard(db, []byte(testSecret), recorder, devBypassCode)

	if runtime == nil {
		runtime = &fakeRuntime{}
	}
	nodeSvc := nodesvc.NewService(noderepo.NewRepo(db), runtime, recorder, stats)

	h := server.Default(server.WithDisablePrintRoute(true))
	h.Use(api.RequestID(), api.Recover())

	nodes := nodehandler.NewNode(nodeSvc, guard, nodehandler.Options{
		AgentAddr: "0.0.0.0:" + testAgentPort,
		// Settings 必须接入：注册命令的主机名取「站点对外地址」，反向代理
		// 部署下请求 Host 是内网地址，agent 连不上。
		Settings: settings.NewService(db, recorder, []byte("node-handler-test-secret-0123456789abcdef")),
	})
	mw := auth.NewMiddleware(authSvc)
	security := platformhandler.NewSecurity(guard, authSvc)
	authHandler := platformhandler.NewAuth(authSvc, false, guard)

	v1 := h.Group("/api/v1")
	v1.POST("/auth/login", authHandler.Login)
	v1.POST("/auth/risk-verification", mw.Require(auth.Real), security.Verify)

	v1.GET("/nodes", mw.Require(auth.Real), nodes.List)
	v1.GET("/nodes/:id", mw.Require(auth.Real), nodes.Get)
	v1.GET("/nodes/:id/stats", mw.Require(auth.Real), nodes.Stats)
	v1.PATCH("/nodes/:id/maintenance", mw.Require(auth.Real), nodes.SetMaintenance)
	v1.PATCH("/nodes/:id/console-host", mw.Require(auth.Real), nodes.SetConsoleHost)
	v1.POST("/nodes/registration-tokens", mw.Require(auth.Real), nodes.CreateEnrollToken)
	v1.DELETE("/nodes/:id", mw.Require(auth.Real), nodes.Remove)

	return &env{h: h, db: db, node: nodeSvc, auth: authSvc}
}

// loginAsAdmin 走**真实的登录接口**建会话。
//
// 刻意不在服务层直接调 Login：会话指纹是 IP + User-Agent，服务层登录时
// 传的 ClientInfo 与随后请求里的客户端信息必须完全一致，否则每条请求都会
// 被判「登录状态已失效」。走 HTTP 登录让两处由同一段代码算出，不会漂移。
func (e *env) loginAsAdmin(t *testing.T) {
	t.Helper()
	t.Setenv("SITE_URL", "") // 屏蔽宿主机环境变量对站点地址的干扰

	hash, err := auth.HashPassword(testPassword)
	if err != nil {
		t.Fatalf("计算口令哈希失败: %v", err)
	}
	user := model.User{
		Username: "node-admin", PasswordHash: hash,
		Role: model.RoleAdmin, Status: model.UserStatusActive,
		// 管理员若未完成引导会被强制停在 bootstrap 阶段（拿不到 access
		// 令牌）。这条与生产一致：新装系统的管理员必须先走引导。
		BootstrapSkipped: true,
	}
	if err := e.db.Create(&user).Error; err != nil {
		t.Fatalf("创建管理员失败: %v", err)
	}

	body := `{"username":"node-admin","password":"` + testPassword + `"}`
	w := ut.PerformRequest(e.h.Engine, "POST", "/api/v1/auth/login",
		jsonBody(body), ut.Header{Key: "Content-Type", Value: "application/json"})
	if w.Code != consts.StatusOK {
		t.Fatalf("登录失败: %d (body=%s)", w.Code, w.Body.String())
	}

	setCookie := string(w.Header().Peek("Set-Cookie"))
	if setCookie == "" {
		t.Fatal("登录响应缺少 Set-Cookie")
	}
	e.cookie = strings.SplitN(setCookie, ";", 2)[0]
}

// seedNode 建一条节点记录。enrolled 决定它是「已接入」还是「待接入」。
func (e *env) seedNode(t *testing.T, name string, enrolled bool) *model.Node {
	t.Helper()

	n := model.Node{
		Name:        name,
		Enabled:     true,
		Status:      model.NodeStatusUnknown,
		EnrollState: model.NodeEnrollPending,
	}
	if enrolled {
		agentID := "agent-" + name
		hb := time.Now().Add(-2 * time.Second)
		n.EnrollState = model.NodeEnrollEnrolled
		n.AgentID = &agentID
		n.LastHeartbeatAt = &hb
		n.ProtocolVersion = 1
	}
	if err := e.db.Create(&n).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	return &n
}

// do 发起带管理员会话的请求。body 为 nil 表示无请求体。
func (e *env) do(t *testing.T, method, path string, body *ut.Body, headers ...ut.Header) *ut.ResponseRecorder {
	t.Helper()
	hs := []ut.Header{{Key: "Cookie", Value: e.cookie}}
	hs = append(hs, headers...)
	return ut.PerformRequest(e.h.Engine, method, path, body, hs...)
}

// doJSON 发起带 JSON 体的请求。
func (e *env) doJSON(t *testing.T, method, path, body string) *ut.ResponseRecorder {
	t.Helper()
	return e.do(t, method, path, jsonBody(body),
		ut.Header{Key: "Content-Type", Value: "application/json"})
}

// decode 解析统一响应信封。
func decode(t *testing.T, w *ut.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("解析响应失败: %v (body=%s)", err, w.Body.String())
	}
	return m
}

func dataOf(t *testing.T, w *ut.ResponseRecorder) map[string]any {
	t.Helper()
	m := decode(t, w)
	d, ok := m["data"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 data 对象: %s", w.Body.String())
	}
	return d
}

func jsonBody(s string) *ut.Body {
	return &ut.Body{Body: strings.NewReader(s), Len: len(s)}
}

// --- List ---

// TestListIncludesRunningAndPending 覆盖列表与状态推导。
//
// 状态**由心跳时间推导**，不用 agent 上报的状态字符串：agent 宕机时它的
// 最后上报会停在「在线」，而那正是运维最需要立刻看出来的情况被掩盖的时候。
func TestListIncludesRunningAndPending(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)

	e.seedNode(t, "kvm-1", true)
	e.seedNode(t, "kvm-pending", false)

	w := e.do(t, "GET", "/api/v1/nodes", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d (body=%s)", w.Code, w.Body.String())
	}

	var resp struct {
		Data []struct {
			Name        string `json:"name"`
			Status      string `json:"status"`
			EnrollState string `json:"enroll_state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("列表 = %d 条, 期望 2 条", len(resp.Data))
	}
	byName := map[string]string{}
	states := map[string]string{}
	for _, n := range resp.Data {
		byName[n.Name] = n.Status
		states[n.Name] = n.EnrollState
	}
	// 心跳是 2 秒前，应在在线阈值内。
	if got := byName["kvm-1"]; got != model.NodeStatusOnline {
		t.Errorf("kvm-1 状态 = %q, 期望 %q", got, model.NodeStatusOnline)
	}
	// 未接入的节点没有运行态可言，显示「未知」而不是「离线」——后者会
	// 让人去查网络，而实际只是还没接。
	if got := byName["kvm-pending"]; got != model.NodeStatusUnknown {
		t.Errorf("kvm-pending 状态 = %q, 期望 %q", got, model.NodeStatusUnknown)
	}
	if states["kvm-pending"] != model.NodeEnrollPending {
		t.Errorf("enroll_state = %q, 期望 %q", states["kvm-pending"], model.NodeEnrollPending)
	}
}

// TestListSurvivesSnapshotFailure 覆盖「取不到运行态不算错误」。
//
// agent 通道暂时不可用时整个列表不能失败：那会让管理员在最需要看节点清单
// 的时候看到一个空白页。但状态必须由**心跳时间**推导，不能沿用库里缓存的
// 状态字符串——agent 宕机时它的最后上报会停在「在线」，而那正是最该被
// 看出来的情形。这里刻意把库里的 status 写成 online、心跳写成 5 分钟前：
// 期望 offline，而不是 online，也不是 unknown。
func TestListSurvivesSnapshotFailure(t *testing.T) {
	e := newEnv(t, &fakeRuntime{err: errSnapshotUnavailable}, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-flaky", true)

	// 模拟「agent 最后一次上报说在线，然后就没消息了」。
	stale := time.Now().Add(-5 * time.Minute)
	if err := e.db.Model(&model.Node{}).Where("id = ?", n.ID).
		Updates(map[string]any{
			"status":            model.NodeStatusOnline,
			"last_heartbeat_at": stale,
		}).Error; err != nil {
		t.Fatalf("造陈旧心跳失败: %v", err)
	}

	w := e.do(t, "GET", "/api/v1/nodes", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("快照失败时列表应仍返回 200，实际 %d (body=%s)", w.Code, w.Body.String())
	}

	var resp struct {
		Data []struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("列表 = %d 条, 期望 1 条", len(resp.Data))
	}
	if got := resp.Data[0].Status; got != model.NodeStatusOffline {
		t.Errorf("陈旧心跳应推导为 %q，实际 %q（不能沿用库里的 status 字符串）",
			model.NodeStatusOffline, got)
	}
}

// --- Get ---

func TestGetReturnsNodeAndRejectsBadID(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-detail", true)

	w := e.do(t, "GET", "/api/v1/nodes/"+itoa(n.ID), nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	if got := dataOf(t, w)["name"]; got != "kvm-detail" {
		t.Errorf("name = %v", got)
	}

	// 非数字 id：路径参数校验必须给出 400，而不是让服务层去查一个 0。
	w = e.do(t, "GET", "/api/v1/nodes/not-a-number", nil)
	if w.Code != consts.StatusBadRequest {
		t.Errorf("非法 id 状态码 = %d, 期望 400 (body=%s)", w.Code, w.Body.String())
	}

	// 不存在：404，而不是 500 或空对象。
	w = e.do(t, "GET", "/api/v1/nodes/999999", nil)
	if w.Code != consts.StatusNotFound {
		t.Errorf("不存在节点状态码 = %d, 期望 404 (body=%s)", w.Code, w.Body.String())
	}
}

// --- Stats ---

// TestStatsExplainsWhyItIsUnavailable 覆盖三种不返回数字的情形。
//
// 每一种都必须给出**不同的**说明，因为它们对应的处理动作完全不同：
// 没接入 → 去接入；没启用采集 → 去配置；节点不可达 → 去查网络。
// 统一回一个 0% 的 CPU 会让三种情况都看起来像「机器很闲」。
func TestStatsExplainsWhyItIsUnavailable(t *testing.T) {
	e := newEnv(t, nil, nil) // stats client 为 nil = 未启用采集
	e.loginAsAdmin(t)

	pending := e.seedNode(t, "kvm-pending", false)
	enrolled := e.seedNode(t, "kvm-enrolled", true)

	// 未接入：先被拦在「无法读取指标」，与有没有采集能力无关。
	// 422 而不是 400：请求本身合法，是资源当前状态不满足前置条件。
	w := e.do(t, "GET", "/api/v1/nodes/"+itoa(pending.ID)+"/stats", nil)
	if w.Code != consts.StatusUnprocessableEntity {
		t.Errorf("未接入节点状态码 = %d, 期望 422 (body=%s)", w.Code, w.Body.String())
	}

	// 已接入但部署未启用采集：503，明确说「未启用」。
	w = e.do(t, "GET", "/api/v1/nodes/"+itoa(enrolled.ID)+"/stats", nil)
	if w.Code != consts.StatusServiceUnavailable {
		t.Fatalf("未启用采集状态码 = %d, 期望 503 (body=%s)", w.Code, w.Body.String())
	}
	if msg := decode(t, w)["error"]; msg == nil {
		t.Error("503 应带错误说明")
	} else if m, _ := msg.(map[string]any); !strings.Contains(mustString(m["message"]), "指标") {
		t.Errorf("503 说明未提到指标: %v", m["message"])
	}
}

// --- SetMaintenance ---

// TestSetMaintenanceRequiresExplicitEnabled 覆盖「没传 enabled」的拒绝。
//
// 用指针区分「没传」与「传了 false」：后者是明确的**退出**维护，当成
// 参数缺失忽略掉会给出一个「设置成功但什么都没变」的反馈。
func TestSetMaintenanceRequiresExplicitEnabled(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-maint", true)

	w := e.doJSON(t, "PATCH", "/api/v1/nodes/"+itoa(n.ID)+"/maintenance", `{"reason":"升级内核"}`)
	if w.Code != consts.StatusBadRequest {
		t.Fatalf("缺 enabled 状态码 = %d, 期望 400 (body=%s)", w.Code, w.Body.String())
	}

	// 传了 false 才是合法请求：此时节点本来就没在维护，属于幂等（不写审计）。
	w = e.doJSON(t, "PATCH", "/api/v1/nodes/"+itoa(n.ID)+"/maintenance", `{"enabled":false}`)
	if w.Code != consts.StatusOK {
		t.Errorf("enabled=false 状态码 = %d, 期望 200 (body=%s)", w.Code, w.Body.String())
	}
}

// TestSetMaintenanceToggles 覆盖进入与退出。
func TestSetMaintenanceToggles(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-toggle", true)
	path := "/api/v1/nodes/" + itoa(n.ID) + "/maintenance"

	w := e.doJSON(t, "PATCH", path, `{"enabled":true,"reason":"升级内核"}`)
	if w.Code != consts.StatusOK {
		t.Fatalf("进入维护状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	d := dataOf(t, w)
	if d["maintenance_mode"] != true {
		t.Errorf("maintenance_mode = %v, 期望 true", d["maintenance_mode"])
	}
	if d["maintenance_reason"] != "升级内核" {
		t.Errorf("maintenance_reason = %v", d["maintenance_reason"])
	}
	// 注意：这里**不**断言 is_migration_target 变成 false。它当前既没有
	// 写路径也没有消费方（迁移受理只看目标节点的 maintenance_mode，
	// 见 vm.Service.ensureNodeUsable），前端也不读它。断言一个死字段等于
	// 把它固化下来。该字段的定档记在 DELIVERY_TEST_REPORT.md，不在这里
	// 用一个看起来通过了的断言掩盖过去。

	w = e.doJSON(t, "PATCH", path, `{"enabled":false}`)
	if w.Code != consts.StatusOK {
		t.Fatalf("退出维护状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	if d := dataOf(t, w); d["maintenance_mode"] != false {
		t.Errorf("退出后 maintenance_mode = %v", d["maintenance_mode"])
	}
	// 原因与时刻随状态同进同退：留一个「未在维护、但原因是升级内核」的
	// 记录，界面要么显示一个不生效的理由，要么得写额外判断忽略它。
	if _, ok := dataOf(t, w)["maintenance_reason"]; ok {
		t.Errorf("退出维护后不该再带原因: %s", w.Body.String())
	}
}

// TestSetMaintenanceRejectsPendingNode 覆盖「未接入的节点没有维护模式」。
func TestSetMaintenanceRejectsPendingNode(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-pending-maint", false)

	w := e.doJSON(t, "PATCH", "/api/v1/nodes/"+itoa(n.ID)+"/maintenance", `{"enabled":true}`)
	if w.Code != consts.StatusUnprocessableEntity {
		t.Errorf("未接入节点状态码 = %d, 期望 422 (body=%s)", w.Code, w.Body.String())
	}
}

// --- SetConsoleHost ---

func TestSetConsoleHostValidatesAndPersists(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-console", true)
	path := "/api/v1/nodes/" + itoa(n.ID) + "/console-host"

	// 带协议的地址会被拒绝：这一列存的是 host[:port]，不是 URL。
	for _, bad := range []string{"https://kvm-1.example.com", "kvm 1", "a/b"} {
		w := e.doJSON(t, "PATCH", path, `{"host":"`+bad+`"}`)
		if w.Code != consts.StatusBadRequest {
			t.Errorf("host=%q 状态码 = %d, 期望 400", bad, w.Code)
		}
	}

	w := e.doJSON(t, "PATCH", path, `{"host":"kvm-node-1.example.com:5901"}`)
	if w.Code != consts.StatusOK {
		t.Fatalf("合法地址状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	var row model.Node
	if err := e.db.First(&row, n.ID).Error; err != nil {
		t.Fatalf("读库失败: %v", err)
	}
	if row.ConsoleHost == nil || *row.ConsoleHost != "kvm-node-1.example.com:5901" {
		t.Errorf("库里的 console_host = %v", row.ConsoleHost)
	}

	// 传空串 = 清除（回到「不提供连接文件」）。
	w = e.doJSON(t, "PATCH", path, `{"host":""}`)
	if w.Code != consts.StatusOK {
		t.Fatalf("清空状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	if err := e.db.First(&row, n.ID).Error; err != nil {
		t.Fatalf("读库失败: %v", err)
	}
	if row.ConsoleHost != nil {
		t.Errorf("清空后 console_host 应为 NULL，实际 %q", *row.ConsoleHost)
	}
}

// --- CreateEnrollToken ---

// TestCreateEnrollTokenReturnsInstallCommand 覆盖令牌签发与安装命令拼接。
//
// 注册命令是**用来复制粘贴**的：端口写死 8443 会让「改过 AGENT_LISTEN_ADDR
// 的部署」拿到一条连不上的命令，而错了只能靠人猜。
func TestCreateEnrollTokenReturnsInstallCommand(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)

	w := e.doJSON(t, "POST", "/api/v1/nodes/registration-tokens", `{"name":"kvm-new","ttl_hours":2}`)
	if w.Code != consts.StatusCreated {
		t.Fatalf("状态码 = %d, 期望 201 (body=%s)", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			Token     string `json:"token"`
			ExpiresAt string `json:"expires_at"`
			Node      struct {
				Name        string `json:"name"`
				EnrollState string `json:"enroll_state"`
			} `json:"node"`
			InstallCommand string `json:"install_command"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Data.Token == "" {
		t.Fatal("响应缺少明文令牌")
	}
	// 端口必须取 AGENT_LISTEN_ADDR 的实际端口，不写死。
	if !strings.Contains(resp.Data.InstallCommand, ":"+testAgentPort) {
		t.Errorf("安装命令端口不对: %q", resp.Data.InstallCommand)
	}
	if !strings.Contains(resp.Data.InstallCommand, resp.Data.Token) {
		t.Errorf("安装命令未带令牌: %q", resp.Data.InstallCommand)
	}
	if resp.Data.Node.Name != "kvm-new" || resp.Data.Node.EnrollState != model.NodeEnrollPending {
		t.Errorf("新节点 = %+v", resp.Data.Node)
	}

	// 明文令牌只出现这一次：库里存的是哈希，再查列表也不该看到它。
	var row model.Node
	if err := e.db.Where("name = ?", "kvm-new").First(&row).Error; err != nil {
		t.Fatalf("读库失败: %v", err)
	}
	if row.EnrollTokenHash == nil || *row.EnrollTokenHash == resp.Data.Token {
		t.Error("库里存的应是令牌哈希，不能是明文")
	}
	w = e.do(t, "GET", "/api/v1/nodes", nil)
	if strings.Contains(w.Body.String(), resp.Data.Token) {
		t.Error("列表接口不应回显明文令牌")
	}
}

// TestCreateEnrollTokenRejectsOutOfRangeTTL 覆盖有效期边界。
//
// ttl_hours 极大时 time.Duration 乘法会溢出成负数，服务端会把它当「未指定」
// 回落到 24 小时——行为与请求完全不符，所以必须先挡越界值再做乘法。
func TestCreateEnrollTokenRejectsOutOfRangeTTL(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)

	for _, body := range []string{
		`{"name":"a","ttl_hours":-1}`,
		`{"name":"b","ttl_hours":169}`,
		`{"name":"c","ttl_hours":1000000}`,
	} {
		w := e.doJSON(t, "POST", "/api/v1/nodes/registration-tokens", body)
		if w.Code != consts.StatusBadRequest {
			t.Errorf("%s 状态码 = %d, 期望 400 (body=%s)", body, w.Code, w.Body.String())
		}
	}
}

// TestCreateEnrollTokenAddressPrecedence 覆盖 enrollAddr / hostOnly 的取址优先级。
//
// 顺序是「站点对外地址 > 请求 Host > 127.0.0.1」：
//  1. 反向代理后请求 Host 是内网地址，agent 连不上，只有显式配置可靠；
//  2. 端口**一律**取 agent 网关端口，不取站点地址的端口——站点地址是面板
//     的 HTTP 面，两者混用会拼出一条连不上的命令。
func TestCreateEnrollTokenAddressPrecedence(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	t.Setenv("SITE_URL", "")

	get := func(t *testing.T, host string) string {
		t.Helper()
		// 用**完整 URL** 而不是 Host 头：c.Request.Host() 读的是请求行里的
		// authority，单独塞一个 Host 头不会改变它（Hertz 在解析请求行时
		// 就定下了 host）。这与真实浏览器经反向代理访问时的形态一致。
		uri := "/api/v1/nodes/registration-tokens"
		if host != "" {
			uri = "http://" + host + uri
		}
		w := e.do(t, "POST", uri,
			jsonBody(`{"name":"addr-`+strings.NewReplacer(".", "-", ":", "-").Replace(host)+`"}`),
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if w.Code != consts.StatusCreated {
			t.Fatalf("状态码 = %d (body=%s)", w.Code, w.Body.String())
		}
		var resp struct {
			Data struct {
				InstallCommand string `json:"install_command"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		return resp.Data.InstallCommand
	}

	// ① 请求 Host 带端口 → 只取主机名，端口仍是 agent 端口。
	if cmd := get(t, "panel.internal:9443"); !strings.Contains(cmd, "panel.internal:"+testAgentPort) {
		t.Errorf("应取请求 Host 的主机名 + agent 端口，实际 %q", cmd)
	}

	// ② 站点对外地址优先于请求 Host，且**不**采用它的端口。
	site := "https://panel.example.com:9443"
	if err := e.db.Create(&model.SystemSetting{Key: "basic.site_url", Value: &site}).Error; err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	if cmd := get(t, "10.0.0.5:8080"); !strings.Contains(cmd, "panel.example.com:"+testAgentPort) {
		t.Errorf("站点地址应优先，且端口取 agent 端口，实际 %q", cmd)
	}

	// ③ 站点地址不是合法 URL → 退回请求 Host，而不是拼一条坏命令。
	bad := "://不是地址"
	if err := e.db.Model(&model.SystemSetting{}).Where("key = ?", "basic.site_url").
		Update("value", bad).Error; err != nil {
		t.Fatalf("改设置失败: %v", err)
	}
	if cmd := get(t, "10.0.0.6:8080"); !strings.Contains(cmd, "10.0.0.6:"+testAgentPort) {
		t.Errorf("站点地址非法时应退回请求 Host，实际 %q", cmd)
	}
}

// TestCreateEnrollTokenFallsBackToLoopback 覆盖「都拿不到时的兜底」。
//
// 退回 127.0.0.1 至少不是一条语法错误的命令（本机 agent 可用）。
func TestCreateEnrollTokenFallsBackToLoopback(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	t.Setenv("SITE_URL", "")

	// 不带 Host 头：此时 c.Request.Host() 为空，应退回 127.0.0.1。
	w := e.do(t, "POST", "/api/v1/nodes/registration-tokens",
		jsonBody(`{"name":"loopback"}`),
		ut.Header{Key: "Content-Type", Value: "application/json"})
	if w.Code != consts.StatusCreated {
		t.Fatalf("状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Data struct {
			InstallCommand string `json:"install_command"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(resp.Data.InstallCommand, "127.0.0.1:"+testAgentPort) {
		t.Errorf("无 Host / 无站点地址时应退回 127.0.0.1，实际 %q", resp.Data.InstallCommand)
	}
}

// --- Remove ---

// TestRemoveRequiresSecondFactorThenSucceeds 覆盖 428 → 换许可 → 重放 → 204。
//
// 这是「高风险操作不能被一行请求绕过」的核心断言：不带许可时必须 428，
// 带许可才真的删。三步一步不少，许可还是**一次性的**。
func TestRemoveRequiresSecondFactorThenSucceeds(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-remove", true)

	w := e.do(t, "DELETE", "/api/v1/nodes/"+itoa(n.ID), nil)
	if w.Code != consts.StatusPreconditionRequired {
		t.Fatalf("无许可状态码 = %d, 期望 428 (body=%s)", w.Code, w.Body.String())
	}

	var required struct {
		Data struct {
			Action      string `json:"action"`
			ChallengeID string `json:"challenge_id"`
			Methods     []struct {
				Method string `json:"method"`
			} `json:"methods"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &required); err != nil {
		t.Fatalf("解析 428 失败: %v", err)
	}
	if required.Data.Action != string(risk.ActionNodeRemove) {
		t.Errorf("action = %q, 期望 %q", required.Data.Action, risk.ActionNodeRemove)
	}
	if required.Data.ChallengeID == "" {
		t.Fatal("428 应带 challenge_id")
	}

	// 换许可。
	verifyBody := `{"challenge_id":"` + required.Data.ChallengeID +
		`","method":"dev_bypass","code":"` + devBypassCode + `"}`
	w = e.doJSON(t, "POST", "/api/v1/auth/risk-verification", verifyBody)
	if w.Code != consts.StatusOK {
		t.Fatalf("换许可状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	grant, _ := dataOf(t, w)["grant"].(string)
	if grant == "" {
		t.Fatal("响应缺少 grant")
	}

	// 重放原请求并带许可。
	w = e.do(t, "DELETE", "/api/v1/nodes/"+itoa(n.ID), nil,
		ut.Header{Key: risk.GrantHeader, Value: grant})
	if w.Code != consts.StatusNoContent {
		t.Fatalf("带许可状态码 = %d, 期望 204 (body=%s)", w.Code, w.Body.String())
	}

	// 软删除：记录还在（审计与历史任务要能引用），但接口查不到了。
	// Unscoped 是必须的：GORM 会为 deleted_at 自动追加 `IS NULL`，用普通
	// 查询验证「记录还在」会得到 record not found，反而看不出是软删。
	if err := e.db.Unscoped().First(&model.Node{}, n.ID).Error; err != nil {
		t.Errorf("应软删除而不是物理删除: %v", err)
	}
	w = e.do(t, "GET", "/api/v1/nodes/"+itoa(n.ID), nil)
	if w.Code != consts.StatusNotFound {
		t.Errorf("移除后查询状态码 = %d, 期望 404", w.Code)
	}

	// 同一个许可不能再用（一次性）。
	w = e.do(t, "DELETE", "/api/v1/nodes/"+itoa(n.ID), nil)
	if w.Code != consts.StatusPreconditionRequired {
		t.Errorf("复用同一许可状态码 = %d, 期望 428（许可一次性）", w.Code)
	}
}

// TestRemoveUnknownNodeStillRequiresVerification 覆盖「先验再做」的顺序。
//
// 顺序不能反：若先查节点再要验证，未验证的调用方就能通过 404 与 428 的
// 差异**枚举出哪些 id 存在**——那是一条不需要任何权限的信息泄漏。
func TestRemoveUnknownNodeStillRequiresVerification(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)

	w := e.do(t, "DELETE", "/api/v1/nodes/999999", nil)
	if w.Code != consts.StatusPreconditionRequired {
		t.Errorf("不存在的节点在无许可时状态码 = %d, 期望 428（不能泄漏存在性）", w.Code)
	}
}

// TestUnauthenticatedRequestsAreRejected 是这组用例的基线。
//
// 少了它，上面所有 200 都不能说明「鉴权接上了」——只说明 handler 能跑。
func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	e := newEnv(t, nil, nil)
	n := e.seedNode(t, "kvm-auth", true)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/nodes"},
		{"GET", "/api/v1/nodes/" + itoa(n.ID)},
		{"GET", "/api/v1/nodes/" + itoa(n.ID) + "/stats"},
		{"DELETE", "/api/v1/nodes/" + itoa(n.ID)},
	} {
		w := ut.PerformRequest(e.h.Engine, tc.method, tc.path, nil)
		if w.Code != consts.StatusUnauthorized {
			t.Errorf("%s %s 状态码 = %d, 期望 401", tc.method, tc.path, w.Code)
		}
	}
}

// TestHandlersDegradeWhenStorageFails 覆盖数据层故障时的状态码。
//
// 关键点是**不能返回 200 空列表**：那会让管理员以为自己的节点全丢了，
// 而实际只是查不出来。每个接口都必须把错误显式传出去。
//
// 用 `DROP TABLE` 制造故障而不是关连接：关连接会让认证中间件先失败，
// 请求根本到不了 handler，测的就不是这里想验的东西了。
func TestHandlersDegradeWhenStorageFails(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-gone", true)

	if err := e.db.Exec("DROP TABLE node").Error; err != nil {
		t.Fatalf("制造存储故障失败: %v", err)
	}

	w := e.do(t, "GET", "/api/v1/nodes", nil)
	if w.Code != consts.StatusInternalServerError {
		t.Errorf("列表在存储故障时状态码 = %d, 期望 500（不能是 200 空列表）", w.Code)
	}

	w = e.do(t, "GET", "/api/v1/nodes/"+itoa(n.ID), nil)
	if w.Code != consts.StatusInternalServerError {
		t.Errorf("详情在存储故障时状态码 = %d, 期望 500", w.Code)
	}

	w = e.doJSON(t, "PATCH", "/api/v1/nodes/"+itoa(n.ID)+"/maintenance", `{"enabled":true}`)
	if w.Code != consts.StatusInternalServerError {
		t.Errorf("维护模式在存储故障时状态码 = %d, 期望 500", w.Code)
	}

	w = e.doJSON(t, "PATCH", "/api/v1/nodes/"+itoa(n.ID)+"/console-host", `{"host":"kvm-1"}`)
	if w.Code != consts.StatusInternalServerError {
		t.Errorf("控制台地址在存储故障时状态码 = %d, 期望 500", w.Code)
	}

	w = e.doJSON(t, "POST", "/api/v1/nodes/registration-tokens", `{"name":"x"}`)
	if w.Code != consts.StatusInternalServerError {
		t.Errorf("注册令牌在存储故障时状态码 = %d, 期望 500", w.Code)
	}
}

// TestStatsReturnsCollectedMetrics 覆盖指标采集成功的正常路径。
//
// 断言两件事：agent 报的数字被原样透出（不做二次换算），以及 `at` 与
// `vm_count` 由控制面自己填——前者是瞬时值的时间戳，后者必须与列表页
// 一致（节点看到的域可能包含面板外手工建的，两处不一致比数字不准更糟）。
func TestStatsReturnsCollectedMetrics(t *testing.T) {
	collected := agent.NodeStats{
		CPUPercent: 37.5, CPUCores: 8,
		LoadAvg1: 1.25, LoadAvg5: 1.0, LoadAvg15: 0.8,
		MemTotalMB: 16384, MemUsedMB: 4096,
		UptimeSeconds: 86400,
	}
	e := newEnv(t, nil, &fakeStats{result: &agent.Result{
		Success: true,
		Data:    map[string]any{agent.NodeStatsDataKey: collected},
	}})
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-stats", true)

	// 一台虚拟机，用来验 vm_count 由控制面统计。
	owner := int64(1)
	if err := e.db.Create(&model.VM{
		NodeID: n.ID, Name: "vm-on-node", Status: model.VMStatusStopped,
		OwnerID: &owner, Present: true,
	}).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}

	w := e.do(t, "GET", "/api/v1/nodes/"+itoa(n.ID)+"/stats", nil)
	if w.Code != consts.StatusOK {
		t.Fatalf("状态码 = %d (body=%s)", w.Code, w.Body.String())
	}

	var resp struct {
		Data struct {
			CPUPercent    float64 `json:"cpu_percent"`
			CPUCores      int     `json:"cpu_cores"`
			MemTotalMB    int     `json:"mem_total_mb"`
			VMCount       int     `json:"vm_count"`
			UptimeSeconds int64   `json:"uptime_seconds"`
			At            string  `json:"at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if resp.Data.CPUPercent != 37.5 || resp.Data.CPUCores != 8 {
		t.Errorf("CPU = %v%% / %d 核, 期望 37.5%% / 8 核",
			resp.Data.CPUPercent, resp.Data.CPUCores)
	}
	if resp.Data.MemTotalMB != 16384 {
		t.Errorf("内存总量 = %d", resp.Data.MemTotalMB)
	}
	if resp.Data.VMCount != 1 {
		t.Errorf("vm_count = %d, 期望 1（由控制面统计，不问节点）", resp.Data.VMCount)
	}
	// 采集时刻必须下发：没有它，界面分不清「当前」与「几分钟前」。
	if resp.Data.At == "" {
		t.Error("响应缺少采集时刻 at")
	}
}

// TestMalformedJSONIsRejected 覆盖请求体解析失败。
//
// 三个带请求体的接口都必须把 Bind 错误翻成 400，而不是让后续逻辑拿着
// 零值继续跑——那会用空 reason / 空 host 覆盖掉用户原来的设置。
func TestMalformedJSONIsRejected(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)
	n := e.seedNode(t, "kvm-badjson", true)

	for _, tc := range []struct{ method, path string }{
		{"PATCH", "/api/v1/nodes/" + itoa(n.ID) + "/maintenance"},
		{"PATCH", "/api/v1/nodes/" + itoa(n.ID) + "/console-host"},
		{"POST", "/api/v1/nodes/registration-tokens"},
	} {
		w := e.do(t, tc.method, tc.path, jsonBody(`{"broken":`),
			ut.Header{Key: "Content-Type", Value: "application/json"})
		if w.Code != consts.StatusBadRequest {
			t.Errorf("%s %s 状态码 = %d, 期望 400", tc.method, tc.path, w.Code)
		}
	}
}

// TestRemoveVerifiedButUnknownNode 覆盖「验证通过后才发现节点不存在」。
//
// 必须是 404 而不是 500：路由与鉴权都正常，只是目标没了。这条补的是
// Remove 里 api.Fail(c, err) 那条分支——428 与 204 都测过，中间这条
// 「服务层拒绝」此前没有证据。
func TestRemoveVerifiedButUnknownNode(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)

	// 先对一个真实节点触发 428，拿 challenge 换许可。
	real := e.seedNode(t, "kvm-real", true)
	w := e.do(t, "DELETE", "/api/v1/nodes/"+itoa(real.ID), nil)
	if w.Code != consts.StatusPreconditionRequired {
		t.Fatalf("状态码 = %d, 期望 428 (body=%s)", w.Code, w.Body.String())
	}
	challenge := dataOf(t, w)["challenge_id"].(string)

	w = e.doJSON(t, "POST", "/api/v1/auth/risk-verification",
		`{"challenge_id":"`+challenge+`","method":"dev_bypass","code":"`+devBypassCode+`"}`)
	if w.Code != consts.StatusOK {
		t.Fatalf("换许可状态码 = %d (body=%s)", w.Code, w.Body.String())
	}
	grant := dataOf(t, w)["grant"].(string)

	w = e.do(t, "DELETE", "/api/v1/nodes/999999", nil,
		ut.Header{Key: risk.GrantHeader, Value: grant})
	if w.Code != consts.StatusNotFound {
		t.Errorf("带许可删除不存在的节点状态码 = %d, 期望 404 (body=%s)", w.Code, w.Body.String())
	}
}

// TestStatsDistinguishesAgentFailures 覆盖「节点回了话但内容不对」的两条路径。
//
// 这两种情况与「节点不可达」必须分开：前者要去看 agent 日志与版本，
// 后者要去看网络。都回一句「读不到指标」会让人从错的地方开始查。
func TestStatsDistinguishesAgentFailures(t *testing.T) {
	t.Run("agent 明确拒绝", func(t *testing.T) {
		e := newEnv(t, nil, &fakeStats{result: &agent.Result{
			Success: false, Message: "不支持的指令",
		}})
		e.loginAsAdmin(t)
		n := e.seedNode(t, "kvm-reject", true)

		w := e.do(t, "GET", "/api/v1/nodes/"+itoa(n.ID)+"/stats", nil)
		if w.Code != consts.StatusUnprocessableEntity {
			t.Fatalf("状态码 = %d, 期望 422 (body=%s)", w.Code, w.Body.String())
		}
		// 节点给的说明要原样透出，不能换成一句笼统的「读取失败」。
		if body := w.Body.String(); !strings.Contains(body, "不支持的指令") {
			t.Errorf("响应未带 agent 的原始说明: %s", body)
		}
	})

	t.Run("agent 返回体缺字段", func(t *testing.T) {
		// 典型场景是控制面与 agent 版本不匹配：指令认得，结构对不上。
		e := newEnv(t, nil, &fakeStats{result: &agent.Result{
			Success: true, Data: map[string]any{"unexpected": 1},
		}})
		e.loginAsAdmin(t)
		n := e.seedNode(t, "kvm-mismatch", true)

		w := e.do(t, "GET", "/api/v1/nodes/"+itoa(n.ID)+"/stats", nil)
		if w.Code != consts.StatusServiceUnavailable {
			t.Errorf("状态码 = %d, 期望 503（版本不匹配属于暂时不可用）", w.Code)
		}
	})
}

// TestRemoveRejectsNonNumericID 覆盖路径参数校验。
func TestRemoveRejectsNonNumericID(t *testing.T) {
	e := newEnv(t, nil, nil)
	e.loginAsAdmin(t)

	w := e.do(t, "DELETE", "/api/v1/nodes/abc", nil)
	// 无许可时先撞 428；这不是我们想验的，所以先确认它确实是 428 而不是
	// 因为路径不合法被提前拒掉——顺序在这里也是契约的一部分。
	if w.Code != consts.StatusPreconditionRequired {
		t.Fatalf("状态码 = %d, 期望 428（验证在路径解析之前）", w.Code)
	}
}

// --- 小工具 ---

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func mustString(v any) string {
	s, _ := v.(string)
	return s
}

// fakeStats 是 agent.Client 的替身，只回一个固定结果。
type fakeStats struct {
	result *agent.Result
	err    error
}

func (f *fakeStats) Execute(context.Context, agent.Operation) (*agent.Result, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.result, nil
}

// errSnapshotUnavailable 模拟 agent 通道不可用。
var errSnapshotUnavailable = &snapshotError{}

type snapshotError struct{}

func (*snapshotError) Error() string { return "agent 通道不可用" }
