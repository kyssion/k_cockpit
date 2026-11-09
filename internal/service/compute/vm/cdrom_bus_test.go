package vm_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
)

// busClient 让 vm.cdrom.apply 可控：可以拦住不下发（用于断言受理阶段的库状态）、
// 也可以让它失败，并记录每次下发的参数。
type busClient struct {
	*stubClient
	mu      sync.Mutex
	ops     []agent.Operation
	release chan struct{}
	fail    bool
}

func (c *busClient) Execute(ctx context.Context, op agent.Operation) (*agent.Result, error) {
	if op.Kind != agent.OpVMCDROMApply {
		return c.stubClient.Execute(ctx, op)
	}
	c.mu.Lock()
	c.ops = append(c.ops, op)
	release, fail := c.release, c.fail
	c.mu.Unlock()

	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fail {
		// 与节点侧失败时的形状一致：Result 失败 + 可读原因。
		return &agent.Result{Success: false, Message: "摘除旧总线的光驱（hdc）失败: device not found"}, nil
	}
	return &agent.Result{Success: true}, nil
}

func (c *busClient) setRelease(ch chan struct{}) {
	c.mu.Lock()
	c.release = ch
	c.mu.Unlock()
}

func (c *busClient) opCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ops)
}

func (c *busClient) lastOp(t *testing.T) agent.Operation {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ops) == 0 {
		t.Fatal("没有下发 vm.cdrom.apply")
	}
	return c.ops[len(c.ops)-1]
}

// TestBusChangeRejectsIDEOnVirtMachine 覆盖受理阶段的机型预判。
//
// virt 机型（aarch64 常用）没有 IDE 控制器：节点会照单全收写进定义，直到
// 开机才报 `IDE controllers are unsupported for this QEMU binary or machine
// type`——等于把机器变成开不了机。受理阶段先拒，用户不用等一个必然失败的
// 任务；节点侧还有一道兜底（见 nodeagent 的 ensureBusSupportedByMachine）。
func TestBusChangeRejectsIDEOnVirtMachine(t *testing.T) {
	client := &busClient{stubClient: newStubClient()}
	svc, db, vmID, isoID := cdromEnvWithClient(t, client)
	ctx := context.Background()

	// 把机改成 virt（aarch64 上的常见选择）。
	if err := db.Model(&model.VM{}).Where("id = ?", vmID).
		Update("machine_type", "virt").Error; err != nil {
		t.Fatalf("写入机型失败: %v", err)
	}
	row := model.VMCDROM{VMID: vmID, NodeID: 1, OrderNo: 0, Bus: model.CDROMBusSATA, ISOFileID: &isoID}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建光驱记录失败: %v", err)
	}

	_, err := svc.SetCDROMBus(ctx, vmID, row.ID, model.CDROMBusIDE, viewer7(), "a", "")
	assertStatus(t, err, 422)
	if !strings.Contains(err.Error(), "IDE") {
		t.Errorf("拒绝原因应指向 IDE 控制器不受支持: %v", err)
	}
	// 不能留下任何任务：这条请求必然失败，入队只会产生一条无意义的失败记录。
	var tasks int64
	db.Model(&model.Task{}).Where("type = ?", model.TaskVMCDROMApply).Count(&tasks)
	if tasks != 0 {
		t.Errorf("机型不支持时不该入队，实际 %d 条任务", tasks)
	}
	// sata/scsi 不受机型限制，必须仍可用。
	if _, err := svc.SetCDROMBus(ctx, vmID, row.ID, model.CDROMBusSCSI, viewer7(), "a", ""); err != nil {
		t.Errorf("virt 机型换到 scsi 应被接受: %v", err)
	}
}

