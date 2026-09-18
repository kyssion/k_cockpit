/**
 * ResourceQuotaPage 管理资源配额与超限处置（F-4-10）。
 *
 * 这一页补上的是「采了但不用」的那一步：流量与运行时长一直在按天累计，
 * 而此前没有任何地方读它们做判断。
 *
 * 三处要在界面上说清的东西：
 *
 *  默认不限   上限填 0 表示不限。默认给一个上限会让用户在自己什么都没做的
 *              时候撞上一堵看不见的墙。
 *  处置两档   **限速**（还能用但变慢，默认）与**断网**（停掉）。后者会让业务
 *              直接中断，选它应当是有意为之。
 *  warned 与 limited 是两回事   前者"快到了"，后者"**已经处置了**"。不分开
 *              的话，用户在网络变慢时无法判断是自己用超了还是会话出了问题。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { quotaEnforceApi, RESOURCE_QUOTAS, type QuotaView } from '@/api/quotaenforce'
import { userApi } from '@/api/useradmin'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

const STATUS_LABEL: Record<string, string> = {
  ok: '正常',
  warned: '接近上限',
  limited: '已超限处置',
}

export function ResourceQuotaPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [editing, setEditing] = useState<{ userID: number; name: string; dim: string } | null>(null)
  const [error, setError] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const users = useQuery({ queryKey: ['users'], queryFn: () => userApi.list({ page_size: 200 }) })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const list = useQuery({
    queryKey: ['resource-quotas', effectiveNodeID],
    queryFn: () => quotaEnforceApi.list(effectiveNodeID),
    enabled: effectiveNodeID > 0,
    // 处置状态会自己变化（用量涨上去），轮询让用户不必手动刷新。
    refetchInterval: 60000,
  })

  const remove = useMutation({
    mutationFn: (id: number) => quotaEnforceApi.remove(id),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['resource-quotas'] })
    },
    onError: (e) => setError(describe(e)),
  })

  if (nodes.isPending || users.isPending) return <PageLoading />
  const items = list.data?.items ?? []
  const userList = users.data?.items ?? []

  // 按用户分组：配额是"某个人被分到多少资源"，按维度平铺看不出这一点。
  const byUser = new Map<number, { name: string; rows: QuotaView[] }>()
  for (const q of items) {
    const g = byUser.get(q.user_id) ?? { name: q.username ?? `#${q.user_id}`, rows: [] }
    g.rows.push(q)
    byUser.set(q.user_id, g)
  }

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">资源配额</h1>
          <p className="mt-1 text-base text-ink-3">
            按用户限制月流量与月运行时长。超限后按设定处置：
            <span className="text-ink-2">限速</span>（还能用但变慢）或
            <span className="text-ink-2">断网</span>（停掉）。上限填 0 表示不限。
          </p>
        </div>
        <div className="flex items-end gap-2">
          <div className="flex flex-col gap-1">
            <label className="text-xs text-ink-3">节点</label>
            <select
              value={effectiveNodeID}
              onChange={(e) => setNodeID(Number(e.target.value))}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {(nodes.data ?? []).map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </select>
          </div>
        </div>
      </header>

      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {list.isPending ? (
        <PageLoading />
      ) : byUser.size === 0 ? (
        <EmptyState
          title="还没有设置配额"
          description="未设置配额的用户不受限制——这是默认行为。需要限额时在下面为具体用户设置。"
        />
      ) : (
        <div className="flex flex-col gap-4">
          {[...byUser.entries()].map(([uid, g]) => (
            <section key={uid} className="rounded-card border border-line bg-surface">
              <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">
                {g.name}
              </h2>
              <table className="w-full text-left text-sm">
                <tbody>
                  {g.rows.map((q) => (
                    <QuotaRow
                      key={q.id}
                      q={q}
                      onEdit={() => setEditing({ userID: uid, name: g.name, dim: q.dimension })}
                      onRemove={() => remove.mutate(q.id)}
                    />
                  ))}
                </tbody>
              </table>
            </section>
          ))}
        </div>
      )}

      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="text-base font-medium text-ink-2">为其他用户设置</h2>
        <div className="mt-2 flex flex-wrap gap-2">
          {userList.map((u) => (
            <button
              key={u.id}
              className="rounded-control border border-line px-2.5 py-1 text-sm text-ink-2 hover:bg-sunken"
              onClick={() => setEditing({ userID: u.id, name: u.username, dim: 'traffic_in' })}
            >
              {u.username}
            </button>
          ))}
        </div>
      </section>

      <QuotaModal
        open={editing !== null}
        nodeID={effectiveNodeID}
        target={editing}
        existing={items}
        onClose={() => setEditing(null)}
        onDone={() => {
          setEditing(null)
          setError('')
          void queryClient.invalidateQueries({ queryKey: ['resource-quotas'] })
        }}
        onError={(m) => {
          setEditing(null)
          setError(m)
        }}
      />
    </div>
  )
}

function QuotaRow({
  q,
  onEdit,
  onRemove,
}: {
  q: QuotaView
  onEdit: () => void
  onRemove: () => void
}) {
  const unlimited = q.limit_value <= 0
  const tone = q.status === 'limited' ? 'danger' : q.status === 'warned' ? 'warning' : 'success'

  return (
    <tr className="border-b border-line last:border-0">
      <td className="px-4 py-2 text-ink-2">{q.dimension_label}</td>
      <td className="px-4 py-2">
        {unlimited ? (
          <span className="text-ink-3">不限</span>
        ) : (
          <span className="kc-nums text-ink">
            {q.used_value} / {q.limit_value} {q.dimension_unit}
            <span className="ml-2 text-xs text-ink-3">（{q.used_percent}%）</span>
          </span>
        )}
      </td>
      <td className="px-4 py-2">
        {unlimited ? (
          <span className="text-ink-3">—</span>
        ) : (
          <StatusBadge tone={tone}>{STATUS_LABEL[q.status] ?? q.status}</StatusBadge>
        )}
      </td>
      <td className="px-4 py-2 text-xs text-ink-3">
        {/* 处置时刻要单独显示：用户问「我的网什么时候开始变慢的」，答案在这里。 */}
        {q.limited_at
          ? `已于 ${formatTime(q.limited_at)} 处置（${q.action === 'block' ? '断网' : '限速'}）`
          : q.detail || ''}
      </td>
      <td className="whitespace-nowrap px-4 py-2 text-right text-sm">
        <button className="text-primary hover:underline" onClick={onEdit}>
          修改
        </button>
        <button className="ml-3 text-danger hover:underline" onClick={onRemove}>
          删除
        </button>
      </td>
    </tr>
  )
}

