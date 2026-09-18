package passthrough_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/passthrough"
	"k_cockpit/internal/task"
)

func newEnv(t *testing.T) (*gorm.DB, *passthrough.Service) {
	t.Helper()
	db, err := database.Open(config.DB{
		Driver: config.DriverSQLite,
		Path:   filepath.Join(t.TempDir(), "pt.db"),
	}, false)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := db.AutoMigrate(
		&model.VMPassthrough{}, &model.VM{}, &model.Node{},
		&model.AuditLog{}, &model.Task{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	db.Create(&model.Node{ID: 1, Name: "node-1"})
	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	q := task.NewQueue(db, recorder, task.Options{})
	q.Register(passthrough.NewExecutor(db, client))
	return db, passthrough.NewService(db, q, client, recorder)
}

func admin() authz.Viewer { return authz.Viewer{UserID: 9, IsAdmin: true} }

func seedVM(t *testing.T, db *gorm.DB, id int64, name, status string) {
	t.Helper()
	if err := db.Create(&model.VM{
		ID: id, Name: name, NodeID: 1, Status: status,
	}).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
}

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被拒绝（%d），实际成功", want)
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != want {
		t.Errorf("状态码 = %d, 期望 %d（%s）", apiErr.Status, want, apiErr.Message)
	}
}

// TestOverviewCarriesGroupPeers 覆盖同组分组的呈现。
//
// IOMMU 分组里的设备只能**一起**直通，而它取决于主板拓扑与 BIOS 设置——
// **只有节点探测得到**，控制面无从推断。用户选了显卡之后发现鼠标键盘也
// 一起被拿走了，正是"同组"的表现。
func TestOverviewCarriesGroupPeers(t *testing.T) {
	_, svc := newEnv(t)

	devices, iommu, err := svc.Overview(context.Background(), 1)
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if iommu == nil || !iommu.Enabled {
		t.Fatal("mock 应报告 IOMMU 已启用")
	}

	var gpu *int
	for i := range devices {
		if devices[i].Address == "0000:01:00.0" {
			gpu = &i
			break
		}
	}
	if gpu == nil {
		t.Fatal("mock 应有显卡")
	}
	// 显卡与它的音频功能在同一个分组里——用户必须能看到这一点。
	if len(devices[*gpu].GroupPeers) == 0 {
		t.Fatal("同组设备必须列出来：用户要在选择的那一刻就看到「还有谁会一起被拿走」")
	}
	if !strings.Contains(strings.Join(devices[*gpu].GroupPeers, " "), "01:00.1") {
		t.Errorf("同组成员不对: %v", devices[*gpu].GroupPeers)
	}
}

// TestUnbindRefusesWhenInUseByVM 覆盖最重要的一条守卫。
//
// 解绑一块正被虚拟机使用的卡，等于在那台机器运行中把它的"显卡"拔掉——而
// 用户点这个按钮时想的多半是"我不需要了"，不是"我要弄坏一台机器"。
func TestUnbindRefusesWhenInUseByVM(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedVM(t, db, 1, "vm-1", model.VMStatusStopped)

	if _, err := svc.Attach(ctx, 1, "0000:01:00.0", "",
		admin(), "root", ""); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}

	err := svc.Unbind(ctx, 1, "0000:01:00.0", admin(), "root", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "虚拟机使用") {
		t.Errorf("报错应说明设备正被使用: %v", err)
	}
}

// TestGroupConflictAcrossVMs 覆盖同组只能属于一台机器。
//
// 不检查的话，用户把同组的两块卡分别挂给两台机器，而 libvirt 只会在第二台
// 启动时失败——那时第一台已经跑起来了，两边都看不出问题出在"分组"上。
func TestGroupConflictAcrossVMs(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedVM(t, db, 1, "vm-1", model.VMStatusStopped)
	seedVM(t, db, 2, "vm-2", model.VMStatusStopped)

	// 显卡与音频同属分组 1。
	if _, err := svc.Attach(ctx, 1, "0000:01:00.0", "",
		admin(), "root", ""); err != nil {
		t.Fatalf("首次挂载失败: %v", err)
	}
	_, err := svc.Attach(ctx, 2, "0000:01:00.1", "", admin(), "root", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "分组") {
		t.Errorf("报错应指出是分组冲突: %v", err)
	}
}

// TestAttachRequiresStopped 覆盖关机要求。
//
// 直通设备不支持热插拔（有限的支持需要内核与固件的配合），而失败方式是一台
// 机器卡在半启动状态。用户以为能热插而实际不能时，坏掉的是他那台正在跑的机器。
func TestAttachRequiresStopped(t *testing.T) {
	db, svc := newEnv(t)
	seedVM(t, db, 1, "vm-1", model.VMStatusRunning)

	_, err := svc.Attach(context.Background(), 1, "0000:01:00.0", "",
		admin(), "root", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "关机") {
		t.Errorf("报错应说明需要先关机: %v", err)
	}
}

// TestNonPassthroughDeviceRejected 覆盖不可直通的设备。
//
// 例如与宿主机管理网络共用的网卡——直通它会让面板失去连接。
func TestNonPassthroughDeviceRejected(t *testing.T) {
	db, svc := newEnv(t)
	seedVM(t, db, 1, "vm-1", model.VMStatusStopped)

	_, err := svc.Attach(context.Background(), 1, "0000:00:1f.6", "",
		admin(), "root", "")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "不可直通") {
		t.Errorf("应说明为什么不可直通: %v", err)
	}
}

func TestAttachUnknownDeviceRejected(t *testing.T) {
	db, svc := newEnv(t)
	seedVM(t, db, 1, "vm-1", model.VMStatusStopped)

	_, err := svc.Attach(context.Background(), 1, "0000:ff:ff.0", "",
		admin(), "root", "")
	assertStatus(t, err, 404)
}

// TestDetachRequiresStopped 覆盖卸载同样要求关机。
func TestDetachRequiresStopped(t *testing.T) {
	db, svc := newEnv(t)
	ctx := context.Background()
	seedVM(t, db, 1, "vm-1", model.VMStatusStopped)

	if _, err := svc.Attach(ctx, 1, "0000:01:00.0", "",
		admin(), "root", ""); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	db.Model(&model.VM{}).Where("id = ?", 1).Update("status", model.VMStatusRunning)

	err := svc.Detach(ctx, 1, "0000:01:00.0", admin(), "root", "")
	assertStatus(t, err, 409)
}

// TestAuditRecordsGroup 覆盖审计里记分组。
//
// 事后排查"为什么这台机器起不来"时，"当时挂在哪个分组"是唯一能解释它的东西。
func TestAuditRecordsGroup(t *testing.T) {
	db, svc := newEnv(t)
	seedVM(t, db, 1, "vm-1", model.VMStatusStopped)

	if _, err := svc.Attach(context.Background(), 1, "0000:01:00.0", "",
		admin(), "root", ""); err != nil {
		t.Fatalf("挂载失败: %v", err)
	}
	var rec model.AuditLog
	if err := db.Where("action = ?", "vm.passthrough.attach").First(&rec).Error; err != nil {
		t.Fatalf("应记审计: %v", err)
	}
	if rec.Params == nil || !strings.Contains(*rec.Params, "iommu_group") {
		t.Errorf("审计应含分组信息: %v", rec.Params)
	}
}

func TestDetachNotAttachedRejected(t *testing.T) {
	_, svc := newEnv(t)

	err := svc.Detach(context.Background(), 1, "0000:01:00.0", admin(), "root", "")
	assertStatus(t, err, 404)
}
