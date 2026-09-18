/**
 * MetricsPanel 是主机与虚拟机共用的指标面板（F-8-01 / F-8-02）。
 *
 * 两边画的是同一组指标（CPU / 内存 / 网络），只是取数入口不同，因此做成
 * 一个接收 loader 的组件而不是各写一遍——两份实现迟早会在「内存的百分比
 * 怎么算」「缺口怎么标」这类细节上分叉，然后同一份数据在两个页面上显示
 * 出不同的结论。
 */
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import {
  formatBytes,
  formatDuration,
  formatMB,
  formatPercent,
  monitorApi,
  RANGES,
  type MetricSeries,
  type RangeKey,
} from '@/api/monitor'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { TimeSeriesChart } from '@/components/common/TimeSeriesChart'

export interface MetricsPanelProps {
  /** 取数入口。主机用 monitorApi.host，虚拟机用 monitorApi.vm。 */
  load: (key: RangeKey) => Promise<MetricSeries>
  /** 查询缓存键的前缀，用来区分不同对象。 */
  queryKey: (string | number)[]
  /** 顶部说明，写清这块数据的来源与用途。 */
  description?: string
  /** 受控区间：同一页上有别的组件也要跟着这个区间取数时使用。 */
  range?: RangeKey
  onRangeChange?: (v: RangeKey) => void
  hideRangePicker?: boolean
}

export function MetricsPanel({
  load,
  queryKey,
  description,
  range: controlledRange,
  onRangeChange,
  hideRangePicker,
}: MetricsPanelProps) {
  const [innerRange, setInnerRange] = useState<RangeKey>('1h')
  const range = controlledRange ?? innerRange
  const setRange = (v: RangeKey) => {
    if (controlledRange === undefined) setInnerRange(v)
    onRangeChange?.(v)
  }

  const series = useQuery({
    queryKey: [...queryKey, range],
    queryFn: () => load(range),
    // 采样是固定间隔推送的，跟着刷一次才看得到最新的点。而更长的区间
    // 变化本来就慢，刷得太勤只是白费请求。
    refetchInterval: range === '1h' ? 15000 : false,
  })

  const points = series.data?.points ?? []
  const interval = series.data?.interval_seconds
  // 内存总量是配置属性，取最新一个点即可（配置变更时它才会变）。
  const lastPoint = points[points.length - 1]

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          {description && <p className="text-base text-ink-3">{description}</p>}
        </div>
        {!hideRangePicker && (
          <div className="flex gap-1">
            {RANGES.map((r) => (
              <button
                key={r.key}
                onClick={() => setRange(r.key)}
                className={`rounded-control px-2.5 py-1 text-sm ${
                  range === r.key
                    ? 'bg-primary/10 text-primary'
                    : 'text-ink-3 hover:bg-sunken hover:text-ink-2'
                }`}
              >
                {r.label}
              </button>
            ))}
          </div>
        )}
      </header>

      {series.isPending ? (
        <PageLoading />
      ) : series.isError ? (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          指标读取失败，请稍后重试。
        </p>
      ) : points.length === 0 ? (
        <EmptyState
          title="这段时间没有指标"
          description="采样器按固定间隔采集，机器关机或采样器未运行时不会产生数据。这不是「指标为 0」。"
        />
      ) : (
        <div className="flex flex-col gap-5">
          <MetricCard
            title="CPU 使用率"
            subtitle={`${points.length} 个采样点${interval ? `，间隔 ${interval} 秒` : ''}`}
            tone="primary"
          >
            <TimeSeriesChart
              points={points.map((p) => ({ at: p.at, value: p.cpu_percent }))}
              intervalSeconds={interval}
              format={formatPercent}
              tone="primary"
            />
          </MetricCard>

          <MetricCard
            title="内存使用"
            subtitle={lastPoint ? `总计 ${formatMB(lastPoint.mem_total_mb)}` : undefined}
            tone="success"
          >
            <TimeSeriesChart
              points={points.map((p) => ({ at: p.at, value: p.mem_used_mb }))}
              intervalSeconds={interval}
              format={formatMB}
              tone="success"
            />
          </MetricCard>

          <div className="grid gap-4 sm:grid-cols-2">
            <MetricCard title="入站流量" tone="warning">
              <TimeSeriesChart
                points={points.map((p) => ({ at: p.at, value: p.net_in_bytes }))}
                intervalSeconds={interval}
                format={formatBytes}
                tone="warning"
                height={90}
              />
            </MetricCard>
            <MetricCard title="出站流量" tone="danger">
              <TimeSeriesChart
                points={points.map((p) => ({ at: p.at, value: p.net_out_bytes }))}
                intervalSeconds={interval}
                format={formatBytes}
                tone="danger"
                height={90}
              />
            </MetricCard>
          </div>
        </div>
      )}
    </div>
  )
}

