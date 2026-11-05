import { lazy, Suspense, type ComponentType } from 'react'
import { createBrowserRouter } from 'react-router'

import { PageLoading, Placeholder } from '@/components/common/Feedback'
import { AppLayout } from '@/layout/AppLayout'
import { LoginPage } from '@/views/auth/LoginPage'
import { ForgotPasswordPage } from '@/views/auth/ForgotPasswordPage'
import { InvitePage } from '@/views/auth/InvitePage'
import { SetupPage } from '@/views/auth/SetupPage'

// 业务页面一律路由级懒加载。
//
// 静态导入会让首屏包包含全部 40 个页面（含创建向导、模板、监控图表这些
// 重依赖），而用户第一次打开时只需要工作台。React Router 的懒加载边界
// 在 AppLayout 的 <Outlet/> 外（见 AppLayout），切换页面不会闪掉整个外壳。
//
// react/only-export-components 要求「文件只导出组件」才能启用 Fast Refresh。
// 本文件是路由配置，导出的是 router 常量而非组件，天然不满足该前提。
/* oxlint-disable react/only-export-components */
const lazyPage = <T extends Record<string, unknown>>(loader: () => Promise<T>, name: keyof T) =>
  lazy(async () => ({ default: (await loader())[name] as ComponentType }))

const DashboardPage = lazyPage(() => import('@/views/dashboard/DashboardPage'), 'DashboardPage')
const VmListPage = lazyPage(() => import('@/views/vm/VmListPage'), 'VmListPage')
const VmDetailPage = lazyPage(() => import('@/views/vm/VmDetailPage'), 'VmDetailPage')
const TrashPage = lazyPage(() => import('@/views/vm/TrashPage'), 'TrashPage')
const TaskListPage = lazyPage(() => import('@/views/task/TaskListPage'), 'TaskListPage')
const MyStoragePage = lazyPage(() => import('@/views/storage/MyStoragePage'), 'MyStoragePage')
const ImportPage = lazyPage(() => import('@/views/storage/ImportPage'), 'ImportPage')
const StoragePoolPage = lazyPage(() => import('@/views/storage/StoragePoolPage'), 'StoragePoolPage')
const StorageVolumePage = lazyPage(
  () => import('@/views/storage/StorageVolumePage'),
  'StorageVolumePage',
)
const TemplatePage = lazyPage(() => import('@/views/template/TemplatePage'), 'TemplatePage')
const NetworkPage = lazyPage(() => import('@/views/network/NetworkPage'), 'NetworkPage')
const NetworkPage2 = lazyPage(() => import('@/views/network/NetworkPage2'), 'NetworkPage2')
const PublicIPPage = lazyPage(() => import('@/views/network/PublicIPPage'), 'PublicIPPage')
const SecurityGroupPage = lazyPage(
  () => import('@/views/network/SecurityGroupPage'),
  'SecurityGroupPage',
)
const PortSecurityPage = lazyPage(
  () => import('@/views/network/PortSecurityPage'),
  'PortSecurityPage',
)
const HostFirewallPage = lazyPage(
  () => import('@/views/network/HostFirewallPage'),
  'HostFirewallPage',
)
const FirewallPage = lazyPage(() => import('@/views/network/FirewallPage'), 'FirewallPage')
const PortMirrorPage = lazyPage(() => import('@/views/network/PortMirrorPage'), 'PortMirrorPage')
const PassthroughPage = lazyPage(() => import('@/views/network/PassthroughPage'), 'PassthroughPage')
const CapturePage = lazyPage(() => import('@/views/network/CapturePage'), 'CapturePage')
const NodeListPage = lazyPage(() => import('@/views/node/NodeListPage'), 'NodeListPage')
const NodeDetailPage = lazyPage(() => import('@/views/node/NodeDetailPage'), 'NodeDetailPage')
const UserAdminPage = lazyPage(() => import('@/views/user/UserAdminPage'), 'UserAdminPage')
const AuditLogPage = lazyPage(() => import('@/views/audit/AuditLogPage'), 'AuditLogPage')
const QuotaPage = lazyPage(() => import('@/views/settings/QuotaPage'), 'QuotaPage')
const ResourceQuotaPage = lazyPage(
  () => import('@/views/settings/ResourceQuotaPage'),
  'ResourceQuotaPage',
)
const SettingsPage = lazyPage(() => import('@/views/settings/SettingsPage'), 'SettingsPage')
const SecurityPage = lazyPage(() => import('@/views/security/SecurityPage'), 'SecurityPage')
const APIKeyPage = lazyPage(() => import('@/views/security/APIKeyPage'), 'APIKeyPage')
const SchedulerPage = lazyPage(() => import('@/views/system/SchedulerPage'), 'SchedulerPage')
const AlertPage = lazyPage(() => import('@/views/system/AlertPage'), 'AlertPage')
const AboutPage = lazyPage(() => import('@/views/system/AboutPage'), 'AboutPage')
const VersionPage = lazyPage(() => import('@/views/system/VersionPage'), 'VersionPage')
const DiagnosticsPage = lazyPage(() => import('@/views/system/DiagnosticsPage'), 'DiagnosticsPage')
const ApiDocsPage = lazyPage(() => import('@/views/apidocs/ApiDocsPage'), 'ApiDocsPage')
const LogPage = lazyPage(() => import('@/views/system/LogPage'), 'LogPage')
const HostTuningPage = lazyPage(() => import('@/views/system/HostTuningPage'), 'HostTuningPage')
const PlatformCheckPage = lazyPage(
  () => import('@/views/system/PlatformCheckPage'),
  'PlatformCheckPage',
)
const AccessControlPage = lazyPage(
  () => import('@/views/system/AccessControlPage'),
  'AccessControlPage',
)

