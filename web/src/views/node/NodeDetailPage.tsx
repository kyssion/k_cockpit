/**
 * 节点详情页（F-6-01 ~ F-6-05）。
 *
 * 页签结构与虚拟机详情页保持一致（概览 / 存储 / 网络 / 虚拟机），并且同样
 * **惰性挂载**：磁盘探测要唤醒 agent 去跑一遍 `lsblk`，四个页签一次性拉全
 * 会让打开页面变成一个「顺便把节点问一遍」的动作。
 *
 * 维护模式的开关放在页头而不是藏进概览页签：它是一个**改变整台节点行为**
 * 的状态，页面顶部必须能一眼看到当前处于哪种模式。藏在第二个页签里会让人
 * 在别处操作失败后，回头才发现原来是维护中。
 */
import { useMutation, useQuery, useQueryClient, type UseQueryResult } from '@tanstack/react-query'
import { HostMetricsPanel } from '@/views/monitor/MetricsPanel'
import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { networkApi, CAPABILITY_STATE_LABEL, CAPABILITY_STATE_TONE } from '@/api/network'
import { nodeApi, type NodeView } from '@/api/node'
import { storageApi, POOL_STATUS_LABEL, POOL_STATUS_TONE } from '@/api/storage'
import { vmApi } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { Meter } from '@/components/common/Meter'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatBytes, formatDateTime, relativeTime } from '@/utils/format'
import { ENROLL_STATE_LABEL, NODE_STATUS_LABEL, NODE_STATUS_TONE } from '@/utils/labels'

type TabKey = 'overview' | 'storage' | 'network' | 'vms' | 'monitor'

const TABS: { key: TabKey; label: string }[] = [
  { key: 'overview', label: '概览' },
  { key: 'storage', label: '存储' },
  { key: 'network', label: '网络' },
  { key: 'vms', label: '虚拟机' },
  { key: 'monitor', label: '监控' },
]

