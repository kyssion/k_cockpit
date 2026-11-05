package vm_test

import (
	"context"
	"errors"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/platform/cryptoutil"
	"k_cockpit/internal/service/compute/vm"
)

// consoleProbe 记录控制面下发的控制台操作，并按预设返回结果。
type consoleProbe struct {
	ops []agent.Operation
	res *agent.Result
	err error
}

func (p *consoleProbe) Execute(_ context.Context, op agent.Operation) (*agent.Result, error) {
	p.ops = append(p.ops, op)
	return p.res, p.err
}

func newConsoleEnv(t *testing.T, probe *consoleProbe) (*vm.Service, *gorm.DB, *model.VM) {
	t.Helper()
	svc, _, db := newTestEnvWithClient(t, probe)
	svc.SetEncryptionKey(cryptoutil.DeriveKey([]byte("console-test-key"), "v1"))

	row := &model.VM{
		NodeID: 1, Name: "vm-console", Status: model.VMStatusRunning,
		OwnerID: ptr(int64(7)), DisplayDevice: model.DisplayVNC,
		VNCBind: "127.0.0.1",
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("准备虚拟机失败: %v", err)
	}
	return svc, db, row
}

// TestUpdateConsoleDispatchesToNode 是本批修复的核心：
// 开启控制台必须真的下发到节点，而不是只改数据库。
func TestUpdateConsoleDispatchesToNode(t *testing.T) {
	probe := &consoleProbe{res: &agent.Result{
		Success: true,
		Data: map[string]any{
			agent.ConsoleUpdatePortKey:            float64(5903),
			agent.ConsoleUpdateRestartRequiredKey: true,
		},
	}}
	svc, db, row := newConsoleEnv(t, probe)

	enabled := true
	cfg, err := svc.UpdateConsole(context.Background(), row.ID,
		vm.ConsoleUpdate{Enabled: &enabled}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("开启控制台失败: %v", err)
	}

	if len(probe.ops) != 1 {
		t.Fatalf("应下发 1 次，实际 %d 次", len(probe.ops))
	}
	op := probe.ops[0]
	if op.Kind != agent.OpVMConsoleUpdate {
		t.Errorf("操作类型 = %q, 期望 %q", op.Kind, agent.OpVMConsoleUpdate)
	}
	if op.Target != row.Name {
		t.Errorf("目标 = %q, 期望 %q", op.Target, row.Name)
	}
	if got, _ := op.Params["enabled"].(bool); !got {
		t.Errorf("enabled 未下发: %#v", op.Params)
	}
	if got, _ := op.Params["listen"].(string); got != "127.0.0.1" {
		t.Errorf("默认监听地址 = %q, 期望 127.0.0.1", got)
	}

	// 节点回传的实际端口要落库（否则界面显示 0，用户不知道连哪儿）。
	got := &model.VM{}
	if err := db.First(got, row.ID).Error; err != nil {
		t.Fatalf("读取虚拟机失败: %v", err)
	}
	if !got.VNCEnabled {
		t.Error("VNCEnabled 未落库")
	}
	if got.VNCPort == nil || *got.VNCPort != 5903 {
		t.Errorf("端口未按节点结果落库: %v", got.VNCPort)
	}
	if !cfg.Enabled {
		t.Error("返回的配置未反映开启状态")
	}
}

// TestUpdateConsoleExposeSetsListen 覆盖「对外暴露」：监听地址必须变成 0.0.0.0。
func TestUpdateConsoleExposeSetsListen(t *testing.T) {
	probe := &consoleProbe{res: &agent.Result{Success: true}}
	svc, db, row := newConsoleEnv(t, probe)

	enabled, exposed := true, true
	ctx := vm.WithExposureVerified(context.Background())
	if _, err := svc.UpdateConsole(ctx, row.ID,
		vm.ConsoleUpdate{Enabled: &enabled, Exposed: &exposed},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("暴露失败: %v", err)
	}

	if len(probe.ops) != 1 {
		t.Fatalf("应下发 1 次，实际 %d 次", len(probe.ops))
	}
	if got, _ := probe.ops[0].Params["listen"].(string); got != "0.0.0.0" {
		t.Errorf("监听地址 = %q, 期望 0.0.0.0", got)
	}

	got := &model.VM{}
	if err := db.First(got, row.ID).Error; err != nil {
		t.Fatalf("读取虚拟机失败: %v", err)
	}
	if !got.VNCExposed || got.VNCBind != "0.0.0.0" {
		t.Errorf("暴露状态未落库: exposed=%v bind=%q", got.VNCExposed, got.VNCBind)
	}
}

