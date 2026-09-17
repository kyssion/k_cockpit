import { lazy, Suspense } from 'react'
import { createBrowserRouter } from 'react-router'

import { PageLoading, Placeholder } from '@/components/common/Feedback'
import { AppLayout } from '@/layout/AppLayout'
import { DashboardPage } from '@/views/dashboard/DashboardPage'
import { LoginPage } from '@/views/auth/LoginPage'
import { SetupPage } from '@/views/auth/SetupPage'
import { NetworkPage } from '@/views/network/NetworkPage'
import { NodeDetailPage } from '@/views/node/NodeDetailPage'
import { NodeListPage } from '@/views/node/NodeListPage'
import { PublicIPPage } from '@/views/network/PublicIPPage'
import { SecurityGroupPage } from '@/views/network/SecurityGroupPage'
import { ImportPage } from '@/views/storage/ImportPage'
import { MyStoragePage } from '@/views/storage/MyStoragePage'
import { TemplatePage } from '@/views/template/TemplatePage'
import { SecurityPage } from '@/views/security/SecurityPage'
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
function pending(title: string, planned: string) {
  return <Placeholder title={title} planned={planned} />
}

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
          { path: 'public-ip', element: pending('公网 IP', '资源池与绑定') },
          { path: 'template', element: <TemplatePage /> },
          { path: 'node', element: <NodeListPage /> },
          { path: 'node/:id', element: <NodeDetailPage /> },
          { path: 'storage-pool', element: <StoragePoolPage /> },
          { path: 'network', element: <NetworkPage /> },
          { path: 'firewall', element: pending('防火墙', '规则与连接管理') },
          { path: 'user', element: pending('用户管理', '账号与配额') },
          { path: 'settings', element: <SettingsPage /> },
          { path: 'security', element: <SecurityPage /> },
          { path: '*', element: <Placeholder title="页面不存在" planned="请检查地址是否正确" /> },
        ],
      },
    ],
  },
])
