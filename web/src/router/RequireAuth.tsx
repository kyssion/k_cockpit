import type { ReactNode } from 'react'
import { Navigate, useLocation } from 'react-router'

import { PageLoading } from '@/components/common/Feedback'
import { useSessionBootstrap } from '@/hooks/useSessionBootstrap'
import { useSessionStore } from '@/stores/session'

/**
 * 登录守卫。
 *
 * 三态处理，缺一不可：
 *   - `unknown`：启动时尚未向服务端确认，显示加载态——**不能**直接跳登录页，
 *     否则已登录用户每次刷新都会看到登录页闪一下；
 *   - `anonymous`：跳登录页，并把当前地址记入 state，登录后回跳；
 *   - `authenticated`：放行。
 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const status = useSessionStore((s) => s.status)
  const bootstrap = useSessionBootstrap()
  const location = useLocation()

  if (status === 'unknown' || bootstrap.isPending) {
    return <PageLoading label="正在恢复登录状态…" />
  }

  if (status === 'anonymous') {
    return <Navigate to="/login" state={{ from: location.pathname + location.search }} replace />
  }

  return <>{children}</>
}
