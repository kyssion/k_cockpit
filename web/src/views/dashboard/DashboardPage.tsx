import { useSessionStore } from '@/stores/session'

/**
 * 工作台。
 *
 * 当前为骨架：先确认登录链路与布局可用，真实的概览卡片、资源统计与
 * 快捷入口随对应业务能力接入（FRONTEND §5.2）。
 */
export function DashboardPage() {
  const user = useSessionStore((s) => s.user)

  return (
    <div className="flex flex-col gap-4">
      <header>
        <h1 className="text-lg font-semibold text-ink">工作台</h1>
        <p className="mt-1 text-base text-ink-3">
          当前登录：<span className="text-ink-2">{user?.username}</span>
          <span className="ml-2 rounded-pill border border-line-strong px-2 py-0.5 text-xs text-ink-2">
            {user?.role === 'admin' ? '管理员' : '租户'}
          </span>
        </p>
      </header>

      <div className="rounded-card border border-line bg-surface p-5">
        <p className="text-base text-ink-2">
          概览卡片、资源统计与快捷入口将在对应业务能力接入后补全。
        </p>
      </div>
    </div>
  )
}
