import { useQuery } from '@tanstack/react-query'
import { useEffect } from 'react'

import { authApi } from '@/api/auth'
import { useSessionStore } from '@/stores/session'

/**
 * 应用启动时恢复登录态。
 *
 * 前端不保存令牌（令牌在 HttpOnly Cookie 里），因此「我是否已登录」只能
 * 问服务端一次。失败一律视为未登录——包括 401 之外的错误：
 * 把网络异常当成「已登录」会让界面进入必然失败的状态。
 *
 * staleTime 设为 Infinity：会话状态由本模块显式失效，不随时间自动重取。
 */
export function useSessionBootstrap() {
  const setAuthenticated = useSessionStore((s) => s.setAuthenticated)
  const setAnonymous = useSessionStore((s) => s.setAnonymous)

  const query = useQuery({
    queryKey: ['auth', 'session'],
    queryFn: authApi.current,
    retry: false,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  })

  useEffect(() => {
    if (query.isSuccess) {
      setAuthenticated(query.data.user)
    } else if (query.isError) {
      setAnonymous()
    }
  }, [query.isSuccess, query.isError, query.data, setAuthenticated, setAnonymous])

  return query
}
