package platformcheck_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platformcheck"
	"k_cockpit/internal/task"
)

func newEnv(t *testing.T) (*gorm.DB, *platformcheck.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "pc.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	// 连接必须在测试结束时关闭：Windows 不允许删除仍被占用的数据库文件，
	// 不关连接会让 t.TempDir() 的自动清理失败，进而把测试判为失败。
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(
		&model.PortSecurityPolicy{}, &model.PublicIPBinding{}, &model.PublicIP{},
		&model.PortMirror{}, &model.Node{}, &model.AuditLog{}, &model.Task{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	db.Create(&model.Node{ID: 1, Name: "node-1"})
	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	q := task.NewQueue(db, recorder, task.Options{})
	q.Register(platformcheck.NewExecutor(db, client))
	return db, platformcheck.NewService(db, q, client, recorder)
}

func seedActivePolicy(t *testing.T, db *gorm.DB, port string, status string) {
	t.Helper()
	if err := db.Create(&model.PortSecurityPolicy{
		NodeID: 1, PortRef: port, Status: status,
	}).Error; err != nil {
		t.Fatalf("造数据失败: %v", err)
	}
}

// TestCheckFindsDrift 覆盖本功能的核心。
//
// 面板上显示「端口安全已启用」，而节点上的流表早被一次重启清掉了——这个状态
// **不会以任何形式报警**。自检正是去找它，返回的应当是偏差而不只是「能力齐不齐」。
func TestCheckFindsDrift(t *testing.T) {
	db, svc := newEnv(t)
	seedActivePolicy(t, db, "vnet0", model.PortSecurityActive)
	seedActivePolicy(t, db, "vnet1", model.PortSecurityActive)

	result, err := svc.Check(context.Background(), 1)
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	if result.Drifts == 0 {
		t.Fatal("mock 刻意让半数期望报偏差，自检应当检出它们")
	}
	// 每一项都要能指向具体对象——只报「有 3 处偏差」而说不出是哪几处，
	// 用户无法据此做任何事。
	for _, it := range result.Items {
		if it.Target == "" {
			t.Errorf("检查项必须指向具体对象: %+v", it)
		}
		if !it.OK && it.Actual == "" {
			t.Errorf("偏差必须说明实际状态: %+v", it)
		}
	}
}

// TestPendingNotCountedAsDrift 覆盖**不产生噪声**。
//
// pending 的策略本来就还没下发，把它们报成偏差会让自检结果里充满噪声——
// 而噪声会让人把这个功能关掉。
func TestPendingNotCountedAsDrift(t *testing.T) {
	db, svc := newEnv(t)
	seedActivePolicy(t, db, "vnet-pending", model.PortSecurityPending)

	result, err := svc.Check(context.Background(), 1)
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	for _, it := range result.Items {
		if it.Target == "vnet-pending" {
			t.Error("pending 的策略不该进入自检——它本来就还没下发")
		}
	}
}

// TestSeverityIsGraded 覆盖分级。
//
// 一律标红的结果是用户对红色麻木——而「整个地址校验被放开」与「少了一条
// 计数规则」不该得到同样的注意力。
func TestSeverityIsGraded(t *testing.T) {
	db, svc := newEnv(t)
	// 造一条必定报偏差的（mock 里第 2 项起报偏差）。
	seedActivePolicy(t, db, "vnet-a", model.PortSecurityActive)
	seedActivePolicy(t, db, "vnet-b", model.PortSecurityActive)

	result, err := svc.Check(context.Background(), 1)
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	sawCritical := false
	for _, it := range result.Items {
		if it.OK {
			if it.Severity != "info" {
				t.Errorf("通过的项应为 info，实际 %q", it.Severity)
			}
			continue
		}
		if it.Category == "端口安全" {
			// 端口安全失效会让**整台机器的地址校验放开**，那是安全边界。
			if it.Severity != "critical" {
				t.Errorf("端口安全的偏差应为 critical，实际 %q", it.Severity)
			}
			sawCritical = true
		}
	}
	if !sawCritical {
		t.Error("应至少检出一处端口安全偏差")
	}
}

// TestUnrepairableItemsAreMarked 覆盖「不可修复」要标出来。
//
// 缺 OVS 装不上——把它标成可修复，用户会点那个按钮、等一会、然后发现什么
// 都没变，而下一次自检还会看到同一条偏差。
func TestUnrepairableItemsAreMarked(t *testing.T) {
	_, svc := newEnv(t)

	result, err := svc.Check(context.Background(), 1)
	if err != nil {
		t.Fatalf("自检失败: %v", err)
	}
	var meterItem *agent.CheckItem
	for i := range result.Items {
		if strings.Contains(result.Items[i].Target, "meter") {
			meterItem = &result.Items[i]
			break
		}
	}
	if meterItem == nil {
		t.Fatal("mock 应报告 meter 缺失——它是「能力非必需但缺失」的代表")
	}
	if meterItem.OK {
		t.Error("mock 里 meter 是缺失的")
	}
	if meterItem.Repairable {
		t.Error("缺 meter 装不上，不该标为可修复——否则用户会点那个按钮然后发现什么都没变")
	}
	if meterItem.Fix == "" {
		t.Error("不可修复的也要给出要做什么")
	}
}

// TestOVSServiceDownIsSeparateFromAvailable 覆盖「装了但服务挂了」。
//
// 它是最容易被忽略的一种状态：探测说「可用」，而所有依赖它的功能都不生效。
func TestOVSServiceDownIsSeparateFromAvailable(t *testing.T) {
	_, svc := newEnv(t)

	st, err := svc.OVSStatus(context.Background(), 1)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !st.Available {
		t.Fatal("mock 里 OVS 是装了的")
	}
	// 两个字段必须分开：只报 Available 的话，「服务挂了」永远看不出来。
	if !st.ServiceActive {
		t.Error("前置条件：mock 里服务在跑")
	}
	if st.OpenFlow13 != true {
		t.Error("mock 里应支持 OpenFlow 1.3")
	}
}

// TestClientIPExplainsProxy 覆盖反向代理下的口径。
//
// 不加区分的话，用户会把代理地址填进防火墙白名单——而那放行的是所有经
// 代理过来的请求，等于对全网开放。
func TestClientIPExplainsProxy(t *testing.T) {
	plain := platformcheck.ClientIP("203.0.113.9", "")
	if plain["client_ip"] != "203.0.113.9" {
		t.Errorf("直接地址不对: %v", plain)
	}
	if _, has := plain["note"]; has {
		t.Error("没有反代时不该给出代理说明——那只会让人困惑")
	}

	proxied := platformcheck.ClientIP("10.0.0.1", "203.0.113.9")
	note, _ := proxied["note"].(string)
	if !strings.Contains(note, "203.0.113.9") || !strings.Contains(note, "10.0.0.1") {
		t.Errorf("说明里应同时给出两个地址: %q", note)
	}
	if !strings.Contains(note, "等于放行所有") {
		t.Errorf("必须说清填错的后果: %q", note)
	}
}

// TestRepairAuditNotesLimits 覆盖修复的能力边界要留痕。
//
// 修复不是「一键变好」：缺 OVS 装不上。审计里不写这一点的话，事后会被读成
// 「修复过就好」。
func TestRepairAuditNotesLimits(t *testing.T) {
	db, svc := newEnv(t)

	if _, err := svc.Repair(context.Background(), 1, nil,
		authz.Viewer{UserID: 9, IsAdmin: true}, "root", "10.0.0.1"); err != nil {
		t.Fatalf("修复失败: %v", err)
	}
	var rec model.AuditLog
	if err := db.Where("action = ?", "platform.repair").First(&rec).Error; err != nil {
		t.Fatalf("应记审计: %v", err)
	}
	if rec.Params == nil || !strings.Contains(*rec.Params, "不会被修复") {
		t.Errorf("审计应说明修复的能力边界: %v", rec.Params)
	}
}
