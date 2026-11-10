package passaudit_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/config"
	"k_cockpit/internal/platform/database"
	"k_cockpit/internal/service/platform/passaudit"
)

// fakeAgent 记录收到的操作并返回预设结果。
type fakeAgent struct {
	res *agent.Result
	err error
	ops []agent.Operation
}

func (f *fakeAgent) Execute(_ context.Context, op agent.Operation) (*agent.Result, error) {
	f.ops = append(f.ops, op)
	return f.res, f.err
}

func newDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "passaudit.db"),
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
		&model.User{}, &model.Node{}, &model.AuditLog{}, &model.SchedulerEvent{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db
}

func mkUser(t *testing.T, db *gorm.DB, username string, hit bool) model.User {
	t.Helper()
	u := model.User{
		Username: username, PasswordHash: "x", Role: model.RoleTenant,
		Status: model.UserStatusActive, BreachHit: hit,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("建用户 %s 失败: %v", username, err)
	}
	return u
}

func mkNode(t *testing.T, db *gorm.DB, name, state string) model.Node {
	t.Helper()
	n := model.Node{Name: name, EnrollState: state, Status: model.NodeStatusOnline}
	if err := db.Create(&n).Error; err != nil {
		t.Fatalf("建节点 %s 失败: %v", name, err)
	}
	return n
}

func agentHit(username string) agent.PasswordHit {
	return agent.PasswordHit{Username: username, Reason: "弱口令"}
}

func auditInfo(checked int, hits ...agent.PasswordHit) *agent.Result {
	return &agent.Result{Success: true, Data: map[string]any{
		agent.PasswordAuditDataKey: agent.PasswordAuditInfo{Checked: checked, Hits: hits},
	}}
}

