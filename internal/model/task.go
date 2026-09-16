package model

import "time"

// 任务状态。
//
// 状态**以控制面记录为权威**（f-7-01 R-002）：agent 只上报进度与阶段，
// 不决定终态——否则一个行为异常的 agent 就能把任务标成成功。
const (
	TaskPending  = "pending"
	TaskRunning  = "running"
	TaskSuccess  = "success"
	TaskFailed   = "failed"
	TaskCanceled = "canceled"
	// TaskUnknown 表示「执行中但结果未知」：节点失联时使用。
	// **它不等于失败**——把失联判成失败会误导用户去排查一个可能已经成功的操作。
	// 由节点重连后的对账收敛为真实结论（f-7-01 R-006）。
	TaskUnknown = "unknown"
)

// 任务类型。取值形如「资源域.动作」，与 agent 的领域操作标识对应。
const (
	TaskVMCreate = "vm.create"
	// TaskVMPower 覆盖全部电源操作，具体动作在任务参数的 `action` 字段中。
	//
	// 不为每个动作单独建类型：五个动作的执行逻辑相同（探测前置已完成，
	// 下发指令、回写投影），拆成五个类型只会产生五份几乎相同的代码，
	// 而它们的差异用参数表达更自然。
	TaskVMPower  = "vm.power"
	TaskVMDelete = "vm.delete"

	// 存储池变更。这两个任务**全局串行**（f-5-01 R-003）：同一时刻只允许
	// 一个存储池变更任务运行，因此它们共用同一个资源锁键。
	TaskStoragePoolCreate = "storage.pool.create"
	TaskStoragePoolDelete = "storage.pool.delete"

	// 快照（F-2-07）。三者共用资源锁键 vm:<id>，因此与电源操作天然互斥：
	// 恢复快照时不会有并发的开机请求插进来——那会让恢复出来的磁盘状态
	// 立刻被一次开机覆盖掉一半。
	TaskVMSnapshotCreate  = "vm.snapshot.create"
	TaskVMSnapshotRestore = "vm.snapshot.restore"
	TaskVMSnapshotDelete  = "vm.snapshot.delete"

	// TaskVMConfigUpdate 修改硬件配置（F-2-05）。
	//
	// 与元数据修改（备注、分组）区分开：后者只存在于控制面，直接改库即可，
	// 不需要任务；只有需要**下发到节点**的改动才走这条路。
	TaskVMConfigUpdate = "vm.config.update"

	// 网络变更（F-2-03「网络管理」）。
	//
	// 每种资源一个任务类型而不是每个动作一个：同一资源的增删改**执行逻辑
	// 相同**（下发 → 回写投影），具体动作放在参数的 action 字段里。
	// 这与电源操作的处理方式一致。
	//
	// 资源锁键都是 vm:<id>，因此网卡、静态地址、端口转发的变更彼此串行，
	// 也不会与电源操作交错——「改完网卡正好赶上关机」会让下发落在错误的
	// 状态下。
	TaskVMInterfaceChange   = "vm.interface.change"
	TaskVMStaticIPChange    = "vm.staticip.change"
	TaskVMPortForwardChange = "vm.portforward.change"
)

// Task 对应 task 表。
//
// 这是**异步操作的唯一载体**：任何可能超过数秒的操作都必须入队，
// 接口不同步等待完成（f-7-01 R-001）。
type Task struct {
	ID     int64  `gorm:"primaryKey"`
	Type   string `gorm:"size:48;not null"`
	Status string `gorm:"size:16;not null;default:pending"`

	NodeID       *int64
	ResourceType *string `gorm:"size:32"`
	ResourceID   *int64
	ResourceName *string `gorm:"size:128"`
	// OwnerID 是资源归属，用于「tenant 只看自己的任务」（R-010）。
	OwnerID *int64

	// Params 与 Result 必须**脱敏后**存储（R-009）：
	// 口令、密钥、令牌一律不落库。
	Params *string `gorm:"type:text"`
	Result *string `gorm:"type:text"`

	Progress     int     `gorm:"not null;default:0"`
	CurrentStage *string `gorm:"size:64"`
	// Error 是面向用户的失败原因，不含内部堆栈（R-009）。
	Error           *string `gorm:"size:512"`
	CancelRequested bool    `gorm:"not null;default:false"`

	CreatedBy *int64
	// IdempotencyKey 由控制面生成并随指令下发；agent 据此去重，
	// 网络重放不会导致重复执行（R-005）。
	//
	// 唯一索引必须在此声明：AutoMigrate 按模型建表，模型不声明索引时
	// 测试库里就没有它，幂等保证会在测试中「看起来不存在」，
	// 而生产库（由迁移创建）有索引——两边行为分叉。
	IdempotencyKey *string `gorm:"size:64;uniqueIndex:uniq_task_idempotency_key"`

	StartedAt  *time.Time
	FinishedAt *time.Time
	// DispatchedAt 与 CreatedAt 分开记录，用于区分「排队耗时」与「执行耗时」（R-011）。
	DispatchedAt   *time.Time
	LastReportedAt *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// TableName 固定表名。
func (Task) TableName() string { return "task" }

// IsTerminal 报告任务是否已进入终态。
//
// 注意 unknown **不是**终态：它等待对账收敛。
func (t *Task) IsTerminal() bool {
	switch t.Status {
	case TaskSuccess, TaskFailed, TaskCanceled:
		return true
	default:
		return false
	}
}

// TaskResource 返回任务的资源锁键（f-7-01 R-003）。
//
// 同一资源的操作互斥：后到的任务保持 pending 直到锁释放。
// 资源类型或 ID 缺失时返回零值，表示该任务不参与资源互斥。
func (t *Task) TaskResource() (string, int64) {
	if t.ResourceType == nil || t.ResourceID == nil {
		return "", 0
	}
	return *t.ResourceType, *t.ResourceID
}
