/**
 * TimeSeriesChart 是自绘的时序折线图（F-8-01/02）。
 *
 * 没有引入图表库：这里只需要一张折线加一块填充，而一个图表库的体积通常
 * 超过整个应用的其余部分。自绘的代价是下面这些细节要自己处理，而它们恰好
 * 是这张图最容易画错的地方。
 *
 * **最重要的一条：数据缺口不能连成直线。**
 *
 * 采样是按固定间隔做的，而节点离线、采样器重启或刚开机的那段时间里根本没
 * 有数据。把缺口两侧的点直接连起来，图上会得到一条平滑的线段——看起来那
 * 段时间指标平稳，实际是**什么都没采到**。而用户正是拿这张图判断「那段时间
 * 发生了什么」，把"没数据"画成"很平稳"会让他在错误的方向上排查。
 *
 * 因此缺口处**断开**（分成多段折线），并在图上标出断了几处。
 */
import { useMemo, useState } from 'react'

export interface SeriesPoint {
  /** RFC3339 时间戳。 */
  at: string
  value: number
}

export interface TimeSeriesChartProps {
  points: SeriesPoint[]
  /** 相邻两点的间隔秒数，用来判断哪里有缺口。 */
  intervalSeconds?: number
  /** 纵轴单位换算与格式化。 */
  format: (value: number) => string
  /** 纵轴高度占满容器。 */
  height?: number
  tone?: 'primary' | 'success' | 'warning' | 'danger'
  /** 无数据时的说明。 */
  emptyHint?: string
}

const TONE_STROKE: Record<string, string> = {
  primary: 'var(--color-primary, #3b82f6)',
  success: 'var(--color-success, #16a34a)',
  warning: 'var(--color-warning, #d97706)',
  danger: 'var(--color-danger, #dc2626)',
}

/** 缺口判定：超过 2.5 倍采样间隔没有点，就认为中间断了。 */
const GAP_FACTOR = 2.5

export function TimeSeriesChart({
  points,
  intervalSeconds,
  format,
  height = 120,
  tone = 'primary',
  emptyHint = '这段时间没有采集到数据',
}: TimeSeriesChartProps) {
  const [hover, setHover] = useState<number | null>(null)

  const model = useMemo(() => buildModel(points, intervalSeconds), [points, intervalSeconds])

  if (!model) {
    // 空数据要说清是「没采到」，而不是留一个空白框——空白框会让人以为是
    // 图表没加载出来，于是反复刷新。
    return (
      <div
        className="flex items-center justify-center rounded-control bg-sunken text-sm text-ink-3"
        style={{ height }}
      >
        {emptyHint}
      </div>
    )
  }

  const { min, max, ticks } = axisOf(model.allValues)
  const W = 100 // 用百分比坐标，宽度自适应
  const H = height
  const stroke = TONE_STROKE[tone] ?? TONE_STROKE.primary

  const y = (v: number) => {
    const span = max - min || 1
    // 留出上下各 6px，避免极值贴边被裁掉。
    return H - 6 - ((v - min) / span) * (H - 12)
  }
  const x = (i: number) => (model.total === 1 ? W / 2 : (i / (model.total - 1)) * W)

  const hovered = hover !== null ? (model.flat[hover] ?? null) : null
  const last = model.flat[model.flat.length - 1]

  return (
    <div className="flex flex-col gap-1.5">
      <div className="relative" style={{ height }}>
        <svg
          viewBox={`0 0 ${W} ${H}`}
          preserveAspectRatio="none"
          className="h-full w-full overflow-visible"
          role="img"
        >
          {/* 纵向网格：3 条，足够读数又不会把图压花。 */}
          {ticks.map((t) => (
            <line
              key={t}
              x1={0}
              x2={W}
              y1={y(t)}
              y2={y(t)}
              stroke="currentColor"
              className="text-line"
              strokeWidth={0.4}
              vectorEffect="non-scaling-stroke"
            />
          ))}

          {/* **每段单独画**：缺口处不连起来。 */}
          {model.segments.map((seg, si) => {
            const only = seg.length === 1 ? seg[0] : undefined
            return (
              <g key={si}>
                <polyline
                  points={seg.map((p) => `${x(p.index)},${y(p.value)}`).join(' ')}
                  fill="none"
                  stroke={stroke}
                  strokeWidth={1.5}
                  strokeLinejoin="round"
                  strokeLinecap="round"
                  vectorEffect="non-scaling-stroke"
                />
                {/* 段只有一个点时画不出线，用一个小圆点标出来，
                    否则那段数据在图上完全不可见。 */}
                {only && <circle cx={x(only.index)} cy={y(only.value)} r={1.6} fill={stroke} />}
              </g>
            )
          })}

          {hovered && (
            <line
              x1={x(hovered.index)}
              x2={x(hovered.index)}
              y1={0}
              y2={H}
              stroke="currentColor"
              className="text-ink-3"
              strokeWidth={0.6}
              vectorEffect="non-scaling-stroke"
            />
          )}
        </svg>

        {/* 悬停热区：按点均分，比逐个渲染圆点命中率更高。 */}
        <div className="absolute inset-0 flex" onMouseLeave={() => setHover(null)}>
          {model.flat.map((p, i) => (
            <div
              key={i}
              className="flex-1"
              onMouseEnter={() => setHover(i)}
              title={`${format(p.value)} · ${shortTime(p.at)}`}
            />
          ))}
        </div>
      </div>

      <div className="flex items-baseline justify-between text-xs text-ink-3">
        <span>{model.flat[0] ? shortTime(model.flat[0].at) : ''}</span>
        {hovered ? (
          <span className="text-ink">
            {format(hovered.value)} · {shortTime(hovered.at)}
          </span>
        ) : (
          <span>
            {format(min)} ~ {format(max)}
          </span>
        )}
        <span>{last ? shortTime(last.at) : ''}</span>
      </div>

      {/* 缺口单独说明：断点本身在图上只是一处空白，而用户需要知道
          「那段时间没采到」，否则会把它当成指标为 0。 */}
      {model.gaps > 0 && (
        <p className="text-xs text-warning">
          这段时间有 {model.gaps} 处没有采集到数据（图上已断开）——那几段并非指标为 0，
          而是采样缺失。
        </p>
      )}
    </div>
  )
}

