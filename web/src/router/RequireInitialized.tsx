import type { ReactNode } from 'react'
import { Navigate, Outlet, useLocation } from 'react-router'

import { PageLoading } from '@/components/common/Feedback'
import { useSetupStatus } from '@/hooks/useSetupStatus'

const SETUP_PATH = '/setup'

/**
 * 初始化守卫。
 *
 * 系统尚无管理员时，把所有入口导向初始化页——否则用户会停在登录页，
 * 而此刻**没有任何账号可以登录**，界面上却看不出原因。
 *
 * 反向也成立：已初始化后访问初始化页会被送回登录页，避免出现一个
 * 点了必然报错的入口（后端也会拒绝，前端不该展示必败的操作）。
 */
export function RequireInitialized({ children }: { children?: ReactNode }) {
  const status = useSetupStatus()
  const location = useLocation()

  if (status.isPending) {
    return <PageLoading label="正在检查系统状态…" />
  }

  if (status.isError) {
    return (
      <div className="flex min-h-full items-center justify-center p-6">
        <div className="max-w-md rounded-card border border-line bg-surface p-6 text-center">
          <p className="text-md font-medium text-ink">无法连接服务</p>
          <p className="mt-2 text-base text-ink-3">
            请确认控制面已启动，然后刷新页面重试。
          </p>
        </div>
      </div>
    )
  }

  const onSetupPage = location.pathname === SETUP_PATH

  if (!status.data.initialized && !onSetupPage) {
    return <Navigate to={SETUP_PATH} replace />
  }

  if (status.data.initialized && onSetupPage) {
    return <Navigate to="/login" replace />
  }

  return <>{children ?? <Outlet />}</>
}
