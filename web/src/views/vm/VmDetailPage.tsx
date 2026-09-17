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
import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import {
  editApi,
  type EditField,
  type EditForm,
  type EditGroupInfo,
} from '@/api/edit'
import { nodeApi } from '@/api/node'
import { ExportTab } from '@/views/vm/ExportTab'
import { GuestActionsSection } from '@/views/vm/GuestActionsSection'
import { templateApi } from '@/api/template'
import {
  SNAPSHOT_KIND_LABEL,
  SNAPSHOT_STATUS_LABEL,
  SNAPSHOT_STATUS_TONE,
  snapshotApi,
  type Snapshot,
} from '@/api/snapshot'
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
  NIC_MODEL_OPTIONS,
  netApi,
  type AddPortForwardInput,
  type BindStaticIPInput,
  type InterfaceInput,
  type PortForward,
} from '@/api/net'
import {
  NIC_MODEL_LABEL,
  vmApi,
  type DiskAction,
  type NICModel,
  type PowerAction,
  type StaticIP,
  type TaskRef,
  type VMInterface,
  type VmStatus,
  type VmView,
} from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Meter } from '@/components/common/Meter'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatBytes, formatDateTime, relativeTime } from '@/utils/format'
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
type TabKey = 'system' | 'snapshot' | 'network' | 'schedule' | 'export' | 'console' | 'edit'