function MetricCard({
  title,
  subtitle,
  children,
}: {
  title: string
  subtitle?: string
  tone?: string
  children: React.ReactNode
}) {
  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="mb-3 flex items-baseline justify-between">
        <h3 className="text-base font-medium text-ink">{title}</h3>
        {subtitle && <span className="text-xs text-ink-3">{subtitle}</span>}
      </div>
      {children}
    </section>
  )
}

/** 供页面直接使用的两个特化版本，避免每处都写 loader。 */
export function HostMetricsPanel({ nodeID, description }: { nodeID: number; description?: string }) {
  return (
    <MetricsPanel
      load={(key) => monitorApi.host(nodeID, key)}
      queryKey={['monitor', 'host', nodeID]}
      description={description}
    />
  )
}

export function VMMetricsPanel({ vmID, description }: { vmID: number; description?: string }) {
  const [range, setRange] = useState<RangeKey>('1h')
  const runtime = useQuery({
    queryKey: ['monitor', 'runtime', vmID, range],
    queryFn: () => monitorApi.runtime(vmID, range),
  })

  return (
    <div className="flex flex-col gap-4">
      {/* 运行时长是 F-8-06 超限处置与配额里「运行时长」维度的共同数据源，
          因此它按**区间**统计而不是给一个总数——用户要看的是「这段时间
          它跑了多久」，而那正是计费与超限判断的依据。 */}
      <div className="rounded-card border border-line bg-surface px-4 py-3">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <span className="text-base font-medium text-ink">区间内运行时长</span>
          <span className="text-base text-ink">
            {runtime.isPending
              ? '读取中…'
              : runtime.isError
                ? '读取失败'
                : formatDuration(runtime.data?.seconds ?? 0)}
          </span>
        </div>
        <p className="mt-1 text-xs text-ink-3">
          按天累计，与「监控」里的区间选择联动。它是超限处置与配额中运行时长维度的依据。
        </p>
      </div>

      <RangeAwareRange onChange={setRange} value={range} />
      <MetricsPanel
        load={(key) => monitorApi.vm(vmID, key)}
        queryKey={['monitor', 'vm', vmID]}
        description={description}
        range={range}
        onRangeChange={setRange}
        hideRangePicker
      />
    </div>
  )
}

/** RangeAwareRange 只是把区间选择器提到运行时长卡片之上，让两者共用。 */
function RangeAwareRange({
  value,
  onChange,
}: {
  value: RangeKey
  onChange: (v: RangeKey) => void
}) {
  return (
    <div className="flex gap-1">
      {RANGES.map((r) => (
        <button
          key={r.key}
          onClick={() => onChange(r.key)}
          className={`rounded-control px-2.5 py-1 text-sm ${
            value === r.key
              ? 'bg-primary/10 text-primary'
              : 'text-ink-3 hover:bg-sunken hover:text-ink-2'
          }`}
        >
          {r.label}
        </button>
      ))}
    </div>
  )
}
