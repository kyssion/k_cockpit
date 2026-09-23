package storage_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/storage"
	"k_cockpit/internal/task"
)

const (
	systemDisk = "/dev/sda"
	freeDisk   = "/dev/sdb"
	dataDisk   = "/dev/sdc"
)

// diskClient 用可控的磁盘清单替换 mock 的设备探测结果。
//
// 需要它的理由与 vm 包的 probeClient 相同：mock 返回固定清单，而本包要
// 验证的恰恰是「不同状态的盘被区别对待」——系统盘、已挂载、已有数据。
type diskClient struct {
	*agent.MockClient
	disks []agent.Disk
	// volumes 是 scan 返回的池内磁盘，用于覆盖删除前的占用检查。
	volumes []string
	// failCreate 让创建返回失败，用于覆盖 agent 侧校验拒绝的分支。
	failCreate bool
}

func (c *diskClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	switch op.Kind {
	case agent.OpNodeDisks:
		return &agent.Result{Success: true, Data: map[string]any{agent.DiskListKey: c.disks}}, nil
	case agent.OpStoragePoolScan:
		if len(c.volumes) == 0 {
			return &agent.Result{Success: true, Data: map[string]any{}}, nil
		}
		return &agent.Result{
			Success: true,
			Data:    map[string]any{agent.VolumeListKey: c.volumes},
		}, nil
	case agent.OpStoragePoolCreate:
		if c.failCreate {
			// agent 侧的校验失败：控制面看到的信息可能滞后，最终防线在这里。
			return &agent.Result{Success: false, Message: "设备已被 LVM 占用"}, nil
		}
		return &agent.Result{
			Success: true,
			Data: map[string]any{
				"mount_path":        "/var/lib/k_cockpit/pools" + op.Target,
				agent.StatusDataKey: "ready",
			},
		}, nil
	}
	return c.MockClient.Execute(ctx, op)
}

func defaultDisks() []agent.Disk {
	return []agent.Disk{
		{DeviceID: "sys", Path: systemDisk, SizeBytes: 64 << 30, IsSystem: true, Mounted: true},
		{DeviceID: "free", Path: freeDisk, SizeBytes: 512 << 30},
		{DeviceID: "data", Path: dataDisk, SizeBytes: 1024 << 30, HasData: true, Filesystem: "ext4"},
	}
}

func newTestEnv(t *testing.T) (*storage.Service, *task.Queue, *gorm.DB, *diskClient) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "storage.db"),
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
		&model.StoragePool{}, &model.Task{}, &model.TaskStage{}, &model.AuditLog{}, &model.Node{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{ID: 1, Name: "node-1"}).Error; err != nil {
		t.Fatalf("创建测试节点失败: %v", err)
	}

	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 2,
		PollInterval:  20 * time.Millisecond,
	})
	client := &diskClient{MockClient: agent.NewMockClient(), disks: defaultDisks()}
	queue.Register(storage.NewCreateExecutor(db, client))
	queue.Register(storage.NewDeleteExecutor(db, client))

	ctx, cancel := context.WithCancel(context.Background())
	queue.Start(ctx)
	t.Cleanup(func() {
		cancel()
		queue.Stop()
	})

	// settings 传 nil：本包不依赖设置模块，阈值走内置默认值。
	return storage.NewService(db, queue, recorder, client, nil), queue, db, client
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时: %s", what)
}

func assertAPIError(t *testing.T, err error, status int) {
	t.Helper()
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("期望业务错误, 实际 %v", err)
	}
	if apiErr.Status != status {
		t.Fatalf("状态码 = %d, 期望 %d (%s)", apiErr.Status, status, apiErr.Message)
	}
}

// createReq 构造一个「所有确认都已做对」的请求，各测试只改要验证的字段。
func createReq(deviceID, confirmName string) storage.CreateRequest {
	return storage.CreateRequest{
		NodeID:            1,
		DeviceID:          deviceID,
		FSType:            "ext4",
		ConfirmDeviceName: confirmName,
	}
}

