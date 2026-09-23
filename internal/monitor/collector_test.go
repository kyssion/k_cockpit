package monitor_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/monitor"
)

// deadAgent 让所有采集都失败。
type deadAgent struct{ agent.Client }

func (deadAgent) Execute(context.Context, agent.Operation) (*agent.Result, error) {
	return nil, errors.New("node unreachable")
}

func newTestEnv(t *testing.T, client agent.Client) (*gorm.DB, *monitor.Collector) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "monitor.db"),
		MaxOpenConns: 1,
		MaxIdleConns: 1,
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
		&model.HostStatsRecord{}, &model.VMStatsRecord{},
		&model.VMRuntimeDaily{}, &model.TrafficStatDaily{},
		&model.Node{}, &model.VM{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled, Enabled: true,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	opts := monitor.DefaultOptions()
	opts.Interval = time.Second
	return db, monitor.NewCollector(db, client, opts)
}

func seedVM(t *testing.T, db *gorm.DB, name, status string) *model.VM {
	t.Helper()
	owner := int64(7)
	vm := model.VM{NodeID: 1, Name: name, Status: status, OwnerID: &owner, Present: true}
	if err := db.Create(&vm).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return &vm
}

// TestTickCollectsHost 覆盖最基本的采集。
func TestTickCollectsHost(t *testing.T) {
	db, c := newTestEnv(t, agent.NewMockClient())
	seedVM(t, db, "vm-1", model.VMStatusRunning)

	c.Tick(context.Background())

	var hosts int64
	db.Model(&model.HostStatsRecord{}).Count(&hosts)
	if hosts != 1 {
		t.Errorf("宿主机记录 = %d, 期望 1", hosts)
	}

	var vms int64
	db.Model(&model.VMStatsRecord{}).Count(&vms)
	if vms != 1 {
		t.Errorf("虚拟机记录 = %d, 期望 1", vms)
	}
}

// TestFailureWritesNoRecord 覆盖本包最重要的一个决定。
//
// 采集失败时**不写记录**，而不是写一条全 0 的。写 0 会让图表显示「这台
// 机器 CPU 为 0%」——那看起来像"机器很闲"，而不是"没采到"。一张混了假 0
// 的曲线比一段空白更危险：前者不会引起任何怀疑。
func TestFailureWritesNoRecord(t *testing.T) {
	db, c := newTestEnv(t, deadAgent{})
	seedVM(t, db, "vm-1", model.VMStatusRunning)

	c.Tick(context.Background())

	var hosts, vms int64
	db.Model(&model.HostStatsRecord{}).Count(&hosts)
	db.Model(&model.VMStatsRecord{}).Count(&vms)
	if hosts != 0 || vms != 0 {
		t.Errorf("采集失败时不该写入任何记录: host=%d vm=%d", hosts, vms)
	}
}

// TestOnlyRunningVMsAreCollected 覆盖采集范围。
//
// 停机机器没有指标可读，而给它们写记录会让图表上出现一条贴着 0 的线——
// 那看起来像"这台机器很闲"，而不是"它没在跑"。
func TestOnlyRunningVMsAreCollected(t *testing.T) {
	db, c := newTestEnv(t, agent.NewMockClient())
	seedVM(t, db, "running", model.VMStatusRunning)
	seedVM(t, db, "stopped", model.VMStatusStopped)

	c.Tick(context.Background())

	var vms int64
	db.Model(&model.VMStatsRecord{}).Count(&vms)
	if vms != 1 {
		t.Errorf("虚拟机记录 = %d, 期望只采运行中的那 1 台", vms)
	}

	var rec model.VMStatsRecord
	db.First(&rec)
	var running model.VM
	db.Where("name = ?", "running").First(&running)
	if rec.VMID != running.ID {
		t.Error("采到的是停机的那台")
	}
}

// TestRuntimeAccumulatesAcrossTicks 覆盖按天累计。
//
// **累加而不是覆盖**：每一次采样代表"这台机器又跑了 interval 秒"，而配额
// 与超限处置要的正是这个总量。
func TestRuntimeAccumulatesAcrossTicks(t *testing.T) {
	db, c := newTestEnv(t, agent.NewMockClient())
	vm := seedVM(t, db, "vm-1", model.VMStatusRunning)

	for i := 0; i < 3; i++ {
		c.Tick(context.Background())
	}

	var row model.VMRuntimeDaily
	if err := db.Where("vm_id = ?", vm.ID).First(&row).Error; err != nil {
		t.Fatalf("没有累计记录: %v", err)
	}
	// 3 次 × 1 秒间隔（测试里把 Interval 设成 1 秒）。
	if row.Seconds != 3 {
		t.Errorf("累计秒数 = %d, 期望 3（累加而不是覆盖）", row.Seconds)
	}
	// 只有一行（按天分桶 + upsert）。
	var n int64
	db.Model(&model.VMRuntimeDaily{}).Where("vm_id = ?", vm.ID).Count(&n)
	if n != 1 {
		t.Errorf("记录了 %d 行, 期望 1 行（同一天应 upsert）", n)
	}
}

