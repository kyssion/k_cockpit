// Package settings 实现系统设置（F-9-01）。
//
// 三件事是本包的设计核心：
//
//  1. **优先级固定为 环境变量 > 面板设置 > 默认值**（R-001），判定在服务端。
//     环境变量代表部署者的意图，界面上的修改不应悄悄覆盖它——那会让
//     「我明明改了为什么不生效」成为最难排查的一类问题。
//
//  2. **被环境变量锁定的项必须只读**（R-002），且服务端**拒绝**修改请求。
//     「接受但不生效」比直接拒绝更糟：用户以为改成功了，直到某天发现
//     系统行为与设置不符。
//
//  3. **元数据在代码中声明**（Q-004），库里只存用户改过的值。元数据是
//     开发者契约——它会随版本演进（新增设置项、调整默认值、改变校验规则），
//     放进库会形成「代码与数据两个来源」，而两者迟早不一致。
package settings

// 分组。只有**已交付**的分组会出现在界面上（R-005）。
const (
	GroupBasic    = "basic"
	GroupSecurity = "security"
	GroupSession  = "session"
	GroupVM       = "vm"
	GroupStorage  = "storage"
	GroupNetwork  = "network"
	// GroupNotification 是邮件与通知设置。它曾长期是「未交付标签」——
	// 直到发信能力落地之前，把半成品显示出来只会让人以为功能坏了（R-005）。
	GroupNotification = "notification"
	// GroupLogging 是日志归档策略（G-43）。
	GroupLogging = "logging"
	// GroupScheduler 是调度相关的运营旋钮（G-48）。
	GroupScheduler = "scheduler"
)

// GroupInfo 是分组的展示信息。
type GroupInfo struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Order int    `json:"order"`
}

// groups 是分组清单，按展示顺序排列。
var groups = []GroupInfo{
	{Key: GroupBasic, Label: "基础", Order: 1},
	{Key: GroupSecurity, Label: "安全", Order: 2},
	{Key: GroupSession, Label: "会话", Order: 3},
	{Key: GroupVM, Label: "虚拟机", Order: 4},
	{Key: GroupStorage, Label: "存储", Order: 5},
	{Key: GroupNetwork, Label: "网络", Order: 6},
	// 通知标签此前是「未交付标签」：它的设置项不出现在界面（R-005）。
	// 现在邮件能力已交付，分组随之进入清单——留着那个占位项只会让
	// 「SMTP 服务器」孤零零地出现，而密码、端口、加密方式都不见了。
	{Key: GroupNotification, Label: "通知", Order: 7},
	{Key: GroupLogging, Label: "日志", Order: 8},
	{Key: GroupScheduler, Label: "调度", Order: 9},
}

// Kind 是设置项的值类型。
type Kind string

// 值类型。
const (
	KindString Kind = "string"
	KindInt    Kind = "int"
	KindBool   Kind = "bool"
	KindSelect Kind = "select"
)

// ApplyMode 描述变更如何生效。
type ApplyMode string

// 生效方式。
const (
	// ApplyImmediate 即时生效：变更后立刻影响系统行为。
	//
	// 这类项在应用失败时**必须回滚**（R-007）——例如改错了监听端口，
	// 会直接失去面板的访问路径，而用户手上没有任何补救手段。
	ApplyImmediate ApplyMode = "immediate"
	// ApplyRestart 需要重启服务才生效。
	ApplyRestart ApplyMode = "restart"
)

