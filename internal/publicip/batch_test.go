package publicip_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/model"
	"k_cockpit/internal/publicip"
)

// TestBatchUnbindReportsPerItem 覆盖批量最要紧的一条：**逐条如实报告**。
//
// 一个笼统的「批量操作失败」会让用户不知道该处理哪几个——而那正是他要处理
// 的东西。
func TestBatchUnbindReportsPerItem(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vm := seedVM(t, db, "vm1", 1)
	node, vmID := vm.NodeID, vm.ID

	// 两个地址：一个绑着、一个没绑。
	bound := model.PublicIP{NodeID: node, IP: "203.0.113.10"}
	free := model.PublicIP{NodeID: node, IP: "203.0.113.11"}
	db.Create(&bound)
	db.Create(&free)
	db.Create(&model.PublicIPBinding{
		PublicIPID: bound.ID, NodeID: node, VMID: &vmID,
	})

	res, err := svc.BatchUnbind(ctx, []int64{bound.ID, free.ID}, admin(), "root", "")
	if err != nil {
		t.Fatalf("批量解绑失败: %v", err)
	}
	if len(res.OK) != 1 {
		t.Errorf("应成功 1 条，实际 %d", len(res.OK))
	}
	if len(res.Failed) != 1 {
		t.Fatalf("应失败 1 条（那个没绑的），实际 %d", len(res.Failed))
	}
	// **失败的那条要能对上号**：只给 id 的话用户看不出是哪个地址。
	if res.Failed[0].ID != free.ID {
		t.Errorf("失败的应是未绑定的那个（%d），实际 %d", free.ID, res.Failed[0].ID)
	}
	if res.Failed[0].Reason == "" {
		t.Error("失败必须给原因")
	}
}

// TestBatchMessageExplainsNoRollback 覆盖总结里那句最要紧的话。
//
// 用户看到「部分失败」时第一个问题是「那前面那几条还算数吗」。答案是不回滚
// ——回滚意味着把已经下发好的规则再撤掉，那会让一次失败的批量操作变成两次
// 网络变更。
func TestBatchMessageExplainsNoRollback(t *testing.T) {
	svc, db := newTestEnv(t)
	ctx := context.Background()
	vm := seedVM(t, db, "vm1", 1)
	node, vmID := vm.NodeID, vm.ID

	bound := model.PublicIP{NodeID: node, IP: "203.0.113.10"}
	free := model.PublicIP{NodeID: node, IP: "203.0.113.11"}
	db.Create(&bound)
	db.Create(&free)
	db.Create(&model.PublicIPBinding{
		PublicIPID: bound.ID, NodeID: node, VMID: &vmID,
	})

	res, err := svc.BatchUnbind(ctx, []int64{bound.ID, free.ID}, admin(), "root", "")
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if !strings.Contains(res.Message, "不会被撤销") {
		t.Errorf("部分失败时总结必须说清已成功的那几条不会被撤销，实际: %q", res.Message)
	}
}

// TestBatchAllFail 覆盖全失败。
func TestBatchAllFail(t *testing.T) {
	svc, db := newTestEnv(t)
	node := seedVM(t, db, "vm1", 1).NodeID

	free := model.PublicIP{NodeID: node, IP: "203.0.113.11"}
	db.Create(&free)

	res, err := svc.BatchUnbind(context.Background(), []int64{free.ID}, admin(), "root", "")
	if err != nil {
		t.Fatalf("失败: %v", err)
	}
	if len(res.OK) != 0 || len(res.Failed) != 1 {
		t.Fatalf("应全失败，实际 ok=%d failed=%d", len(res.OK), len(res.Failed))
	}
	if !strings.Contains(res.Message, "全部失败") {
		t.Errorf("全失败的总结应明说没有产生任何变更，实际: %q", res.Message)
	}
}

// TestBatchIsCapped 覆盖上限，且**理由要说出来**。
//
// 上限不是"系统限制"而是与宿主机网络有关：每个地址都要下发一条 NAT 规则，
// 一次几百条会让配置在几秒内连续变更，期间正在连的会话可能中断。
func TestBatchIsCapped(t *testing.T) {
	svc, _ := newTestEnv(t)

	ids := make([]int64, publicip.MaxBatch+1)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	_, err := svc.BatchUnbind(context.Background(), ids, admin(), "root", "")
	if err == nil {
		t.Fatal("超过上限应被拒绝")
	}
	if !strings.Contains(err.Error(), "NAT") {
		t.Errorf("报错应说明理由（与宿主机网络有关），实际: %v", err)
	}
}

func TestBatchRejectsEmpty(t *testing.T) {
	svc, _ := newTestEnv(t)

	if _, err := svc.BatchUnbind(context.Background(), nil, admin(), "root", ""); err == nil {
		t.Error("空列表应被拒绝")
	}
	if _, err := svc.BatchBind(context.Background(), []int64{1}, 0, "", admin(), "root", ""); err == nil {
		t.Error("没指定目标虚拟机应被拒绝")
	}
}
