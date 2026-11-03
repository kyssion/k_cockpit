/**
 * 虚拟机详情页（F-2-03）。
 *
 * 结构对齐 FRONTEND.md §5.3.3：Hero（状态与电源操作）+ 标签页
 * （系统信息 / 快照管理 / 网络管理 / 目录共享 / 定时任务 / 控制台 / 编辑）。
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

import { useTabStore } from '@/stores/tabs'
import { TagEditor } from './TagEditor'
import { DeleteVmModal } from './DeleteVmModal'
import { MakeIndependentModal } from './MakeIndependentModal'
import { ReinstallModal } from './ReinstallModal'
import { PowerButton, VmHero } from './VmHero'
import { TabBar, type TabKey } from './VmTabs'

import { ShareTab } from './ShareTab'
import { CDROMTab } from './CDROMTab'
import { ConsoleTab } from './ConsoleTab'
import { CredentialSection } from './CredentialSection'
import { DiskTab } from './DiskTab'
import { EditTab } from './EditTab'
import { EventTab } from './EventTab'
import { NetworkTab } from './NetworkTab'
import { PCIeInfoBlock } from './PCIeInfoBlock'
import { ScheduleTab } from './ScheduleTab'
import { SnapshotTab } from './SnapshotTab'
import { SystemTab } from './SystemTab'
import { XMLTab } from './XMLTab'
import { VMMetricsPanel } from '@/views/monitor/MetricsPanel'

import { nodeApi } from '@/api/node'
import { ExportTab } from '@/views/vm/ExportTab'
import { GuestActionsSection } from '@/views/vm/GuestActionsSection'
import { MigrateSection } from '@/views/vm/MigrateSection'
import { isActive, taskApi } from '@/api/task'
import { vmApi, type PowerAction } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Icon } from '@/components/common/Icon'
import { PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { describe } from '@/utils/describe'
import { formatDateTime, formatMemory } from '@/utils/format'
import {
  POWER_ACTION_DANGEROUS,
  POWER_ACTION_LABEL,
  VM_STATUS_PULSE,
  VM_STATUS_LABEL,
  VM_STATUS_TONE,
} from '@/utils/labels'


export function VmDetailPage() {
  const { id } = useParams<{ id: string }>()
  const vmID = Number(id)
  const setTabTitle = useTabStore((s) => s.setTitle)
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  const [tab, setTab] = useState<TabKey>('system')
  const [confirmAction, setConfirmAction] = useState<PowerAction | null>(null)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [lockOpen, setLockOpen] = useState(false)
  const [lockReason, setLockReason] = useState('')
  const [rescueOpen, setRescueOpen] = useState(false)
  const [indepOpen, setIndepOpen] = useState(false)
  const [reinstallOpen, setReinstallOpen] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const tasks = useQuery({
    queryKey: ['tasks', { resource_id: vmID }],
    queryFn: () => taskApi.list({ resource_id: vmID, page_size: 5 }),
    enabled: Number.isFinite(vmID),
    // 实时通道（任务流）会在任务变化时让这个查询失效，这里是断线时的兜底。
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((t) => isActive(t.status)) ? 10000 : false,
  })

  // 电源操作是异步的。详情页停留期间，只要还有在途任务就持续刷新，
  // 让状态变化能被看到——否则用户会盯着一个不动的「运行中」反复点击。
  const hasActiveTask = (tasks.data?.items ?? []).some((t) => isActive(t.status))

  const detail = useQuery({
    queryKey: ['vm', vmID],
    queryFn: () => vmApi.get(vmID),
    enabled: Number.isFinite(vmID),
    refetchInterval: hasActiveTask ? 10000 : false,
  })

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })

  // 标签标题用机器名而不是「虚拟机」：路由只能给出"这是哪一类页面"，而用户
  // 在一排标签里认的是名字。
  //
  // 放在**所有提前返回之前**：Hook 必须在每次渲染里都以同样顺序执行，而
  // 详情页在加载中、出错时会提前返回。
  const vmName = detail.data?.name
  useEffect(() => {
    if (vmName) setTabTitle(`/vm/${vmID}`, vmName)
  }, [vmName, vmID, setTabTitle])

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
        <div className="flex flex-wrap items-start justify-between gap-4">
          <div className="min-w-0">
            <h1 className="text-xl font-semibold text-ink">{vm.name}</h1>
            {/* 元信息行集中在标题下：节点、地址、规格是「这台机器是什么」的
                三件事，原先散在下方几张卡里，核对时得在页面上来回找。 */}
            <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1.5 text-sm text-ink-3">
              <StatusBadge tone={VM_STATUS_TONE[vm.status]} pulse={VM_STATUS_PULSE[vm.status]}>
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
              <span className="flex items-center gap-1">
                <Icon name="node" className="h-3.5 w-3.5" />
                {nodeName ?? `节点 #${vm.node_id}`}
              </span>
              <span className="kc-mono">{vm.ip_summary || '未分配地址'}</span>
              <span className="kc-nums">
                {vm.vcpu} 核 · {formatMemory(vm.memory_mb)} · {vm.disk_gb} GB
              </span>
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
            {/* 分隔：左边是日常操作（电源、控制台），右边是改配置 / 不可逆的
                那几个。原先十来个按钮等宽排成一排，删除和开机看起来一样重。 */}
            <span aria-hidden="true" className="mx-0.5 h-5 w-px self-center bg-line" />
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

      {/* 标签（F-2-16）。放在 Hero 之后、锁定提示之前：它回答
          「这台机器有哪些属性」，是概览的一部分，而锁定提示是对当前
          状态的告警，两者不该混在一起。 */}
      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="mb-2 text-sm text-ink-3">标签</h2>
        <TagEditor vmID={vm.id} />
      </section>

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
          {/* 给一条出路：只说清风险而不给出解决方式，用户能做的是
              「那就别删模板」——而模板管理迟早需要删。 */}
          <p className="mt-2 text-sm text-ink-3">
            如果这个模板需要下线，可以先把这台机器的磁盘合并成独立镜像，
            之后它就不再依赖模板了。
          </p>
          <Button
            size="sm"
            variant="secondary"
            className="mt-2"
            onClick={() => setIndepOpen(true)}
          >
            解除模板依赖
          </Button>
        </div>
      )}

      <MakeIndependentModal
        open={indepOpen}
        vm={vm}
        onClose={() => setIndepOpen(false)}
      />

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
          {/* 登录凭据（G-30）挂在「系统信息」下：它回答的是"这台机器怎么登"，
              与配置摘要属于同一处上下文。 */}
          <CredentialSection vmID={vm.id} />
          <GuestActionsSection vm={vm} />
          {/* 迁移挂在「系统信息」下：它改变的是这台机器**在哪**，
              与「这台机器是什么样」属于同一处上下文。 */}
          {/* PCIe 槽位：热插拔设备需要空闲根端口，用完的表现是"挂载成功
              但设备不出现"，而报错里不会提到槽位。 */}
          <PCIeInfoBlock vmID={vm.id} />
          <MigrateSection vm={vm} />
        </div>
      )}
      {tab === 'network' && <NetworkTab vmID={vm.id} />}
      {tab === 'console' && <ConsoleTab vmID={vm.id} />}

      {/* 固件类型一并传进去：**只有 UEFI 机器才有启动项可修**，而"能不能
          修"这个判断不该由快照页自己再查一次虚拟机。 */}
      {tab === 'snapshot' && <SnapshotTab vmID={vm.id} firmware={vm.firmware ?? ''} />}
      {/* 监控画的是**这台机器实际发生了什么**，与「系统信息」里的配置
          是两回事——配置说它应该有多少内存，监控说它实际用了多少。 */}
      {tab === 'monitor' && (
        <VMMetricsPanel
          vmID={vm.id}
          description="指标由采样器按固定间隔采集。图上断开的地方表示那段时间没采到数据，而不是指标为 0。"
        />
      )}
      {/* 光驱：可以多个。**弹出与移除是两件事**，界面上分开呈现。 */}
      {/* 磁盘：列表来自节点实时探测，换总线要关机、系统盘不可卸载。 */}
      {tab === 'disks' && <DiskTab vmID={vm.id} />}
      {tab === 'cdrom' && <CDROMTab vmID={vm.id} nodeID={vm.node_id} />}
      {tab === 'share' && <ShareTab vmID={vm.id} />}
      {/* 事件：把审计与任务两条流水合成一条，回答"这台机器发生过什么"。 */}
      {tab === 'event' && <EventTab vmID={vm.id} />}
      {tab === 'schedule' && <ScheduleTab vmID={vm.id} />}
      {tab === 'export' && <ExportTab vm={vm} />}
      {/* 只读的 libvirt 定义：排查「面板显示的和实际跑的不是一回事」时
          它是唯一的真相。 */}
      {tab === 'xml' && <XMLTab vmID={vm.id} />}
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






