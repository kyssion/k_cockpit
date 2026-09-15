import { useId, type InputHTMLAttributes, type ReactNode } from 'react'

import { cn } from '@/utils/cn'

interface InputProps extends Omit<InputHTMLAttributes<HTMLInputElement>, 'id'> {
  label: string
  /** 字段级错误：与后端返回的 details.field 对应，用于就地提示。 */
  error?: string
  hint?: ReactNode
}

export function Input({ label, error, hint, className, ...rest }: InputProps) {
  // 用 useId 生成稳定 id：组件可能在同页出现多次，手写 id 必然重复。
  const id = useId()
  const errorId = `${id}-error`

  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={id} className="text-sm font-medium text-ink-2">
        {label}
      </label>
      <input
        {...rest}
        id={id}
        aria-invalid={error ? true : undefined}
        aria-describedby={error ? errorId : undefined}
        className={cn(
          'h-9 rounded-control border bg-sunken px-3 text-base text-ink',
          'placeholder:text-ink-3',
          'focus:outline-none focus-visible:border-brand',
          error ? 'border-danger' : 'border-line-strong',
          className,
        )}
      />
      {error && (
        <p id={errorId} className="text-sm text-danger">
          {error}
        </p>
      )}
      {!error && hint && <p className="text-sm text-ink-3">{hint}</p>}
    </div>
  )
}
