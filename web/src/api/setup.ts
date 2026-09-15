/**
 * 首次初始化接口（ADR-0008）。
 *
 * 这两个接口在系统未初始化时必须公开——此刻还没有任何账号可用于认证。
 * 安全性由「一次性令牌只能从服务端日志获取」保证。
 */
import { get, post } from './client'
import type { UserView } from './auth'

export interface SetupStatus {
  initialized: boolean
}

export interface CreateAdminResult {
  user: UserView
  /** 是否已自动建立会话。为 false 时应引导用户去登录页。 */
  auto_login: boolean
}

export const setupApi = {
  status: () => get<SetupStatus>('/api/v1/setup/status'),

  createAdmin: (token: string, username: string, password: string) =>
    post<CreateAdminResult>('/api/v1/setup/admin', { token, username, password }),
}
