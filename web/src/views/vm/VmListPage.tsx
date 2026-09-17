import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Link } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { CLONE_MODE_HINT, templateApi, type CloneMode } from '@/api/template'
import {
  vmApi,
  type BatchResult,
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
import { VM_STATUS_LABEL, VM_STATUS_TONE } from '@/utils/labels'

const PAGE_SIZE = 20

export function VmListPage() {
  const queryClient = useQueryClient()
  const [page, setPage] = useState(1)
  const [keyword, setKeyword] = useState('')
  const [search, setSearch] = useState('')
  const [createOpen, setCreateOpen] = useState(false)

  // 批量操作（F-2-01）。选择状态用 Set 而不是数组：判重与删除都是 O(1)，
  // 而列表上的每次勾选都会走一次这两件事。
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [batchResult, setBatchResult] = useState<BatchResult | null>(null)
  const [batchError, setBatchError] = useState('')
  // 批量删除的确认框。它承载两件事：让用户选磁盘处理方式（R-009 不给默认
  // 值），以及提前列出被锁定、将被跳过的那些（R-010 不静默跳过）。
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [diskAction, setDiskAction] = useState<DiskAction>('keep')

  // 翻页、搜索时清空选择：选中的项可能已经不在当前页上，留着会让
  // 「N 台已选」与实际看到的对不上——用户会怀疑是不是选错了。
  function clearSelection() {
    setSelected(new Set())
    setBatchResult(null)
    setBatchError('')
  }

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const vms = useQuery({
    queryKey: ['vms', page, search],
    queryFn: () => vmApi.list({ page, page_size: PAGE_SIZE, keyword: search }),
  })

  // node_id → 名称。虚拟机只存节点 ID，界面上要显示人能认出的名字。
  const nodeNames = new Map((nodes.data ?? []).map((n) => [n.id, n.name]))
  const total = vms.data?.pagination.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE))

  const pageItems = vms.data?.items ?? []
  const selectedOnPage = pageItems.filter((v) => selected.has(v.id))
  const allSelected = pageItems.length > 0 && selectedOnPage.length === pageItems.length
  const someSelected = selectedOnPage.length > 0 && !allSelected

  // 选中项里有多少台正在运行。规格要求操作条上给出这个数字（f-2-01 §3.2）：
  // 「开机」对已运行的机器是空操作，「关机」对已关机的也是——提前知道数量，
  // 用户才能判断这一批里有多少会真正发生变化。
  const runningCount = selectedOnPage.filter((v) => v.status === 'running').length

  // 被锁定的选中项。它们删不掉（F-2-12），因此要在用户点下删除**之前**
  // 就告诉他（f-2-01 R-010：不静默跳过）。
  const lockedSelected = selectedOnPage.filter((v) => v.locked)
  const deletableSelected = selectedOnPage.filter((v) => !v.locked)

  const batch = useMutation({
    mutationFn: (vars: {
      action: PowerAction | 'delete'
      diskAction?: DiskAction
      ids?: number[]
    }) => vmApi.batchAction(vars.ids ?? [...selected], vars.action, vars.diskAction),
    onSuccess: (result) => {
      setBatchError('')
      setBatchResult(result)
      setDeleteOpen(false)
      // 全部成功时清空选择：用户下一步多半是看结果或换个筛选，
      // 留着选中状态会让操作条一直挡在底部。
      if (result.failed === 0) setSelected(new Set())
      void queryClient.invalidateQueries({ queryKey: ['vms'] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => {
      setBatchResult(null)
      setBatchError(describe(err))
    },
  })

  function submitSearch(event: FormEvent) {
    event.preventDefault()
    setPage(1)
    setSearch(keyword)
    clearSelection()
  }

  return (
    <div className="flex flex-col gap-4">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">虚拟机</h1>
          <p className="mt-1 text-base text-ink-3">
            共 {total} 台。状态来自最近一次与虚拟化层对账的结果。
          </p>
        </div>
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          创建虚拟机
        </Button>
      </header>

      <form onSubmit={submitSearch} className="flex gap-2">
        <input
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          placeholder="搜索名称"
          className="h-8 w-64 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink placeholder:text-ink-3 focus:outline-none focus-visible:border-brand"
        />
        <Button variant="secondary" size="sm" type="submit">
          搜索
        </Button>
      </form>

      {vms.isPending && <PageLoading />}

      {vms.isError && (
        <div className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(vms.error)}
        </div>
      )}

      {vms.data && vms.data.items.length === 0 && (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState
            title={search ? '没有匹配的虚拟机' : '还没有虚拟机'}
            description={
              search
                ? '试试其他关键词，或清空搜索条件。'
                : '先接入节点，然后就可以在节点上创建虚拟机。'
            }
            action={
              search ? (
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => {
                    setKeyword('')
                    setSearch('')
                    setPage(1)
                  }}
                >
                  清空搜索
                </Button>
              ) : (
                <Button size="sm" onClick={() => setCreateOpen(true)}>
                  创建第一台虚拟机
                </Button>
              )
            }
          />
        </div>
      )}

      {vms.data && vms.data.items.length > 0 && (
        <>
          <div className="overflow-x-auto rounded-card border border-line">
            <table className="w-full border-collapse text-base">
              <thead>
                <tr className="bg-sunken text-left text-xs text-ink-2">
                  <th className="w-10 px-4 py-2.5 font-medium">
                    <input
                      type="checkbox"
                      aria-label="全选本页"
                      checked={allSelected}
                      ref={(el) => {
                        // 部分选中时显示为「不确定」：这个中间态比「未选中」
                        // 更准确地反映了当前情况。
                        if (el) el.indeterminate = someSelected && !allSelected
                      }}
                      onChange={(e) => {
                        setSelected(
                          e.target.checked
                            ? new Set(vms.data.items.map((v) => v.id))
                            : new Set(),
                        )
                        setBatchResult(null)
                      }}
                    />
                  </th>
                  <th className="px-4 py-2.5 font-medium">名称</th>
                  <th className="px-4 py-2.5 font-medium">状态</th>
                  <th className="px-4 py-2.5 font-medium">配置</th>
                  <th className="px-4 py-2.5 font-medium">IP</th>
                  <th className="px-4 py-2.5 font-medium">节点</th>
                  <th className="px-4 py-2.5 font-medium">创建时间</th>
                </tr>
              </thead>
              <tbody>
                {vms.data.items.map((vm) => (
                  <VmRow
                    key={vm.id}
                    vm={vm}
                    nodeName={nodeNames.get(vm.node_id)}
                    selected={selected.has(vm.id)}
                    onToggle={() => {
                      const next = new Set(selected)
                      if (next.has(vm.id)) next.delete(vm.id)
                      else next.add(vm.id)
                      setSelected(next)
                      setBatchResult(null)
                    }}
                  />
                ))}
              </tbody>
            </table>
          </div>

          {totalPages > 1 && (
            <div className="flex items-center justify-end gap-2 text-base text-ink-2">
              <Button
                variant="secondary"
                size="sm"
                disabled={page <= 1}
                onClick={() => {
                  setPage((p) => p - 1)
                  clearSelection()
                }}
              >
                上一页
              </Button>
              <span className="kc-nums">
                {page} / {totalPages}
              </span>
              <Button
                variant="secondary"
                size="sm"
                disabled={page >= totalPages}
                onClick={() => {
                  setPage((p) => p + 1)
                  clearSelection()
                }}
              >
                下一页
              </Button>
            </div>
          )}
        </>
      )}

      {batchError && (
        <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-base text-danger">
          {batchError}
        </p>
      )}

      {/* 批量结果：区分成功与失败，失败项**逐条给出原因**（f-2-01 边界）。
          笼统地说「部分失败」会迫使 50 台逐个点开排查。 */}
      {batchResult && (
        <section className="rounded-card border border-line">
          <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
            <h2 className="text-sm font-medium text-ink-2">
              批量操作结果：
              <span className="ml-1 text-success">{batchResult.succeeded} 台已提交</span>
              {batchResult.failed > 0 && (
                <span className="ml-1 text-danger">{batchResult.failed} 台失败</span>
              )}
            </h2>
            <button
              className="text-sm text-ink-3 hover:text-ink"
              onClick={() => setBatchResult(null)}
            >
              关闭
            </button>
          </div>
          {batchResult.failed > 0 && (
            <ul className="flex flex-col gap-1.5 px-4 py-3 text-base">
              {batchResult.items
                .filter((i) => !i.ok)
                .map((i) => (
                  <li key={i.vm_id} className="flex gap-2">
                    <span className="kc-mono text-ink-3">#{i.vm_id}</span>
                    <span className="text-danger">{i.error}</span>
                  </li>
                ))}
            </ul>
          )}
        </section>
      )}

      {/* 底部浮出的操作条（f-2-01 §3.2）。固定在视口底部而不是表格下方：
          列表可能很长，操作条跟着滚动的话用户每次都要先滚到底。 */}
      {selected.size > 0 && (
        <div className="fixed bottom-5 left-1/2 z-30 -translate-x-1/2">
          <div className="flex items-center gap-3 rounded-card border border-line-strong bg-surface px-4 py-2.5 shadow-lg">
            <span className="text-base text-ink">
              已选 <span className="kc-nums font-medium">{selected.size}</span> 台
              {runningCount > 0 && (
                <span className="ml-1.5 text-ink-3">· {runningCount} 台运行中</span>
              )}
              {/* 锁定项在**点之前**就要标出来（R-010）：等到点下删除才被
                  拒绝，用户会以为是系统出了问题。 */}
              {lockedSelected.length > 0 && (
                <span className="ml-1.5 text-warning">· {lockedSelected.length} 台已锁定</span>
              )}
            </span>

            <span className="h-4 w-px bg-line" />

            <Button
              size="sm"
              variant="secondary"
              loading={batch.isPending && batch.variables?.action === 'start'}
              onClick={() => batch.mutate({ action: 'start' })}
            >
              开机
            </Button>
            <Button
              size="sm"
              variant="secondary"
              loading={batch.isPending && batch.variables?.action === 'shutdown'}
              onClick={() => batch.mutate({ action: 'shutdown' })}
            >
              关机
            </Button>
            <Button
              size="sm"
              variant="secondary"
              loading={batch.isPending && batch.variables?.action === 'poweroff'}
              onClick={() => batch.mutate({ action: 'poweroff' })}
            >
              强制断电
            </Button>
            <Button
              size="sm"
              variant="danger"
              // 全部被锁时直接禁用：点进去只会看到一个「没有可删除项」的
              // 确认框，那一步没有任何意义。
              disabled={deletableSelected.length === 0}
              title={
                deletableSelected.length === 0
                  ? '选中的虚拟机全部已锁定，需先解锁才能删除'
                  : ''
              }
              onClick={() => setDeleteOpen(true)}
            >
              删除
            </Button>

            <button
              className="text-sm text-ink-3 hover:text-ink"
              onClick={clearSelection}
            >
              取消选择
            </button>
          </div>
        </div>
      )}

      <Modal
        open={deleteOpen}
        title={`删除 ${deletableSelected.length} 台虚拟机`}
        onClose={() => setDeleteOpen(false)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeleteOpen(false)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={batch.isPending && batch.variables?.action === 'delete'}
              onClick={() =>
                batch.mutate({
                  action: 'delete',
                  diskAction,
                  // 只提交未被锁定的那些。被锁的即便发出去也必然失败，
                  // 让用户收到一串注定失败的结果没有意义。
                  ids: deletableSelected.map((v) => v.id),
                })
              }
            >
              确认删除
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-3.5">
          {/* 锁定项**整体提示并列出**（R-010），不静默跳过。
              用户需要知道是哪几台、以及为什么删不掉。 */}
          {lockedSelected.length > 0 && (
            <div className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2.5">
              <p className="text-base font-medium text-warning">
                {lockedSelected.length} 台已锁定，将被跳过
              </p>
              <ul className="mt-1 flex flex-col gap-0.5">
                {lockedSelected.map((v) => (
                  <li key={v.id} className="text-sm text-ink-2">
                    {v.name}
                    {v.lock_reason && <span className="text-ink-3">（{v.lock_reason}）</span>}
                  </li>
                ))}
              </ul>
              <p className="mt-1.5 text-sm text-ink-3">
                锁定用于防止误删，需先在详情页解锁才能删除。
              </p>
            </div>
          )}

          <div>
            <p className="text-base text-ink">磁盘处理方式</p>
            {/* **不给默认值**（R-009）：两种方式的代价完全不同，
                让界面替用户选一个等于把这个决定藏起来。 */}
            <div className="mt-1.5 flex flex-col gap-2">
              <label className="flex cursor-pointer items-start gap-2.5">
                <input
                  type="radio"
                  className="mt-1"
                  name="disk-action"
                  checked={diskAction === 'keep'}
                  onChange={() => setDiskAction('keep')}
                />
                <span>
                  <span className="block text-base text-ink">保留磁盘</span>
                  <span className="block text-sm text-ink-3">
                    磁盘会留下并继续占用存储空间，可在存储页手动清理。
                  </span>
                </span>
              </label>
              <label className="flex cursor-pointer items-start gap-2.5">
                <input
                  type="radio"
                  className="mt-1"
                  name="disk-action"
                  checked={diskAction === 'delete'}
                  onChange={() => setDiskAction('delete')}
                />
                <span>
                  <span className="block text-base text-danger">连同磁盘一起删除</span>
                  <span className="block text-sm text-ink-3">
                    <span className="font-medium text-danger">磁盘上的数据将无法恢复。</span>
                    请确认这几台机器确实不再需要。
                  </span>
                </span>
              </label>
            </div>
          </div>

          <p className="text-sm text-ink-3">
            需要先关机或强制断电才能删除；有任务正在执行的虚拟机会被拒绝并给出原因。
          </p>
        </div>
      </Modal>

      <CreateVmModal open={createOpen} onClose={() => setCreateOpen(false)} />
    </div>
  )
}

function VmRow({
  vm,
  nodeName,
  selected,
  onToggle,
}: {
  vm: VmView
  nodeName?: string
  selected: boolean
  onToggle: () => void
}) {
  return (
    <tr className={selected ? 'border-t border-line bg-brand/5' : 'border-t border-line hover:bg-raised'}>
      <td className="px-4 py-2.5">
        <input
          type="checkbox"
          aria-label={`选择 ${vm.name}`}
          checked={selected}
          onChange={onToggle}
        />
      </td>
      <td className="px-4 py-2.5">
        <Link to={`/vm/${vm.id}`} className="font-medium text-ink hover:text-brand hover:underline">
          {vm.name}
        </Link>
        {/* 锁定标记紧跟在名字后面而不是单独一列：它是对这一行的**状态注解**
            （「这台不能删」），而不是一个需要横向比较的属性。 */}
        {vm.locked && (
          <span
            className="ml-2 rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning"
            title={vm.lock_reason ? `已锁定：${vm.lock_reason}` : '已锁定，需先解锁才能删除'}
          >
            已锁定
          </span>
        )}
        {vm.group_name && <span className="ml-2 text-xs text-ink-3">{vm.group_name}</span>}
      </td>
      <td className="px-4 py-2.5">
        <div className="flex items-center gap-2">
          <StatusBadge tone={VM_STATUS_TONE[vm.status]}>{VM_STATUS_LABEL[vm.status]}</StatusBadge>
          {/* 投影过期时明确提示：把陈旧数据显示成当前状态，排障时比没有数据更危险。 */}
          {vm.stale && (
            <span className="text-xs text-warning" title={`最近对账：${formatDateTime(vm.last_synced_at)}`}>
              数据可能陈旧
            </span>
          )}
        </div>
      </td>
      <td className="kc-nums px-4 py-2.5 text-ink-2">
        {vm.vcpu} 核 · {formatMemory(vm.memory_mb)} · {vm.disk_gb} GB
      </td>
      <td className="kc-mono px-4 py-2.5 text-ink-2">{vm.ip_summary || '—'}</td>
      <td className="px-4 py-2.5 text-ink-2">{nodeName ?? `#${vm.node_id}`}</td>
      <td className="px-4 py-2.5 text-ink-2">{relativeTime(vm.created_at)}</td>
    </tr>
  )
}

function formatMemory(mb: number): string {
  if (mb >= 1024) return `${(mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1)} GB`
  return `${mb} MB`
}

function CreateVmModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [nodeID, setNodeID] = useState(0)
  const [vcpu, setVcpu] = useState(2)
  const [memoryMB, setMemoryMB] = useState(2048)
  const [diskGB, setDiskGB] = useState(20)
  const [error, setError] = useState('')
  const [taskID, setTaskID] = useState<number | null>(null)
  const [templateID, setTemplateID] = useState(0)
  const [cloneMode, setCloneMode] = useState<CloneMode>('full')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })

  // 只列出**目标节点上**可用的模板：模板盘就在它所属节点的存储池里，
  // 跨节点使用需要先导出再导入。把别的节点的模板列出来、选下去再被拒绝，
  // 只会让人以为是自己操作错了。
  const templates = useQuery({
    queryKey: ['templates', { node_id: nodeID, only_ready: true }],
    queryFn: () => templateApi.list({ node_id: nodeID, only_ready: true }),
    enabled: open && nodeID > 0,
  })

  const create = useMutation({
    mutationFn: () =>
      vmApi.create({
        name: name.trim(),
        node_id: nodeID,
        vcpu,
        memory_mb: memoryMB,
        disk_gb: diskGB,
        template_id: templateID > 0 ? templateID : undefined,
        clone_mode: templateID > 0 ? cloneMode : undefined,
      }),
    onSuccess: (result) => {
      setTaskID(result.task_id)
      setError('')
      // 列表要刷新才能看到新虚拟机——创建是异步的，此刻它可能还没出现。
      void queryClient.invalidateQueries({ queryKey: ['vms'] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => setError(describe(err)),
  })

  function handleClose() {
    setName('')
    setNodeID(0)
    setTemplateID(0)
    setCloneMode('full')
    setError('')
    setTaskID(null)
    onClose()
  }

  // 选中模板后把默认规格填进来：那些值取自制备模板时的源虚拟机，是最合理
  // 的起点。**不是**强制——用户可以改，改了就以他填的为准（磁盘另有下限）。
  function pickTemplate(id: number) {
    setTemplateID(id)
    const tpl = (templates.data ?? []).find((t) => t.id === id)
    if (!tpl) return
    if (tpl.default_cpu > 0) setVcpu(tpl.default_cpu)
    if (tpl.default_memory_mb > 0) setMemoryMB(tpl.default_memory_mb)
    if (tpl.min_disk_gb > 0) setDiskGB(tpl.min_disk_gb)
  }

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setError('')
    if (!name.trim()) {
      setError('请输入虚拟机名')
      return
    }
    if (nodeID <= 0) {
      setError('请选择节点')
      return
    }
    create.mutate()
  }

  const availableNodes = nodes.data ?? []

  return (
    <Modal
      open={open}
      title={taskID === null ? '创建虚拟机' : '创建任务已提交'}
      description={
        taskID === null
          ? '创建是异步操作，提交后会生成一个任务，可在任务中心查看进度。'
          : '任务正在执行。任务完成后虚拟机才会出现在列表中。'
      }
      onClose={handleClose}
    >
      {taskID === null && (
        <form onSubmit={handleSubmit} className="flex flex-col gap-4">
          <Input
            label="名称"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="web-01"
            hint="1-63 位字母、数字或连字符，同一节点内不可重名"
            autoFocus
            disabled={create.isPending}
          />

          <div className="flex flex-col gap-1.5">
            <label htmlFor="vm-node" className="text-sm font-medium text-ink-2">
              节点
            </label>
            <select
              id="vm-node"
              value={nodeID}
              onChange={(e) => setNodeID(Number(e.target.value))}
              disabled={create.isPending}
              className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
            >
              <option value={0}>请选择节点</option>
              {availableNodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                  {n.status !== 'online' ? '（离线）' : ''}
                </option>
              ))}
            </select>
            {availableNodes.length === 0 && (
              <p className="text-sm text-ink-3">
                还没有可用节点，请先到
                <Link to="/node" className="mx-1 text-brand hover:underline">
                  节点管理
                </Link>
                接入。
              </p>
            )}
          </div>

          {/* 模板选择。留空 = 从零安装（走安装介质），选模板 = 克隆一份
              现成的系统盘。两条路都在这里，不另开一个「从模板创建」入口——
              对用户来说它们是同一件事：「建一台机器，给我一个系统」。 */}
          {nodeID > 0 && (
            <div className="flex flex-col gap-1.5">
              <label htmlFor="vm-template" className="text-sm font-medium text-ink-2">
                模板（可选）
              </label>
              <select
                id="vm-template"
                value={templateID}
                onChange={(e) => pickTemplate(Number(e.target.value))}
                disabled={create.isPending}
                className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
              >
                <option value={0}>不使用模板（从零安装）</option>
                {(templates.data ?? []).map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name} · {t.default_cpu} 核 {t.default_memory_mb} MB
                  </option>
                ))}
              </select>

              {templateID > 0 && (
                <div className="mt-1 flex flex-col gap-1.5 rounded-control border border-line px-3 py-2.5">
                  {(['full', 'linked'] as const).map((mode) => (
                    <label key={mode} className="flex cursor-pointer items-start gap-2.5">
                      <input
                        type="radio"
                        className="mt-1"
                        name="clone-mode"
                        checked={cloneMode === mode}
                        onChange={() => setCloneMode(mode)}
                      />
                      <span>
                        <span className="block text-base text-ink">
                          {CLONE_MODE_HINT[mode].label}
                        </span>
                        <span className="block text-sm text-ink-3">
                          {CLONE_MODE_HINT[mode].detail}
                        </span>
                      </span>
                    </label>
                  ))}
                </div>
              )}
            </div>
          )}

          <div className="grid grid-cols-3 gap-3">
            <Input
              label="CPU（核）"
              type="number"
              min={1}
              value={vcpu}
              onChange={(e) => setVcpu(Number(e.target.value))}
              disabled={create.isPending}
            />
            <Input
              label="内存（MB）"
              type="number"
              min={128}
              step={128}
              value={memoryMB}
              onChange={(e) => setMemoryMB(Number(e.target.value))}
              disabled={create.isPending}
            />
            <Input
              label="磁盘（GB）"
              type="number"
              min={1}
              value={diskGB}
              onChange={(e) => setDiskGB(Number(e.target.value))}
              disabled={create.isPending}
            />
          </div>

          {error && (
            <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
              {error}
            </p>
          )}

          <div className="flex justify-end gap-2">
            <Button variant="secondary" size="sm" type="button" onClick={handleClose}>
              取消
            </Button>
            <Button size="sm" type="submit" loading={create.isPending}>
              提交创建
            </Button>
          </div>
        </form>
      )}

      {taskID !== null && (
        <div className="flex flex-col gap-4">
          <p className="text-base text-ink-2">
            任务 <span className="kc-mono text-ink">#{taskID}</span> 已提交。
          </p>
          <div className="flex justify-end gap-2">
            <Button variant="secondary" size="sm" onClick={handleClose}>
              留在本页
            </Button>
            <Link to="/task">
              <Button size="sm">查看任务</Button>
            </Link>
          </div>
        </div>
      )}
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
