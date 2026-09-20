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
