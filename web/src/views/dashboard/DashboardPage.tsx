import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useNavigate } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import {
  dashboardApi,
  type DashboardAllocation,
  type DashboardHost,
  type DashboardSummary,
  type HostHardwareView,
  type HostNetStatsView,
  type TuningStateView,
  type TuningView,
} from '@/api/dashboard'
import { monitorApi, RANGES, type RangeKey } from '@/api/monitor'
import { nodeApi } from '@/api/node'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Icon, type IconName } from '@/components/common/Icon'
import { Segmented } from '@/components/common/Segmented'
import { StatusBadge } from '@/components/common/StatusBadge'
import { TimeSeriesChart } from '@/components/common/TimeSeriesChart'
import { useSessionStore } from '@/stores/session'
import { MyQuotaPanel } from './MyQuotaPanel'
import { cn } from '@/utils/cn'
import { formatBytes, relativeTime } from '@/utils/format'
import { readRecentVisits } from '@/utils/recentVisits'
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
      {/* 首屏第一行回答「现在怎么样」而不是重复一遍菜单名：问候 + 角色
          + 一句状态。状态点与文字配合，颜色不单独承担含义。 */}
      <header className="flex flex-wrap items-end justify-between gap-3">
        <div className="min-w-0">
          <h1 className="text-xl font-semibold text-ink">
            {greeting()}，{user?.username}
          </h1>
          <p className="mt-1.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-base text-ink-3">
            <span className="flex items-center gap-1.5">
              <span
                aria-hidden="true"
                className={cn(
                  'h-1.5 w-1.5 rounded-full',
                  (summary.data?.alerts?.length ?? 0) > 0 ? 'bg-warning' : 'bg-success',
                )}
              />
              {(summary.data?.alerts?.length ?? 0) > 0 ? '有需要关注的事项' : '系统运行正常'}
            </span>
            {summary.data && (
              <>
                <span className="text-ink-3/50">·</span>
                <span>{summary.data.vms.running} 台虚拟机运行中</span>
                {summary.data.nodes && (
                  <>
                    <span className="text-ink-3/50">·</span>
                    <span>
                      {summary.data.nodes.online}/{summary.data.nodes.total} 个节点在线
                    </span>
                  </>
                )}
              </>
            )}
            <span className="rounded-pill border border-line-strong px-2 py-0.5 text-xs text-ink-2">
              {user?.role === 'admin' ? '管理员' : '租户'}
            </span>
          </p>
        </div>
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
              icon="vm"
              label={user?.role === 'admin' ? '虚拟机' : '我的虚拟机'}
              value={summary.data.vms.total}
              detail={`${summary.data.vms.running} 台运行中`}
              to="/vm"
            />
            {summary.data.nodes ? (
              <StatCard
                icon="node"
                label="节点"
                value={summary.data.nodes.total}
                detail={`${summary.data.nodes.online} 个在线`}
                to="/node"
              />
            ) : (
              <StatCard
                icon="vm"
                label="已关机"
                value={summary.data.vms.stopped}
                detail={summary.data.vms.other > 0 ? `${summary.data.vms.other} 台非运行态` : '没有非运行态的机器'}
              />
            )}
            <StatCard
              icon="task"
              label="进行中的任务"
              value={summary.data.tasks.active}
              detail="含结果未定的任务"
              to="/task"
            />
            <StatCard
              icon="alert"
              label="近 24 小时失败"
              value={summary.data.tasks.failed_24h}
              detail={summary.data.tasks.failed_24h > 0 ? '建议查看失败原因' : '最近一天没有失败'}
              tone={summary.data.tasks.failed_24h > 0 ? 'danger' : 'default'}
              to="/task"
            />
          </div>

          {user?.role === 'admin' && (
            <HostCard host={summary.data.host} allocation={summary.data.allocation} />
          )}

          {user?.role === 'admin' && <ResourceTrendCard />}

          {/* G-32：普通用户的配额视角。管理员走平台视图（上方的宿主机资源
              卡），配额是「我的额度还剩多少」的问题，两类卡片并存会让
              页面出现两套「用量」口径。 */}
          {user?.role !== 'admin' && <MyQuotaPanel />}

          <CommitCard allocation={summary.data.allocation} host={summary.data.host} />

          {user?.role === 'admin' && <HostDetailSection />}

          <RecentVisits />

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
  // 正常时不再单独占一条横幅：状态已经在上方的问候行里用「一个点 + 一句话」
  // 表达了，再来一条绿色大条只是重复占用首屏最贵的位置。
  if (alerts.length === 0) return null

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