// TestUpdateConsoleNodeRejectsKeepsRecord 覆盖节点拒绝：控制面不能留下
// 一个「看起来已开启」的配置。
func TestUpdateConsoleNodeRejectsKeepsRecord(t *testing.T) {
	probe := &consoleProbe{res: &agent.Result{Success: false, Message: "libvirt 拒绝了控制台配置"}}
	svc, db, row := newConsoleEnv(t, probe)

	enabled := true
	_, err := svc.UpdateConsole(context.Background(), row.ID,
		vm.ConsoleUpdate{Enabled: &enabled}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)

	got := &model.VM{}
	if err := db.First(got, row.ID).Error; err != nil {
		t.Fatalf("读取虚拟机失败: %v", err)
	}
	if got.VNCEnabled {
		t.Error("节点拒绝后不应把 VNCEnabled 写成 true")
	}
}

// TestUpdateConsoleNodeUnreachable 覆盖「未送达」：与「执行失败」区分文案。
func TestUpdateConsoleNodeUnreachable(t *testing.T) {
	probe := &consoleProbe{err: errors.New("connection lost")}
	svc, _, row := newConsoleEnv(t, probe)

	enabled := true
	_, err := svc.UpdateConsole(context.Background(), row.ID,
		vm.ConsoleUpdate{Enabled: &enabled}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 503)
}

// TestUpdateConsoleForwardsPassword 确保改密真的走到节点。
func TestUpdateConsoleForwardsPassword(t *testing.T) {
	probe := &consoleProbe{res: &agent.Result{Success: true}}
	svc, db, row := newConsoleEnv(t, probe)
	// 先把控制台置为已开启，避免改密请求同时触发一次"开启"。
	if err := db.Model(&model.VM{}).Where("id = ?", row.ID).
		Update("vnc_enabled", true).Error; err != nil {
		t.Fatalf("预置状态失败: %v", err)
	}

	password := "s3cret"
	if _, err := svc.UpdateConsole(context.Background(), row.ID,
		vm.ConsoleUpdate{Password: &password}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("改密失败: %v", err)
	}

	if len(probe.ops) != 1 {
		t.Fatalf("应下发 1 次，实际 %d 次", len(probe.ops))
	}
	if got, _ := probe.ops[0].Params["password"].(string); got != password {
		t.Errorf("密码未下发: %#v", probe.ops[0].Params)
	}
}

// TestUpdateConsoleRejectsExposeWhenDisabled 防止「没开控制台却暴露端口」。
func TestUpdateConsoleRejectsExposeWhenDisabled(t *testing.T) {
	probe := &consoleProbe{res: &agent.Result{Success: true}}
	svc, _, row := newConsoleEnv(t, probe)

	exposed := true
	ctx := vm.WithExposureVerified(context.Background())
	_, err := svc.UpdateConsole(ctx, row.ID,
		vm.ConsoleUpdate{Exposed: &exposed}, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
	if len(probe.ops) != 0 {
		t.Error("被拒绝的请求不应下发到节点")
	}
}

// TestUpdateConsoleRejectsSPICEWithoutSupport 覆盖能力边界：节点没上报
// SPICE 时直接拒绝，而不是写一份指向不存在端口的配置。
func TestUpdateConsoleRejectsSPICEWithoutSupport(t *testing.T) {
	probe := &consoleProbe{res: &agent.Result{Success: true}}
	svc, _, row := newConsoleEnv(t, probe)

	enabled := true
	_, err := svc.UpdateConsole(context.Background(), row.ID,
		vm.ConsoleUpdate{Protocol: vm.ConsoleProtocolSPICE, Enabled: &enabled},
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
	if len(probe.ops) != 0 {
		t.Error("SPICE 不支持时不应下发")
	}
}
