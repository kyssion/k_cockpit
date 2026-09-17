package vm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// TestOnlinePasswordRequiresRunning 覆盖在线改密的前置条件。
func TestOnlinePasswordRequiresRunning(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-g1", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)), GuestAgent: true,
	}
	db.Create(&row)

	_, _, err := svc.GuestAction(ctx, row.ID, vm.GuestRequest{
		Action: "password_online", Username: "root", Password: "newpass123",
	}, authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 422)
}

// TestOnlinePasswordRequiresGuestAgent 覆盖在线类动作对 Guest Agent 的依赖。
//
// 这句拒绝必须说清「怎么办」（去来宾里装并启动它），而不只是「不行」——
// 用户看到「未检测到 Guest Agent」的下一个问题一定是「那我要做什么」。
func TestOnlinePasswordRequiresGuestAgent(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-g2", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), GuestAgent: false,
	}
	db.Create(&row)

	_, _, err := svc.GuestAction(ctx, row.ID, vm.GuestRequest{
		Action: "password_online", Username: "root", Password: "newpass123",
	}, authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 422)
}

// TestOfflinePasswordRequiresStoppedAndNoAgent 覆盖离线路径的特殊性。
//
// 离线改密**不用** Guest Agent（那正是它存在的意义：来宾起不来的时候用），
// 但必须关机——挂载一块正在被写入的磁盘会同时损坏两侧看到的内容。
func TestOfflinePasswordRequiresStoppedAndNoAgent(t *testing.T) {
	ctx := context.Background()

	// 运行中 → 拒绝（即使装了 agent）。
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	row := model.VM{
		NodeID: 1, Name: "vm-g3", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), GuestAgent: true,
	}
	db.Create(&row)
	_, _, err := svc.GuestAction(ctx, row.ID, vm.GuestRequest{
		Action: "password_offline", Username: "root", Password: "newpass123",
	}, authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 422)

	// 关机且**没有** agent → 接受。
	svc2, _, db2 := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusStopped})
	row2 := model.VM{
		NodeID: 1, Name: "vm-g4", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)), GuestAgent: false,
	}
	db2.Create(&row2)
	if _, _, err := svc2.GuestAction(ctx, row2.ID, vm.GuestRequest{
		Action: "password_offline", Username: "root", Password: "newpass123",
	}, authz.Viewer{UserID: 7}, "alice", ""); err != nil {
		t.Errorf("离线改密不应依赖 Guest Agent: %v", err)
	}
}

// TestGuestActionScrubsPassword 覆盖 R-009：口令不落库。
//
// 任务参数是**持久化**的，不清的话密码会一直留在 task.params 里——而那个表
// 是运维随时会翻的。清除必须放在失败路径上也要走到的地方：恰恰是失败时，
// 用户最可能把参数贴出来求助。
func TestGuestActionScrubsPassword(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{
		NodeID: 1, Name: "vm-g5", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), GuestAgent: true,
	}
	db.Create(&row)

	const secret = "s3cret-pass-123"
	tk, _, err := svc.GuestAction(ctx, row.ID, vm.GuestRequest{
		Action: "password_online", Username: "root", Password: secret,
	}, viewer, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	// 入队时参数里**必须**有密码：否则执行器拿不到它。
	var queued model.Task
	db.First(&queued, tk.ID)
	if queued.Params == nil || !strings.Contains(*queued.Params, secret) {
		t.Fatal("受理时参数里没有密码——执行器将无法改密")
	}

	// 等任务跑完。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var now model.Task
		db.First(&now, tk.ID)
		if now.Status == model.TaskSuccess || now.Status == model.TaskFailed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	var after model.Task
	db.First(&after, tk.ID)
	if after.Params != nil && strings.Contains(*after.Params, secret) {
		t.Errorf("执行后密码仍留在任务参数里——它是持久化的，会被运维随手翻到")
	}
	// 注意不能直接搜 "password"：动作名 password_online 里也有这个子串。
	// 要查的是**字段**是否还在。
	if after.Params != nil && strings.Contains(*after.Params, `"password":`) {
		t.Errorf("参数里仍留有 password 字段: %s", *after.Params)
	}
}

