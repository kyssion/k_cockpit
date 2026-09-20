/**
 * 全局的实时任务通道（F-7-02）。
 *
 * 只在 AppLayout 里挂一次，而不是每个页面各开一条 EventSource：浏览器对
 * 同源并发连接数有上限，几条流同时开着会把普通的接口请求挤到排队里——
 * 表现是"接了实时通道之后，其他页面反而变慢了"。
 *
 * 收到事件后**不直接改本地状态**，而是让 TanStack Query 失效重取：事件只
 * 说明"什么变了"，推来的快照可能在到达时就已经过期，而重新查一次永远是
 * 准确的。
 */
import { useEffect, useRef, useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'

import { openTaskStream } from '@/api/task'

/** 连接状态。`connecting` 是初始态，也是断线重连时的态。 */
export type StreamState = 'connecting' | 'open' | 'error'

export function useTaskStream(): StreamState {
  const queryClient = useQueryClient()
  const [state, setState] = useState<StreamState>('connecting')
  // 用 ref 装 handlers：它们每次渲染都是新函数，放进 useEffect 依赖会让
  // 连接被反复断开重连。
  const invalidate = useRef(() => {
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    void queryClient.invalidateQueries({ queryKey: ['vms'] })
    void queryClient.invalidateQueries({ queryKey: ['vm'] })
    void queryClient.invalidateQueries({ queryKey: ['dashboard'] })
  })

  useEffect(() => {
    const close = openTaskStream({
      onConnected: () => setState('open'),
      onEvent: () => invalidate.current(),
      onError: () => {
        // 断开时浏览器会自动重连，因此这里不手动重开一条——那只会让
        // 重连风暴里的连接数翻倍。只把状态显示出来。
        setState((s) => (s === 'open' ? 'connecting' : 'error'))
      },
      onGap: () => {
        // 有丢弃时按"可能漏了变化"处理：重取一次。这与收到事件是同一个
        // 动作——两者都意味着"你看到的状态可能不是最新的"。
        invalidate.current()
      },
    })
    return close
  }, [])

  return state
}
