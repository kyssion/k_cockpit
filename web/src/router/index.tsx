import { lazy, Suspense } from 'react'
import { createBrowserRouter } from 'react-router'

import { PageLoading, Placeholder } from '@/components/common/Feedback'
import { AppLayout } from '@/layout/AppLayout'
import { DashboardPage } from '@/views/dashboard/DashboardPage'
import { LoginPage } from '@/views/auth/LoginPage'
import { SetupPage } from '@/views/auth/SetupPage'
import { NetworkPage } from '@/views/network/NetworkPage'
import { NetworkPage2 } from '@/views/network/NetworkPage2'
import { NodeDetailPage } from '@/views/node/NodeDetailPage'
import { AuditLogPage } from '@/views/audit/AuditLogPage'
import { UserAdminPage } from '@/views/user/UserAdminPage'
import { NodeListPage } from '@/views/node/NodeListPage'
import { PublicIPPage } from '@/views/network/PublicIPPage'
import { FirewallPage } from '@/views/network/FirewallPage'
import { PortMirrorPage } from '@/views/network/PortMirrorPage'
import { SecurityGroupPage } from '@/views/network/SecurityGroupPage'
import { PortSecurityPage } from '@/views/network/PortSecurityPage'
import { HostFirewallPage } from '@/views/network/HostFirewallPage'
import { PassthroughPage } from '@/views/network/PassthroughPage'
import { CapturePage } from '@/views/network/CapturePage'
import { ImportPage } from '@/views/storage/ImportPage'
import { MyStoragePage } from '@/views/storage/MyStoragePage'
import { StorageVolumePage } from '@/views/storage/StorageVolumePage'
import { SchedulerPage } from '@/views/system/SchedulerPage'
import { DiagnosticsPage } from '@/views/system/DiagnosticsPage'
import { VersionPage } from '@/views/system/VersionPage'
import { LogPage } from '@/views/system/LogPage'
import { HostTuningPage } from '@/views/system/HostTuningPage'
import { PlatformCheckPage } from '@/views/system/PlatformCheckPage'
import { ResourceQuotaPage } from '@/views/settings/ResourceQuotaPage'
import { TemplatePage } from '@/views/template/TemplatePage'
import { APIKeyPage } from '@/views/security/APIKeyPage'
import { SecurityPage } from '@/views/security/SecurityPage'
import { QuotaPage } from '@/views/settings/QuotaPage'
import { SettingsPage } from '@/views/settings/SettingsPage'
import { StoragePoolPage } from '@/views/storage/StoragePoolPage'
import { TaskListPage } from '@/views/task/TaskListPage'
import { VmDetailPage } from '@/views/vm/VmDetailPage'

// noVNC 只在控制台页面用得到，但它是主 chunk 里最大的一块（约 60 KB）。
// 静态导入会让所有用户（包括从不打开控制台的那些）都下载它。
//
// react/only-export-components 要求「文件只导出组件」才能启用 Fast Refresh。
// 本文件是路由配置，导出的是 router 常量而非组件，天然不满足该前提。
/* oxlint-disable react/only-export-components */
const LazyConsolePage = lazy(() =>
  import('@/views/vm/ConsolePage').then((m) => ({ default: m.ConsolePage })),
)
/* oxlint-enable react/only-export-components */
import { VmListPage } from '@/views/vm/VmListPage'

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
          { path: 'diagnostics', element: <DiagnosticsPage /> },
          { path: 'version', element: <VersionPage /> },
          { path: 'logs', element: <LogPage /> },
          { path: 'host-tuning', element: <HostTuningPage /> },
          { path: 'platform-check', element: <PlatformCheckPage /> },
          { path: 'resource-quota', element: <ResourceQuotaPage /> },
          { path: 'network', element: <NetworkPage /> },
          { path: 'network/base', element: <NetworkPage2 /> },
          // 用户管理（F-1-07）仍是占位。
          //
          // 保留一个**明确的占位**而不是把它藏起来：隐藏会让「这个产品没有
          // 这个能力」与「这个能力还没做」看起来一样——前者是设计判断，
          // 后者是欠账，对使用者是两件事。
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
