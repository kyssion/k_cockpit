import { useMutation } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router'

import { authApi } from '@/api/auth'
import { Button } from '@/components/common/Button'
import { CommandPalette } from '@/components/common/CommandPalette'
import { RiskVerificationGate } from '@/components/risk/RiskVerificationGate'
import { TabBar } from '@/components/common/TabBar'
import { TaskTray } from '@/components/common/TaskTray'
import { rememberVisit } from '@/utils/recentVisits'
import { useTaskStream } from '@/hooks/useTaskStream'
import { LANGS, t } from '@/locales'
import { useLocaleStore } from '@/stores/locale'
import { useTabStore } from '@/stores/tabs'
import { THEME_LABEL, useTheme } from '@/hooks/useTheme'
import { useSessionStore } from '@/stores/session'
import type { UserRole } from '@/api/auth'
import { cn } from '@/utils/cn'

interface NavItem {
  to: string
  label: string
  /** 英文名。语言包只收录通用文案，导航属于"每一处都必须有英文"的那一类：
   *  菜单是进入所有功能的入口，半中半英比全中文更难看。 */
  en: string
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
  { to: '/', label: '工作台', en: 'Dashboard'},
  { to: '/vm', label: '虚拟机', en: 'Virtual Machines'},
  { to: '/trash', label: '回收站', en: 'Trash'},
  { to: '/task', label: '任务中心', en: 'Tasks'},
  { to: '/alerts', label: '告警中心', en: 'Alerts'},
  { to: '/template', label: '模板', en: 'Templates'},
  { to: '/import', label: '导入', en: 'Import'},
  { to: '/public-ip', label: '公网 IP', en: 'Public IPs'},
  { to: '/security-group', label: '安全组', en: 'Security Groups'},
  { to: '/port-security', label: '端口安全', en: 'Port Security', roles: ['admin']},
  { to: '/host-firewall', label: '宿主机防火墙', en: 'Host Firewall', roles: ['admin']},
  { to: '/host-tuning', label: '宿主机调优', en: 'Host Tuning', roles: ['admin']},
  { to: '/platform-check', label: '平台自检', en: 'Platform Check', roles: ['admin']},
  { to: '/access-control', label: '访问控制', en: 'Access Control', roles: ['admin']},
  { to: '/passthrough', label: '硬件直通', en: 'PCI Passthrough', roles: ['admin']},
  { to: '/capture', label: '抓包诊断', en: 'Capture'},
  // 防火墙不在这里列：它是管理员专属的（见下面 admin 分组）。
  // 无角色限制地列出来，普通租户会看到一个点进去 403 的菜单。
  { to: '/port-mirror', label: '端口镜像', en: 'Port Mirror'},
  { to: '/api-keys', label: 'API 凭证', en: 'API Keys'},
  { to: '/node', label: '节点管理', en: 'Nodes', roles: ['admin']},
  { to: '/storage-pool', label: '存储池', en: 'Storage Pools', roles: ['admin']},
  { to: '/storage-volume', label: '存储卷', en: 'Storage Volumes', roles: ['admin']},
  { to: '/scheduler', label: '调度器', en: 'Schedulers', roles: ['admin']},
  { to: '/diagnostics', label: '诊断导出', en: 'Diagnostics', roles: ['admin']},
  { to: '/version', label: '版本与关于', en: 'Version'},
  { to: '/api-docs', label: 'API 文档', en: 'API Docs'},
  { to: '/logs', label: '日志', en: 'Logs', roles: ['admin']},
  { to: '/resource-quota', label: '资源配额', en: 'Resource Quotas', roles: ['admin']},
  { to: '/network', label: '网络中心', en: 'Network', roles: ['admin']},
  { to: '/network/base', label: '网络底座', en: 'Network Fabric', roles: ['admin']},
  { to: '/firewall', label: '防火墙', en: 'Firewall', roles: ['admin']},
  { to: '/user', label: '用户管理', en: 'Users', roles: ['admin']},
  { to: '/audit', label: '审计日志', en: 'Audit Log', roles: ['admin']},
  { to: '/quota', label: '存储配额', en: 'Storage Quota', roles: ['admin']},
  { to: '/settings', label: '系统设置', en: 'Settings', roles: ['admin']},
  { to: '/security', label: '安全中心', en: 'Security'},
]

/**
 * titleFor 由路由推断标签标题。
 *
 * 只推断到"这是哪一类页面"（虚拟机、节点……），具体名字留给页面自己——
 * 路由里有的是 ID，而用户认的是名字。
 */
function titleFor(path: string): string {
  const exact = NAV_ITEMS.find((i) => i.to === path)
  if (exact) return exact.label
  if (path.startsWith('/vm/')) return '虚拟机'
  if (path.startsWith('/node/')) return '节点'
  return '页面'
}