// TestRunNowRefusedWhenDisabled：手动触发是一个会惊动全站的写操作，
// 开关关着时必须明确拒绝，而不是偷偷跑一遍。
func TestRunNowRefusedWhenDisabled(t *testing.T) {
	db := newDB(t)
	svc := passaudit.NewService(db, &fakeAgent{}, func() bool { return false }, nil)

	res, err := svc.RunNow(context.Background())
	if res != nil {
		t.Errorf("未开启时不应返回结果: %+v", res)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != 422 || apiErr.Code != api.CodeValidationFailed {
		t.Errorf("状态/错误码 = %d/%s, 期望 422/%s", apiErr.Status, apiErr.Code, api.CodeValidationFailed)
	}
	if !strings.Contains(apiErr.Message, "未开启") {
		t.Errorf("提示应说明开关未开: %q", apiErr.Message)
	}
}

// TestRunNowWithoutChannel：没有节点通道时不是"0 命中"，而是"没检查"。
// 把这两者混同，界面会在没检查过的账号上显示绿色的"安全"。
func TestRunNowWithoutChannel(t *testing.T) {
	t.Run("无 agent", func(t *testing.T) {
		svc := passaudit.NewService(newDB(t), nil, func() bool { return true }, nil)
		res, err := svc.RunNow(context.Background())
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if res.Unavailable == "" {
			t.Error("必须标记为未检查")
		}
	})

	t.Run("无已接入节点", func(t *testing.T) {
		db := newDB(t)
		mkNode(t, db, "pending-node", model.NodeEnrollPending)
		fa := &fakeAgent{res: auditInfo(3)}
		svc := passaudit.NewService(db, fa, func() bool { return true }, nil)

		res, err := svc.RunNow(context.Background())
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if !strings.Contains(res.Unavailable, "没有已接入的节点") {
			t.Errorf("应说明没有已接入节点: %q", res.Unavailable)
		}
		if len(fa.ops) != 0 {
			t.Error("没有已接入节点时不应向节点下指令")
		}
	})

	t.Run("节点不可达", func(t *testing.T) {
		db := newDB(t)
		mkNode(t, db, "n1", model.NodeEnrollEnrolled)
		svc := passaudit.NewService(db, &fakeAgent{err: errors.New("rpc timeout")},
			func() bool { return true }, nil)

		res, err := svc.RunNow(context.Background())
		if err != nil {
			t.Fatalf("节点不可达不应作为业务错误上报: %v", err)
		}
		if !strings.Contains(res.Unavailable, "不可达") {
			t.Errorf("应说明节点不可达: %q", res.Unavailable)
		}
	})

	t.Run("节点侧失败", func(t *testing.T) {
		db := newDB(t)
		mkNode(t, db, "n1", model.NodeEnrollEnrolled)
		svc := passaudit.NewService(db, &fakeAgent{
			res: &agent.Result{Success: false, Message: "未找到弱口令清单"},
		}, func() bool { return true }, nil)

		res, err := svc.RunNow(context.Background())
		if err != nil {
			t.Fatalf("不应报错: %v", err)
		}
		if !strings.Contains(res.Unavailable, "未找到弱口令清单") {
			t.Errorf("应转达节点侧原因: %q", res.Unavailable)
		}
	})
}

// TestRunNowMarksHitsAndClearsStale 是本包最重要的一条：状态表必须与本次
// 结果**一致**——上次命中、这次没命中的账号要被清掉，否则界面会永远警告
// 一个已经改过密码的人。
func TestRunNowMarksHitsAndClearsStale(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	mkNode(t, db, "n1", model.NodeEnrollEnrolled)

	alice := mkUser(t, db, "alice", false)
	bob := mkUser(t, db, "bob", true) // 上次命中
	carol := mkUser(t, db, "carol", false)

	fa := &fakeAgent{res: auditInfo(3,
		agent.PasswordHit{Username: "alice", Reason: "已泄露"},
		agent.PasswordHit{Username: "ghost", Reason: "弱口令"}, // 节点上有、面板里没有
	)}
	rec := audit.NewRecorder(db)
	svc := passaudit.NewService(db, fa, func() bool { return true }, rec)

	var notified []int64
	svc.SetNotifier(func(_ context.Context, userID int64, subject, body string) {
		notified = append(notified, userID)
		if !strings.Contains(subject, "口令") {
			t.Errorf("通知主题应说明口令安全: %q", subject)
		}
		if strings.Contains(body, "http") {
			t.Errorf("通知正文不应带链接: %q", body)
		}
	})

	res, err := svc.RunNow(ctx)
	if err != nil {
		t.Fatalf("检查失败: %v", err)
	}
	if res.Unavailable != "" {
		t.Fatalf("不应标记为未检查: %q", res.Unavailable)
	}
	if res.Checked != 3 {
		t.Errorf("Checked = %d, 期望 3", res.Checked)
	}
	if len(res.Hits) != 1 || res.Hits[0].Username != "alice" {
		t.Fatalf("命中列表 = %+v, 期望只有 alice", res.Hits)
	}
	if res.Hits[0].UserID != alice.ID {
		t.Errorf("命中里的 UserID = %d, 期望 %d", res.Hits[0].UserID, alice.ID)
	}
	if res.Hits[0].Reason != "已泄露" {
		t.Errorf("命中原因未透传: %q", res.Hits[0].Reason)
	}

	var gotAlice, gotBob, gotCarol model.User
	db.First(&gotAlice, alice.ID)
	db.First(&gotBob, bob.ID)
	db.First(&gotCarol, carol.ID)
	if !gotAlice.BreachHit {
		t.Error("alice 应被标记为命中")
	}
	if gotAlice.BreachCheckedAt == nil {
		t.Error("alice 应记录检查时间")
	}
	if gotBob.BreachHit {
		t.Error("bob 上次命中、这次不在结果里，必须被清除")
	}
	if gotBob.BreachCheckedAt == nil {
		t.Error("清除命中时也要更新检查时间")
	}
	if gotCarol.BreachHit || gotCarol.BreachCheckedAt != nil {
		t.Error("从未命中的账号不应被触碰")
	}

	if len(notified) != 1 || notified[0] != alice.ID {
		t.Errorf("只应通知命中者本人, 实际 %v", notified)
	}

	var entry model.AuditLog
	if err := db.Where("action = ?", "user.password_audit").First(&entry).Error; err != nil {
		t.Fatalf("未写审计: %v", err)
	}
	if entry.Params == nil || !strings.Contains(*entry.Params, "\"hits\":1") {
		t.Errorf("审计参数应记录命中数, 实际 %v", entry.Params)
	}

	// 指令必须打到那个已接入节点，且 Kind 正确。
	if len(fa.ops) != 1 {
		t.Fatalf("应只下一条指令, 实际 %d", len(fa.ops))
	}
	if fa.ops[0].Kind != agent.OpSecurityPasswordAudit {
		t.Errorf("操作类型 = %s", fa.ops[0].Kind)
	}
	if fa.ops[0].NodeID == 0 {
		t.Error("NodeID 不能为 0：真实通道下 0 永远不在线")
	}
}

// TestRunNowToleratesUnknownUsernames：节点返回一个面板里不存在的用户名
// （已删账号、系统账号）时跳过即可，不能让整次检查失败。
func TestRunNowToleratesUnknownUsernames(t *testing.T) {
	db := newDB(t)
	mkNode(t, db, "n1", model.NodeEnrollEnrolled)
	mkUser(t, db, "alice", false)
	svc := passaudit.NewService(db, &fakeAgent{res: auditInfo(9,
		agent.PasswordHit{Username: "root", Reason: "弱口令"},
		agent.PasswordHit{Username: "alice", Reason: "弱口令"},
	)}, func() bool { return true }, nil)

	res, err := svc.RunNow(context.Background())
	if err != nil {
		t.Fatalf("未知用户名不应让检查失败: %v", err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Username != "alice" {
		t.Errorf("命中列表 = %+v, 期望仅 alice", res.Hits)
	}
}

// TestRunNowToleratesMalformedPayload：节点返回的结构对不上时按"零结果"
// 处理并继续。它不能 panic——一次格式变更不该把控制面打挂。
func TestRunNowToleratesMalformedPayload(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
	}{
		{"缺少结果键", map[string]any{"other": 1}},
		{"结果不是对象", map[string]any{agent.PasswordAuditDataKey: "oops"}},
		{"空数据", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := newDB(t)
			mkNode(t, db, "n1", model.NodeEnrollEnrolled)
			stale := mkUser(t, db, "stale", true)
			svc := passaudit.NewService(db, &fakeAgent{
				res: &agent.Result{Success: true, Data: c.data},
			}, func() bool { return true }, nil)

			res, err := svc.RunNow(context.Background())
			if err != nil {
				t.Fatalf("解析失败不应报错: %v", err)
			}
			if res.Checked != 0 || len(res.Hits) != 0 {
				t.Errorf("结果 = %+v, 期望零值", res)
			}
			// 解析不出结果等同于"本轮没有任何人命中"，旧标记应当被清掉。
			var got model.User
			db.First(&got, stale.ID)
			if got.BreachHit {
				t.Error("无可解析结果时应清除旧的命中标记")
			}
		})
	}
}

func TestStatusCountsHits(t *testing.T) {
	db := newDB(t)
	svc := passaudit.NewService(db, nil, nil, nil)
	ctx := context.Background()

	if n, err := svc.Status(ctx); err != nil || n != 0 {
		t.Fatalf("空库 Status = %d, %v", n, err)
	}
	mkUser(t, db, "a", true)
	mkUser(t, db, "b", true)
	mkUser(t, db, "c", false)
	n, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status 失败: %v", err)
	}
	if n != 2 {
		t.Errorf("命中账号数 = %d, 期望 2", n)
	}
}

// TestStatusReportsDatabaseFailure：库不可用时必须报 500 而不是返回 0，
// 否则界面会显示"全部安全"。
func TestStatusReportsDatabaseFailure(t *testing.T) {
	db := newDB(t)
	svc := passaudit.NewService(db, nil, nil, nil)
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
	if _, err := svc.Status(context.Background()); err == nil {
		t.Fatal("库不可用时 Status 必须报错")
	}
}
