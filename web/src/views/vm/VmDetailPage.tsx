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

import { useIsAdmin } from '@/stores/session'
import { useTabStore } from '@/stores/tabs'
import { TagEditor } from './TagEditor'

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

import { editApi } from '@/api/edit'
import { nodeApi } from '@/api/node'
import { ExportTab } from '@/views/vm/ExportTab'
import { GuestActionsSection } from '@/views/vm/GuestActionsSection'
import { MigrateSection } from '@/views/vm/MigrateSection'
import { templateApi } from '@/api/template'
import { isActive, taskApi } from '@/api/task'
import {
  vmApi,
  type DiskAction,
  type PowerAction,
  type VmView,
} from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Icon } from '@/components/common/Icon'
import { Meter } from '@/components/common/Meter'
import { PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { describe } from '@/utils/describe'
import { formatDateTime, formatMemory, relativeTime } from '@/utils/format'
import {
  POWER_ACTION_DANGEROUS,
  POWER_ACTION_LABEL,
  VM_STATUS_PULSE,
  VM_STATUS_LABEL,
  VM_STATUS_TONE,
} from '@/utils/labels'

/** 详情页的页签。取值与 FRONTEND.md §5.3.3 的表格一一对应。 */
type TabKey =
  | 'system'
  | 'snapshot'
  | 'network'
  | 'disks'
  | 'monitor'
  | 'cdrom'
  | 'share'
  | 'event'
  | 'schedule'
  | 'export'
  | 'console'
  | 'xml'
  | 'edit'

const TABS: { key: TabKey; label: string }[] = [
  { key: 'system', label: '系统信息' },
  { key: 'snapshot', label: '快照管理' },
  { key: 'network', label: '网络管理' },
  { key: 'disks', label: '磁盘' },
  { key: 'monitor', label: '监控' },
  { key: 'cdrom', label: '光驱' },
  { key: 'share', label: '目录共享' },
  { key: 'schedule', label: '定时任务' },
  { key: 'export', label: '导出' },
  { key: 'console', label: '控制台' },
  { key: 'xml', label: '定义' },
  { key: 'edit', label: '编辑' },
]

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
          // 停机时没有指标，但卡片不该因此空着：把创建时定下的规格摆出来，
          // 那是这张卡在停机状态下唯一诚实且有用的内容。
          <div className="flex flex-col gap-2">
            <p className="text-sm text-ink-3">虚拟机未运行，无实时指标。以下是设定的规格：</p>
            <dl className="flex flex-col gap-2">
              <HeroRow label="vCPU">{vm.vcpu} 核</HeroRow>
              <HeroRow label="内存">{formatMemory(vm.memory_mb)}</HeroRow>
              <HeroRow label="磁盘">{vm.disk_gb} GB</HeroRow>
            </dl>
          </div>
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
            className="block overflow-hidden rounded-control border border-line bg-console"
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
  // 强制删除（兜底）：勾上之后提交走 force-delete。只有管理员能勾——
  // 后端同样只放管理员，这里先藏掉是为了一般用户不必看到一个永远用不上
  // 的危险选项。
  const [force, setForce] = useState(false)
  const isAdmin = useIsAdmin()

  const remove = useMutation({
    mutationFn: () =>
      force
        ? vmApi.forceRemove(vmID, diskAction as DiskAction)
        : vmApi.remove(vmID, diskAction as DiskAction),
    onSuccess: () => {
      setDiskAction('')
      setForce(false)
      setError('')
      onDeleted()
    },
    onError: (err) => setError(describe(err)),
  })

  function handleClose() {
    setDiskAction('')
    setForce(false)
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

        <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-line-strong px-3 py-2.5 hover:bg-sunken">
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

        <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-line-strong px-3 py-2.5 hover:bg-sunken">
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

        <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-line-strong px-3 py-2.5 hover:bg-sunken">
          <input
            type="radio"
            name="disk_action"
            className="mt-0.5"
            checked={diskAction === 'transfer'}
            onChange={() => setDiskAction('transfer')}
          />
          <span>
            <span className="block text-base text-ink">转移到我的存储</span>
            <span className="block text-sm text-ink-3">
              磁盘文件会被搬进「我的存储 - 虚拟磁盘」，之后可以挂到别的机器上，
              或下载下来。虚拟机本身的记录仍移入回收站。
            </span>
          </span>
        </label>

        {isAdmin && (
          <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-warning/40 bg-warning/5 px-3 py-2.5">
            <input
              type="checkbox"
              className="mt-1"
              checked={force}
              onChange={(e) => setForce(e.target.checked)}
            />
            <span>
              <span className="block text-base text-warning">强制删除（兜底）</span>
              <span className="block text-sm text-ink-3">
                状态探测不出、常规删除失败的机器才需要勾选：节点会跳过常规清理，
                直接删除域定义并重启 libvirt——同节点上其它虚拟机会经历一次服务重启。
                需要二次验证。
              </span>
            </span>
          </label>
        )}

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </fieldset>
    </Modal>
  )
}

/**
 * MakeIndependentModal 把链接克隆的磁盘合并为独立盘。
 *
 * 这个操作**本身没有可选项**（不像扩容要填数字），因此弹窗的职责只有两件：
 * 说清它要花什么代价（时间与空间），以及说清做完之后**能做什么之前做不到的
 * 事**——删掉那个模板。
 */
function MakeIndependentModal({
  open,
  vm,
  onClose,
}: {
  open: boolean
  vm: VmView
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [error, setError] = useState('')

  const running = vm.status === 'running'

  const run = useMutation({
    mutationFn: () => vmApi.makeDisksIndependent(vm.id),
    onSuccess: () => {
      setError('')
      onClose()
      void queryClient.invalidateQueries({ queryKey: ['vm', vm.id] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (e) => setError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title="解除模板依赖"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={running}
            loading={run.isPending}
            onClick={() => run.mutate()}
          >
            开始合并
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <p className="text-base text-ink-2">
          这一步会把这台机器的磁盘从「模板之上的一层覆盖」合并成一个独立的完整镜像。合并之后它不再依赖任何模板，那个模板也就可以安全删除了。
        </p>

        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
          代价有两项，都取决于磁盘有多大：
          <span className="mt-1 block">· 需要时间——要把整个镜像复制一遍，期间机器保持关机</span>
          <span className="block">· 需要空间——合并后会占用一个完整镜像的空间，而不是现在的增量大小</span>
        </p>

        {/* 运行中**直接禁用**并说明原因，而不是等后端拒绝——那种报错会让
            人以为是自己哪里操作错了。 */}
        {running && (
          <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            这台虚拟机正在运行，需要先关机。合并要复制整个镜像，而运行中的机器
            还在往那层覆盖里写——复制出来的内容会与任何一个时刻都对不上。
          </p>
        )}

        {error && (
          <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </Modal>
  )
}
