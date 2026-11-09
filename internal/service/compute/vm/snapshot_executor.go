package vm

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/service/ops/task"
)

// snapshotCreateParams 是 vm.snapshot.create 任务的参数。
type snapshotCreateParams struct {
	VMID          int64  `json:"vm_id"`
	VMName        string `json:"vm_name"`
	SnapshotID    int64  `json:"snapshot_id"`
	SnapshotName  string `json:"snapshot_name"`
	Kind          string `json:"kind"`
	IncludeMemory bool   `json:"include_memory"`
	// ObservedStatus 是受理时探测到的状态，仅作为排障线索，不参与执行判定。
	ObservedStatus string `json:"observed_status"`
}

// SnapshotCreateExecutor 执行 vm.snapshot.create 任务。
type SnapshotCreateExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewSnapshotCreateExecutor 构造快照创建执行器。
func NewSnapshotCreateExecutor(db *gorm.DB, client agent.Client) *SnapshotCreateExecutor {
	return &SnapshotCreateExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *SnapshotCreateExecutor) Type() string { return model.TaskVMSnapshotCreate }

// Run 下发创建指令并把结果写回快照记录。
func (e *SnapshotCreateExecutor) Run(ctx context.Context, t *model.Task) error {
	var p snapshotCreateParams
	if !decodeParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	params := map[string]any{
		"name":           p.SnapshotName,
		"kind":           p.Kind,
		"include_memory": p.IncludeMemory,
	}
	// 快照名在虚拟化层里由我们自己生成，不直接用用户填的名字：名字可以重复
	// 出现在界面上（跨虚拟机），而虚拟化层的标识一旦建立就不该再变。
	if p.SnapshotID > 0 {
		params["domain_name"] = domainSnapshotName(p.VMName, p.SnapshotID)
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind: agent.OpVMSnapshotCreate, NodeID: *t.NodeID,
		Target: p.VMName, Params: params,
	})
	if err != nil {
		e.fail(ctx, p.SnapshotID, "节点不可达")
		return api.Unavailable("节点不可达，创建快照的指令未送达")
	}
	if !result.Success {
		// **节点明确失败 → 删掉记录**，而不是把它留成一条 error 行。
		//
		// 实机抓出：UEFI 域不支持内部快照，任务失败，但列表里仍留着一条
		// status=ready 的记录，`can_restore/can_delete` 都是 true——用户点进去
		// 恢复会失败、删除也会失败（后者见缺陷「删除快照传错参数键」）。一条
		// 指向"从未存在过的快照"的记录比没有记录更糟。
		//
		// 失败原因不会因此丢失：任务列表与审计流水里有完整的错误信息，而
		// 那才是用户排查的地方；快照列表只该列出真正可用的还原点。
		e.removeRecord(ctx, p.SnapshotID)
		return api.ValidationFailed(result.Message)
	}

	updates := map[string]any{
		"status":     model.SnapshotReady,
		"updated_at": time.Now(),
	}
	if name, ok := result.Data["domain_name"].(string); ok && name != "" {
		updates["domain_name"] = name
	}
	if size, ok := result.Data["size_bytes"].(float64); ok {
		updates["size_bytes"] = int64(size)
	}
	// 受理时记录的 kind 是**预测**（含内存/状态推导），agent 会按虚拟化层
	// 的真实约束执行（如 UEFI 域不支持内部快照，被强制走外部）——按实际
	// 执行结果回写，否则界面上的种类与磁盘上的 overlay 对不上（真机实测：
	// 关机 UEFI 建的快照预测 internal、实际 external）。
	if kind, ok := result.Data["snapshot_kind"].(string); ok && kind != "" {
		if kind == model.SnapshotKindInternal || kind == model.SnapshotKindExternal {
			updates["kind"] = kind
		}
	}

	if err := e.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("id = ?", p.SnapshotID).Updates(updates).Error; err != nil {
		// 宿主侧已成功，只是本地没写上。不返回错误：让任务报失败会误导用户
		// 去重试一个已经存在的快照（重试还会因同名被拒绝，更困惑）。
		log.Printf("[vm] 回写快照创建结果失败 id=%d: %v", p.SnapshotID, err)
	}

	log.Printf("[vm] 快照创建完成 snapshot=%d vm=%d kind=%s", p.SnapshotID, p.VMID, p.Kind)
	return nil
}

// fail 把快照标记为失败。
//
// 只用于**节点不可达**这一类"到底建没建成功说不准"的情况：那时既不能删除
// 记录（节点上可能真的有一份快照），也不能装作成功。标记成 error 让记录
// 停在"待对账"的状态，用户仍可显式删除它。
//
// 节点**明确失败**的路径改用 removeRecord（见 Run 里的说明）。
func (e *SnapshotCreateExecutor) fail(ctx context.Context, id int64, reason string) {
	if id <= 0 {
		return
	}
	err := e.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status": model.SnapshotError, "updated_at": time.Now(),
		}).Error
	if err != nil {
		log.Printf("[vm] 标记快照失败状态出错 id=%d: %v", id, err)
	}
	log.Printf("[vm] 快照创建失败 id=%d: %s", id, reason)
}

