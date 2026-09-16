/**
 * 虚拟机详情页（F-2-03）。
 *
 * 结构对齐 FRONTEND.md §5.3.3：Hero（状态与电源操作）+ 标签页
 * （系统信息 / 快照管理 / 网络管理 / 定时任务 / 控制台 / 编辑）。
 *
 * 两条贯穿全页的约定：
 *
 * 1. **页签惰性挂载**——只有当前可见的页签会挂载并拉数据。六个页签一次性
 *    拉全部数据会让打开页面产生十几个并发请求，而用户通常只看其中一个。
 * 2. **未实现的页签明确标注，不隐藏**——隐藏会让「这个产品没有这个能力」
 *    与「这个能力还没做」看起来一样。前者是设计判断，后者是欠账，对使用者
 *    是两件完全不同的事。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import {
  SCHEDULE_ACTION_LABEL,
  SCHEDULE_RESULT_LABEL,
  WEEKDAY_LABEL,
  scheduleApi,
  type ScheduleAction,
  type ScheduleType,
  type VMSchedule,
} from '@/api/schedule'
import { isActive, taskApi, type TaskView } from '@/api/task'
import {
  NIC_MODEL_LABEL,
  vmApi,
  type DiskAction,
  type PowerAction,
  type VmView,
} from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatDateTime, relativeTime } from '@/utils/format'
import {
  POWER_ACTION_DANGEROUS,
  POWER_ACTION_LABEL,
  TASK_STATUS_LABEL,
  TASK_STATUS_TONE,
  VM_STATUS_LABEL,
  VM_STATUS_TONE,
  taskTypeLabel,
} from '@/utils/labels'

/** 详情页的页签。取值与 FRONTEND.md §5.3.3 的表格一一对应。 */
type TabKey = 'system' | 'snapshot' | 'network' | 'schedule' | 'console' | 'edit'

const TABS: { key: TabKey; label: string }[] = [
  { key: 'system', label: '系统信息' },
  { key: 'snapshot', label: '快照管理' },
  { key: 'network', label: '网络管理' },
  { key: 'schedule', label: '定时任务' },
  { key: 'console', label: '控制台' },
  { key: 'edit', label: '编辑' },
]