// noVNC 是控制台页面最重的一块（约 60 KB），只在独立窗口与控制台标签用得到。
const LazyConsolePage = lazy(() =>
  import('@/views/vm/ConsolePage').then((m) => ({ default: m.ConsolePage })),
)

// 独立窗口用的加载态：整屏而不是卡片，避免出现一屏空白。
function StreamFallback() {
  return (
    <div className="flex h-full items-center justify-center bg-base">
      <PageLoading />
    </div>
  )
}
/* oxlint-enable react/only-export-components */

import { RequireAuth } from './RequireAuth'
import { RequireInitialized } from './RequireInitialized'

/** 未实现页面的占位：明确写出「尚未实现」，而不是假装成空数据。 */
export const router = createBrowserRouter([
  {
    // 最外层：系统尚无管理员时，所有入口都导向初始化页。
    element: <RequireInitialized />,
    children: [
      { path: '/setup', element: <SetupPage /> },
      { path: '/login', element: <LoginPage /> },
      // 找回密码是**公开**路由：处于这个状态的人拿不出任何凭据，
      // 要求登录才能找回等于没有这个功能。
      { path: '/forgot', element: <ForgotPasswordPage /> },
      // 接受邀请同样是**公开**路由：受邀人此刻还没有账号，要求登录才能
      // 注册等于没有这个功能。它在初始化之外、登录之外，是唯一一条"未登录
      // 也能进"的写操作入口。
      { path: '/invite/:token', element: <InvitePage /> },
      // 独立控制台窗口：**不带布局**。控制台要占满屏幕才好用，而布局里的
      // 侧栏、标签栏与顶栏会各吃掉一条。它仍需要登录（Cookie 由浏览器自动
      // 带上，因此新开的窗口同样是已登录状态）。
      {
        path: '/vm/:id/console-window',
        element: (
          <RequireAuth>
            <Suspense fallback={<StreamFallback />}>
              <LazyConsolePage standalone />
            </Suspense>
          </RequireAuth>
        ),
      },
      {
        path: '/',
        element: (
          <RequireAuth>
            <AppLayout />
          </RequireAuth>
        ),
        children: [
          { index: true, element: <DashboardPage /> },
          { path: 'vm', element: <VmListPage /> },
          { path: 'trash', element: <TrashPage /> },
          { path: 'vm/:id', element: <VmDetailPage /> },
          {
            path: 'vm/:id/console',
            element: (
              <Suspense fallback={<PageLoading />}>
                <LazyConsolePage />
              </Suspense>
            ),
          },
          { path: 'task', element: <TaskListPage /> },
          { path: 'my-storage', element: <MyStoragePage /> },
          { path: 'import', element: <ImportPage /> },
          { path: 'public-ip', element: <PublicIPPage /> },
          { path: 'security-group', element: <SecurityGroupPage /> },
          { path: 'port-security', element: <PortSecurityPage /> },
          { path: 'host-firewall', element: <HostFirewallPage /> },
          { path: 'passthrough', element: <PassthroughPage /> },
          { path: 'capture', element: <CapturePage /> },
          { path: 'firewall', element: <FirewallPage /> },
          { path: 'port-mirror', element: <PortMirrorPage /> },
          { path: 'api-keys', element: <APIKeyPage /> },
          { path: 'template', element: <TemplatePage /> },
          { path: 'node', element: <NodeListPage /> },
          { path: 'node/:id', element: <NodeDetailPage /> },
          { path: 'storage-pool', element: <StoragePoolPage /> },
          { path: 'storage-volume', element: <StorageVolumePage /> },
          { path: 'scheduler', element: <SchedulerPage /> },
          { path: 'alerts', element: <AlertPage /> },
          { path: 'diagnostics', element: <DiagnosticsPage /> },
          { path: 'version', element: <VersionPage /> },
          { path: 'about', element: <AboutPage /> },
          { path: 'api-docs', element: <ApiDocsPage /> },
          { path: 'logs', element: <LogPage /> },
          { path: 'host-tuning', element: <HostTuningPage /> },
          { path: 'platform-check', element: <PlatformCheckPage /> },
          { path: 'access-control', element: <AccessControlPage /> },
          { path: 'resource-quota', element: <ResourceQuotaPage /> },
          { path: 'network', element: <NetworkPage /> },
          { path: 'network/base', element: <NetworkPage2 /> },
          // 用户管理（F-1-07）：创建 / 编辑 / 封禁 / 删除，封禁会级联处理
          // 该用户的 SSH 与运行中虚拟机。
          { path: 'user', element: <UserAdminPage /> },
          { path: 'audit', element: <AuditLogPage /> },
          { path: 'quota', element: <QuotaPage /> },
          { path: 'settings', element: <SettingsPage /> },
          { path: 'security', element: <SecurityPage /> },
          { path: '*', element: <Placeholder title="页面不存在" planned="请检查地址是否正确" /> },
        ],
      },
    ],
  },
])
