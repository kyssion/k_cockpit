package vm

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/model"
	"k_cockpit/internal/task"
)

// createParams 是 vm.create 任务的参数。
//
// 它刻意与 vm.CreateRequest 分开：前者是**已入队并持久化到 task.params 的
// 任务参数**，后者是接口请求。两者字段相近但生命周期不同——共用结构会让
// 「接口加一个字段」意外影响历史任务的反序列化。
type createParams struct {
	Name      string `json:"name"`
	NodeID    int64  `json:"node_id"`
	VCPU      int    `json:"vcpu"`
	MemoryMB  int    `json:"memory_mb"`
	DiskGB    int    `json:"disk_gb"`
	Remark    string `json:"remark"`
	GroupName string `json:"group_name"`
	OwnerID   int64  `json:"owner_id"`

	// --- 创建向导的硬件与系统配置（f-2-02）---
	//
	// 键名与矩阵（editFields）一致：这份参数是「那一次创建填了什么」，
	// 而矩阵是「允许填什么」，两者对齐之后校验才能复用同一套规则。

	DiskFormat string `json:"disk_format,omitempty"`
	DiskBus    string `json:"disk_bus,omitempty"`
	NicModel   string `json:"nic_model,omitempty"`
	OSType     string `json:"os_type,omitempty"`
	OSVariant  string `json:"os_variant,omitempty"`
	// Hostname / InitialPassword / InitMode / StaticIP：第一次开机就该是
	// 什么样。建好再设要走来宾自动化，而那要求 Guest Agent 已在运行——
	// 对一个刚装好的系统不成立，因此随创建一起下发。
	Hostname        string `json:"hostname,omitempty"`
	InitialPassword string `json:"initial_password,omitempty"`
	InitMode        string `json:"init_mode,omitempty"`
	StaticIP        string `json:"static_ip,omitempty"`
	// DataDisks 是除系统盘之外要一并建立的磁盘。
	DataDisks       []DataDiskSpec `json:"data_disks,omitempty"`
	MachineType     string         `json:"machine_type,omitempty"`
	Firmware        string         `json:"firmware,omitempty"`
	SecureBoot      bool           `json:"secure_boot"`
	BootOrder       string         `json:"boot_order,omitempty"`
	AutoStart       bool           `json:"auto_start"`
	Watchdog        string         `json:"watchdog,omitempty"`
	CPUType         string         `json:"cpu_type,omitempty"`
	CPULimitPercent int            `json:"cpu_limit_percent"`
	APIC            bool           `json:"apic"`
	PAE             bool           `json:"pae"`
	FreezeOnStart   bool           `json:"freeze_on_start"`
	DiskIOPSTotal   int            `json:"disk_iops_total"`
	DiskIOPSRead    int            `json:"disk_iops_read"`
	DiskIOPSWrite   int            `json:"disk_iops_write"`

	// ISOFileID 非零表示创建后把该镜像挂到光驱（ISO 安装路径）。
	ISOFileID int64 `json:"iso_file_id,omitempty"`
	// SwitchID / SecurityGroupIDs 描述主网口（order 0）。
	SwitchID         int64   `json:"switch_id,omitempty"`
	SecurityGroupIDs []int64 `json:"security_group_ids,omitempty"`

	// BatchKey 只用于任务中心的分组展示，不参与下发。
	BatchKey string `json:"batch_key,omitempty"`

	// --- 从模板克隆（f-3-02）；TemplateID 为零表示从零安装 ---

	TemplateID int64  `json:"template_id,omitempty"`
	CloneMode  string `json:"clone_mode,omitempty"`
	// TemplateDiskPath 是模板盘在宿主机上的路径，随任务持久化。
	//
	// 不在这里回查模板表：任务可能排很久才执行，那时模板已被删除或改名，
	// 回查会得到空值或另一份路径。**入队那一刻的路径才是这次要用的**。
	TemplateDiskPath string `json:"template_disk_path,omitempty"`
}