export function NodeDetailPage() {
  const { id } = useParams()
  const nodeID = Number(id)
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  const [tab, setTab] = useState<TabKey>('overview')
  const [maintenanceOpen, setMaintenanceOpen] = useState(false)
  const [reason, setReason] = useState('')
  const [error, setError] = useState('')

  const detail = useQuery({
    queryKey: ['node', nodeID],
    queryFn: () => nodeApi.get(nodeID),
    enabled: Number.isFinite(nodeID),
  })

  const setMaintenance = useMutation({
    mutationFn: (enabled: boolean) => nodeApi.setMaintenance(nodeID, enabled, reason),
    onSuccess: () => {
      setMaintenanceOpen(false)
      setReason('')
      setError('')
      // 节点列表上也显示维护标记，因此两处都要刷新。
      void queryClient.invalidateQueries({ queryKey: ['node', nodeID] })
      void queryClient.invalidateQueries({ queryKey: ['nodes'] })
    },
    onError: (err) => {
      setMaintenanceOpen(false)
      setError(describe(err))
    },
  })

  if (detail.isPending) return <PageLoading />
  if (detail.isError) {
    return (
      <div className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
        {describe(detail.error)}
      </div>
    )
  }

  const node = detail.data

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-col gap-2">
        <Link to="/node" className="w-fit text-sm text-ink-3 hover:text-brand">
          ← 返回节点列表
        </Link>

        <div className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-lg font-semibold text-ink">{node.name}</h1>
            <div className="mt-1.5 flex flex-wrap items-center gap-2">
              <StatusBadge tone={NODE_STATUS_TONE[node.status]}>
                {NODE_STATUS_LABEL[node.status]}
              </StatusBadge>
              <StatusBadge tone={node.enroll_state === 'enrolled' ? 'info' : 'warning'}>
                {ENROLL_STATE_LABEL[node.enroll_state]}
              </StatusBadge>
              {node.maintenance_mode && <StatusBadge tone="warning">维护中</StatusBadge>}
              <span className="text-xs text-ink-3">
                最近心跳：{relativeTime(node.last_heartbeat_at)}
              </span>
            </div>
          </div>

          <div className="flex flex-wrap justify-end gap-2">
            {node.maintenance_mode ? (
              <Button
                variant="secondary"
                size="sm"
                loading={setMaintenance.isPending}
                onClick={() => setMaintenance.mutate(false)}
              >
                退出维护
              </Button>
            ) : (
              <Button variant="secondary" size="sm" onClick={() => setMaintenanceOpen(true)}>
                进入维护
              </Button>
            )}
            <Button
              variant="danger"
              size="sm"
              onClick={() => navigate('/node')}
              title="移除节点请回到列表页操作"
            >
              移除节点
            </Button>
          </div>
        </div>
      </div>

      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {/* 维护模式的提示条。它说明的是「现在这台节点上什么做不了」，
          而不是「节点有问题」——因此用 warning 而不是 danger。 */}
      {node.maintenance_mode && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base font-medium text-warning">该节点处于维护模式</p>
          <p className="mt-1 text-base text-ink-2">
            此节点上虚拟机的创建与电源操作已被拒绝（现有虚拟机继续运行，不受影响）。
            {node.maintenance_reason ? (
              <>
                维护原因：<span className="text-ink">{node.maintenance_reason}</span>
              </>
            ) : (
              <span className="text-ink-3">（未填写原因）</span>
            )}
            {node.maintenance_at && (
              <span className="text-ink-3"> · 自 {formatDateTime(node.maintenance_at)}</span>
            )}
          </p>
          {/* 维护中却仍标记为可迁移目标，是一条值得单说的不一致：迁移会把
              新虚拟机放到它上面，而那正是「引入变更」——与维护模式的定义冲突。 */}
          {node.is_migration_target && (
            <p className="mt-1 text-sm text-warning">
              该节点仍被标记为可迁移目标。维护期间不建议继续承接迁移。
            </p>
          )}
        </div>
      )}

      <TabBar value={tab} onChange={setTab} />

      {tab === 'overview' && <OverviewTab node={node} />}
      {tab === 'storage' && <StorageTab nodeID={node.id} />}
      {tab === 'network' && <NetworkTab nodeID={node.id} />}
      {tab === 'vms' && <VmsTab nodeID={node.id} />}
      {tab === 'monitor' && (
        <HostMetricsPanel
          nodeID={node.id}
          description="宿主机的整体负载。它是这台机器上所有虚拟机的合计——单个虚拟机占用偏高时，这里能看出还有多少余量。"
        />
      )}

      <Modal
        open={maintenanceOpen}
        title={`让「${node.name}」进入维护模式`}
        description="进入后，该节点上虚拟机的创建与电源操作会被拒绝。已运行的虚拟机不受影响——维护模式的意义是「不再引入变更」，而不是「停止业务」。"
        onClose={() => setMaintenanceOpen(false)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setMaintenanceOpen(false)}>
              取消
            </Button>
            <Button size="sm" loading={setMaintenance.isPending} onClick={() => setMaintenance.mutate(true)}>
              进入维护
            </Button>
          </>
        }
      >
        <Input
          label="维护原因（可选）"
          value={reason}
          maxLength={255}
          placeholder="例如：升级内核，预计 30 分钟"
          onChange={(e) => setReason(e.target.value)}
          hint="会写入该节点的备注，让其他人知道为什么处于维护中。退出维护时不会自动清除备注。"
        />
      </Modal>
    </div>
  )
}

function OverviewTab({ node }: { node: NodeView }) {
  // 指标只在**已接入**的节点上取：未接入的节点连 agent 都没有，
  // 请求它只会得到一个必然失败的往返。
  const stats = useQuery({
    queryKey: ['node-stats', node.id],
    queryFn: () => nodeApi.stats(node.id),
    enabled: node.enroll_state === 'enrolled' && node.status !== 'offline',
    refetchInterval: node.status === 'online' ? 5000 : false,
  })

  return (
    <div className="flex flex-col gap-4">
      <NodeMetrics node={node} stats={stats} />

      <div className="grid gap-4 lg:grid-cols-2">
      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="text-sm text-ink-3">基本信息</h2>
        <dl className="mt-2.5 flex flex-col gap-2">
          <Row label="节点 ID">#{node.id}</Row>
          <Row label="Agent 版本">{node.agent_version || '—'}</Row>
          <Row label="协议版本">
            {node.protocol_version > 0 ? `v${node.protocol_version}` : '—'}
          </Row>
          <Row label="最近心跳">
            {node.last_heartbeat_at ? formatDateTime(node.last_heartbeat_at) : '从未上报'}
          </Row>
          <Row label="可迁移目标">{node.is_migration_target ? '是' : '否'}</Row>
          <Row label="接入时间">{formatDateTime(node.created_at)}</Row>
          <Row label="备注">{node.remark || '—'}</Row>
        </dl>
      </section>

      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="text-sm text-ink-3">能力自报</h2>
        {/* 能力列表为空与「没有能力」是两回事：前者是节点还没上报过，
            后者是上报了但一个都没有。混在一起会让用户去排查一个其实
            只是「还没接入」的节点。 */}
        {node.capabilities && node.capabilities.length > 0 ? (
          <ul className="mt-2.5 flex flex-wrap gap-1.5">
            {node.capabilities.map((c) => (
              <li
                key={c}
                className="rounded-pill bg-raised px-2 py-0.5 text-xs text-ink-2"
              >
                {c}
              </li>
            ))}
          </ul>
        ) : (
          <p className="mt-2.5 text-sm text-ink-3">
            {node.enroll_state === 'enrolled'
              ? '节点尚未上报任何能力。'
              : '节点还未接入，接入后由 agent 自报能力。'}
          </p>
        )}

        {node.last_error && (
          <div className="mt-3 rounded-control border border-danger/30 bg-danger/10 px-3 py-2">
            <p className="text-xs text-ink-3">最近错误</p>
            <p className="mt-0.5 text-base text-danger">{node.last_error}</p>
          </div>
        )}
      </section>
      </div>
    </div>
  )
}

