/**
 * 会话状态：用户信息与登录态（FRONTEND §4.9 全局状态五块之一）。
 *
 * 只放**跨页面共享**的会话数据。服务端数据（列表、详情）一律交给
 * TanStack Query，不在此处缓存——两套缓存并存必然出现不一致。
 */
import { create } from 'zustand'

import type { UserRole, UserView } from '@/api/auth'

/** 登录态。`unknown` 表示尚未向前端确认，用于启动时的初始态。 */
export type AuthStatus = 'unknown' | 'authenticated' | 'anonymous'

interface SessionState {
  status: AuthStatus
  user: UserView | null
  setAuthenticated: (user: UserView) => void
  setAnonymous: () => void
}

export const useSessionStore = create<SessionState>((set) => ({
  status: 'unknown',
  user: null,

  setAuthenticated: (user) => set({ status: 'authenticated', user }),
  setAnonymous: () => set({ status: 'anonymous', user: null }),
}))

/** 便捷判断：当前用户是否为管理员。 */
export function useIsAdmin(): boolean {
  return useSessionStore((s) => s.user?.role === 'admin')
}

/** 便捷读取：当前用户角色。 */
export function useRole(): UserRole | undefined {
  return useSessionStore((s) => s.user?.role)
}