const TABS: { key: TabKey; label: string }[] = [
  { key: 'system', label: '系统信息' },
  { key: 'snapshot', label: '快照管理' },
  { key: 'network', label: '网络管理' },
  { key: 'schedule', label: '定时任务' },
  { key: 'export', label: '导出' },
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
  const [lockOpen, setLockOpen] = useState(false)
  const [lockReason, setLockReason] = useState('')
  const [rescueOpen, setRescueOpen] = useState(false)
  const [reinstallOpen, setReinstallOpen] = useState(false)
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

  // 加锁是同步的（锁只在控制面），解锁需要二次验证——但 428 与重放都在
  // 请求层处理，这里拿到的是一个普通的 Promise。
  const setLock = useMutation({
    mutationFn: (locked: boolean) => vmApi.setLock(vmID, locked, lockReason),
    onSuccess: (view) => {
      setLockOpen(false)
      setLockReason('')
      setError('')
      setNotice(view.locked ? '已锁定：删除操作将被拒绝' : '已解锁')
      refresh()
    },
    onError: (err) => {
      setLockOpen(false)
      setError(describe(err))
    },
  })

  // 救援进入 / 退出。两者都是任务：都要改硬件配置并重启，因此提交后按
  // 任务跟踪，页面不等待。
  const rescue = useMutation({
    mutationFn: (action: 'enter' | 'exit') =>
      action === 'enter' ? vmApi.enterRescue(vmID) : vmApi.exitRescue(vmID),
    onSuccess: (result, action) => {
      setRescueOpen(false)
      setError('')
      setNotice(
        action === 'enter'
          ? `已提交进入救援，任务 #${result.task_id} 正在执行`
          : `已提交退出救援，任务 #${result.task_id} 正在执行`,
      )
      refresh()
    },
    onError: (err) => {
      setRescueOpen(false)
      setError(describe(err))
    },
  })

  // 重装与清理备份。重装会触发二次验证，弹框由请求层唤起。
  const reinstall = useMutation({
    mutationFn: (templateID: number) => vmApi.reinstall(vmID, templateID),
    onSuccess: (result) => {
      setReinstallOpen(false)
      setError('')
      setNotice(`已提交重装，任务 #${result.task_id} 正在执行`)
      refresh()
    },
    onError: (err) => {
      setReinstallOpen(false)
      setError(describe(err))
    },
  })

  const purgeBackup = useMutation({
    mutationFn: () => vmApi.purgeReinstallBackup(vmID),
    onSuccess: (result) => {
      setError('')
      setNotice(`已提交清理备份，任务 #${result.task_id} 正在执行`)
      refresh()
    },
    onError: (err) => setError(describe(err)),
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
            {/* 锁定的开关。加锁是收紧、解锁是放松——只有后者需要验证，
                因此这里就是一次普通的调用。 */}
            {vm.locked ? (
              <Button
                variant="secondary"
                size="sm"
                loading={setLock.isPending}
                onClick={() => setLock.mutate(false)}
              >
                解锁
              </Button>
            ) : (
              <Button variant="secondary" size="sm" onClick={() => setLockOpen(true)}>
                锁定
              </Button>
            )}
            {/* 重装系统。高风险（整块系统盘被替换），因此走二次验证——
                但验证弹框由请求层自动唤起，这里只是一次普通调用。
                未清理的备份会挡着它，因此有备份时禁用并说明。 */}
            <Button
              variant="secondary"
              size="sm"
              disabled={vm.status !== 'stopped' || vm.has_reinstall_backup}
              title={
                vm.has_reinstall_backup
                  ? '存在未清理的系统盘备份，请先清理——否则会覆盖你回到原系统的唯一退路'
                  : vm.status !== 'stopped'
                    ? '重装需要先关机'
                    : ''
              }
              onClick={() => setReinstallOpen(true)}
            >
              重装系统
            </Button>

            {/* 救援入口只在关机时可用：它改动的是引导顺序与盘型，热改会让
                控制面记录的配置与虚拟化层实际分叉。禁用 + 说明原因，而不是
                点下去才被后端拒绝。 */}
            {vm.rescue_active ? (
              <Button
                variant="secondary"
                size="sm"
                disabled={vm.status !== 'stopped'}
                loading={rescue.isPending}
                title={vm.status !== 'stopped' ? '退出救援需要先关机' : ''}
                onClick={() => rescue.mutate('exit')}
              >
                退出救援
              </Button>
            ) : (
              <Button
                variant="secondary"
                size="sm"
                disabled={vm.status !== 'stopped'}
                title={vm.status !== 'stopped' ? '进入救援需要先关机' : ''}
                onClick={() => setRescueOpen(true)}
              >
                进入救援
              </Button>
            )}
            <Button
              variant="danger"
              size="sm"
              // 锁定时禁止删除（F-2-12）。按钮禁用 + 说明原因，而不是点下去
              // 才被后端拒绝——后者会让用户以为是系统出了问题。
              disabled={vm.locked}
              title={vm.locked ? '该虚拟机已锁定，需先解锁才能删除' : ''}
              onClick={() => setDeleteOpen(true)}
            >
              删除
            </Button>
          </div>
        </div>
      </div>

      <VmHero vm={vm} />

      {/* 链式克隆的依赖必须显示出来：磁盘只是模板之上的一层覆盖，模板被删后
          数据就不可用了，而且不会立刻报错。用户看不到这条关系，就无法理解
          为什么「删掉一个模板」会让自己的机器出事。 */}
      {vm.clone_mode === 'linked' && (
        <div className="rounded-card border border-line bg-raised px-4 py-3">
          <p className="text-base text-ink-2">
            此虚拟机是模板
            <span className="text-ink"> #{vm.template_id}</span> 的链式克隆，
            磁盘以该模板为底层。
            <span className="font-medium text-ink">
              模板被删除后，这台机器的数据将不可用
            </span>
            ——且不会立刻报错，要等到下次开机或读到未缓存的数据块时才暴露。
          </p>
        </div>
      )}

      {/* 救援提示放在锁定提示之前：救援改变的是「你现在看到的是什么系统」，
          比「能不能删」更根本。救援模式下盘型、网卡、引导顺序都被改过，
          把它当成日常状态会让人做出错误判断。 */}
      {vm.rescue_active && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base font-medium text-warning">此虚拟机处于救援模式</p>
          <p className="mt-1 text-base text-ink-2">
            它当前从救援镜像启动，盘型、网卡与引导顺序均已调整——
            <span className="font-medium text-ink">画面里看到的不是你的系统</span>。
            磁盘内容原样保留，退出救援时会按进入前的配置自动还原。
            {vm.rescue_since && (
              <span className="text-ink-3"> · 自 {formatDateTime(vm.rescue_since)}</span>
            )}
          </p>
        </div>
      )}

      {/* 备份提示。它是用户「回到原来的系统」的唯一退路，因此既要让他知道
          它存在，也要让他知道它挡着下一次重装。 */}
      {vm.has_reinstall_backup && (
        <div className="flex items-start justify-between gap-4 rounded-card border border-line bg-raised px-4 py-3">
          <p className="text-base text-ink-2">
            重装系统时留下的原系统盘备份仍在占用存储空间。
            <span className="text-ink-3">
              它是回到原系统的唯一退路；确认不再需要后可以清理，
              清理后即可再次重装。
            </span>
            {vm.reinstall_at && (
              <span className="text-ink-3"> · 重装于 {formatDateTime(vm.reinstall_at)}</span>
            )}
          </p>
          <Button
            variant="secondary"
            size="sm"
            disabled={vm.status !== 'stopped'}
            loading={purgeBackup.isPending}
            title={vm.status !== 'stopped' ? '清理备份需要先关机' : ''}
            onClick={() => purgeBackup.mutate()}
          >
            清理备份
          </Button>
        </div>
      )}

      {vm.locked && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base font-medium text-warning">此虚拟机已锁定</p>
          <p className="mt-1 text-base text-ink-2">
            锁定期间无法删除，也不会被批量删除操作选中执行。
            {vm.lock_reason ? (
              <>
                锁定原因：<span className="text-ink">{vm.lock_reason}</span>
              </>
            ) : (
              <span className="text-ink-3">（未填写原因）</span>
            )}
            {vm.locked_at && (
              <span className="text-ink-3"> · {formatDateTime(vm.locked_at)}</span>
            )}
          </p>
        </div>
      )}

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
        <div className="flex flex-col gap-4">
          <SystemTab
            vm={vm}
            nodeName={nodeName}
            tasks={tasks.data?.items ?? []}
          />
          {/* 来宾自动化（f-2-10）挂在「系统信息」下而不是另开一个页签：
              它是对这台机器的运维动作，与「这台机器是什么样」属于同一处
              上下文，而页签已经七个了。 */}
          <GuestActionsSection vm={vm} />
        </div>
      )}
      {tab === 'network' && <NetworkTab vmID={vm.id} />}
      {tab === 'console' && <ConsoleTab vmID={vm.id} />}

      {tab === 'snapshot' && <SnapshotTab vmID={vm.id} />}
      {tab === 'schedule' && <ScheduleTab vmID={vm.id} />}
      {tab === 'export' && <ExportTab vm={vm} />}
      {tab === 'edit' && <EditTab vmID={vm.id} onSaved={refresh} />}

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

      <ReinstallModal
        open={reinstallOpen}
        nodeID={vm.node_id}
        vmName={vm.name}
        pending={reinstall.isPending}
        onClose={() => setReinstallOpen(false)}
        onConfirm={(templateID) => reinstall.mutate(templateID)}
      />

      <Modal
        open={rescueOpen}
        title={`让「${vm.name}」进入救援模式`}
        description="虚拟机会从救援镜像启动，盘型、网卡与引导顺序会被调整。磁盘内容原样保留，退出时按进入前的配置自动还原。"
        onClose={() => setRescueOpen(false)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setRescueOpen(false)}>
              取消
            </Button>
            <Button
              size="sm"
              loading={rescue.isPending}
              onClick={() => rescue.mutate('enter')}
            >
              进入救援
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-2 text-base text-ink-2">
          <p>
            进入前会保存一份配置快照（引导顺序、机型、固件、网卡与显示设备），
            退出时按它还原。
          </p>
          <p className="text-ink-3">
            快照只覆盖救援过程会改动的字段——你在这期间通过编辑页改的其它配置
            不会被回滚，因为那属于「你的改动」，不属于「救援过程」。
          </p>
          <p>
            当前为
            <span className="text-ink">{VM_STATUS_LABEL[vm.status]}</span>
            ，进入救援需要保持关机状态。
          </p>
        </div>
      </Modal>

      <Modal
        open={lockOpen}
        title={`锁定「${vm.name}」`}
        description="锁定后无法删除该虚拟机，也不会被批量删除选中执行。随时可以解锁——解锁需要一次二次验证。"
        onClose={() => setLockOpen(false)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setLockOpen(false)}>
              取消
            </Button>
            <Button size="sm" loading={setLock.isPending} onClick={() => setLock.mutate(true)}>
              锁定
            </Button>
          </>
        }
      >
        {/* 原因必填与否是一个取舍：要求必填能让「为什么锁着」总有答案，
            但也可能让人嫌麻烦干脆不加锁。因此它是可选的，但界面上明确
            说明「会被显示给其他人看」——写不写由用户判断。 */}
        <Input
          label="锁定原因（可选）"
          value={lockReason}
          maxLength={255}
          placeholder="例如：生产环境主库，禁止删除"
          onChange={(e) => setLockReason(e.target.value)}
          hint="会显示在虚拟机列表与详情页上，让其他人知道为什么不能删。"
        />
      </Modal>
    </div>
  )
}

/**
 * ReinstallModal 选择模板并确认重装（F-2-11）。
 *
 * 确认框里把「会丢什么、会留什么」写清楚：整块系统盘被替换，而硬件配置、
 * 数据盘与主网口绑定都保留。用户点下这个按钮之前必须知道边界在哪——
 * 「重装会不会把我挂的数据盘也格了」是最先要回答的问题。
 */
