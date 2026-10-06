// routes_platform 登记身份与系统域的路由：认证与会话、初始化引导、账号
// 自管理、高风险二次验证、系统设置、用户与邀请、API 凭证、审计查询、
// 请求日志、口令检查、密钥轮换、日志管理与版本信息。
//
// handler 的构造与路由登记放在同一处：一个域读一个文件就够。
package router

import (
	"github.com/cloudwego/hertz/pkg/route"

	"k_cockpit/internal/handler/platform"
)

func registerPlatformRoutes(v1 *route.RouterGroup, deps Deps, g guards) {
	authHandler := platform.NewAuth(deps.Auth, deps.SecureCookie, deps.Risk)
	setupHandler := platform.NewSetup(deps.Bootstrap, deps.Auth, deps.SecureCookie)
	securityHandler := platform.NewSecurity(deps.Risk, deps.Auth)
	accountHandler := platform.NewAccount(deps.Auth, deps.Risk, deps.AuditRecorder)
	reqLogHandler := platform.NewReqLog(deps.ReqLog)
	passAuditHandler := platform.NewPassAudit(deps.PassAudit)
	authKeyHandler := platform.NewAuthKey(deps.AuthKey, deps.Risk)
	inviteHandler := platform.NewInvite(deps.Invite)
	settingsHandler := platform.NewSettings(deps.Settings, deps.Mailer)
	apiKeyHandler := platform.NewAPIKey(deps.APIKey)
	accessControlHandler := platform.NewAccessControl(deps.AccessControl)
	auditHandler := platform.NewAuditLog(deps.AuditLog)
	userAdminHandler := platform.NewUserAdmin(deps.UserAdmin)
	loggingHandler := platform.NewLogging(deps.Logging, deps.AuditRecorder)
	versionHandler := platform.NewVersion()

	{
		// 公开接口：系统尚无管理员时不可能要求认证（自举问题，见 ADR-0008）。
		// 安全性由「一次性令牌只能从服务端日志获取」保证。
		v1.GET("/setup/status", setupHandler.Status)
		v1.POST("/setup/admin", setupHandler.CreateAdmin)

		// 公开接口：获取凭据的入口，必须在 API.md 中显式标记为公开。
		v1.POST("/auth/login", authHandler.Login)

		// 登录后续阶段（F-1-08）。
		//
		// **不挂 requireAuth**：它们用请求体里的 login_token 认证，而那个
		// 令牌是中间态（五分钟、只差一步到完整权限），走 Cookie 会话中间
		// 件会把它当成完整会话放过去。
		v1.POST("/auth/login/verify", authHandler.VerifyLogin)
		v1.POST("/auth/login/password", authHandler.ForceChangePassword)
		v1.POST("/auth/bootstrap/skip", authHandler.SkipBootstrap)
		v1.POST("/auth/bootstrap/totp/setup", authHandler.StagedBeginTOTP)
		v1.POST("/auth/bootstrap/totp/confirm", authHandler.StagedConfirmTOTP)
		v1.POST("/auth/bootstrap/email/code", authHandler.StagedSendEmailCode)
		v1.POST("/auth/bootstrap/email/confirm", authHandler.StagedConfirmEmail)

		// 找回密码（公开）。三步而不是一步：邮件里的码只用来换一张短命的
		// 重置票据，能改密码的凭据因此不经过邮箱。
		v1.POST("/auth/forgot/send", authHandler.RequestPasswordReset)
		v1.POST("/auth/forgot/verify", authHandler.VerifyResetCode)
		v1.POST("/auth/forgot/reset", authHandler.ResetPassword)

		v1.POST("/auth/logout", g.requireAuth, authHandler.Logout)
		v1.GET("/auth/session", g.requireAuth, authHandler.Session)
		v1.GET("/auth/sessions", g.requireAuth, authHandler.Sessions)
		v1.DELETE("/auth/sessions/:id", g.requireAuth, authHandler.RevokeSession)

		// 高风险二次验证（f-10-01）：清单只读，验证接口换取一次性许可。
		// 清单是**唯一事实来源**，前端不得硬编码第二份（R-002）。
		v1.GET("/security/high-risk-policy", g.requireAuth, securityHandler.Policy)
		v1.POST("/auth/risk-verification", g.requireAuth, securityHandler.Verify)

		// 账号自管理（F-1-03 / F-10-01）。
		//
		// 三件事都要提供**当前密码**。这不是形式主义：拿到一个未锁屏的浏览器
		// 或偷到一个会话令牌就能改掉密码并把主人锁在外面，而那时主人连
		// "怎么进不去了"都查不出来——会话是合法的，日志里看不出异常。
		//
		// **刻意不要求二次验证**：用户重新生成恢复码的常见原因恰恰是"手机
		// 丢了、恢复码快用完了"，那时他刚用掉一个恢复码登进来。要求 TOTP
		// 就是要求他拿出已经丢了的东西，那条路会彻底走不通。
		// 邮箱绑定（F-1-08）。验证码发往待绑定地址，因此**不需要**该邮箱
		// 当前属于自己——否则改绑就走不通了。
		v1.POST("/auth/email/code", g.requireAuth, authHandler.SendEmailCode)
		v1.PUT("/auth/email", g.requireAuth, authHandler.ConfirmEmail)

		v1.PUT("/auth/password", g.requireAuth, accountHandler.ChangePassword)
		v1.PUT("/auth/username", g.requireAuth, accountHandler.ChangeUsername)
		v1.POST("/auth/recovery-codes", g.requireAuth, accountHandler.RegenerateRecoveryCodes)
		// 补齐安全设置后清除"已跳过引导"标记（F-1-08）。走访问级会话：
		// 邮箱与 2FA 都是在安全中心补齐的，那时登录早已完成。
		v1.POST("/auth/bootstrap/complete", g.requireAuth, authHandler.CompleteBootstrap)

		// 二次验证方式的绑定：没有绑定渠道，428 将永远无法通过。
		v1.GET("/auth/security-setup", g.requireAuth, securityHandler.SetupStatus)
		v1.POST("/auth/totp/setup", g.requireAuth, securityHandler.BeginTOTP)
		v1.POST("/auth/totp/confirm", g.requireAuth, securityHandler.ConfirmTOTP)

		// 系统设置（F-9-01）：**仅管理员**（R-014）。设置变更不得成为
		// 绕过权限的通道，因此 tenant 连可见性都没有。
		v1.GET("/settings", g.requireAuth, g.adminOnly, settingsHandler.List)
		v1.PATCH("/settings", g.requireAuth, g.adminOnly, settingsHandler.Update)
		v1.POST("/settings/rollback", g.requireAuth, g.adminOnly, settingsHandler.Rollback)
		// 测试发信（F-1-08）：SMTP 配置是否正确，只有真的发一封才知道。
		v1.POST("/settings/mail/test", g.requireAuth, g.adminOnly, settingsHandler.TestMail)

		// 公网访问与开发模式开关（F-10-06）。
		//
		// 它**看起来像普通设置项但不是**：关掉公网访问时若调用方自己就在
		// 公网上，那一刻他的连接就断了——因此需要显式确认。而"是不是公网"
		// 靠请求方地址判断，判错的方向是相反的。
		//
		// 环境变量优先于面板（与 F-9-01 一致）：部署方在启动参数里关掉之后，
		// 面板上那个开关不该能把它打开。
		v1.GET("/settings/access", g.requireAuth, g.adminOnly, accessControlHandler.Get)
		v1.PUT("/settings/access", g.requireAuth, g.adminOnly, accessControlHandler.Set)

		// 用户管理（F-1-07）。整体归管理员。
		//
		// 封禁是一个**级联动作**（置状态 → 撤销会话 → 停运行中的虚拟机），
		// 且**单步失败仅告警**：封禁的实质是置状态，那一步就达成了目的；
		// 后两步是减少暴露面的加固，它们的失败不该把整个操作判为失败——
		// 那会让界面显示「封禁失败」，而那人其实已经被封了。
		//
		// 几处不可逆的自锁被显式挡住：不能改自己的角色、不能封禁自己、
		// 不能把最后一个可用管理员降级或删除。
		v1.GET("/users", g.requireAuth, g.adminOnly, userAdminHandler.List)
		v1.POST("/users", g.requireAuth, g.adminOnly, userAdminHandler.Create)
		v1.PATCH("/users/:id", g.requireAuth, g.adminOnly, userAdminHandler.Update)
		v1.PUT("/users/:id/status", g.requireAuth, g.adminOnly, userAdminHandler.SetStatus)
		v1.DELETE("/users/:id", g.requireAuth, g.adminOnly, userAdminHandler.Delete)
		// SSH 访问（F-1-10）。单独一个接口而不是塞进 PATCH：它会**结束在线会话**，
		// 与"改资料"不是一个量级的动作。
		v1.PUT("/users/:id/ssh", g.requireAuth, g.adminOnly, userAdminHandler.SetSSHAccess)

		// 审计流水（F-1-12）。
		//
		// 在此之前审计只有写入没有读取——Recorder 一直在记，但没有人能查。
		// **权限隔离在服务层**：租户只看得到自己的记录，且看不到系统动作
		// （operator_id 为空）。返回别人的记录时用 404 而非 403——403 会
		// 确认「这个 id 存在」。
		v1.GET("/audit", g.requireAuth, auditHandler.List)
		v1.GET("/audit/facets", g.requireAuth, auditHandler.Facets)
		v1.GET("/audit/:id", g.requireAuth, auditHandler.Get)

		// API 凭证（F-1-10）与一次性动作令牌。
		//
		// 生成与撤销**只接受会话认证**（handler 内校验）：不允许用一个
		// API Key 去轮换 API Key——那会形成一个自我延续的凭据链，原持有者
		// 撤销它时攻击者手上那个仍然有效。
		v1.GET("/api-keys", g.requireAuth, apiKeyHandler.Get)
		v1.POST("/api-keys", g.requireAuth, apiKeyHandler.Create)
		v1.DELETE("/api-keys", g.requireAuth, apiKeyHandler.Revoke)
		v1.POST("/action-tokens", g.requireAuth, apiKeyHandler.IssueActionToken)

		// 日志管理（F-9-02）。
		//
		// 归管理员：日志里有**全部请求的上下文**（谁在什么时候访问了什么）
		// 以及服务端内部的报错细节，对租户既无意义也不该被看到。
		//
		// 调级别与清理都记审计：把级别调到 DEBUG 会让日志量显著上升，
		// 而清理之后就无法回溯了——两者事后都要能回答「是谁在什么时候做的」。
		v1.GET("/settings/log/status", g.requireAuth, g.adminOnly, loggingHandler.Status)
		// 请求日志（F-10-07）：与审计分开，默认关闭。
		v1.GET("/request-logs", g.requireAuth, g.adminOnly, reqLogHandler.List)
		v1.DELETE("/request-logs", g.requireAuth, g.adminOnly, reqLogHandler.Clear)
		// 弱口令 / 泄露口令检查（F-10-06）。判定在节点侧完成：控制面只有
		// argon2 哈希，无从比对。
		v1.POST("/security/password-audit", g.requireAuth, g.adminOnly, passAuditHandler.RunNow)
		v1.GET("/security/password-audit", g.requireAuth, g.adminOnly, passAuditHandler.Status)
		// 会话签名密钥轮换（F-1-09）：轮换即全员登出，因此要二次验证。
		v1.GET("/settings/auth-key", g.requireAuth, g.adminOnly, authKeyHandler.Status)
		v1.POST("/settings/auth-key/rotate", g.requireAuth, g.adminOnly, authKeyHandler.Rotate)
		// 邀请注册（F-1-10）：创建与撤销是管理员动作，预览与接受是**公开**接口
		// ——拿到链接的人必须能在未登录状态下看到"这是给谁的"并完成注册。
		v1.GET("/invites", g.requireAuth, g.adminOnly, inviteHandler.List)
		v1.POST("/invites", g.requireAuth, g.adminOnly, inviteHandler.Create)
		v1.POST("/invites/:id/revoke", g.requireAuth, g.adminOnly, inviteHandler.Revoke)
		v1.POST("/invites/:id/resend", g.requireAuth, g.adminOnly, inviteHandler.Resend)
		v1.GET("/invites/preview", inviteHandler.Preview)
		v1.POST("/invites/accept", inviteHandler.Accept)
		v1.GET("/settings/log/read", g.requireAuth, g.adminOnly, loggingHandler.Read)
		v1.PUT("/settings/log/level", g.requireAuth, g.adminOnly, loggingHandler.SetLevel)
		v1.GET("/settings/log/export", g.requireAuth, g.adminOnly, loggingHandler.Export)
		// 实时日志流（SSE）。见 GAP_CLOSURE 的 G-28 论证：日志在线查看是
		// 唯一「持续在变、而且停不下来」的那条流，也是唯一值得换成推送的地方。
		//
		// 鉴权仍是 Cookie——EventSource 对同源请求会自动带上它，因此不需要
		// 把令牌放进查询串。
		v1.GET("/settings/log/stream", g.requireAuth, g.adminOnly, loggingHandler.Stream)
		v1.POST("/settings/log/delete", g.requireAuth, g.adminOnly, loggingHandler.Delete)

		// 版本与关于（F-9-05）。
		//
		// **不限制角色**：内容是版本号与依赖清单，不含租户数据也不含密钥。
		// 它最常见的用途是「遇到问题先看一眼自己跑的是哪个版本」，把入口
		// 藏起来只会让用户去别处猜。
		v1.GET("/version", g.requireAuth, versionHandler.Get)
	}
}
