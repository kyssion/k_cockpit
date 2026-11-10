package platform

import "strings"

// 接口文档的元数据（G-50）：摘要、认证方式、参数与 curl 的生成规则。
//
// 清单本身仍从路由表实时取出（见 apidocs.go）；这里只补路由表里没有的
// 那部分信息。认证方式是**按注册处机械提取的显式数据表**而不是运行时
// 推断——公开与管理员路由是安全相关的清单，本就该有一处显式的登记。

// apiDocParam 描述一个参数。
type apiDocParam struct {
	Name string `json:"name"`
	// In 取值 path / query / body。
	In   string `json:"in"`
	Note string `json:"note,omitempty"`
}

// publicAPIRoutes 是无需登录即可访问的路由（不含 /api/v1 前缀）。
//
// 它从 router.Register 里逐条核对而来：公开面是安全边界，值得一份
// 显式清单——新增公开路由时必须同步这里，而漏同步的表现（文档标错
// 认证方式）比多维护一张表更糟。
var publicAPIRoutes = map[string]bool{
	"GET /health":                        true,
	"GET /setup/status":                  true,
	"POST /setup/admin":                  true,
	"POST /auth/login":                   true,
	"POST /auth/login/verify":            true,
	"POST /auth/login/password":          true,
	"POST /auth/bootstrap/skip":          true,
	"POST /auth/bootstrap/totp/setup":    true,
	"POST /auth/bootstrap/totp/confirm":  true,
	"POST /auth/bootstrap/email/code":    true,
	"POST /auth/bootstrap/email/confirm": true,
	"POST /auth/forgot/send":             true,
	"POST /auth/forgot/verify":           true,
	"POST /auth/forgot/reset":            true,
	"GET /invites/preview":               true,
	"POST /invites/accept":               true,
}