interface Flat {
  at: string
  value: number
  index: number
}

function buildModel(points: SeriesPoint[], intervalSeconds?: number) {
  const valid = points.filter((p) => Number.isFinite(p.value))
  if (valid.length === 0) return null

  const flat: Flat[] = valid.map((p, i) => ({ at: p.at, value: p.value, index: i }))
  const allValues = valid.map((p) => p.value)

  // 缺口阈值：优先用服务端给的采样间隔，没有就按实际点距的中位数推。
  const threshold = (intervalSeconds && intervalSeconds > 0
    ? intervalSeconds
    : medianGapSeconds(valid)) * GAP_FACTOR

  const segments: Flat[][] = []
  let current: Flat[] = []
  let gaps = 0
  flat.forEach((point, i) => {
    const prev = i > 0 ? flat[i - 1] : undefined
    if (prev) {
      const dt = secondsBetween(prev.at, point.at)
      if (dt > threshold) {
        gaps++
        if (current.length > 0) segments.push(current)
        current = []
      }
    }
    current.push(point)
  })
  if (current.length > 0) segments.push(current)

  return { flat, segments, gaps, allValues, total: flat.length }
}

function medianGapSeconds(points: SeriesPoint[]): number {
  if (points.length < 2) return 60
  const gaps: number[] = []
  for (let i = 1; i < points.length; i++) {
    const prev = points[i - 1]
    const cur = points[i]
    if (!prev || !cur) continue
    const dt = secondsBetween(prev.at, cur.at)
    if (dt > 0) gaps.push(dt)
  }
  if (gaps.length === 0) return 60
  gaps.sort((a, b) => a - b)
  return gaps[Math.floor(gaps.length / 2)] ?? 60
}

function secondsBetween(a: string, b: string): number {
  const ta = Date.parse(a)
  const tb = Date.parse(b)
  if (!Number.isFinite(ta) || !Number.isFinite(tb)) return 0
  return Math.abs(tb - ta) / 1000
}

/** 纵轴范围与刻度。上下各留出一成，避免极值贴边。 */
function axisOf(values: number[]) {
  let min = values.length > 0 ? Math.min(...values) : 0
  let max = values.length > 0 ? Math.max(...values) : 1
  if (min === max) {
    // 一条水平线时给一个对称区间，否则所有点会叠在同一条线上。
    const pad = Math.abs(min) * 0.1 || 1
    min -= pad
    max += pad
  }
  const minV = Math.min(min, 0)
  const ticks = [minV, minV + (max - minV) / 2, max]
  return { min: minV, max, ticks }
}

function shortTime(at: string): string {
  const d = new Date(at)
  if (Number.isNaN(d.getTime())) return at
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