// 被业务代码引用的设置项键。
//
// 提为常量而不是在各处写字符串：设置项的键是**跨模块契约**——写入端
// （本包）与读取端（vm / storage）必须一致，任何一处拼错都会导致「界面
// 上改了、业务侧没生效」，而这类问题不会报错，只会表现为「设置好像没用」。
const (
	KeyVMStaleThreshold      = "vm.stale_threshold_seconds"
	KeyStorageStaleThreshold = "storage.stale_threshold_minutes"
	// KeySiteURL 是站点对外地址：拼邀请链接等「要发给站外的人」的链接时使用。
	// 由 internal/settings 声明、internal/invite 消费。
	KeySiteURL = "basic.site_url"
	// KeyLogKeepFiles 是日志归档的保留数量（不含当前文件）。
	// 由 internal/settings 声明、internal/logging 消费（经 main 装配）。
	KeyLogKeepFiles = "logging.keep_files"
	// KeyNetworkGlobalBandwidth / KeyNetworkGlobalBurst 是速率型全局带宽
	// 总限（G-44）：全部虚拟机共享的节点出向总限与突发峰值。
	// 由 internal/settings 声明、internal/network 消费（下发动作）。
	KeyNetworkGlobalBandwidth = "network.global_bandwidth_mbps"
	KeyNetworkGlobalBurst     = "network.global_burst_mbps"
	// KeyVMDefaultDiskIOPS 是新建虚拟机时未显式填写 IOPS 的默认值（G-45）。
	// 由 internal/settings 声明、internal/vm 消费（向导预填 + 受理兜底）。
	KeyVMDefaultDiskIOPS = "vm.default_disk_iops"
	// KeyVMRescueISO 是救援模式的启动镜像（G-47）：ISO 存放目录下的相对
	// 路径；留空表示沿用节点的内置默认救援镜像。
	KeyVMRescueISO = "vm.rescue_iso"
	// KeySchedulerEventKeepHours 是调度事件的保留期（G-48）。
	// 由 internal/settings 声明、internal/scheduler 消费（保留清理循环）。
	KeySchedulerEventKeepHours = "scheduler.event_keep_hours"
	// KeyStorageAutoTrim 是存储空间自动回收开关（G-52）。
	// 由 internal/settings 声明、internal/storage 消费（trim 循环）。
	KeyStorageAutoTrim = "storage.auto_trim_enabled"
	// KeyNetworkPortRangeStart / End 是端口转发宿主机端口的自动分配范围
	// （G-54）。由 internal/settings 声明、internal/vm 消费（受理时分配）。
	KeyNetworkPortRangeStart = "network.port_range_start"
	KeyNetworkPortRangeEnd   = "network.port_range_end"
)

// SMTP 相关设置项的键：由 internal/settings 声明、internal/mailer 消费。
// 跨模块的键必须是常量——写错一个字符不会报错，只会表现为「界面改了、
// 发信没变」，而这类问题在测试环境里几乎不会暴露。
const (
	KeySMTPHost     = "notification.smtp_host"
	KeySMTPPort     = "notification.smtp_port"
	KeySMTPSecurity = "notification.smtp_security"
	KeySMTPUsername = "notification.smtp_username"
	KeySMTPPassword = "notification.smtp_password"
	KeySMTPFrom     = "notification.smtp_from"
	KeySMTPFromName = "notification.smtp_from_name"
	KeySMTPTimeout  = "notification.smtp_timeout_seconds"
)