// adminAPIRoutes 是仅管理员可访问的路由（不含 /api/v1 前缀）。
//
// 同样从 router.Register 机械提取（带 adminOnly 中间件的那些行）。
// 未列出的已登录路由即租户可用；以 docs/03-api/API.md 为最终权威。
var adminAPIRoutes = map[string]bool{
	// 告警中心（F-8-07）：带全平台节点/任务明细，ack 还会改写所有管理员
	// 看到的状态。第 13 轮实机越权测试后收归管理员。
	"GET /alerts":                               true,
	"POST /alerts/:id/ack":                      true,
	"POST /alerts/ack-all":                      true,
	"GET /nodes":                                true,
	"GET /nodes/:id":                            true,
	"GET /nodes/:id/stats":                      true,
	"PATCH /nodes/:id/maintenance":              true,
	"GET /maintenance":                          true,
	"POST /maintenance/enter":                   true,
	"POST /maintenance/exit":                    true,
	"PATCH /nodes/:id/console-host":             true,
	"POST /nodes/registration-tokens":           true,
	"DELETE /nodes/:id":                         true,
	"GET /nodes/:id/disks":                      true,
	"GET /nodes/:id/storage-pools":              true,
	"GET /storage-pools/:id":                    true,
	"POST /storage-pools":                       true,
	"PATCH /storage-pools/:id":                  true,
	"DELETE /storage-pools/:id":                 true,
	"GET /storage-partitions":                   true,
	"POST /storage-partitions":                  true,
	"DELETE /storage-partitions":                true,
	"POST /storage-pools/:id/config":            true,
	"POST /storage-pools/:id/unmount":           true,
	"POST /storage/trim":                        true,
	"GET /vpc-acl":                              true,
	"POST /vpc-acl":                             true,
	"PUT /vpc-acl/:id":                          true,
	"DELETE /vpc-acl/:id":                       true,
	"GET /vpc-acl/preview":                      true,
	"POST /vpc-acl/apply":                       true,
	"GET /nodes/:id/network":                    true,
	"GET /nodes/:id/networks":                   true,
	"POST /nodes/:id/vpc-switches":              true,
	"PATCH /vpc-switches/:id":                   true,
	"POST /vpc-switches/:id/migrate":            true,
	"POST /vpc-switches/:id/reconfigure":        true,
	"POST /networks/ports/release":              true,
	"POST /networks/counters/reset":             true,
	"POST /networks/ipv6/policy":                true,
	"POST /networks/global-bandwidth/apply":     true,
	"DELETE /vpc-switches/:id":                  true,
	"GET /settings":                             true,
	"PATCH /settings":                           true,
	"POST /settings/rollback":                   true,
	"POST /settings/mail/test":                  true,
	"POST /templates/:id/preprocess":            true,
	"GET /monitor/host":                         true,
	"GET /monitor/host/devices":                 true,
	"GET /users":                                true,
	"POST /users":                               true,
	"PATCH /users/:id":                          true,
	"PUT /users/:id/status":                     true,
	"DELETE /users/:id":                         true,
	"PUT /users/:id/ssh":                        true,
	"GET /networks":                             true,
	"GET /nodes/:id/unmanaged-vms":              true,
	"POST /nodes/:id/adopt-vm":                  true,
	"POST /vms/:id/force-delete":                true,
	"GET /networks/bridges":                     true,
	"POST /networks/bridges":                    true,
	"DELETE /networks/bridges/:id":              true,
	"POST /networks/bridges/:id/uplink":         true,
	"POST /networks/bridges/:id/uplink/confirm": true,
	"DELETE /networks/bridges/:id/uplink":       true,
	"POST /networks/repair":                     true,
	"GET /port-mirrors":                         true,
	"POST /port-mirrors":                        true,
	"PATCH /port-mirrors/:id":                   true,
	"DELETE /port-mirrors/:id":                  true,
	"GET /port-mirrors/:id/precheck":            true,
	"POST /port-mirrors/:id/enable":             true,
	"POST /port-mirrors/:id/confirm":            true,
	"POST /port-mirrors/:id/disable":            true,
	"GET /firewall/policy":                      true,
	"PATCH /firewall/policy":                    true,
	"GET /firewall/rules":                       true,
	"POST /firewall/rules":                      true,
	"DELETE /firewall/rules/:ruleID":            true,
	"GET /firewall/precheck":                    true,
	"POST /firewall/apply":                      true,
	"POST /firewall/rollback":                   true,
	"GET /vms/:id/firewall":                     true,
	"PUT /vms/:id/firewall":                     true,
	"DELETE /vms/:id/firewall":                  true,
	"GET /settings/access":                      true,
	"PUT /settings/access":                      true,
	"GET /ovs/status":                           true,
	"GET /ovs/ports":                            true,
	"GET /ovs/leases":                           true,
	"POST /ovs/check":                           true,
	"POST /ovs/repair":                          true,
	"GET /host/tuning":                          true,
	"PUT /host/tuning":                          true,
	"GET /host/cpu-affinity-presets":            true,
	"POST /host/cpu-affinity-presets":           true,
	"DELETE /host/cpu-affinity-presets/:id":     true,
	"GET /host/passthrough":                     true,
	"POST /host/passthrough/bind":               true,
	"POST /host/passthrough/unbind":             true,
	"GET /vms/:id/passthrough":                  true,
	"POST /vms/:id/passthrough":                 true,
	"DELETE /vms/:id/passthrough":               true,
	"GET /host-firewall":                        true,
	"PATCH /host-firewall/policy":               true,
	"POST /host-firewall/rules":                 true,
	"DELETE /host-firewall/rules/:id":           true,
	"GET /host-firewall/precheck":               true,
	"POST /host-firewall/apply":                 true,
	"POST /host-firewall/rollback":              true,
	"GET /host-firewall/connections":            true,
	"POST /host-firewall/connections/close":     true,
	"GET /resource-quotas":                      true,
	"PUT /resource-quotas":                      true,
	"DELETE /resource-quotas/:id":               true,
	"POST /resource-quotas/:id/reset-usage":     true,
	"GET /settings/log/status":                  true,
	"GET /request-logs":                         true,
	"DELETE /request-logs":                      true,
	"POST /security/password-audit":             true,
	"GET /security/password-audit":              true,
	"GET /settings/auth-key":                    true,
	"POST /settings/auth-key/rotate":            true,
	"GET /invites":                              true,
	"POST /invites":                             true,
	"POST /invites/:id/revoke":                  true,
	"POST /invites/:id/resend":                  true,
	"GET /settings/log/read":                    true,
	"PUT /settings/log/level":                   true,
	"GET /settings/log/export":                  true,
	"GET /settings/log/stream":                  true,
	"POST /settings/log/delete":                 true,
	"GET /compute-quotas":                       true,
	"PUT /compute-quotas":                       true,
	"GET /diagnostics/categories":               true,
	"GET /diagnostics/export":                   true,
	"GET /port-security":                        true,
	"POST /port-security/preview":               true,
	"POST /port-security":                       true,
	"DELETE /port-security/:id":                 true,
	"GET /schedulers":                           true,
	"GET /scheduler-events":                     true,
	"GET /storage-volumes":                      true,
	"POST /storage-volumes/preview":             true,
	"POST /storage-volumes":                     true,
	"DELETE /storage-volumes/:id":               true,
	"GET /public-ips":                           true,
	"POST /public-ips":                          true,
	"POST /public-ips/batch/bind":               true,
	"POST /public-ips/batch/unbind":             true,
	"DELETE /public-ips/:id":                    true,
	"GET /public-ips/:id/preview":               true,
	"POST /public-ips/:id/bind":                 true,
	"POST /public-ips/:id/migrate":              true,
	"DELETE /public-ips/:id/bind":               true,
	"GET /public-ips/ipv6-prefixes":             true,
	"POST /public-ips/reload":                   true,
	"GET /quotas":                               true,
	"PUT /quotas":                               true,
	"POST /tasks/clear":                         true,
}