/**
 * NodeMetrics 渲染宿主机指标（F-6-03）。
 *
 * 与虚拟机资源卡的处理方式一致：采集失败时显示「采集失败」而不是一排 0——
 * 0% 的 CPU 看起来是「机器很闲」，而实际是「不知道」，两者对排查的意义
 * 完全相反。
 */
function NodeMetrics({
  node,
  stats,
}: {
  node: NodeView
  stats: UseQueryResult<import('@/api/node').NodeStats>
}) {
  if (node.enroll_state !== 'enrolled') {
    return (
      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="text-sm text-ink-3">运行指标</h2>
        <p className="mt-2.5 text-sm text-ink-3">节点尚未接入，接入后由 agent 上报指标。</p>
      </section>
    )
  }

  const data = stats.data
  const memPercent = data && data.mem_total_mb > 0 ? (data.mem_used_mb / data.mem_total_mb) * 100 : 0
  const diskPercent =
    data && data.disk_total_bytes > 0 ? (data.disk_used_bytes / data.disk_total_bytes) * 100 : 0

  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between gap-2">
        <h2 className="text-sm text-ink-3">运行指标</h2>
        {data && <span className="text-xs text-ink-3">{relativeTime(data.at)}</span>}
      </div>

      {stats.isPending ? (
        <p className="mt-2.5 text-sm text-ink-3">读取中…</p>
      ) : stats.isError ? (
        <p className="mt-2.5 text-sm text-warning">指标采集失败：{describe(stats.error)}</p>
      ) : data ? (
        <div className="mt-2.5 grid gap-4 lg:grid-cols-2">
          <div className="flex flex-col gap-3">
            <Meter
              label="CPU"
              detail={`${data.cpu_cores} 核 · 负载 ${data.load_avg_1.toFixed(2)}`}
              percent={data.cpu_percent}
            />
            <Meter
              label="内存"
              detail={`${formatBytes(data.mem_used_mb * 1024 * 1024)} / ${formatBytes(
                data.mem_total_mb * 1024 * 1024,
              )}`}
              percent={memPercent}
            />
            <Meter
              label="存储"
              detail={`${formatBytes(data.disk_used_bytes)} / ${formatBytes(data.disk_total_bytes)}`}
              percent={diskPercent}
            />
          </div>

          <dl className="flex flex-col gap-2">
            {/* 负载与占用率**分开列**：两者不同步时才有信息量——一台占用率
                不高但负载持续超过核数的机器，说明有大量进程在等 IO。 */}
            <Row label="平均负载">
              {data.load_avg_1.toFixed(2)} / {data.load_avg_5.toFixed(2)} /{' '}
              {data.load_avg_15.toFixed(2)}
            </Row>
            <Row label="虚拟机">
              {data.vm_count} 台<span className="text-ink-3"> · {data.vm_running} 台运行中</span>
            </Row>
            <Row label="宿主机运行">{formatUptime(data.uptime_seconds)}</Row>
            <Row label="Agent 启动">{formatDateTime(data.agent_started_at)}</Row>
          </dl>
        </div>
      ) : null}
    </section>
  )
}

