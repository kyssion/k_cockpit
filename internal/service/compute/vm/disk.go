package vm

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/service/ops/task"
)

// DiskResizeResult 是扩容结果。
type DiskResizeResult struct {
	Task *model.Task `json:"task"`
	// OldGB / NewGB 是扩容前后的容量。
	OldGB int `json:"old_gb"`
	NewGB int `json:"new_gb"`
	// GuestGrowNeeded 为 true 表示**来宾里还需要扩分区与文件系统**。
	//
	// 必须显式告诉用户：宿主机侧扩完只是"盘子变大了"，而操作系统看到的
	// 仍然是原来的分区。不提醒的话，用户会以为扩容失败。
	GuestGrowNeeded bool `json:"guest_grow_needed"`
}

// ResizeDisk 把虚拟机的系统盘扩容到指定容量。
//
// 三条约束，每一条都对应一类真实的事故：
//
//  1. **只能扩，不能缩。** 缩容会**丢数据**：镜像文件变小之后，文件系统里
//     超出新边界的那些块还在原地，但已经不属于这个设备了——而文件系统
//     自己不知道。它的下一次写入就可能覆盖掉别的东西。这不是"有风险"，
//     是"一定会坏"。
//
//  2. **要求关机。** 运行中的磁盘扩容在宿主机侧是可以做的（热插拔 + 内核
//     重新读取容量），但**来宾里仍然要扩分区**——而那是 guest agent 那条
//     路径会顺带做的事。两条路径各做一半的话，用户得到的是"盘大了但用不了"
//     ——正是这个功能要消灭的那种机器。因此这里直接要求关机，并在报错里
//     **指向另一个入口**。
//
//  3. **宿主机侧扩完不等于可用。** 来宾里的分区与文件系统还要扩，而做法
//     依系统不同。界面上必须把这一步说出来（见 GuestGrowNeeded）。
func (s *Service) ResizeDisk(
	ctx context.Context, id int64, newGB int, v authz.Viewer, operatorName, clientIP string,
) (*DiskResizeResult, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	if newGB <= 0 {
		return nil, api.InvalidParameter("容量必须大于 0")
	}
	// 上限与创建时一致：一个填错的数字（比如把 GB 当成 MB）在这里的后果
	// 是创建了一个几千 TB 的稀疏文件——它看起来只占一点空间，直到写满。
	if newGB > 65536 {
		return nil, api.InvalidParameter("容量不能超过 65536 GB（64 TB）——请确认单位是 GB")
	}

	if newGB == vm.DiskGB {
		// 相同的容量**拒绝**而不是静默成功：一次没有任何变化的扩容会在
		// 节点上跑一遍完整流程，而用户以为自己做了什么。
		return nil, api.ValidationFailed("新容量与当前容量相同")
	}
	if newGB < vm.DiskGB {
		// 见约束一。文案要说清"为什么"，而不只是"不支持"——用户很可能
		// 本来就以为缩容是可以的（在别的地方见过）。
		return nil, api.ValidationFailed(
			"磁盘只能扩大，不能缩小。缩容会让镜像里超出新边界的那些数据" +
				"仍留在原地而文件系统不再知道自己拥有它们——下一次写入就可能" +
				"覆盖别的内容。需要更小的盘请新建一台并用迁移的方式搬过去。")
	}
	if vm.Status == model.VMStatusRunning {
		// 见约束二。**指向另一个入口**：用户想做的事（扩盘）在那边能做，
		// 而只告诉他"要关机"会让他去关机再回来——那也能成，但多了一步，
		// 而且他在这一步里会怀疑自己是不是搞错了。
		return nil, api.Conflict(
			"虚拟机正在运行。运行中的扩容请在「来宾自动化」里用「扩容磁盘」" +
				"——它会在扩完之后**顺带在来宾里扩好文件系统**。这里的入口用于" +
				"关机状态的机器，扩完之后还需要你进来宾自己扩分区。")
	}

	// **先在途检查，再入队**（第 2 轮 NEW-001）。
	//
	// 在此之前，同一台机器的并发扩容会各自入队一个任务，而它们都去改同一张
	// 盘：真机上 10 个并发请求让控制面记着 3 GB，节点上却是 11 GiB。队列的
	// 资源锁只保证这些任务**串行执行**，不阻止它们被创建——于是用户拿到 10
	// 个"已受理"，实际只有第一个能改变容量。
	//
	// 锁把「检查 → 入队」做成一个原子动作：不加锁时两个请求可以同时通过
	// 检查，然后各插一条在途任务，等于没检查。
	resizeGuard.Lock()
	defer resizeGuard.Unlock()

	if busy, err := s.hasActiveResize(ctx, vm.ID); err != nil {
		return nil, err
	} else if busy {
		// 文案要说**下一步做什么**：「已有任务」三个字本身不告诉用户该等
		// 还是该去别处看一眼。
		return nil, api.Conflict("该虚拟机已有扩容任务进行中，请等待完成后再试")
	}

	t, err := s.queue.Enqueue(ctx, task.Spec{
		Type:         model.TaskVMDiskResize,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		OwnerID:      derefOwner(vm.OwnerID),
		CreatedBy:    v.UserID,
		Params: map[string]any{
			"vm_id": vm.ID, "vm_name": vm.Name,
			"old_gb": vm.DiskGB, "new_gb": newGB,
			"disk_format": vm.DiskFormat,
		},
	})
	if err != nil {
		// **不要把入队错误吞成 Internal。**
		//
		// 入队失败有一类明确的原因（任务类型没注册、参数不合法），而它们
		// 的原始文案是能直接指出问题的。换成一句「服务内部错误」之后，
		// 用户与排查的人都只能看到"失败了"，而真正的原因被丢掉了——
		// 这个错误就是在写这段代码时踩到的：任务类型没注册，本该得到
		// 「不支持的任务类型」，实际得到的是「服务内部错误」。
		return nil, err
	}

	// **这里不再预写 disk_gb。**
	//
	// 旧实现在入队之后立刻把 disk_gb 更新成 newGB，理由是"让界面马上看到
	// 新容量"。那不是投影，是猜测：节点上那个镜像文件此刻还是原来的大小，
	// 而 10 个并发请求会依次覆盖，最后一个说了算——于是控制面记着一个谁都
	// 没实现过的数字。真实容量由执行器在节点确认之后写（见
	// DiskResizeExecutor.Run），并由采集器的域扫描持续对账。

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm",
		ResourceID: vm.ID, ResourceName: vm.Name,
		Action: "vm.disk.resize",
		Params: map[string]any{
			"old_gb": vm.DiskGB, "new_gb": newGB,
			"note": "仅扩宿主机侧；来宾内分区与文件系统需自行扩展",
		},
		Success: true, ClientIP: clientIP,
	})

	return &DiskResizeResult{
		Task: t, OldGB: vm.DiskGB, NewGB: newGB,
		// **由服务端给出而不是让界面自己填一句固定文案**：这一步是否必要
		// 取决于扩容路径，而那是服务端才知道的事。
		GuestGrowNeeded: true,
	}, nil
}