// removeRecord 删掉一条**从未真正创建成功**的快照记录。
//
// 与 fail 的分工：节点明确拒绝时快照在虚拟化层里不存在，留着记录只会让
// 界面显示一个点恢复就报错、点删除也可能失败的幽灵条目。
func (e *SnapshotCreateExecutor) removeRecord(ctx context.Context, id int64) {
	if id <= 0 {
		return
	}
	if err := e.db.WithContext(ctx).Delete(&model.VMSnapshot{}, id).Error; err != nil {
		log.Printf("[vm] 移除失败的快照记录出错 id=%d: %v", id, err)
	}
}

// snapshotRestoreParams 是 vm.snapshot.restore 任务的参数。
type snapshotRestoreParams struct {
	VMID         int64  `json:"vm_id"`
	VMName       string `json:"vm_name"`
	SnapshotID   int64  `json:"snapshot_id"`
	SnapshotName string `json:"snapshot_name"`
	DomainName   string `json:"domain_name"`
	// ObservedStatus 与 SnapshotVMStatus 用于判断是否需要先关机。
	ObservedStatus   string `json:"observed_status"`
	SnapshotVMStatus string `json:"snapshot_vm_status"`
}

// SnapshotRestoreExecutor 执行 vm.snapshot.restore 任务。
type SnapshotRestoreExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewSnapshotRestoreExecutor 构造快照恢复执行器。
func NewSnapshotRestoreExecutor(db *gorm.DB, client agent.Client) *SnapshotRestoreExecutor {
	return &SnapshotRestoreExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *SnapshotRestoreExecutor) Type() string { return model.TaskVMSnapshotRestore }

// Run 下发恢复指令。
func (e *SnapshotRestoreExecutor) Run(ctx context.Context, t *model.Task) error {
	var p snapshotRestoreParams
	if !decodeParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind: agent.OpVMSnapshotRestore, NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{
			"domain_name": p.DomainName,
			"snapshot_id": p.SnapshotID,
		},
	})
	if err != nil {
		e.finish(ctx, p.SnapshotID, model.SnapshotReady)
		return api.Unavailable("节点不可达，恢复快照的指令未送达")
	}
	if !result.Success {
		e.finish(ctx, p.SnapshotID, model.SnapshotReady)
		return api.ValidationFailed(result.Message)
	}

	// 恢复成功后快照本身仍然存在（它只是一个还原点），因此状态回到 ready，
	// 并把 is_current 置为 true——虚拟机现在运行在这个快照上。
	//
	// 置 true 而不是让界面去猜：用户需要一眼看出「当前状态对应哪个还原点」，
	// 否则他会在恢复后立刻再恢复一次同一个快照。
	e.finish(ctx, p.SnapshotID, model.SnapshotReady)
	if err := e.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("vm_id = ?", p.VMID).Update("is_current", false).Error; err != nil {
		log.Printf("[vm] 清除旧快照的当前标记失败 vm=%d: %v", p.VMID, err)
	}
	if err := e.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("id = ?", p.SnapshotID).Update("is_current", true).Error; err != nil {
		log.Printf("[vm] 标记当前快照失败 id=%d: %v", p.SnapshotID, err)
	}

	// 恢复会回滚磁盘，虚拟机当前的状态已不可信——标记为 unknown 而不是猜一个：
	// 猜「stopped」会让界面显示已关机，而用户点「开机」时才发现它其实在运行。
	if err := e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", p.VMID).
		Updates(map[string]any{
			"status":         model.VMStatusUnknown,
			"last_synced_at": time.Now(),
		}).Error; err != nil {
		log.Printf("[vm] 重置虚拟机投影状态失败 vm=%d: %v", p.VMID, err)
	}

	log.Printf("[vm] 快照恢复完成 snapshot=%d vm=%d", p.SnapshotID, p.VMID)
	return nil
}

// finish 把快照状态设回某个值（恢复失败时不能让它停在 restoring）。
func (e *SnapshotRestoreExecutor) finish(ctx context.Context, id int64, status string) {
	err := e.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("id = ?", id).
		Updates(map[string]any{"status": status, "updated_at": time.Now()}).Error
	if err != nil {
		log.Printf("[vm] 更新快照状态失败 id=%d: %v", id, err)
	}
}

// snapshotDeleteParams 是 vm.snapshot.delete 任务的参数。
type snapshotDeleteParams struct {
	VMID         int64  `json:"vm_id"`
	VMName       string `json:"vm_name"`
	SnapshotID   int64  `json:"snapshot_id"`
	SnapshotName string `json:"snapshot_name"`
	DomainName   string `json:"domain_name"`
}

