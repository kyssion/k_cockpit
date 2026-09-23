package dashboard_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/dashboard"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/node"
)

// fakeRuntime 提供可控的心跳：mock 永远返回"刚刚心跳过"，而离线的判定
// 恰恰是本包要覆盖的东西——因此节点运行态必须由测试自己给。
type fakeRuntime struct {
	hb map[int64]time.Time
}

func (f fakeRuntime) Snapshot(_ context.Context, id int64) (*agent.Snapshot, error) {
	t, ok := f.hb[id]
	if !ok {
		return nil, nil
	}
	return &agent.Snapshot{Status: agent.StatusOnline, LastHeartbeat: t}, nil
}

func newEnv(t *testing.T, rt fakeRuntime) (*gorm.DB, *dashboard.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "dash.db"),
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
		&model.Node{}, &model.VM{}, &model.VMLock{}, &model.Task{},
		&model.HostStatsRecord{}, &model.StoragePool{}, &model.ResourceQuota{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	return db, dashboard.NewService(db, node.NewService(db, rt, nil, nil))
}

func admin() authz.Viewer { return authz.Viewer{UserID: 9, IsAdmin: true} }

func tenant(id int64) authz.Viewer { return authz.Viewer{UserID: id, IsAdmin: false} }

func seedNode(t *testing.T, db *gorm.DB, id int64, name, enroll string) {
	t.Helper()
	now := time.Now()
	if err := db.Create(&model.Node{
		ID: id, Name: name, EnrollState: enroll, Enabled: true,
		LastHeartbeatAt: &now,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
}

func seedVM(t *testing.T, db *gorm.DB, name, status string, owner int64, present bool) model.VM {
	t.Helper()
	vm := model.VM{
		NodeID: 1, Name: name, Status: status, OwnerID: &owner, Present: present,
		VCPU: 2, MemoryMB: 2048, DiskGB: 40,
	}
	if err := db.Create(&vm).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return vm
}

func seedTask(t *testing.T, db *gorm.DB, typ, status string, owner int64, finished *time.Time) {
	t.Helper()
	task := model.Task{Type: typ, Status: status, OwnerID: &owner, FinishedAt: finished}
	if err := db.Create(&task).Error; err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
}

// TestVMCountsAndAllocation 覆盖虚拟机的计数与「已承诺」资源。
//
// 已停止的机器**不计入**运行中的 vCPU 与内存，但**计入**磁盘：它占着存储
// 配额，而 CPU 与内存只有在跑起来的时候才真的被占住。
func TestVMCountsAndAllocation(t *testing.T) {
	db, svc := newEnv(t, fakeRuntime{hb: map[int64]time.Time{}})
	seedNode(t, db, 1, "node-1", model.NodeEnrollEnrolled)
	seedVM(t, db, "run-1", model.VMStatusRunning, 7, true)
	seedVM(t, db, "run-2", model.VMStatusRunning, 7, true)
	stopped := seedVM(t, db, "stop-1", model.VMStatusStopped, 7, true)
	seedVM(t, db, "paused-1", model.VMStatusPaused, 7, true)
	// present=false 必须走 Update 写：直接 Create 一个 false 会被 GORM 当作
	// 零值省略，数据库填入默认值 true（见 model.VM.Present 的说明）。
	gone := seedVM(t, db, "gone-1", model.VMStatusRunning, 7, true)
	if err := db.Model(&model.VM{}).Where("id = ?", gone.ID).
		Update("present", false).Error; err != nil {
		t.Fatalf("置失效失败: %v", err)
	}
	if err := db.Create(&model.VMLock{VMID: stopped.ID, Locked: true}).Error; err != nil {
		t.Fatalf("加锁失败: %v", err)
	}

	got, err := svc.Summary(context.Background(), admin())
	if err != nil {
		t.Fatalf("获取概览失败: %v", err)
	}
	if got.VMs.Total != 4 || got.VMs.Running != 2 || got.VMs.Stopped != 1 || got.VMs.Other != 1 {
		t.Errorf("虚拟机计数 = %+v, 期望 4/2/1/1", got.VMs)
	}
	if got.VMs.Locked != 1 {
		t.Errorf("锁定数 = %d, 期望 1", got.VMs.Locked)
	}
	// 失效的那台不进入任何计数，但要以 Missing 报出来。
	if got.VMs.Missing != 1 {
		t.Errorf("失效数 = %d, 期望 1", got.VMs.Missing)
	}
	if got.Allocation.VCPU != 8 || got.Allocation.MemoryMB != 8192 || got.Allocation.DiskGB != 160 {
		t.Errorf("已分配 = %+v, 期望 8 核 / 8192 MB / 160 GB", got.Allocation)
	}
	if got.Allocation.RunningVCPU != 4 || got.Allocation.RunningMemoryMB != 4096 {
		t.Errorf("运行中已分配 = %+v, 期望 4 核 / 4096 MB", got.Allocation)
	}
}

// TestTenantSeesOnlyOwnVMs 覆盖归属过滤。
//
// 这是首页上唯一一处「按人切数据」的地方，漏了就是一次越权：租户会看到
// 全平台有多少台机器、跑了多少核。
func TestTenantSeesOnlyOwnVMs(t *testing.T) {
	db, svc := newEnv(t, fakeRuntime{hb: map[int64]time.Time{}})
	seedVM(t, db, "mine", model.VMStatusRunning, 7, true)
	seedVM(t, db, "others", model.VMStatusRunning, 8, true)

	got, err := svc.Summary(context.Background(), tenant(7))
	if err != nil {
		t.Fatalf("获取概览失败: %v", err)
	}
	if got.Scope != dashboard.ScopeSelf {
		t.Errorf("scope = %q, 期望 self", got.Scope)
	}
	if got.VMs.Total != 1 || got.VMs.Running != 1 {
		t.Errorf("租户看到的虚拟机 = %+v, 期望只有自己的 1 台", got.VMs)
	}
	if len(got.RecentVMs) != 1 || got.RecentVMs[0].Name != "mine" {
		t.Errorf("最近虚拟机 = %+v, 期望只含自己的那台", got.RecentVMs)
	}
	// 节点与宿主资源是平台级信息，租户不该拿到。
	if got.Nodes != nil || got.Host != nil {
		t.Error("租户不该看到节点与宿主机资源")
	}
}

// TestHostUsageUsesLatestSample 覆盖采样口径：每个节点只取最新一条。
//
// 取「最近 N 分钟的全部」会让同一个节点在一个采样间隔内被重复计一次，
// 内存一栏立刻翻倍——而它看起来只是一台机器"用了很多内存"。
func TestHostUsageUsesLatestSample(t *testing.T) {
	db, svc := newEnv(t, fakeRuntime{hb: map[int64]time.Time{}})
	seedNode(t, db, 1, "node-1", model.NodeEnrollEnrolled)
	seedNode(t, db, 2, "node-2", model.NodeEnrollEnrolled)
	now := time.Now().UTC()

	samples := []model.HostStatsRecord{
		{NodeID: 1, At: now.Add(-2 * time.Minute), CPUPercent: 90, CPUCores: 8, MemUsedMB: 100, MemTotalMB: 1000},
		{NodeID: 1, At: now.Add(-time.Minute), CPUPercent: 10, CPUCores: 8, MemUsedMB: 200, MemTotalMB: 1000},
		{NodeID: 2, At: now.Add(-time.Minute), CPUPercent: 30, CPUCores: 8, MemUsedMB: 300, MemTotalMB: 1000},
	}
	for i := range samples {
		if err := db.Create(&samples[i]).Error; err != nil {
			t.Fatalf("写入采样失败: %v", err)
		}
	}
	if err := db.Create(&model.StoragePool{
		NodeID: 1, DeviceID: "disk-1", TotalBytes: 1000, UsableBytes: 400,
	}).Error; err != nil {
		t.Fatalf("创建存储池失败: %v", err)
	}

	got, err := svc.Summary(context.Background(), admin())
	if err != nil {
		t.Fatalf("获取概览失败: %v", err)
	}
	if got.Host == nil {
		t.Fatal("有采样时 host 不该为空")
	}
	if got.Host.SampledNodes != 2 {
		t.Errorf("采样节点数 = %d, 期望 2", got.Host.SampledNodes)
	}
	if got.Host.CPUCores != 16 || got.Host.MemTotalMB != 2000 || got.Host.MemUsedMB != 500 {
		t.Errorf("宿主汇总 = %+v, 期望 16 核 / 已用 500 / 共 2000 MB", got.Host)
	}
	// CPU 取各节点最新值的平均：(10 + 30) / 2 = 20。
	if got.Host.CPUPercent < 19.9 || got.Host.CPUPercent > 20.1 {
		t.Errorf("CPU = %.1f, 期望 20", got.Host.CPUPercent)
	}
	if got.Host.DiskTotalBytes != 1000 || got.Host.DiskUsedBytes != 600 {
		t.Errorf("存储 = %d/%d, 期望 1000/600", got.Host.DiskUsedBytes, got.Host.DiskTotalBytes)
	}
}

// TestHostNilWithoutSamples 覆盖「没有数据」的表达。
func TestHostNilWithoutSamples(t *testing.T) {
	_, svc := newEnv(t, fakeRuntime{hb: map[int64]time.Time{}})
	got, err := svc.Summary(context.Background(), admin())
	if err != nil {
		t.Fatalf("获取概览失败: %v", err)
	}
	if got.Host != nil {
		t.Errorf("无采样时 host 应为 nil, 实际 %+v", got.Host)
	}
}

// TestNodeStatusCounts 覆盖节点计数与告警。
//
// 状态由**心跳时间**推导（见 node.deriveStatus），因此测试给的是心跳时刻
// 而不是状态字符串——后者正是 mock 撒谎的地方（agent 宕机时它最后一次
// 上报仍写着"在线"）。
func TestNodeStatusCounts(t *testing.T) {
	db, svc := newEnv(t, fakeRuntime{hb: map[int64]time.Time{
		1: time.Now(),
		2: time.Now().Add(-2 * node.OfflineThreshold),
	}})
	seedNode(t, db, 1, "online", model.NodeEnrollEnrolled)
	seedNode(t, db, 2, "offline", model.NodeEnrollEnrolled)
	seedNode(t, db, 3, "pending", model.NodeEnrollPending)
	if err := db.Model(&model.Node{}).Where("id = ?", 3).
		Update("maintenance_mode", true).Error; err != nil {
		t.Fatalf("置维护模式失败: %v", err)
	}

	got, err := svc.Summary(context.Background(), admin())
	if err != nil {
		t.Fatalf("获取概览失败: %v", err)
	}
	if got.Nodes == nil {
		t.Fatal("管理员应当看到节点")
	}
	if got.Nodes.Total != 3 || got.Nodes.Online != 1 || got.Nodes.Offline != 1 || got.Nodes.Pending != 1 {
		t.Errorf("节点计数 = %+v, 期望 3/1/1/1", *got.Nodes)
	}
	if got.Nodes.Maintenance != 1 {
		t.Errorf("维护中 = %d, 期望 1", got.Nodes.Maintenance)
	}
	if !hasAlert(got.Alerts, "1 个节点离线") || !hasAlert(got.Alerts, "1 个节点等待接入") {
		t.Errorf("告警 = %+v, 期望含离线与待接入", got.Alerts)
	}
}

// TestTaskCounts 覆盖任务计数：unknown 算进行中，超过 24 小时的失败不算。
func TestTaskCounts(t *testing.T) {
	db, svc := newEnv(t, fakeRuntime{hb: map[int64]time.Time{}})
	seedNode(t, db, 1, "node-1", model.NodeEnrollEnrolled)
	recent := time.Now().UTC().Add(-time.Hour)
	old := time.Now().UTC().Add(-48 * time.Hour)
	seedTask(t, db, model.TaskVMCreate, model.TaskRunning, 7, nil)
	seedTask(t, db, model.TaskVMCreate, model.TaskUnknown, 7, nil)
	seedTask(t, db, model.TaskVMCreate, model.TaskFailed, 7, &recent)
	seedTask(t, db, model.TaskVMCreate, model.TaskFailed, 7, &old)

	got, err := svc.Summary(context.Background(), admin())
	if err != nil {
		t.Fatalf("获取概览失败: %v", err)
	}
	if got.Tasks.Active != 2 {
		t.Errorf("进行中 = %d, 期望 2（含 unknown）", got.Tasks.Active)
	}
	if got.Tasks.Failed24h != 1 {
		t.Errorf("近 24 小时失败 = %d, 期望 1", got.Tasks.Failed24h)
	}
	if !hasAlert(got.Alerts, "近 24 小时有 1 个任务失败") {
		t.Errorf("告警 = %+v, 期望含失败任务", got.Alerts)
	}
}

func hasAlert(alerts []dashboard.Alert, text string) bool {
	for _, a := range alerts {
		if a.Text == text {
			return true
		}
	}
	return false
}