// newCreateParams 由创建请求构造任务参数。
//
// 单独一个函数而不是在 CreateBatch 里就地拼：参数里有**默认值兜底**
// （APIC / PAE 未提交时取 true）与模板推导（磁盘不小于模板），这两件事
// 在受理时定下来并随任务持久化，执行阶段就不必再读模板表——任务可能排
// 很久，那时模板早就被删了。
func newCreateParams(
	req CreateRequest, name string, vcpu, memoryMB, diskGB int, tpl *model.Template, ownerID int64,
) createParams {
	p := createParams{
		Name: name, NodeID: req.NodeID, VCPU: vcpu, MemoryMB: memoryMB, DiskGB: diskGB,
		Remark: req.Remark, GroupName: req.GroupName, OwnerID: ownerID,

		DiskFormat: req.DiskFormat, DiskBus: req.DiskBus, NicModel: req.NicModel,
		OSType: req.OSType, OSVariant: req.OSVariant,
		MachineType: req.MachineType, Firmware: req.Firmware,
		// 第一次开机就该是什么样：随创建一起下发，建好再设要走来宾自动化，
		// 而那要求 Guest Agent 已在运行——对刚装好的系统不成立。
		Hostname:        req.Hostname,
		InitialPassword: req.InitialPassword,
		InitMode:        req.InitMode,
		StaticIP:        req.StaticIP,
		DataDisks:       req.DataDisks,
		SecureBoot:      req.SecureBoot, BootOrder: req.BootOrder, AutoStart: req.AutoStart,
		Watchdog: req.Watchdog, CPUType: req.CPUType, CPULimitPercent: req.CPULimitPercent,
		// 未提交时取 true：它们默认是开着的，而 bool 的零值无法区分
		// 「用户关掉了」与「用户没填」。用指针表达三态是这个字段唯一
		// 正确的方式，代价只是调用方多判一次空。
		APIC: true, PAE: true,
		FreezeOnStart:    req.FreezeOnStart,
		DiskIOPSTotal:    req.DiskIOPSTotal,
		DiskIOPSRead:     req.DiskIOPSRead,
		DiskIOPSWrite:    req.DiskIOPSWrite,
		ISOFileID:        req.ISOFileID,
		SwitchID:         req.SwitchID,
		SecurityGroupIDs: req.SecurityGroupIDs,
		BatchKey:         req.BatchKey,
	}
	if req.APIC != nil {
		p.APIC = *req.APIC
	}
	if req.PAE != nil {
		p.PAE = *req.PAE
	}
	if tpl != nil {
		p.TemplateID = tpl.ID
		p.CloneMode = req.CloneMode
		p.TemplateDiskPath = tpl.DiskPathOf()
	}
	return p
}

// CreateExecutor 执行 vm.create 任务。
type CreateExecutor struct {
	db    *gorm.DB
	agent agent.Client
	// encKey 用于加密保存创建时给定的初始登录密码（R-005：只写不读）。
	encKey []byte
}

// NewCreateExecutor 构造创建执行器。
func NewCreateExecutor(db *gorm.DB, client agent.Client) *CreateExecutor {
	return &CreateExecutor{db: db, agent: client}
}

// SetEncryptionKey 设置凭据加密密钥；不设置时初始凭据不落库（虚拟机照常创建）。
func (e *CreateExecutor) SetEncryptionKey(key []byte) { e.encKey = key }

// Type 返回处理的任务类型。
func (e *CreateExecutor) Type() string { return model.TaskVMCreate }