// SnapshotDeleteExecutor 执行 vm.snapshot.delete 任务。
type SnapshotDeleteExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewSnapshotDeleteExecutor 构造快照删除执行器。
func NewSnapshotDeleteExecutor(db *gorm.DB, client agent.Client) *SnapshotDeleteExecutor {
	return &SnapshotDeleteExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *SnapshotDeleteExecutor) Type() string { return model.TaskVMSnapshotDelete }

// Run 下发删除指令并移除记录。
func (e *SnapshotDeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	var p snapshotDeleteParams
	if !decodeParams(t, &p) {
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	// 虚拟化层里的快照名，与用户填的显示名**不是一回事**：创建时由
	// domainSnapshotName 生成 kc-snap-<id>（名字可重复出现在界面上，而
	// virsh 的标识一旦建立就应当稳定）。记录里存的是节点回传的值，缺失时
	// 按同一套规则重算。
	//
	// 这里下发错值就等于「删除一个不存在的快照」：virsh 报 not found，
	// 而旧实现把 not find 当幂等成功——控制面删记录、节点快照仍在。
	domainName := strings.TrimSpace(p.DomainName)
	if domainName == "" {
		domainName = domainSnapshotName(p.VMName, p.SnapshotID)
	}
	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind: agent.OpVMSnapshotDelete, NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{
			// 节点按这个名字执行 `virsh snapshot-delete --snapshotname`。
			// 此前没有这个键，节点读到的恒为空串，于是报
			// `Failed to get option 'snapshotname': Option argument is empty`
			// ——删除快照从未真正执行过（实机复现）。
			"snapshot_name": domainName,
			// 兼容旧节点（它读的是 domain_name）；两个键同值是刻意的。
			"domain_name": domainName,
			"snapshot_id": p.SnapshotID,
		},
	})
	if err != nil {
		// 恢复成 ready：删除没送达，这条快照还好好地在那里。
		// 让它停在 deleting 会让界面永远显示「删除中」，而实际什么都没发生。
		e.rollbackStatus(ctx, p.SnapshotID)
		return api.Unavailable("节点不可达，删除快照的指令未送达")
	}
	if !result.Success {
		e.rollbackStatus(ctx, p.SnapshotID)
		return api.ValidationFailed(result.Message)
	}

	// **物理删除记录**，与虚拟机删除的「标记 present=false」不同。
	//
	// 区别在于：虚拟机记录被审计流水与历史任务引用，删掉会让这些引用悬空；
	// 而快照只是一条投影，它消失之后「谁在什么时候删了它」由审计负责记录，
	// 列表里留一条「已删除的快照」只会让人困惑。
	//
	// 若删除失败（比如磁盘上的快照已经不在了），记录同样移除——留一条
	// 永远删不掉的幽灵记录比缺少它更糟。
	if err := e.db.WithContext(ctx).
		Where("id = ?", p.SnapshotID).
		Delete(&model.VMSnapshot{}).Error; err != nil {
		log.Printf("[vm] 移除快照记录失败 id=%d: %v", p.SnapshotID, err)
	}

	log.Printf("[vm] 快照已删除 snapshot=%d vm=%d", p.SnapshotID, p.VMID)
	return nil
}

// rollbackStatus 把状态从 deleting 恢复成 ready。
func (e *SnapshotDeleteExecutor) rollbackStatus(ctx context.Context, id int64) {
	err := e.db.WithContext(ctx).Model(&model.VMSnapshot{}).
		Where("id = ?", id).
		Updates(map[string]any{"status": model.SnapshotReady, "updated_at": time.Now()}).Error
	if err != nil {
		log.Printf("[vm] 回滚快照状态失败 id=%d: %v", id, err)
	}
}

// domainSnapshotName 生成虚拟化层使用的快照标识。
//
// 带上前缀与 ID 而不是直接用用户填的名字：用户可以在不同虚拟机上使用同名
// 快照，也可以随时改名，而虚拟化层的标识一旦建立就应当保持稳定。
func domainSnapshotName(vmName string, id int64) string {
	return vmName + "-snap-" + strconvItoa(id)
}

// decodeParams 解析任务参数。
//
// 抽出来是因为三个快照执行器的开头完全一样，而这段代码在出错时该做什么
// （返回内部错误）也不该由每个执行器各写一遍。
func decodeParams(t *model.Task, dst any) bool {
	if t.Params == nil {
		return false
	}
	if err := json.Unmarshal([]byte(*t.Params), dst); err != nil {
		log.Printf("[vm] 解析 %s 参数失败: %v", t.Type, err)
		return false
	}
	return true
}

func strconvItoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
