/**
 * 高风险验证的全局状态（f-10-01）。
 *
 * 请求层只暴露一个「请完成验证」的回调，弹框与重放由本模块负责，页面代码
 * 完全不感知 428 的存在——否则每个调用高风险操作的页面都要写一遍同样的
 * 弹框与重试逻辑。
 */
import { create } from 'zustand'

import type { RiskRequiredData } from '@/api/client'

export interface PendingVerification {
  required: RiskRequiredData
  resolve: (grant: string | null) => void
}

interface RiskStore {
  pending: PendingVerification | null
  /** 请求用户完成一次验证；返回许可，用户取消时返回 null。 */
  requestVerification: (required: RiskRequiredData) => Promise<string | null>
  /** 由验证弹窗调用，结束当前流程。 */
  settle: (grant: string | null) => void
}

export const useRiskStore = create<RiskStore>((set, get) => ({
  pending: null,

  requestVerification: (required) =>
    new Promise<string | null>((resolve) => {
      // 同一时刻只保留一个验证流程（f-10-01 §3.3 的「单飞加锁」）。
      //
      // 并发触发多个高风险操作时，后到的请求直接以 428 结束而不是排队：
      // 排出一个弹框队列会让用户以为「每输一次码才放行一个操作」，而实际
      // 上他只需要重试——那时许可已经签发过一次了。
      if (get().pending) {
        resolve(null)
        return
      }
      set({ pending: { required, resolve } })
    }),

  settle: (grant) => {
    const current = get().pending
    set({ pending: null })
    current?.resolve(grant)
  },
}))