// resizeGuard 串行化「检查在途扩容 → 入队」这一小段临界区。
//
// 它保护的**不是**磁盘操作本身（那是队列的资源锁与节点侧的事），而是控制面
// 上的一个检查-然后-写入窗口：两个并发请求可以同时看到"没有在途任务"，然后
// 各插一条，检查就白做了。临界区里只有一次 COUNT 和一次 INSERT，代价可以
// 忽略；真正耗时的部分都在队列里跑。
//
// 单进程部署下这把锁够用。多实例部署需要的是数据库层的唯一约束（"同一 VM
// 同时只能有一条在途扩容任务"），那不在本次范围内。
var resizeGuard sync.Mutex

// hasActiveResize 报告该虚拟机是否已有在途的扩容任务。
//
// unknown 也算在途：它表示「执行中但结果未知」，节点上那张盘可能正在被
// qemu-img 改写。此时放行第二个任务，等于让两个 qemu-img 同时操作同一个
// 镜像文件——那不只是数字不对，是可能把盘写坏。队列的资源锁同样把 unknown
// 视为占用（见 task.hasRunningOn），这里与它保持一致。
func (s *Service) hasActiveResize(ctx context.Context, vmID int64) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&model.Task{}).
		Where("type = ? AND resource_type = ? AND resource_id = ? AND status IN ?",
			model.TaskVMDiskResize, "vm", vmID,
			[]string{model.TaskPending, model.TaskRunning, model.TaskUnknown}).
		Count(&n).Error
	if err != nil {
		// 查不到时**不放行**：在无法确认的情况下再下发一次改盘操作，
		// 是这里能犯的最严重的错误。
		log.Printf("[vm] 查询在途扩容任务失败 vm=%d: %v", vmID, err)
		return false, api.Internal()
	}
	return n > 0, nil
}

// DiskResizeExecutor 执行关机状态下的磁盘扩容。
type DiskResizeExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewDiskResizeExecutor 构造执行器。
func NewDiskResizeExecutor(db *gorm.DB, client agent.Client) *DiskResizeExecutor {
	return &DiskResizeExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *DiskResizeExecutor) Type() string { return model.TaskVMDiskResize }