function QuotaModal({
  open,
  nodeID,
  target,
  existing,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  target: { userID: number; name: string; dim: string } | null
  existing: QuotaView[]
  onClose: () => void
  onDone: () => void
  onError: (m: string) => void
}) {
  const [dim, setDim] = useState(target?.dim ?? 'traffic_in')
  const [limit, setLimit] = useState('')
  const [action, setAction] = useState<'throttle' | 'block'>('throttle')
  const [seeded, setSeeded] = useState<string | null>(null)

  // 换目标时用已有值初始化。
  const key = `${target?.userID ?? 0}-${target?.dim ?? ''}`
  if (seeded !== key) {
    setSeeded(key)
    setDim(target?.dim ?? 'traffic_in')
    const cur = existing.find((q) => q.user_id === target?.userID && q.dimension === (target?.dim ?? 'traffic_in'))
    setLimit(cur && cur.limit_value > 0 ? String(cur.limit_value) : '')
    setAction(cur?.action ?? 'throttle')
  }

  const save = useMutation({
    mutationFn: () =>
      quotaEnforceApi.set(nodeID, {
        user_id: target?.userID ?? 0,
        dimension: dim,
        limit_value: Number(limit) || 0,
        action,
      }),
    onSuccess: onDone,
    onError: (e) => onError(describe(e)),
  })

  const unit = RESOURCE_QUOTAS.dimensions.find((d) => d.value === dim)?.unit ?? ''

  return (
    <Modal
      open={open}
      title={`为 ${target?.name ?? ''} 设置配额`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={save.isPending} onClick={() => save.mutate()}>
            保存
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">维度</label>
          <select
            value={dim}
            onChange={(e) => setDim(e.target.value)}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            {RESOURCE_QUOTAS.dimensions.map((d) => (
              <option key={d.value} value={d.value}>
                {d.label}
              </option>
            ))}
          </select>
        </div>

        <Input
          label={`上限（${unit}）`}
          value={limit}
          placeholder="留空或填 0 表示不限"
          onChange={(e) => setLimit(e.target.value)}
          hint={`按自然月统计（UTC 分桶）。达到 ${RESOURCE_QUOTAS.warnPercent}% 时预警，超过上限时按下面的方式处置。留空表示不限——默认不限额，避免用户在什么都没做的时候撞上一堵看不见的墙。`}
        />

        <div className="flex flex-col gap-1.5">
          <span className="text-sm text-ink-2">超限后</span>
          <label className="flex cursor-pointer gap-2.5 rounded-control border border-line px-3 py-2">
            <input
              type="radio"
              checked={action === 'throttle'}
              onChange={() => setAction('throttle')}
              className="mt-1"
            />
            <span className="flex flex-col">
              <span className="text-base text-ink">限速（推荐）</span>
              <span className="text-xs text-ink-3">还能用，但变慢。用户的业务不会中断。</span>
            </span>
          </label>
          <label className="flex cursor-pointer gap-2.5 rounded-control border border-warning/40 px-3 py-2">
            <input
              type="radio"
              checked={action === 'block'}
              onChange={() => setAction('block')}
              className="mt-1"
            />
            <span className="flex flex-col">
              <span className="text-base text-warning">断网</span>
              <span className="text-xs text-ink-3">
                直接停掉。业务会立刻中断——选它请确认是有意为之，而不是没注意默认值。
              </span>
            </span>
          </label>
        </div>

        <p className="rounded-control bg-sunken px-3 py-2 text-xs text-ink-3">
          修改上限会清除该维度的处置状态：把上限从 100 提到 200 之后，那条
          「已限速」的记录就不再成立——不清的话，用户明明已经合规，网络却还是慢的。
        </p>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}

function formatTime(s: string): string {
  const t = Date.parse(s)
  if (!Number.isFinite(t)) return s
  return new Date(t).toLocaleString()
}
