/**
 * 页面标签栏（FRONTEND §5.1）。
 *
 * 为什么要有它：这个面板的页面多、层级深，而从「某台虚拟机的磁盘」回到
 * 「刚才那台机器的网络」要按两三次后退。标签让**已经打开过的页面**始终
 * 在一跳之内可达。
 *
 * 两条规则：
 *
 *  1. 工作台固定不可关闭——它是"总有地方可去"的兜底，关掉之后用户可能
 *     面对一片空白。
 *  2. 标题可以被页面自己改精确（详情页把 `/vm/12` 改成机器名）：路由只能
 *     给出"这是哪一类页面"，而用户认的是名字。
 */
import { create } from 'zustand'

export interface TabItem {
  path: string
  title: string
  /** 固定标签不可关闭。 */
  fixed?: boolean
}

interface TabState {
  tabs: TabItem[]
  open: (path: string, title: string) => void
  setTitle: (path: string, title: string) => void
  close: (path: string) => string | null
  closeOthers: (path: string) => void
}

/** 最多保留多少个标签。 */
//
// 上限不是刁难：标签栏横着排，开二十个之后每一个都只剩两三个字宽，那时
// 它已经不能"一跳可达"了。
const MAX_TABS = 12

export const useTabStore = create<TabState>((set, get) => ({
  tabs: [{ path: '/', title: '工作台', fixed: true }],

  open: (path, title) => {
    const tabs = get().tabs
    if (tabs.some((t) => t.path === path)) return
    const next = [...tabs, { path, title }]
    // 超出上限时丢掉**最旧的那个可关闭标签**：新开的那个是用户刚要看的，
    // 丢它等于这次点击没有反应。
    while (next.length > MAX_TABS) {
      const idx = next.findIndex((t) => !t.fixed)
      if (idx < 0) break
      next.splice(idx, 1)
    }
    set({ tabs: next })
  },

  setTitle: (path, title) => {
    set({
      tabs: get().tabs.map((t) => (t.path === path ? { ...t, title } : t)),
    })
  },

  close: (path) => {
    const tabs = get().tabs
    const target = tabs.find((t) => t.path === path)
    if (!target || target.fixed) return null
    const idx = tabs.indexOf(target)
    const next = tabs.filter((t) => t.path !== path)
    set({ tabs: next })
    // 返回关掉之后该去哪：优先右边那个，没有就左边——与浏览器的行为一致，
    // 用户不需要重新学。
    return next[Math.min(idx, next.length - 1)]?.path ?? null
  },

  closeOthers: (path) => {
    set({ tabs: get().tabs.filter((t) => t.path === path || t.fixed) })
  },
}))
