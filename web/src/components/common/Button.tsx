import type { ButtonHTMLAttributes, ReactNode } from 'react'

import { cn } from '@/utils/cn'

type Variant = 'primary' | 'hero' | 'secondary' | 'danger' | 'ghost'
type Size = 'sm' | 'md'

const variants: Record<Variant, string> = {
  // 实底上的文字用 on-brand / on-danger：暗色主题下亮底配白字达不到 AA
  // （见 tokens.css 头注），亮底配深字是暗色主题的现代惯例。
  primary: 'bg-brand text-on-brand hover:bg-brand-hover active:bg-brand-active',
  // hero：页面级主操作专用（每个视图至多一处）——品牌渐变底 + 光晕，
  // hover 时光晕增强、渐变微亮；按压下潜由下方公共类提供。
  hero: 'bg-linear-to-r from-hero-from to-hero-to text-on-brand shadow-glow',
  secondary: 'border border-line-strong bg-surface text-ink hover:bg-sunken',
  danger: 'bg-danger text-on-danger hover:opacity-90',
  ghost: 'text-ink-2 hover:bg-sunken hover:text-ink',
}

const sizes: Record<Size, string> = {
  sm: 'h-8 px-3 text-sm',
  md: 'h-9 px-4 text-base',
}

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant
  size?: Size
  loading?: boolean
  children: ReactNode
}

export function Button({
  variant = 'primary',
  size = 'md',
  loading = false,
  disabled,
  className,
  children,
  ...rest
}: ButtonProps) {
  return (
    <button
      {...rest}
      disabled={disabled || loading}
      aria-busy={loading}
      className={cn(
        // 按压轻微下潜 + 色彩过渡：手感来自「按下有反应、松手即回弹」，
        // 位移只有 2%，是反馈而不是动画。
        'inline-flex items-center justify-center gap-2 rounded-control font-medium',
        'transition-[color,background-color,border-color,box-shadow,opacity,transform]',
        'duration-(--kc-duration-fast) ease-(--kc-ease) active:scale-[0.98]',
        'disabled:cursor-not-allowed disabled:opacity-50 disabled:active:scale-100',
        variants[variant],
        sizes[size],
        className,
      )}
    >
      {loading && (
        <span
          aria-hidden
          className="size-3.5 animate-spin rounded-pill border-2 border-current border-t-transparent"
        />
      )}
      {children}
    </button>
  )
}
