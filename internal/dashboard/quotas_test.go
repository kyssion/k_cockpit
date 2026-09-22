package dashboard_test

import (
	"context"
	"testing"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/authz"
	"k_cockpit/internal/computequota"
	"k_cockpit/internal/dashboard"
	"k_cockpit/internal/model"
	"k_cockpit/internal/quota"
	"k_cockpit/internal/quotaenforce"
)

// dptr 是 int64 指针的简写（本包测试没有现成的同名辅助）。
func dptr(v int64) *int64 { return &v }

// quotaEnv 在 newEnv 的表集合上补齐配额聚合用到的表，并装配三个读数服务。
// 返回 db 供用例预置数据。
func quotaEnv(t *testing.T) (context.Context, *gorm.DB, *dashboard.Service) {
	t.Helper()
	db, svc := newEnv(t, fakeRuntime{hb: map[int64]time.Time{}})
	if err := db.AutoMigrate(
		&model.ComputeQuota{}, &model.UserStorage{},
		&model.VMRuntimeDaily{}, &model.TrafficStatDaily{},
		// countsOf 统计数量型维度要用这三张表。
		&model.VMSnapshot{}, &model.PortForward{}, &model.PublicIPBinding{},
	); err != nil {
		t.Fatalf("建配额表失败: %v", err)
	}
	svc.SetQuotaReaders(
		computequota.NewService(db, nil),
		quota.NewService(db, nil),
		quotaenforce.NewService(db, nil, nil, nil),
	)
	return context.Background(), db, svc
}

// TestQuotaOverviewAggregatesDimensions 覆盖「我的配额」聚合（G-32）。
//
// 聚合的核心承诺是：用户看到的数字与判定层用的数字**出自同一处**。因此
// 测试预置真实的配额行，验证三条数据源在同一个视图里对得上。
func TestQuotaOverviewAggregatesDimensions(t *testing.T) {
	ctx, db, svc := quotaEnv(t)
	viewer := authz.Viewer{UserID: 7}

	seedNode(t, db, 1, "node-a", "active")
	seedNode(t, db, 2, "node-b", "active")

	// 节点 1：有虚拟机（存量型用量的来源）。
	if err := db.Create(&model.VM{
		NodeID: 1, Name: "u7-vm", Status: model.VMStatusStopped,
		OwnerID: dptr(7), VCPU: 2, MemoryMB: 2048, DiskGB: 40, Present: true,
	}).Error; err != nil {
		t.Fatalf("预置虚拟机失败: %v", err)
	}
	// 节点 1 配了计算配额与运行时长配额；节点 2 只有一条配额行
	// （用户还没建机，但这个节点也应该出现在视图里）。
	if err := db.Create(&model.ComputeQuota{
		NodeID: 1, UserID: 7, VCPU: 8, MemoryMB: 8192, VMCount: 4,
	}).Error; err != nil {
		t.Fatalf("预置计算配额失败: %v", err)
	}
	if err := db.Create(&model.ResourceQuota{
		NodeID: 1, UserID: 7, Dimension: model.QuotaDimRuntime, LimitValue: 100,
	}).Error; err != nil {
		t.Fatalf("预置资源配额失败: %v", err)
	}
	if err := db.Create(&model.ResourceQuota{
		NodeID: 2, UserID: 7, Dimension: model.QuotaDimRuntime, LimitValue: 50,
	}).Error; err != nil {
		t.Fatalf("预置资源配额失败: %v", err)
	}

	out, err := svc.QuotaOverview(ctx, viewer)
	if err != nil {
		t.Fatalf("获取配额总览失败: %v", err)
	}
	if len(out.Nodes) != 2 {
		t.Fatalf("节点数 = %d, 期望 2（有虚拟机的 + 只有配额行的）", len(out.Nodes))
	}

	byNode := map[int64]dashboard.NodeQuota{}
	for _, n := range out.Nodes {
		byNode[n.NodeID] = n
	}
	n1 := byNode[1]
	if n1.NodeName == "" {
		t.Error("节点名没有解析出来")
	}
	if n1.VCPU.Used != 2 || n1.VCPU.Limit != 8 || !n1.VCPU.HasLimit {
		t.Errorf("vCPU = %+v, 期望 2/8", n1.VCPU)
	}
	if n1.MemoryMB.Used != 2048 || n1.MemoryMB.Limit != 8192 {
		t.Errorf("内存 = %+v, 期望 2048/8192", n1.MemoryMB)
	}
	if n1.VMs.Used != 1 || n1.VMs.Limit != 4 {
		t.Errorf("VM 数 = %+v, 期望 1/4", n1.VMs)
	}
	if n1.RuntimeHours.Limit != 100 || !n1.RuntimeHours.HasLimit {
		t.Errorf("运行时长 = %+v, 期望 0/100", n1.RuntimeHours)
	}
	if n1.WorstStatus != model.QuotaStatusOK {
		t.Errorf("WorstStatus = %q, 期望 ok", n1.WorstStatus)
	}
	// 节点 2 没有虚拟机：计算维度应全部为 0，但运行时长上限可见。
	if n2 := byNode[2]; n2.RuntimeHours.Limit != 50 || !n2.RuntimeHours.HasLimit {
		t.Errorf("节点 2 运行时长 = %+v, 期望 0/50", n2.RuntimeHours)
	}
}

// TestQuotaOverviewAdminEmpty 覆盖管理员视角：返回空列表而不是错误，
// 前端不必为同一页面维护两条错误路径。
func TestQuotaOverviewAdminEmpty(t *testing.T) {
	ctx, _, svc := quotaEnv(t)

	out, err := svc.QuotaOverview(ctx, authz.Viewer{UserID: 1, IsAdmin: true})
	if err != nil {
		t.Fatalf("管理员调用不应报错: %v", err)
	}
	if len(out.Nodes) != 0 {
		t.Errorf("管理员应得到空列表, 实际 %d 个节点", len(out.Nodes))
	}
}
