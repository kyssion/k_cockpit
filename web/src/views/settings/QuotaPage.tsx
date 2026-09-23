/**
 * QuotaPage 管理存储配额（F-9-02）。
 *
 * 配额是**按用户按节点**给的：磁盘就在那台宿主机上，一个用户在 A 节点的
 * 用量与 B 节点无关——合成一个总数会让「A 满了」影响到 B 上的操作，而两者
 * 根本没有共用任何资源。
 *
 * 页面刻意把**用量明细**摊开显示（虚拟机磁盘 / 导出 / 模板 / 文件），而不是
 * 只给一个总数：用户超额时第一个问题永远是「我这些空间被什么占了」，只给
 * 一个总数等于让他自己去猜。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { quotaApi, type QuotaUsage } from '@/api/quota'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

const GB = 1024 * 1024 * 1024

export function QuotaPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [editing, setEditing] = useState<QuotaUsage | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const list = useQuery({
    queryKey: ['quotas', effectiveNodeID],
    queryFn: () => quotaApi.list(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['quotas'] })
    void queryClient.invalidateQueries({ queryKey: ['audit'] })
  }

  const setQuota = useMutation({
    mutationFn: (input: Parameters<typeof quotaApi.set>[0]) => quotaApi.set(input),
    onSuccess: () => {
      setEditing(null)
      setError('')
      setNotice('配额已更新——它只影响「还能不能新增」，不影响已有的资源')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (nodes.isPending) return <PageLoading />
  const items = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold text-ink">存储配额</h1>
          <p className="mt-1 text-base text-ink-3">
            配额按<span className="text-ink-2">用户 × 节点</span>给——磁盘就在那台
            宿主机上，两个节点之间没有共用任何资源。
          </p>
        </div>
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
        <EmptyState
          title="还没有配额记录"
          description="未设配额的用户不受限制——先能用，再谈限额。需要限制时在这里新建。"
        />
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">用户</th>
                <th className="px-4 py-2.5 font-medium">用量 / 限额</th>
                <th className="px-4 py-2.5 font-medium">明细</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((u) => (
                <tr key={`${u.user_id}-${u.node_id}`} className="border-t border-line">
                  <td className="px-4 py-2.5">
                    <span className="kc-nums text-ink">#{u.user_id}</span>
                  </td>

                  <td className="px-4 py-2.5">
                    <span className="kc-nums text-ink">{formatBytes(u.total_bytes)}</span>
                    <span className="text-ink-3"> / </span>
                    {/* 「不限」与「还有很多」必须长得不一样：前者永远不变，
                        后者会随用量变化。 */}
                    <span className={u.unlimited ? 'text-ink-3' : 'text-ink'}>
                      {u.unlimited ? '不限' : formatBytes(u.quota_bytes)}
                    </span>
                    <Meter used={u.total_bytes} quota={u.quota_bytes} unlimited={u.unlimited} />
                  </td>

                  {/* 明细摊开：超额时第一个问题是「空间被什么占了」。 */}
                  <td className="px-4 py-2.5 text-xs text-ink-3">
                    <div>虚拟机 {formatBytes(u.vm_disks_bytes)}</div>
                    <div>文件 {formatBytes(u.files_bytes)}</div>
                    <div>导出 {formatBytes(u.exports_bytes)}</div>
                    <div>模板 {formatBytes(u.templates_bytes)}</div>
                  </td>

                  <td className="px-4 py-2.5">
                    {u.read_only ? (
                      <StatusBadge tone="warning">只读</StatusBadge>
                    ) : u.over_quota ? (
                      <StatusBadge tone="danger">已超限</StatusBadge>
                    ) : (
                      <StatusBadge tone="success">正常</StatusBadge>
                    )}
                  </td>

                  <td className="px-4 py-2.5">
                    <button
                      className="text-sm text-brand hover:underline"
                      onClick={() => setEditing(u)}
                    >
                      设置
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <EditQuotaModal
        usage={editing}
        onClose={() => setEditing(null)}
        onSubmit={(input) => setQuota.mutate(input)}
        busy={setQuota.isPending}
      />
    </div>
  )
}