// --- 创建设备名确认（R-004）---

func TestCreateRequiresMatchingDeviceName(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)
	ctx := context.Background()

	// 设备名确认的作用是让用户在动手前看清自己选中的是哪块盘。
	// 只要点一下「我确认」起不到这个作用，所以这里逐字符比较。
	for _, confirm := range []string{"", "/dev/sd", "/DEV/SDB", "sdb", "/dev/sdb1"} {
		_, err := svc.Create(ctx, createReq("free", confirm), 7, "admin", "10.0.0.1")
		assertAPIError(t, err, 422)
	}

	// 首尾空格被容忍：用户从终端复制设备路径时常带上空格，为此报错
	// 属于吹毛求疵，而它并不会让用户误解自己确认的是哪块盘。
	req := createReq("free", "  "+freeDisk+"  ")
	if _, err := svc.Create(ctx, req, 7, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("首尾空格应被容忍: %v", err)
	}
}

func TestCreateRejectsSystemDisk(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)

	// 系统盘即便设备名确认正确也必须拒绝：它是唯一「无论用户怎么确认
	// 都不能动」的类别（承载 / 或 /boot）。
	_, err := svc.Create(context.Background(), createReq("sys", systemDisk), 7, "admin", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestCreateRequiresDataLossConfirmation(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)
	ctx := context.Background()

	// 已有文件系统的设备默认拒绝（§4.1）。
	_, err := svc.Create(ctx, createReq("data", dataDisk), 7, "admin", "10.0.0.1")
	assertAPIError(t, err, 422)

	var apiErr *api.Error
	errors.As(err, &apiErr)
	if !strings.Contains(apiErr.Message, "数据") {
		t.Errorf("拒绝原因未说明数据风险: %q", apiErr.Message)
	}

	// 显式确认后放行：直接禁止会让一块曾被格式化过的盘永远无法使用。
	req := createReq("data", dataDisk)
	req.ConfirmDataLoss = true
	if _, err := svc.Create(ctx, req, 7, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("确认后应放行: %v", err)
	}
}

func TestCreateRejectsUnknownDevice(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)

	// 设备不在节点当前上报的清单中：凭控制面的旧记录去格式化一块可能
	// 已被拔掉的盘，结果只会是一个含义不明的失败。
	_, err := svc.Create(context.Background(), createReq("ghost", "/dev/sdz"), 7, "admin", "10.0.0.1")
	assertAPIError(t, err, 422)
}

func TestCreateRejectsUnsupportedFilesystem(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)

	req := createReq("free", freeDisk)
	req.FSType = "ntfs"
	_, err := svc.Create(context.Background(), req, 7, "admin", "10.0.0.1")
	assertAPIError(t, err, 400)
}

// --- 创建链路 ---

func TestCreateFlowWritesPoolAndDefaultsToFirst(t *testing.T) {
	svc, _, db, _ := newTestEnv(t)
	ctx := context.Background()

	tk, err := svc.Create(ctx, createReq("free", freeDisk), 7, "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	if tk.Type != model.TaskStoragePoolCreate {
		t.Errorf("任务类型 = %q", tk.Type)
	}
	// 存储操作**全局串行**（R-003）：所有存储池变更任务共用同一个资源锁键，
	// 这样同一时刻只有一个在跑。
	if rt, _ := tk.TaskResource(); rt != "storage_global" {
		t.Errorf("资源锁键 = %q, 期望 storage_global", rt)
	}

	waitFor(t, "存储池记录建立", func() bool {
		var count int64
		db.Model(&model.StoragePool{}).Where("device_id = ?", "free").Count(&count)
		return count == 1
	})

	var pool model.StoragePool
	db.Where("device_id = ?", "free").First(&pool)
	// 第一个池自动成为默认（R-006）：否则用户创建完还要再点一次「设为默认」，
	// 而绝大多数节点只有一个池。
	if !pool.IsDefault {
		t.Error("第一个存储池应自动设为默认")
	}
	if pool.Status != model.StoragePoolReady {
		t.Errorf("状态 = %q, 期望 ready", pool.Status)
	}
	if pool.MountPath == nil || *pool.MountPath == "" {
		t.Error("未记录挂载点")
	}
}