// moduleLabels 把路径首段翻译成模块名，与 API.md 的模块划分一致。
//
// **必须覆盖全部首段**：漏一个，摘要里就会直接漏出英文原文（实机测试
// 抓出「查询host列表」「创建imports」这类条目）。TestAPIDocsSummariesUseChinese
// 会扫描全量路由，把漏登记的段拦下来。
var moduleLabels = map[string]string{
	"health":             "健康检查",
	"setup":              "初始化",
	"auth":               "认证与会话",
	"account":            "账号自管理",
	"vms":                "虚拟机",
	"vm-tags":            "虚拟机标签",
	"tags":               "标签",
	"templates":          "模板",
	"template-exports":   "模板导出",
	"nodes":              "节点",
	"maintenance":        "站点维护",
	"tasks":              "任务",
	"storage-pools":      "存储池",
	"storage-partitions": "磁盘分区",
	"storage-volumes":    "存储卷",
	"storage":            "存储",
	"my-storage":         "我的存储",
	"share-mounts":       "目录共享",
	"networks":           "网络中心",
	"network":            "网络",
	"vpc-switches":       "VPC 交换机",
	"vpc-acl":            "VPC ACL",
	"security-groups":    "安全组",
	"public-ips":         "公网 IP",
	"port-security":      "端口安全",
	"port-mirrors":       "端口镜像",
	"captures":           "网络抓包",
	"firewall":           "KVM 防火墙",
	"host-firewall":      "宿主机防火墙",
	"passthrough":        "硬件直通",
	"host":               "宿主机",
	"ovs":                "OVS",
	"imports":            "镜像导入",
	"users":              "用户管理",
	"invites":            "邀请注册",
	"quota":              "配额",
	"quotas":             "存储配额",
	"compute-quotas":     "计算配额",
	"resource-quotas":    "周期配额",
	"settings":           "系统设置",
	"request-logs":       "请求日志",
	"schedulers":         "调度器",
	"scheduler-events":   "调度事件",
	"schedule":           "定时任务",
	"schedules":          "定时任务",
	"dashboard":          "工作台",
	"monitor":            "监控",
	"search":             "全局搜索",
	"alerts":             "告警中心",
	"audit":              "审计日志",
	"logs":               "服务端日志",
	"diagnostics":        "诊断",
	"api-keys":           "API 凭证",
	"action-tokens":      "一次性动作令牌",
	"security":           "安全中心",
	"platform-check":     "平台自检",
	"host-tuning":        "宿主机调优",
	"access-control":     "公网访问控制",
	"version":            "版本信息",
	"versions":           "版本与依赖",
}

// singularLabels 把模块名收敛成「单个资源」的说法，供动作句做宾语。
// 未登记的模块沿用模块名本身。
var singularLabels = map[string]string{
	"告警中心": "告警",
	"邀请注册": "邀请",
	"用户管理": "用户",
	"镜像导入": "导入任务",
}

