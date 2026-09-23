/**
 * Segmented 是「同一时刻只有一个生效」的切换器：视图 / 分组 / 时间范围。
 *
 * 用 `aria-pressed` 而不是普通按钮：同一时刻只有一种生效，做成可多选的
 * 样子会让人以为能同时按状态和模板分组。视觉上把选项拼成一个整体（共边
 * 框），也比一排离散按钮更能表达「这组里挑一个」。
 */
export function Segmented<T extends string>({
  value,
  onChange,
  options,
  size = 'sm',
}: {
  value: T
  onChange: (v: T) => void
  options: { value: T; label: string }[]
  size?: 'sm' | 'md'
}) {
  return (
    <div className="inline-flex overflow-hidden rounded-control border border-line-strong">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          onClick={() => onChange(o.value)}
          aria-pressed={value === o.value}
          className={
            (size === 'md' ? 'px-3 py-1.5 text-base ' : 'px-2.5 py-1 text-sm ') +
            (value === o.value
              ? 'bg-brand/10 text-brand'
              : 'text-ink-2 hover:bg-raised hover:text-ink')
          }
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}