// Run 下发创建指令并写入投影。
//
// 顺序很重要：**先让虚拟化层创建成功，再写投影**。反过来的话，创建失败时
// 会留下一条并不存在的虚拟机记录——用户看到它、点进去操作、然后收到
// 「不存在」，而原因早已无从查起。
func (e *CreateExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}

	var p createParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析创建参数失败: %v", err)
		return api.Internal()
	}

	// 有模板就走克隆，没有就从零安装。
	//
	// 两者在控制面是**同一个任务类型、同一套受理逻辑**（对用户来说「从模板
	// 建一台机器」与「新建一台机器」是同一件事），只在最后下发时分开——
	// 节点侧也只需要实现「给我一块盘，从它派生」，而不必理解模板的概念。
	op := agent.Operation{
		Kind:   agent.OpVMCreate,
		NodeID: p.NodeID,
		Target: p.Name,
		Params: map[string]any{
			"vcpu":      p.VCPU,
			"memory_mb": p.MemoryMB,
			"disk_gb":   p.DiskGB,

			// 下面这批是**创建时选的硬件与系统配置**（f-2-02）。随指令一次
			// 下发而不是创建后再逐项改：后者会产生一条「先按默认建好、再改」
			// 的中间态，而那台机器在中间态里是**可以被引导的**——用户看到
			// 它起来了，进去装系统，然后配置被后续改动覆盖。
			"disk_format": p.DiskFormat,
			"disk_bus":    p.DiskBus,
			"os_type":     p.OSType,
			"os_variant":  p.OSVariant,
			// 首次开机的样子：主机名、初始凭据、初始化方式、静态地址。
			"hostname":          p.Hostname,
			"initial_password":  p.InitialPassword,
			"init_mode":         p.InitMode,
			"static_ip":         p.StaticIP,
			"data_disks":        p.DataDisks,
			"machine_type":      p.MachineType,
			"firmware":          p.Firmware,
			"secure_boot":       p.SecureBoot,
			"boot_order":        p.BootOrder,
			"auto_start":        p.AutoStart,
			"watchdog":          p.Watchdog,
			"cpu_type":          p.CPUType,
			"cpu_limit_percent": p.CPULimitPercent,
			"apic":              p.APIC,
			"pae":               p.PAE,
			"freeze_on_start":   p.FreezeOnStart,
			"nic_model":         p.NicModel,
			"disk_iops_total":   p.DiskIOPSTotal,
			"disk_iops_read":    p.DiskIOPSRead,
			"disk_iops_write":   p.DiskIOPSWrite,
		},
	}
	if p.TemplateID > 0 {
		op.Kind = agent.OpVMClone
		op.Params["template_id"] = p.TemplateID
		op.Params["clone_mode"] = p.CloneMode
		op.Params["template_disk_path"] = p.TemplateDiskPath
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, op)
	if err != nil {
		// 指令未送达与执行失败是两回事，但对用户而言都需要一个可操作的说法。
		return api.Unavailable("节点不可达，创建指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	now := time.Now()
	vm := model.VM{
		NodeID:    p.NodeID,
		Name:      p.Name,
		VCPU:      p.VCPU,
		MemoryMB:  p.MemoryMB,
		DiskGB:    p.DiskGB,
		OwnerID:   optID(p.OwnerID),
		Remark:    optStr(p.Remark),
		GroupName: optStr(p.GroupName),

		// 状态以 agent 返回为准；拿不到时记为 unknown 而**不是**猜一个
		// running——猜错会让界面显示「运行中」，而用户点关机才发现它没起来。
		Status:       model.VMStatusUnknown,
		Present:      true,
		LastSyncedAt: &now,

		// 创建向导选定的配置（f-2-02）。留空的项由数据库默认值兜底，
		// 与矩阵里的 Default 保持一致。
		DiskFormat: orDefault(p.DiskFormat, "qcow2"),
		DiskBus:    orDefault(p.DiskBus, "virtio"),
		NicModel:   orDefault(p.NicModel, "virtio"),
		OSType:     orDefault(p.OSType, "linux"),
		// 具体版本可为空：多数机器是从镜像装的，只有识别过或手选才有值。
		OSVariant:       p.OSVariant,
		MachineType:     orDefault(p.MachineType, "q35"),
		Firmware:        orDefault(p.Firmware, "bios"),
		BootOrder:       orDefault(p.BootOrder, "disk,cdrom,network"),
		Watchdog:        orDefault(p.Watchdog, "none"),
		CPUType:         orDefault(p.CPUType, "host"),
		SecureBoot:      p.SecureBoot,
		AutoStart:       p.AutoStart,
		CPULimitPercent: p.CPULimitPercent,
		APIC:            p.APIC,
		PAE:             p.PAE,
		FreezeOnStart:   p.FreezeOnStart,
		DiskIOPSTotal:   p.DiskIOPSTotal,
		DiskIOPSRead:    p.DiskIOPSRead,
		DiskIOPSWrite:   p.DiskIOPSWrite,
	}
	if p.TemplateID > 0 {
		vm.TemplateID = &p.TemplateID
		vm.CloneMode = p.CloneMode
		if vm.CloneMode == "" {
			vm.CloneMode = model.CloneFull
		}
		// 链式克隆的父盘路径由**节点返回**，不按模板路径推算：节点侧
		// 可能为了性能把模板盘放到别处（比如 SSD 缓存层），由节点说了算
		// 才不会对不上。它是排查「克隆机起不来」时第一个要看的东西。
		if info, ok := result.Data[agent.CloneDataKey].(agent.CloneInfo); ok && info.BackingPath != "" {
			vm.BackingPath = &info.BackingPath
		}
	} else {
		vm.CloneMode = model.CloneFull
	}

	if uuid, ok := result.Data["uuid"].(string); ok && uuid != "" {
		vm.UUID = &uuid
	}
	if status, ok := result.Data["status"].(string); ok && status != "" {
		vm.Status = status
	}

	if err := e.db.WithContext(ctx).Create(&vm).Error; err != nil {
		// 同名冲突是最常见的失败：给出可操作的原因，而不是笼统的 500。
		if isDuplicateKey(err) {
			return api.Conflict("同名虚拟机已存在")
		}
		log.Printf("[vm] 写入虚拟机投影失败: %v", err)
		return api.Internal()
	}

	// 主网口：不建的话这台机器在控制面里「没有网络」——详情页的网络标签
	// 是空的，静态地址与端口转发也无从绑定（它们都挂在网口上）。
	if err := e.createPrimaryInterface(ctx, &vm, p); err != nil {
		// 网口没建成**不算创建失败**：虚拟机已经能用，缺的只是控制面记录。
		// 把它当失败会让一台实际可用的机器被标成失败，而用户更可能想去
		// 删掉重来——那才真的丢了东西。
		log.Printf("[vm] 写入主网口失败 vm=%d: %v", vm.ID, err)
	}

	// ISO 安装：把镜像挂到光驱，否则新机器空盘无法引导。
	if p.ISOFileID > 0 {
		if err := e.attachISO(ctx, &vm, p.ISOFileID); err != nil {
			log.Printf("[vm] 挂载安装镜像失败 vm=%d: %v", vm.ID, err)
		}
	}

	// 初始凭据：随创建一起写下来宾记录，详情页才能展示"这台机器的登录
	// 凭据是什么"。与控制台密码一样**只写不读**（R-005）——接口不返回
	// 明文，只能重设。
	if p.InitialPassword != "" {
		e.saveInitialCredential(ctx, &vm, p)
	}

	log.Printf("[vm] 已创建虚拟机 id=%d name=%s node=%d task=%d", vm.ID, vm.Name, vm.NodeID, t.ID)
	return nil
}

// saveInitialCredential 加密保存创建时给定的初始登录密码。
//
// 失败只记日志：虚拟机已经建好了，为"凭据没记下来"把整个任务判失败会让
// 用户以为创建没成功，而那恰恰是他最不该误判的一件事。
func (e *CreateExecutor) saveInitialCredential(ctx context.Context, vm *model.VM, p createParams) {
	if e.encKey == nil {
		log.Printf("[vm] 未配置加密密钥，跳过初始凭据 vm=%d", vm.ID)
		return
	}
	sealed, err := cryptoutil.Seal(e.encKey, p.InitialPassword)
	if err != nil {
		log.Printf("[vm] 加密初始密码失败 vm=%d: %v", vm.ID, err)
		return
	}
	username := initialUsername(orDefault(p.OSType, "linux"))
	row := model.VMCredential{
		VMID:        vm.ID,
		Username:    &username,
		PasswordEnc: sealed,
	}
	if err := e.db.WithContext(ctx).Create(&row).Error; err != nil {
		log.Printf("[vm] 写入初始凭据失败 vm=%d: %v", vm.ID, err)
	}
}

// createPrimaryInterface 写入主网口与安全组关联。
func (e *CreateExecutor) createPrimaryInterface(
	ctx context.Context, vm *model.VM, p createParams,
) error {
	nic := model.VMInterface{
		VMID:      vm.ID,
		NodeID:    vm.NodeID,
		Order:     0,
		IsPrimary: true,
		Model:     vm.NicModel,
	}
	if p.SwitchID > 0 {
		nic.SwitchID = &p.SwitchID
	}
	if len(p.SecurityGroupIDs) > 0 {
		nic.SecurityGroupID = &p.SecurityGroupIDs[0]
	}
	if err := e.db.WithContext(ctx).Create(&nic).Error; err != nil {
		return err
	}
	for _, gid := range p.SecurityGroupIDs {
		row := model.InterfaceSecurityGroup{InterfaceID: nic.ID, GroupID: gid}
		// 重复挂载同一组不算错误：唯一索引会拦，而这里的语义是「确保挂上」。
		if err := e.db.WithContext(ctx).
			Where(model.InterfaceSecurityGroup{InterfaceID: nic.ID, GroupID: gid}).
			FirstOrCreate(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

// attachISO 把镜像挂到新虚拟机的第一个光驱。
//
// 与详情页的「挂载」走同一个 agent 操作、同一张表：创建时的这一次挂载
// 不是特殊路径，它就是一次普通的挂载，只是发生在机器刚建好时。
func (e *CreateExecutor) attachISO(ctx context.Context, vm *model.VM, fileID int64) error {
	isoID := fileID
	row := model.VMCDROM{
		VMID: vm.ID, NodeID: vm.NodeID, OrderNo: 0,
		ISOFileID: &isoID, Bus: model.CDROMBusSATA,
	}
	if err := e.db.WithContext(ctx).Create(&row).Error; err != nil {
		return err
	}
	_, err := e.agent.Execute(ctx, agent.Operation{
		Kind:   agent.OpVMCDROMApply,
		NodeID: vm.NodeID,
		Target: vm.Name,
		Params: map[string]any{
			"action": "attach", "vm_id": vm.ID, "vm_name": vm.Name,
			"order_no": 0, "bus": row.Bus, "iso_file_id": isoID,
		},
	})
	return err
}

// orDefault 在值为空时取默认值。
func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// powerParams 是 vm.power 任务的参数。
type powerParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	Action string `json:"action"`
	// ObservedStatus 是受理时探测到的状态，用于事后区分「受理时探测结果与
	// 投影不一致」与「执行时状态已变」两种情况。它**不参与执行判定**：
	// 判定已在受理时完成，这里只作为排障线索。
	ObservedStatus string `json:"observed_status"`
}

// PowerExecutor 执行 vm.power 任务。
type PowerExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewPowerExecutor 构造电源执行器。
func NewPowerExecutor(db *gorm.DB, client agent.Client) *PowerExecutor {
	return &PowerExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *PowerExecutor) Type() string { return model.TaskVMPower }

// Run 下发电源指令并回写投影状态。
//
// 这里**不重复探测**：状态合法性已在受理时校验（Service.Power），而任务的
// 资源锁保证同一虚拟机的操作串行执行，不会插入其他操作。再次探测只会把
// 竞态窗口从「受理到执行」扩大到「两次探测之间」，得不偿失。
func (e *PowerExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}

	var p powerParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析电源参数失败: %v", err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	action := PowerAction(p.Action)

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   action.Op(),
		NodeID: *t.NodeID,
		Target: p.VMName,
	})
	if err != nil {
		// 指令未送达：不能断言操作失败，但用户需要一个可操作的说法。
		return api.Unavailable("节点不可达，电源指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 状态以 agent 返回为准；它没返回时用动作的期望状态。
	//
	// 记 expected 而**不是**沿用投影值：投影是操作前的旧值，沿用它等于
	// 让界面在操作成功后仍显示旧状态，看起来像操作没生效。
	status := targetStatus[action]
	if reported, ok := result.Data[agent.StatusDataKey].(string); ok && reported != "" {
		status = reported
	}
	e.updateStatus(ctx, p.VMID, status)

	log.Printf("[vm] 电源操作完成 id=%d action=%s status=%s task=%d", p.VMID, action, status, t.ID)
	return nil
}

// updateStatus 回写投影状态。
//
// 失败时**不返回错误**：宿主侧操作已经成功，只是本地投影没写上。此时让任务
// 报失败会误导用户去重试一个已经生效的操作（重试还会被状态机拒绝，更困惑）。
// 投影是缓存，下一次对账会收敛；日志里留痕即可。
func (e *PowerExecutor) updateStatus(ctx context.Context, vmID int64, status string) {
	now := time.Now()
	err := e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", vmID).
		Updates(map[string]any{"status": status, "last_synced_at": now}).Error
	if err != nil {
		log.Printf("[vm] 回写投影状态失败 id=%d status=%s: %v", vmID, status, err)
	}
}

// deleteParams 是 vm.delete 任务的参数。
type deleteParams struct {
	VMID   int64  `json:"vm_id"`
	VMName string `json:"vm_name"`
	// DiskAction 取值 delete / keep，由用户在界面上显式选择（R-009）。
	DiskAction     string `json:"disk_action"`
	ObservedStatus string `json:"observed_status"`
	// Purge 为 true 表示这是回收站里的**彻底删除**：成功后物理删除记录
	// 而不只是标记 present = false。
	Purge bool `json:"purge,omitempty"`
}

// DeleteExecutor 执行 vm.delete 任务。
type DeleteExecutor struct {
	db    *gorm.DB
	agent agent.Client
}

// NewDeleteExecutor 构造删除执行器。
func NewDeleteExecutor(db *gorm.DB, client agent.Client) *DeleteExecutor {
	return &DeleteExecutor{db: db, agent: client}
}

// Type 返回处理的任务类型。
func (e *DeleteExecutor) Type() string { return model.TaskVMDelete }

// Run 下发删除指令并把投影标记为「已不在虚拟化层」。
func (e *DeleteExecutor) Run(ctx context.Context, t *model.Task) error {
	if t.Params == nil {
		return api.Internal()
	}

	var p deleteParams
	if err := json.Unmarshal([]byte(*t.Params), &p); err != nil {
		log.Printf("[vm] 解析删除参数失败: %v", err)
		return api.Internal()
	}
	if t.NodeID == nil {
		return api.Internal()
	}

	result, err := task.ReporterFrom(ctx).Dispatch(ctx, e.agent, agent.Operation{
		Kind:   agent.OpVMDelete,
		NodeID: *t.NodeID,
		Target: p.VMName,
		Params: map[string]any{"disk_action": p.DiskAction},
	})
	if err != nil {
		return api.Unavailable("节点不可达，删除指令未送达")
	}
	if !result.Success {
		return api.ValidationFailed(result.Message)
	}

	// 磁盘转移到「我的存储」：节点搬完之后回传文件清单，控制面据此建
	// storage_file 行。
	//
	// 由节点回传而不是控制面自己算：文件落在哪个目录、叫什么名字、实际
	// 多大，只有做搬运动作的一方知道。凭设备名猜出来的路径差一个字符，
	// 结果就是"我的存储里有一个点不开的文件"。
	if p.DiskAction == agent.DiskActionTransfer {
		e.recordTransferred(ctx, t, result)
	}

	// purge 表示这是回收站里的**彻底删除**：节点上已经删干净了，记录也不
	// 必再留。它与下面的标记是互斥的两条路。
	if p.Purge {
		// 关联行一起清：网口、静态地址、端口转发都以虚拟机为主体，留着
		// 会变成一串找不到主人的孤儿记录——而它们不会被任何查询用到。
		for _, table := range []any{
			&model.VMInterface{}, &model.StaticIP{}, &model.PortForward{},
			&model.InterfaceSecurityGroup{}, &model.VMLock{}, &model.VMCDROM{},
		} {
			table := table
			if err := e.db.WithContext(ctx).Where("vm_id = ?", p.VMID).
				Delete(table).Error; err != nil {
				// 只记日志：主记录删不掉才是问题，关联清理失败不至于让
				// 整个任务失败——那样用户会以为没删掉而再点一次。
				log.Printf("[vm] 清理关联记录失败 vm=%d table=%T: %v", p.VMID, table, err)
			}
		}
		if err := e.db.WithContext(ctx).Where("id = ?", p.VMID).
			Delete(&model.VM{}).Error; err != nil {
			log.Printf("[vm] 彻底删除虚拟机失败 id=%d: %v", p.VMID, err)
			return api.Internal()
		}
		log.Printf("[vm] 已彻底删除虚拟机 id=%d name=%s task=%d", p.VMID, p.VMName, t.ID)
		return nil
	}

	// **不物理删除记录**：审计流水与历史任务都会引用它，删掉会让这些引用
	// 悬空，事后追查「谁在什么时候删了哪台机器」就无从谈起。标记 present
	// 为 false 后列表不再显示，但记录保留（f-2-01 §5.1）。
	now := time.Now()
	err = e.db.WithContext(ctx).Model(&model.VM{}).
		Where("id = ?", p.VMID).
		Updates(map[string]any{
			"present":        false,
			"status":         model.VMStatusUnknown,
			"last_synced_at": now,
		}).Error
	if err != nil {
		log.Printf("[vm] 标记虚拟机已删除失败 id=%d: %v", p.VMID, err)
	}

	log.Printf("[vm] 已删除虚拟机 id=%d name=%s disk_action=%s task=%d",
		p.VMID, p.VMName, p.DiskAction, t.ID)
	return nil
}

// recordTransferred 把转移回来的磁盘登记成「我的存储」里的文件。
//
// 登记失败**不判任务失败**：虚拟机已经删掉了，节点上的文件也搬完了，此时
// 报失败会让用户去重试一个已经生效且不可重来的操作。文件没进列表是可以
// 事后补的，而"重试删除"是灾难。
func (e *DeleteExecutor) recordTransferred(ctx context.Context, t *model.Task, result *agent.Result) {
	if result == nil || result.Data == nil {
		return
	}
	raw, ok := result.Data[agent.VMDiskTransferDataKey]
	if !ok {
		return
	}
	// 走一遍 JSON：同进程拿到的是结构体，跨进程是 map。
	blob, err := json.Marshal(raw)
	if err != nil {
		log.Printf("[vm] 序列化转移结果失败: %v", err)
		return
	}
	var files []agent.TransferredDisk
	if err := json.Unmarshal(blob, &files); err != nil {
		log.Printf("[vm] 解析转移结果失败: %v", err)
		return
	}
	if len(files) == 0 || t.NodeID == nil {
		return
	}
	now := time.Now()
	for i := range files {
		f := &files[i]
		if f.RelPath == "" || f.Filename == "" {
			continue
		}
		row := model.StorageFile{
			NodeID: *t.NodeID, UserID: t.OwnerID, RelPath: f.RelPath,
			Category: model.FileCategoryDisk, Filename: f.Filename,
			SizeBytes: f.SizeBytes, UploadedAt: &now,
		}
		if err := e.db.WithContext(ctx).Create(&row).Error; err != nil {
			// 唯一约束冲突是这里唯一常见的失败：同一台机器被删两次（第二
			// 次的转移目标同名）。它说明文件已经在列表里了，不需要报错。
			log.Printf("[vm] 登记转移的磁盘失败 task=%d dev=%s: %v", t.ID, f.Dev, err)
		}
	}
}

// isDuplicateKey 判断错误是否为唯一约束冲突。
func isDuplicateKey(err error) bool {
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate") || strings.Contains(msg, "unique constraint")
}

func optID(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func optStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
