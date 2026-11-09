package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/compute/vm"
	"k_cockpit/internal/service/platform/settings"
)

// rescueISOSettings 是救援用例的最小设置源。
//
// 只有「救援 ISO」一项有值，其余一律返回调用方给的默认值——与 settings 为
// nil 时的行为完全一致，因此这些用例不会被别的设置项意外影响。
type rescueISOSettings struct{ iso string }

func (s *rescueISOSettings) Int(_ string, fallback int) int { return fallback }

func (s *rescueISOSettings) Bool(_ string, fallback bool) bool { return fallback }

func (s *rescueISOSettings) String(key, fallback string) string {
	if key == settings.KeyVMRescueISO {
		return s.iso
	}
	return fallback
}

// newRescueEnv 构造一个带「救援 ISO」设置的环境（iso 为空即未配置）。
func newRescueEnv(
	t *testing.T, client agent.Client, iso string,
) (*vm.Service, *gorm.DB, *rescueISOSettings) {
	t.Helper()
	provider := &rescueISOSettings{iso: iso}
	svc, _, db := newTestEnvWithSettings(t, client, provider)
	return svc, db, provider
}

// waitForVM 轮询直到详情满足条件。
//
// 救援的执行器是异步的（走队列），受理成功后记录不会立刻变成目标状态——
// 直接断言会读到「还没开始」的中间态，而那个状态下的字段同样有可能满足
// 一个写错的断言，测试就变成了碰运气。
func waitForVM(
	t *testing.T, svc *vm.Service, id int64, viewer authz.Viewer, cond func(vm.View) bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err := svc.Get(context.Background(), id, viewer)
		if err == nil && cond(*v) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待虚拟机 %d 达到目标状态超时", id)
}

func TestEnterRescueRequiresStopped(t *testing.T) {
	// 探测返回 running：救援要改硬件配置，热改会让控制面记录的配置与
	// 虚拟化层实际的配置分叉——而那份分叉在退出还原时会变成一个
	// 「还原成什么样」说不清的状态。
	//
	// 救援 ISO 在这里**必须配置**：未配置时会被配置校验先拒掉（见
	// TestEnterRescueRejectsUnconfiguredISO），那样这个用例就测不到
	// 它本来的意思（状态判定）了。
	svc, db, _ := newRescueEnv(t, &probeClient{newStubClient(), model.VMStatusRunning}, rescueISO)
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-r1", Status: model.VMStatusRunning, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.EnterRescue(ctx, row.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)

	var apiErr *api.Error
	if errors.As(err, &apiErr) && !strings.Contains(apiErr.Message, "关机") {
		t.Errorf("拒绝原因未说明需要关机: %q", apiErr.Message)
	}
}

// rescueISO 是「已配置救援镜像」这一前提的取值。
//
// 相对路径而不是绝对路径：设置项只接受 ISO 存放目录内的相对路径，
// 用绝对路径会让用例卡在路径校验上，而不是它要验证的那条规则。
const rescueISO = "rescue/systemrescue.iso"

// TestEnterRescueRejectsUnconfiguredISO 覆盖「成功但没做事」的那条路径。
//
// 未配置救援 ISO 时，节点侧的 vmRescueEnter 只保存一份快照就返回成功——
// 任务成功、rescue_active 置 true，而宿主机上的域定义与电源状态一个都没变。
// 那比失败更糟：用户会按救援环境的预期去操作一台其实还从系统盘引导的机器。
// 因此控制面必须**在受理时**就明确拒绝，且不留下任何任务与状态。
func TestEnterRescueRejectsUnconfiguredISO(t *testing.T) {
	svc, db, provider := newRescueEnv(
		t, &probeClient{newStubClient(), model.VMStatusStopped}, "")
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{NodeID: 1, Name: "vm-no-iso", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.EnterRescue(ctx, row.ID, viewer, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)

	// 文案必须**可执行**：只说「失败」会让人去怀疑节点状态，而真正要改的是
	// 系统设置里的救援镜像。
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if !strings.Contains(apiErr.Message, "救援镜像") {
			t.Errorf("拒绝原因未说明缺少救援镜像: %q", apiErr.Message)
		}
		if !strings.Contains(apiErr.Message, "系统设置") {
			t.Errorf("拒绝原因未说明去哪里配置: %q", apiErr.Message)
		}
	}

	// 不入队、不改状态：拒绝必须发生在任何副作用之前。
	var count int64
	db.Model(&model.Task{}).Where("type = ?", model.TaskVMRescueEnter).Count(&count)
	if count != 0 {
		t.Errorf("未配置救援镜像时仍入队了 %d 个救援任务", count)
	}
	var stored model.VM
	if err := db.First(&stored, row.ID).Error; err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if stored.RescueActive {
		t.Error("被拒绝的进入请求把记录置成了救援中")
	}
	if stored.RescueConfig != nil {
		t.Error("被拒绝的进入请求写入了配置快照")
	}

	// 配上镜像后同一台机器应能正常受理——只测拒绝的用例拦不住一个
	// 「永远拒绝」的实现。
	provider.iso = rescueISO
	if _, err := svc.EnterRescue(ctx, row.ID, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("配置救援镜像后应受理: %v", err)
	}
}

