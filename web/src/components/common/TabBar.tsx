/**
 * TabBar 是顶部的页面标签栏。
 *
 * 标签只做三件事：显示、切换、关闭。不做"刷新""固定""拖拽排序"——它们
 * 听起来有用，实际会让人在这条窄窄的区域里犹豫，而标签栏的价值恰恰是
 * 「不用想就能点」。
 */
import { NavLink, useLocation, useNavigate } from 'react-router'

import { useTabStore } from '@/stores/tabs'
import { cn } from '@/utils/cn'

export function TabBar() {
  const location = useLocation()
  const navigate = useNavigate()
  const tabs = useTabStore((s) => s.tabs)
  const close = useTabStore((s) => s.close)
  const closeOthers = useTabStore((s) => s.closeOthers)

  // 单个标签时不必占一行：那只是把面包屑换了个位置。
  if (tabs.length <= 1) return null

  return (
    <div className="flex h-10 shrink-0 items-center gap-1 overflow-x-auto border-b border-line bg-surface px-4">
      {tabs.map((t) => {
        const active = location.pathname === t.path
        return (
          <div
            key={t.path}
            className={cn(
              'group flex h-7 shrink-0 items-center gap-1 rounded-control border px-2.5 text-sm',
              active
                ? 'border-brand/40 bg-brand/10 text-brand'
                : 'border-transparent text-ink-3 hover:bg-raised hover:text-ink-2',
            )}
          >
            <NavLink to={t.path} className="max-w-[160px] truncate">
              {t.title}
            </NavLink>
            {!t.fixed && (
              <button
                type="button"
                aria-label={`关闭 ${t.title}`}
                className="text-ink-3 hover:text-danger"
                onClick={() => {
                  const next = close(t.path)
                  // 关掉的是当前页时才跳转：关掉一个后台标签却把用户带走，
                  // 是最让人措手不及的一类行为。
                  if (active && next) navigate(next)
                }}
              >
                ×
              </button>
            )}
          </div>
        )
      })}

      {tabs.filter((t) => !t.fixed).length > 1 && (
        <button
          type="button"
          className="ml-1 shrink-0 text-xs text-ink-3 hover:text-ink-2"
          onClick={() => closeOthers(location.pathname)}
        >
          关闭其他
        </button>
      )}
    </div>
  )
}
