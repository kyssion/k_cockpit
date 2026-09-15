import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from 'react-router'

import { ApiError, setUnauthorizedHandler } from '@/api/client'
import { router } from '@/router'
import { useSessionStore } from '@/stores/session'

/**
 * 查询缓存配置。
 *
 * 不在这里做全局错误提示：不同页面对同一种错误的表现不同（表单就地提示、
 * 列表整页错误态），统一弹 toast 会让用户看到与自己操作无关的错误。
 */
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: (failureCount, error) => {
        // 认证与权限类错误不重试：重试不会让它变好，只会拖慢错误反馈。
        if (error instanceof ApiError && error.status < 500) return false
        return failureCount < 2
      },
      refetchOnWindowFocus: false,
      staleTime: 30_000,
    },
  },
})

// 收到 401 时清空登录态。跳转交给路由守卫完成——它同时负责记录回跳地址。
setUnauthorizedHandler(() => {
  useSessionStore.getState().setAnonymous()
})

export function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}