func TestSecondPoolIsNotDefault(t *testing.T) {
	svc, _, _, _ := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, createReq("free", freeDisk), 7, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	req := createReq("data", dataDisk)
	req.ConfirmDataLoss = true
	if _, err := svc.Create(ctx, req, 7, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	// 必须先等两个任务都落地再断言：创建是异步的，直接查会读到「一个池都
	// 还没有」的状态，而那个状态下的默认池数量同样是 0——用例会失败在一个
	// 与本例要验证的规则毫无关系的地方。
	waitFor(t, "两个存储池记录建立", func() bool {
		pools, err := svc.List(ctx, 1)
		return err == nil && len(pools) == 2
	})

	// 每节点至多一个默认池（R-006）。第二个池不应抢走默认标记。
	pools, err := svc.List(ctx, 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	defaults := 0
	for _, p := range pools {
		if p.IsDefault {
			defaults++
		}
	}
	if defaults != 1 {
		t.Errorf("默认池数量 = %d, 期望 1", defaults)
	}
}

func TestCreateRejectsOccupiedDevice(t *testing.T) {
	svc, _, db, _ := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, createReq("free", freeDisk), 7, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}
	waitFor(t, "存储池记录建立", func() bool {
		var count int64
		db.Model(&model.StoragePool{}).Where("device_id = ?", "free").Count(&count)
		return count == 1
	})

	// 同一设备不能建两个池。数据库有唯一索引兜底，但先查一次才能给出
	// 可读的原因（否则用户看到的是一句约束冲突）。
	_, err := svc.Create(ctx, createReq("free", freeDisk), 7, "admin", "10.0.0.1")
	assertAPIError(t, err, 409)
}

func TestDiskListMarksOccupied(t *testing.T) {
	svc, _, db, _ := newTestEnv(t)

	if err := db.Create(&model.StoragePool{
		NodeID: 1, DeviceID: "free", Status: model.StoragePoolReady,
	}).Error; err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	disks, err := svc.Disks(context.Background(), 1)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}

	byID := map[string]storage.DiskView{}
	for _, d := range disks {
		byID[d.DeviceID] = d
	}

	if byID["free"].Usable {
		t.Error("已被存储池占用的设备不应可用")
	}
	if byID["free"].InUseBy == "" {
		t.Error("未标注占用者")
	}
	if byID["sys"].Usable {
		t.Error("系统盘不应可用")
	}
	// 有数据的盘仍然 usable：它需要显式确认，而不是被直接禁止。
	if !byID["data"].Usable {
		t.Error("含数据的设备应可通过显式确认后使用")
	}
	if !byID["data"].HasData {
		t.Error("未标注设备含有数据，用户看不到风险提示")
	}
}

func TestAgentSideFailureSurfaces(t *testing.T) {
	svc, _, db, client := newTestEnv(t)
	client.failCreate = true

	if _, err := svc.Create(context.Background(), createReq("free", freeDisk), 7, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, "任务失败", func() bool {
		var tk model.Task
		db.Where("type = ?", model.TaskStoragePoolCreate).First(&tk)
		return tk.Status == model.TaskFailed
	})

	// agent 侧的校验失败必须传递到用户：它比控制面校验更准确，因为控制面
	// 掌握的设备信息可能已经滞后（R-005）。
	var tk model.Task
	db.Where("type = ?", model.TaskStoragePoolCreate).First(&tk)
	if tk.Error == nil || !strings.Contains(*tk.Error, "LVM") {
		t.Errorf("失败原因未传递: %v", tk.Error)
	}

	// 失败时**不写池记录**：留下一条「存在但不可用」的池，用户会拿它去
	// 建虚拟机，然后在某个说不清的时刻失败。
	var count int64
	db.Model(&model.StoragePool{}).Count(&count)
	if count != 0 {
		t.Errorf("失败后仍写入了 %d 条存储池记录", count)
	}
}