// nounLabels 是「子资源 / 从属概念」段的中文名。
//
// 与 verbLabels 的区别：名词段回答「这是谁的什么」，GET 它是查询，其余
// 方法是对它的写操作；动作段回答「对它做什么」，措辞是动宾结构。
var nounLabels = map[string]string{
	"passthrough":          "直通设备",
	"tuning":               "调优参数",
	"disks":                "磁盘",
	"stats":                "资源占用",
	"network":              "网络能力",
	"networks":             "交换机",
	"storage-pools":        "存储池",
	"storage-partitions":   "磁盘分区",
	"storage-volumes":      "存储卷",
	"unmanaged-vms":        "存量虚拟机",
	"vpc-switches":         "VPC 交换机",
	"ports":                "端口",
	"leases":               "租约",
	"files":                "文件",
	"uploads":              "上传会话",
	"rules":                "规则",
	"policy":               "策略",
	"connections":          "连接",
	"sessions":             "会话",
	"session":              "会话",
	"categories":           "分类",
	"devices":              "设备",
	"api-keys":             "API 凭证",
	"quotas":               "配额",
	"snapshots":            "快照",
	"interfaces":           "网卡",
	"nic":                  "网卡",
	"port-forwards":        "端口转发",
	"public-ips":           "公网 IP",
	"static-ips":           "静态地址",
	"schedules":            "定时任务",
	"shares":               "共享目录",
	"cdroms":               "光驱",
	"tags":                 "标签",
	"console":              "控制台",
	"exports":              "导出任务",
	"imports":              "导入任务",
	"bridges":              "网桥",
	"uplink":               "上联",
	"status":               "状态",
	"facets":               "取值维度",
	"format":               "支持的格式",
	"client-ip":            "出口地址",
	"vms":                  "虚拟机",
	"nodes":                "节点",
	"users":                "用户",
	"alerts":               "告警",
	"tasks":                "任务",
	"templates":            "模板",
	"settings":             "设置",
	"stream":               "事件流",
	"access":               "访问控制",
	"auth-key":             "签名密钥",
	"log":                  "服务端日志",
	"high-risk-policy":     "高风险动作清单",
	"password-audit":       "弱密码审计",
	"recovery-codes":       "恢复码",
	"security-setup":       "安全设置",
	"addresses":            "地址",
	"edit-form":            "编辑表单",
	"create-form":          "创建向导元数据",
	"initial-credential":   "初始凭据",
	"migrate-targets":      "可迁移目标",
	"migrations":           "迁移记录",
	"neighbors":            "邻居",
	"pcie-info":            "PCIe 信息",
	"guest-status":         "来宾状态",
	"runtime":              "运行态",
	"effective":            "生效配置",
	"delete-preview":       "删除影响预览",
	"config-changes":       "可改配置",
	"family":               "镜像链",
	"connection-file":      "控制台连接文件",
	"frame":                "控制台画面",
	"screenshot":           "控制台截图",
	"ws":                   "控制台通道",
	"iso":                  "ISO 镜像",
	"health":               "健康状态",
	"host-detail":          "宿主机详情",
	"summary":              "概览",
	"cpu-affinity-presets": "绑核预设",
	"host":                 "宿主机资源",
	"ipv6-prefixes":        "IPv6 前缀",
	"firewall":             "防火墙规则",
	"guest-actions":        "来宾操作",
	"monitor":              "监控数据",
	"xml":                  "域定义",
	"level":                "日志级别",
	"backup":               "系统备份",
	"disk":                 "磁盘",
	"counters":             "计数器",
	"global-bandwidth":     "全局带宽",
	"version":              "版本",
	"export":               "导出",
}