// TestGuestActionAuditHasNoPassword 覆盖审计流水同样不含密码。
func TestGuestActionAuditHasNoPassword(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-g6", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), GuestAgent: true,
	}
	db.Create(&row)

	const secret = "another-secret-456"
	if _, _, err := svc.GuestAction(ctx, row.ID, vm.GuestRequest{
		Action: "password_online", Username: "deploy", Password: secret,
	}, authz.Viewer{UserID: 7}, "alice", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	var logs []model.AuditLog
	db.Where("action LIKE ?", "vm.guest.%").Find(&logs)
	if len(logs) == 0 {
		t.Fatal("未记录审计")
	}
	for _, l := range logs {
		if l.Params != nil && strings.Contains(*l.Params, secret) {
			t.Errorf("审计记录里出现了密码: %s", *l.Params)
		}
	}
}

// TestGuestActionRejectsInjection 覆盖输入校验。
//
// 用户名与密码会被拼进 shell 命令交给来宾执行，含空白或换行的输入会让命令
// 被拆开——那是注入，不只是格式问题。
func TestGuestActionRejectsInjection(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-g7", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), GuestAgent: true,
	}
	db.Create(&row)

	cases := []struct{ name, user, pass string }{
		{"用户名含空格", "root rm -rf /", "validpass123"},
		{"用户名含冒号", "root:x", "validpass123"},
		{"密码含换行", "root", "pass\nword1234"},
		{"密码过短", "root", "short"},
		{"用户名为空", "  ", "validpass123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.GuestAction(ctx, row.ID, vm.GuestRequest{
				Action: "password_online", Username: tc.user, Password: tc.pass,
			}, authz.Viewer{UserID: 7}, "alice", "")
			if err == nil {
				t.Fatal("应被拒绝")
			}
		})
	}
}

func TestGuestActionRejectsUnknownAction(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	row := model.VM{
		NodeID: 1, Name: "vm-g8", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), GuestAgent: true,
	}
	db.Create(&row)

	_, _, err := svc.GuestAction(ctx, row.ID, vm.GuestRequest{Action: "format_everything"},
		authz.Viewer{UserID: 7}, "alice", "")
	assertAPIError(t, err, 400)
}

func TestGuestActionRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()

	theirs := model.VM{
		NodeID: 1, Name: "vm-g9", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(20)), GuestAgent: true,
	}
	db.Create(&theirs)

	// 404 而非 403：403 会确认「这个 ID 存在」。
	_, _, err := svc.GuestAction(ctx, theirs.ID, vm.GuestRequest{
		Action: "password_online", Username: "root", Password: "newpass123",
	}, authz.Viewer{UserID: 10}, "bob", "")
	assertAPIError(t, err, 404)
}

// TestGuestCapabilitiesReflectState 覆盖可用动作按当前状态计算。
//
// 让界面据此禁用按钮，而不是让用户点下去才知道不行。
func TestGuestCapabilitiesReflectState(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{agent.NewMockClient(), model.VMStatusRunning})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{
		NodeID: 1, Name: "vm-ga", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), GuestAgent: true,
	}
	db.Create(&row)

	view, err := svc.GuestCapabilities(ctx, row.ID, viewer)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !view.GuestAgentReady {
		t.Error("未反映 Guest Agent 就绪")
	}
	// 运行中：在线改密与附加磁盘可用，离线改密不可用。
	has := func(a string) bool {
		for _, x := range view.AvailableActions {
			if x == a {
				return true
			}
		}
		return false
	}
	if !has("password_online") {
		t.Error("运行中应可用在线改密")
	}
	if has("password_offline") {
		t.Error("运行中不应提供离线改密——它要求关机")
	}
}
