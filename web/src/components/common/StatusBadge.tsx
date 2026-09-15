import { cn } from '@/utils/cn'

/** 状态语义（FRONTEND §4.2：全局统一，不允许各页面另配色）。 */
export type StatusTone = 'success' | 'warning' | 'danger' | 'info' | 'idle' | 'brand'

const tones: Record<StatusTone, string> = {
  success: 'bg-success/12 text-success border-success/30',
  warning: 'bg-warning/12 text-warning border-warning/30',
  danger: 'bg-danger/12 text-danger border-danger/30',
  info: 'bg-info/12 text-info border-info/30',
  idle: 'bg-idle/12 text-idle border-idle/30',
  brand: 'bg-brand/12 text-brand border-brand/30',
}

const dots: Record<StatusTone, string> = {
  success: 'bg-success',
  warning: 'bg-warning',
  danger: 'bg-danger',
  info: 'bg-info',
  idle: 'bg-idle',
  brand: 'bg-brand',
}

interface StatusBadgeProps {
  tone: StatusTone
  children: string
  /** 叠加条纹：用于「维护中」这类需要与常规状态区分的场景。 */
  striped?: boolean
  className?: string
}

export function StatusBadge({ tone, children, striped = false, className }: StatusBadgeProps) {
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-pill border px-2 py-0.5 text-xs font-medium',
        'whitespace-nowrap',
        tones[tone],
        striped && 'bg-[repeating-linear-gradient(45deg,transparent,transparent_3px,currentColor_3px,currentColor_4px)]',
        className,
      )}
    >
      {!striped && <span aria-hidden className={cn('size-1.5 rounded-pill', dots[tone])} />}
      {children}
    </span>
  )
}