// TestTrafficAccumulates 覆盖流量累计。
func TestTrafficAccumulates(t *testing.T) {
	db, c := newTestEnv(t, agent.NewMockClient())
	vm := seedVM(t, db, "vm-1", model.VMStatusRunning)

	c.Tick(context.Background())

	var row model.TrafficStatDaily
	if err := db.Where("scope_type = ? AND scope_id = ?",
		model.TrafficScopeVM, vm.ID).First(&row).Error; err != nil {
		t.Fatalf("没有流量记录: %v", err)
	}
	if row.BytesIn <= 0 {
		t.Error("入向流量应大于 0")
	}

	// 再采一次应当累加而不是覆盖。
	before := row.BytesIn
	c.Tick(context.Background())
	db.Where("scope_type = ? AND scope_id = ?", model.TrafficScopeVM, vm.ID).First(&row)
	if row.BytesIn <= before {
		t.Errorf("流量未累加: %d → %d", before, row.BytesIn)
	}
}

// TestCleanupRemovesOnlyDetail 覆盖保留期策略。
//
// **只清明细，不动聚合表**：`vm_runtime_daily` 与 `traffic_stat_daily` 是
// 按天一行（一台机器一年才 365 行），它们才是长期要留的东西——配额按月算、
// 运行时长要能回溯。而明细只在排查最近几天时有用。
func TestCleanupRemovesOnlyDetail(t *testing.T) {
	db, c := newTestEnv(t, agent.NewMockClient())
	vm := seedVM(t, db, "vm-1", model.VMStatusRunning)

	c.Tick(context.Background())

	// 把明细的写入时间挪到保留期之外。
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	db.Model(&model.HostStatsRecord{}).Where("1 = 1").Update("at", old)
	db.Model(&model.VMStatsRecord{}).Where("1 = 1").Update("at", old)

	c.Cleanup(context.Background())

	var hosts, vms int64
	db.Model(&model.HostStatsRecord{}).Count(&hosts)
	db.Model(&model.VMStatsRecord{}).Count(&vms)
	if hosts != 0 || vms != 0 {
		t.Errorf("过期明细未清理: host=%d vm=%d", hosts, vms)
	}

	// 聚合表必须留着。
	var runtime, traffic int64
	db.Model(&model.VMRuntimeDaily{}).Where("vm_id = ?", vm.ID).Count(&runtime)
	db.Model(&model.TrafficStatDaily{}).Where("scope_id = ?", vm.ID).Count(&traffic)
	if runtime != 1 || traffic != 1 {
		t.Errorf("聚合表被误删: runtime=%d traffic=%d", runtime, traffic)
	}
}

// TestIntervalBytesConversion 覆盖单位换算。
//
// Kbps 是**千比特每秒**，换算成字节要先乘 1000、再除 8、再乘秒数。
// 三处里任何一处写错都会得到一个数量级错误的数字，而它看起来完全正常。
func TestIntervalBytesConversion(t *testing.T) {
	db, c := newTestEnv(t, agent.NewMockClient())
	seedVM(t, db, "vm-1", model.VMStatusRunning)

	c.Tick(context.Background())

	var rec model.VMStatsRecord
	db.First(&rec)

	// mock 给的 NetRxKbps 在几十 Kbps 量级，间隔 1 秒 → 应为几千字节。
	// 若误把 Kbps 当 KB/s，结果会差 8 倍；若漏乘 1000，会差 1000 倍。
	if rec.NetInBytes <= 0 {
		t.Fatalf("入向字节数 = %d", rec.NetInBytes)
	}
	if rec.NetInBytes > 1_000_000 {
		t.Errorf("入向字节数 = %d，疑似单位换算出错（1 秒内不该有这么多）", rec.NetInBytes)
	}
}

// TestDisabledNodeIsSkipped 覆盖采集范围。
func TestDisabledNodeIsSkipped(t *testing.T) {
	db, c := newTestEnv(t, agent.NewMockClient())
	seedVM(t, db, "vm-1", model.VMStatusRunning)

	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Update("enabled", false).Error; err != nil {
		t.Fatalf("禁用节点失败: %v", err)
	}
	c.Tick(context.Background())

	var hosts int64
	db.Model(&model.HostStatsRecord{}).Count(&hosts)
	if hosts != 0 {
		t.Error("被禁用的节点不该被采集")
	}
}