/**
 * StatCard 一个统计数。
 *
 * 图标不是装饰：四张卡的数字长得一样，扫视时先认出「这是哪一类」再读数字，
 * 比每次都得看一遍标签快。图标底色按语义分色，但**只有失败那类在数字上
 * 用颜色**——其余数字保持中性色，否则整屏都在喊。
 */
function StatCard({
  icon,
  label,
  value,
  unit,
  detail,
  to,
  tone = 'default',
}: {
  icon: IconName
  label: string
  value: number
  unit?: string
  detail?: string
  to?: string
  tone?: 'default' | 'danger'
}) {
  const alert = tone === 'danger' && value > 0
  const body = (
    <>
      <div className="flex items-start justify-between gap-2">
        <p className="text-base text-ink-3">{label}</p>
        <span
          aria-hidden="true"
          className={cn(
            'flex h-7 w-7 shrink-0 items-center justify-center rounded-control',
            alert
              ? 'bg-danger/12 text-danger'
              : to
                ? 'bg-brand/10 text-brand'
                : 'bg-sunken text-ink-2',
          )}
        >
          <Icon name={icon} className="h-3.5 w-3.5" />
        </span>
      </div>
      <p className={cn('kc-nums mt-2 text-2xl font-semibold', alert ? 'text-danger' : 'text-ink')}>
        {value}
        {unit && <span className="ml-0.5 text-md font-normal text-ink-3">{unit}</span>}
      </p>
      {detail && <p className="mt-1 text-xs text-ink-3">{detail}</p>}
    </>
  )

  if (!to) {
    return <div className="rounded-card border border-line bg-surface p-4">{body}</div>
  }
  return (
    <Link
      to={to}
      className="block rounded-card border border-line bg-surface p-4 transition-colors hover:border-brand/40"
    >
      {body}
    </Link>
  )
}

/**
 * ResourceTrendCard 宿主机资源走势。
 *
 * 与「宿主机资源」是同一份数据的两种尺度：那边回答「此刻多少」，这边回答
 * 「这段时间是什么形状」——「现在 90%」和「一路涨到 90%」要采取的行动不同。
 * 数据来自采集器写入的指标明细（不是实时探测），因此拿不到时如实说明，
 * 不留一个空白框让人以为图表没加载出来。
 */
