/**
 * 界面语言（FRONTEND §7）。
 *
 * 只存"选了哪种语言"，取文案由 locales/index.ts 负责——两件事分开之后，
 * 以后换成 i18next 之类的库也只需替换取文案那一层。
 */
import { create } from 'zustand'

import type { Lang } from '@/locales'

const KEY = 'kc.lang'

function read(): Lang {
  try {
    const v = localStorage.getItem(KEY)
    if (v === 'zh-CN' || v === 'en-US') return v
  } catch {
    // 读不到就用默认；语言偏好丢掉不影响功能。
  }
  // 默认跟随浏览器，而不是默认中文：一个已经把系统调成英文的人，不该在
  // 打开面板时先看到满屏看不懂的字。
  const nav = typeof navigator !== 'undefined' ? navigator.language : ''
  return nav.startsWith('en') ? 'en-US' : 'zh-CN'
}

interface LocaleState {
  lang: Lang
  setLang: (lang: Lang) => void
}

export const useLocaleStore = create<LocaleState>((set) => ({
  lang: read(),
  setLang: (lang) => {
    set({ lang })
    // 同步 <html lang>：屏幕阅读器与浏览器的翻译提示都依赖它。
    document.documentElement.lang = lang
    try {
      localStorage.setItem(KEY, lang)
    } catch {
      // 存不下不影响本次切换。
    }
  },
}))
