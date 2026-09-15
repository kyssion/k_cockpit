import { createBrowserRouter } from 'react-router'

import { Placeholder } from '@/components/common/Feedback'
import { AppLayout } from '@/layout/AppLayout'
import { DashboardPage } from '@/views/dashboard/DashboardPage'
import { LoginPage } from '@/views/auth/LoginPage'
import { SetupPage } from '@/views/auth/SetupPage'
import { NodeListPage } from '@/views/node/NodeListPage'
import { SecurityPage } from '@/views/security/SecurityPage'
import { TaskListPage } from '@/views/task/TaskListPage'
import { VmDetailPage } from '@/views/vm/VmDetailPage'
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
          { path: 'task', element: <TaskListPage /> },
          { path: 'my-storage', element: pending('我的存储', '个人空间与文件管理') },
          { path: 'public-ip', element: pending('公网 IP', '资源池与绑定') },
          { path: 'node', element: <NodeListPage /> },
          { path: 'storage-pool', element: pending('存储池', '存储池与磁盘管理') },
          { path: 'network', element: pending('网络中心', '交换机、安全组与 ACL') },
          { path: 'firewall', element: pending('防火墙', '规则与连接管理') },
          { path: 'user', element: pending('用户管理', '账号与配额') },
          { path: 'settings', element: pending('系统设置', '配置项与环境变量对照') },
          { path: 'security', element: <SecurityPage /> },
          { path: '*', element: <Placeholder title="页面不存在" planned="请检查地址是否正确" /> },
        ],
      },
    ],
  },
])