// Run 下发扩容，并在节点确认之后把**真实**容量写回投影。
//
// 写回是这里唯一的目的地：disk_gb 不是控制面自己的配置，而是节点事实的
// 投影。入队时预写（旧实现）与节点回传之间隔着一次真实的 qemu-img，中间
// 任何一步都可能不成立——所以只有执行完、拿到节点实测结果，这个数字才能
// 被写下去。
func (e *DiskResizeExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析扩容参数失败: %v", err)
		return api.Internal()
	}
	nodeID := int64(0)
	if t.NodeID != nil {
		nodeID = *t.NodeID
	}
	requestedGB, hasRequested := numberAsInt(p["new_gb"])
	oldGB, hasOld := numberAsInt(p["old_gb"])

	result, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMDiskResize,
		NodeID: nodeID,
		Target: strOf(p["vm_name"]),
		Params: p,
	})
	if err != nil {
		return api.Unavailable("节点不可达，磁盘未扩容")
	}
	if !result.Success {
		// **没有预写，所以没有要回滚的东西**（第 2 轮 NEW-001）。
		//
		// 旧实现在这里把 disk_gb 从 new_gb 改回 old_gb——那一步的前提正是
		// 入队时的预写。预写取消之后这段回滚不只多余，而是有害的：只要
		// disk_gb 恰好等于 new_gb（用户先手工改过一次、或采集器已经对账到
		// 这个值），一次失败的任务就会把一个**正确**的容量改回旧值，制造出
		// 一个与节点不符的投影——正是这条缺陷的表现本身。
		return api.ValidationFailed(result.Message)
	}

	realGB, ok := diskResizeOutcome(result, requestedGB, hasRequested)
	if !ok {
		msg := strings.TrimSpace(result.Message)
		if msg == "" {
			msg = "节点未确认扩容结果，磁盘容量未变更"
		}
		return api.ValidationFailed(msg)
	}

	e.writeBack(ctx, t, realGB, oldGB, hasOld)
	return nil
}

// diskResizeOutcome 从节点结果里取「真实的扩容结果」。
//
// 优先用节点回传的 NewGB：那是它在 qemu-img 之后**实测**过的容量
// （见 nodeagent.vmDiskResize 的 QemuImgSize 校验），而不是我们请求的值。
// 并发扩容时请求值恰恰是最不可信的那个——10 个请求里只有第一个真正改变了
// 文件，后面的目标值都比文件当前容量还小。
//
// ok 为 false 表示「没有任何可信的结论」，调用方**不得**写投影。
func diskResizeOutcome(result *agent.Result, requestedGB int, hasRequested bool) (int, bool) {
	info, has := result.Data[agent.VMDiskDataKey].(agent.VMDiskResizeInfo)
	if !has {
		// 节点没回 vm_disk（旧版 agent，或解码器未登记）：退回请求值。
		//
		// 这是唯一一处「相信请求而不是实测」的地方，因此必须留日志——
		// 否则将来排查容量不符时，看不出这一次到底是实测还是猜的。
		if !hasRequested {
			return 0, false
		}
		log.Printf("[vm] 节点未返回扩容结果，按请求值回写 disk_gb=%d", requestedGB)
		return requestedGB, true
	}
	if !info.Applied {
		// 节点明确说「没扩」。此时写投影会把一次未发生的扩容记成事实。
		return 0, false
	}
	if info.NewGB <= 0 {
		// 扩了但没报容量：无法确认真实值，宁可不写，等采集器的域扫描收敛。
		return 0, false
	}
	return info.NewGB, true
}

// writeBack 用**条件更新**把真实容量写回投影。
//
// 条件里带 old_gb（入队那一刻的容量）：扩容期间 disk_gb 可能已经被另一条
// 路径改过——采集器的域扫描对账出真实容量，或用户又发起过一次扩容。无条件
// Update 会把这些更正确的值覆盖成一个基于旧假设的值。
//
// RowsAffected == 0 表示「记录已经不是预期的样子」：此时**不覆盖**，交给
// 采集器收敛。节点侧确实扩了，所以任务本身仍然算成功——把任务判失败会让
// 用户以为盘没变大，从而再操作一次。
//
// 写库失败同样只记日志：节点上的盘已经变大了，把任务标成失败是错的结论，
// 而投影会在下一轮域扫描（≤70s）里被纠正。
func (e *DiskResizeExecutor) writeBack(
	ctx context.Context, t *model.Task, realGB, oldGB int, hasOld bool,
) {
	var vmID int64
	if t.ResourceID != nil {
		vmID = *t.ResourceID
	}
	if vmID == 0 {
		log.Printf("[vm] 任务 %d 缺少资源 ID，无法回写磁盘容量", t.ID)
		return
	}

	q := e.db.WithContext(ctx).Model(&model.VM{}).Where("id = ?", vmID)
	if hasOld {
		q = q.Where("disk_gb = ?", oldGB)
	}
	res := q.Update("disk_gb", realGB)
	if res.Error != nil {
		log.Printf("[vm] 回写磁盘容量失败 vm=%d new=%d: %v", vmID, realGB, res.Error)
		return
	}
	if res.RowsAffected == 0 && hasOld {
		// 投影已被别的路径改过：日志里要带上 old/new，否则看不出来是
		// 「谁赢了」——这正是这次缺陷最难查的那一部分。
		log.Printf("[vm] 磁盘容量已被改动，跳过回写 vm=%d 期望旧值=%d 实测新值=%d",
			vmID, oldGB, realGB)
	}
}

func numberAsInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		return int(x), true
	}
	return 0, false
}

// derefOwner 把可空的归属转成任务入队需要的值（0 表示无归属）。
func derefOwner(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}
