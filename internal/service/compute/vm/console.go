package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/model"
	"k_cockpit/internal/platform/api"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/authz"
	"k_cockpit/internal/platform/cryptoutil"
)

// ConsoleSessionLimit 是同一虚拟机的控制台会话上限（R-014）。
//
// 取 3 而不是更大：控制台会话会占用转发资源与一条 agent 流，而同一个
// 用户极少需要同时开三个画面。设上限的目的不是限制正常使用，而是避免
// 「反复重连却没释放旧会话」把资源耗尽——那种情况下，没有上限的表现是
// 整个节点的控制台都打不开，而且很难定位。
const ConsoleSessionLimit = 3

// screenshotTTL 是截帧的缓存时长。
//
// 复用它而不是每次请求都向节点要一张图：控制台预览是按秒刷新的，
// 多开几个页面就会变成对节点的持续轮询。
const screenshotTTL = 5 * time.Second

// DefaultVNCPort 是 VNC 的默认端口。
const DefaultVNCPort = 5900

// DefaultSPICEPort 是 SPICE 的默认端口。
//
// 与 VNC 分开取段（5900 / 5901 起）是 libvirt 的惯例，也是**让两者能同时
// 开**的前提——同一个端口上只能有一个监听者。
const DefaultSPICEPort = 5901

// 控制台协议。
//
// 做成一个维度而不是两套接口：SPICE 的「对外暴露」与 VNC 是**同一类风险**
// ——都是暴露一个远程控制入口。另起一套意味着把二次验证、监听地址切换、
// 警告文案再写一遍，而那两份迟早会分叉：某天有人给 VNC 那条加了更严的
// 限制，而 SPICE 那条还开着，且不会有任何地方报错。
const (
	ConsoleProtocolVNC   = "vnc"
	ConsoleProtocolSPICE = "spice"
)

