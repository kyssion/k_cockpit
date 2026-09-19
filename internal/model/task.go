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

	// 存储卷的创建与删除（f-5-02）。
	//
	// **两者共用一个类型**，由 Params 里的 action 区分——它们的参数几乎
	// 一样（都只要卷名与设备），而拆成两个类型会有两个直接后果：执行器
	// 要注册两份（漏一份就是一个「任务永远不执行」的静默故障），以及
	// 「哪些参数属于哪一边」要在两处各维护一遍。
	//
	// 与存储池分开则是另一回事：池是「把这几块盘组织起来」，而卷要跑
	// pvcreate/vgcreate/lvcreate 三条命令、镜像还要等初始同步，耗时差一个
	// 量级，混在一起会让任务列表上看不出哪个慢在哪。
	TaskStorageVolumeApply = "storage.volume.apply"

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

	// TaskVPCSwitchChange 修改节点上的虚拟交换机（F-4-02）。
	//
	// 资源锁键是 node:<id> 而不是 vm:<id>：交换机属于节点，同一次改动会
	// 影响该节点上所有接入它的虚拟机，因此它与同节点的其它交换机变更必须
	// 串行——两个并发改动会各自基于「当前状态」计算，后落地的那个覆盖掉
	// 前一个，而前一个的下发结果已经生效在宿主机上了。
	TaskVPCSwitchChange = "vpc.switch.change"

	// 救援系统（f-2-12）。进入与退出**都是任务**——两者都要改虚拟机的
	// 硬件配置（引导顺序、盘型）并重启，是宿主机上的实际操作。
	//
	// 两次操作各自独立、不合并成一个「切换」：进入与退出之间隔着用户的
	// 排查工作（可能几小时），合并成一个任务既无法表达这段间隔，也让
	// 「进入成功了但退出失败」这种情况无法分别重试。
	TaskVMRescueEnter = "vm.rescue.enter"
	TaskVMRescueExit  = "vm.rescue.exit"

	// 模板（F-3-01 / F-3-02）与模板克隆。
	//
	// 制备与删除是耗时的磁盘操作（复制整块系统盘、删除可能很大的镜像），
	// 因此都走队列。克隆本身复用 vm.create（见 vm.createParams.TemplateID），
	// 不另立任务类型——对用户来说「从模板建一台机器」与「新建一台机器」
	// 是同一件事，界面上也是同一个入口。
	TaskTemplatePrepare = "template.prepare"
	TaskTemplateDelete  = "template.delete"

	// 重装系统（f-2-11）。两者都是磁盘操作，因此都走队列。
	TaskVMReinstall      = "vm.reinstall"
	TaskVMReinstallPurge = "vm.reinstall.purge"

	// 导出（f-2-14）。导出要打包整块磁盘，可能跑到几十分钟；删除产物是
	// 一次文件删除，但它们共用同一套「受理 → 执行 → 回写状态」的形状。
	TaskVMExport       = "vm.export"
	TaskVMExportDelete = "vm.export.delete"

	// 来宾自动化（f-2-10）：在线改密、离线改密、附加磁盘自动分区挂载、
	// 系统盘进来宾扩容。
	//
	// 四种动作共用一个任务类型：它们都要「先做宿主机侧的准备、再进来宾执行」，
	// 骨架完全一致，差异只在具体命令上。拆成四个类型会让四份几乎相同的
	// 受理与执行逻辑各自演化。
	TaskVMGuest = "vm.guest"

	// 镜像导入（f-2-13）。格式转换可能要处理几十 GB 的文件，因此走队列。
	TaskImageImport = "image.import"

	// 跨节点迁移（f-2-09）。要搬运整块磁盘，是最耗时的操作之一。
	TaskVMMigrate = "vm.migrate"

	// 目录共享的挂载与卸载（f-5-06）。
	TaskShareMount = "share.mount"

	// 把链接克隆的磁盘变成独立盘（解除对父盘的依赖）。
	TaskVMDisksIndependent = "vm.disks.independent"

	// 关机状态下的磁盘扩容。
	TaskVMDiskResize = "vm.disk.resize"

	// 平台自检后的重新下发（f-4-13）。
	TaskPlatformRepair = "platform.repair"

	// 宿主机性能调优（KSM / ZRAM / 嵌套虚拟化）。
	TaskHostTuning = "host.tuning.apply"

	// 虚拟机直通设备的挂载与卸载。
	TaskPassthroughChange = "vm.passthrough.change"

	// 宿主机防火墙的下发与回滚（f-4-11 第一层）。
	//
	// 应用与回滚共用一个类型：两者下发的都是"期望状态"，只在 enabled 上
	// 不同。拆成两个类型会有两个执行器要注册——漏一个就是"任务永远不执行"
	// 的静默故障。
	TaskHostFirewallApply = "host_firewall.apply"

	// 配额处置（f-4-10）：对某用户的网络施加或撤销限速 / 断网。
	TaskQuotaEnforce = "quota.enforce"

	// 抓包与删除抓包文件（f-4-12）。
	//
	// 两者分开：抓包是**长时间运行**的（要等 duration 秒），而删除是秒级
	// 的。混在一个类型里会让任务列表上看不出哪个慢在哪。
	TaskNetworkCapture       = "network.capture"
	TaskNetworkCaptureDelete = "network.capture.delete"

	// 端口安全策略下发（f-4-08）。下发的是**期望状态**而不是增量指令。
	TaskPortSecurityApply = "port_security.apply"

	// 安全组规则应用（f-4-04）。下发的是**汇总去重后**的生效规则。
	TaskSecurityGroupApply = "security_group.apply"

	// 公网地址变更（f-4-06）：绑定、解绑、浮动迁移。
	//
	// 三者共用一个任务类型：对节点而言都是「让这个地址指向这里 /
	// 不指向任何地方」，骨架一致，只在具体规则上有差异。
	TaskPublicIPChange = "public_ip.change"
)

