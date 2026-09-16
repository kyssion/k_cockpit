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
	// GroupNotification 属未交付标签：它的设置项**不出现在界面**，
	// 而不是显示为不可用——半成品显示出来只会让人以为功能坏了（R-005）。
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
	// 通知标签尚未交付：它不出现在 groups 中，因此其设置项也不会被返回。
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

	// --- 未交付标签（不会出现在清单中）---
	//
	// 保留它的意义在于：当通知能力交付时，只需把分组加入 groups 并补上
	// 设置项，机制无需改动。
	{
		Key:          "notification.smtp_host",
		Group:        GroupNotification,
		Label:        "SMTP 服务器",
		Kind:         KindString,
		Default:      "",
		EnvVar:       "SMTP_HOST",
		Apply:        ApplyRestart,
		Rollbackable: false,
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
