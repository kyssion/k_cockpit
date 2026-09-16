package vm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"

	"k_cockpit/internal/agent"
	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/authz"
	"k_cockpit/internal/cryptoutil"
	"k_cockpit/internal/model"
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

// DefaultVNCPort 是控制台的默认端口。
const DefaultVNCPort = 5900

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

	Enabled       bool   `json:"enabled"`
	Port          int    `json:"port,omitempty"`
	Bind          string `json:"bind,omitempty"`
	Exposed       bool   `json:"exposed"`
	HasPassword   bool   `json:"has_password"`
	DisplayDevice string `json:"display_device"`

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
	// Enabled 开启或关闭控制台。
	Enabled *bool
	// Password 设置新的控制台密码。**只写不读**（R-005）。
	Password *string
	// Exposed 切换对外暴露。**高危操作**，受理前必须已完成二次验证（R-004）。
	Exposed *bool
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
func (s *Service) Console(ctx context.Context, id int64, v authz.Viewer) (*ConsoleConfig, error) {
	vm, err := s.load(ctx, id, v)
	if err != nil {
		return nil, err
	}
	return s.consoleConfig(ctx, vm)
}

func (s *Service) consoleConfig(ctx context.Context, vm *model.VM) (*ConsoleConfig, error) {
	cfg := &ConsoleConfig{
		VMID:            vm.ID,
		Enabled:         vm.VNCEnabled,
		Bind:            vm.VNCBind,
		Exposed:         vm.VNCExposed,
		DisplayDevice:   vm.DisplayDevice,
		SessionLimit:    ConsoleSessionLimit,
		ActiveSessions:  s.consoleSessions().count(vm.ID),
		StreamSupported: s.streamSupported(),
	}
	if vm.VNCPort != nil {
		cfg.Port = *vm.VNCPort
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

	return cfg, nil
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

	updates := map[string]any{}
	action := "vm.console.update"
	after := map[string]any{}

	if req.Enabled != nil {
		if *req.Enabled && vm.VNCPort == nil {
			port := DefaultVNCPort
			updates["vnc_port"] = port
		}
		updates["vnc_enabled"] = *req.Enabled
		after["enabled"] = *req.Enabled
		if *req.Enabled {
			action = "vm.console.enable"
		} else {
			action = "vm.console.disable"
		}
	}

	if req.Exposed != nil {
		// 高危：受理前**必须**已完成二次验证（R-004）。检查放在服务层
		// 而不是只在 handler：服务可能被其他入口调用，把安全判定放在
		// 一个入口上，等于给另一个入口留了缺口。
		if err := s.ensureExposureVerified(ctx); err != nil {
			return nil, err
		}
		updates["vnc_exposed"] = *req.Exposed
		// 暴露时改为监听所有地址；关闭时收回 127.0.0.1。
		if *req.Exposed {
			updates["vnc_bind"] = "0.0.0.0"
		} else {
			updates["vnc_bind"] = "127.0.0.1"
		}
		after["exposed"] = *req.Exposed
		action = "vm.console.exposure"
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
	return s.consoleConfig(ctx, reloaded)
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
			return nil, nil, api.Unavailable("当前 agent 为模拟模式，不提供控制台数据流")
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
	if len(password) > 8 {
		// VNC 协议的认证在多数实现上只使用前 8 个字符，接受更长的密码
		// 会给用户「我设了长密码」的错觉，而实际生效的只是前 8 位。
		return api.InvalidParameter("控制台密码最长 8 位（这是 VNC 协议的限制）")
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
