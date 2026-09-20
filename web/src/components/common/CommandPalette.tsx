/**
 * CommandPalette 是全局搜索面板（F-9-08），⌘K / Ctrl+K 唤起。
 *
 * 三条刻意的取舍：
 *
 *  1. **只做跳转，不做操作**。面板里没有"关机""删除"这类动作：一个能随手
 *     触发写操作的搜索框，等于把危险操作放在离误击最近的地方。
 *  2. **至少输两个字符才查**。单字符会命中几乎全部记录，而返回一堆无关
 *     结果比"没有结果"更浪费时间。
 *  3. **键盘可全程操作**（↑↓ 选择、回车跳转、Esc 关闭）。用鼠标去点结果
 *     的话，它就只是另一个搜索框，而不是"命令面板"。
 */
import { useQuery } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { useNavigate } from 'react-router'

import { searchApi, type SearchMatch } from '@/api/search'
import { cn } from '@/utils/cn'

const KIND_LABEL: Record<string, string> = {
  vm: '虚拟机',
  node: '节点',
  template: '模板',
}

export function CommandPalette({ onClose }: { onClose: () => void }) {
  const navigate = useNavigate()
  const [q, setQ] = useState('')
  const [active, setActive] = useState(0)

  const enabled = q.trim().length >= 2
  const results = useQuery({
    queryKey: ['search', q.trim()],
    queryFn: () => searchApi.query(q.trim()),
    enabled,
  })

  const matches = useMemo(() => results.data?.matches ?? [], [results.data])
  // 选中项在**渲染时**收敛而不是用 effect 重置：结果变少之后旧的序号可能
  // 已经越界，而 effect 要多渲染一轮才纠正过来。
  const cursor = Math.min(active, Math.max(0, matches.length - 1))

  function go(m: SearchMatch) {
    navigate(m.link)
    onClose()
  }

  function onKeyDown(e: React.KeyboardEvent) {
    if (e.key === 'Escape') {
      onClose()
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActive(Math.min(cursor + 1, matches.length - 1))
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive(Math.max(cursor - 1, 0))
      return
    }
    if (e.key === 'Enter' && matches[cursor]) {
      e.preventDefault()
      go(matches[cursor])
    }
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-start justify-center bg-black/40 pt-[10vh]"
      onClick={onClose}
    >
      <div
        className="w-[min(560px,90vw)] overflow-hidden rounded-card border border-line-strong bg-surface shadow-3"
        onClick={(e) => e.stopPropagation()}
      >
        <input
          autoFocus
          value={q}
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder="搜索虚拟机、节点、模板（至少两个字符）"
          className="h-12 w-full border-b border-line bg-transparent px-4 text-md text-ink placeholder:text-ink-3 focus:outline-none"
        />

        <div className="max-h-[60vh] overflow-y-auto">
          {q.trim().length < 2 && (
            <p className="px-4 py-6 text-center text-base text-ink-3">
              输入至少两个字符开始搜索。
            </p>
          )}

          {enabled && results.isPending && (
            <p className="px-4 py-6 text-center text-base text-ink-3">搜索中…</p>
          )}

          {enabled && results.isError && (
            <p className="px-4 py-6 text-center text-base text-danger">搜索失败，请稍后重试。</p>
          )}

          {enabled && !results.isPending && matches.length === 0 && (
            <p className="px-4 py-6 text-center text-base text-ink-3">
              没有匹配的虚拟机、节点或模板。
            </p>
          )}

          {matches.map((m, i) => (
            <button
              key={`${m.kind}-${m.id}`}
              type="button"
              onMouseEnter={() => setActive(i)}
              onClick={() => go(m)}
              className={cn(
                'flex w-full items-center gap-3 px-4 py-2.5 text-left',
                i === cursor ? 'bg-brand/10' : 'hover:bg-raised',
              )}
            >
              <span className="w-14 shrink-0 rounded-pill bg-sunken px-1.5 py-0.5 text-center text-xs text-ink-3">
                {KIND_LABEL[m.kind] ?? m.kind}
              </span>
              <span className="min-w-0 flex-1">
                <span className="block truncate text-base text-ink">{m.name}</span>
                {m.subtitle && (
                  <span className="block truncate text-xs text-ink-3">{m.subtitle}</span>
                )}
              </span>
            </button>
          ))}
        </div>

        <div className="flex items-center gap-3 border-t border-line px-4 py-2 text-xs text-ink-3">
          <span>↑↓ 选择</span>
          <span>回车跳转</span>
          <span>Esc 关闭</span>
        </div>
      </div>
    </div>
  )
}
