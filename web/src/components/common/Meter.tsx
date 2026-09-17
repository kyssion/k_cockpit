/**
 * Meter 是一条带数值的计量条。
 *
 * 抽成公共组件而不是各页各写一份：虚拟机资源卡与节点指标卡都要用它，
 * 而两处各写一份的话，迟早会出现「同样的 90% 在一页是红的、在另一页是蓝的」
 * ——颜色的含义必须全局一致，否则它就不再是信号。
 */

interface MeterProps {
  label: string
  /** 右侧的补充说明，例如「2 核」「8.2 GB / 16 GB」。 */
  detail?: string
  /** 0-100。超出范围会被夹住，而不是画出一条溢出容器的条。 */
  percent: number
}

/**
 * 告警阈值。
 *
 * 85% 与 60% 而不是 90% / 75%：留给用户反应的时间比「精确」更重要——
 * 等到 90% 才变红，留给人的处置窗口已经很小了。
 */
export function Meter({ label, detail, percent }: MeterProps) {
  const clamped = Math.max(0, Math.min(100, percent))
  const tone = clamped >= 85 ? 'bg-danger' : clamped >= 60 ? 'bg-warning' : 'bg-brand'

  return (
    <div>
      <div className="flex items-baseline justify-between gap-2 text-base">
        <span className="text-ink-3">{label}</span>
        <span className="kc-nums text-ink">
          <span className="font-medium">{clamped.toFixed(0)}%</span>
          {detail && <span className="ml-1.5 text-xs text-ink-3">{detail}</span>}
        </span>
      </div>
      <div className="mt-1 h-1.5 overflow-hidden rounded-pill bg-line">
        <div className={`h-full rounded-pill ${tone}`} style={{ width: `${clamped}%` }} />
      </div>
    </div>
  )
}
