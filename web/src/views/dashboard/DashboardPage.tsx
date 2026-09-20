import { useQuery } from '@tanstack/react-query'
import { Link, useNavigate } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import {
  dashboardApi,
  type DashboardAllocation,
  type DashboardHost,
  type DashboardSummary,
} from '@/api/dashboard'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Meter } from '@/components/common/Meter'
import { StatusBadge } from '@/components/common/StatusBadge'
import { useSessionStore } from '@/stores/session'
import { cn } from '@/utils/cn'
import { formatBytes, relativeTime } from '@/utils/format'
import { VM_STATUS_LABEL, VM_STATUS_TONE } from '@/utils/labels'
import type { VmStatus } from '@/api/vm'

/**
 * 工作台（F-8-03 管理员 / F-8-04 租户）。
 *
 * 两种角色共用这一个页面，差别只在**数字的范围**：管理员看全景（节点、
 * 宿主机资源），租户只看自己的。页面结构保持一致是有意的——「租户版少了
 * 什么」若由两套页面各写一遍，漏掉归属过滤的那天不会有任何报错。
 */
export function DashboardPage() {
  const user = useSessionStore((s) => s.user)
  const navigate = useNavigate()

  const summary = useQuery({
    queryKey: ['dashboard'],
    queryFn: dashboardApi.summary,
    // 平时 30 秒一次：概览是「路过看一眼」的页面。有未落定的任务时快到
    // 5 秒——那时用户真正盯的是进度，慢一次他就得自己去任务中心翻。
    refetchInterval: (q) => ((q.state.data?.tasks.active ?? 0) > 0 ? 5000 : 30000),
  })

  return (
    <div className="flex flex-col gap-4">
      <header>
        <h1 className="text-lg font-semibold text-ink">工作台</h1>
        <p className="mt-1 text-base text-ink-3">
          当前登录：<span className="text-ink-2">{user?.username}</span>
          <span className="ml-2 rounded-pill border border-line-strong px-2 py-0.5 text-xs text-ink-2">
            {user?.role === 'admin' ? '管理员' : '租户'}
          </span>
        </p>
      </header>

      {summary.isPending && <PageLoading />}

      {summary.isError && (
        <div className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(summary.error)}
        </div>
      )}

      {summary.data && (
        <>
          <StatusBanner data={summary.data} />

          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
            <StatCard
              label={user?.role === 'admin' ? '虚拟机' : '我的虚拟机'}
              value={summary.data.vms.total}
              detail={`${summary.data.vms.running} 台运行中`}
              to="/vm"
            />
            {summary.data.nodes ? (
              <StatCard
                label="节点"
                value={summary.data.nodes.total}
                detail={`${summary.data.nodes.online} 个在线`}
                to="/node"
              />
            ) : (
              <StatCard
                label="已关机"
                value={summary.data.vms.stopped}
                detail={summary.data.vms.other > 0 ? `${summary.data.vms.other} 台非运行态` : '没有非运行态的机器'}
              />
            )}
            <StatCard
              label="进行中的任务"
              value={summary.data.tasks.active}
              detail="含结果未定的任务"
              to="/task"
            />
            <StatCard
              label="近 24 小时失败"
              value={summary.data.tasks.failed_24h}
              detail={summary.data.tasks.failed_24h > 0 ? '建议查看失败原因' : '最近一天没有失败'}
              tone={summary.data.tasks.failed_24h > 0 ? 'danger' : 'default'}
              to="/task"
            />
          </div>

          {user?.role === 'admin' && <HostCard host={summary.data.host} />}

          <CommitCard allocation={summary.data.allocation} host={summary.data.host} />

          <section className="rounded-card border border-line bg-surface">
            <div className="flex items-center justify-between gap-3 border-b border-line px-4 py-3">
              <h2 className="text-md font-medium text-ink">最近虚拟机</h2>
              <Link to="/vm" className="text-base text-brand hover:underline">
                全部虚拟机 →
              </Link>
            </div>

            {summary.data.recent_vms.length === 0 ? (
              <EmptyState
                title="还没有虚拟机"
                description="虚拟机是这里的主体。可以从模板克隆一台，或者先导入一个镜像做成模板。"
                action={
                  <Button size="sm" onClick={() => navigate('/vm')}>
                    前往虚拟机
                  </Button>
                }
              />
            ) : (
              <div className="overflow-x-auto">
                <table className="w-full border-collapse text-base">
                  <thead>
                    <tr className="bg-sunken text-left text-xs text-ink-2">
                      <th className="px-4 py-2.5 font-medium">名称</th>
                      <th className="px-4 py-2.5 font-medium">状态</th>
                      <th className="px-4 py-2.5 font-medium">规格</th>
                      <th className="px-4 py-2.5 font-medium">节点</th>
                      <th className="px-4 py-2.5 font-medium">创建于</th>
                    </tr>
                  </thead>
                  <tbody>
                    {summary.data.recent_vms.map((vm) => (
                      <tr key={vm.id} className="border-t border-line hover:bg-raised">
                        <td className="px-4 py-2.5">
                          <Link
                            to={`/vm/${vm.id}`}
                            className="font-medium text-ink hover:text-brand hover:underline"
                          >
                            {vm.name}
                          </Link>
                        </td>
                        <td className="px-4 py-2.5">
                          <StatusBadge tone={VM_STATUS_TONE[vm.status as VmStatus] ?? 'idle'}>
                            {VM_STATUS_LABEL[vm.status as VmStatus] ?? vm.status}
                          </StatusBadge>
                        </td>
                        <td className="kc-nums px-4 py-2.5 text-ink-2">
                          {vm.vcpu} 核 · {formatBytes(vm.memory_mb * 1024 * 1024)}
                        </td>
                        <td className="px-4 py-2.5 text-ink-2">{vm.node_name || '—'}</td>
                        <td className="px-4 py-2.5 text-ink-2">{relativeTime(vm.created_at)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      )}
    </div>
  )
}

/**
 * 状态横幅。
 *
 * 没有告警时它也要占住位置并说一句「正常」——一个只在出问题时出现的横幅，
 * 会让人不确定「没出现」是因为真的没事，还是因为这一屏还没加载完。
 */
function StatusBanner({ data }: { data: DashboardSummary }) {
  const alerts = data.alerts ?? []
  if (alerts.length === 0) {
    return (
      <div className="rounded-card border border-success/30 bg-success/10 px-4 py-3">
        <p className="text-md font-medium text-success">系统运行正常</p>
        <p className="mt-0.5 text-base text-ink-2">
          {data.vms.running} 台虚拟机运行中
          {data.nodes ? ` · ${data.nodes.online}/${data.nodes.total} 个节点在线` : ''}
        </p>
      </div>
    )
  }

  const danger = alerts.some((a) => a.level === 'danger')
  return (
    <div
      className={cn(
        'rounded-card border px-4 py-3',
        danger ? 'border-danger/30 bg-danger/10' : 'border-warning/30 bg-warning/10',
      )}
    >
      <p className={cn('text-md font-medium', danger ? 'text-danger' : 'text-warning')}>
        {danger ? '有需要立即处理的事项' : '有需要注意的事项'}
      </p>
      <ul className="mt-1 flex flex-col gap-0.5">
        {alerts.map((alert) => (
          <li key={alert.text}>
            <Link
              to={alert.link}
              className={cn(
                'text-base hover:underline',
                alert.level === 'danger' ? 'text-danger' : 'text-warning',
              )}
            >
              {alert.text} →
            </Link>
          </li>
        ))}
      </ul>
    </div>
  )
}

function StatCard({
  label,
  value,
  detail,
  to,
  tone = 'default',
}: {
  label: string
  value: number
  detail?: string
  to?: string
  tone?: 'default' | 'danger'
}) {
  const body = (
    <>
      <p className="text-base text-ink-3">{label}</p>
      <p
        className={cn(
          'kc-nums mt-1 text-xl font-semibold',
          tone === 'danger' && value > 0 ? 'text-danger' : 'text-ink',
        )}
      >
        {value}
      </p>
      {detail && <p className="mt-0.5 text-xs text-ink-3">{detail}</p>}
    </>
  )

  if (!to) {
    return <div className="rounded-card border border-line bg-surface p-4">{body}</div>
  }
  return (
    <Link
      to={to}
      className="block rounded-card border border-line bg-surface p-4 hover:border-brand/40"
    >
      {body}
    </Link>
  )
}

/**
 * 宿主机的实际用量。
 *
 * 三条都来自**最近一次采样**而不是实时探测：首页是打开最频繁的页面，让
 * 它去逐节点探测等于把探测频率交给用户的刷新行为。采样时间因此必须显示
 * 出来——一个没有时刻的百分比，用户无法判断它是刚才的还是一小时前的。
 */
function HostCard({ host }: { host?: DashboardHost | null }) {
  if (!host) {
    return (
      <section className="rounded-card border border-dashed border-line-strong px-4 py-4">
        <h2 className="text-md font-medium text-ink">宿主机资源</h2>
        <p className="mt-1 text-base text-ink-3">
          还没有采样数据。采集器每 60 秒写入一次，接入节点后稍等一分钟即可看到。
        </p>
      </section>
    )
  }

  const memPercent = host.mem_total_mb > 0 ? (host.mem_used_mb / host.mem_total_mb) * 100 : 0
  const diskPercent =
    host.disk_total_bytes > 0 ? (host.disk_used_bytes / host.disk_total_bytes) * 100 : 0

  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between gap-3">
        <h2 className="text-md font-medium text-ink">宿主机资源</h2>
        <p className="text-xs text-ink-3">
          {host.sampled_nodes} 个节点的采样 · {relativeTime(host.sampled_at)}
        </p>
      </div>
      <div className="mt-3 grid gap-4 sm:grid-cols-3">
        <Meter label="CPU" detail={`${host.cpu_cores} 核`} percent={host.cpu_percent} />
        <Meter
          label="内存"
          detail={`${formatBytes(host.mem_used_mb * 1024 * 1024)} / ${formatBytes(host.mem_total_mb * 1024 * 1024)}`}
          percent={memPercent}
        />
        <Meter
          label="存储池"
          detail={`${formatBytes(host.disk_used_bytes)} / ${formatBytes(host.disk_total_bytes)}`}
          percent={diskPercent}
        />
      </div>
    </section>
  )
}

/**
 * 已承诺给虚拟机的资源。
 *
 * 它与「宿主机资源」是两回事：**承诺**是不管用没用已经划出去的量，而
 * 宿主机那一栏是实际用了多少。分开呈现而不是叠成一条进度条，是因为两者
 * 量纲不同——叠在一起会出现「看起来 120%」这种既无法解释也无法行动的数字。
 */
function CommitCard({
  allocation,
  host,
}: {
  allocation: DashboardAllocation
  host?: DashboardHost | null
}) {
  const cpuPercent =
    host && host.cpu_cores > 0 ? (allocation.running_vcpu / host.cpu_cores) * 100 : null
  const memPercent =
    host && host.mem_total_mb > 0 ? (allocation.running_memory_mb / host.mem_total_mb) * 100 : null

  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <h2 className="text-md font-medium text-ink">已分配给虚拟机</h2>
      <div className="mt-3 grid gap-4 sm:grid-cols-3">
        <CommitItem
          label="vCPU"
          text={`${allocation.running_vcpu} / ${allocation.vcpu} 核`}
          note="运行中的 / 全部"
          percent={cpuPercent}
        />
        <CommitItem
          label="内存"
          text={`${formatBytes(allocation.running_memory_mb * 1024 * 1024)} / ${formatBytes(allocation.memory_mb * 1024 * 1024)}`}
          note="运行中的 / 全部"
          percent={memPercent}
        />
        <CommitItem
          label="磁盘"
          text={`${allocation.disk_gb} GB`}
          note="按配置容量，含已关机"
        />
      </div>
    </section>
  )
}

function CommitItem({
  label,
  text,
  note,
  percent,
}: {
  label: string
  text: string
  note: string
  /** 相对宿主机总量的占比；宿主数据不可用时为 null，此时不显示占比。 */
  percent?: number | null
}) {
  return (
    <div>
      <p className="text-base text-ink-3">{label}</p>
      <p className="kc-nums mt-0.5 text-md text-ink">{text}</p>
      <p className="mt-0.5 text-xs text-ink-3">
        {note}
        {percent !== null && percent !== undefined && ` · 占宿主 ${percent.toFixed(0)}%`}
      </p>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '加载失败，请稍后重试'
}