function ResourceTrendCard() {
  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const [nodeID, setNodeID] = useState(0)
  const [range, setRange] = useState<RangeKey>('1h')

  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0
  const series = useQuery({
    queryKey: ['dashboard-host-series', effectiveNodeID, range],
    queryFn: () => monitorApi.host(effectiveNodeID, range),
    enabled: effectiveNodeID > 0,
    // 采集间隔是一分钟，跟得再紧也只是重复读同一批点。
    refetchInterval: 60000,
  })

  if (nodes.isPending) return null
  const nodeList = nodes.data ?? []
  if (nodeList.length === 0) return null

  const points = series.data?.points ?? []
  const cpu = points.map((p) => ({ at: p.at, value: p.cpu_percent }))
  const mem = points.map((p) => ({
    at: p.at,
    value: p.mem_total_mb > 0 ? (p.mem_used_mb / p.mem_total_mb) * 100 : 0,
  }))
  const percent = (v: number) => `${v.toFixed(0)}%`
  const latest = (list: { value: number }[]): number | null => {
    const last = list[list.length - 1]
    return last ? last.value : null
  }

  const chart = (title: string, list: { at: string; value: number }[], tone: 'primary' | 'success') => (
    <div>
      <div className="mb-2 flex items-baseline justify-between gap-2">
        <p className="text-sm text-ink-2">{title}</p>
        {latest(list) !== null && (
          <p className="kc-nums text-sm text-ink-3">当前 {percent(latest(list) as number)}</p>
        )}
      </div>
      <TimeSeriesChart
        points={list}
        intervalSeconds={series.data?.interval_seconds}
        format={percent}
        tone={tone}
        emptyHint="这段时间没有采集到数据"
      />
    </div>
  )

  return (
    <section className="rounded-card border border-line bg-surface">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3">
        <div>
          <h2 className="text-md font-medium text-ink">资源走势</h2>
          <p className="mt-0.5 text-xs text-ink-3">
            采集器每分钟写入一次；与上方「宿主机资源」是同一份数据的不同尺度。
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {nodeList.length > 1 && (
            <select
              value={effectiveNodeID}
              onChange={(e) => setNodeID(Number(e.target.value))}
              aria-label="节点"
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-sm text-ink"
            >
              {nodeList.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </select>
          )}
          <Segmented
            value={range}
            onChange={setRange}
            options={RANGES.map((r) => ({ value: r.key, label: r.label.replace('近 ', '') }))}
          />
        </div>
      </div>

      <div className="grid gap-4 px-4 py-4 lg:grid-cols-2">
        {chart('CPU 使用率', cpu, 'primary')}
        {chart('内存使用率', mem, 'success')}
      </div>
    </section>
  )
}

/** 按本地时间给一句问候。 */
function greeting(): string {
  const h = new Date().getHours()
  if (h < 6) return '凌晨好'
  if (h < 12) return '上午好'
  if (h < 18) return '下午好'
  return '晚上好'
}

/**
 * 宿主机的实际用量。
 *
 * 三条都来自**最近一次采样**而不是实时探测：首页是打开最频繁的页面，让
 * 它去逐节点探测等于把探测频率交给用户的刷新行为。采样时间因此必须显示
 * 出来——一个没有时刻的百分比，用户无法判断它是刚才的还是一小时前的。
 */
/**
 * HostCard 宿主机资源。
 *
 * 每条都是**双进度条**：上面是此刻实际用了多少，下面是"理论最大"——
 * 即运行中的虚拟机全部跑满时最多会占多少。
 *
 * 两条必须同时给：只看实际用量，用户不知道还剩多少余量能再开一台；
 * 只看理论最大，他又会以为那些余量已经被用掉了。两者叠在一条上会出现
 * 无法解释的百分比，因此各占一条。
 */
function HostCard({
  host,
  allocation,
}: {
  host?: DashboardHost | null
  allocation: DashboardAllocation
}) {
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
        <DualMeter
          label="CPU"
          detail={`${host.cpu_cores} 核`}
          current={host.cpu_percent}
          ceiling={host.cpu_cores > 0 ? (allocation.running_vcpu / host.cpu_cores) * 100 : null}
        />
        <DualMeter
          label="内存"
          detail={`${formatBytes(host.mem_used_mb * 1024 * 1024)} / ${formatBytes(host.mem_total_mb * 1024 * 1024)}`}
          current={memPercent}
          ceiling={
            host.mem_total_mb > 0 ? (allocation.running_memory_mb / host.mem_total_mb) * 100 : null
          }
        />
        <DualMeter
          label="存储池"
          detail={`${formatBytes(host.disk_used_bytes)} / ${formatBytes(host.disk_total_bytes)}`}
          current={diskPercent}
          ceiling={
            host.disk_total_bytes > 0
              ? ((allocation.disk_gb * 1024 * 1024 * 1024) / host.disk_total_bytes) * 100
              : null
          }
        />
      </div>
      <p className="mt-3 text-xs text-ink-3">
        每条的上栏是此刻的实际占用，下栏是<strong className="font-normal text-ink-2">理论最大</strong>
        ——即运行中的虚拟机同时满载时的占用。
      </p>
    </section>
  )
}

/** 单条细进度条。放在双进度条里用，因此不带标签。 */
function Bar({ percent, tone }: { percent: number; tone: 'current' | 'ceiling' }) {
  const width = Math.max(0, Math.min(100, percent))
  return (
    <div className="h-1.5 w-full overflow-hidden rounded-pill bg-sunken">
      <div
        className={cn('h-full rounded-pill', tone === 'current' ? 'bg-brand' : 'bg-ink-3/50')}
        style={{ width: `${width}%` }}
      />
    </div>
  )
}

