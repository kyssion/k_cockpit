package node_test

import (
	"context"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/model"
	"k_cockpit/internal/node"
)

func TestStatsRejectsUnenrolledNode(t *testing.T) {
	svc, db := newTestService(t, &fakeRuntime{})
	ctx := context.Background()

	row := model.Node{Name: "node-p", EnrollState: model.NodeEnrollPending}
	db.Create(&row)

	// 未接入的节点连 agent 都没有，请求它只会得到一个必然失败的往返。
	// 受理时就拒绝，给出一句能看懂的话。
	_, err := svc.Stats(ctx, row.ID)
	assertAPIError(t, err, 422)
}

func TestStatsRejectsMissingNode(t *testing.T) {
	svc, _ := newTestService(t, &fakeRuntime{})

	_, err := svc.Stats(context.Background(), 9999)
	assertAPIError(t, err, 404)
}

// TestStatsWithoutClientSaysUnsupported 覆盖一条刻意的选择。
//
// 未装配指标采集时**明确说「不支持」**，而不是给出一堆 0——0% 的 CPU
// 看起来是「机器很闲」，而实际是「没读到」。两者对排查的意义完全相反。
func TestStatsWithoutClientSaysUnsupported(t *testing.T) {
	db := newStatsDB(t)
	// 不传 agent 客户端。
	svc := node.NewService(db, &fakeRuntime{}, audit.NewRecorder(db), nil)
	ctx := context.Background()

	row := model.Node{Name: "node-e", EnrollState: model.NodeEnrollEnrolled}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	_, err := svc.Stats(ctx, row.ID)
	assertAPIError(t, err, 503)
}

func TestStatsCountsVMsFromControlPlane(t *testing.T) {
	db := newStatsDB(t)
	svc := node.NewService(db, &fakeRuntime{}, audit.NewRecorder(db), agent.NewMockClient())
	ctx := context.Background()

	nodeRow := model.Node{Name: "node-s", EnrollState: model.NodeEnrollEnrolled}
	if err := db.Create(&nodeRow).Error; err != nil {
		t.Fatalf("创建节点失败: %v", err)
	}

	// 三台在虚拟化层、一台已不在（present=false）。
	vms := []model.VM{
		{NodeID: nodeRow.ID, Name: "a", Status: model.VMStatusRunning, Present: true},
		{NodeID: nodeRow.ID, Name: "b", Status: model.VMStatusRunning, Present: true},
		{NodeID: nodeRow.ID, Name: "c", Status: model.VMStatusStopped, Present: true},
		{NodeID: nodeRow.ID, Name: "d", Status: model.VMStatusStopped, Present: false},
	}
	for i := range vms {
		if err := db.Create(&vms[i]).Error; err != nil {
			t.Fatalf("创建虚拟机失败: %v", err)
		}
	}

	// present=false 必须走 Update 设置。
	//
	// ⚠️ `Present` 带 `default:true`，而 GORM 在 Create 时会**省略零值**
	// （false），数据库随即填入 `true`——也就是
	// `Create(&VM{Present: false})` 得到的是一条**存在**的记录。
	// 这与 model/schedule.go 里记录的是同一个陷阱，生产代码里删除走 Update。
	if err := db.Model(&model.VM{}).Where("name = ?", "d").
		Update("present", false).Error; err != nil {
		t.Fatalf("标记虚拟机已移除失败: %v", err)
	}

	stats, err := svc.Stats(ctx, nodeRow.ID)
	if err != nil {
		t.Fatalf("读取指标失败: %v", err)
	}
	// 数量由**控制面统计**（它持有投影），因此与列表页的数字一致。
	if stats.VMCount != 3 {
		t.Errorf("虚拟机数 = %d, 期望 3（已不在虚拟化层的那台不计入）", stats.VMCount)
	}
	if stats.VMRunning != 2 {
		t.Errorf("运行中 = %d, 期望 2", stats.VMRunning)
	}
	if stats.At.IsZero() {
		t.Error("未带采集时刻——界面无法判断数据新鲜度")
	}
	// 指标本身来自节点（mock），不是控制面凭空造的。
	if stats.CPUCores == 0 {
		t.Error("未取到节点上报的 CPU 核数")
	}
}
