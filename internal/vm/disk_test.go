package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/model"
	"k_cockpit/internal/vm"
)

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

// seedVM 造一台指定容量与状态的虚拟机。
//
// 内联构造而不是复用别处的助手：那些助手是为控制台/删除场景建的，
// 它们的默认字段会随着那些场景变化——而这里的测试依赖的只有容量与状态。
func seedVM(t *testing.T, db *gorm.DB, gb int, status string) int64 {
	t.Helper()
	// 节点必须处于「已纳管」状态：ensureNodeUsable 只对纳管中的节点放行，
	// 而一个默认零值的节点会让扩容以一个笼统的内部错误失败。
	node := model.Node{Name: "node-1", EnrollState: model.NodeEnrollEnrolled}
	db.Create(&node)
	// **必须设属主**：s.load 会按归属过滤，而属主为空的机器任何人都查不到
	// ——那会让这个测试以"虚拟机不存在"失败，看起来像别的问题。
	owner := int64(7)
	v := model.VM{
		Name: "vm-disk-test", NodeID: node.ID, Status: status,
		DiskGB: gb, VCPU: 2, MemoryMB: 2048, OwnerID: &owner,
	}
	if err := db.Create(&v).Error; err != nil {
		t.Fatalf("创建虚拟机失败: %v", err)
	}
	return v.ID
}

// TestShrinkIsRefused 覆盖最要紧的一条。
//
// 缩容会**丢数据**：镜像文件变小之后，文件系统里超出新边界的那些块还在原地，
// 但已经不属于这个设备了——而文件系统自己不知道。这不是「有风险」，是「一定
// 会坏」。因此它必须被**拒绝**，而不是"提示风险后允许"。
func TestShrinkIsRefused(t *testing.T) {
	svc, _, db := newTestEnv(t)
	vid := seedVM(t, db, 100, model.VMStatusStopped)

	_, err := svc.ResizeDisk(context.Background(), vid, 50,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertStatus(t, err, 422)
	// 文案要说清**为什么**，而不只是「不支持」——用户很可能本来就以为
	// 缩容可以（在别的地方见过）。
	if err != nil && !strings.Contains(err.Error(), "覆盖") {
		t.Errorf("报错应说明后果（会覆盖别的内容），实际: %v", err)
	}
}

// TestSameSizeIsRefused 覆盖「没有变化」。
//
// 静默成功会让用户以为做了什么，而节点上跑了一遍完整流程。
func TestSameSizeIsRefused(t *testing.T) {
	svc, _, db := newTestEnv(t)
	vid := seedVM(t, db, 100, model.VMStatusStopped)

	_, err := svc.ResizeDisk(context.Background(), vid, 100,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertStatus(t, err, 422)
}

// TestRunningIsRefusedAndPointsElsewhere 覆盖运行中的拒绝。
//
// **必须指向另一个入口**：用户想做的事（扩盘）在那边能做。只说"要关机"会
// 让他去关机再回来——那也能成，但多了一步，而他在这一步里会怀疑自己是不是
// 搞错了。
func TestRunningIsRefusedAndPointsElsewhere(t *testing.T) {
	svc, _, db := newTestEnv(t)
	vid := seedVM(t, db, 100, model.VMStatusRunning)

	_, err := svc.ResizeDisk(context.Background(), vid, 200,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	assertStatus(t, err, 409)
	if err != nil && !strings.Contains(err.Error(), "来宾自动化") {
		t.Errorf("应指向「来宾自动化」那个入口，实际: %v", err)
	}
}

// TestGrowSucceeds 覆盖正常扩容。
//
// 同时断言**记录被更新**与**GuestGrowNeeded 被标出**——后者是这个操作最
// 容易让用户误解的一点：宿主机侧扩完只是"盘子变大了"，操作系统看到的仍是
// 原来的分区。
func TestGrowSucceeds(t *testing.T) {
	svc, q, db := newTestEnv(t)
	// **必须注册执行器**：队列会拒绝没有执行器的任务类型，而测试环境
	// 不走 main.go 的装配。
	q.Register(vm.NewDiskResizeExecutor(db, agent.NewMockClient()))
	vid := seedVM(t, db, 100, model.VMStatusStopped)

	res, err := svc.ResizeDisk(context.Background(), vid, 200,
		authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("扩容失败: %v", err)
	}
	if res.OldGB != 100 || res.NewGB != 200 {
		t.Errorf("容量记录不对: %d → %d", res.OldGB, res.NewGB)
	}
	if res.Task == nil {
		t.Error("应产生任务")
	}
	if !res.GuestGrowNeeded {
		t.Error("必须标出「来宾里还要扩分区」——不提醒的话用户会以为扩容失败")
	}

	var vm model.VM
	db.First(&vm, vid)
	if vm.DiskGB != 200 {
		t.Errorf("记录应更新为 200，实际 %d", vm.DiskGB)
	}
}

// TestAbsurdSizeIsRefused 覆盖单位想错。
//
// 一个填错的数字（把 GB 当成 MB）在这里的后果是创建了一个几千 TB 的稀疏
// 文件——它看起来只占一点空间，直到写满。
func TestAbsurdSizeIsRefused(t *testing.T) {
	svc, _, db := newTestEnv(t)
	vid := seedVM(t, db, 100, model.VMStatusStopped)

	for _, bad := range []int{0, -1, 65537} {
		_, err := svc.ResizeDisk(context.Background(), vid, bad,
			authz.Viewer{UserID: 7}, "alice", "10.0.0.1")
		if err == nil {
			t.Errorf("容量 %d 应被拒绝", bad)
		}
	}
}
