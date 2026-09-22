import { get, post } from './client'

/**
 * 邀请注册接口（F-1-10）。
 *
 * 邀请把"谁能进来"变成一次**由管理员发出、有期限、可追溯**的动作 —— 这个面板
 * 能接管宿主机上的全部虚拟机，开放注册等于把入口交给任何人。
 */
export type InviteStatus = 'pending' | 'accepted' | 'revoked' | 'expired'

export const INVITE_STATUS_LABEL: Record<InviteStatus, string> = {
  pending: '待接受',
  accepted: '已接受',
  revoked: '已撤销',
  expired: '已过期',
}

export const INVITE_STATUS_TONE: Record<InviteStatus, 'success' | 'idle' | 'warning' | 'danger'> = {
  pending: 'warning',
  accepted: 'success',
  revoked: 'danger',
  expired: 'idle',
}

/** 一条邀请（**不含令牌**）。 */
export interface InviteView {
  id: number
  email: string
  role: string
  quota_enabled: boolean
  quota_bytes: number
  remark?: string
  expires_at: string
  status: InviteStatus
  /** 只在创建与重发时返回一次，之后不再可读。 */
  link?: string
}

export const inviteApi = {
  list: () => get<{ items: InviteView[] }>('/api/v1/invites'),

  create: (input: {
    email: string
    role?: string
    quota_enabled?: boolean
    quota_bytes?: number
    remark?: string
    ttl_hours?: number
  }) => post<InviteView>('/api/v1/invites', input),

  revoke: (id: number) => post<InviteView>(`/api/v1/invites/${id}/revoke`),
  resend: (id: number) => post<InviteView>(`/api/v1/invites/${id}/resend`),

  /** 公开预览：拿到链接的人先看这是给谁、什么角色。 */
  preview: (token: string) => get<InviteView>('/api/v1/invites/preview', { token }),

  /** 公开接受：注册完需要自己登录一次（不自动建会话）。 */
  accept: (input: { token: string; username: string; password: string; email?: string }) =>
    post<void>('/api/v1/invites/accept', input),
}