// 阶段的执行状态。取值与 task.status 保持同一套词汇，避免界面上出现
// 两种说法（「执行中」与「running」）指着同一件事。
const (
	StagePending = "pending"
	StageRunning = "running"
	StageSuccess = "success"
	StageFailed  = "failed"
	StageSkipped = "skipped"
)

// TaskStage 对应 task_stage 表：任务内部的阶段明细。
//
// 与 task.current_stage 的分工需要说清楚：后者是**当前正在做什么**的一个
// 字符串，供列表页显示一行「正在创建磁盘」；本表是**整个过程**的流水，
// 供详情页还原「每一步各花了多久、卡在了哪一步」。
//
// 两者不可互相替代：列表页要的是单值（一行显示不下一条时间线），而排查
// 时需要的恰恰是被单值覆盖掉的历史——一个任务失败在第 4 步，如果只知道
// 它当前停在「配置网络」，就看不出前三步是快是慢、哪一步重试过。
//
// 阶段**由节点上报**（agent.Operation.OnStage）：控制面不知道一次创建在
// 宿主机上分了几步、每步多长。控制面自己编一条时间线，在换成节点侧实现后
// 会立刻对不上号——而那种「看起来对、其实不对」的时间线比没有更糟。
type TaskStage struct {
	ID     int64 `gorm:"primaryKey"`
	TaskID int64 `gorm:"not null;index:idx_task_stage_task_seq,priority:1"`
	// Seq 是阶段序号，从 1 开始，决定时间线的展示顺序。
	//
	// 用显式序号而不是靠 started_at 排序：同一毫秒内开始的阶段（mock 与
	// 快速操作中很常见）时间戳完全相同，靠时间排序会得到随机顺序，而一条
	// 顺序错乱的时间线会把人引向错误的结论。
	Seq int `gorm:"not null;index:idx_task_stage_task_seq,priority:2"`

	// Key 是稳定标识（供程序判断），Name 是中文名（供界面显示）。
	// 两者都存：只存 Key 会让界面自己去维护一张翻译表，而那表迟早与后端
	// 新增的阶段对不上；只存 Name 则无法按阶段做统计与告警。
	Key  string `gorm:"column:key;size:64;not null"`
	Name string `gorm:"size:64"`

	Status string `gorm:"size:16;not null;default:pending"`
	// Message 是该阶段的补充说明（失败原因、跳过理由）。
	Message *string `gorm:"size:512"`

	Retryable bool `gorm:"not null;default:false"`
	// RetryOfStageID 指向被重试的原始阶段。
	//
	// 保留这条链而不是覆盖原记录：「这一步重试过 3 次」本身是排障时最需要
	// 知道的事实之一，而覆盖会让它看起来像一次就过了。
	RetryOfStageID *int64

	StartedAt  *time.Time
	FinishedAt *time.Time
}

// TableName 固定表名。
func (TaskStage) TableName() string { return "task_stage" }

// Duration 返回该阶段的耗时；未结束时返回 0。
func (s *TaskStage) Duration() time.Duration {
	if s.StartedAt == nil || s.FinishedAt == nil {
		return 0
	}
	return s.FinishedAt.Sub(*s.StartedAt)
}

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
