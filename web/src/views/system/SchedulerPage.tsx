/**
 * SchedulerPage 展示周期性调度器与它们的调度事件（F-7-04）。
 *
 * 页面要回答两个不同的问题，而它们**不能混在一起**：
 *
 *   1. 有哪些东西在周期性跑？   → 注册表（代码里登记的）
 *   2. 它们最近实际做了什么？   → 事件表（只记实际动作）
 *
 * 混起来的后果是：一个正常但最近无事可做的调度器会因为「没有事件」而从
 * 列表里消失，而用户会把它读成故障。因此列表**始终来自注册表**，事件是
 * 挂在每一项下面的附加信息。
 */
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { formatAgo, formatInterval, schedulerApi, type SchedulerView } from '@/api/scheduler'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { StatusBadge } from '@/components/common/StatusBadge'

export function SchedulerPage() {
  const [onlyFailed, setOnlyFailed] = useState(false)
  // 按调度器筛选（G-38）：后端一直支持按 key 过滤，之前只是没有入口——
  // 事件一多，「只看失败」之外还需要定位到单个调度器的历史。
  const [schedulerKey, setSchedulerKey] = useState('')

  const overview = useQuery({
    queryKey: ['schedulers'],
    queryFn: schedulerApi.overview,
    refetchInterval: 30000,
  })

  const events = useQuery({
    queryKey: ['scheduler-events', onlyFailed, schedulerKey],
    queryFn: () => schedulerApi.events(schedulerKey || undefined, onlyFailed ? 'failed' : undefined),
  })

  if (overview.isPending) return <PageLoading />

  const items = overview.data?.items ?? []
  const groups = groupBy(items)

  return (
    <div className="flex flex-col gap-5">
      <header>
        <h1 className="text-xl font-semibold text-ink">调度器</h1>
        <p className="mt-1 text-base text-ink-3">
          系统内置的周期性工作。这里
          <span className="text-ink-2">只记录实际发生的动作</span>
          ——普通轮询不留痕。因此「最近没有事件」通常意味着它没事可做，而不是它坏了。
        </p>
      </header>

      <div className="flex flex-col gap-6">
        {groups.map(([group, list]) => (
          <section key={group} className="flex flex-col gap-3">
            <h2 className="text-base font-medium text-ink-2">{group}</h2>
            {list.map((v) => (
              <SchedulerCard key={v.scheduler.key} view={v} />
            ))}
          </section>
        ))}
      </div>

      <section className="flex flex-col gap-3">
        <header className="flex flex-wrap items-baseline justify-between gap-3">
          <h2 className="text-base font-medium text-ink-2">调度事件</h2>
          <div className="flex items-center gap-2">
            <select
              value={schedulerKey}
              onChange={(e) => setSchedulerKey(e.target.value)}
              aria-label="按调度器筛选"
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value="">全部调度器</option>
              {items.map((v) => (
                <option key={v.scheduler.key} value={v.scheduler.key}>
                  {v.scheduler.name}
                </option>
              ))}
            </select>
            <Button
              size="sm"
              variant="secondary"
              onClick={() => setOnlyFailed((v) => !v)}
            >
              {onlyFailed ? '显示全部' : '只看失败'}
            </Button>
          </div>
        </header>

        {events.isPending ? (
          <PageLoading />
        ) : (events.data?.items ?? []).length === 0 ? (
          <EmptyState
            title={onlyFailed ? '最近没有失败的调度' : '还没有调度事件'}
            description="事件只在调度器**实际做了事**的时候产生。没有事件属于正常——检查、扫描这类轮询不记账。"
          />
        ) : (
          <div className="overflow-hidden rounded-card border border-line">
            <table className="w-full text-left text-sm">
              <thead className="bg-sunken text-ink-3">
                <tr>
                  <th className="px-3 py-2 font-normal">时间</th>
                  <th className="px-3 py-2 font-normal">调度器</th>
                  <th className="px-3 py-2 font-normal">范围</th>
                  <th className="px-3 py-2 font-normal">做了什么</th>
                  <th className="px-3 py-2 font-normal">结果</th>
                </tr>
              </thead>
              <tbody>
                {(events.data?.items ?? []).map((e) => (
                  <tr key={e.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                    <td className="whitespace-nowrap px-3 py-2 text-ink-3">
                      {formatAgo(e.at)}
                    </td>
                    <td className="px-3 py-2 text-ink">{e.scheduler_name ?? e.scheduler_key}</td>
                    <td className="px-3 py-2 text-ink-2">{e.scope ?? '—'}</td>
                    <td className="px-3 py-2 text-ink-2">{e.message ?? '—'}</td>
                    <td className="px-3 py-2">
                      <StatusBadge tone={e.status === 'failed' ? 'danger' : 'success'}>
                        {e.status === 'failed' ? '失败' : '完成'}
                      </StatusBadge>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  )
}

function SchedulerCard({ view }: { view: SchedulerView }) {
  const { scheduler: info, events, last_action_at, failed_count } = view

  return (
    <div className="rounded-card border border-line bg-surface p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="flex flex-wrap items-baseline gap-2">
          <span className="text-base font-medium text-ink">{info.name}</span>
          {/* 周期必须显示：界面上没有别的字段能告诉用户「多久做一次」，
              而它是判断这个东西是不是还活着的唯一依据。 */}
          <span className="rounded-pill bg-sunken px-1.5 py-0.5 text-xs text-ink-3">
            {formatInterval(info.interval_seconds)}
          </span>
          {failed_count > 0 && (
            <span className="rounded-pill bg-danger/10 px-1.5 py-0.5 text-xs text-danger">
              最近 {failed_count} 次失败
            </span>
          )}
        </span>
        <span className="text-xs text-ink-3">
          {last_action_at ? (
            <>最近动作 {formatAgo(last_action_at)}</>
          ) : (
            // 这一句是整页最要紧的一处表达。
            <span title="它一直在按周期运行，只是最近没有需要做的事">
              运行中 · 暂无事件
            </span>
          )}
        </span>
      </div>

      <p className="mt-1.5 text-sm text-ink-3">{info.description}</p>

      {events.length > 0 && (
        <ul className="mt-2 flex flex-col gap-1 border-t border-line pt-2">
          {events.map((e) => (
            <li key={e.id} className="flex flex-wrap items-baseline gap-2 text-sm">
              <span className={e.status === 'failed' ? 'text-danger' : 'text-ink-2'}>
                {e.message ?? '—'}
              </span>
              {e.scope && <span className="text-xs text-ink-3">（{e.scope}）</span>}
              <span className="text-xs text-ink-3">{formatAgo(e.at)}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/** groupBy 按分组归并，保持后端给出的分组顺序。 */
function groupBy(items: SchedulerView[]): [string, SchedulerView[]][] {
  const order: string[] = []
  const buckets = new Map<string, SchedulerView[]>()
  for (const v of items) {
    const g = v.scheduler.group
    if (!buckets.has(g)) {
      buckets.set(g, [])
      order.push(g)
    }
    buckets.get(g)!.push(v)
  }
  return order.map((g) => [g, buckets.get(g)!])
}
