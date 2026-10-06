// wire_platform 装配 internal/platform 域：认证与安全、设置、请求日志、
// 口令检查、邮件、API 凭证、审计日志查询、用户管理与邀请注册。
//
// platformServices 分两步装配（见 setupServices 的调用顺序）：
// 认证簇在 setupAuth（先于一切业务服务），设置与用户簇在服务装配阶段——
// userAdmin 依赖 storage 域的配额服务，invite 又依赖 userAdmin，因此这一
// 簇必须排在 storage 之后。
package main

import (
	"context"
	"log"

	"k_cockpit/internal/platform/accesscontrol"
	"k_cockpit/internal/platform/apikey"
	"k_cockpit/internal/platform/audit"
	"k_cockpit/internal/platform/auditlog"
	"k_cockpit/internal/platform/auth"
	"k_cockpit/internal/platform/authkey"
	"k_cockpit/internal/platform/cryptoutil"
	"k_cockpit/internal/platform/invite"
	"k_cockpit/internal/platform/mailer"
	"k_cockpit/internal/platform/passaudit"
	"k_cockpit/internal/platform/reqlog"
	"k_cockpit/internal/platform/risk"
	"k_cockpit/internal/platform/settings"
	"k_cockpit/internal/platform/useradmin"
)

// platformServices 承载 internal/platform 域的全部服务实例。
type platformServices struct {
	// 认证与安全（setupAuth 阶段构造，其余域都依赖它们）。
	issuer    *auth.TokenIssuer
	authSvc   *auth.Service
	riskGuard *risk.Guard
	bootstrap *auth.Bootstrap
	authKey   *authkey.Service

	// 设置与附属（服务装配阶段的第一步：后续每个域都读它）。
	settings *settings.Service
	reqLog   *reqlog.Service
	// passAudit 的判定在节点侧，控制面只管开关、定时与结果落地。
	passAudit *passaudit.Service
	mailer    *mailer.Service

	// 管理面（依赖 storage 域的配额服务，因此在 setupPlatformAdminServices 装配）。
	apiKey   *apikey.Service
	auditLog *auditlog.Service
	// userAdmin 创建账号时要同步写配额初始值，invite 复用它的创建入口。
	userAdmin     *useradmin.Service
	invite        *invite.Service
	accessControl *accesscontrol.Service
}

// setupAuth 装配认证、审计记录器、会话密钥轮换、高风险守卫与初始化引导。
func (a *app) setupAuth() {
	// 装配认证。签名密钥强度不足时直接失败启动——带着弱密钥继续运行，
	// 等于把所有会话置于可伪造的风险之下。
	issuer, err := auth.NewTokenIssuer(a.cfg.Session.Secret)
	if err != nil {
		log.Fatalf("初始化令牌签发器失败: %v", err)
	}
	a.platform.issuer = issuer
	a.recorder = audit.NewRecorder(a.db)

	// 会话签名密钥（F-1-09）：库里没有时用配置里的初始密钥建一条，之后轮换
	// 直接换库里的记录——换一次环境变量再重启的做法，在"怀疑泄漏"的场景下
	// 没有可用的时间窗。
	a.platform.authKey = authkey.NewService(a.db, cryptoutil.DeriveKey(
		[]byte(a.cfg.Session.Secret), "k_cockpit/auth_key/v1"), a.cfg.Session.Secret, a.recorder)
	issuer.SetKeyProvider(func() (string, []byte, error) {
		return a.platform.authKey.Load(context.Background())
	})

	a.platform.authSvc = auth.NewService(a.db, issuer, a.recorder, auth.Config{
		IdleTimeout:     a.cfg.Session.IdleTimeout,
		AbsoluteTimeout: a.cfg.Session.AbsoluteTimeout,
	})

	// 高风险二次验证的守卫。根密钥复用会话密钥但在内部按用途派生：
	// 单一密钥配置避免部署时多一个必填项，而用途隔离保证签名与加密
	// 不会互相影响。
	a.platform.riskGuard = risk.NewGuard(a.db, []byte(a.cfg.Session.Secret), a.recorder, a.cfg.Security.DevBypassCode)
	if a.platform.riskGuard.DevBypassEnabled() {
		// 醒目地打印：一个只在环境变量里的开关很容易被遗忘，而界面上仍
		// 显示「已绑定验证器」会让所有人以为防护是完整的。
		log.Printf("[risk] ⚠️  开发期万能验证码已启用（SECURITY_DEV_BYPASS_CODE）：" +
			"二次验证可被该固定值直接绕过。**部署到生产前必须清除该配置**" +
			"（生产环境配置它会导致启动失败）。")
	}

	// 系统尚无管理员时生成一次性初始化令牌。**它只打印到日志**：
	// 能读到日志即等价于拥有服务器访问权，这是「谁有权初始化」的判据。
	// 不采用默认账号密码——那是全网皆知的凭据，存在被抢先登录的窗口。
	bootstrap, token, err := auth.NewBootstrap(a.db, a.recorder)
	if err != nil {
		log.Fatalf("检查初始化状态失败: %v", err)
	}
	a.platform.bootstrap = bootstrap
	if token != "" {
		log.Printf("\n"+
			"============================================================\n"+
			"  系统尚未初始化，请访问面板创建首个管理员。\n"+
			"  一次性初始化令牌（仅本次运行有效，创建后立即失效）：\n\n"+
			"    %s\n\n"+
			"  提示：该令牌等同于初始化权限，请勿写入公开渠道；\n"+
			"  日志文件的访问权限即初始化权限，请妥善控制。\n"+
			"============================================================", token)
	}
}

