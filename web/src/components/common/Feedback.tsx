import type { ReactNode } from 'react'

import { cn } from '@/utils/cn'

/** 页面级加载态。 */
export function PageLoading({ label = '加载中…' }: { label?: string }) {
  return (
    <div className="flex min-h-full items-center justify-center gap-3 py-20 text-ink-2">
      <span
        aria-hidden
        className="size-4 animate-spin rounded-pill border-2 border-current border-t-transparent"
      />
      <span className="text-base">{label}</span>
    </div>
  )
}

/** 空态：说明「为什么这里是空的」以及下一步能做什么。 */
export function EmptyState({
  title,
  description,
  action,
  className,
}: {
  title: string
  description?: string
  action?: ReactNode
  className?: string
}) {
  return (
    <div className={cn('flex flex-col items-center gap-2 py-16 text-center', className)}>
      <p className="text-md font-medium text-ink">{title}</p>
      {description && <p className="max-w-md text-base text-ink-3">{description}</p>}
      {action && <div className="mt-2">{action}</div>}
    </div>
  )
}

/**
 * 未实现页面的占位。
 *
 * 它明确写出「尚未实现」，而不是假装成空数据或错误——后者会让人误以为
 * 功能坏了或没有数据，浪费排查时间。
 */
export function Placeholder({ title, planned }: { title: string; planned?: string }) {
  return (
    <div className="flex h-full flex-col">
      <header className="mb-4">
        <h1 className="text-xl font-semibold text-ink">{title}</h1>
      </header>
      <div className="flex flex-1 items-center justify-center rounded-card border border-dashed border-line-strong">
        <div className="text-center">
          <p className="text-base text-ink-2">此页面尚未实现</p>
          {planned && <p className="mt-1 text-sm text-ink-3">{planned}</p>}
        </div>
      </div>
    </div>
  )
}
