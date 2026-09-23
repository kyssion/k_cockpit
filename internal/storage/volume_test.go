package storage_test

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
	"k_cockpit/internal/storage"
	"k_cockpit/internal/task"
)

type volumeEnv struct {
	svc    *storage.Service
	db     *gorm.DB
	queue  *task.Queue
	client agent.Client
}

func newVolumeEnv(t *testing.T) *volumeEnv {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "vol.db"),
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
		&model.StoragePool{}, &model.StorageVolume{}, &model.UserStorage{},
		&model.Node{}, &model.VM{}, &model.AuditLog{}, &model.Task{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	recorder := audit.NewRecorder(db)
	q := task.NewQueue(db, recorder, task.Options{})
	client := agent.NewMockClient()
	// 执行器在这里注册**只是为了让它能被直接调用**（见 TestDeleteRemovesRecord）：
	// 队列在测试里不启动后台循环，因此不注册也不会影响入队。
	q.Register(storage.NewVolumeExecutor(db, client))
	return &volumeEnv{
		svc: storage.NewService(db, q, recorder, client, nil), db: db, queue: q, client: client,
	}
}

func adminViewer() authz.Viewer { return authz.Viewer{UserID: 9999, IsAdmin: true} }

func devices(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "/dev/sd"+string(rune('b'+i)))
	}
	return out
}

// TestStripeRequiresMultiplication 覆盖那个**直觉容易出错的乘法**。
//
// 用户会想「镜像要 2 块、条带要 2 块，那我给 2 块盘就行了吧」。实际上每一
// 份镜像自己也要分散到多条带上，因此需要 stripe × mirror 块。
func TestStripeRequiresMultiplication(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	req := storage.VolumeRequest{
		NodeID: 1, Name: "v1", SizeGB: 100,
		StripeCount: 2, MirrorCount: 2,
		Devices: devices(3), // 需要 4 块，只给 3 块
	}
	_, err := env.svc.PreviewVolume(ctx, req)
	if err == nil {
		t.Fatal("3 块盘不该满足 2×2 的聚合")
	}
	if !strings.Contains(err.Error(), "4") {
		t.Errorf("报错应给出需要的块数: %v", err)
	}

	// 给够 4 块就能过。
	req.Devices = devices(4)
	plan, err := env.svc.PreviewVolume(ctx, req)
	if err != nil {
		t.Fatalf("4 块盘应满足: %v", err)
	}
	if plan.RequiredDevices != 4 {
		t.Errorf("需要 %d 块, 期望 4", plan.RequiredDevices)
	}
}

// TestStripeWithoutMirrorWarns 覆盖本块最重要的认知点。
//
// **条带 ≠ 冗余。** 看到「用了 4 块盘」很自然会以为那是 4 块盘的冗余，
// 而条带任何一块盘故障都会让整个卷不可用，且因为数据是分散的，剩下那些
// 盘上的内容也无法单独恢复。
func TestStripeWithoutMirrorWarns(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	plan, err := env.svc.PreviewVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "striped", SizeGB: 100, StripeCount: 4, Devices: devices(4),
	})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if plan.HasRedundancy {
		t.Error("纯条带不该被判为有冗余")
	}
	if len(plan.Warnings) == 0 {
		t.Fatal("纯条带必须给出「没有冗余」的提示")
	}
	if !strings.Contains(strings.Join(plan.Warnings, " "), "没有冗余") {
		t.Errorf("提示应直说没有冗余: %v", plan.Warnings)
	}

	// 未确认时**不创建**，也不报错。
	created, tk, err := env.svc.CreateVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "striped", SizeGB: 100, StripeCount: 4, Devices: devices(4),
	}, false, adminViewer(), "root", "")
	if err != nil {
		t.Fatalf("未确认时应返回计划而不是报错——那是一个岔路口: %v", err)
	}
	if tk != nil || created == nil {
		t.Fatal("未确认时不该创建")
	}
	var n int64
	env.db.Model(&model.StorageVolume{}).Count(&n)
	if n != 0 {
		t.Error("未确认时库里不该有记录")
	}

	// 确认后创建。
	_, tk, err = env.svc.CreateVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "striped", SizeGB: 100, StripeCount: 4, Devices: devices(4),
	}, true, adminViewer(), "root", "")
	if err != nil {
		t.Fatalf("确认后创建失败: %v", err)
	}
	if tk == nil {
		t.Error("确认后应产生任务")
	}
}