function DualMeter({
  label,
  detail,
  current,
  ceiling,
}: {
  label: string
  detail: string
  current: number
  /** 理论最大占比；宿主总量未知时为 null，此时只画实际那条。 */
  ceiling: number | null
}) {
  return (
    <div>
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-base text-ink-3">{label}</span>
        <span className="kc-nums text-xs text-ink-2">{detail}</span>
      </div>
      <div className="mt-1.5 flex flex-col gap-1">
        <Bar percent={current} tone="current" />
        {ceiling !== null && <Bar percent={ceiling} tone="ceiling" />}
      </div>
      <p className="kc-nums mt-1 text-xs text-ink-3">
        实际 {current.toFixed(0)}%
        {ceiling !== null && ` · 理论最大 ${ceiling.toFixed(0)}%`}
      </p>
    </div>
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

/**
 * HostDetailSection 宿主机细节：调优（KSM / zRAM）、硬件构成、网络统计。
 *
 * **按节点**加载而不是随概览一起返回：这三块都要向节点发请求，放进概览
 * 会让首页在节点多时变慢，且某个节点不支持探测时会把整页拖成错误。
 *
 * 节点下拉是必须的：多节点部署里，"内存插了几条""网桥转发了多少"没有
 * 主语就没有任何意义。
 */
function HostDetailSection() {
  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const [nodeID, setNodeID] = useState(0)

  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0
  const detail = useQuery({
    queryKey: ['dashboard-host-detail', effectiveNodeID],
    queryFn: () => dashboardApi.hostDetail(effectiveNodeID),
    enabled: effectiveNodeID > 0,
    // 这些是"查看一次"的信息，不跟着概览刷新。
    refetchInterval: 30000,
  })

  if (nodes.isPending) return null

  return (
    <section className="rounded-card border border-line bg-surface">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-3">
        <h2 className="text-md font-medium text-ink">宿主机细节</h2>
        <div className="flex items-center gap-2">
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
          <Link to="/host-tuning" className="text-base text-brand hover:underline">
            调优设置 →
          </Link>
        </div>
      </div>

      {detail.isError ? (
        <p className="px-4 py-3 text-base text-danger">{describe(detail.error)}</p>
      ) : (
        <div className="grid gap-4 px-4 py-3.5 lg:grid-cols-3">
          <TuningBlock tuning={detail.data?.tuning} />
          <HardwareBlock hardware={detail.data?.hardware} />
          <NetStatsBlock stats={detail.data?.netstats} />
        </div>
      )}
    </section>
  )
}

/** KSM / zRAM。数据来自调优服务，因此与调优页是同一份口径。 */
function TuningBlock({ tuning }: { tuning?: TuningView }) {
  return (
    <div>
      <h3 className="text-base text-ink">KSM / zRAM</h3>
      {!tuning ? (
        <p className="mt-1 text-sm text-ink-3">暂无数据</p>
      ) : (
        <dl className="mt-1.5 flex flex-col gap-1 text-sm">
          <TuningRow label="KSM 去重" state={tuning.ksm} />
          <TuningRow label="zRAM 压缩" state={tuning.zram} />
        </dl>
      )}
    </div>
  )
}

function TuningRow({ label, state }: { label: string; state?: TuningStateView }) {
  if (!state) return null
  return (
    <div className="flex items-baseline justify-between gap-2">
      <dt className="text-ink-2">{label}</dt>
      <dd className="kc-nums text-ink-3">
        <StatusBadge tone={state.enabled ? 'success' : 'idle'}>
          {state.enabled ? '已启用' : '未启用'}
        </StatusBadge>
        {state.enabled && state.saved_bytes ? (
          <span className="ml-1.5">省 {formatBytes(state.saved_bytes)}</span>
        ) : null}
      </dd>
    </div>
  )
}

/**
 * 硬件构成：CPU 拓扑与每核占用、内存插槽。
 *
 * "探测不到"要如实说：一个空列表会被读成"这台机器没有内存条"，而实际
 * 只是节点还没实现这个能力。
 */
function HardwareBlock({ hardware }: { hardware?: HostHardwareView }) {
  if (!hardware) return <p className="text-sm text-ink-3">硬件信息加载中…</p>
  if (hardware.unavailable) {
    return (
      <div>
        <h3 className="text-base text-ink">硬件</h3>
        <p className="mt-1 text-sm text-ink-3">{hardware.unavailable}</p>
      </div>
    )
  }

  const cores = hardware.core_percent ?? []
  return (
    <div>
      <h3 className="text-base text-ink">硬件</h3>
      <p className="mt-1 text-sm text-ink-2">
        {hardware.cpu_model || 'CPU'} · {hardware.sockets} 路 × {hardware.cores_per_socket} 核 ×{' '}
        {hardware.threads_per_core} 线程
      </p>

      {/* 每核一个色块：一眼看出是不是某几个核被打满（那是绑核或中断集中
          的典型表现，只给一个平均值看不出来）。 */}
      {cores.length > 0 && (
        <div className="mt-2 flex flex-wrap gap-0.5">
          {cores.map((p, i) => (
            <span
              key={i}
              title={`核心 ${i + 1}：${p.toFixed(1)}%`}
              className={cn(
                'h-3 w-3 rounded-[2px]',
                p >= 80 ? 'bg-danger' : p >= 50 ? 'bg-warning' : p >= 20 ? 'bg-brand/70' : 'bg-sunken',
              )}
            />
          ))}
        </div>
      )}

      <ul className="mt-2 flex flex-col gap-0.5 text-sm">
        {hardware.mem_slots.map((s) => (
          <li key={s.index} className="flex items-baseline justify-between gap-2">
            <span className="text-ink-2">插槽 {s.index}</span>
            <span className={cn('kc-nums', s.populated ? 'text-ink-3' : 'text-ink-3/60')}>
              {s.populated ? `${formatBytes(s.size_mb * 1024 * 1024)}${s.label ? ` · ${s.label}` : ''}` : '空闲'}
            </span>
          </li>
        ))}
      </ul>
    </div>
  )
}

/** 网络统计：规则条数与交换机/网桥计数。 */
function NetStatsBlock({ stats }: { stats?: HostNetStatsView }) {
  if (!stats) return <p className="text-sm text-ink-3">网络统计加载中…</p>
  if (stats.unavailable) {
    return (
      <div>
        <h3 className="text-base text-ink">网络统计</h3>
        <p className="mt-1 text-sm text-ink-3">{stats.unavailable}</p>
      </div>
    )
  }

  return (
    <div>
      <h3 className="text-base text-ink">网络统计</h3>
      <dl className="mt-1.5 flex flex-col gap-1 text-sm">
        <Row label="NAT 网关规则" value={`${stats.nat_rules} 条`} />
        <Row label="iptables DNAT" value={`${stats.dnat_rules} 条`} />
        <Row label="交换机入口" value={formatBytes(stats.switch_ingress_bytes)} />
        <Row label="交换机出口" value={formatBytes(stats.switch_egress_bytes)} />
      </dl>
      {stats.bridges.length > 0 && (
        <ul className="mt-2 flex flex-col gap-0.5 text-xs">
          {stats.bridges.map((b) => (
            <li key={b.name} className="flex items-baseline justify-between gap-2">
              <span className="text-ink-2">{b.name}</span>
              <span className="kc-nums text-ink-3">
                ↓{formatBytes(b.rx_bytes)} ↑{formatBytes(b.tx_bytes)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-2">
      <dt className="text-ink-2">{label}</dt>
      <dd className="kc-nums text-ink-3">{value}</dd>
    </div>
  )
}

/**
 * RecentVisits 最近访问。
 *
 * 用 localStorage 而不是后端：它只是"我刚才看了哪几台机器"这一层便利，
 * 为它建一张表并让每次打开详情页都写一次库并不划算。
 */
function RecentVisits() {
  const visits = readRecentVisits()
  if (visits.length === 0) return null

  return (
    <section className="rounded-card border border-line bg-surface">
      <div className="border-b border-line px-4 py-2.5">
        <h2 className="text-base font-medium text-ink">最近访问</h2>
      </div>
      <ul className="flex flex-wrap gap-2 px-4 py-3">
        {visits.map((v) => (
          <li key={v.path}>
            <Link
              to={v.path}
              className="rounded-pill border border-line px-3 py-1 text-sm text-ink-2 hover:border-brand/40 hover:text-ink"
            >
              {v.title}
            </Link>
          </li>
        ))}
      </ul>
    </section>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '加载失败，请稍后重试'
}