export function AppLayout() {
  const navigate = useNavigate()
  const user = useSessionStore((s) => s.user)
  const setAnonymous = useSessionStore((s) => s.setAnonymous)
  const [searchOpen, setSearchOpen] = useState(false)
  const lang = useLocaleStore((s) => s.lang)

  // 路由变化即注册一个标签。标签的初始标题由路由推断（见 titleFor），
  // 精确的名字由页面自己补齐——详情页拿到机器名后会把它改掉。
  const location = useLocation()
  const openTab = useTabStore((s) => s.open)
  useEffect(() => {
    const title = titleFor(location.pathname)
    openTab(location.pathname, title)
    // 最近访问：它只是"我刚才看了哪几台机器"这一层便利，放在本地即可。
    // 详情页拿到机器名后会用 setTabTitle 改成真名，这里只能是类名，
    // 因此 RecentVisits 展示的是「虚拟机 / 节点」这一类标题加路径。
    rememberVisit(location.pathname, title)
  }, [location.pathname, openTab])

  // ⌘K / Ctrl+K 唤起全局搜索。阻止默认行为是必要的：浏览器把这个组合键
  // 留给了自己的搜索栏，不拦的话面板还没打开焦点就被抢走了。
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setSearchOpen((v) => !v)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const logout = useMutation({
    mutationFn: authApi.logout,
    // 无论成功与否都清空本地登录态：继续留在界面上只会不断触发 401。
    onSettled: () => {
      setAnonymous()
      navigate('/login', { replace: true })
    },
  })

  // 全局只挂一条实时通道（任务状态流）。它替代任务中心与底部任务栏的轮询，
  // 并在任务变化时让相关列表重新取数。
  const streamState = useTaskStream()

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
              {lang === 'en-US' ? item.en : item.label}
            </NavLink>
          ))}
        </nav>
      </aside>

      <div className="flex min-w-0 flex-1 flex-col">
        <TabBar />

        <header className="flex h-14 shrink-0 items-center justify-end gap-3 border-b border-line bg-surface px-6">
          <Button variant="ghost" size="sm" onClick={() => setSearchOpen(true)}>
            {t('common.search', lang)}（⌘K）
          </Button>
          <ThemeToggle />
          <LangToggle />
          <span className="text-base text-ink-2">{user?.username}</span>
          <Button variant="ghost" size="sm" loading={logout.isPending} onClick={() => logout.mutate()}>
            {t('auth.logout', lang)}
          </Button>
        </header>

        <main className="min-h-0 flex-1 overflow-y-auto bg-base p-6">
          <Outlet />
        </main>

        {/* 常驻任务栏：只在有进行中任务时出现。 */}
        <TaskTray connected={streamState === 'open'} />
      </div>

      {/* 高风险操作的验证弹窗。挂在布局里而非请求层：它需要 Router 上下文
          （未绑定验证方式时给出跳转），而请求层是纯模块。 */}
      <RiskVerificationGate />
      {/* 条件渲染而不是传 open：卸载即重置面板状态，不必在 effect 里
          再清一次关键字与选中项。 */}
      {searchOpen && <CommandPalette onClose={() => setSearchOpen(false)} />}
    </div>
  )
}

/**
 * ThemeToggle 循环切换「跟随系统 → 浅色 → 深色」。
 *
 * 用循环而不是下拉：三档里的每一档都很短，而下拉要点两次。按钮上写的是
 * **当前档位**而不是"切换主题"——后者不告诉用户点下去会变成什么。
 */
function ThemeToggle() {
  const { mode, cycle } = useTheme()
  const lang = useLocaleStore((s) => s.lang)
  return (
    <Button
      variant="ghost"
      size="sm"
      onClick={cycle}
      title={`${t('theme.label', lang)}：${THEME_LABEL[mode]}`}
    >
      {t('theme.label', lang)} · {THEME_LABEL[mode]}
    </Button>
  )
}

/**
 * LangToggle 切换界面语言。
 *
 * 选项写的是**语言自身的名字**（简体中文 / English）而不是按当前语言翻译
 * 后的名字：语言选择器若也跟着界面语言变，不熟悉当前语言的人就再也换不
 * 回来了——那是一个能把自己锁死的开关。
 */
function LangToggle() {
  const { lang, setLang } = useLocaleStore()
  const next = lang === 'zh-CN' ? 'en-US' : 'zh-CN'
  return (
    <Button variant="ghost" size="sm" onClick={() => setLang(next)} title={t('lang.label', lang)}>
      {LANGS.find((l) => l.value === lang)?.label ?? lang}
    </Button>
  )
}