// verbLabels 是真正的动作段：摘要按「动作 + 宾语」组织（绑定直通设备）。
var verbLabels = map[string]string{
	"bind":            "绑定",
	"unbind":          "解绑",
	"ack":             "确认",
	"ack-all":         "全部确认",
	"resend":          "重发",
	"revoke":          "撤销",
	"cancel":          "取消",
	"clear":           "清空",
	"rotate":          "轮换",
	"release":         "释放",
	"apply":           "应用",
	"confirm":         "确认",
	"preview":         "预览",
	"precheck":        "预检",
	"check":           "检查",
	"repair":          "修复",
	"reload":          "重载",
	"reset":           "重置",
	"reset-usage":     "重置用量",
	"trim":            "回收空间",
	"verify":          "校验",
	"send":            "发送",
	"test":            "测试",
	"enable":          "启用",
	"disable":         "停用",
	"skip":            "跳过",
	"accept":          "接受",
	"close":           "关闭",
	"delete":          "删除",
	"delete-all":      "全部删除",
	"read":            "读取",
	"export":          "导出",
	"download":        "下载",
	"file":            "下载",
	"complete":        "完成",
	"parse":           "解析",
	"flatten":         "合并",
	"rebase":          "重定基",
	"preprocess":      "预处理",
	"promote-child":   "提升子卷",
	"promote-delete":  "提升并删除",
	"resize":          "扩容",
	"eject":           "弹出",
	"upload":          "上传",
	"unmount":         "卸载",
	"config":          "配置",
	"reconfigure":     "重新配置",
	"restore":         "恢复",
	"login":           "登录",
	"logout":          "登出",
	"rotate-auth-key": "轮换签名密钥",
}

// actionLabels 是**过程性动作**（不是对某个子资源的读写），保留
// 「对<模块>执行「…」」的措辞：它读起来是一条操作，而不是一次数据访问。
var actionLabels = map[string]string{
	"power":         "电源操作",
	"power-actions": "电源操作",
	"rescue":        "救援模式切换",
	"reinstall":     "重装系统",
	"migrate":       "迁移",
	"clone":         "克隆",
	"lock":          "软锁",
	"timeline":      "时间线",
	"owner":         "归属",
	"batch-actions": "批量操作",
	"batch-clone":   "批量克隆",
	"batch-delete":  "批量删除",
	"force-delete":  "强制删除",
	"trash":         "移入回收站",
	"metadata":      "元数据",
}

