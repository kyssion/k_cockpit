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
    // 详情抽屉用的是单数键 ['task', id]：TanStack Query 的前缀匹配按数组元素
    // 比较，'task' 不是 'tasks' 的前缀，漏掉它会让抽屉停在旧状态。
    void queryClient.invalidateQueries({ queryKey: ['task'] })
    void queryClient.invalidateQueries({ queryKey: ['vms'] })
    void queryClient.invalidateQueries({ queryKey: ['vm'] })
    void queryClient.invalidateQueries({ queryKey: ['dashboard'] })
  })

  useEffect(() => {
    const close = openTaskStream({
      onConnected: () => {
        setState('open')
        // **每次（重）连成功都要重取一次**：断线期间的变化没有任何订阅者。
        // 控制面重启是最典型的一次——旧进程停机时把在途任务收敛成
        // unknown/failed，那一刻事件发出去时这条流还没建立；只把状态标成
        // 「已连接」的话，底部「进行中任务」会永远停在「执行中」（它没有
        // 轮询兜底），用户看到的就是"任务卡住了"。
        invalidate.current()
      },
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
