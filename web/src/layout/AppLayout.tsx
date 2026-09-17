import { useMutation } from '@tanstack/react-query'
import { NavLink, Outlet, useNavigate } from 'react-router'

import { authApi } from '@/api/auth'
import { Button } from '@/components/common/Button'
import { RiskVerificationGate } from '@/components/risk/RiskVerificationGate'
import { useSessionStore } from '@/stores/session'
import type { UserRole } from '@/api/auth'
import { cn } from '@/utils/cn'

interface NavItem {
  to: string
  label: string
  /** 不填表示所有角色可见。 */
  roles?: UserRole[]
}

/**
 * 菜单定义。
 *
 * **前端隐藏菜单不构成安全边界**（f-1-06 R-010）：这里只决定「看不看得到」，
 * 「能不能做」由后端对每个接口独立判定。任何依赖前端隐藏来保护的数据都视为缺陷。
 */
const NAV_ITEMS: NavItem[] = [
  { to: '/', label: '工作台' },
  { to: '/vm', label: '虚拟机' },
  { to: '/task', label: '任务中心' },
  { to: '/template', label: '模板' },
  { to: '/import', label: '导入' },
  { to: '/my-storage', label: '我的存储' },
  { to: '/public-ip', label: '公网 IP' },
  { to: '/node', label: '节点管理', roles: ['admin'] },
  { to: '/storage-pool', label: '存储池', roles: ['admin'] },
  { to: '/network', label: '网络中心', roles: ['admin'] },
  { to: '/firewall', label: '防火墙', roles: ['admin'] },
  { to: '/user', label: '用户管理', roles: ['admin'] },
  { to: '/settings', label: '系统设置', roles: ['admin'] },
  { to: '/security', label: '安全中心' },
]

export function AppLayout() {
  const navigate = useNavigate()
  const user = useSessionStore((s) => s.user)
  const setAnonymous = useSessionStore((s) => s.setAnonymous)

  const logout = useMutation({
    mutationFn: authApi.logout,
    // 无论成功与否都清空本地登录态：继续留在界面上只会不断触发 401。
    onSettled: () => {
      setAnonymous()
      navigate('/login', { replace: true })
    },
  })

  const visibleItems = NAV_ITEMS.filter(
    (item) => !item.roles || (user && item.roles.includes(user.role)),
  )

  return (
    <div className="flex h-full">
      <aside className="flex w-[220px] shrink-0 flex-col border-r border-line bg-surface">
        <div className="flex h-14 items-center px-5">
          <span className="text-md font-semibold text-ink">K Cockpit</span>
        </div>

        <nav className="flex flex-1 flex-col gap-0.5 overflow-y-auto px-2 pb-4">
          {visibleItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.to === '/'}
              className={({ isActive }) =>
                cn(
                  'rounded-control px-3 py-2 text-base transition-colors',
                  isActive
                    ? 'bg-brand/12 font-medium text-brand'
                    : 'text-ink-2 hover:bg-raised hover:text-ink',
                )
              }
            >
              {item.label}
            </NavLink>
          ))}
        </nav>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 shrink-0 items-center justify-end gap-3 border-b border-line bg-surface px-6">
          <span className="text-base text-ink-2">{user?.username}</span>
          <Button variant="ghost" size="sm" loading={logout.isPending} onClick={() => logout.mutate()}>
            登出
          </Button>
        </header>

        <main className="min-h-0 flex-1 overflow-y-auto bg-base p-6">
          <Outlet />
        </main>
      </div>

      {/* 高风险操作的验证弹窗。挂在布局里而非请求层：它需要 Router 上下文
          （未绑定验证方式时给出跳转），而请求层是纯模块。 */}
      <RiskVerificationGate />
    </div>
  )
}