// summaryOverrides 是需要精确措辞的接口摘要（"METHOD path"，不含前缀）。
// 启发式生成的摘要对大多数 CRUD 足够，但认证与初始化这类接口的语义
// 值得手写。
var summaryOverrides = map[string]string{
	"POST /auth/login":          "登录（多阶段：ok / login_verify / force_password_change / bootstrap_security）",
	"POST /auth/login/verify":   "完成登录的第二阶段验证（TOTP / 邮箱验证码）",
	"POST /auth/login/password": "首次登录后修改初始密码",
	"POST /setup/admin":         "创建首个管理员账号（仅系统未初始化时可用）",
	"GET /maintenance":          "查询站点维护状态",
	"POST /maintenance/enter":   "进入站点维护（逐节点接管，需二次验证）",
	"POST /maintenance/exit":    "退出站点维护（只解除本次接管的节点）",
	"GET /vms/create-form":      "创建向导的步骤、字段矩阵与前置条件（由后端统一下发）",
	"POST /tasks/stream":        "任务事件的 SSE 实时通道",
	"POST /auth/forgot/send":    "申请找回密码验证码（只发到已验证邮箱）",
	"POST /auth/forgot/reset":   "用一次性重置票据设置新密码",

	"GET /invites/preview":         "公开预览一条邀请（不暴露配额）",
	"POST /invites/accept":         "接受邀请并自助创建账号",
	"GET /nodes/:id/unmanaged-vms": "扫描节点上面板之外的存量域（未纳管清单）",
	"POST /nodes/:id/adopt-vm":     "纳管一个存量域：只补记录，不改动域本身",
	"POST /vms/:id/force-delete":   "强制删除僵尸虚拟机（跳过状态探测，需二次验证）",

	// --- 认证与安全引导：语义无法从路径推断，逐条手写 ---
	"POST /auth/logout":                  "退出登录（吊销当前会话）",
	"PUT /auth/password":                 "修改登录密码（会踢掉其它会话）",
	"PUT /auth/username":                 "修改用户名",
	"PUT /auth/email":                    "更换邮箱（需验证码确认）",
	"POST /auth/email/code":              "发送邮箱验证码",
	"POST /auth/totp/setup":              "开始绑定 TOTP（返回密钥与二维码）",
	"POST /auth/totp/confirm":            "确认绑定 TOTP",
	"POST /auth/recovery-codes":          "重新生成 TOTP 恢复码",
	"POST /auth/risk-verification":       "提交高风险操作的二次验证码",
	"GET /auth/security-setup":           "查询账号安全设置状态",
	"GET /auth/session":                  "查询当前会话",
	"GET /auth/sessions":                 "查询登录会话列表",
	"DELETE /auth/sessions/:id":          "踢出指定的登录会话",
	"POST /auth/bootstrap/complete":      "完成安全初始化引导",
	"POST /auth/bootstrap/skip":          "跳过安全初始化引导",
	"POST /auth/bootstrap/totp/setup":    "开始绑定 TOTP（引导阶段）",
	"POST /auth/bootstrap/totp/confirm":  "确认绑定 TOTP（引导阶段）",
	"POST /auth/bootstrap/email/code":    "发送引导邮箱验证码",
	"POST /auth/bootstrap/email/confirm": "确认引导邮箱",
	"POST /auth/forgot/verify":           "校验找回密码验证码",

	// --- 平台与节点 ---
	"GET /health":      "健康检查（存活与依赖状态）",
	"GET /version":     "查询版本与依赖信息",
	"GET /search":      "跨资源全局搜索",
	"GET /quota":       "查询我的配额用量",
	"GET /my-storage":  "查询我的存储用量",
	"POST /my-storage": "开通我的存储空间",
	"PUT /my-storage/uploads/:uploadID/chunks/:index": "上传一个分片",
	"POST /nodes/registration-tokens":                 "生成节点接入令牌（一次性）",
	"PATCH /nodes/:id/console-host":                   "设置节点控制台对外地址",
	"PATCH /nodes/:id/maintenance":                    "设置节点维护模式",
	"GET /monitor/host/devices":                       "查询宿主机设备清单",
	"GET /settings/access":                            "查询公网访问控制策略",
	"PUT /settings/access":                            "更新公网访问控制策略",
	"GET /settings/auth-key":                          "查询会话签名密钥",
	"PUT /settings/log/level":                         "调整服务端日志级别",
	"POST /settings/mail/test":                        "发送测试邮件",
	"POST /settings/rollback":                         "回滚系统设置到上一版本",

	// --- 网络 ---
	"POST /networks/counters/reset":         "清空网络计数器",
	"POST /networks/global-bandwidth/apply": "应用全局带宽限制",
	"POST /networks/ipv6/policy":            "更新 IPv6 策略",
	"POST /networks/bridges/:id/uplink":     "设置网桥上联",
	"DELETE /networks/bridges/:id/uplink":   "取消网桥上联",
	"GET /tags/vms":                         "查询打了标签的虚拟机",
	"DELETE /public-ips/:id/bind":           "解绑公网 IP",
	"POST /firewall/rollback":               "回滚 KVM 防火墙规则",
	"POST /host-firewall/rollback":          "回滚宿主机防火墙规则",
	"POST /imports/parse":                   "解析导入源文件（探测格式与系统）",

	// --- 存储与模板 ---
	"POST /storage/trim":                 "回收存储空间（TRIM）",
	"POST /templates/:id/flatten":        "合并模板镜像链",
	"POST /templates/:id/preprocess":     "预处理模板（离线准备）",
	"POST /templates/:id/promote-child":  "提升子卷为模板",
	"POST /templates/:id/promote-delete": "提升子卷并删除父模板",
	"POST /templates/:id/rebase":         "变更模板的父镜像",

	// --- 虚拟机 ---
	"GET /vms/trash":                          "查询回收站",
	"POST /vms/:id/disk/resize":               "扩容虚拟机磁盘",
	"POST /vms/:id/disks/independent":         "创建独立数据盘（不随快照回滚）",
	"PUT /vms/:id/cdroms/:cdromID/bus":        "切换光驱总线",
	"PUT /vms/:id/cdroms/:cdromID/iso":        "挂载或更换光驱镜像",
	"POST /vms/:id/config-changes":            "预演虚拟机配置变更",
	"PATCH /vms/:id/console":                  "调整控制台参数（协议与密码）",
	"GET /vms/:id/public-ips/guest-status":    "查询虚拟机公网 IP 的来宾状态",
	"POST /vms/:id/security-groups/apply":     "应用安全组到虚拟机",
	"POST /vms/:id/nvram/repair":              "修复虚拟机 NVRAM",
	"DELETE /vms/:id/reinstall/backup":        "删除重装前的系统备份",
	"POST /vms/:id/xml/precheck":              "预检虚拟机域定义",
	"GET /vms/:id/timeline":                   "查询虚拟机操作时间线",
	"PATCH /vms/:id/metadata":                 "更新虚拟机元数据（备注与分组）",
	"PUT /vms/:id/owner":                      "转移虚拟机归属",
	"POST /vms/port-forwards/batch-delete":    "批量删除端口转发",
	"GET /vms/:id/exports/:exportID/download": "下载导出的文件",
}