// TestRescueRoundTripRestoresConfig 覆盖 F-2-12 的核心承诺：**退出还原**。
//
// 进入前把配置存下来，退出时按它写回。没有这一步就只能猜一个默认值填回去，
// 那是悄悄改掉用户的配置——他进入救援是为了修系统，退出后发现引导顺序被
// 重置成了默认值，机器起不来了。
func TestRescueRoundTripRestoresConfig(t *testing.T) {
	// 救援 ISO 必填（未配置时进入救援会被直接拒绝，见
	// TestEnterRescueRejectsUnconfiguredISO）。
	svc, db, _ := newRescueEnv(t, &probeClient{newStubClient(), model.VMStatusStopped}, rescueISO)
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	original := model.VM{
		NodeID: 1, Name: "vm-rescue", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)),
		// 一组**刻意非默认**的配置：用默认值做样本的话，即使还原逻辑
		// 写的是「填默认值」也会通过，测试就失去意义了。
		BootOrder:     "cdrom,disk",
		MachineType:   "i440fx",
		DisplayDevice: "vnc",
		Firmware:      "bios",
		SecureBoot:    false,
	}
	db.Create(&original)

	// --- 进入救援 ---
	if _, err := svc.EnterRescue(ctx, original.ID, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("进入救援失败: %v", err)
	}
	waitForVM(t, svc, original.ID, viewer, func(v vm.View) bool { return v.RescueActive })

	entered, err := svc.Get(ctx, original.ID, viewer)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if !entered.RescueActive {
		t.Fatal("未标记为救援模式")
	}
	if entered.RescueSince == nil {
		t.Error("未记录进入救援的时间")
	}

	var stored model.VM
	if err := db.First(&stored, original.ID).Error; err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if stored.RescueConfig == nil || *stored.RescueConfig == "" {
		t.Fatal("未保存配置快照——退出时将无法还原")
	}

	// --- 退出救援 ---
	if _, err := svc.ExitRescue(ctx, original.ID, viewer, "alice", "10.0.0.1"); err != nil {
		t.Fatalf("退出救援失败: %v", err)
	}
	waitForVM(t, svc, original.ID, viewer, func(v vm.View) bool { return !v.RescueActive })

	var restored model.VM
	if err := db.First(&restored, original.ID).Error; err != nil {
		t.Fatalf("读取记录失败: %v", err)
	}
	if restored.BootOrder != original.BootOrder {
		t.Errorf("引导顺序未还原: %q → %q", original.BootOrder, restored.BootOrder)
	}
	if restored.MachineType != original.MachineType {
		t.Errorf("机型未还原: %q → %q", original.MachineType, restored.MachineType)
	}
	if restored.Firmware != original.Firmware {
		t.Errorf("固件未还原: %q → %q", original.Firmware, restored.Firmware)
	}
	// 快照要一起清掉：留着一份不再对应的快照，下次进入救援时若那次进入
	// 失败，旧的会被当成「本次的原配置」去还原，还原到一个更久远的状态。
	if restored.RescueConfig != nil {
		t.Error("退出后快照未清除")
	}
	if restored.RescueSince != nil {
		t.Error("退出后救援时刻未清除")
	}
}

func TestEnterRescueRejectsWhenAlreadyActive(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{newStubClient(), model.VMStatusStopped})
	ctx := context.Background()
	viewer := authz.Viewer{UserID: 7}

	row := model.VM{
		NodeID: 1, Name: "vm-r2", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)), RescueActive: true,
	}
	db.Create(&row)

	_, err := svc.EnterRescue(ctx, row.ID, viewer, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestExitRescueRejectsWhenNotInRescue(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{newStubClient(), model.VMStatusStopped})
	ctx := context.Background()

	row := model.VM{NodeID: 1, Name: "vm-r3", Status: model.VMStatusStopped, OwnerID: ptr(int64(7))}
	db.Create(&row)

	_, err := svc.ExitRescue(ctx, row.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

// TestExitRescueWithoutSnapshotIsRejected 覆盖一条刻意的选择。
//
// 快照缺失时**拒绝退出**，而不是用默认值「还原」：后者会静默改掉用户的
// 引导顺序与固件设置，而他无从判断这是不是自己原来的配置。相比起来，
// 「需要人工确认」是一个明确得多、也安全得多的结果。
func TestExitRescueWithoutSnapshotIsRejected(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{newStubClient(), model.VMStatusStopped})
	ctx := context.Background()

	// 状态是「救援中」但没有快照——模拟历史数据或写入中断。
	row := model.VM{
		NodeID: 1, Name: "vm-r4", Status: model.VMStatusStopped,
		OwnerID: ptr(int64(7)), RescueActive: true,
	}
	db.Create(&row)

	_, err := svc.ExitRescue(ctx, row.ID, authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestRescueRejectsOthersVM(t *testing.T) {
	svc, _, db := newTestEnvWithClient(t, &probeClient{newStubClient(), model.VMStatusStopped})
	ctx := context.Background()

	theirs := model.VM{
		NodeID: 1, Name: "vm-r5", Status: model.VMStatusStopped, OwnerID: ptr(int64(20)),
	}
	db.Create(&theirs)

	// 404 而非 403：403 会确认「这个 ID 存在」。
	_, err := svc.EnterRescue(ctx, theirs.ID, authz.Viewer{UserID: 10}, "bob", "10.0.0.2")
	assertAPIError(t, err, 404)
}