/** formatUptime 把秒换算成「3 天 4 小时」。 */
function formatUptime(seconds: number): string {
  if (seconds <= 0) return '—'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days} 天 ${hours} 小时`
  if (hours > 0) return `${hours} 小时 ${minutes} 分`
  return `${minutes} 分`
}

function StorageTab({ nodeID }: { nodeID: number }) {
  const disks = useQuery({ queryKey: ['node-disks', nodeID], queryFn: () => storageApi.disks(nodeID) })
  const pools = useQuery({ queryKey: ['node-pools', nodeID], queryFn: () => storageApi.pools(nodeID) })

  return (
    <div className="flex flex-col gap-4">
      <section className="rounded-card border border-line bg-surface">
        <h2 className="px-4 py-3 text-sm text-ink-3">块设备</h2>
        {disks.isPending ? (
          <PageLoading />
        ) : disks.isError ? (
          <p className="px-4 pb-4 text-base text-warning">{describe(disks.error)}</p>
        ) : (disks.data ?? []).length === 0 ? (
          <p className="px-4 pb-4 text-base text-ink-3">没有探测到块设备。</p>
        ) : (
          <table className="w-full text-base">
            <thead className="text-xs text-ink-3">
              <tr className="border-t border-line">
                <th className="px-4 py-2 text-left font-normal">设备</th>
                <th className="px-4 py-2 text-left font-normal">容量</th>
                <th className="px-4 py-2 text-left font-normal">文件系统</th>
                <th className="px-4 py-2 text-left font-normal">状态</th>
              </tr>
            </thead>
            <tbody>
              {(disks.data ?? []).map((d) => (
                <tr key={d.device_id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="px-4 py-2.5">
                    <span className="font-medium text-ink">{d.path}</span>
                    <span className="ml-2 text-xs text-ink-3">{d.device_id}</span>
                  </td>
                  <td className="kc-nums px-4 py-2.5 text-ink-2">{formatBytes(d.size_bytes)}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {d.filesystem || '—'}
                    {d.mount_point && <span className="text-xs text-ink-3"> → {d.mount_point}</span>}
                  </td>
                  <td className="px-4 py-2.5">
                    {/* 三种状态互斥且优先级明确：系统盘 > 已占用 > 空闲。
                        顺序反了会把系统盘显示成「空闲」，而它恰恰是最不能动的。 */}
                    {d.is_system ? (
                      <span className="text-xs text-ink-3">系统盘</span>
                    ) : d.in_use_by ? (
                      <span className="text-xs text-warning">已用于 {d.in_use_by}</span>
                    ) : d.has_data ? (
                      <span className="text-xs text-warning">含数据</span>
                    ) : (
                      <span className="text-xs text-success">空闲</span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="rounded-card border border-line bg-surface">
        <h2 className="px-4 py-3 text-sm text-ink-3">存储池</h2>
        {pools.isPending ? (
          <PageLoading />
        ) : (pools.data ?? []).length === 0 ? (
          <p className="px-4 pb-4 text-base text-ink-3">
            该节点还没有存储池。在「存储」页选定设备后创建。
          </p>
        ) : (
          <ul className="flex flex-col">
            {(pools.data ?? []).map((p) => (
              <li
                key={p.id}
                className="flex items-center justify-between gap-3 border-t border-line px-4 py-2.5"
              >
                <span className="flex items-baseline gap-2">
                  <span className="text-base text-ink">{p.device_path || p.device_id}</span>
                  {p.is_default && <span className="text-xs text-brand">默认池</span>}
                </span>
                <span className="flex items-center gap-3">
                  <span className="kc-nums text-sm text-ink-3">
                    {formatBytes(p.usable_bytes)} / {formatBytes(p.total_bytes)}
                    {/* 新鲜度：容量是 agent 周期上报的，不是实时探测的。 */}
                    {p.stale && <span className="ml-1 text-warning">可能过期</span>}
                  </span>
                  <StatusBadge tone={POOL_STATUS_TONE[p.status]}>
                    {POOL_STATUS_LABEL[p.status]}
                  </StatusBadge>
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  )
}

function NetworkTab({ nodeID }: { nodeID: number }) {
  const status = useQuery({ queryKey: ['node-network', nodeID], queryFn: () => networkApi.status(nodeID) })
  const networks = useQuery({
    queryKey: ['node-networks', nodeID],
    queryFn: () => networkApi.networks(nodeID),
  })

  return (
    <div className="flex flex-col gap-4">
      <section className="rounded-card border border-line bg-surface p-4">
        <div className="flex items-baseline justify-between gap-2">
          <h2 className="text-sm text-ink-3">后端能力</h2>
          {status.data && (
            <span className="text-sm text-ink-2">{status.data.mode_label}</span>
          )}
        </div>

        {status.isPending ? (
          <PageLoading />
        ) : status.isError ? (
          <p className="mt-2.5 text-base text-warning">{describe(status.error)}</p>
        ) : (
          <>
            {status.data?.probe_failed && (
              <p className="mt-2.5 text-base text-warning">
                探测失败{status.data.probe_message ? `：${status.data.probe_message}` : ''}
                ——下方能力全部为「未知」，这不等于缺失。
              </p>
            )}
            <ul className="mt-2.5 flex flex-col gap-2">
              {(status.data?.capabilities ?? []).map((c) => (
                <li key={c.key} className="flex items-start justify-between gap-3">
                  <span>
                    <span className="text-base text-ink">{c.label}</span>
                    {c.required && <span className="ml-1.5 text-xs text-ink-3">必需</span>}
                    {c.reason && <span className="block text-sm text-ink-3">{c.reason}</span>}
                    {c.fix && (
                      <code className="mt-1 block rounded bg-raised px-1.5 py-0.5 text-xs text-ink-2">
                        {c.fix}
                      </code>
                    )}
                  </span>
                  <StatusBadge tone={CAPABILITY_STATE_TONE[c.state]}>
                    {CAPABILITY_STATE_LABEL[c.state]}
                  </StatusBadge>
                </li>
              ))}
            </ul>
          </>
        )}
      </section>

      <section className="rounded-card border border-line bg-surface">
        <h2 className="px-4 py-3 text-sm text-ink-3">虚拟交换机</h2>
        {(networks.data ?? []).length === 0 ? (
          <p className="px-4 pb-4 text-base text-ink-3">该节点还没有虚拟交换机。</p>
        ) : (
          <ul className="flex flex-col">
            {(networks.data ?? []).map((n) => (
              <li
                key={n.id}
                className="flex items-center justify-between gap-3 border-t border-line px-4 py-2.5"
              >
                <span className="flex items-baseline gap-2">
                  <span className="text-base text-ink">{n.name}</span>
                  {n.is_system && <span className="text-xs text-ink-3">系统</span>}
                  <span className="text-xs text-ink-3">{n.bridge_name}</span>
                </span>
                <span className="text-sm text-ink-3">
                  {n.cidr || '未配置网段'}
                  {n.vlan_id != null && <span className="ml-2">VLAN {n.vlan_id}</span>}
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  )
}

function VmsTab({ nodeID }: { nodeID: number }) {
  const vms = useQuery({
    queryKey: ['vms', { node_id: nodeID }],
    queryFn: () => vmApi.list({ node_id: nodeID, page_size: 100 }),
  })

  if (vms.isPending) return <PageLoading />

  const items = vms.data?.items ?? []
  if (items.length === 0) {
    return <EmptyState title="该节点上还没有虚拟机" description="在虚拟机页新建并选择这个节点。" />
  }

  return (
    <section className="rounded-card border border-line bg-surface">
      <ul className="flex flex-col">
        {items.map((v) => (
          <li
            key={v.id}
            className="flex items-center justify-between gap-3 border-b border-line px-4 py-2.5 last:border-b-0"
          >
            <Link
              to={`/vm/${v.id}`}
              className="font-medium text-ink hover:text-brand hover:underline"
            >
              {v.name}
            </Link>
            <span className="flex items-center gap-3 text-sm text-ink-3">
              <span className="kc-nums">
                {v.vcpu} 核 · {formatBytes(v.memory_mb * 1024 * 1024)}
              </span>
              <span>{v.ip_summary || '—'}</span>
            </span>
          </li>
        ))}
      </ul>
    </section>
  )
}

function TabBar({ value, onChange }: { value: TabKey; onChange: (k: TabKey) => void }) {
  return (
    <div role="tablist" className="flex flex-wrap gap-1 border-b border-line">
      {TABS.map((t) => {
        const active = t.key === value
        return (
          <button
            key={t.key}
            role="tab"
            aria-selected={active}
            onClick={() => onChange(t.key)}
            className={[
              'rounded-t-control px-3.5 py-2 text-base transition-colors',
              active
                ? 'border-b-2 border-brand font-medium text-brand'
                : 'border-b-2 border-transparent text-ink-3 hover:text-ink',
            ].join(' ')}
          >
            {t.label}
          </button>
        )
      })}
    </div>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-base">
      <dt className="shrink-0 text-ink-3">{label}</dt>
      <dd className="truncate text-right text-ink">{children}</dd>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
