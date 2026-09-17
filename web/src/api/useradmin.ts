/**
 * 用户管理接口（F-1-07）。
 *
 * 封禁的返回值里有一处与别处不同：**级联动作的失败在 `warnings` 里**，
 * 而整体仍是成功。这不是实现从简——封禁的实质是置状态，那一步已经生效；
 * 撤销会话、停虚拟机是减少暴露面的加固。让加固的失败把整个操作判为失败，
 * 界面会显示「封禁失败」，**而那人其实已经被封了**。
 *
 * 因此界面要把这两类结果分开呈现：错误是一个红框，而 warnings 是一句
 * 「已封禁，但有两台虚拟机未停止」。
 */
import { del, get, patch, post, put } from './client'

export type UserRole = 'admin' | 'tenant'
export type UserStatus = 'pending' | 'active' | 'banned'

export interface UserView {
  id: number
  username: string
  role: UserRole
  status: UserStatus
  email?: string
  totp_enabled: boolean
  remark?: string
  created_at: string
  /** 为空表示从未登录——通常说明这个账号已经不需要了。 */
  last_login_at?: string
}

export interface UserPage {
  items: UserView[]
  total: number
  page: number
  page_size: number
}

export interface SetStatusResult {
  user: UserView
  /** 级联动作里失败的条目。它们**不影响操作本身的成败**。 */
  warnings?: string[]
}

export const userApi = {
  list: (f: {
    keyword?: string
    status?: string
    role?: string
    page?: number
    page_size?: number
  }) => get<UserPage>('/api/v1/users', f),

  create: (input: { username: string; password: string; role?: UserRole; email?: string; remark?: string }) =>
    post<UserView>('/api/v1/users', input),

  /** 编辑资料。**不含密码**——改密是独立且更敏感的动作。 */
  update: (id: number, input: { email?: string; remark?: string; role?: UserRole }) =>
    patch<UserView>(`/api/v1/users/${id}`, input),

  /** 封禁 / 解封。级联失败在 `warnings` 里，不算操作失败。 */
  setStatus: (id: number, status: UserStatus) =>
    put<SetStatusResult>(`/api/v1/users/${id}/status`, { status }),

  /** 删除（软删除）。名下有虚拟机时会被拒绝。 */
  remove: (id: number) => del<{ deleted: boolean }>(`/api/v1/users/${id}`),
}

export const ROLE_LABEL: Record<UserRole, string> = {
  admin: '管理员',
  tenant: '租户',
}

export const STATUS_LABEL: Record<UserStatus, string> = {
  pending: '待激活',
  active: '正常',
  banned: '已封禁',
}

export const STATUS_TONE: Record<UserStatus, 'success' | 'warning' | 'danger'> = {
  active: 'success',
  pending: 'warning',
  banned: 'danger',
}
