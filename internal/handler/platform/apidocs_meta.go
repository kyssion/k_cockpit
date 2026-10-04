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
	// dev 注册入口只在 AGENT_TRANSPORT=mock 时注册（见 router.Register），
	// 出现时就是公开的。
	"POST /dev/agent-register": true,
}

// adminAPIRoutes 是仅管理员可访问的路由（不含 /api/v1 前缀）。
//
// 同样从 router.Register 机械提取（带 adminOnly 中间件的那些行）。
// 未列出的已登录路由即租户可用；以 docs/03-api/API.md 为最终权威。
var adminAPIRoutes = map[string]bool{
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
	"GET /network/client-ip":                    true,
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
var moduleLabels = map[string]string{
	"health":             "健康检查",
	"setup":              "初始化",
	"auth":               "认证与会话",
	"account":            "账号自管理",
	"vms":                "虚拟机",
	"vm-tags":            "虚拟机标签",
	"templates":          "模板",
	"template-exports":   "模板导出",
	"nodes":              "节点",
	"maintenance":        "站点维护",
	"tasks":              "任务",
	"storage-pools":      "存储池",
	"storage-partitions": "磁盘分区",
	"storage-volumes":    "存储卷",
	"my-storage":         "我的存储",
	"share-mounts":       "目录共享",
	"networks":           "网络中心",
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
	"users":              "用户管理",
	"invites":            "邀请注册",
	"quotas":             "存储配额",
	"compute-quotas":     "计算配额",
	"resource-quotas":    "周期配额",
	"settings":           "系统设置",
	"request-logs":       "请求日志",
	"scheduler":          "调度器",
	"schedules":          "定时任务",
	"dashboard":          "工作台",
	"monitor":            "监控",
	"search":             "全局搜索",
	"alerts":             "告警中心",
	"logs":               "服务端日志",
	"diagnostics":        "诊断",
	"api-keys":           "API 凭证",
	"action-tokens":      "一次性动作令牌",
	"security":           "安全中心",
	"platform-check":     "平台自检",
	"host-tuning":        "宿主机调优",
	"access-control":     "公网访问控制",
	"versions":           "版本与依赖",
	"dev":                "开发期入口",
}

// actionGlossary 把常见的动作段翻译成中文；未登记的动作原样保留。
var actionGlossary = map[string]string{
	"power":     "电源操作",
	"rescue":    "救援模式切换",
	"migrate":   "迁移",
	"clone":     "克隆",
	"snapshot":  "快照操作",
	"console":   "控制台",
	"tags":      "标签",
	"lock":      "软锁",
	"owner":     "归属",
	"timeline":  "时间线",
	"disks":     "磁盘",
	"cdrom":     "光驱",
	"nic":       "网卡",
	"reinstall": "重装系统",
	"enter":     "进入",
	"exit":      "退出",
	"reset":     "重置",
	"reload":    "重载规则",
	"rollback":  "回滚",
	"preview":   "预览",
	"apply":     "应用",
	"rotate":    "轮换",
	"upload":    "上传",
	"download":  "下载",
	"purge":     "清理",
	"trim":      "回收空间",
	"unbind":    "解除绑定",
	"release":   "释放",
	"verify":    "校验",
	"send":      "发送",
	"test":      "测试",
	"clear":     "清空",
}

// summaryOverrides 是需要精确措辞的接口摘要（"METHOD path"，不含前缀）。
// 启发式生成的摘要对大多数 CRUD 足够，但认证与初始化这类接口的语义
// 值得手写。
var summaryOverrides = map[string]string{
	"POST /auth/login":             "登录（多阶段：ok / login_verify / force_password_change / bootstrap_security）",
	"POST /auth/login/verify":      "完成登录的第二阶段验证（TOTP / 邮箱验证码）",
	"POST /auth/login/password":    "首次登录后修改初始密码",
	"POST /setup/admin":            "创建首个管理员账号（仅系统未初始化时可用）",
	"GET /maintenance":             "查询站点维护状态",
	"POST /maintenance/enter":      "进入站点维护（逐节点接管，需二次验证）",
	"POST /maintenance/exit":       "退出站点维护（只解除本次接管的节点）",
	"GET /vms/create-form":         "创建向导的步骤、字段矩阵与前置条件（由后端统一下发）",
	"POST /tasks/stream":           "任务事件的 SSE 实时通道",
	"POST /auth/forgot/send":       "申请找回密码验证码（只发到已验证邮箱）",
	"POST /auth/forgot/reset":      "用一次性重置票据设置新密码",
	"POST /dev/agent-register":     "模拟 agent 注册（仅 AGENT_TRANSPORT=mock 时注册该路由）",
	"GET /invites/preview":         "公开预览一条邀请（不暴露配额）",
	"POST /invites/accept":         "接受邀请并自助创建账号",
	"GET /nodes/:id/unmanaged-vms": "扫描节点上面板之外的存量域（未纳管清单）",
	"POST /nodes/:id/adopt-vm":     "纳管一个存量域：只补记录，不改动域本身",
	"POST /vms/:id/force-delete":   "强制删除僵尸虚拟机（跳过状态探测，需二次验证）",
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

// summarizeOf 生成接口摘要：先查覆盖表，其余按方法与路径形状推断。
func summarizeOf(method, relPath string) string {
	if s, ok := summaryOverrides[method+" "+relPath]; ok {
		return s
	}

	segs := splitPath(relPath)
	module := moduleOf(segs)
	// 最后一个路径段是动作还是资源，决定措辞。
	last := segs[len(segs)-1]
	hasParam := strings.Contains(relPath, ":")

	switch {
	case last != "" && strings.HasPrefix(last, ":"):
		// 以参数结尾：典型 CRUD。
		switch method {
		case "GET":
			return "查询" + module + "详情"
		case "PATCH", "PUT":
			return "更新" + module
		case "DELETE":
			return "删除" + module
		default:
			return "操作" + module
		}
	case hasParam:
		// 参数之后的动作段。
		action, ok := actionGlossary[last]
		if !ok {
			action = last
		}
		return "对" + module + "执行「" + action + "」"
	default:
		switch method {
		case "GET":
			return "查询" + module + "列表"
		case "POST":
			return "创建" + module
		case "PATCH", "PUT":
			return "更新" + module
		case "DELETE":
			return "删除" + module
		default:
			return "操作" + module
		}
	}
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