function Meter({
  used,
  quota,
  unlimited,
}: {
  used: number
  quota: number
  unlimited: boolean
}) {
  // 不限时不画条：画成 0% 会让人以为「没用」，而画成 100% 会让人以为「满了」。
  if (unlimited || quota <= 0) return null
  const pct = Math.min(100, Math.max(0, (used / quota) * 100))
  // 阈值留给用户反应时间比精确更重要：75% 就开始变黄，而不是等到 90%。
  const tone = pct >= 90 ? 'bg-danger' : pct >= 75 ? 'bg-warning' : 'bg-brand'
  return (
    <div className="mt-1 h-1.5 w-32 overflow-hidden rounded-pill bg-line">
      <div className={`h-full rounded-pill ${tone}`} style={{ width: `${pct}%` }} />
    </div>
  )
}

function EditQuotaModal({
  usage,
  onClose,
  onSubmit,
  busy,
}: {
  usage: QuotaUsage | null
  onClose: () => void
  onSubmit: (input: {
    user_id: number
    node_id: number
    enabled: boolean
    quota_bytes: number
    read_only: boolean
  }) => void
  busy: boolean
}) {
  // 以 GB 为单位输入：配额的粒度是 GB，让用户输入字节只会得到一个
  // 位数多到自己都数不清的数字。
  const [gb, setGB] = useState('')
  const [unlimited, setUnlimited] = useState(false)
  const [readOnly, setReadOnly] = useState(false)
  const [enabled, setEnabled] = useState(true)

  const [seen, setSeen] = useState<string | null>(null)
  const key = usage ? `${usage.user_id}-${usage.node_id}` : null
  if (key && key !== seen) {
    setSeen(key)
    setGB(usage!.quota_bytes > 0 ? String(Math.round(usage!.quota_bytes / GB)) : '')
    setUnlimited(usage!.unlimited)
    setReadOnly(usage!.read_only)
    setEnabled(!usage!.unlimited)
  }

  if (!usage) return null

  const submit = () => {
    if (unlimited) {
      onSubmit({
        user_id: usage.user_id, node_id: usage.node_id,
        enabled: false, quota_bytes: 0, read_only: readOnly,
      })
      return
    }
    const n = Number(gb)
    if (!Number.isFinite(n) || n <= 0) return
    onSubmit({
      user_id: usage.user_id, node_id: usage.node_id,
      enabled, quota_bytes: Math.round(n * GB), read_only: readOnly,
    })
  }

  return (
    <Modal
      open
      title={`设置用户 #${usage.user_id} 的配额`}
      description="配额决定的是「还能不能新增」，不是「已有的怎么办」——超限不会删掉任何东西。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={!unlimited && !(Number(gb) > 0)}
            loading={busy}
            onClick={submit}
          >
            保存
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <label className="flex cursor-pointer items-center gap-2.5">
          <input
            type="checkbox"
            checked={unlimited}
            onChange={(e) => {
              setUnlimited(e.target.checked)
              if (e.target.checked) setEnabled(false)
              else setEnabled(true)
            }}
          />
          <span className="text-base text-ink">
            不限额
            <span className="ml-2 text-sm text-ink-3">
              未设配额的用户不受限制——先能用，再谈限额
            </span>
          </span>
        </label>

        {!unlimited && (
          <Input
            label="限额（GB）"
            value={gb}
            onChange={(e) => setGB(e.target.value)}
            hint={`当前用量 ${formatBytes(usage.total_bytes)}`}
          />
        )}

        <label className="flex cursor-pointer items-start gap-2.5">
          <input
            type="checkbox"
            className="mt-1"
            checked={readOnly}
            onChange={(e) => setReadOnly(e.target.checked)}
          />
          <span>
            <span className="block text-base text-ink">转为只读</span>
            <span className="block text-sm text-ink-3">
              该用户在此节点上不能新建任何占空间的资源。已超限时常用它作为处置
              手段，而不是直接删资源。
            </span>
          </span>
        </label>
      </div>
    </Modal>
  )
}

function formatBytes(n: number): string {
  if (n <= 0) return '0'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let v = n
  let i = 0
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
