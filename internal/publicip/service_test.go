package publicip_test

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
	"k_cockpit/internal/authz"
	"k_cockpit/internal/config"
	"k_cockpit/internal/database"
	"k_cockpit/internal/model"
	"k_cockpit/internal/publicip"
	"k_cockpit/internal/task"
)

func newTestEnv(t *testing.T) (*publicip.Service, *gorm.DB) {
	t.Helper()

	db, err := database.Open(config.DB{
		Driver:       config.DriverSQLite,
		Path:         filepath.Join(t.TempDir(), "publicip.db"),
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
		&model.PublicIP{}, &model.PublicIPBinding{}, &model.Node{}, &model.VM{},
		&model.Task{}, &model.TaskStage{}, &model.AuditLog{},
	); err != nil {
		t.Fatalf("建表失败: %v", err)
	}
	if err := db.Create(&model.Node{
		ID: 1, Name: "node-1", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	client := agent.NewMockClient()
	recorder := audit.NewRecorder(db)
	queue := task.NewQueue(db, recorder, task.Options{
		MaxConcurrent: 1,
		PollInterval:  20 * time.Millisecond,
	})
	queue.Register(publicip.NewChangeExecutor(db, client))

	ctx, cancel := context.WithCancel(context.Background())
	queue.Start(ctx)
	t.Cleanup(func() {
		cancel()
		queue.Stop()
	})

	return publicip.NewService(db, queue, client, recorder), db
}

// 测试里统一用的操作者（管理员：地址池是节点级资源）。
func admin() authz.Viewer { return authz.Viewer{UserID: 1, IsAdmin: true} }

func seedVM(t *testing.T, db *gorm.DB, name string, nodeID int64) *model.VM {
	t.Helper()
	owner := int64(7)
	row := model.VM{
		NodeID: nodeID, Name: name, Status: model.VMStatusStopped,
		OwnerID: &owner, Present: true,
	}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return &row
}

// TestExpandSkipsReservedAddresses 覆盖 CIDR 展开的边界。
//
// 网络地址、广播地址与网关**不能被分配给虚拟机**。展开时不过滤的话，
// 用户会得到一个看起来正常、绑定时才失败的地址，而失败信息来自内核，
// 与他刚才做的事联系不起来。
func TestExpandSkipsReservedAddresses(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	views, err := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.0/29", Gateway: "203.0.113.1",
	}, admin(), "admin", "10.0.0.1")
	if err != nil {
		t.Fatalf("录入失败: %v", err)
	}

	got := map[string]bool{}
	for _, v := range views {
		got[v.IP] = true
	}

	// /29 = 8 个地址：0 是网络地址、7 是广播地址、1 是网关，可用 5 个。
	if got["203.0.113.0"] {
		t.Error("网络地址被录入了——它不能分配给虚拟机")
	}
	if got["203.0.113.7"] {
		t.Error("广播地址被录入了——它不能分配给虚拟机")
	}
	if got["203.0.113.1"] {
		t.Error("网关地址被录入了——它属于宿主机")
	}
	if len(got) != 5 {
		t.Errorf("录入 %d 个地址, 期望 5（/29 去掉网络、广播与网关）: %v", len(got), got)
	}
	for _, ip := range []string{"203.0.113.2", "203.0.113.6"} {
		if !got[ip] {
			t.Errorf("可用地址 %s 未被录入", ip)
		}
	}
}

// TestExpandRejectsOversizedCIDR 覆盖一条防手滑的限制。
//
// 一个 /8 会展开出一千六百多万条记录。用户多半是打错了一位掩码，而照做
// 会让库里多出一批他完全没打算录入的地址，且清理起来同样困难。
func TestExpandRejectsOversizedCIDR(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "10.0.0.0/8",
	}, admin(), "admin", "")
	assertStatus(t, err, 400)
}

func TestCreateRejectsBadAddress(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	for _, bad := range []string{"", "not-an-ip", "999.999.999.999"} {
		_, err := svc.Create(ctx, publicip.CreateRequest{
			NodeID: 1, IP: bad,
		}, admin(), "admin", "")
		if err == nil {
			t.Errorf("地址 %q 应被拒绝", bad)
		}
	}
}

// TestIPv6RejectsNAT 覆盖模式与地址族的组合校验。
//
// IPv6 地址本就充足，不需要地址转换，而上游通常也不会为它配一条 NAT 规则。
// 允许这个组合会让绑定在节点上以一句内核报错结束。
func TestIPv6RejectsNAT(t *testing.T) {
	svc, _ := newTestEnv(t)
	ctx := context.Background()

	_, err := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "2001:db8::1",
		SupportedModes: []string{model.PublicIPModeNAT},
	}, admin(), "admin", "")
	assertStatus(t, err, 400)
}