// ConsoleConfig 是控制台的对外视图。
//
// **不含密码**，也不含任何可以推导出密码的字段（R-005）：只支持设置新密码，
// 不支持查看。这不是「暂时不方便」，而是密码一旦可读，加密存储的意义就
// 只剩下防止直接拖库——而最大的泄漏面恰恰是通过接口。
type ConsoleConfig struct {
	VMID int64 `json:"vm_id"`

	// Available 表示该虚拟机是否有控制台（R-011）。
	//
	// 显示设备为 none 的虚拟机没有画面可看。返回明确的不可用原因，
	// 而不是让界面展示一个点了打不开的按钮。
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`

	// Protocols 是这台虚拟机**可用的**控制台协议。
	//
	// 由节点上报：SPICE 需要 libvirt 编译时带 SPICE 支持，而 VNC 几乎总是
	// 可用。不给这个列表的话，界面会显示一个点了打不开的 SPICE 选项。
	Protocols []string `json:"protocols"`

	// 下面是**当前所选协议**的状态。
	//
	// 不把两种协议各返回一份，是因为界面上同一时刻只在配一种——返回两份
	// 会让「我改的是哪个」变成需要从数据里推断的事。
	Protocol string `json:"protocol"`

	Enabled       bool   `json:"enabled"`
	Port          int    `json:"port,omitempty"`
	Bind          string `json:"bind,omitempty"`
	Exposed       bool   `json:"exposed"`
	HasPassword   bool   `json:"has_password"`
	DisplayDevice string `json:"display_device"`

	// Host 是控制台**对外可达**的地址（取自节点的 console_host）。
	//
	// 它与 Bind 是两件事：Bind 是宿主机上的监听地址（常常是 127.0.0.1），
	// 而 Host 是"从用户网络看过去该连哪里"。生成 SPICE 连接文件要用后者，
	// 用前者的连接文件在用户机器上必然连不上。
	//
	// 为空表示管理员还没给这个节点填地址，此时不提供连接文件——猜一个
	// 地址的代价是"下载了却连不上"，而用户无从判断是控制台没开还是地址错。
	Host string `json:"host,omitempty"`

	// ActiveSessions 与 SessionLimit 供界面判断还能不能再开一个（R-014）。
	ActiveSessions int `json:"active_sessions"`
	SessionLimit   int `json:"session_limit"`

	// StreamSupported 表示当前 agent 是否支持流式转发。
	//
	// 为 false 时控制台打不开，但原因不是配置问题而是传输能力缺失——
	// 界面据此给出不同的提示，避免用户去反复检查「控制台是不是没开」。
	StreamSupported bool `json:"stream_supported"`
}

// ConsoleUpdate 是控制台配置的变更请求。
type ConsoleUpdate struct {
	// Protocol 选择要配置哪一种（vnc / spice），留空按 VNC。
	//
	// 两种协议共用**同一段**更新逻辑与全部守卫（二次验证、监听地址切换、
	// 关闭时释放会话）——只有落库的字段名不同。见 UpdateConsole 里的前缀。
	Protocol string
	// Enabled 开启或关闭控制台。
	Enabled *bool
	// Password 设置新的控制台密码。**只写不读**（R-005）。
	Password *string
	// Exposed 切换对外暴露。**高危操作**，受理前必须已完成二次验证（R-004）。
	Exposed *bool
}

// maxVNCPasswordLen 是 VNC 协议的**硬限制**：口令最多 8 个字节。
//
// 这是协议决定的（RFB 的口令固定 8 字节），不是我们的偏好。超长口令 libvirt
// 会拒绝，但拒绝的位置与说法不一致（第 3 轮 DEF-R4-02 实测）：
//   - 未启用控制台时走另一条入口，得到 400「控制台密码最长 8 位（这是 VNC 协议的限制）」；
//   - 已启用时口令被原样下发，libvirt 回来的是 422 加英文原文
//     `VNC password is 18 characters long, only 8 permitted`。
//
// 同一个约束、两种状态码、两套文案，用户会以为是两件事。现在统一在
// UpdateConsole 入口判一次，两条分支共用同一句话。
//
// **按字节计**而不是 rune：libvirt 与 RFB 都按字节数算，一个 8 字符的中文口令
// 是 24 字节，下发必然被拒——用 rune 计数会让这条校验形同虚设。
const maxVNCPasswordLen = 8

// validateConsolePassword 校验控制台口令；非 VNC 协议不适用（SPICE 无此限制）。
func validateConsolePassword(protocol string, pw *string) error {
	if protocol != ConsoleProtocolVNC || pw == nil {
		return nil
	}
	if len(*pw) > maxVNCPasswordLen {
		// 沿用既有那句文案（400 / InvalidParameter）：这是**输入不合法**，
		// 不是"服务端校验没过"。两条分支现在共用这一句。
		return api.InvalidParameter(
			"控制台密码最长 8 位（这是 VNC 协议的限制；按字节计算，一个中文字符算 3 位）")
	}
	return nil
}

// ConsoleSession 是一次控制台会话。
type ConsoleSession struct {
	ID        string
	VMID      int64
	UserID    int64
	StartedAt time.Time
}

// sessions 管理控制台会话（R-007 / R-014）。
type sessionRegistry struct {
	mu      sync.Mutex
	byID    map[string]*ConsoleSession
	perVM   map[int64]int
	nextSeq uint64
}

func newSessionRegistry() *sessionRegistry {
	return &sessionRegistry{
		byID:  make(map[string]*ConsoleSession),
		perVM: make(map[int64]int),
	}
}

// open 登记一个会话；达到上限时返回 false。
func (r *sessionRegistry) open(vmID, userID int64) (*ConsoleSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.perVM[vmID] >= ConsoleSessionLimit {
		return nil, false
	}

	r.nextSeq++
	session := &ConsoleSession{
		ID:        fmt.Sprintf("cs-%d-%d", vmID, r.nextSeq),
		VMID:      vmID,
		UserID:    userID,
		StartedAt: time.Now(),
	}
	r.byID[session.ID] = session
	r.perVM[vmID]++
	return session, true
}

// close 释放一个会话，返回它是否存在。
func (r *sessionRegistry) close(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	session, ok := r.byID[id]
	if !ok {
		return false
	}
	delete(r.byID, id)

	if r.perVM[session.VMID] > 0 {
		r.perVM[session.VMID]--
	}
	if r.perVM[session.VMID] == 0 {
		delete(r.perVM, session.VMID)
	}
	return true
}

// count 返回某虚拟机的活跃会话数。
func (r *sessionRegistry) count(vmID int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.perVM[vmID]
}

// Console 返回控制台配置与状态（API-030）。
//
// protocol 为空时按 VNC——那是绝大多数情况，也是历史行为。
func (s *Service) Console(
	ctx context.Context, id int64, protocol string, v authz.Viewer,
) (*ConsoleConfig, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	return s.consoleConfig(ctx, vm, protocol)
}

func (s *Service) consoleConfig(
	ctx context.Context, vm *model.VM, protocol string,
) (*ConsoleConfig, error) {
	// **协议只决定取哪几个字段，不决定走哪段逻辑。**
	protocol = normalizeProtocol(protocol)

	cfg := &ConsoleConfig{
		VMID: vm.ID,
		// Protocols 是**可用**的协议列表（由能力探测给），不是当前选的。
		// SPICE 需要 libvirt 编译时带支持，而 VNC 几乎总是可用——不给
		// 这个列表的话，界面会显示一个点了打不开的 SPICE 选项。
		Protocols:       availableProtocols(vm),
		Protocol:        protocol,
		Enabled:         enabledOf(vm, protocol),
		Bind:            bindOf(vm, protocol),
		Exposed:         exposedOf(vm, protocol),
		DisplayDevice:   vm.DisplayDevice,
		SessionLimit:    ConsoleSessionLimit,
		ActiveSessions:  s.consoleSessions().count(vm.ID),
		StreamSupported: s.streamSupported(),
	}
	// 端口按**当前所选协议**取：两种协议各自监听在不同端口上（VNC 5900 段、
	// SPICE 5901 段），取错的后果是连接文件指向一个别的服务的端口，而用户
	// 会以为是控制台没开。
	switch protocol {
	case ConsoleProtocolSPICE:
		if vm.SPICEPort != nil {
			cfg.Port = *vm.SPICEPort
		}
	default:
		if vm.VNCPort != nil {
			cfg.Port = *vm.VNCPort
		}
	}

	// 显示设备为 none 的虚拟机没有控制台（R-011）。这里给出**原因**，
	// 而不是返回一份「可用但打不开」的配置。
	if !vm.HasConsole() {
		cfg.Available = false
		cfg.UnavailableReason = "该虚拟机没有图形显示设备，无法提供控制台"
		return cfg, nil
	}
	cfg.Available = true

	hasPassword, err := s.hasConsolePassword(ctx, vm.ID)
	if err != nil {
		return nil, err
	}
	cfg.HasPassword = hasPassword

	// 控制台对外地址来自节点，而不是虚拟机：同一台宿主机上的所有虚拟机
	// 共用同一个入口地址，让每台机器各存一份只会在迁移后全部过期。
	var node model.Node
	if err := s.db.WithContext(ctx).Where("id = ?", vm.NodeID).
		Select("id", "console_host").First(&node).Error; err == nil && node.ConsoleHost != nil {
		cfg.Host = *node.ConsoleHost
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("[vm] 查询节点控制台地址失败 node=%d: %v", vm.NodeID, err)
	}

	return cfg, nil
}

// ConsoleConnectionFile 生成 SPICE 连接文件（.vv）的内容与文件名。
//
// 为什么只支持 SPICE：VNC 没有标准的连接文件格式，而浏览器里的 VNC 由
// noVNC 走面板代理——那条路已经可用。SPICE 恰恰相反：它的体验优势全在
// 本地客户端上（USB 重定向、剪贴板、音频），而这些只有在 virt-viewer
// 里才存在。
//
// 密码是**可选**的：默认不写进文件（用户手输），只有显式请求时才带，且
// 那时必须已经完成二次验证。控制台密码属于凭据，"只写不读"（R-005）那
// 条原则的落点就在这里——默认路径不泄漏。
func (s *Service) ConsoleConnectionFile(
	ctx context.Context, id int64, protocol string, withPassword bool,
	v authz.Viewer, operatorName, clientIP string,
) (*ConsoleFile, error) {
	protocol = normalizeProtocol(protocol)
	if protocol != ConsoleProtocolSPICE {
		return nil, api.ValidationFailed("只有 SPICE 提供连接文件；VNC 请在浏览器中使用内置控制台")
	}

	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if !vm.HasConsole() {
		return nil, api.ValidationFailed("该虚拟机没有图形显示设备，无法提供控制台")
	}
	cfg, err := s.consoleConfig(ctx, vm, protocol)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, api.ValidationFailed("SPICE 控制台尚未开启")
	}
	if cfg.Host == "" {
		return nil, api.ValidationFailed(
			"该节点还没有配置控制台的对外地址，无法生成连接文件（请管理员在节点上填写）")
	}

	password := ""
	if withPassword {
		if !cfg.HasPassword {
			return nil, api.ValidationFailed("该控制台尚未设置密码，无需写入连接文件")
		}
		p, err := s.consolePassword(ctx, vm.ID)
		if err != nil {
			return nil, err
		}
		password = p
	}

	s.record(ctx, audit.Entry{
		OperatorID: v.UserID, OperatorName: operatorName,
		NodeID: vm.NodeID, ResourceType: "vm", ResourceID: vm.ID, ResourceName: vm.Name,
		Action:     "vm.console.connection_file",
		Params:     map[string]any{"protocol": protocol, "with_password": withPassword},
		AfterState: map[string]any{"host": cfg.Host, "port": cfg.Port},
		Success:    true,
		ClientIP:   clientIP,
	})

	return &ConsoleFile{
		Filename: spiceFileName(vm.Name),
		Content:  buildVV(cfg.Host, cfg.Port, vm.Name, password),
	}, nil
}

// ConsoleFile 是一个待下载的连接文件。
type ConsoleFile struct {
	Filename string
	Content  string
}

// spiceFileName 生成下载文件名。
func spiceFileName(vmName string) string {
	return sanitizeFilePart(vmName) + "-spice.vv"
}

// sanitizeFilePart 去掉文件名里不安全的字符。
//
// 虚拟机名允许的字符比文件名广，直接拼进 Content-Disposition 会让一个
// 含换行或引号的名字注入额外的响应头——那是实打实的头注入，不只是
// "文件名难看"。
func sanitizeFilePart(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "vm"
	}
	return out
}

// buildVV 构造 .vv 文件内容（INI 形态）。
//
// 字段顺序固定、只写我们认识的键：这个文件由外部程序解析，而它的格式
// 容忍度高到可以塞进任意键——只输出固定的几行是唯一可靠的做法。
func buildVV(host string, port int, vmName, password string) string {
	var b strings.Builder
	b.WriteString("[virt-viewer]\n")
	b.WriteString("type=spice\n")
	b.WriteString("host=" + host + "\n")
	if port > 0 {
		b.WriteString("port=" + strconv.Itoa(port) + "\n")
	}
	b.WriteString("title=" + vmName + "\n")
	if password != "" {
		b.WriteString("password=" + password + "\n")
	}
	b.WriteString("delete-this-file=1\n")
	return b.String()
}

// UpdateConsole 更新控制台配置（API-031）。
//
// 同步完成而不入队：这些变更只改控制面记录并对节点下发一条轻量指令，
// 没有耗时可言。做成异步任务会让用户点一下「开启控制台」还要去任务中心
// 看结果。
func (s *Service) UpdateConsole(
	ctx context.Context, id int64, req ConsoleUpdate, v authz.Viewer, operatorName, clientIP string,
) (*ConsoleConfig, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNodeUsable(ctx, vm.NodeID); err != nil {
		return nil, err
	}
	if !vm.HasConsole() {
		return nil, api.ValidationFailed("该虚拟机没有图形显示设备，无法开启控制台")
	}

	// **协议只决定落库的字段名，不决定走哪段逻辑。**
	//
	// 这是本函数里最要紧的一处设计：二次验证、监听地址切换、关闭时释放
	// 会话——这三件事对两种控制台完全一样，因此它们只有一份实现，只是
	// 写入的前缀不同。分成两个函数的话，某天给 VNC 那条加了更严的限制，
	// 而 SPICE 那条还开着，且不会有任何地方报错。
	protocol := req.Protocol
	if protocol != ConsoleProtocolSPICE {
		protocol = ConsoleProtocolVNC
	}

	// 口令长度在**入口**统一校验（第 3 轮 DEF-R4-02）：启用 / 未启用两条分支
	// 共用同一句文案与同一个状态码。
	if err := validateConsolePassword(protocol, req.Password); err != nil {
		return nil, err
	}
	isSPICE := protocol == ConsoleProtocolSPICE
	prefix := protocol + "_"

	// SPICE 是 libvirt 的**编译期可选**能力。节点没上报支持时直接拒绝：
	// 落库只会得到一份指向不存在端口的连接文件，而用户在下载之前不会知道。
	if isSPICE && !vm.SPICESupported {
		return nil, api.ValidationFailed(
			"当前节点未上报 SPICE 支持（libvirt 编译时未启用 spice），无法开启 SPICE 控制台")
	}

	enabled := enabledOf(vm, protocol)
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	exposed := exposedOf(vm, protocol)
	if req.Exposed != nil {
		exposed = *req.Exposed
	}
	if req.Exposed != nil && *req.Exposed && !enabled {
		return nil, api.ValidationFailed("控制台未开启时不能对外暴露，请先开启控制台")
	}

	// 监听地址与端口是**节点上的事实**，不是控制面的偏好：必须写进域 XML
	// 才真的生效。暴露时监听所有地址，否则收在回环上（R-001）。
	listen := "127.0.0.1"
	if exposed {
		listen = "0.0.0.0"
	}
	port := 0
	if isSPICE {
		if vm.SPICEPort != nil {
			port = *vm.SPICEPort
		}
	} else if vm.VNCPort != nil {
		port = *vm.VNCPort
	}

	if req.Exposed != nil {
		// 高危：受理前**必须**已完成二次验证（R-004）。检查放在服务层
		// 而不是只在 handler：服务可能被其他入口调用，把安全判定放在
		// 一个入口上，等于给另一个入口留了缺口。
		//
		// **这一处对两种协议是同一段代码**——见上面的说明。
		if err := s.ensureExposureVerified(ctx); err != nil {
			return nil, err
		}
	}

	// 先下发、后落库：节点拒绝时控制面不能留下一个"看起来已生效"的配置。
	// 这是本函数与其它元数据修改最大的不同——控制台配置的真相在域 XML 里。
	needsNode := req.Enabled != nil || req.Exposed != nil || req.Password != nil
	restartRequired := false
	if needsNode {
		params := map[string]any{
			"protocol": protocol,
			"enabled":  enabled,
			"listen":   listen,
			"port":     port,
		}
		if req.Password != nil {
			params["password"] = *req.Password
		}
		res, err := s.agent.Execute(ctx, agent.Operation{
			Kind:   agent.OpVMConsoleUpdate,
			NodeID: vm.NodeID,
			Target: vm.Name,
			Params: params,
		})
		if err != nil {
			// 未送达 ≠ 已执行：明确说明"配置未变更"，避免用户以为点了就生效。
			return nil, api.Unavailable("节点不可达，控制台配置未变更")
		}
		if !res.Success {
			return nil, api.ValidationFailed(res.Message)
		}
		if p, ok := res.Data[agent.ConsoleUpdatePortKey].(float64); ok && p > 0 {
			port = int(p)
		}
		restartRequired, _ = res.Data[agent.ConsoleUpdateRestartRequiredKey].(bool)
	}

	updates := map[string]any{}
	action := "vm.console.update"
	after := map[string]any{"protocol": protocol}

	if req.Enabled != nil {
		updates[prefix+"enabled"] = *req.Enabled
		after["enabled"] = *req.Enabled
		if *req.Enabled {
			action = "vm.console.enable"
		} else {
			action = "vm.console.disable"
		}
	}

	if req.Exposed != nil {
		updates[prefix+"exposed"] = *req.Exposed
		updates[prefix+"bind"] = listen
		after["exposed"] = *req.Exposed
		action = "vm.console.exposure"
	}

	if port > 0 {
		updates[prefix+"port"] = port
	}
	if restartRequired {
		after["restart_required"] = true
	}

	if req.Password != nil {
		if err := s.setConsolePassword(ctx, vm.ID, *req.Password); err != nil {
			return nil, err
		}
		action = "vm.console.password"
		// **不记录密码本身**，只记录「已改过」（R-005 与 f-9-01 R-011 同理）。
		after["password"] = "[已更新]"
	}

	if len(updates) > 0 {
		if err := s.db.WithContext(ctx).Model(&model.VM{}).
			Where("id = ?", vm.ID).Updates(updates).Error; err != nil {
			log.Printf("[vm] 更新控制台配置失败 id=%d: %v", vm.ID, err)
			return nil, api.Internal()
		}
	}

	// 关闭控制台时**释放现有会话**（R-007）：留着它们等于让已经关闭的
	// 控制台继续占用转发资源，而用户看不到任何画面。
	if req.Enabled != nil && !*req.Enabled {
		s.consoleSessions().closeByVM(vm.ID)
	}

	s.record(ctx, audit.Entry{
		OperatorName: operatorName,
		NodeID:       vm.NodeID,
		ResourceType: "vm",
		ResourceID:   vm.ID,
		ResourceName: vm.Name,
		Action:       action,
		AfterState:   after,
		Success:      true,
		ClientIP:     clientIP,
	})

	reloaded, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	return s.consoleConfig(ctx, reloaded, req.Protocol)
}

// OpenConsole 建立一次控制台会话并返回字节流。
//
// 鉴权与授权在**建立连接时**完成（R-003）：WebSocket 升级之后就没有
// HTTP 状态码可用了，那时再发现「这台虚拟机不是你的」已经没有合适的
// 表达方式。
func (s *Service) OpenConsole(ctx context.Context, id int64, viewerID int64, v authz.Viewer) (agent.Stream, *ConsoleSession, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, nil, err
	}
	if !vm.HasConsole() {
		return nil, nil, api.ValidationFailed("该虚拟机没有图形显示设备，无法打开控制台")
	}
	if !vm.VNCEnabled {
		return nil, nil, api.ValidationFailed("控制台尚未开启")
	}

	opener, ok := s.agent.(agent.StreamOpener)
	if !ok {
		// 传输能力缺失与配置问题要分开说：让用户去检查「控制台开没开」
		// 是误导，它明明已经开了。
		return nil, nil, api.Unavailable("当前节点的 agent 不支持控制台流式通道")
	}

	session, ok := s.consoleSessions().open(vm.ID, viewerID)
	if !ok {
		return nil, nil, api.Conflict(
			fmt.Sprintf("该虚拟机的控制台会话已达上限（%d），请关闭其他控制台后重试", ConsoleSessionLimit),
		)
	}

	stream, err := opener.OpenStream(ctx, agent.StreamVNC, vm.NodeID, vm.Name)
	if err != nil {
		s.consoleSessions().close(session.ID)
		if errors.Is(err, agent.ErrStreamUnsupported) {
			return nil, nil, api.Unavailable("当前节点的 agent 不支持控制台数据流")
		}
		return nil, nil, api.Unavailable("无法建立控制台通道")
	}

	return stream, session, nil
}

// CloseConsole 释放一次控制台会话。
func (s *Service) CloseConsole(sessionID string) {
	s.consoleSessions().close(sessionID)
}

// streamSupported 报告当前 agent 是否支持流式转发。
func (s *Service) streamSupported() bool {
	_, ok := s.agent.(agent.StreamOpener)
	return ok
}

// ensureExposureVerified 校验对外暴露是否已通过二次验证。
//
// 具体的许可校验在 handler 层完成（那里才有请求上下文），这里只做一次
// 兜底检查：调用方没有标记「已验证」时一律拒绝。这样即使将来有人新增了
// 一个绕过 guard 的入口，也不会静默地把端口暴露出去。
func (s *Service) ensureExposureVerified(ctx context.Context) error {
	if verified := ctx.Value(ctxKeyExposureVerified); verified == true {
		return nil
	}
	return api.ValidationFailed("对外暴露控制台需要先完成二次验证")
}

// ctxKeyExposureVerified 是「已完成暴露验证」在 context 中的键。
type ctxKeyExposureVerifiedType struct{}

var ctxKeyExposureVerified = ctxKeyExposureVerifiedType{}

// WithExposureVerified 标记本次调用已完成暴露验证。
func WithExposureVerified(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyExposureVerified, true)
}

// hasConsolePassword 报告是否已设置控制台密码。
func (s *Service) hasConsolePassword(ctx context.Context, vmID int64) (bool, error) {
	var count int64
	err := s.db.WithContext(ctx).Model(&model.VMCredential{}).
		Where("vm_id = ? AND username = ?", vmID, model.CredentialVNC).
		Count(&count).Error
	if err != nil {
		log.Printf("[vm] 查询控制台密码失败: %v", err)
		return false, api.Internal()
	}
	return count > 0, nil
}

// setConsolePassword 加密保存控制台密码（R-005）。
func (s *Service) setConsolePassword(ctx context.Context, vmID int64, password string) error {
	if password == "" {
		return api.InvalidParameter("密码不能为空")
	}
	// 长度规则**只在这一处定义**（validateConsolePassword），这里复用而不是
	// 再写一遍：第 3 轮 DEF-R4-02 的根因就是同一个约束散在两处（入口没校验，
	// 这里校验，于是启用/未启用两条分支给出不同状态码与文案）。
	if err := validateConsolePassword(ConsoleProtocolVNC, &password); err != nil {
		return err
	}
	if s.encKey == nil {
		return api.Internal()
	}

	encrypted, err := cryptoutil.Seal(s.encKey, password)
	if err != nil {
		log.Printf("[vm] 加密控制台密码失败: %v", err)
		return api.Internal()
	}

	var existing model.VMCredential
	err = s.db.WithContext(ctx).
		Where("vm_id = ? AND username = ?", vmID, model.CredentialVNC).
		First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		purpose := model.CredentialVNC
		row := model.VMCredential{
			VMID:        vmID,
			Username:    &purpose,
			PasswordEnc: encrypted,
		}
		if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
			log.Printf("[vm] 保存控制台密码失败: %v", err)
			return api.Internal()
		}
		return nil
	case err != nil:
		log.Printf("[vm] 查询控制台密码失败: %v", err)
		return api.Internal()
	}

	err = s.db.WithContext(ctx).Model(&model.VMCredential{}).
		Where("id = ?", existing.ID).
		Updates(map[string]any{"password_enc": encrypted}).Error
	if err != nil {
		log.Printf("[vm] 更新控制台密码失败: %v", err)
		return api.Internal()
	}
	return nil
}

// consolePassword 解密读出控制台密码。
//
// 它只有一个调用点（生成带密码的连接文件），且那一条路径受二次验证保护。
// 单独成函数而不是内联，是为了让"明文密码离开服务端"这件事在代码里只有
// 一处可审计。
func (s *Service) consolePassword(ctx context.Context, vmID int64) (string, error) {
	if s.encKey == nil {
		return "", api.Internal()
	}
	var row model.VMCredential
	err := s.db.WithContext(ctx).
		Where("vm_id = ? AND username = ?", vmID, model.CredentialVNC).
		First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", api.ValidationFailed("该控制台尚未设置密码")
	case err != nil:
		log.Printf("[vm] 查询控制台密码失败: %v", err)
		return "", api.Internal()
	}
	plain, err := cryptoutil.Open(s.encKey, row.PasswordEnc)
	if err != nil {
		// 解密失败通常意味着根密钥变了（重启时未持久化配置），
		// 而不是"密码错了"——按内部错误处理，避免用户去反复改密码。
		log.Printf("[vm] 解密控制台密码失败 vm=%d: %v", vmID, err)
		return "", api.Internal()
	}
	return plain, nil
}

// closeByVM 释放某虚拟机的全部会话。
func (r *sessionRegistry) closeByVM(vmID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for id, session := range r.byID {
		if session.VMID == vmID {
			delete(r.byID, id)
		}
	}
	delete(r.perVM, vmID)
}

// consoleSessions 返回会话注册表，首次访问时惰性创建。
func (s *Service) consoleSessions() *sessionRegistry {
	s.sessionsOnce.Do(func() {
		s.sessions = newSessionRegistry()
	})
	return s.sessions
}

// normalizeProtocol 把协议名归一化。未知值一律按 VNC——
// 那是最保守的选择（VNC 几乎总是可用）。
func normalizeProtocol(p string) string {
	if strings.EqualFold(strings.TrimSpace(p), ConsoleProtocolSPICE) {
		return ConsoleProtocolSPICE
	}
	return ConsoleProtocolVNC
}

// availableProtocols 返回该虚拟机可用的控制台协议。
//
// **VNC 恒定可用**：libvirt 无论怎么编译都带它。SPICE 则是编译期可选的，
// 因此它以节点的上报为准（这里用 DisplayDevice 之外的一个保守判断：
// 只有节点确认过才列出来，见 ConsoleConfig.Protocols 的说明）。
func availableProtocols(vm *model.VM) []string {
	out := []string{ConsoleProtocolVNC}
	if vm.SPICESupported {
		out = append(out, ConsoleProtocolSPICE)
	}
	return out
}

// nodeSupportsSPICE 报告节点当前上报的能力清单里是否含 SPICE。
//
// `vm.spice_supported` 是**节点能力在 VM 行上的投影**：心跳会周期性把它与
// 节点自报的能力对齐（agent/hub 的 onHeartbeat），包括能力消失时收回。
// 但心跳最长 10 秒一次，而新建/新纳管的 VM 在那之前是 false —— 用户往往
// 就在这一刻去开控制台，于是「节点明明支持、界面却只给 vnc、PATCH 直接
// 422」（实机复现）。因此创建与纳管时按节点当前能力初始化，把这个窗口
// 消掉；之后的漂移仍由心跳收敛。
//
// 读不到节点、没有能力清单、解析失败一律按**不支持**处理：宁可让用户等
// 一次心跳，也不要让界面摆出一个点了打不开的协议入口。
func nodeSupportsSPICE(ctx context.Context, db *gorm.DB, nodeID int64) bool {
	var row model.Node
	if err := db.WithContext(ctx).Select("capabilities").First(&row, nodeID).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("[vm] 查询节点能力失败 node=%d: %v", nodeID, err)
		}
		return false
	}
	if row.Capabilities == nil || strings.TrimSpace(*row.Capabilities) == "" {
		return false
	}
	var caps []string
	if err := json.Unmarshal([]byte(*row.Capabilities), &caps); err != nil {
		log.Printf("[vm] 解析节点能力失败 node=%d: %v", nodeID, err)
		return false
	}
	for _, c := range caps {
		if c == agent.CapabilitySPICE {
			return true
		}
	}
	return false
}

func enabledOf(vm *model.VM, protocol string) bool {
	if protocol == ConsoleProtocolSPICE {
		return vm.SPICEEnabled
	}
	return vm.VNCEnabled
}

func bindOf(vm *model.VM, protocol string) string {
	if protocol == ConsoleProtocolSPICE {
		return vm.SPICEBind
	}
	return vm.VNCBind
}

func exposedOf(vm *model.VM, protocol string) bool {
	if protocol == ConsoleProtocolSPICE {
		return vm.SPICEExposed
	}
	return vm.VNCExposed
}
