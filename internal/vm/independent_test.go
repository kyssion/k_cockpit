package vm_test

import (
	"context"
	"strings"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

// viewer7 是种子虚拟机属主对应的调用者。
func viewer7() authz.Viewer { return authz.Viewer{UserID: 7} }

// TestNotLinkedIsRefused 覆盖「已经是独立盘」。
//
// 静默成功会让用户以为"刚才那步做了什么"，而他实际上什么都没发生。
func TestNotLinkedIsRefused(t *testing.T) {
	svc, _, db := newTestEnv(t)
	owner := int64(7)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)
	v := model.VM{
		Name: "full-clone", NodeID: node.ID, Status: model.VMStatusStopped,
		CloneMode: model.CloneFull, OwnerID: &owner, DiskGB: 20,
	}
	db.Create(&v)

	_, err := svc.MakeDisksIndependent(context.Background(), v.ID,
		viewer7(), "alice", "10.0.0.1")
	assertStatus(t, err, 422)
	if err != nil && !strings.Contains(err.Error(), "已经是独立") {
		t.Errorf("报错应说明当前状态，而不只是一句不支持: %v", err)
	}
}

// TestRunningIsRefused 覆盖停机要求。
//
// 合并要复制整个镜像，而运行中的机器还在往那层覆盖里写——复制出来的内容
// 会与任何一个时刻都对不上。
func TestRunningIsRefused(t *testing.T) {
	svc, _, db := newTestEnv(t)
	owner := int64(7)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)
	v := model.VM{
		Name: "linked", NodeID: node.ID, Status: model.VMStatusRunning,
		CloneMode: model.CloneLinked, OwnerID: &owner, DiskGB: 20,
	}
	db.Create(&v)

	_, err := svc.MakeDisksIndependent(context.Background(), v.ID,
		viewer7(), "alice", "10.0.0.1")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "关机") {
		t.Errorf("报错应要求先关机: %v", err)
	}
}

// TestSuccessEnqueuesTask 覆盖正常路径。
func TestSuccessEnqueuesTask(t *testing.T) {
	svc, q, db := newTestEnv(t)
	q.Register(vm.NewIndependentExecutor(db, agent.NewMockClient()))
	owner := int64(7)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)
	v := model.VM{
		Name: "linked", NodeID: node.ID, Status: model.VMStatusStopped,
		CloneMode: model.CloneLinked, OwnerID: &owner, DiskGB: 20,
	}
	db.Create(&v)

	tk, err := svc.MakeDisksIndependent(context.Background(), v.ID,
		viewer7(), "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if tk == nil {
		t.Fatal("应产生任务")
	}
	// **此时标记还没改**：要等节点确认。
	var after model.VM
	db.First(&after, v.ID)
	if after.CloneMode != model.CloneLinked {
		t.Error("下发前不该改标记——节点还没确认")
	}
}

// TestMarkerChangesOnlyAfterNodeSucceeds 覆盖本操作里最要紧的一条。
//
// 标记**只能在节点确认成功之后**才改：先改成 full 的话，控制面会说「这台机器
// 已经独立」而实际它还依赖着父盘。那时用户去删模板——删掉了——然后一堆机器
// 坏掉，而没有任何地方提示过它们还在依赖。
//
// 这是这个操作里唯一一处会**静默造成大面积损坏**的路径。
func TestMarkerChangesOnlyAfterNodeSucceeds(t *testing.T) {
	// 一个**总是失败**的节点：模拟合并中途出错。
	svc, q, db := newTestEnvWithClient(t, &failingAgent{})
	q.Register(vm.NewIndependentExecutor(db, &failingAgent{}))

	owner := int64(7)
	node := model.Node{Name: "n1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)
	v := model.VM{
		Name: "linked", NodeID: node.ID, Status: model.VMStatusStopped,
		CloneMode: model.CloneLinked, OwnerID: &owner, DiskGB: 20,
	}
	db.Create(&v)

	tk, err := svc.MakeDisksIndependent(context.Background(), v.ID,
		viewer7(), "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("入队应成功（失败发生在节点侧）: %v", err)
	}

	// 直接驱动执行器，绕过队列。
	if err := vm.NewIndependentExecutor(db, &failingAgent{}).Run(context.Background(), tk); err == nil {
		t.Fatal("节点失败时执行器应返回错误")
	}

	var after model.VM
	db.First(&after, v.ID)
	if after.CloneMode != model.CloneLinked {
		t.Fatal("节点失败时标记必须保持 linked——" +
			"改成 full 会让控制面说「已独立」而实际还依赖父盘，" +
			"用户去删模板就会有一批机器静默损坏")
	}
}

// failingAgent 是一个总是失败的节点替身。
type failingAgent struct{}

func (f *failingAgent) Execute(_ context.Context, _ agent.Operation) (*agent.Result, error) {
	return &agent.Result{Success: false, Message: "合并中途失败：磁盘空间不足"}, nil
}
