package handler

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"

	"k_cockpit/internal/api"
	"k_cockpit/internal/audit"
	"k_cockpit/internal/auth"
	"k_cockpit/internal/risk"
)

// Account 提供账号自管理：改密码、改用户名、重新生成恢复码（F-1-03 / F-10-01）。
//
// 这三件事有共同的**证明方式**：都要提供当前密码。这一条不是形式主义——
// 拿到一个未锁屏的浏览器、或偷到一个会话令牌，就能改掉密码并把主人锁在外面，
// 而那时主人连"怎么进不去了"都查不出来（会话是合法的，日志里看不异常）。
//
// 为什么**不要求二次验证**（TOTP）：
//
//	用户重新生成恢复码的常见原因，恰恰是"手机丢了、恢复码也快用完了"。
//	那时他刚用掉一个恢复码登进来——要求 TOTP 就是要求他拿出已经丢了的
//	东西，于是这条路彻底走不通。密码是那一刻他唯一还能证明的东西。
//
// 与之相对，**改密码之后必须撤销其它会话**：改密码最常见的动机就是
// "我怀疑账号被盗"，而旧会话如果继续有效，这个动作就白做了。
type Account struct {
	auth  *auth.Service
	guard *risk.Guard
	audit *audit.Recorder
}

// NewAccount 构造账号接口。
func NewAccount(svc *auth.Service, guard *risk.Guard, recorder *audit.Recorder) *Account {
	return &Account{auth: svc, guard: guard, audit: recorder}
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ChangePassword 修改自己的密码（API-030）。
func (h *Account) ChangePassword(ctx context.Context, c *app.RequestContext) {
	var req changePasswordRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	session := auth.CurrentSession(c)
	if user == nil || session == nil {
		api.Fail(c, api.Unauthenticated("请先登录"))
		return
	}
	info := auth.ClientInfoOf(c)

	// 当前密码必须对。少了这一步，拿到会话的人就能把主人锁在外面。
	if ok, err := auth.VerifyPassword(user.PasswordHash, req.CurrentPassword); err != nil || !ok {
		h.deny(ctx, user.ID, "account.password.change", info.IP)
		api.Fail(c, api.ValidationFailed("当前密码不正确"))
		return
	}

	if err := validateNewPassword(req.NewPassword, user.Username); err != nil {
		api.Fail(c, err)
		return
	}
	// 新密码与旧密码相同：这看起来是"改了"，实际什么都没变，而用户会以为
	// 自己已经把可疑的密码换掉了。
	if ok, _ := auth.VerifyPassword(user.PasswordHash, req.NewPassword); ok {
		api.Fail(c, api.ValidationFailed("新密码不能与当前密码相同"))
		return
	}

	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		api.Fail(c, api.Internal())
		return
	}

	now := time.Now()
	updates := map[string]any{
		"password_hash": hash,
		// 改完密码就不再处于「强制改密」状态了——那正是这个标记的意义。
		"force_password_change": false,
		"security_updated_at":   now,
	}
	if err := h.auth.UpdateUser(ctx, user.ID, updates); err != nil {
		api.Fail(c, err)
		return
	}

	// **撤销其它会话**（保留当前这一个，否则用户改完密码立刻被登出，
	// 而他会以为改密码失败了）。
	revoked, err := h.auth.RevokeOtherSessions(ctx, user.ID, session.ID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	// TOTP 的一次性许可也一并作废：它们是在旧密码下签发的。
	h.guard.RevokeUserGrants(user.ID)

	h.record(ctx, audit.Entry{
		OperatorID: user.ID, OperatorName: user.Username,
		ResourceType: "account", ResourceID: user.ID, ResourceName: user.Username,
		Action:  "account.password.change",
		Params:  map[string]any{"revoked_sessions": revoked},
		Success: true, ClientIP: info.IP,
	})

	api.OK(c, map[string]any{
		"revoked_sessions": revoked,
		"notice":           "密码已修改。" + revokeNotice(revoked),
	})
}

type changeUsernameRequest struct {
	CurrentPassword string `json:"current_password"`
	Username        string `json:"username"`
}

// ChangeUsername 修改自己的登录名（API-031）。
func (h *Account) ChangeUsername(ctx context.Context, c *app.RequestContext) {
	var req changeUsernameRequest
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	if user == nil {
		api.Fail(c, api.Unauthenticated("请先登录"))
		return
	}
	info := auth.ClientInfoOf(c)

	if ok, err := auth.VerifyPassword(user.PasswordHash, req.CurrentPassword); err != nil || !ok {
		h.deny(ctx, user.ID, "account.username.change", info.IP)
		api.Fail(c, api.ValidationFailed("当前密码不正确"))
		return
	}

	name := strings.TrimSpace(req.Username)
	if err := validateUsername(name); err != nil {
		api.Fail(c, err)
		return
	}
	if name == user.Username {
		api.Fail(c, api.ValidationFailed("新用户名与当前用户名相同"))
		return
	}

	// 唯一性由数据库约束兜底，但这里先查一次是为了给出**可以说清楚的错误**。
	// 冲突时不明说"这个名字被谁用了"——那等于提供了一个用户名枚举接口
	// （f-1-06 R-005）。
	taken, err := h.auth.UsernameTaken(ctx, name, user.ID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	if taken {
		api.Fail(c, api.Conflict("该用户名已被使用"))
		return
	}

	now := time.Now()
	if err := h.auth.UpdateUser(ctx, user.ID, map[string]any{
		"username":            name,
		"security_updated_at": now,
	}); err != nil {
		api.Fail(c, err)
		return
	}

	// **不撤销会话**：改用户名不影响凭据的有效性，而把用户踢下线只会让他
	// 以为操作失败了。密码才是凭据，用户名只是标识。
	h.record(ctx, audit.Entry{
		OperatorID: user.ID, OperatorName: name,
		ResourceType: "account", ResourceID: user.ID, ResourceName: name,
		Action: "account.username.change",
		// 改名要留下**改之前叫什么**：事后查审计时，旧名字是唯一能对上的线索。
		Params:  map[string]any{"previous_username": user.Username},
		Success: true, ClientIP: info.IP,
	})

	api.OK(c, map[string]any{"username": name})
}

// RegenerateRecoveryCodes 重新生成恢复码（API-032）。
//
// **旧码全部作废。** 发一批新的"万能钥匙"而旧的还能用，等于把可用的入口
// 翻了一倍，而用户以为只换了新的那一张纸——抽屉里那张旧的依然能登进来。
func (h *Account) RegenerateRecoveryCodes(ctx context.Context, c *app.RequestContext) {
	var req struct {
		CurrentPassword string `json:"current_password"`
	}
	if err := c.Bind(&req); err != nil {
		api.Fail(c, api.InvalidParameter("请求参数不合法"))
		return
	}

	user := auth.CurrentUser(c)
	if user == nil {
		api.Fail(c, api.Unauthenticated("请先登录"))
		return
	}
	info := auth.ClientInfoOf(c)

	if ok, err := auth.VerifyPassword(user.PasswordHash, req.CurrentPassword); err != nil || !ok {
		h.deny(ctx, user.ID, "account.recovery.regenerate", info.IP)
		api.Fail(c, api.ValidationFailed("当前密码不正确"))
		return
	}
	if !user.TotpEnabled {
		// 没绑定验证器时恢复码没有意义：它唯一的用途是"TOTP 不可用时登进来"，
		// 而没有 TOTP 时登录本来就不需要它。
		api.Fail(c, api.ValidationFailed("尚未绑定验证器，恢复码没有意义"))
		return
	}

	user, err := h.auth.UserByID(ctx, user.ID)
	if err != nil {
		api.Fail(c, err)
		return
	}
	codes, err := h.guard.RegenerateRecoveryCodes(ctx, user)
	if err != nil {
		api.Fail(c, err)
		return
	}

	h.record(ctx, audit.Entry{
		OperatorID: user.ID, OperatorName: user.Username,
		ResourceType: "account", ResourceID: user.ID, ResourceName: user.Username,
		Action:  "account.recovery.regenerate",
		Params:  map[string]any{"count": len(codes)},
		Success: true, ClientIP: info.IP,
	})

	api.OK(c, map[string]any{
		"recovery_codes": codes,
		"notice": "已生成一批新的恢复码，**之前的所有恢复码已全部作废**。" +
			"请立即保存到安全的地方——这串码只显示这一次。",
	})
}

// --- 内部 ---

// validateNewPassword 校验新密码。
//
// 规则与管理员创建用户时**共用同一个口径**（见 useradmin）：两处各写一套
// 迟早会出现「管理员设的密码合规、用户自己改的却不合规」这种矛盾，而用户
// 只会觉得系统在刁难他。
func validateNewPassword(password, username string) error {
	if len(password) < 8 {
		return api.ValidationFailed("新密码至少 8 位")
	}
	if len(password) > 128 {
		return api.ValidationFailed("新密码最长 128 位")
	}
	if strings.EqualFold(password, username) {
		return api.ValidationFailed("密码不能与用户名相同")
	}
	return nil
}

func validateUsername(name string) error {
	if len(name) < 3 {
		return api.ValidationFailed("用户名至少 3 位")
	}
	if len(name) > 32 {
		return api.ValidationFailed("用户名最长 32 位")
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '-'
		if !ok {
			return api.ValidationFailed("用户名只能包含字母、数字、点、下划线与连字符")
		}
	}
	return nil
}

func revokeNotice(n int64) string {
	if n <= 0 {
		return "当前没有其它登录中的会话。"
	}
	return "已退出其它设备上的 " + strconv.FormatInt(n, 10) + " 个登录会话。"
}

// deny 记录一次验证失败。
//
// 失败的尝试也要留痕：连续多次"当前密码不正确"是账号被盗用的典型信号，
// 而只记录成功的话，那种信号在审计里是看不见的。
func (h *Account) deny(ctx context.Context, userID int64, action, ip string) {
	h.record(ctx, audit.Entry{
		OperatorID: userID, ResourceType: "account", ResourceID: userID,
		Action: action, Success: false, ClientIP: ip,
	})
}

func (h *Account) record(ctx context.Context, e audit.Entry) {
	if h.audit == nil {
		return
	}
	h.audit.Record(ctx, e)
}