// TestMirrorDoublesPhysicalSpace 覆盖容量与物理占用的区别。
//
// 只给用户看可用容量，他会以为「还能再建一个这么大的卷」，而物理盘早就满了。
func TestMirrorDoublesPhysicalSpace(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	plan, err := env.svc.PreviewVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "mirrored", SizeGB: 100, MirrorCount: 2, Devices: devices(2),
	})
	if err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	if plan.SizeGB != 100 {
		t.Errorf("可用容量 = %d, 期望 100", plan.SizeGB)
	}
	if plan.PhysicalGB != 200 {
		t.Errorf("物理占用 = %d, 期望 200（镜像 2 份）", plan.PhysicalGB)
	}
	if !plan.HasRedundancy {
		t.Error("镜像应被判为有冗余——它是唯一能容忍坏盘的配置")
	}
	// 镜像的代价与同步时间都要提示出来。
	joined := strings.Join(plan.Warnings, " ")
	if !strings.Contains(joined, "同步") {
		t.Errorf("应提示镜像需要初始同步: %v", plan.Warnings)
	}
}

// TestCreateMirroredVolumeStartsInSync 覆盖状态由节点决定而不是一律 active。
//
// 镜像卷建好之后要同步几小时，期间写明显变慢。把它标成 active，用户会认为
// 卷是好的、慢是别的原因，于是一路排查到网络。
func TestCreateMirroredVolumeStartsInSync(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	if _, _, err := env.svc.CreateVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "mir", SizeGB: 50, MirrorCount: 2, Devices: devices(2),
	}, true, adminViewer(), "root", ""); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	var row model.StorageVolume
	env.db.Where("name = ?", "mir").First(&row)
	if row.Status != model.VolumeSync {
		t.Errorf("镜像卷初始状态 = %q, 期望 sync", row.Status)
	}
}

func TestPlainVolumeIsActive(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	if _, _, err := env.svc.CreateVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "plain", SizeGB: 50, Devices: devices(1),
	}, true, adminViewer(), "root", ""); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	var row model.StorageVolume
	env.db.Where("name = ?", "plain").First(&row)
	// 单盘无镜像无条带：没有同步过程，直接 active。
	if row.Status != model.VolumeActive {
		t.Errorf("状态 = %q, 期望 active", row.Status)
	}
}

func TestDuplicateDeviceRejected(t *testing.T) {
	env := newVolumeEnv(t)

	// 同一块盘出现两次：LVM 会拒绝，但报错来自 lvm 命令、与「你选重了」
	// 联系不起来。而且它看起来像"我选了 4 块盘"，实际只有 3 块。
	_, _, err := env.svc.CreateVolume(context.Background(), storage.VolumeRequest{
		NodeID: 1, Name: "dup", SizeGB: 10,
		Devices: []string{"/dev/sdb", "/dev/sdb"},
	}, true, adminViewer(), "root", "")
	assertStatus(t, err, 400)
}

func TestNonDevPathRejected(t *testing.T) {
	env := newVolumeEnv(t)

	_, _, err := env.svc.CreateVolume(context.Background(), storage.VolumeRequest{
		NodeID: 1, Name: "bad", SizeGB: 10, Devices: []string{"/tmp/foo"},
	}, true, adminViewer(), "root", "")
	assertStatus(t, err, 400)
}

// TestDeviceOccupiedByPoolRejected 覆盖设备独占。
//
// 一个块设备同时属于两处会让两边都写出错乱的数据，而这种损坏**不会立刻
// 报错**——它要等到文件系统层面的元数据互相覆盖时才暴露。
func TestDeviceOccupiedByPoolRejected(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	dev := "/dev/sdb"
	path := "/mnt/pool1"
	if err := env.db.Create(&model.StoragePool{
		NodeID: 1, DeviceID: "sdb", DevicePath: &dev, Kind: "local",
		MountPath: &path, Status: "ready",
	}).Error; err != nil {
		t.Fatalf("创建存储池失败: %v", err)
	}

	_, _, err := env.svc.CreateVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "conflict", SizeGB: 10, Devices: []string{dev},
	}, true, adminViewer(), "root", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "已被") {
		t.Errorf("报错应指出被谁占用: %v", err)
	}
}

