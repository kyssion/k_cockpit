/**
 * MyStoragePage 展示当前用户的存储用量与配额（F-9-02）。
 *
 * 两条与其它页面一致的处理：
 *
 * 1. **用量按来源分项显示**。用户看到「超出配额 200 GB」时的第一个问题是
 *    「是什么占了这么多」——只给一个总数等于让他自己去翻。
 * 2. **不限额与「还有很多」分开**。前者的剩余量永远不变，后者会随用量变化，
 *    显示成同一个数字会让人以为额度是无限的（或反之）。
 */
import { useQuery } from '@tanstack/react-query'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { quotaApi, type QuotaUsage } from '@/api/quota'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Meter } from '@/components/common/Meter'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatBytes } from '@/utils/format'

export function MyStoragePage() {
  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const quotas = useQuery({ queryKey: ['my-quota'], queryFn: () => quotaApi.mine() })

  if (quotas.isPending) return <PageLoading />

  const items = quotas.data?.items ?? []
  const nodeName = (id: number) => (nodes.data ?? []).find((n) => n.id === id)?.name ?? `#${id}`

  return (
    <div className="flex max-w-[880px] flex-col gap-5">
      <header>
        <h1 className="text-lg font-semibold text-ink">我的存储</h1>
        <p className="mt-1 text-base text-ink-3">
          配额按**节点**分别计算——磁盘就在那台宿主机上，一个节点上的用量与
          另一个节点无关。虚拟机磁盘、模板与导出产物都计入。
        </p>
      </header>

      {quotas.isError ? (
        <p className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(quotas.error)}
        </p>
      ) : items.length === 0 ? (
        <EmptyState
          title="还没有用量记录"
          description="创建虚拟机或导出产物后，这里会显示各节点上的占用。"
        />
      ) : (
        <div className="flex flex-col gap-4">
          {items.map((u) => (
            <QuotaCard key={u.node_id} usage={u} nodeName={nodeName(u.node_id)} />
          ))}
        </div>
      )}
    </div>
  )
}

function QuotaCard({ usage, nodeName }: { usage: QuotaUsage; nodeName: string }) {
  const percent =
    usage.unlimited || usage.quota_bytes === 0
      ? 0
      : (usage.total_bytes / usage.quota_bytes) * 100

  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between gap-3">
        <h2 className="text-base font-medium text-ink">{nodeName}</h2>
        <span className="flex items-center gap-2">
          {usage.read_only && <StatusBadge tone="warning">只读</StatusBadge>}
          {usage.over_quota && <StatusBadge tone="danger">已超额</StatusBadge>}
          {usage.unlimited && <StatusBadge tone="idle">不限额度</StatusBadge>}
        </span>
      </div>

      <div className="mt-3">
        {usage.unlimited ? (
          // 不限额度时不给计量条：一条永远空着的条会让人以为「额度还很
          // 充足」，而实际是没有额度这回事。
          <p className="kc-nums text-base text-ink">
            已用 <span className="font-medium">{formatBytes(usage.total_bytes)}</span>
            <span className="ml-1.5 text-sm text-ink-3">未设上限</span>
          </p>
        ) : (
          <Meter
            label="已用"
            detail={`${formatBytes(usage.total_bytes)} / ${formatBytes(usage.quota_bytes)}`}
            percent={percent}
          />
        )}
      </div>

      {/* 分项：用户看到超额时的第一个问题是「什么占了这么多」。 */}
      <dl className="mt-3 grid grid-cols-3 gap-x-4 gap-y-1.5 text-base">
        <Item label="虚拟机磁盘" bytes={usage.vm_disks_bytes} />
        <Item label="模板" bytes={usage.templates_bytes} />
        <Item label="导出产物" bytes={usage.exports_bytes} />
      </dl>

      {!usage.unlimited && (
        <p className="mt-3 text-sm text-ink-3">
          剩余 {formatBytes(Math.max(0, usage.remaining_bytes))}。
          {usage.over_quota &&
            '已超出额度：仍可以查看与删除，但新建虚拟机、导出与制备模板都会被拒绝。'}
        </p>
      )}
    </section>
  )
}

function Item({ label, bytes }: { label: string; bytes: number }) {
  return (
    <div className="flex items-baseline justify-between gap-2">
      <dt className="text-ink-3">{label}</dt>
      <dd className="kc-nums text-ink">{formatBytes(bytes)}</dd>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