// authOf 判定认证方式。
func authOf(relPath, method string) string {
	key := method + " " + relPath
	if publicAPIRoutes[key] {
		return "public"
	}
	if adminAPIRoutes[key] {
		return "admin"
	}
	return "user"
}

// summarizeOf 生成接口摘要：先查覆盖表，其余按「模块 + 子资源 + 动作」推断。
//
// 三条规则：
//  1. 末段是路径参数 → 对该资源的 CRUD，对象取「模块 + 上一层名词」，
//     所以 /security-groups/:id/rules/:id 是「安全组规则」而不是「安全组」；
//  2. 末段是名词或动作段 → 「动词 + 宾语」，宾语优先用最近的子资源名词，
//     没有就用模块的单数说法（/alerts/:id/ack → 确认告警）；
//  3. 其余按方法与模块推断集合操作。
func summarizeOf(method, relPath string) string {
	if summary, ok := summaryOverrides[method+" "+relPath]; ok {
		return summary
	}
	segs := splitPath(relPath)
	if len(segs) == 0 {
		return "接口"
	}
	module := moduleOf(segs)
	last := segs[len(segs)-1]

	// 规则 1：末段是参数（资源详情 / 更新 / 删除）。
	if strings.HasPrefix(last, ":") {
		obj := module
		if len(segs) >= 3 {
			if noun, ok := nounLabels[segs[len(segs)-2]]; ok {
				obj = joinLabel(module, noun)
			}
		}
		switch method {
		case "GET":
			return joinLabel("查询", obj) + "详情"
		case "PATCH", "PUT":
			return joinLabel("更新", obj)
		case "DELETE":
			return joinLabel("删除", obj)
		default:
			return joinLabel("操作", obj)
		}
	}

	// 规则 2：动作段优先于名词段——动作段是「对它做什么」，
	// 名词段是「它是谁的什么」。
	if verb, ok := verbLabels[last]; ok {
		return joinLabel(verb, objectOf(segs, module))
	}
	if action, ok := actionLabels[last]; ok {
		return "对" + spaceIfASCII(module) + module + "执行「" + action + "」"
	}
	// 首段本身是集合（/vms、/settings）：不能把模块名再当一次宾语，
	// 否则会得到「虚拟机虚拟机」。
	if noun, ok := nounLabels[last]; ok && len(segs) > 1 {
		obj := objectForNoun(segs, module, noun)
		return joinLabel(nounVerb(method), obj)
	}

	// 规则 3：纯集合（/vms、/users）。
	if method == "GET" {
		return joinLabel("查询", module) + "列表"
	}
	return joinLabel(collectionVerb(method), module)
}

// objectOf 给动作段找宾语：最近的子资源名词；找不到用模块的单数说法。
//
// 只对名词求值（动作段跳过），因为「对直通设备绑定」这类两层动作在
// 本项目里不存在，跳过不会丢信息。
func objectOf(segs []string, module string) string {
	for i := len(segs) - 2; i >= 1; i-- {
		if noun, ok := nounLabels[segs[i]]; ok {
			return noun
		}
	}
	return singularOf(module)
}

