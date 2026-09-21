/**
 * 最近访问（F-8-03 的一小块便利能力）。
 *
 * 放在 localStorage 而不是后端：它回答的只是"我刚才看了哪几台机器"，
 * 为它建一张表并让每次打开详情页都写一次库并不划算。
 *
 * 单独成一个模块而不是留在页面文件里：页面的导出必须只有组件，否则
 * Fast Refresh 会失效（lint 规则 react/only-export-components）。
 */
export interface Visit {
  path: string
  title: string
}

const RECENT_KEY = 'kc.recent-visits'
const MAX_VISITS = 8

export function readRecentVisits(): Visit[] {
  try {
    const raw = localStorage.getItem(RECENT_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw) as Visit[]
    return Array.isArray(parsed) ? parsed.slice(0, MAX_VISITS) : []
  } catch {
    return []
  }
}

/**
 * 记录一次访问。
 *
 * 首页与"页面"（推断不出标题的路由）不记：它们没有辨识度，记下来只是
 * 把真正有用的几条挤掉。
 */
export function rememberVisit(path: string, title: string) {
  if (!path || path === '/' || !title || title === '页面') return
  try {
    const next = [{ path, title }, ...readRecentVisits().filter((v) => v.path !== path)].slice(
      0,
      MAX_VISITS,
    )
    localStorage.setItem(RECENT_KEY, JSON.stringify(next))
  } catch {
    // 隐私模式下 localStorage 可能不可写，这个便利功能丢掉即可。
  }
}
