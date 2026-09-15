import { useQuery } from '@tanstack/react-query'

import { setupApi } from '@/api/setup'

/**
 * 查询系统是否已完成初始化。
 *
 * 不重试：失败通常意味着后端不可达，反复重试只会拖慢错误反馈。
 * staleTime 取得较长——初始化状态一旦为 true 就永久为 true，
 * 短时间内重复查询没有意义。
 */
export function useSetupStatus() {
  return useQuery({
    queryKey: ['setup', 'status'],
    queryFn: setupApi.status,
    retry: false,
    staleTime: 5 * 60_000,
    refetchOnWindowFocus: false,
  })
}