// setupPlatformServices 装配设置、请求日志、口令检查与邮件。
//
// 它是服务装配的第一步：后面每个域的设置项读取（陈旧阈值、SMTP、保留期）
// 都从这里取实例。
func (a *app) setupPlatformServices() {
	db, mockAgent := a.db, a.mockAgent

	a.platform.settings = settings.NewService(db, a.recorder)
	// 请求日志的开关来自设置项：每次写入前读取，因此改了设置立即生效，
	// 不需要重启（重启才能生效会让人以为开关坏了）。
	a.platform.reqLog = reqlog.NewService(db, func(ctx context.Context) bool {
		return a.platform.settings.Bool("security.request_log_enabled", false)
	})

	// 口令检查：判定在节点侧，这里只做开关、定时与结果落地。
	a.platform.passAudit = passaudit.NewService(db, mockAgent, func() bool {
		return a.platform.settings.Bool("security.password_breach_check", false)
	}, a.recorder)
	// 邮件（F-1-08）：配置来自系统设置，因此管理员保存 SMTP 后**立即**生效，
	// 无需重启——"测试邮件"按钮是验证配置是否正确的唯一手段，重启才能生效
	// 会让那个按钮看起来一直是坏的。
	a.platform.mailer = mailer.New(a.platform.settings.MailConfig)
	// 日志归档保留数来自设置项：启动时取一次，之后每次修改立即应用
	// （applier 里改的是同一个 Logger 的选项，写路径不受影响）。
	a.logger.SetKeepFiles(a.platform.settings.Int(settings.KeyLogKeepFiles, 5))
	a.platform.settings.RegisterApplier(settings.KeyLogKeepFiles, intApplier{fn: a.logger.SetKeepFiles})
	// 登录阶段的二次验证复用 risk 的校验逻辑（恢复码一次性、TOTP 容差），
	// 接线在 router.Register 内完成——漏接的表现是"登录时永远提示服务
	// 不可用"，而编译期看不出来。
	a.platform.authSvc.SetMailer(a.platform.mailer)
}

// setupPlatformAdminServices 装配 API 凭证、审计日志查询、用户管理、邀请与访问控制。
//
// 排在 storage 与 compute 之后：userAdmin 要接 storage 的配额写入
// （quotaAdapter），invite 复用 userAdmin 的创建入口并发邮件。
func (a *app) setupPlatformAdminServices() {
	db := a.db

	a.platform.apiKey = apikey.NewService(db, a.recorder)
	a.platform.auditLog = auditlog.NewService(db)
	a.platform.userAdmin = useradmin.NewService(db, a.recorder, quotaAdapter{svc: a.storage.quota})
	// 邀请注册（F-1-10）。账号创建复用 useradmin（密码由受邀人自设），链接里的站点地址取自设置项——没有配置时给出相对链接，由管理员自己补域名。
	a.platform.invite = invite.NewService(db, a.recorder)
	a.platform.invite.SetUserCreator(a.platform.userAdmin.CreateFromInvite)
	a.platform.invite.SetMailer(func(ctx context.Context, to, link, role string) error {
		// role 直接进正文：受邀人需要知道自己被邀请成什么角色。
		return a.platform.mailer.Send(ctx, to, "邀请你加入 K Cockpit",
			"你被邀请加入 K Cockpit，角色："+role+"\n\n"+
				"请打开下面的链接完成注册（链接三天内有效）：\n"+link+"\n\n"+
				"如果你并不认识邀请你的人，请忽略这封邮件。\n")
	})
	// 链接里的站点地址取自设置项（环境变量 > 面板设置 > 默认值），读取发生在
	// 每次拼链接时，因此改设置立即生效。没有配置时返回空串、链接退化为相对
	// 路径——界面会提示管理员补上；猜一个错误域名发给收件人比相对路径更糟。
	a.platform.invite.SetSiteURL(func(ctx context.Context) string {
		return a.platform.settings.String(settings.KeySiteURL, "")
	})
	// SSH 访问要下发到宿主机，因此接上 agent（不接时只改控制面记录）。
	a.platform.userAdmin.SetAgent(a.mockAgent)
	a.platform.accessControl = accesscontrol.NewService(db, a.recorder, accesscontrol.Options{})
}