func TestDuplicateAddressRejected(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	if _, err := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.10",
	}, admin(), "admin", ""); err != nil {
		t.Fatalf("首次录入失败: %v", err)
	}

	// 同一节点内地址唯一（uniq_public_ip_node_ip）：同一个地址在两台
	// 宿主机上出现会让流量随机落到其中一台，而从现象上几乎看不出来。
	_, err := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.10",
	}, admin(), "admin", "")
	assertStatus(t, err, 409)

	_ = db
}

// TestBindThenRejectsSecondBind 覆盖「一个地址同一时刻只有一条有效绑定」。
//
// 静默改绑看起来更省事，但它让「这个地址原来指向谁」这个信息在一次点击里
// 消失了——而如果用户点错了一行，他连错在哪里都看不到。换目标要走
// 浮动迁移，那个入口会明确说明在做什么。
func TestBindThenRejectsSecondBind(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	vmA := seedVM(t, db, "vm-a", 1)
	vmB := seedVM(t, db, "vm-b", 1)

	views, err := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.20",
	}, admin(), "admin", "")
	if err != nil {
		t.Fatalf("录入失败: %v", err)
	}
	ipID := views[0].ID

	if _, err := svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: ipID, VMID: vmA.ID, Mode: model.PublicIPModeNAT,
	}, admin(), "admin", ""); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	waitForIPBound(t, svc, ipID, vmA.ID)

	_, err = svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: ipID, VMID: vmB.ID, Mode: model.PublicIPModeNAT,
	}, admin(), "admin", "")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "浮动迁移") {
		t.Errorf("拒绝文案应指向正确的做法: %v", err)
	}
}

// TestMigrateReleasesOldAndBindsNew 覆盖浮动迁移（f-4-06）。
//
// 这是公网 IP 最有价值的场景——**故障转移**。迁移必须原子：解绑与绑定之间
// 如果插进了失败，地址会悬空（不指向任何地方），而那时外部访问已经断了，
// 用户却以为迁移还在进行。
func TestMigrateReleasesOldAndBindsNew(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	vmA := seedVM(t, db, "vm-old", 1)
	vmB := seedVM(t, db, "vm-new", 1)

	views, err := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.30",
	}, admin(), "admin", "")
	if err != nil {
		t.Fatalf("录入失败: %v", err)
	}
	ipID := views[0].ID

	if _, err := svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: ipID, VMID: vmA.ID, Mode: model.PublicIPModeNAT,
	}, admin(), "admin", ""); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	waitForIPBound(t, svc, ipID, vmA.ID)

	if _, err := svc.Migrate(ctx, publicip.MigrateRequest{
		PublicIPID: ipID, ToVMID: vmB.ID,
	}, admin(), "admin", ""); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	waitForIPBound(t, svc, ipID, vmB.ID)

	// 旧绑定必须被**标记释放**而不是删除：事后追查「这个地址在某个时间点
	// 指向谁」时，唯一能回答的就是这条历史。
	var bindings []model.PublicIPBinding
	db.Where("public_ip_id = ?", ipID).Order("id ASC").Find(&bindings)
	if len(bindings) != 2 {
		t.Fatalf("绑定记录 = %d 条, 期望 2（旧的标记释放、新的生效）", len(bindings))
	}
	if bindings[0].ReleasedAt == nil {
		t.Error("旧绑定未被标记释放")
	}
	if bindings[1].ReleasedAt != nil {
		t.Error("新绑定不应带释放时间")
	}
	if bindings[1].VMID == nil || *bindings[1].VMID != vmB.ID {
		t.Errorf("新绑定未指向目标虚拟机: %v", bindings[1].VMID)
	}

	// 同一时刻只有一条有效绑定——这是网络层的事实。
	var active int64
	db.Model(&model.PublicIPBinding{}).
		Where("public_ip_id = ? AND released_at IS NULL", ipID).Count(&active)
	if active != 1 {
		t.Errorf("有效绑定 = %d 条, 期望 1", active)
	}
}

func TestMigrateRejectsSameVM(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vmA := seedVM(t, db, "vm-same", 1)

	views, _ := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.40",
	}, admin(), "admin", "")
	ipID := views[0].ID

	if _, err := svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: ipID, VMID: vmA.ID, Mode: model.PublicIPModeNAT,
	}, admin(), "admin", ""); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	waitForIPBound(t, svc, ipID, vmA.ID)

	_, err := svc.Migrate(ctx, publicip.MigrateRequest{
		PublicIPID: ipID, ToVMID: vmA.ID,
	}, admin(), "admin", "")
	assertStatus(t, err, 422)
}

// TestBindRejectsCrossNodeVM 覆盖一条没有网络路径的组合。
//
// 公网地址是节点内资源；指向另一台宿主机上的虚拟机没有任何路径可走。
func TestBindRejectsCrossNodeVM(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	if err := db.Create(&model.Node{
		ID: 2, Name: "node-2", EnrollState: model.NodeEnrollEnrolled,
	}).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}
	remote := seedVM(t, db, "vm-remote", 2)

	views, _ := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.50",
	}, admin(), "admin", "")
	ipID := views[0].ID

	_, err := svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: ipID, VMID: remote.ID, Mode: model.PublicIPModeNAT,
	}, admin(), "admin", "")
	assertStatus(t, err, 422)
}

