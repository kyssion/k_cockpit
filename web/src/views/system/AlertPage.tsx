/**
 * AlertPage 是告警中心（F-8-07）。
 *
 * 三处要在界面上说清的东西：
 *
 *  1. **确认 ≠ 解决**。确认只是"我看到了"，问题仍然存在，因此条目不会消失。
 *     把它做成"关掉"会让一个仍然存在的问题从视野里消失。
 *  2. **告警是持续状态，不是事件流**。同一个问题只占一行，"从什么时候开始"
 *     由 first_at 表达——它是判断"刚发生的还是拖了三天"的唯一依据。
 *  3. **问题消失会自动关闭但保留记录**：恢复过这件事本身有价值（可以据此
 *     判断问题是偶发还是持续）。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import { alertApi, ALERT_KIND_LABEL, type AlertItem } from '@/api/alert'
import { ApiError, NetworkError } from '@/api/client'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { StatusBadge } from '@/components/common/StatusBadge'
import { relativeTime } from '@/utils/format'

export function AlertPage() {
  const queryClient = useQueryClient()
  const [status, setStatus] = useState<'active' | 'acked' | ''>('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const list = useQuery({
    queryKey: ['alerts', status],
    queryFn: () => alertApi.list({ status: status || undefined }),
    // 告警由后台每 5 分钟评估一次，跟着刷才看得到新条目。
    refetchInterval: 60000,
  })

  const refresh = () => {
    setError('')
    void queryClient.invalidateQueries({ queryKey: ['alerts'] })
  }

  const ack = useMutation({
    mutationFn: (id: number) => alertApi.ack(id),
    onSuccess: refresh,
    onError: (e) => setError(describe(e)),
  })

  const ackAll = useMutation({
    mutationFn: alertApi.ackAll,
    onSuccess: (res) => {
      setNotice(`已确认 ${res.acknowledged} 条。它们仍会显示在列表里——确认不等于解决。`)
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const items = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold text-ink">告警中心</h1>
          <p className="mt-1 text-base text-ink-3">
            共 {items.length} 条，其中未确认 {list.data?.active ?? 0} 条
            {list.data?.danger ? `（${list.data.danger} 条严重）` : ''}。
            告警每 5 分钟评估一次；问题消失后会自动关闭。
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Filter
            value={status}
            onChange={setStatus}
            options={[
              { value: '', label: '全部' },
              { value: 'active', label: '未确认' },
              { value: 'acked', label: '已确认' },
            ]}
          />
          <Button
            variant="secondary"
            size="sm"
            disabled={!list.data?.active}
            loading={ackAll.isPending}
            onClick={() => ackAll.mutate()}
          >
            全部确认
          </Button>
        </div>
      </header>

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {list.isPending ? (
        <PageLoading />
      ) : items.length === 0 ? (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState
            title="没有需要处理的告警"
            description="节点离线、任务失败、配额超限、存储与负载过高都会在这里出现。"
            action={
              <Link to="/">
                <Button size="sm">回到工作台</Button>
              </Link>
            }
          />
        </div>
      ) : (
        <div className="flex flex-col gap-3">
          {items.map((a) => (
            <AlertRow key={a.id} alert={a} onAck={() => ack.mutate(a.id)} busy={ack.isPending} />
          ))}
        </div>
      )}
    </div>
  )
}

function AlertRow({
  alert,
  onAck,
  busy,
}: {
  alert: AlertItem
  onAck: () => void
  busy: boolean
}) {
  const tone = alert.level === 'danger' ? 'danger' : 'warning'
  return (
    <article className="rounded-card border border-line bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <StatusBadge tone={tone}>{ALERT_KIND_LABEL[alert.kind] ?? alert.kind}</StatusBadge>
            {alert.status === 'acked' && <StatusBadge tone="idle">已确认</StatusBadge>}
            {alert.node_id && <span className="text-xs text-ink-3">节点 #{alert.node_id}</span>}
          </div>
          <h2 className="mt-1.5 text-base text-ink">{alert.title}</h2>
          {alert.detail && <p className="mt-0.5 text-sm text-ink-3">{alert.detail}</p>}
          <p className="mt-1.5 text-xs text-ink-3">
            始于 {relativeTime(alert.first_at)} · 最近一次 {relativeTime(alert.last_at)}
            {alert.ack_at ? ` · 已确认于 ${relativeTime(alert.ack_at)}` : ''}
          </p>
        </div>
        {alert.status === 'active' && (
          <Button variant="secondary" size="sm" disabled={busy} onClick={onAck}>
            确认
          </Button>
        )}
      </div>
    </article>
  )
}

function Filter<T extends string>({
  value,
  onChange,
  options,
}: {
  value: T
  onChange: (v: T) => void
  options: { value: T; label: string }[]
}) {
  return (
    <div className="inline-flex overflow-hidden rounded-control border border-line-strong">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          onClick={() => onChange(o.value)}
          className={
            'px-2.5 py-1 text-sm ' +
            (value === o.value ? 'bg-brand/10 text-brand' : 'text-ink-2 hover:bg-raised')
          }
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