// --- 删除 ---

func TestDeleteRejectedWhenPoolHasVolumes(t *testing.T) {
	svc, _, db, client := newTestEnv(t)
	client.volumes = []string{"vm-web-01.qcow2", "vm-db-01.qcow2"}

	if err := db.Create(&model.StoragePool{
		NodeID: 1, DeviceID: "free", Status: model.StoragePoolReady,
	}).Error; err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	var pool model.StoragePool
	db.First(&pool)

	// 池内仍有磁盘时必须拒绝并**列出占用者**（R-008）：只说「无法删除」
	// 会让用户不知道要先去清理什么。
	_, err := svc.Delete(context.Background(), pool.ID, 7, "admin", "10.0.0.1")
	assertAPIError(t, err, 409)

	var apiErr *api.Error
	errors.As(err, &apiErr)
	for _, name := range client.volumes {
		if !strings.Contains(apiErr.Message, name) {
			t.Errorf("拒绝原因未列出占用者 %s: %q", name, apiErr.Message)
		}
	}
}

func TestDeleteFlowSoftDeletes(t *testing.T) {
	svc, _, db, _ := newTestEnv(t)

	if err := db.Create(&model.StoragePool{
		NodeID: 1, DeviceID: "free", Status: model.StoragePoolReady, IsDefault: true,
	}).Error; err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	var pool model.StoragePool
	db.First(&pool)

	if _, err := svc.Delete(context.Background(), pool.ID, 7, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, "记录被软删除", func() bool {
		var got model.StoragePool
		if err := db.First(&got, pool.ID).Error; err != nil {
			return false
		}
		return got.DeletedAt != nil
	})

	// 默认标记必须一并清掉：留下一个指向已删除设备的默认池，会让「该节点
	// 有默认池」与「默认池已不存在」同时成立。
	var got model.StoragePool
	db.First(&got, pool.ID)
	if got.IsDefault {
		t.Error("删除后仍保留默认标记")
	}

	// 列表按 deleted_at 过滤。
	pools, _ := svc.List(context.Background(), 1)
	if len(pools) != 0 {
		t.Errorf("已删除的池仍出现在列表中: %d 条", len(pools))
	}
}

func TestDeleteRejectedWhenNodeOffline(t *testing.T) {
	svc, _, db, _ := newTestEnv(t)
	ctx := context.Background()

	if err := db.Create(&model.StoragePool{
		NodeID: 999, DeviceID: "free", Status: model.StoragePoolReady,
	}).Error; err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	var pool model.StoragePool
	db.First(&pool)

	// 节点不存在（等价于不可达）：拒绝删除而不是盲目下发——无法确认占用
	// 情况就删除，可能删掉正在被虚拟机使用的池。
	_, err := svc.Delete(ctx, pool.ID, 7, "admin", "10.0.0.1")
	assertAPIError(t, err, 404)
}

// --- 默认池 ---

func TestSetDefaultMovesFlag(t *testing.T) {
	svc, _, db, _ := newTestEnv(t)
	ctx := context.Background()

	first := model.StoragePool{NodeID: 1, DeviceID: "a", IsDefault: true, Status: model.StoragePoolReady}
	second := model.StoragePool{NodeID: 1, DeviceID: "b", Status: model.StoragePoolReady}
	db.Create(&first)
	db.Create(&second)

	if _, err := svc.SetDefault(ctx, second.ID, true, "admin", "10.0.0.1"); err != nil {
		t.Fatalf("切换失败: %v", err)
	}

	// 同一节点至多一个默认池：切换必须在事务里先把旧标记清掉，
	// 否则会撞上部分唯一索引。
	var reloadedFirst, reloadedSecond model.StoragePool
	db.First(&reloadedFirst, first.ID)
	db.First(&reloadedSecond, second.ID)
	if reloadedFirst.IsDefault {
		t.Error("旧默认池的标记未清除")
	}
	if !reloadedSecond.IsDefault {
		t.Error("新默认池未生效")
	}
}