// TestVolumeNamesAreLVMCompatible 覆盖名称映射。
//
// LVM 的名称有字符集限制，而用户取的名字可能带空格、中文或斜杠。直接用会
// 导致建不出来，而报错来自 lvm 命令、与名字里的那个空格毫无关系。
func TestVolumeNamesAreLVMCompatible(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	// 中文 + 空格 + 大写。
	if _, _, err := env.svc.CreateVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "我的 数据卷", SizeGB: 10, Devices: devices(1),
	}, true, adminViewer(), "root", ""); err != nil {
		t.Fatalf("中文名称不该导致创建失败: %v", err)
	}

	var row model.StorageVolume
	env.db.Where("name = ?", "我的 数据卷").First(&row)
	if row.Name != "我的 数据卷" {
		t.Error("展示用的名称应保留原样")
	}
	// 测试在 storage_test 包里，用不到未导出的 derefStr——就地展开。
	vg, lv := "", ""
	if row.VGName != nil {
		vg = *row.VGName
	}
	if row.LVName != nil {
		lv = *row.LVName
	}
	if vg == "" || lv == "" {
		t.Fatal("应生成 LVM 名称")
	}
	for _, name := range []string{vg, lv} {
		for _, r := range name {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				t.Errorf("LVM 名称含非法字符 %q: %s", r, name)
			}
		}
	}
}

func TestMirrorCountCapped(t *testing.T) {
	env := newVolumeEnv(t)

	// 上限不是技术限制而是理性限制：4 份以上很少有意义，而每多一份就多占
	// 一份空间——用户多半是误填。
	_, _, err := env.svc.CreateVolume(context.Background(), storage.VolumeRequest{
		NodeID: 1, Name: "big", SizeGB: 10, MirrorCount: 8, Devices: devices(8),
	}, true, adminViewer(), "root", "")
	assertStatus(t, err, 400)
}

func TestZeroSizeRejected(t *testing.T) {
	env := newVolumeEnv(t)

	_, _, err := env.svc.CreateVolume(context.Background(), storage.VolumeRequest{
		NodeID: 1, Name: "zero", SizeGB: 0, Devices: devices(1),
	}, true, adminViewer(), "root", "")
	assertStatus(t, err, 400)
}

// TestPreviewChangesNothing 覆盖预检只读。
func TestPreviewChangesNothing(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	if _, err := env.svc.PreviewVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "preview-only", SizeGB: 10, Devices: devices(1),
	}); err != nil {
		t.Fatalf("预检失败: %v", err)
	}
	var n int64
	env.db.Model(&model.StorageVolume{}).Count(&n)
	if n != 0 {
		t.Error("预检不该产生记录")
	}
	var tasks int64
	env.db.Model(&model.Task{}).Count(&tasks)
	if tasks != 0 {
		t.Error("预检不该入队")
	}
}

func TestDuplicateNameRejected(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		_, _, err := env.svc.CreateVolume(ctx, storage.VolumeRequest{
			NodeID: 1, Name: "same", SizeGB: 10, Devices: devices(1),
		}, true, adminViewer(), "root", "")
		if i == 0 && err != nil {
			t.Fatalf("首次创建失败: %v", err)
		}
		if i == 1 {
			assertStatus(t, err, 409)
		}
	}
}

// TestDeleteRemovesRecord 覆盖删除的语义。
//
// 表里没有 deleted_at，而且删除是用户的明确意图——卷与其中的数据都已经
// 销毁了。**保留记录会让那些设备一直显示为被占用**，用户再也建不了新卷。
func TestDeleteRemovesRecord(t *testing.T) {
	env := newVolumeEnv(t)
	ctx := context.Background()

	if _, _, err := env.svc.CreateVolume(ctx, storage.VolumeRequest{
		NodeID: 1, Name: "temp", SizeGB: 10, Devices: devices(1),
	}, true, adminViewer(), "root", ""); err != nil {
		t.Fatalf("创建失败: %v", err)
	}
	var row model.StorageVolume
	env.db.Where("name = ?", "temp").First(&row)

	if _, err := env.svc.DeleteVolume(ctx, row.ID, adminViewer(), "root", ""); err != nil {
		t.Fatalf("删除失败: %v", err)
	}

	// **直接驱动执行器**：测试里的队列没有后台循环，而为了让测试能跑就
	// 给生产代码加一个 RunOnce，是把测试的便利变成产品的接口。
	// 执行器本身是公开类型，直接构造并调用即可。
	// 取**最新**那条：创建任务与删除任务的 resource_id 相同，First 会拿到
	// 创建那条，而拿它去跑执行器只会把卷又"创建"一次。
	var tk model.Task
	if err := env.db.Where("resource_id = ?", row.ID).
		Order("id DESC").First(&tk).Error; err != nil {
		t.Fatalf("没有找到删除任务: %v", err)
	}
	if err := storage.NewVolumeExecutor(env.db, env.client).Run(ctx, &tk); err != nil {
		t.Fatalf("执行删除任务失败: %v", err)
	}

	var n int64
	env.db.Model(&model.StorageVolume{}).Where("id = ?", row.ID).Count(&n)
	if n != 0 {
		t.Error("删除后记录应被清掉——保留它会让设备一直显示为被占用")
	}
}