function ReinstallModal({
  open,
  nodeID,
  vmName,
  pending,
  onClose,
  onConfirm,
}: {
  open: boolean
  nodeID: number
  vmName: string
  pending: boolean
  onClose: () => void
  onConfirm: (templateID: number) => void
}) {
  const [templateID, setTemplateID] = useState(0)

  // 只列**同一节点**上可用的模板：模板盘就在它所属节点的存储池里，
  // 跨节点使用需要先导出再导入。列出来再被拒绝只会让人以为是自己操作错了。
  const templates = useQuery({
    queryKey: ['templates', { node_id: nodeID, only_ready: true }],
    queryFn: () => templateApi.list({ node_id: nodeID, only_ready: true }),
    enabled: open && nodeID > 0,
  })

  const candidates = templates.data ?? []

  return (
    <Modal
      open={open}
      title={`重装「${vmName}」的系统`}
      description="用选定的模板重建系统盘。原系统盘会先被备份保留，确认不再需要后可在详情页清理。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            variant="danger"
            size="sm"
            disabled={templateID === 0}
            loading={pending}
            onClick={() => onConfirm(templateID)}
          >
            确认重装
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2.5">
          <p className="text-base font-medium text-danger">
            整块系统盘会被替换
          </p>
          <p className="mt-1 text-base text-ink-2">
            原系统上的软件、配置与没放在数据盘上的数据都会消失。
            这一步会要求二次验证。
          </p>
        </div>

        <div className="rounded-control border border-line px-3 py-2.5">
          <p className="text-base text-ink">保留的内容</p>
          <ul className="mt-1 flex flex-col gap-0.5 text-sm text-ink-3">
            <li>硬件配置（CPU、内存、机型、固件）</li>
            <li>数据盘及其上的数据</li>
            <li>主网口的绑定关系</li>
          </ul>
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">用于重建的模板</label>
          <select
            value={templateID}
            onChange={(e) => setTemplateID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
          >
            <option value={0}>请选择…</option>
            {candidates.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name} · {t.default_cpu} 核 {t.default_memory_mb} MB
              </option>
            ))}
          </select>
          {candidates.length === 0 && !templates.isPending && (
            <p className="text-xs text-ink-3">
              该节点上没有可用于重装的模板。请先从一个已关机的虚拟机创建模板。
            </p>
          )}
        </div>
      </div>
    </Modal>
  )
}

/**
 * VmHero 是详情页的 Hero 三卡（FRONTEND.md §5.3.3）。
 *
 * 抽成独立组件而不是写在主组件里：它有两个会轮询的查询，而主组件在详情
 * 加载完成前就 return 了——把钩子写在那之后是**条件调用**，React 不允许，
 * 写在之前又拿不到 vm（需要在拿到 vm 之后才能判断「是否在运行」）。
 * 挂载时机本身就是条件，这正是组件的用途。
 */
function VmHero({ vm }: { vm: VmView }) {
  const running = vm.status === 'running'

  const stats = useQuery({
    queryKey: ['vm-stats', vm.id],
    queryFn: () => vmApi.stats(vm.id),
    // **只在运行时轮询**：停机的虚拟机没有指标可读，每 5 秒问一次
    // 只会得到一整屏「—」，还顺带把节点唤醒一遍。
    enabled: running,
    refetchInterval: running ? 5000 : false,
  })

  const form = useQuery({
    queryKey: ['vm-edit-form', vm.id],
    queryFn: () => editApi.form(vm.id),
    // 配置摘要几乎不变：页签切来切去不该让它反复请求。
    staleTime: 60_000,
  })

  // 控制台画面按固定间隔换帧。
  //
  // 20 秒是个折中：帧本身要等 hypervisor 出一帧（比指标慢得多），间隔太短
  // 会让请求堆在一起；太长则预览卡看起来像死图。用 state 计数而不是定时请求
  // 数据——画面交给 `<img>` 自己去取，浏览器能按需要复用连接。
  const [stamp, setStamp] = useState(() => Date.now())
  useEffect(() => {
    if (!vm.has_console) return
    const timer = setInterval(() => setStamp(Date.now()), 20_000)
    return () => clearInterval(timer)
  }, [vm.has_console])

  const values = form.data?.values ?? {}
  const config = (key: string) => {
    const v = values[key]
    return v == null || v === '' ? '—' : String(v)
  }

  return (
    <div className="grid gap-4 lg:grid-cols-3">
      {/* 卡一：状态与配置摘要 */}
      <HeroCard title="状态">
        <dl className="flex flex-col gap-2">
          <HeroRow label="运行时长">
            {running && stats.data ? formatUptime(stats.data.uptime_seconds) : '—'}
          </HeroRow>
          <HeroRow label="机器类型">{config('machine_type')}</HeroRow>
          <HeroRow label="固件">
            {config('firmware')}
            {values.secure_boot === true && <span className="text-ink-3"> · 安全启动</span>}
          </HeroRow>
          <HeroRow label="引导顺序">{config('boot_order')}</HeroRow>
          <HeroRow label="自动启动">
            {values.auto_start === true ? '开启' : values.auto_start === false ? '关闭' : '—'}
          </HeroRow>
        </dl>
      </HeroCard>

      {/* 卡二：资源用量。
          数据来自节点探测（agent.OpVMStats），不是控制面按配置推算——
          控制面看到的 vcpu / memory_mb 是**配置**而不是**用量**，把配置当
          用量显示，用户会看到一台空闲机器常年「内存占满」。 */}
      <HeroCard
        title="资源"
        action={
          running && stats.data ? (
            <span className="text-xs text-ink-3">{relativeTime(stats.data.at)}</span>
          ) : null
        }
      >
        {!running ? (
          <p className="text-sm text-ink-3">虚拟机未运行，无实时指标。</p>
        ) : stats.isError ? (
          // 采集失败要说清楚是「没读到」而不是显示 0%——0% 看起来是
          // 「机器很闲」，而实际是「不知道」。
          <p className="text-sm text-warning">指标采集失败：{describe(stats.error)}</p>
        ) : (
          <div className="flex flex-col gap-3">
            <Meter
              label="CPU"
              detail={`${vm.vcpu} 核`}
              percent={stats.data?.cpu_percent ?? 0}
            />
            <Meter
              label="内存"
              detail={
                stats.data
                  ? `${formatMemory(stats.data.mem_used_mb)} / ${formatMemory(stats.data.mem_total_mb)}`
                  : '—'
              }
              percent={
                stats.data && stats.data.mem_total_mb > 0
                  ? (stats.data.mem_used_mb / stats.data.mem_total_mb) * 100
                  : 0
              }
            />
            <div className="grid grid-cols-2 gap-x-4 gap-y-1.5">
              <Rate label="网络 ↓" kbps={stats.data?.net_rx_kbps} />
              <Rate label="网络 ↑" kbps={stats.data?.net_tx_kbps} />
              <Rate label="磁盘读" kbps={stats.data?.disk_read_kbps} />
              <Rate label="磁盘写" kbps={stats.data?.disk_write_kbps} />
            </div>
          </div>
        )}
      </HeroCard>

      {/* 卡三：控制台预览。
          display=none 的虚拟机**不显示这张卡**：给一个必然黑屏的预览，
          比不给更糟——用户会以为虚拟机出问题了。 */}
      {vm.has_console && (
        <HeroCard
          title="控制台预览"
          action={
            <Link
              to={`/vm/${vm.id}/console`}
              className="text-xs text-brand hover:underline"
            >
              打开控制台 →
            </Link>
          }
        >
          <Link
            to={`/vm/${vm.id}/console`}
            className="block overflow-hidden rounded-control border border-line bg-[#1b1e24]"
          >
            <img
              // key 跟着 stamp 变，强制浏览器重新拉图：URL 里已经带了
              // 时间戳，但 React 不会因为 src 变化就丢弃已解码的旧图，
              // 换 key 能确保加载态与错误态一起重置。
              key={stamp}
              src={vmApi.consoleFrameUrl(vm.id, stamp)}
              alt={`${vm.name} 的控制台预览`}
              className="aspect-video w-full object-cover"
              onError={(e) => {
                // 失败时隐藏图片而不是留一个破图图标：破图看起来像前端坏了，
                // 而实际原因在节点侧。
                e.currentTarget.style.display = 'none'
              }}
            />
          </Link>
          {!running && (
            <p className="mt-1.5 text-xs text-ink-3">虚拟机未运行，画面可能为空。</p>
          )}
        </HeroCard>
      )}
    </div>
  )
}

