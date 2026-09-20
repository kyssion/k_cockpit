/**
 * 主题：浅色 / 深色 / 跟随系统。
 *
 * 实现只有一件事——改 `<html data-theme>`：色值全部是 CSS 变量（见
 * styles/tokens.css），因此不需要重新渲染任何组件，也不存在"某几个组件
 * 没跟着变"这种问题。这正是当初把色值收进令牌的回报。
 *
 * 默认**跟随系统**而不是默认深色：一个面板不该在用户没表态时替他决定。
 * 跟随系统也用 `matchMedia` 实时响应——用户在系统里切到深色，面板不必
 * 等刷新。
 */
import { useCallback, useEffect, useState } from 'react'

export type ThemeMode = 'light' | 'dark' | 'system'

const KEY = 'kc.theme'

function systemPrefersDark(): boolean {
  return typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: dark)').matches
}

function read(): ThemeMode {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'light' || v === 'dark' || v === 'system') return v
  } catch {
    // 隐私模式下读不到；回落到默认即可，不影响使用。
  }
  return 'system'
}

/** 把偏好落到 `<html data-theme>`。 */
function apply(mode: ThemeMode) {
  const resolved = mode === 'system' ? (systemPrefersDark() ? 'dark' : 'light') : mode
  document.documentElement.dataset.theme = resolved
}

/**
 * useTheme 返回当前偏好与切换函数。
 *
 * 组件里**只用这一个 hook** 读写主题：直接改 `document.documentElement`
 * 的写法散落各处之后，"为什么这一页是浅的"就要靠猜了。
 */
export function useTheme() {
  const [mode, setMode] = useState<ThemeMode>(read)

  useEffect(() => {
    apply(mode)
    try {
      localStorage.setItem(KEY, mode)
    } catch {
      // 存不下不影响本次生效。
    }
  }, [mode])

  // 选择"跟随系统"时才监听系统变化；选了固定主题就该固定住——用户在面板
  // 里选了浅色，系统一变就跳回深色是最让人困惑的一类行为。
  useEffect(() => {
    if (mode !== 'system') return
    const mq = window.matchMedia?.('(prefers-color-scheme: dark)')
    if (!mq) return
    const onChange = () => apply('system')
    mq.addEventListener('change', onChange)
    return () => mq.removeEventListener('change', onChange)
  }, [mode])

  const cycle = useCallback(() => {
    setMode((prev) => (prev === 'system' ? 'light' : prev === 'light' ? 'dark' : 'system'))
  }, [])

  return { mode, setMode, cycle }
}

/** 当前偏好的中文名，供切换按钮显示。 */
export const THEME_LABEL: Record<ThemeMode, string> = {
  system: '跟随系统',
  light: '浅色',
  dark: '深色',
}
