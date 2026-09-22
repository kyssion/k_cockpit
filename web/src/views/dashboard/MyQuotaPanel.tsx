import { useQuery } from '@tanstack/react-query'

import { ApiError, NetworkError } from '@/api/client'
import { dashboardApi, type NodeQuota, type UsedLimit } from '@/api/dashboard'
import { formatBytes } from '@/api/monitor'
import { EmptyState, PageLoading } from '@/components/common/Feedback'

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '加载失败，请稍后重试'
}

/**
 * MyQuotaPanel 普通用户的配额视角（G-32）。
 *
 * 对应 QVMConsole 用户工作台的「资源总览 + 配额详情」：顶部 5 张合计卡
 * 回答「我还剩多少」，逐节点明细回答「在哪个节点上用的」。明细默认收起
 * ——多数时候用户只需要看合计，展开是排查时的动作。
 */

/** 跨节点合计。任一节点不限则该维度整体显示「不限」。 */
function totalOf(dimensions: UsedLimit[]): { used: number; limit: number | null; unit: string } {
  let used = 0
  let allLimited = true
  let limit = 0
  let unit = ''
  for (const d of dimensions) {
    used += d.used
    if (!d.has_limit) allLimited = false
    else limit += d.limit
    unit = d.unit ?? unit
  }
  return { used, limit: allLimited ? limit : null, unit }
}

/** 按 used/limit 给出进度条颜色。 */
function toneOf(d: UsedLimit): string {
  if (!d.has_limit || d.limit <= 0) return 'bg-brand'
  if (d.used >= d.limit) return 'bg-danger'
  if (d.used * 100 >= d.limit * 80) return 'bg-warning'
  return 'bg-brand'
}

/** 展示一个维度的「已用 / 上限」文本。 */
function usageText(d: UsedLimit): string {
  const used = fmt(d.used, d.unit ?? '')
  if (!d.has_limit) return `${used} / 不限`
  return `${used} / ${fmt(d.limit, d.unit ?? '')}`
}

function fmt(v: number, unit: string): string {
  if (unit === 'MB') return formatBytes(v * 1024 * 1024)
  return `${v}${unit ? ' ' + unit : ''}`
}

export function MyQuotaPanel() {
  const quotas = useQuery({
    queryKey: ['dashboard-quotas'],
    queryFn: dashboardApi.myQuotas,
    staleTime: 30_000,
  })

  if (quotas.isPending) return <PageLoading />
  if (quotas.isError) {
    return (
      <div className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
        {describe(quotas.error)}
      </div>
    )
  }

  const nodes = quotas.data.nodes
  if (nodes.length === 0) {
    return (
      <section className="rounded-card border border-line bg-surface">
        <EmptyState
          title="还没有分配给你的资源"
          description="管理员还没有为你的账号开通任何节点上的配额。开通后，这里会显示你的资源用量与剩余额度。"
        />
      </section>
    )
  }

  // 合计 5 卡：跨节点求和。计算与存储是存量（当前占多少），流量与运行
  // 时长是本月累计（UTC 月）——两类放在一起时用小标题区分口径。
  const sum = (key: keyof NodeQuota) =>
    totalOf(nodes.map((n) => n[key] as UsedLimit))
  const cards = [
    { label: 'vCPU', ...sum('vcpu') },
    { label: '内存', ...sum('memory_mb') },
    { label: '虚拟机', ...sum('vms') },
    { label: '存储', ...sum('storage_gb') },
    { label: '本月运行时长', ...sum('runtime_hours') },
  ]

  return (
    <section className="flex flex-col gap-3">
      <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-5">
        {cards.map((c) => (
          <div key={c.label} className="rounded-card border border-line bg-surface p-4">
            <p className="text-sm text-ink-3">{c.label}</p>
            <p className="mt-1 kc-nums text-xl font-semibold text-ink">{fmt(c.used, c.unit)}</p>
            <p className="mt-0.5 text-xs text-ink-3">
              {c.limit === null ? '配额：不限' : `配额：${fmt(c.limit, c.unit)}`}
            </p>
          </div>
        ))}
      </div>

      <details className="rounded-card border border-line bg-surface">
        <summary className="cursor-pointer px-4 py-3 text-md font-medium text-ink">
          按节点查看明细
        </summary>
        <div className="flex flex-col gap-4 border-t border-line px-4 py-3.5">
          {nodes.map((n) => (
            <div key={n.node_id} className="flex flex-col gap-2">
              <div className="flex items-center gap-2">
                <span className="text-base font-medium text-ink">{n.node_name || '未知节点'}</span>
                {n.worst_status === 'limited' && (
                  <span className="rounded-pill bg-danger/10 px-2 py-0.5 text-xs text-danger">
                    已超限
                  </span>
                )}
                {n.worst_status === 'warned' && (
                  <span className="rounded-pill bg-warning/10 px-2 py-0.5 text-xs text-warning">
                    接近上限
                  </span>
                )}
              </div>
              <QuotaRow label="vCPU" dim={n.vcpu} />
              <QuotaRow label="内存" dim={n.memory_mb} />
              <QuotaRow label="虚拟机" dim={n.vms} />
              <QuotaRow label="存储" dim={n.storage_gb} />
              <QuotaRow label="本月入站流量" dim={n.traffic_in_gb} />
              <QuotaRow label="本月出站流量" dim={n.traffic_out_gb} />
              <QuotaRow label="本月运行时长" dim={n.runtime_hours} />
            </div>
          ))}
        </div>
      </details>
    </section>
  )
}

/** QuotaRow 一行维度：标签 + 进度条 + 数字。 */
function QuotaRow({ label, dim }: { label: string; dim: UsedLimit }) {
  const percent =
    dim.has_limit && dim.limit > 0 ? Math.min(100, (dim.used / dim.limit) * 100) : 0
  return (
    <div className="grid grid-cols-[9rem_1fr_auto] items-center gap-3">
      <span className="text-sm text-ink-2">{label}</span>
      <div className="h-1.5 overflow-hidden rounded-pill bg-sunken">
        <div
          className={`h-full rounded-pill transition-[width] ${toneOf(dim)}`}
          style={{ width: `${percent}%` }}
        />
      </div>
      <span className="kc-nums text-sm text-ink-2">{usageText(dim)}</span>
    </div>
  )
}