export function VmDetailPage() {
  const { id } = useParams<{ id: string }>()
  const vmID = Number(id)
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  const [tab, setTab] = useState<TabKey>('system')
  const [confirmAction, setConfirmAction] = useState<PowerAction | null>(null)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const tasks = useQuery({
    queryKey: ['tasks', { resource_id: vmID }],
    queryFn: () => taskApi.list({ resource_id: vmID, page_size: 5 }),
    enabled: Number.isFinite(vmID),
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((t) => isActive(t.status)) ? 2500 : false,
  })

  // 电源操作是异步的。详情页停留期间，只要还有在途任务就持续刷新，
  // 让状态变化能被看到——否则用户会盯着一个不动的「运行中」反复点击。
  const hasActiveTask = (tasks.data?.items ?? []).some((t) => isActive(t.status))

  const detail = useQuery({
    queryKey: ['vm', vmID],
    queryFn: () => vmApi.get(vmID),
    enabled: Number.isFinite(vmID),
    refetchInterval: hasActiveTask ? 2500 : false,
  })

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: ['vm', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['vms'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    void queryClient.invalidateQueries({ queryKey: ['vm-interfaces', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['vm-static-ips', vmID] })
  }

  const power = useMutation({
    mutationFn: (action: PowerAction) => vmApi.power(vmID, action),
    onSuccess: (result, action) => {
      setConfirmAction(null)
      setError('')
      setNotice(`已提交「${POWER_ACTION_LABEL[action]}」，任务 #${result.task_id} 正在执行`)
      refresh()
    },
    onError: (err) => {
      setConfirmAction(null)
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

  const vm = detail.data
  const nodeName = (nodes.data ?? []).find((n) => n.id === vm.node_id)?.name

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-col gap-2">
        <Link to="/vm" className="w-fit text-sm text-ink-3 hover:text-brand">
          ← 返回虚拟机列表
        </Link>
        <div className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-lg font-semibold text-ink">{vm.name}</h1>
            <div className="mt-1.5 flex items-center gap-2">
              <StatusBadge tone={VM_STATUS_TONE[vm.status]}>
                {VM_STATUS_LABEL[vm.status]}
              </StatusBadge>
              {vm.stale && (
                <span
                  className="text-xs text-warning"
                  title={`最近对账：${formatDateTime(vm.last_synced_at)}`}
                >
                  数据可能陈旧
                </span>
              )}
              {!vm.present && (
                <span className="text-xs text-warning">虚拟化层已不存在</span>
              )}
            </div>
          </div>

          <div className="flex flex-wrap justify-end gap-2">
            {vm.available_actions.map((action) => (
              <PowerButton
                key={action}
                action={action}
                loading={power.isPending && power.variables === action}
                onTrigger={(a) => {
                  setError('')
                  setNotice('')
                  // 只有会造成不可逆后果的动作才弹确认框。给「开机」也加确认
                  // 会让用户养成无脑点确认的习惯，真正危险时那道防线就失效了。
                  if (POWER_ACTION_DANGEROUS[a]) {
                    setConfirmAction(a)
                  } else {
                    power.mutate(a)
                  }
                }}
              />
            ))}
            {/* 控制台入口只在真的有控制台时出现：display=none 的虚拟机点了
                也打不开，给一个必然失败的按钮比不给更糟。 */}
            {vm.has_console && (
              <Link to={`/vm/${vm.id}/console`}>
                <Button variant="secondary" size="sm">
                  控制台
                </Button>
              </Link>
            )}
            <Button variant="danger" size="sm" onClick={() => setDeleteOpen(true)}>
              删除
            </Button>
          </div>
        </div>
      </div>

      {vm.available_actions.length === 0 && !vm.stale && (
        <p className="text-base text-ink-3">
          当前状态（{VM_STATUS_LABEL[vm.status]}）下没有可执行的电源操作。
        </p>
      )}

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      <TabBar value={tab} onChange={setTab} />

      {/* 惰性挂载：条件渲染而非 CSS 隐藏，未选中的页签不会发任何请求。 */}
      {tab === 'system' && (
        <SystemTab
          vm={vm}
          nodeName={nodeName}
          tasks={tasks.data?.items ?? []}
        />
      )}
      {tab === 'network' && <NetworkTab vmID={vm.id} />}
      {tab === 'console' && <ConsoleTab vmID={vm.id} />}

      {tab === 'snapshot' && (
        <PlannedTab
          title="快照管理"
          requirement="F-2-07"
          description="创建、恢复、删除快照；关机态与运行态分别使用内部快照与外部快照；配额与含子快照的专项提示。"
          blocked="当前阻塞：需要先建 snapshot 表（迁移里尚未创建），以及 agent 侧的快照能力。"
        />
      )}
      {tab === 'schedule' && <ScheduleTab vmID={vm.id} />}
      {tab === 'edit' && (
        <PlannedTab
          title="编辑配置"
          requirement="F-2-05"
          description="基础配置 / 磁盘与驱动器 / 启动与安全 / 网口 / 硬件直通 / 高级设置；差异提交，运行态可改项与需关机项分别标注。"
          blocked="当前阻塞：磁盘、引导、直通等配置项尚未在投影中建模，且需要 agent 的改配能力。"
        />
      )}

      <Modal
        open={confirmAction !== null}
        title={confirmAction ? `确认${POWER_ACTION_LABEL[confirmAction]}` : ''}
        description={confirmAction ? POWER_ACTION_DANGEROUS[confirmAction] : undefined}
        onClose={() => setConfirmAction(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setConfirmAction(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={power.isPending}
              onClick={() => confirmAction && power.mutate(confirmAction)}
            >
              确认{confirmAction ? POWER_ACTION_LABEL[confirmAction] : ''}
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          当前状态：{VM_STATUS_LABEL[vm.status]}。操作提交后可在「系统信息」中跟踪进度。
        </p>
      </Modal>

      <DeleteVmModal
        open={deleteOpen}
        vmID={vm.id}
        vmName={vm.name}
        onClose={() => setDeleteOpen(false)}
        onDeleted={() => {
          void queryClient.invalidateQueries({ queryKey: ['vms'] })
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
          navigate('/vm')
        }}
      />
    </div>
  )
}

/** TabBar 是详情页的页签条。 */
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

/** SystemTab 汇总系统信息与最近任务。 */
function SystemTab({
  vm,
  nodeName,
  tasks,
}: {
  vm: VmView
  nodeName?: string
  tasks: TaskView[]
}) {
  return (
    <>
      <section className="rounded-card border border-line">
        <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">配置</h2>
        <dl className="grid grid-cols-2 gap-x-6 gap-y-3 px-4 py-3.5 text-base sm:grid-cols-3">
          <Field label="CPU">{vm.vcpu} 核</Field>
          <Field label="内存">{formatMemory(vm.memory_mb)}</Field>
          <Field label="磁盘">{vm.disk_gb} GB</Field>
          <Field label="IP">{vm.ip_summary || '—'}</Field>
          <Field label="所属节点">{nodeName ?? `#${vm.node_id}`}</Field>
          <Field label="分组">{vm.group_name || '—'}</Field>
          <Field label="UUID">
            <span className="kc-mono text-sm">{vm.uuid || '—'}</span>
          </Field>
          <Field label="归属">{vm.owner_id ? `用户 #${vm.owner_id}` : '—'}</Field>
          <Field label="创建时间">{formatDateTime(vm.created_at)}</Field>
          <Field label="备注">{vm.remark || '—'}</Field>
          <Field label="最近对账">{formatDateTime(vm.last_synced_at)}</Field>
          <Field label="控制台">
            {vm.has_console ? '可用' : '无（display=none）'}
          </Field>
        </dl>
      </section>

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">最近任务</h2>
          <Link to="/task" className="text-sm text-brand hover:underline">
            全部任务 →
          </Link>
        </div>

        {tasks.length === 0 && (
          <EmptyState title="没有相关任务" description="对该虚拟机的操作会记录在这里。" />
        )}

        {tasks.length > 0 && (
          <table className="w-full border-collapse text-base">
            <tbody>
              {tasks.map((t) => (
                <tr key={t.id} className="border-t border-line first:border-t-0">
                  <td className="kc-mono px-4 py-2.5 text-ink-3">#{t.id}</td>
                  <td className="px-4 py-2.5 text-ink">{taskTypeLabel(t.type)}</td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={TASK_STATUS_TONE[t.status]}>
                      {TASK_STATUS_LABEL[t.status]}
                    </StatusBadge>
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{relativeTime(t.created_at)}</td>
                  <td className="px-4 py-2.5 text-xs text-danger">{t.error || ''}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </>
  )
}

/** NetworkTab 展示网卡与静态地址（F-2-03「网络管理」）。 */
function NetworkTab({ vmID }: { vmID: number }) {
  const interfaces = useQuery({
    queryKey: ['vm-interfaces', vmID],
    queryFn: () => vmApi.interfaces(vmID),
  })
  const staticIPs = useQuery({
    queryKey: ['vm-static-ips', vmID],
    queryFn: () => vmApi.staticIPs(vmID),
  })

  if (interfaces.isPending || staticIPs.isPending) return <PageLoading />
  if (interfaces.isError) return <ErrorBox message={describe(interfaces.error)} />
  if (staticIPs.isError) return <ErrorBox message={describe(staticIPs.error)} />

  const nics = interfaces.data.items
  const ips = staticIPs.data.items

  return (
    <div className="flex flex-col gap-4">
      <section className="rounded-card border border-line">
        <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">网卡</h2>

        {nics.length === 0 ? (
          <EmptyState
            title="没有网卡"
            description="该虚拟机尚未配置网络接口。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">序号</th>
                <th className="px-4 py-2 text-left font-normal">型号</th>
                <th className="px-4 py-2 text-left font-normal">MAC</th>
                <th className="px-4 py-2 text-left font-normal">接入网络</th>
                <th className="px-4 py-2 text-left font-normal">限速</th>
                <th className="px-4 py-2 text-left font-normal">下发状态</th>
              </tr>
            </thead>
            <tbody>
              {nics.map((n) => (
                <tr key={n.id} className="border-t border-line">
                  <td className="px-4 py-2.5 text-ink">
                    {n.order}
                    {n.is_primary && (
                      <span className="ml-1.5 text-xs text-ink-3">主网卡</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{NIC_MODEL_LABEL[n.model] ?? n.model}</td>
                  <td className="kc-mono px-4 py-2.5 text-sm text-ink-2">{n.mac || '—'}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {n.switch_name ?? <span className="text-ink-3">节点默认网络</span>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {n.rate_limit_mbps > 0 ? `${n.rate_limit_mbps} Mbps` : '不限速'}
                  </td>
                  <td className="px-4 py-2.5">
                    <AppliedBadge applied={n.applied} at={n.last_applied_at} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="rounded-card border border-line">
        <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">
          静态地址
        </h2>

        {ips.length === 0 ? (
          <EmptyState
            title="没有分配静态地址"
            description="虚拟机的 IP 由 DHCP 分配时，这里为空；实际拿到的地址显示在「系统信息」的 IP 一栏。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">地址</th>
                <th className="px-4 py-2 text-left font-normal">网卡</th>
                <th className="px-4 py-2 text-left font-normal">MAC</th>
                <th className="px-4 py-2 text-left font-normal">来源</th>
                <th className="px-4 py-2 text-left font-normal">下发状态</th>
              </tr>
            </thead>
            <tbody>
              {ips.map((s) => (
                <tr key={s.id} className="border-t border-line">
                  <td className="kc-mono px-4 py-2.5 text-ink">{s.ip}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {s.interface_order !== undefined ? `#${s.interface_order}` : '—'}
                  </td>
                  <td className="kc-mono px-4 py-2.5 text-sm text-ink-2">{s.mac || '—'}</td>
                  {/* 区分来源：排查时一个查 DHCP 服务，另一个要进系统看配置文件。 */}
                  <td className="px-4 py-2.5 text-ink-2">
                    {s.is_dhcp_reservation ? 'DHCP 静态租约' : '手工配置'}
                  </td>
                  <td className="px-4 py-2.5">
                    <AppliedBadge applied={s.applied} at={s.applied_at} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <p className="rounded-card border border-line bg-raised px-4 py-3 text-sm text-ink-3">
        网卡的增删改需要下发到节点才能生效，尚未实现。当前页面为只读。
        <br />
        静态地址与「系统信息」里的 IP 可能不一致：前者是我们**期望**的分配，
        后者是从节点**探测到**的实际地址。不一致时以实际地址为准。
      </p>
    </div>
  )
}

/** AppliedBadge 标注配置是否已下发到节点。 */
function AppliedBadge({ applied, at }: { applied: boolean; at?: string }) {
  if (applied) {
    return (
      <span className="text-sm text-success" title={at ? formatDateTime(at) : undefined}>
        已生效
      </span>
    )
  }
  // 尚未下发**不是失败**：任务可能还在队列里。用 idle 色而不是红色——
  // 标红会让人去排查一个可能马上就会完成的操作。
  return (
    <span className="text-sm text-ink-3" title="配置已保存，尚未下发到节点">
      尚未生效
    </span>
  )
}

/** ConsoleTab 提供控制台入口与状态说明。 */
function ConsoleTab({ vmID }: { vmID: number }) {
  return (
    <section className="rounded-card border border-line">
      <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">
        VNC 控制台
      </h2>
      <div className="flex flex-col gap-3 px-4 py-3.5">
        <p className="text-base text-ink-2">
          控制台流量经面板代理，不直连宿主机端口。
        </p>
        <div>
          <Link to={`/vm/${vmID}/console`}>
            <Button size="sm">打开控制台</Button>
          </Link>
        </div>
        <p className="text-sm text-ink-3">
          控制台开关、改密与「对外暴露」需要下发到节点，尚未实现；当前可从
          虚拟机列表进入控制台。
        </p>
      </div>
    </section>
  )
}

/** ScheduleTab 管理虚拟机的定时操作（F-7-05）。 */
function ScheduleTab({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState('')

  const list = useQuery({
    queryKey: ['vm-schedules', vmID],
    queryFn: () => scheduleApi.list(vmID),
  })

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: ['vm-schedules', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const setEnabled = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      scheduleApi.setEnabled(vmID, id, enabled),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const remove = useMutation({
    mutationFn: (id: number) => scheduleApi.remove(vmID, id),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (list.isPending) return <PageLoading />
  if (list.isError) return <ErrorBox message={describe(list.error)} />

  const items = list.data.items

  return (
    <div className="flex flex-col gap-4">
      {error && <ErrorBox message={error} />}

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">定时任务</h2>
          <Button size="sm" onClick={() => setCreating(true)}>
            新增
          </Button>
        </div>

        {items.length === 0 ? (
          <EmptyState
            title="没有定时任务"
            description="可以设置定时开机或关机，例如每晚定时关机以节省资源。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">动作</th>
                <th className="px-4 py-2 text-left font-normal">计划</th>
                <th className="px-4 py-2 text-left font-normal">下次执行</th>
                <th className="px-4 py-2 text-left font-normal">上次结果</th>
                <th className="px-4 py-2 text-left font-normal">状态</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <tr key={s.id} className="border-t border-line">
                  <td className="px-4 py-2.5 text-ink">
                    {SCHEDULE_ACTION_LABEL[s.action] ?? s.action}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{describeSchedule(s)}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {s.next_run_at ? formatDateTime(s.next_run_at) : '—'}
                  </td>
                  <td className="px-4 py-2.5">
                    <LastResultCell result={s.last_result} taskID={s.last_task_id} />
                  </td>
                  <td className="px-4 py-2.5">
                    <span className={s.enabled ? 'text-success' : 'text-ink-3'}>
                      {s.enabled ? '已启用' : '已停用'}
                    </span>
                  </td>
                  <td className="px-4 py-2.5 text-right">
                    <button
                      className="text-sm text-brand hover:underline"
                      disabled={setEnabled.isPending}
                      onClick={() => setEnabled.mutate({ id: s.id, enabled: !s.enabled })}
                    >
                      {s.enabled ? '停用' : '启用'}
                    </button>
                    <button
                      className="ml-3 text-sm text-danger hover:underline"
                      disabled={remove.isPending}
                      onClick={() => remove.mutate(s.id)}
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <p className="rounded-card border border-line bg-raised px-4 py-3 text-sm text-ink-3">
        这里的「删除」删的是**定时任务本身**，不会删除虚拟机。
        <br />
        服务停机期间错过的时间点不会被补执行（记为「已跳过」）——补执行会让
        恢复后连着做几次本该分散在不同时间的操作。
        <br />
        「删除虚拟机」类任务仅支持一次性，且需要二次验证，暂未接入本页。
      </p>

      <CreateScheduleModal
        open={creating}
        vmID={vmID}
        onClose={() => setCreating(false)}
        onCreated={() => {
          setCreating(false)
          refresh()
        }}
      />
    </div>
  )
}

/** LastResultCell 显示上次执行结果。 */
function LastResultCell({ result, taskID }: { result?: string; taskID?: number }) {
  if (!result) return <span className="text-ink-3">尚未执行</span>

  // 「已跳过」用中性色而不是红色：它不是失败，而是**刻意不执行**。
  // 标红会让人去排查一个按设计就没有运行的任务。
  const tone =
    result === 'success' ? 'text-success' : result === 'skipped' ? 'text-ink-2' : 'text-danger'

  return (
    <span className={tone}>
      {SCHEDULE_RESULT_LABEL[result as keyof typeof SCHEDULE_RESULT_LABEL] ?? result}
      {taskID !== undefined && (
        <Link to="/task" className="ml-1.5 text-xs text-ink-3 hover:text-brand">
          #{taskID}
        </Link>
      )}
    </span>
  )
}

/** describeSchedule 把调度配置写成人话。 */
function describeSchedule(s: VMSchedule): string {
  const at = s.time_of_day
  switch (s.schedule_type) {
    case 'once':
      return `一次性 · ${at}`
    case 'daily':
      return `每天 ${at}`
    case 'weekly': {
      const days = s.weekdays.map((d) => WEEKDAY_LABEL[d] ?? d).join('、')
      return `${days || '未选'} ${at}`
    }
    default:
      return at
  }
}

/** CreateScheduleModal 新建定时任务。 */
function CreateScheduleModal({
  open,
  vmID,
  onClose,
  onCreated,
}: {
  open: boolean
  vmID: number
  onClose: () => void
  onCreated: () => void
}) {
  const [action, setAction] = useState<ScheduleAction>('shutdown')
  const [kind, setKind] = useState<ScheduleType>('daily')
  const [weekdays, setWeekdays] = useState<number[]>([1])
  const [timeOfDay, setTimeOfDay] = useState('03:00')
  const [date, setDate] = useState('')
  const [error, setError] = useState('')

  const create = useMutation({
    mutationFn: () =>
      scheduleApi.create(vmID, {
        action,
        schedule_type: kind,
        weekdays: kind === 'weekly' ? weekdays : undefined,
        time_of_day: timeOfDay,
        date: kind === 'once' ? date : undefined,
      }),
    onSuccess: () => {
      reset()
      onCreated()
    },
    onError: (err) => setError(describe(err)),
  })

  function reset() {
    setAction('shutdown')
    setKind('daily')
    setWeekdays([1])
    setTimeOfDay('03:00')
    setDate('')
    setError('')
  }

  function handleClose() {
    reset()
    onClose()
  }

  // 提交按钮的可用性：把「后端一定会拒绝的请求」拦在本地，让用户不用
  // 提交一次才知道少填了东西。真正的校验仍在服务端。
  const ready =
    timeOfDay !== '' &&
    (kind !== 'once' || date !== '') &&
    (kind !== 'weekly' || weekdays.length > 0)

  return (
    <Modal
      open={open}
      title="新增定时任务"
      onClose={handleClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={handleClose}>
            取消
          </Button>
          <Button size="sm" disabled={!ready} loading={create.isPending} onClick={() => create.mutate()}>
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <label className="flex flex-col gap-1">
          <span className="text-sm text-ink-2">动作</span>
          <select
            className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            value={action}
            onChange={(e) => setAction(e.target.value as ScheduleAction)}
          >
            <option value="shutdown">关机</option>
            <option value="start">开机</option>
          </select>
        </label>

        <label className="flex flex-col gap-1">
          <span className="text-sm text-ink-2">重复方式</span>
          <select
            className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            value={kind}
            onChange={(e) => setKind(e.target.value as ScheduleType)}
          >
            <option value="once">仅一次</option>
            <option value="daily">每天</option>
            <option value="weekly">每周</option>
          </select>
        </label>

        {kind === 'weekly' && (
          <fieldset className="flex flex-col gap-1.5">
            <legend className="text-sm text-ink-2">星期</legend>
            <div className="flex flex-wrap gap-1.5">
              {[1, 2, 3, 4, 5, 6, 7].map((d) => {
                const on = weekdays.includes(d)
                return (
                  <button
                    key={d}
                    type="button"
                    onClick={() =>
                      setWeekdays(on ? weekdays.filter((x) => x !== d) : [...weekdays, d].sort())
                    }
                    className={[
                      'rounded-control border px-2.5 py-1 text-sm',
                      on
                        ? 'border-brand bg-brand/10 text-brand'
                        : 'border-line-strong text-ink-3 hover:text-ink',
                    ].join(' ')}
                  >
                    {WEEKDAY_LABEL[d]}
                  </button>
                )
              })}
            </div>
          </fieldset>
        )}

        {kind === 'once' && (
          <Input
            label="日期"
            type="date"
            value={date}
            onChange={(e) => setDate(e.target.value)}
          />
        )}

        <Input
          label="时刻"
          type="time"
          value={timeOfDay}
          onChange={(e) => setTimeOfDay(e.target.value)}
          hint="按服务器本地时间执行"
        />

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </Modal>
  )
}

/** PlannedTab 说明一个尚未实现的页签。 */
function PlannedTab({
  title,
  requirement,
  description,
  blocked,
}: {
  title: string
  requirement: string
  description: string
  blocked: string
}) {
  return (
    <section className="rounded-card border border-line bg-raised px-4 py-5">
      <p className="text-base font-medium text-ink">{title}</p>
      <p className="mt-1.5 text-base text-ink-2">{description}</p>
      <p className="mt-3 text-sm text-ink-3">
        对应需求 {requirement}。
      </p>
      <p className="mt-1 text-sm text-ink-3">{blocked}</p>
    </section>
  )
}

function ErrorBox({ message }: { message: string }) {
  return (
    <p role="alert" className="rounded-card bg-danger/10 px-4 py-3 text-base text-danger">
      {message}
    </p>
  )
}

function PowerButton({
  action,
  loading,
  onTrigger,
}: {
  action: PowerAction
  loading: boolean
  onTrigger: (action: PowerAction) => void
}) {
  // 开机是恢复性操作，用主按钮；关机类用次级按钮；强制断电是危险动作，
  // 与其它操作在视觉上区分开。
  const variant = action === 'start' ? 'primary' : action === 'poweroff' ? 'danger' : 'secondary'
  return (
    <Button variant={variant} size="sm" loading={loading} onClick={() => onTrigger(action)}>
      {POWER_ACTION_LABEL[action]}
    </Button>
  )
}

function DeleteVmModal({
  open,
  vmID,
  vmName,
  onClose,
  onDeleted,
}: {
  open: boolean
  vmID: number
  vmName: string
  onClose: () => void
  onDeleted: () => void
}) {
  const [diskAction, setDiskAction] = useState<DiskAction | ''>('')
  const [error, setError] = useState('')

  const remove = useMutation({
    mutationFn: () => vmApi.remove(vmID, diskAction as DiskAction),
    onSuccess: () => {
      setDiskAction('')
      setError('')
      onDeleted()
    },
    onError: (err) => setError(describe(err)),
  })

  function handleClose() {
    setDiskAction('')
    setError('')
    onClose()
  }

  return (
    <Modal
      open={open}
      title={`删除虚拟机 ${vmName}`}
      description="删除后该虚拟机将不再出现在列表中，操作不可撤销。"
      onClose={handleClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={handleClose}>
            取消
          </Button>
          <Button
            variant="danger"
            size="sm"
            // 未选择磁盘处理方式时禁用提交：默认值是「用户最可能接受的选项」，
            // 而连盘删除的误操作代价是数据永久丢失，这个选择必须由用户做出。
            disabled={diskAction === ''}
            loading={remove.isPending}
            onClick={() => remove.mutate()}
          >
            确认删除
          </Button>
        </>
      }
    >
      <fieldset className="flex flex-col gap-3">
        <legend className="mb-1 text-sm font-medium text-ink-2">磁盘处理方式</legend>

        <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-line-strong px-3 py-2.5 hover:bg-raised">
          <input
            type="radio"
            name="disk_action"
            className="mt-0.5"
            checked={diskAction === 'keep'}
            onChange={() => setDiskAction('keep')}
          />
          <span>
            <span className="block text-base text-ink">保留磁盘</span>
            <span className="block text-sm text-ink-3">
              磁盘保留在存储池中，可事后手动清理。不删除数据。
            </span>
          </span>
        </label>

        <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-line-strong px-3 py-2.5 hover:bg-raised">
          <input
            type="radio"
            name="disk_action"
            className="mt-0.5"
            checked={diskAction === 'delete'}
            onChange={() => setDiskAction('delete')}
          />
          <span>
            <span className="block text-base text-danger">连同磁盘删除</span>
            <span className="block text-sm text-ink-3">
              磁盘数据将被永久删除，无法恢复。
            </span>
          </span>
        </label>

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </fieldset>
    </Modal>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-xs text-ink-3">{label}</dt>
      <dd className="text-ink">{children}</dd>
    </div>
  )
}

function formatMemory(mb: number): string {
  if (mb >= 1024) return `${(mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1)} GB`
  return `${mb} MB`
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
