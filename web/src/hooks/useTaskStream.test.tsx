/**
 * 实时任务通道的**重连语义**：连接成功后必须重取一次。
 *
 * 守的是一个真实事故面：控制面重启时，旧进程会把在途任务收敛成
 * unknown/failed，而那些事件发生在任何订阅者连上来之前。底部「进行中任务」
 * 托盘没有轮询兜底，只认事件，于是会永远停在「执行中」——用户看到的就是
 * 「重启之后任务卡住了」。拿到 connected 只把状态标成“已连接”是不够的，
 * 那一刻恰恰是“你错过了什么”的信号。
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook } from '@testing-library/react'
import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { useTaskStream } from '@/hooks/useTaskStream'

type Handlers = {
  onConnected?: () => void
  onEvent?: (e: unknown) => void
  onGap?: (dropped: number) => void
  onError?: () => void
}

const state = vi.hoisted(() => ({ handlers: null as Handlers | null }))

vi.mock('@/api/task', () => ({
  openTaskStream: (h: Handlers) => {
    state.handlers = h
    return () => {
      state.handlers = null
    }
  },
}))

function setup() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const invalidate = vi.spyOn(client, 'invalidateQueries').mockResolvedValue(undefined)
  const wrapper = ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={client}>{children}</QueryClientProvider>
  )
  const view = renderHook(() => useTaskStream(), { wrapper })
  return { view, invalidate }
}

describe('useTaskStream 重连', () => {
  beforeEach(() => {
    state.handlers = null
  })

  it('连接成功后立即重取任务（含单数键 task）', () => {
    const { view, invalidate } = setup()
    expect(state.handlers).not.toBeNull()

    act(() => {
      state.handlers?.onConnected?.()
    })

    expect(view.result.current).toBe('open')
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['tasks'] })
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['task'] })
    // 任务状态往往伴随虚拟机状态变化，一并刷新才不会出现列表与详情打架。
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['vms'] })
  })

  it('收到任务事件时同样重取', () => {
    const { invalidate } = setup()
    act(() => {
      state.handlers?.onEvent?.({ kind: 'task', status: 'failed' })
    })
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['tasks'] })
  })

  it('连接断开时状态退回 connecting，重连成功后再次重取', () => {
    const { view, invalidate } = setup()

    act(() => {
      state.handlers?.onConnected?.()
    })
    expect(view.result.current).toBe('open')

    act(() => {
      state.handlers?.onError?.()
    })
    expect(view.result.current).toBe('connecting')

    invalidate.mockClear()
    act(() => {
      state.handlers?.onConnected?.()
    })
    expect(view.result.current).toBe('open')
    // 重连是第二次“可能错过了变化”，必须再取一次。
    expect(invalidate).toHaveBeenCalledWith({ queryKey: ['tasks'] })
  })
})