// Option 是枚举型的可选项。
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Spec 是一个设置项的元数据（R-003）。
//
// 每个设置项都必须声明清楚：键、分组、类型、默认值、**对应环境变量名**、
// 生效方式、是否敏感、是否可回滚。缺任何一项，界面都无法正确渲染或校验。
type Spec struct {
	Key         string `json:"key"`
	Group       string `json:"group"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Kind        Kind   `json:"kind"`
	Default     string `json:"default"`
	// EnvVar 是对应的环境变量名。它决定该项是否被锁定（R-001/R-002）。
	EnvVar string    `json:"env_var"`
	Apply  ApplyMode `json:"apply"`
	// Secret 表示值不回传明文（R-009），审计中也不记录（R-011）。
	Secret bool `json:"secret"`
	// Rollbackable 表示该项是否参与回滚。
	//
	// 默认全部可回滚；显式设为 false 用于那些「回滚本身也没有意义」的项。
	Rollbackable bool `json:"rollbackable"`

	// 以下是类型相关的约束，供服务端校验与前端渲染共同使用。
	Options  []Option `json:"options,omitempty"`
	MinValue *int     `json:"min_value,omitempty"`
	MaxValue *int     `json:"max_value,omitempty"`
	Unit     string   `json:"unit,omitempty"`
}

// specs 是设置项清单。
//
// **只登记真正会被使用的项**：加了设置项却没有任何代码读它，等于给用户一个
// 「改了但不生效」的开关——那正是 R-002 要避免的情形，只是原因不同。
var specs = []Spec{
	// --- 基础 ---
	{
		Key:          "site.name",
		Group:        GroupBasic,
		Label:        "站点名称",
		Description:  "显示在标题与通知中的系统名称。",
		Kind:         KindString,
		Default:      "K Cockpit",
		EnvVar:       "SITE_NAME",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		// 站点对外地址：邀请链接要发给站外的人，相对路径对他们没有意义。
		// 留空时邀请链接按相对路径返回，界面会提示管理员补上——猜一个错误
		// 域名比给相对路径更糟（收件人点开是别人的站点）。
		Key:          KeySiteURL,
		Group:        GroupBasic,
		Label:        "站点对外地址",
		Description:  "拼进邀请链接等发给站外用户的地址，例如 https://panel.example.com。留空时链接为相对路径，站外无法直接打开。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "SITE_URL",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},

	// --- 安全 ---
	{
		Key:          "auth.max_login_attempts",
		Group:        GroupSecurity,
		Label:        "登录失败上限",
		Description:  "同一来源在统计窗口内允许的失败次数，超过后暂时拒绝登录。",
		Kind:         KindInt,
		Default:      "5",
		EnvVar:       "AUTH_MAX_LOGIN_ATTEMPTS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1),
		MaxValue:     intPtr(100),
	},
	{
		Key:          "auth.risk_verification_enabled",
		Group:        GroupSecurity,
		Label:        "高风险操作二次验证",
		Description:  "关闭后，删除虚拟机等高风险操作将不再要求二次验证。仅在完全可信的环境中关闭。",
		Kind:         KindBool,
		Default:      "true",
		EnvVar:       "RISK_VERIFICATION_ENABLED",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},

	// --- 会话 ---
	{
		Key:          "session.idle_timeout_minutes",
		Group:        GroupSession,
		Label:        "空闲超时",
		Description:  "用户无操作超过该时长后需要重新登录。轮询与心跳不计为活动。",
		Kind:         KindInt,
		Default:      "30",
		EnvVar:       "SESSION_IDLE_TIMEOUT_MINUTES",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1),
		MaxValue:     intPtr(1440),
		Unit:         "分钟",
	},
	{
		Key:          "session.absolute_timeout_hours",
		Group:        GroupSession,
		Label:        "绝对超时",
		Description:  "无论是否活跃，登录超过该时长后都必须重新认证。",
		Kind:         KindInt,
		Default:      "12",
		EnvVar:       "SESSION_ABSOLUTE_TIMEOUT_HOURS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1),
		MaxValue:     intPtr(720),
		Unit:         "小时",
	},

	// --- 虚拟机 ---
	{
		Key:          KeyVMStaleThreshold,
		Group:        GroupVM,
		Label:        "投影陈旧阈值",
		Description:  "超过该时长未与虚拟化层对账时，界面标注「数据可能陈旧」。",
		Kind:         KindInt,
		Default:      "60",
		EnvVar:       "VM_STALE_THRESHOLD_SECONDS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(10),
		MaxValue:     intPtr(3600),
		Unit:         "秒",
	},
	{
		// 默认磁盘 IOPS（G-45）：向导会预填这个值；受理时三个 IOPS 项都
		// 未填的请求也会用它兜底——"新机器默认带限速"由此成为一处配置，
		// 而不是每台机器都要记得去改一遍。
		Key:          KeyVMDefaultDiskIOPS,
		Group:        GroupVM,
		Label:        "默认磁盘 IOPS",
		Description:  "新建虚拟机时预填的 IOPS 上限（总量）。0 表示默认不限制；显式填写了的请求不受它影响。",
		Kind:         KindInt,
		Default:      "0",
		EnvVar:       "VM_DEFAULT_DISK_IOPS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(0),
		MaxValue:     intPtr(1000000),
	},
	{
		// 救援系统 ISO（G-47）：救援模式此前只能用节点内置的默认镜像，
		// 管理员换成自己维护的救援盘（SystemRescueCd、WinPE 等）没有入口。
		// 是**宿主机上**的路径：相对 ISO 存放目录（storage.iso_dir）。
		Key:          KeyVMRescueISO,
		Group:        GroupVM,
		Label:        "救援系统 ISO",
		Description:  "救援模式启动用的镜像，相对 ISO 存放目录的路径（例如 rescue/systemrescue.iso）。留空用节点内置的默认救援镜像。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "VM_RESCUE_ISO",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},

	// --- 存储 ---
	{
		Key:          KeyStorageStaleThreshold,
		Group:        GroupStorage,
		Label:        "容量数据陈旧阈值",
		Description:  "空间数据超过该时长未上报时，界面标注「容量可能已过期」。",
		Kind:         KindInt,
		Default:      "5",
		EnvVar:       "STORAGE_STALE_THRESHOLD_MINUTES",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1),
		MaxValue:     intPtr(1440),
		Unit:         "分钟",
	},

	// --- 安全与审计 ---
	//
	// 请求日志默认**关闭**：它记录每一次接口调用，量级与审计不在一个数量级，
	// 默认打开会把磁盘用在一堆"健康检查"上。它是排查问题时才需要的东西，
	// 因此默认关、按需开、并且能一键清掉。
	{
		Key:          "security.request_log_enabled",
		Group:        GroupSecurity,
		Label:        "记录请求日志",
		Description:  "记录每一次接口调用（方法、路径、状态码、耗时）。默认关闭——它的量级远大于审计日志，且多数时候用不上。",
		Kind:         KindBool,
		Default:      "false",
		EnvVar:       "SECURITY_REQUEST_LOG_ENABLED",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		Key:          "security.request_log_keep_days",
		Group:        GroupSecurity,
		Label:        "请求日志保留天数",
		Description:  "超过这个天数的请求日志会在清理时被删除。",
		Kind:         KindInt,
		Default:      "7",
		EnvVar:       "SECURITY_REQUEST_LOG_KEEP_DAYS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1),
		MaxValue:     intPtr(90),
		Unit:         "天",
	},
	{
		// 自动轮换会话签名密钥的间隔。
		//
		// 0 表示不自动轮换：轮换等于全员登出，是否让它自动发生取决于部署
		// 形态——一个自建自用的面板不该因为"到日子了"把所有人都踢出去，而
		// 一个托管多个租户的环境应当定期换。
		Key:          "security.auth_key_rotate_days",
		Group:        GroupSecurity,
		Label:        "会话密钥自动轮换间隔",
		Description:  "超过这个天数后自动更换会话签名密钥。轮换会让**所有人立即登出**（包括正在操作的自己）。0 表示不自动轮换。",
		Kind:         KindInt,
		Default:      "0",
		EnvVar:       "SECURITY_AUTH_KEY_ROTATE_DAYS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(0),
		MaxValue:     intPtr(365),
		Unit:         "天",
	},
	{
		// 输入侧防护（请求过滤）：拦路径穿越 / 空字节 / 扫描器探测与
		// 异常 Content-Type。默认开——它是安全边界；提供开关是为了排查
		// 「某个客户端被误拦」时能临时关掉，而不是让人平时关着。
		Key:          "security.request_filter_enabled",
		Group:        GroupSecurity,
		Label:        "请求过滤（输入侧防护）",
		Description:  "拦截路径穿越、空字节、扫描器探测路径与异常 Content-Type 的请求。排查客户端被误拦时可临时关闭。",
		Kind:         KindBool,
		Default:      "true",
		EnvVar:       "SECURITY_REQUEST_FILTER_ENABLED",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		// 定时弱口令检查。
		//
		// 真正的"泄露"判定（比对泄露库）需要外部数据，控制面不联网也不持有
		// 明文密码；这里做的是**弱口令与已知泄露口令**的周期检查，判定由
		// 节点侧完成（见 agent 的 security.password_audit），控制面只负责
		// 开关、定时与结果。
		Key:          "security.password_breach_check",
		Group:        GroupSecurity,
		Label:        "定时检查弱口令与泄露口令",
		Description:  "按天检查账号口令是否出现在弱口令 / 已知泄露口令清单中。命中不自动改密，只在安全中心标出，由用户自己改。",
		Kind:         KindBool,
		Default:      "false",
		EnvVar:       "SECURITY_PASSWORD_BREACH_CHECK",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},

	// --- 路径 ---
	//
	// 这一组存在的理由很具体：默认的目录布局（ISO 放在哪、模板放哪、
	// 临时文件放哪）在多数部署里够用，但只要有人的数据盘挂在别处，
	// 它就变成了一个必须能改的配置——而改不了的结果是把数据盘塞进
	// 系统盘，直到某天写满。
	//
	// 留空表示用内置默认目录，而不是"关掉这个功能"。
	{
		Key:          "storage.iso_dir",
		Group:        GroupStorage,
		Label:        "ISO 存放目录",
		Description:  "「我的存储」里 ISO 类文件在宿主机上的存放位置。留空用内置默认目录。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "STORAGE_ISO_DIR",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		Key:          "storage.disk_dir",
		Group:        GroupStorage,
		Label:        "虚拟磁盘目录",
		Description:  "「我的存储」里虚拟磁盘类文件的存放位置。留空用内置默认目录。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "STORAGE_DISK_DIR",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		Key:          "storage.template_dir",
		Group:        GroupStorage,
		Label:        "模板目录",
		Description:  "模板与模板包在宿主机上的存放位置。留空用内置默认目录。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "STORAGE_TEMPLATE_DIR",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		// 存储空间自动回收（G-52）：删盘后块设备上的可回收块随时间积累，
		// 自动 trim 让"记得去点"不再是回收发生的前提。执行结果在调度
		// 事件里（调度器页可见）。
		Key:          KeyStorageAutoTrim,
		Group:        GroupStorage,
		Label:        "自动回收存储空间",
		Description:  "每天对全部在线节点执行一次 trim，回收已删除虚拟磁盘占用的块。执行结果见调度器页的「存储空间自动回收」事件。",
		Kind:         KindBool,
		Default:      "false",
		EnvVar:       "STORAGE_AUTO_TRIM_ENABLED",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		Key:          "storage.temp_dir",
		Group:        GroupStorage,
		Label:        "临时目录",
		Description:  "导入、导出与打包过程中的临时文件位置。它需要有足够的空间容纳整块镜像。留空用内置默认目录。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "STORAGE_TEMP_DIR",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},

	// --- 网络 ---
	{
		Key:          "network.default_mode",
		Group:        GroupNetwork,
		Label:        "默认网络模式",
		Description:  "新建网络时使用的模式。隔离模式不含上行，虚拟机之间可互通。",
		Kind:         KindSelect,
		Default:      "nat",
		EnvVar:       "NETWORK_DEFAULT_MODE",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		Options: []Option{
			{Value: "nat", Label: "NAT 出网"},
			{Value: "empty", Label: "隔离（无上行）"},
		},
	},
	{
		// 全局带宽总限（G-44）：与累计型配额（按月流量）、交换机级限速分工，
		// 管的是"这台宿主机总共能出多少"。0 表示不限。
		//
		// 注意它只改控制面的值：节点上的整形规则要在网络中心点「应用」才会
		// 对齐——设置可以被批量回滚，而节点状态只能靠一次明确动作收敛。
		Key:          KeyNetworkGlobalBandwidth,
		Group:        GroupNetwork,
		Label:        "全局带宽总限",
		Description:  "全部虚拟机共享的节点出向总带宽。0 表示不限。修改后需在网络中心「应用到节点」才会下发。",
		Kind:         KindInt,
		Default:      "0",
		EnvVar:       "NETWORK_GLOBAL_BANDWIDTH_MBPS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(0),
		MaxValue:     intPtr(400000),
		Unit:         "Mbps",
	},
	{
		// 端口自动分配范围（G-54）：新增端口转发时宿主机端口留空，即从该
		// 范围内取一个未被占用的（同一节点、同一协议内独占）。
		Key:          KeyNetworkPortRangeStart,
		Group:        GroupNetwork,
		Label:        "端口自动分配起点",
		Description:  "新增端口转发时宿主机端口留空，自动从这个范围里取未占用的端口。",
		Kind:         KindInt,
		Default:      "10000",
		EnvVar:       "NETWORK_PORT_RANGE_START",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1024),
		MaxValue:     intPtr(65535),
	},
	{
		Key:          KeyNetworkPortRangeEnd,
		Group:        GroupNetwork,
		Label:        "端口自动分配终点",
		Description:  "自动分配范围的上界（含）。范围应避开宿主机临时端口段（通常 32768 起）。",
		Kind:         KindInt,
		Default:      "20000",
		EnvVar:       "NETWORK_PORT_RANGE_END",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1024),
		MaxValue:     intPtr(65535),
	},
	{
		Key:          KeyNetworkGlobalBurst,
		Group:        GroupNetwork,
		Label:        "全局带宽突发峰值",
		Description:  "短暂突发时允许达到的峰值，仅在设置了总限时有效。0 表示不启用突发额度。",
		Kind:         KindInt,
		Default:      "0",
		EnvVar:       "NETWORK_GLOBAL_BURST_MBPS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(0),
		MaxValue:     intPtr(400000),
		Unit:         "Mbps",
	},

	// --- 通知（邮件）---
	//
	// 整组的生效方式都是「即时」：SMTP 配置只被发信代码读取，没有常驻连接
	// 需要重建。标成「需重启」会让「测试邮件」按钮在保存后仍然发不出去，
	// 而那正是用户验证配置是否正确的唯一手段。
	{
		Key:          "notification.smtp_host",
		Group:        GroupNotification,
		Label:        "SMTP 服务器",
		Description:  "发信服务器主机名，例如 smtp.example.com。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "SMTP_HOST",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		Key:          "notification.smtp_port",
		Group:        GroupNotification,
		Label:        "SMTP 端口",
		Description:  "常用端口：25（明文）、587（STARTTLS）、465（隐式 TLS）。",
		Kind:         KindInt,
		Default:      "587",
		EnvVar:       "SMTP_PORT",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1),
		MaxValue:     intPtr(65535),
	},
	{
		Key:          "notification.smtp_security",
		Group:        GroupNotification,
		Label:        "加密方式",
		Description:  "服务器不支持所选方式时发信会直接失败，不会退回明文。",
		Kind:         KindSelect,
		Default:      "starttls",
		EnvVar:       "SMTP_SECURITY",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		Options: []Option{
			{Value: "starttls", Label: "STARTTLS（推荐）"},
			{Value: "tls", Label: "隐式 TLS"},
			{Value: "none", Label: "不加密（仅可信内网）"},
		},
	},
	{
		Key:          "notification.smtp_username",
		Group:        GroupNotification,
		Label:        "SMTP 用户名",
		Description:  "留空表示服务器不需要认证。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "SMTP_USERNAME",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		Key:          "notification.smtp_password",
		Group:        GroupNotification,
		Label:        "SMTP 密码",
		Description:  "保存后不再回传明文；留空表示保持当前密码不变。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "SMTP_PASSWORD",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		Secret:       true,
	},
	{
		Key:          "notification.smtp_from",
		Group:        GroupNotification,
		Label:        "发件邮箱",
		Description:  "收件人看到的发件地址。",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "SMTP_FROM",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		Key:          "notification.smtp_from_name",
		Group:        GroupNotification,
		Label:        "发件人名称",
		Description:  "留空则只显示发件邮箱。",
		Kind:         KindString,
		Default:      "K Cockpit",
		EnvVar:       "SMTP_FROM_NAME",
		Apply:        ApplyImmediate,
		Rollbackable: true,
	},
	{
		Key:          "notification.smtp_timeout_seconds",
		Group:        GroupNotification,
		Label:        "连接超时",
		Description:  "连接与读写的超时时间，网络较差时可适当调大。",
		Kind:         KindInt,
		Default:      "15",
		EnvVar:       "SMTP_TIMEOUT_SECONDS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(5),
		MaxValue:     intPtr(120),
		Unit:         "秒",
	},

	// --- 日志 ---
	//
	// 归档本身（按大小轮转 + gzip 压缩）在 internal/logging 里，这里只管
	// 保留多少。缩小数值不会立刻删历史归档：排障依据不该为了界面上数字
	// 好看而被顺手清掉，收敛发生在下一次轮转。
	{
		Key:          KeyLogKeepFiles,
		Group:        GroupLogging,
		Label:        "日志最大备份数",
		Description:  "轮转归档（已压缩）的保留数量，不含当前文件。归档按大小轮转并压缩存放。",
		Kind:         KindInt,
		Default:      "5",
		EnvVar:       "LOG_KEEP_FILES",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1),
		MaxValue:     intPtr(60),
		Unit:         "个",
	},

	// --- 调度 ---
	{
		// 调度事件保留期（G-48）：事件表只在调度器实际做了事时才有行，
		// 但一个反复失败的调度器每轮都留一条失败记录，保留期给这件事
		// 一个确定的边界。
		Key:          KeySchedulerEventKeepHours,
		Group:        GroupScheduler,
		Label:        "调度事件保留期",
		Description:  "超过该小时数的调度事件会被周期清理（默认 168 小时 = 一周）。",
		Kind:         KindInt,
		Default:      "168",
		EnvVar:       "SCHEDULER_EVENT_KEEP_HOURS",
		Apply:        ApplyImmediate,
		Rollbackable: true,
		MinValue:     intPtr(1),
		MaxValue:     intPtr(8760),
		Unit:         "小时",
	},
}

func intPtr(v int) *int { return &v }

// Specs 返回**已交付**的设置项清单（R-005）。
//
// 未交付标签的项被过滤掉，而不是标记为「不可用」：显示一个灰掉的开关，
// 用户只会以为功能坏了，而不是「还没做」。
func Specs() []Spec {
	delivered := deliveredGroups()

	out := make([]Spec, 0, len(specs))
	for _, s := range specs {
		if delivered[s.Group] {
			out = append(out, s)
		}
	}
	return out
}

// Groups 返回已交付的分组（按展示顺序）。
func Groups() []GroupInfo {
	out := make([]GroupInfo, len(groups))
	copy(out, groups)
	return out
}

// SpecOf 按键查找设置项；未登记或未交付时返回 false。
func SpecOf(key string) (Spec, bool) {
	delivered := deliveredGroups()
	for _, s := range specs {
		if s.Key == key && delivered[s.Group] {
			return s, true
		}
	}
	return Spec{}, false
}

func deliveredGroups() map[string]bool {
	set := make(map[string]bool, len(groups))
	for _, g := range groups {
		set[g.Key] = true
	}
	return set
}