// TestDeleteRejectsBoundAddress 覆盖一条会制造控制面与宿主机分叉的路径。
//
// 移除一个正在被使用的地址，会让它的绑定记录指向一条不存在的地址，
// 而节点上那条规则仍然生效——两边就此分叉，且没有任何一处在报错。
func TestDeleteRejectsBoundAddress(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vmA := seedVM(t, db, "vm-bound", 1)

	views, _ := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.60",
	}, admin(), "admin", "")
	ipID := views[0].ID

	if _, err := svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: ipID, VMID: vmA.ID, Mode: model.PublicIPModeNAT,
	}, admin(), "admin", ""); err != nil {
		t.Fatalf("绑定失败: %v", err)
	}
	waitForIPBound(t, svc, ipID, vmA.ID)

	err := svc.Delete(ctx, ipID, admin(), "admin", "")
	assertStatus(t, err, 409)

	// 解绑之后应可移除。
	if _, err := svc.Unbind(ctx, ipID, admin(), "admin", ""); err != nil {
		t.Fatalf("解绑失败: %v", err)
	}
	waitForIPFree(t, svc, ipID)
	if err := svc.Delete(ctx, ipID, admin(), "admin", ""); err != nil {
		t.Errorf("解绑后应可移除: %v", err)
	}
}

// TestMaintenanceModeBlocksChanges 覆盖维护模式对网络变更的拦截。
//
// 维护的意义就是不引入变更，而公网地址的绑定正是网络层面的一次变更。
func TestMaintenanceModeBlocksChanges(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()

	views, _ := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.70",
	}, admin(), "admin", "")
	ipID := views[0].ID
	vmA := seedVM(t, db, "vm-maint", 1)

	if err := db.Model(&model.Node{}).Where("id = ?", 1).
		Update("maintenance_mode", true).Error; err != nil {
		t.Fatalf("设置维护模式失败: %v", err)
	}

	_, err := svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: ipID, VMID: vmA.ID, Mode: model.PublicIPModeNAT,
	}, admin(), "admin", "")
	assertStatus(t, err, 422)
}

// TestPreviewDoesNotChangeAnything 覆盖预览是只读的。
//
// 预览的全部意义就是让用户在改动网络之前看到将要发生什么——它本身
// 绝不能产生副作用。
func TestPreviewDoesNotChangeAnything(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vmA := seedVM(t, db, "vm-preview", 1)

	views, _ := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.80",
	}, admin(), "admin", "")
	ipID := views[0].ID

	preview, err := svc.Preview(ctx, ipID, vmA.ID, model.PublicIPModeNAT, admin())
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if len(preview.Added) == 0 {
		t.Error("预览未给出任何将要新增的规则——那样它就没有信息量")
	}

	// 预览之后地址仍然未绑定。
	var count int64
	db.Model(&model.PublicIPBinding{}).
		Where("public_ip_id = ? AND released_at IS NULL", ipID).Count(&count)
	if count != 0 {
		t.Error("预览产生了绑定——它必须是只读的")
	}
}

func TestBindRejectsUnsupportedMode(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vmA := seedVM(t, db, "vm-mode", 1)

	views, err := svc.Create(ctx, publicip.CreateRequest{
		NodeID: 1, IP: "203.0.113.90",
		// 该地址只支持 NAT（上游只给了这一条路）。
		SupportedModes: []string{model.PublicIPModeNAT},
	}, admin(), "admin", "")
	if err != nil {
		t.Fatalf("录入失败: %v", err)
	}
	ipID := views[0].ID

	_, err = svc.Bind(ctx, publicip.BindRequest{
		PublicIPID: ipID, VMID: vmA.ID, Mode: model.PublicIPModeBridged,
	}, admin(), "admin", "")
	// 让用户去试错会得到一句来自内核的报错，而不是「这个地址不支持这种模式」。
	assertStatus(t, err, 422)
}

// --- 辅助 ---

func waitForIPBound(t *testing.T, svc *publicip.Service, ipID, vmID int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, err := svc.List(context.Background(), 0)
		if err == nil {
			for _, v := range items {
				if v.ID == ipID && v.VMID != nil && *v.VMID == vmID {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待地址 %d 绑定到虚拟机 %d 超时", ipID, vmID)
}

func waitForIPFree(t *testing.T, svc *publicip.Service, ipID int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		items, err := svc.List(context.Background(), 0)
		if err == nil {
			for _, v := range items {
				if v.ID == ipID && v.VMID == nil {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待地址 %d 解绑超时", ipID)
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