// objectForNoun 给名词段拼完整宾语。
//
// 名词嵌在另一层子资源里时带上那层名字（/settings/log/status →
// 服务端日志状态）；直接挂在模块下（含参数之后）就用「模块 + 名词」，
// 因为「磁盘」单说无法说明是谁的磁盘（/nodes/:id/disks → 节点磁盘）。
func objectForNoun(segs []string, module, noun string) string {
	for i := len(segs) - 2; i >= 0; i-- {
		seg := segs[i]
		if strings.HasPrefix(seg, ":") {
			return joinLabel(module, noun)
		}
		if prev, ok := nounLabels[seg]; ok && i > 0 {
			return joinLabel(prev, noun)
		}
	}
	return joinLabel(module, noun)
}

// nounVerb 是名词段的动作词：GET 是查询，POST 是创建，其余按写操作。
func nounVerb(method string) string {
	switch method {
	case "GET":
		return "查询"
	case "POST":
		return "创建"
	case "PATCH", "PUT":
		return "更新"
	case "DELETE":
		return "删除"
	default:
		return "操作"
	}
}

// collectionVerb 是集合级接口的动作词。
func collectionVerb(method string) string {
	switch method {
	case "GET":
		return "查询"
	case "POST":
		return "创建"
	case "PATCH", "PUT":
		return "更新"
	case "DELETE":
		return "删除"
	default:
		return "操作"
	}
}

// singularOf 取模块的单数说法（动作句的兜底宾语）。
func singularOf(module string) string {
	if s, ok := singularLabels[module]; ok {
		return s
	}
	return module
}

// joinLabel 拼接两个中文词；任一侧是 ASCII 时补一个空格，
// 否则会出现「查询OVS 状态」「OVS状态」这类中英粘连。
func joinLabel(verb, obj string) string {
	if verb == "" || obj == "" {
		return verb + obj
	}
	if isASCII(verb[len(verb)-1]) || isASCII(obj[0]) {
		return verb + " " + obj
	}
	return verb + obj
}

// spaceIfASCII 在字符串以 ASCII 开头时返回一个空格，用于中文前缀之后，
// 避免「对VPC 交换机」这种中英粘连。
func spaceIfASCII(s string) string {
	if s != "" && isASCII(s[0]) {
		return " "
	}
	return ""
}

func isASCII(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// moduleOf 取模块名（找不到时退回首段本身，保证每个接口都有分组）。
func moduleOf(segs []string) string {
	if len(segs) == 0 {
		return "接口"
	}
	if label, ok := moduleLabels[segs[0]]; ok {
		return label
	}
	return segs[0]
}

func splitPath(p string) []string {
	out := []string{}
	for _, s := range strings.Split(p, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// paramsOf 提取路径参数；查询与请求体参数只有少数接口登记在 bodyHints。
func paramsOf(relPath string) []apiDocParam {
	out := []apiDocParam{}
	for _, seg := range splitPath(relPath) {
		if strings.HasPrefix(seg, ":") {
			name := strings.TrimPrefix(seg, ":")
			note := "路径参数"
			if name == "id" {
				note = "资源 ID"
			}
			out = append(out, apiDocParam{Name: name, In: "path", Note: note})
		}
	}
	return out
}

// exampleValue 给路径参数一个示例值，让 curl 拿来即用。
func exampleValue(name string) string {
	switch name {
	case "id":
		return "1"
	case "node_id", "nodeId":
		return "1"
	default:
		return "<" + name + ">"
	}
}

// curlOf 生成可复制的 curl 命令。
//
// 认证头按认证方式给：公开接口不带；其余给 Cookie 占位（API 凭证调用时
// 换成 Authorization: Bearer，见页面底部说明）。
func curlOf(method, fullPath, auth string) string {
	url := "http://127.0.0.1:8080" + fullPath
	// 路径参数替换成示例值。
	for _, seg := range splitPath(fullPath) {
		if strings.HasPrefix(seg, ":") {
			name := strings.TrimPrefix(seg, ":")
			url = strings.Replace(url, seg, exampleValue(name), 1)
		}
	}

	parts := []string{"curl -X " + method + " '" + url + "'"}
	if auth != "public" {
		parts = append(parts, "-H 'Cookie: kc_session=<你的会话>'")
	}
	switch method {
	case "POST", "PUT", "PATCH":
		parts = append(parts, "-H 'Content-Type: application/json'", "-d '{}'")
	}
	return strings.Join(parts, " \\\n  ")
}