function HeroCard({
  title,
  action,
  children,
}: {
  title: string
  action?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between gap-2">
        <h2 className="text-sm text-ink-3">{title}</h2>
        {action}
      </div>
      <div className="mt-2.5">{children}</div>
    </section>
  )
}

function HeroRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-base">
      <dt className="shrink-0 text-ink-3">{label}</dt>
      <dd className="truncate text-right text-ink">{children}</dd>
    </div>
  )
}

/** Rate 显示一项速率；无数据时显示「—」而不是 0。 */
function Rate({ label, kbps }: { label: string; kbps?: number }) {
  return (
    <div className="flex items-baseline justify-between gap-2 text-base">
      <span className="shrink-0 text-ink-3">{label}</span>
      <span className="kc-nums truncate text-right text-ink">{formatRate(kbps)}</span>
    </div>
  )
}

/** formatRate 把 Kbps 换算成可读的速率。 */
function formatRate(kbps?: number): string {
  if (kbps == null) return '—'
  if (kbps < 1000) return `${kbps.toFixed(0)} Kbps`
  return `${(kbps / 1000).toFixed(1)} Mbps`
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

/** NetworkTab 管理虚拟机的网络（F-2-03「网络管理」）。 */
function NetworkTab({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [editNic, setEditNic] = useState<VMInterface | null>(null)
  const [deleteNic, setDeleteNic] = useState<VMInterface | null>(null)
  const [addingNic, setAddingNic] = useState(false)
  const [bindingIP, setBindingIP] = useState(false)
  const [deleteIP, setDeleteIP] = useState<StaticIP | null>(null)
  const [addingPF, setAddingPF] = useState(false)
  const [deletePF, setDeletePF] = useState<PortForward | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const interfaces = useQuery({
    queryKey: ['vm-interfaces', vmID],
    queryFn: () => vmApi.interfaces(vmID),
  })
  const staticIPs = useQuery({
    queryKey: ['vm-static-ips', vmID],
    queryFn: () => vmApi.staticIPs(vmID),
  })
  const forwards = useQuery({
    queryKey: ['vm-port-forwards', vmID],
    queryFn: () => netApi.listPortForwards(vmID),
  })

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: ['vm-interfaces', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['vm-static-ips', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['vm-port-forwards', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  // 所有写操作都是「提交任务」这一个形状，用一个 hook 收口：
  // 各自写一遍 onSuccess / onError 只会得到五份几乎相同的代码。
  const submit = useMutation({
    mutationFn: (fn: () => Promise<TaskRef>) => fn(),
    onSuccess: (r, _fn, ctx) => {
      void ctx
      setNotice(`已提交，任务 #${r.task_id} 正在执行`)
      setError('')
      closeAll()
      refresh()
    },
    onError: (err) => {
      setError(describe(err))
      closeAll()
    },
  })

  function closeAll() {
    setAddingNic(false)
    setEditNic(null)
    setDeleteNic(null)
    setBindingIP(false)
    setDeleteIP(null)
    setAddingPF(false)
    setDeletePF(null)
  }

  if (interfaces.isPending || staticIPs.isPending || forwards.isPending) return <PageLoading />
  if (interfaces.isError) return <ErrorBox message={describe(interfaces.error)} />
  if (staticIPs.isError) return <ErrorBox message={describe(staticIPs.error)} />
  if (forwards.isError) return <ErrorBox message={describe(forwards.error)} />

  const nics = interfaces.data.items
  const ips = staticIPs.data.items
  const pfs = forwards.data.items

  return (
    <div className="flex flex-col gap-4">
      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && <ErrorBox message={error} />}

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">网卡</h2>
          <Button size="sm" onClick={() => setAddingNic(true)}>
            新增网卡
          </Button>
        </div>

        {nics.length === 0 ? (
          <EmptyState title="没有网卡" description="该虚拟机尚未配置网络接口。" />
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
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {nics.map((n) => (
                <tr key={n.id} className="border-t border-line">
                  <td className="px-4 py-2.5 text-ink">
                    {n.order}
                    {n.is_primary && <span className="ml-1.5 text-xs text-ink-3">主网卡</span>}
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
                  <td className="px-4 py-2.5 text-right whitespace-nowrap">
                    <button
                      className="text-sm text-brand hover:underline"
                      onClick={() => setEditNic(n)}
                    >
                      编辑
                    </button>
                    <button
                      className="ml-3 text-sm text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={n.is_primary}
                      // 主网卡不可删：重装系统依赖它保持网络可达。
                      // 禁用而不是隐藏，附上原因让用户知道为什么。
                      title={n.is_primary ? '主网卡不能删除：重装系统等操作依赖它保持网络可达' : ''}
                      onClick={() => setDeleteNic(n)}
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

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">静态地址</h2>
          <Button size="sm" onClick={() => setBindingIP(true)}>
            绑定地址
          </Button>
        </div>

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
                <th className="px-4 py-2 text-right font-normal">操作</th>
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
                  <td className="px-4 py-2.5 text-right">
                    <button
                      className="text-sm text-danger hover:underline"
                      onClick={() => setDeleteIP(s)}
                    >
                      解绑
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">端口转发</h2>
          <Button size="sm" onClick={() => setAddingPF(true)}>
            新增转发
          </Button>
        </div>

        {pfs.length === 0 ? (
          <EmptyState
            title="没有端口转发"
            description="端口转发把宿主机上的一个端口指向虚拟机的服务，让外部可以访问。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">宿主机端口</th>
                <th className="px-4 py-2 text-left font-normal">目标</th>
                <th className="px-4 py-2 text-left font-normal">允许来源</th>
                <th className="px-4 py-2 text-left font-normal">下发状态</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {pfs.map((p) => (
                <tr key={p.id} className="border-t border-line">
                  <td className="kc-mono px-4 py-2.5 text-ink">
                    {p.protocol}/{p.host_port}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {p.target_ip ? `${p.target_ip}:${p.target_port}` : `:${p.target_port}`}
                  </td>
                  <td className="px-4 py-2.5">
                    {p.allowed_ips ? (
                      <span className="kc-mono text-sm text-ink-2">{p.allowed_ips}</span>
                    ) : (
                      // 空来源意味着**任何地址都能访问**。用 warning 色而不是
                      // 灰色「—」：这是一个需要用户注意的状态，不是「没填」。
                      <span className="text-sm text-warning">不限制（任何来源均可访问）</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <AppliedBadge applied={p.applied} at={p.last_applied_at} />
                  </td>
                  <td className="px-4 py-2.5 text-right">
                    <button
                      className="text-sm text-danger hover:underline"
                      onClick={() => setDeletePF(p)}
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
        静态地址与「系统信息」里的 IP 可能不一致：前者是我们**期望**的分配，
        后者是从节点**探测到**的实际地址。不一致时以实际地址为准。
        <br />
        端口转发会把虚拟机的服务暴露到外部网络，请确认「允许来源」符合预期。
      </p>

      <NicModal
        // key 让「新增」与「编辑某一块」在使用不同状态时重新挂载：
        // 表单的初始值取自 props，切换目标就该拿到新的初始值。
        key={editNic ? `nic-${editNic.id}` : 'nic-new'}
        open={addingNic || editNic !== null}
        vmID={vmID}
        nic={editNic}
        submitting={submit.isPending}
        onClose={() => {
          setAddingNic(false)
          setEditNic(null)
        }}
        onSubmit={(input) =>
          submit.mutate(() =>
            editNic
              ? netApi.updateInterface(vmID, editNic.id, input)
              : netApi.addInterface(vmID, input),
          )
        }
      />

      <Modal
        open={deleteNic !== null}
        title={`删除网卡 #${deleteNic?.order ?? ''}`}
        description="删除后该网卡的配置与地址都会失效。"
        onClose={() => setDeleteNic(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeleteNic(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={submit.isPending}
              onClick={() => deleteNic && submit.mutate(() => netApi.removeInterface(vmID, deleteNic.id))}
            >
              确认删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          删除的是虚拟机的第 {deleteNic?.order} 块网卡（MAC {deleteNic?.mac ?? '—'}）。
          来宾系统里依赖它的网络配置将失效。
        </p>
      </Modal>

      <BindIPModal
        // 每次打开都重新挂载：关闭再打开不该带着上一次填了一半的地址。
        key={bindingIP ? 'bind-open' : 'bind-closed'}
        open={bindingIP}
        vmID={vmID}
        nics={nics}
        submitting={submit.isPending}
        onClose={() => setBindingIP(false)}
        onSubmit={(input) => submit.mutate(() => netApi.bindStaticIP(vmID, input))}
      />

      <Modal
        open={deleteIP !== null}
        title={`解绑地址 ${deleteIP?.ip ?? ''}`}
        description="解绑后虚拟机将回到由 DHCP 分配地址。"
        onClose={() => setDeleteIP(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeleteIP(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={submit.isPending}
              onClick={() => deleteIP && submit.mutate(() => netApi.unbindStaticIP(vmID, deleteIP.id))}
            >
              确认解绑
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          这只解除控制面的分配关系，不会删除来宾系统里已经写好的网络配置。
        </p>
      </Modal>

      <PortForwardModal
        key={addingPF ? 'pf-open' : 'pf-closed'}
        open={addingPF}
        vmID={vmID}
        usedPorts={pfs.map((p) => `${p.protocol}/${p.host_port}`)}
        submitting={submit.isPending}
        onClose={() => setAddingPF(false)}
        onSubmit={(input) => submit.mutate(() => netApi.addPortForward(vmID, input))}
      />

      <Modal
        open={deletePF !== null}
        title={`删除转发 ${deletePF?.protocol}/${deletePF?.host_port ?? ''}`}
        description="删除后该端口将不再指向这台虚拟机。"
        onClose={() => setDeletePF(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeletePF(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={submit.isPending}
              onClick={() => deletePF && submit.mutate(() => netApi.removePortForward(vmID, deletePF.id))}
            >
              确认删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          删除的是宿主机上的转发规则，虚拟机的服务本身不受影响。
        </p>
      </Modal>
    </div>
  )
}

/** NicModal 新增或编辑网卡。 */
function NicModal({
  open,
  nic,
  submitting,
  onClose,
  onSubmit,
}: {
  open: boolean
  vmID: number
  nic: VMInterface | null
  submitting: boolean
  onClose: () => void
  onSubmit: (input: InterfaceInput) => void
}) {
  // 初始值直接取自 props，切换目标时由调用方通过 `key` 触发重新挂载，
  // 状态自然重置。在渲染期间用 ref 记录「是否已初始化」是反模式：
  // ref 的读写在渲染中是不被允许的，而且它改变不了这一次渲染的结果。
  //
  // 编辑已有网卡时**不允许改序号与 MAC**：序号是来宾里的设备顺序（改了
  // 会让 eth0/eth1 对调），MAC 可能已被来宾按它配置过网络。
  const [model, setModel] = useState<NICModel>(nic?.model ?? 'virtio')
  const [rateLimit, setRateLimit] = useState(String(nic?.rate_limit_mbps ?? 0))
  const [allowed, setAllowed] = useState(nic?.allowed_addresses ?? '')
  const [error, setError] = useState('')

  const limit = Number(rateLimit)
  const ready = Number.isFinite(limit) && limit >= 0

  return (
    <Modal
      open={open}
      title={nic ? `编辑网卡 #${nic.order}` : '新增网卡'}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={!ready}
            loading={submitting}
            onClick={() => {
              if (!ready) {
                setError('限速需要是一个不小于 0 的数字')
                return
              }
              onSubmit({
                model,
                rate_limit_mbps: limit,
                allowed_addresses: allowed,
              })
            }}
          >
            保存
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <label className="flex flex-col gap-1">
          <span className="text-base text-ink">型号</span>
          <select
            className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            value={model}
            onChange={(e) => setModel(e.target.value as NICModel)}
          >
            {NIC_MODEL_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <span className="text-sm text-ink-3">
            装完系统发现没网，原因通常就是这里选了一个来宾没有驱动的型号。
          </span>
        </label>

        <Input
          label="限速上限（Mbps）"
          type="number"
          min={0}
          value={rateLimit}
          onChange={(e) => setRateLimit(e.target.value)}
          hint="0 表示不限速。"
        />

        <Input
          label="允许的源地址（可选）"
          value={allowed}
          placeholder="留空表示不限制"
          onChange={(e) => setAllowed(e.target.value)}
          hint="逗号分隔多个地址，用于防止 IP 欺骗。"
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

/** BindIPModal 绑定静态地址。 */
function BindIPModal({
  open,
  nics,
  submitting,
  onClose,
  onSubmit,
}: {
  open: boolean
  vmID: number
  nics: VMInterface[]
  submitting: boolean
  onClose: () => void
  onSubmit: (input: BindStaticIPInput) => void
}) {
  // 初始值取自当前网卡列表；每次打开由调用方通过 `key` 重新挂载。
  const [ip, setIP] = useState('')
  const [order, setOrder] = useState(nics[0] ? String(nics[0].order) : '')
  const [dhcp, setDHCP] = useState(true)

  return (
    <Modal
      open={open}
      title="绑定静态地址"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={ip.trim() === ''}
            loading={submitting}
            onClick={() =>
              onSubmit({
                ip: ip.trim(),
                interface_order: order === '' ? undefined : Number(order),
                is_dhcp_reservation: dhcp,
              })
            }
          >
            绑定
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="地址"
          value={ip}
          placeholder="例如 10.0.0.20"
          onChange={(e) => setIP(e.target.value)}
          hint="同一节点上不能有两台虚拟机使用同一个地址。"
        />

        <label className="flex flex-col gap-1">
          <span className="text-base text-ink">绑定到网卡</span>
          <select
            className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            value={order}
            onChange={(e) => setOrder(e.target.value)}
          >
            <option value="">不指定</option>
            {nics.map((n) => (
              <option key={n.id} value={n.order}>
                #{n.order}
                {n.is_primary ? '（主网卡）' : ''}
              </option>
            ))}
          </select>
        </label>

        <label className="flex cursor-pointer items-start gap-2.5">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={dhcp}
            onChange={(e) => setDHCP(e.target.checked)}
          />
          <span>
            <span className="block text-base text-ink">通过 DHCP 静态租约下发</span>
            <span className="block text-sm text-ink-3">
              关闭时只记录分配关系，需要你在来宾系统里手工配置地址。
            </span>
          </span>
        </label>
      </div>
    </Modal>
  )
}

/** PortForwardModal 新增端口转发。 */
function PortForwardModal({
  open,
  usedPorts,
  submitting,
  onClose,
  onSubmit,
}: {
  open: boolean
  vmID: number
  usedPorts: string[]
  submitting: boolean
  onClose: () => void
  onSubmit: (input: AddPortForwardInput) => void
}) {
  // 每次打开由调用方通过 `key` 重新挂载，状态自然是初始值。
  const [protocol, setProtocol] = useState<'tcp' | 'udp'>('tcp')
  const [hostPort, setHostPort] = useState('')
  const [targetIP, setTargetIP] = useState('')
  const [targetPort, setTargetPort] = useState('')
  const [allowed, setAllowed] = useState('')
  const [error, setError] = useState('')

  const h = Number(hostPort)
  const t = Number(targetPort)
  const ready =
    Number.isInteger(h) && h >= 1 && h <= 65535 && Number.isInteger(t) && t >= 1 && t <= 65535

  return (
    <Modal
      open={open}
      title="新增端口转发"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={!ready}
            loading={submitting}
            onClick={() => {
              if (!ready) {
                setError('端口需要是 1 到 65535 之间的整数')
                return
              }
              onSubmit({
                protocol,
                host_port: h,
                target_ip: targetIP.trim() || undefined,
                target_port: t,
                allowed_ips: allowed.trim() || undefined,
              })
            }}
          >
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex gap-3">
          <label className="flex flex-1 flex-col gap-1">
            <span className="text-base text-ink">协议</span>
            <select
              className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
              value={protocol}
              onChange={(e) => setProtocol(e.target.value as 'tcp' | 'udp')}
            >
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
            </select>
          </label>
          <Input
            label="宿主机端口"
            type="number"
            min={1}
            max={65535}
            value={hostPort}
            onChange={(e) => setHostPort(e.target.value)}
          />
        </div>

        {usedPorts.length > 0 && (
          <p className="text-sm text-ink-3">
            这台虚拟机已占用：<span className="kc-mono">{usedPorts.join('、')}</span>。
            端口在节点内独占，与其它虚拟机冲突的会被拒绝。
          </p>
        )}

        <Input
          label="目标地址（可选）"
          value={targetIP}
          placeholder="留空则按虚拟机的实际地址"
          onChange={(e) => setTargetIP(e.target.value)}
        />

        <Input
          label="目标端口"
          type="number"
          min={1}
          max={65535}
          value={targetPort}
          onChange={(e) => setTargetPort(e.target.value)}
        />

        <Input
          label="允许的来源（可选）"
          value={allowed}
          placeholder="留空表示不限制"
          onChange={(e) => setAllowed(e.target.value)}
          hint="留空意味着任何地址都能访问这个端口，请确认这是预期的。"
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

/** SnapshotTab 管理虚拟机的快照（F-2-07）。 */
function SnapshotTab({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [restoreTarget, setRestoreTarget] = useState<Snapshot | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Snapshot | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const list = useQuery({
    queryKey: ['vm-snapshots', vmID],
    queryFn: () => snapshotApi.list(vmID),
    // 有快照处于中间状态时持续刷新：创建与恢复都要几十秒到几分钟，
    // 不刷新的话用户会对着「创建中」反复点刷新。
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((s) => s.status === 'creating' || s.status === 'restoring')
        ? 3000
        : false,
  })

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: ['vm-snapshots', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const restore = useMutation({
    mutationFn: (id: number) => snapshotApi.restore(vmID, id),
    onSuccess: (r) => {
      setRestoreTarget(null)
      setError('')
      setNotice(`已提交恢复，任务 #${r.task_id} 正在执行`)
      refresh()
    },
    onError: (err) => {
      setRestoreTarget(null)
      setError(describe(err))
    },
  })

  const remove = useMutation({
    mutationFn: (id: number) => snapshotApi.remove(vmID, id),
    onSuccess: (r) => {
      setDeleteTarget(null)
      setError('')
      setNotice(`已提交删除，任务 #${r.task_id} 正在执行`)
      refresh()
    },
    onError: (err) => {
      setDeleteTarget(null)
      setError(describe(err))
    },
  })

  if (list.isPending) return <PageLoading />
  if (list.isError) return <ErrorBox message={describe(list.error)} />

  const { items, quota, used } = list.data

  return (
    <div className="flex flex-col gap-4">
      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && <ErrorBox message={error} />}

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <div className="flex items-center gap-2">
            <h2 className="text-sm font-medium text-ink-2">快照</h2>
            <span className={used >= quota ? 'text-xs text-warning' : 'text-xs text-ink-3'}>
              {used} / {quota}
            </span>
          </div>
          <Button
            size="sm"
            // 配额用完时直接禁用，而不是让用户填完名字才被拒绝。
            disabled={used >= quota}
            onClick={() => setCreating(true)}
          >
            创建快照
          </Button>
        </div>

        {items.length === 0 ? (
          <EmptyState
            title="没有快照"
            description="快照可以保留某个时刻的磁盘状态，改动出问题时可回滚到它。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">名称</th>
                <th className="px-4 py-2 text-left font-normal">类型</th>
                <th className="px-4 py-2 text-left font-normal">大小</th>
                <th className="px-4 py-2 text-left font-normal">状态</th>
                <th className="px-4 py-2 text-left font-normal">创建时间</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <tr key={s.id} className="border-t border-line">
                  <td className="px-4 py-2.5">
                    <span className="text-ink">{s.name}</span>
                    {/* 当前快照要一眼看出来，否则用户会在恢复后又点一次恢复。 */}
                    {s.is_current && (
                      <span className="ml-2 rounded-pill bg-brand/10 px-1.5 py-0.5 text-xs text-brand">
                        当前状态
                      </span>
                    )}
                    {s.description && (
                      <span className="block text-sm text-ink-3">{s.description}</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {SNAPSHOT_KIND_LABEL[s.kind] ?? s.kind}
                    {s.include_memory && <span className="ml-1 text-xs text-ink-3">含内存</span>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {s.size_bytes > 0 ? formatBytes(s.size_bytes) : '—'}
                  </td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={SNAPSHOT_STATUS_TONE[s.status]}>
                      {SNAPSHOT_STATUS_LABEL[s.status] ?? s.status}
                    </StatusBadge>
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{formatDateTime(s.created_at)}</td>
                  <td className="px-4 py-2.5 text-right">
                    <button
                      className="text-sm text-brand hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={!s.can_restore || restore.isPending}
                      title={restoreDisabledReason(s)}
                      onClick={() => setRestoreTarget(s)}
                    >
                      恢复
                    </button>
                    <button
                      className="ml-3 text-sm text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={!s.can_delete || remove.isPending}
                      title={deleteDisabledReason(s)}
                      onClick={() => setDeleteTarget(s)}
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
        快照类型由系统选择：含内存的用内部快照（可恢复运行现场），运行中且不含
        内存的用外部快照（对持续写入的磁盘更可靠）。
        <br />
        <span className="text-warning">恢复快照会丢弃快照之后的所有磁盘改动</span>
        ，且不可撤销。有子快照或正处于「当前状态」的快照不能删除。
      </p>

      <CreateSnapshotModal
        open={creating}
        vmID={vmID}
        onClose={() => setCreating(false)}
        onCreated={(taskID) => {
          setCreating(false)
          setNotice(`已提交创建，任务 #${taskID} 正在执行`)
          refresh()
        }}
      />

      <Modal
        open={restoreTarget !== null}
        title={`恢复到快照「${restoreTarget?.name ?? ''}」`}
        onClose={() => setRestoreTarget(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setRestoreTarget(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={restore.isPending}
              onClick={() => restoreTarget && restore.mutate(restoreTarget.id)}
            >
              确认恢复
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          <span className="font-medium text-danger">
            该快照之后产生的所有磁盘改动都会丢失，且无法撤销。
          </span>
        </p>
        <p className="mt-2 text-base text-ink-3">
          创建于 {formatDateTime(restoreTarget?.created_at)}
          {restoreTarget?.include_memory
            ? '，包含内存状态，将恢复到当时的运行现场。'
            : '，不含内存，恢复后虚拟机处于关机状态。'}
        </p>
      </Modal>

      <Modal
        open={deleteTarget !== null}
        title={`删除快照「${deleteTarget?.name ?? ''}」`}
        description="删除快照不会影响虚拟机当前的数据，只是失去这个还原点。"
        onClose={() => setDeleteTarget(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeleteTarget(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={remove.isPending}
              onClick={() => deleteTarget && remove.mutate(deleteTarget.id)}
            >
              确认删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          这是一个还原点，删除后无法再用它恢复。虚拟机当前的数据不受影响。
        </p>
      </Modal>
    </div>
  )
}

/**
 * 下面两个函数给出按钮被禁用的原因。
 *
 * 用 title 提示而不是只在界面上写一段通用说明：用户盯着某一个灰掉的按钮时，
 * 需要知道的是「这一个为什么不能点」，而不是「一般来说什么情况下不能点」。
 */
function restoreDisabledReason(s: Snapshot): string {
  if (s.is_current) return '虚拟机当前正运行在这个快照上，无需恢复'
  if (s.status === 'creating') return '快照正在创建中'
  if (s.status === 'restoring') return '正在恢复中'
  if (s.status === 'error') return '该快照创建失败，不能用于恢复'
  if (s.status === 'deleting') return '快照正在删除中'
  return ''
}

function deleteDisabledReason(s: Snapshot): string {
  if (s.has_children) return '该快照存在子快照，请先删除子快照'
  if (s.is_current) return '虚拟机当前正运行在这个快照上，不能删除'
  if (s.status === 'creating') return '快照正在创建中'
  if (s.status === 'deleting') return '快照正在删除中'
  return ''
}

/** CreateSnapshotModal 新建快照。 */
function CreateSnapshotModal({
  open,
  vmID,
  onClose,
  onCreated,
}: {
  open: boolean
  vmID: number
  onClose: () => void
  onCreated: (taskID: number) => void
}) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [includeMemory, setIncludeMemory] = useState(false)
  const [error, setError] = useState('')

  const create = useMutation({
    mutationFn: () => snapshotApi.create(vmID, { name, description, include_memory: includeMemory }),
    onSuccess: (r) => {
      reset()
      onCreated(r.task_id)
    },
    onError: (err) => setError(describe(err)),
  })

  function reset() {
    setName('')
    setDescription('')
    setIncludeMemory(false)
    setError('')
  }

  return (
    <Modal
      open={open}
      title="创建快照"
      onClose={() => {
        reset()
        onClose()
      }}
      footer={
        <>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              reset()
              onClose()
            }}
          >
            取消
          </Button>
          <Button
            size="sm"
            disabled={name.trim() === ''}
            loading={create.isPending}
            onClick={() => create.mutate()}
          >
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="名称"
          value={name}
          maxLength={128}
          placeholder="例如 before-upgrade"
          onChange={(e) => setName(e.target.value)}
        />

        <Input
          label="描述（可选）"
          value={description}
          maxLength={255}
          onChange={(e) => setDescription(e.target.value)}
        />

        <label className="flex cursor-pointer items-start gap-2.5">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={includeMemory}
            onChange={(e) => setIncludeMemory(e.target.checked)}
          />
          <span>
            <span className="block text-base text-ink">同时保存运行状态（含内存）</span>
            <span className="block text-sm text-ink-3">
              开启后可以把虚拟机恢复到按下快照那一刻的运行现场，代价是快照体积
              可能数倍于磁盘本身。关闭则只能恢复到关机状态。
            </span>
          </span>
        </label>

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </Modal>
  )
}

/** EditTab 编辑虚拟机配置（F-2-05）。 */
function EditTab({ vmID, onSaved }: { vmID: number; onSaved: () => void }) {
  const [group, setGroup] = useState('basic')
  const [draft, setDraft] = useState<Record<string, string> | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const form = useQuery({
    queryKey: ['vm-edit-form', vmID],
    queryFn: () => editApi.form(vmID),
  })

  // 表单在**首次渲染时**从接口数据派生一次，之后由本地状态接管（f-2-01 R-012）。
  //
  // 用「渲染时派生」而不是 useEffect + setState：后者表达不出「只取第一次」
  // 这个意图——后台每次刷新都会重新触发 effect，把用户正在输入的内容冲回
  // 原值，而用户看到的是「我打的字自己消失了」，最难排查的一类问题。
  const currentDraft = draft ?? (form.data ? buildDraft(form.data.values) : null)

  const diff = form.data && currentDraft ? computeChanges(form.data, currentDraft) : null
  const dirty = diff !== null && diff.keys.length > 0

  const save = useMutation({
    mutationFn: async () => {
      if (!diff) return
      // 两类修改走不同接口：元数据同步生效，其余配置入队执行。
      // 顺序上先元数据后配置——元数据几乎不会失败，先把它落下来，
      // 万一配置提交失败，用户至少不用重填备注。
      if (diff.hasMetadata) {
        await editApi.updateMetadata(vmID, diff.metadata)
      }
      if (diff.hasConfig) {
        return editApi.updateConfig(vmID, diff.config)
      }
      return undefined
    },
    onSuccess: (result) => {
      setError('')
      setDraft(null) // 重新冻结一次，拿到刚保存后的值
      void form.refetch()
      onSaved()
      setNotice(result ? `已提交配置变更，任务 #${result.task_id} 正在执行` : '已保存')
    },
    onError: (err) => setError(describe(err)),
  })

  if (form.isPending || currentDraft === null) return <PageLoading />
  if (form.isError) return <ErrorBox message={describe(form.error)} />

  const data = form.data
  // 兜底：后端总会下发 groups，但一个空列表不该让整页崩掉。
  const active: EditGroupInfo =
    data.groups.find((g) => g.key === group) ??
    data.groups[0] ?? { key: 'basic', label: '基础配置', planned: false }
  const fields = data.fields.filter((f) => f.group === active.key)
  // 需要关机才能改的项，在当前运行态下不可提交。
  const blockedByStatus = !data.editable_now

  return (
    <div className="flex flex-col gap-4">
      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && <ErrorBox message={error} />}

      {/* 子选项卡由后端下发：新增一个只需要改后端矩阵一处，界面自动跟上。 */}
      <div className="flex flex-wrap gap-1 border-b border-line">
        {data.groups.map((g) => (
          <button
            key={g.key}
            onClick={() => setGroup(g.key)}
            className={[
              'rounded-t-control px-3.5 py-2 text-base transition-colors',
              g.key === active.key
                ? 'border-b-2 border-brand font-medium text-brand'
                : 'border-b-2 border-transparent text-ink-3 hover:text-ink',
            ].join(' ')}
          >
            {g.label}
          </button>
        ))}
      </div>

      {active.planned ? (
        <section className="rounded-card border border-line bg-raised px-4 py-5">
          <p className="text-base font-medium text-ink">{active.label}</p>
          <p className="mt-1.5 text-base text-ink-2">{active.note}</p>
          <p className="mt-3 text-sm text-ink-3">对应需求 F-2-05 / F-2-06。</p>
        </section>
      ) : (
        <section className="rounded-card border border-line">
          <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
            <h2 className="text-sm font-medium text-ink-2">{active.label}</h2>
            <div className="flex items-center gap-3">
              {dirty && <span className="text-sm text-warning">有未保存的修改</span>}
              <Button
                size="sm"
                disabled={!dirty}
                loading={save.isPending}
                onClick={() => save.mutate()}
              >
                保存
              </Button>
            </div>
          </div>

          <div className="flex flex-col gap-4 px-4 py-3.5">
            {fields.length === 0 && (
              <p className="text-base text-ink-3">这个选项卡暂时没有可编辑的项。</p>
            )}
            {fields.map((f) => {
              // 运行态下，需要关机的项禁用输入——但**仍然显示**：
              // 直接隐藏会让用户在关机之后再进来才发现多出几项。
              const blocked = (f.requires_shutdown && blockedByStatus) || f.read_only
              const statusLabel =
                VM_STATUS_LABEL[data.current_status as VmStatus] ?? data.current_status

              // 生效方式与提示合成一句：用户读的是「这一项改了会怎样」，
              // 拆成徽标 + 提示两处反而要来回对照。
              const mode = f.read_only
                ? '由节点上报，不可修改'
                : f.requires_shutdown
                  ? '需关机后修改'
                  : '即时生效，不影响运行'
              const hint = f.requires_shutdown && blockedByStatus && !f.read_only
                ? `当前为${statusLabel}，需关机后才能修改`
                : [mode, f.hint].filter(Boolean).join(' · ')

              return (
                <FieldControl
                  key={f.key}
                  field={f}
                  value={currentDraft[f.key] ?? ''}
                  disabled={blocked || save.isPending}
                  hint={hint}
                  onChange={(v) => setDraft({ ...currentDraft, [f.key]: v })}
                />
              )
            })}
          </div>
        </section>
      )}

      <p className="rounded-card border border-line bg-raised px-4 py-3 text-sm text-ink-3">
        「即时生效」的项只记录在控制面，虚拟机运行中也能改；「需关机」的项要下发到
        节点，关闭电源后才能提交。
        <br />
        保存时**只提交改动过的字段**：等值提交会让一次「改内存」顺带触发一次没有
        理由的重启。
      </p>
    </div>
  )
}

/**
 * FieldControl 按矩阵声明的类型渲染控件。
 *
 * 分支集中在这一处，而不是散在每个字段的渲染里：新增一种控件类型时只需要
 * 在这里加一个 case，其余代码（校验、差异、提交）都不用动。
 */
function FieldControl({
  field,
  value,
  disabled,
  hint,
  onChange,
}: {
  field: EditField
  value: string
  disabled: boolean
  hint: string
  onChange: (v: string) => void
}) {
  if (field.kind === 'boolean') {
    return (
      <label className="flex cursor-pointer items-start gap-2.5">
        <input
          type="checkbox"
          className="mt-0.5"
          checked={value === 'true'}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked ? 'true' : 'false')}
        />
        <span>
          <span className="block text-base text-ink">{field.label}</span>
          <span className={disabled ? 'block text-sm text-warning' : 'block text-sm text-ink-3'}>
            {hint}
          </span>
        </span>
      </label>
    )
  }

  if (field.kind === 'select') {
    return (
      <label className="flex flex-col gap-1">
        <span className="text-base text-ink">{field.label}</span>
        <select
          className={[
            'rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink',
            disabled ? 'cursor-not-allowed opacity-60' : '',
          ].join(' ')}
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        >
          {/* 枚举值来自后端下发的 options，前端不硬编码——
              否则后端新增一个取值时，界面会显示成空白选项。 */}
          {(field.options ?? []).map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <span className={disabled ? 'text-sm text-warning' : 'text-sm text-ink-3'}>{hint}</span>
      </label>
    )
  }

  return (
    <Input
      label={field.label}
      type={field.kind === 'number' ? 'number' : 'text'}
      value={value}
      min={field.min}
      max={field.max}
      disabled={disabled}
      hint={hint}
      onChange={(e) => onChange(e.target.value)}
    />
  )
}

/** buildDraft 把当前值转成表单可编辑的字符串。 */
function buildDraft(values: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(values)) {
    out[k] = v === null || v === undefined ? '' : String(v)
  }
  return out
}

/**
 * computeChanges 算出**真正变化**的字段，并按「是否需要下发」分成两组。
 *
 * 只提交差异而不是整表：整表提交会让一次「改内存」顺带把没动过的 CPU 一起
 * 上报，在需要重启的项上就会触发一次没有理由的重启。
 */
function computeChanges(form: EditForm, draft: Record<string, string>) {
  // 元数据走专门的接口：它同步生效、不入队，与需要下发的配置不是一回事。
  const metadata: { remark?: string; group_name?: string } = {}
  const config: Record<string, unknown> = {}
  const keys: string[] = []
  let hasMetadata = false
  let hasConfig = false

  for (const f of form.fields) {
    if (f.read_only) continue

    const before = toStr(form.values[f.key])
    const after = draft[f.key] ?? ''
    if (before === after) continue

    keys.push(f.key)

    if (!f.requires_node) {
      // 目前只有两项纯元数据，它们有各自的接口。
      if (f.key === 'remark') metadata.remark = after
      else if (f.key === 'group_name') metadata.group_name = after
      else continue
      hasMetadata = true
      continue
    }

    switch (f.kind) {
      case 'number': {
        const n = Number(after)
        if (!Number.isFinite(n)) continue
        config[f.key] = n
        break
      }
      case 'boolean':
        config[f.key] = after === 'true'
        break
      default:
        config[f.key] = after
    }
    hasConfig = true
  }

  return { metadata, config, hasMetadata, hasConfig, keys }
}

function toStr(v: unknown): string {
  return v === null || v === undefined ? '' : String(v)
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
