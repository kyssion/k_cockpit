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
import { VmAlerts } from './VmAlerts'
import { VmDialogs } from './VmDialogs'
import { VmTabsPanel } from './VmTabsPanel'
import { DeleteVmModal } from './DeleteVmModal'
import { MakeIndependentModal } from './MakeIndependentModal'
import { ReinstallModal } from './ReinstallModal'
import { PowerButton, VmHero } from './VmHero'
import { TabBar, type TabKey } from './VmTabs'

import { nodeApi } from '@/api/node'
import { isActive, taskApi } from '@/api/task'
import { vmApi, type PowerAction } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Icon } from '@/components/common/Icon'
import { PageLoading } from '@/components/common/Feedback'
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

      <VmAlerts
        vm={vm}
        onUnlinkTemplate={() => setIndepOpen(true)}
        onPurgeBackup={() => purgeBackup.mutate()}
        purgePending={purgeBackup.isPending}
      />

      <MakeIndependentModal
        open={indepOpen}
        vm={vm}
        onClose={() => setIndepOpen(false)}
      />

      <TabBar value={tab} onChange={setTab} />

      {/* 惰性挂载：条件渲染而非 CSS 隐藏，未选中的页签不会发任何请求。 */}
      <VmTabsPanel
        vm={vm}
        tab={tab}
        tasks={tasks.data?.items ?? []}
        nodeName={nodeName}
        notice={notice}
        error={error}
        onSaved={refresh}
      />

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

      <VmDialogs
        vm={vm}
        confirmAction={confirmAction}
        powerPending={power.isPending}
        onCloseConfirm={() => setConfirmAction(null)}
        onConfirmPower={() => confirmAction && power.mutate(confirmAction)}
        rescueOpen={rescueOpen}
        rescuePending={rescue.isPending}
        onCloseRescue={() => setRescueOpen(false)}
        onEnterRescue={() => rescue.mutate('enter')}
        lockOpen={lockOpen}
        lockPending={setLock.isPending}
        onCloseLock={() => setLockOpen(false)}
        lockReason={lockReason}
        onLockReason={setLockReason}
        onLock={() => setLock.mutate(true)}
      />
    </div>
  )
}
