/**
 * 认证接口（f-1-01）。
 *
 * 契约以 docs/03-api/API.md 为准；令牌只经 HttpOnly Cookie 下发，
 * 响应体中不含令牌，前端也不需要保存它。
 */
import { del, get, post } from './client'

export type UserRole = 'admin' | 'tenant'

export interface UserView {
  id: number
  username: string
  role: UserRole
}

export interface SessionView {
  id: number
  client_ip?: string
  user_agent?: string
  issued_at: string
  expires_at: string
  last_active_at?: string
  /** 是否为当前正在使用的会话（安全中心用于标记「本机」）。 */
  current: boolean
}

/**
 * 登录阶段。
 *
 * `ok` 之外的三种都还**没有**会话：后端只给一枚五分钟的中间态令牌，
 * 前端拿它去走完剩下的步骤。令牌不下发 Cookie，因此刷新页面就得重新登录。
 */
export type LoginStage = 'ok' | 'login_verify' | 'force_password_change' | 'bootstrap_security'

export interface LoginResult {
  stage: LoginStage
  /** 中间态令牌，仅在 stage !== 'ok' 时出现。 */
  login_token?: string
  user?: UserView
  expires_at?: string
}

/** 引导期两步验证绑定的返回。 */
export interface TOTPSetupResult {
  otpauth_uri: string
}

export interface TOTPConfirmResult {
  recovery_codes: string[]
}

export interface CurrentSession {
  user: UserView
  session: SessionView
}

export const authApi = {
  /** 登录。失败时后端返回统一文案，不区分「用户不存在」与「密码错误」。 */
  login: (username: string, password: string) =>
    post<LoginResult>('/api/v1/auth/login', { username, password }),

  /** 登出当前会话。 */
  logout: () => post<void>('/api/v1/auth/logout'),

  /** 取当前会话与用户信息，供应用启动时恢复登录态。 */
  current: () => get<CurrentSession>('/api/v1/auth/session'),

  /** 会话与登录记录列表。 */
  sessions: () => get<SessionView[]>('/api/v1/auth/sessions'),

  /** 撤销指定会话（撤销他人会话返回 404，与「不存在」不可区分）。 */
  revokeSession: (id: number) => del<void>(`/api/v1/auth/sessions/${id}`),

  // --- 登录后续阶段（F-1-08）---

  /** 二次验证：TOTP 动态码或恢复码均可。 */
  verifyLogin: (loginToken: string, code: string) =>
    post<LoginResult>('/api/v1/auth/login/verify', { login_token: loginToken, code }),

  /** 强制改密：账号初始密码不属于使用者本人时必经的一步。 */
  forceChangePassword: (loginToken: string, newPassword: string) =>
    post<LoginResult>('/api/v1/auth/login/password', {
      login_token: loginToken,
      new_password: newPassword,
    }),

  /** 跳过安全初始化引导。 */
  skipBootstrap: (loginToken: string) =>
    post<LoginResult>('/api/v1/auth/bootstrap/skip', { login_token: loginToken }),

  /** 补齐安全设置后清除「已跳过」标记（已登录状态）。 */
  completeBootstrap: () => post<void>('/api/v1/auth/bootstrap/complete'),

  /** 引导期：开始绑定验证器。 */
  bootstrapBeginTOTP: (loginToken: string) =>
    post<TOTPSetupResult>('/api/v1/auth/bootstrap/totp/setup', { login_token: loginToken }),

  /** 引导期：确认绑定。 */
  bootstrapConfirmTOTP: (loginToken: string, code: string) =>
    post<TOTPConfirmResult>('/api/v1/auth/bootstrap/totp/confirm', {
      login_token: loginToken,
      code,
    }),

  /** 引导期：发送邮箱绑定验证码。 */
  bootstrapSendEmailCode: (loginToken: string, email: string) =>
    post<void>('/api/v1/auth/bootstrap/email/code', { login_token: loginToken, email }),

  /** 引导期：确认绑定邮箱。 */
  bootstrapConfirmEmail: (loginToken: string, email: string, code: string) =>
    post<void>('/api/v1/auth/bootstrap/email/confirm', {
      login_token: loginToken,
      email,
      code,
    }),

  // --- 邮箱与找回密码 ---

  /** 已登录：向待绑定邮箱发送验证码。 */
  sendEmailCode: (email: string) => post<void>('/api/v1/auth/email/code', { email }),

  /** 已登录：确认绑定邮箱。 */
  confirmEmail: (email: string, code: string) => post<void>('/api/v1/auth/email', { email, code }),

  /**
   * 发起找回密码。
   *
   * **邮箱不存在也返回成功**——否则这个接口就成了「谁注册了本面板」的
   * 查询入口；输错邮箱的反馈只能由「收不到邮件」承担。
   */
  requestPasswordReset: (email: string) => post<void>('/api/v1/auth/forgot/send', { email }),

  /** 校验邮件验证码，换回一次性重置票据。 */
  verifyResetCode: (email: string, code: string) =>
    post<{ reset_token: string }>('/api/v1/auth/forgot/verify', { email, code }),

  /** 用重置票据设置新密码。 */
  resetPassword: (resetToken: string, newPassword: string) =>
    post<void>('/api/v1/auth/forgot/reset', { reset_token: resetToken, new_password: newPassword }),
}
