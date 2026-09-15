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

export interface LoginResult {
  user: UserView
  expires_at: string
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
}