// TestBusChangeKeepsRecordUntilNodeSucceeds 覆盖「换总线失败但库已改」。
//
// 实机现象：PUT bus=ide 返回 200 + 任务，任务最终 failed（错误是「换总线
// 需要先移除再重新挂载」），而 `vm_cdrom.bus` 在入队**之前**就被改成了 ide
// ——界面显示 ide、节点上仍是 sata，之后所有基于记录的判断都错在一个无人
// 察觉的地方。
//
// 要求：受理时不动记录；节点回报成功之后才改。
func TestBusChangeKeepsRecordUntilNodeSucceeds(t *testing.T) {
	client := &busClient{stubClient: newStubClient()}
	svc, db, vmID, isoID := cdromEnvWithClient(t, client)
	ctx := context.Background()

	// 把一个「sata 总线、已放盘」的光驱写进库（不经 attach 任务）。
	row := model.VMCDROM{VMID: vmID, NodeID: 1, OrderNo: 0, Bus: model.CDROMBusSATA, ISOFileID: &isoID}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建光驱记录失败: %v", err)
	}

	// 拦住下发：这样"受理后库里是什么"是确定的，不会被执行器抢跑。
	release := make(chan struct{})
	client.setRelease(release)

	if _, err := svc.SetCDROMBus(ctx, vmID, row.ID, model.CDROMBusIDE, viewer7(), "a", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	var accepted model.VMCDROM
	if err := db.First(&accepted, row.ID).Error; err != nil {
		t.Fatalf("读取光驱失败: %v", err)
	}
	if accepted.Bus != model.CDROMBusSATA {
		t.Fatalf("受理阶段就把记录改成了 %q：节点还没执行，此时库与域定义必然分叉", accepted.Bus)
	}

	// 下发的参数：目标总线、旧总线、光驱 ID、原介质都要带齐。
	// 等执行器真正把指令发出去（队列有 20ms 轮询间隔），再读参数。
	waitFor(t, "光驱指令下发", func() bool { return client.opCount() > 0 })
	op := client.lastOp(t)
	if got, _ := op.Params["action"].(string); got != "bus" {
		t.Errorf("action = %v, 期望 bus", op.Params["action"])
	}
	if got, _ := op.Params["bus"].(string); got != model.CDROMBusIDE {
		t.Errorf("bus = %v, 期望目标总线 ide", op.Params["bus"])
	}
	if got, _ := op.Params["from_bus"].(string); got != model.CDROMBusSATA {
		t.Errorf("from_bus = %v, 期望旧总线 sata（节点靠它算旧设备名）", op.Params["from_bus"])
	}
	if got, _ := op.Params["iso_path"].(string); got != "iso/ubuntu.iso" {
		t.Errorf("iso_path = %v, 期望换总线时保留原介质", op.Params["iso_path"])
	}

	// 放行后，执行器把新总线写回记录。
	close(release)
	waitFor(t, "换总线回写", func() bool {
		var got model.VMCDROM
		if err := db.First(&got, row.ID).Error; err != nil {
			return false
		}
		return got.Bus == model.CDROMBusIDE
	})
}

// TestBusChangeFailureKeepsOldRecord 覆盖失败分支。
//
// 节点没换成功时记录必须留在旧值：改成新值会让界面显示一台「已是 ide」的
// 机器，而域里还是 sata——用户再去点一次会得到「总线类型没有变化」。
func TestBusChangeFailureKeepsOldRecord(t *testing.T) {
	client := &busClient{stubClient: newStubClient(), fail: true}
	svc, db, vmID, isoID := cdromEnvWithClient(t, client)
	ctx := context.Background()

	row := model.VMCDROM{VMID: vmID, NodeID: 1, OrderNo: 0, Bus: model.CDROMBusSATA, ISOFileID: &isoID}
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("创建光驱记录失败: %v", err)
	}

	if _, err := svc.SetCDROMBus(ctx, vmID, row.ID, model.CDROMBusIDE, viewer7(), "a", ""); err != nil {
		t.Fatalf("受理失败: %v", err)
	}

	waitFor(t, "换总线任务失败", func() bool {
		var tk model.Task
		if err := db.Where("type = ?", model.TaskVMCDROMApply).First(&tk).Error; err != nil {
			return false
		}
		return tk.Status == model.TaskFailed
	})

	var got model.VMCDROM
	if err := db.First(&got, row.ID).Error; err != nil {
		t.Fatalf("读取光驱失败: %v", err)
	}
	if got.Bus != model.CDROMBusSATA {
		t.Errorf("任务失败后总线 = %q，期望保持旧值 sata", got.Bus)
	}
}
