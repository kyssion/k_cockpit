import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Link } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { templateApi } from '@/api/template'
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
import { CreateVmWizard } from './CreateVmWizard'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatDateTime, relativeTime } from '@/utils/format'
import { VM_STATUS_LABEL, VM_STATUS_TONE } from '@/utils/labels'

const PAGE_SIZE = 20

/** 视图模式与分组方式的偏好键。 */
const VIEW_KEY = 'kc.vm.list.view'
const GROUP_KEY = 'kc.vm.list.group'

type ViewMode = 'table' | 'card'
type GroupKey = 'none' | 'status' | 'template' | 'group'

/** 一个分组。key 为空表示不分组（整页一组、不显示组头）。 */
interface VmGroup {
  key: string
  label: string
  items: VmView[]
}

/**
 * groupItems 按所选维度分组。
 *
 * 分组在**前端**做而不是后端：它只改变当前这一页怎么看，不改变"一共有哪些
 * 机器"。交给后端就要为四种分组各写一套 SQL 与分页语义，而分页与分组叠在
 * 一起时"每组取多少"是个没有正确答案的问题。
 */
function groupItems(
  items: VmView[],
  by: GroupKey,
  templateNames: Map<number, string>,
): VmGroup[] {
  if (by === 'none') return [{ key: '', label: '', items }]

  const buckets = new Map<string, VmGroup>()
  for (const vm of items) {
    let key: string
    let label: string
    switch (by) {
      case 'status':
        key = vm.status
        label = VM_STATUS_LABEL[vm.status] ?? vm.status
        break
      case 'template':
        key = vm.template_id ? String(vm.template_id) : 'none'
        label = vm.template_id
          ? (templateNames.get(vm.template_id) ?? `模板 #${vm.template_id}`)
          : '从零安装'
        break
      default:
        key = vm.group_name || 'ungrouped'
        label = vm.group_name || '未分组'
    }
    const group = buckets.get(key)
    if (group) {
      group.items.push(vm)
    } else {
      buckets.set(key, { key, label, items: [vm] })
    }
  }
  return [...buckets.values()]
}

/** 偏好读写。失败一律回落到默认值：偏好丢了不影响功能，不该因此报错。 */
function readPref<T extends string>(key: string, fallback: T): T {
  try {
    const v = localStorage.getItem(key)
    return (v as T) ?? fallback
  } catch {
    return fallback
  }
}

function writePref(key: string, value: string) {
  try {
    localStorage.setItem(key, value)
  } catch {
    // 隐私模式下 localStorage 可能不可写；偏好存不下不影响使用。
  }
}

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

  // 视图与分组是**偏好**：它们不改变"看到哪些机器"，只改变"怎么看"，
  // 因此跨会话保留——每次进来都要重新切一次的话，这个开关等于没有。
  const [view, setView] = useState<ViewMode>(() => readPref(VIEW_KEY, 'table'))
  const [groupBy, setGroupBy] = useState<GroupKey>(() => readPref(GROUP_KEY, 'none'))
  const [sort, setSort] = useState<{ key: string; desc: boolean }>({ key: '', desc: false })
  // 行内菜单与备注弹窗的目标。用「选中的那台」而不是 ID：菜单需要显示名字，
  // 而只存 ID 会强迫每次渲染都回列表里查一遍。
  const [menuVM, setMenuVM] = useState<VmView | null>(null)
  const [remarkVM, setRemarkVM] = useState<VmView | null>(null)

  function changeView(next: ViewMode) {
    setView(next)
    writePref(VIEW_KEY, next)
  }
  function changeGroup(next: GroupKey) {
    setGroupBy(next)
    writePref(GROUP_KEY, next)
  }
  // 点同一列切换升降序，换列则从头开始（降序优先：看列表通常是先找最大的）。
  function toggleSort(key: string) {
    setSort((prev) => (prev.key === key ? { key, desc: !prev.desc } : { key, desc: true }))
    setPage(1)
    clearSelection()
  }

  // 翻页、搜索时清空选择：选中的项可能已经不在当前页上，留着会让
  // 「N 台已选」与实际看到的对不上——用户会怀疑是不是选错了。
  function clearSelection() {
    setSelected(new Set())
    setBatchResult(null)
    setBatchError('')
  }

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const vms = useQuery({
    queryKey: ['vms', page, search, sort.key, sort.desc],
    queryFn: () =>
      vmApi.list({
        page,
        page_size: PAGE_SIZE,
        keyword: search,
        sort_by: sort.key || undefined,
        order: sort.key ? (sort.desc ? 'desc' : 'asc') : undefined,
      }),
  })
  // 模板名只用于「按模板分组」：列表接口带的是模板 ID，而分组标题要显示
  // 人能认出的名字。不分组时不必查。
  const templates = useQuery({
    queryKey: ['templates'],
    queryFn: () => templateApi.list(),
    enabled: groupBy === 'template',
  })

  // node_id → 名称。虚拟机只存节点 ID，界面上要显示人能认出的名字。
  const nodeNames = new Map((nodes.data ?? []).map((n) => [n.id, n.name]))
  const total = vms.data?.pagination.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE))

  const pageItems = vms.data?.items ?? []
  // 模板名只用于「按模板分组」的组头：列表里带的是 ID，而组头要显示名字。
  const templateNames = new Map((templates.data ?? []).map((t) => [t.id, t.name]))
  // 一页只有 20 条，分组不必缓存：为它写一份依赖数组（还得连带缓存
  // pageItems）比直接算一遍更容易出错，收益却是零。
  const groups = groupItems(pageItems, groupBy, templateNames)
  const selectedOnPage = pageItems.filter((v) => selected.has(v.id))

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

      <form onSubmit={submitSearch} className="flex flex-wrap items-center gap-2">
        <input
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          placeholder="搜索名称"
          className="h-8 w-64 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink placeholder:text-ink-3 focus:outline-none focus-visible:border-brand"
        />
        <Button variant="secondary" size="sm" type="submit">
          搜索
        </Button>

        {/* 视图与分组是**偏好**而不是筛选条件：它们不改变看到哪些机器，
            只改变怎么看。因此单独成组、与搜索区分开。 */}
        <div className="ml-auto flex items-center gap-2">
          <Segmented
            value={view}
            onChange={changeView}
            options={[
              { value: 'table', label: '表格' },
              { value: 'card', label: '卡片' },
            ]}
          />
          <Segmented
            value={groupBy}
            onChange={changeGroup}
            options={[
              { value: 'none', label: '不分组' },
              { value: 'status', label: '按状态' },
              { value: 'template', label: '按模板' },
              { value: 'group', label: '自定义分组' },
            ]}
          />
        </div>
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
          <div className="flex flex-col gap-4">
            {groups.map((g) => (
              <section key={g.key} className="flex flex-col gap-2">
                {/* 组头带数量：跨组多选时用户需要知道这一组里选了几台，
                    而只显示组名会让他回头去数。 */}
                {g.label && (
                  <h2 className="text-base text-ink-3">
                    {g.label}
                    <span className="ml-1.5 text-ink-3">（{g.items.length}）</span>
                  </h2>
                )}

                {view === 'table' ? (
                  <div className="overflow-x-auto rounded-card border border-line">
                    <table className="w-full border-collapse text-base">
                      <thead>
                        <tr className="bg-sunken text-left text-xs text-ink-2">
                          <th className="w-10 px-4 py-2.5 font-medium">
                            <input
                              type="checkbox"
                              aria-label={g.label ? `全选「${g.label}」` : '全选本页'}
                              checked={
                                g.items.length > 0 && g.items.every((v) => selected.has(v.id))
                              }
                              ref={(el) => {
                                if (el) {
                                  const picked = g.items.filter((v) => selected.has(v.id)).length
                                  el.indeterminate = picked > 0 && picked < g.items.length
                                }
                              }}
                              onChange={(e) => {
                                const next = new Set(selected)
                                for (const v of g.items) {
                                  if (e.target.checked) next.add(v.id)
                                  else next.delete(v.id)
                                }
                                setSelected(next)
                                setBatchResult(null)
                              }}
                            />
                          </th>
                          <SortableTh
                            label="名称"
                            sortKey="name"
                            active={sort.key}
                            desc={sort.desc}
                            onSort={toggleSort}
                          />
                          <th className="px-4 py-2.5 font-medium">状态</th>
                          <SortableTh
                            label="配置"
                            sortKey="vcpu"
                            active={sort.key}
                            desc={sort.desc}
                            onSort={toggleSort}
                          />
                          <SortableTh
                            label="IP"
                            sortKey="ip"
                            active={sort.key}
                            desc={sort.desc}
                            onSort={toggleSort}
                          />
                          <th className="px-4 py-2.5 font-medium">节点</th>
                          <SortableTh
                            label="创建时间"
                            sortKey="created_at"
                            active={sort.key}
                            desc={sort.desc}
                            onSort={toggleSort}
                          />
                          <th className="px-4 py-2.5 text-right font-medium">操作</th>
                        </tr>
                      </thead>
                      <tbody>
                        {g.items.map((vm) => (
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
                            onMenu={() => setMenuVM(vm)}
                          />
                        ))}
                      </tbody>
                    </table>
                  </div>
                ) : (
                  <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
                    {g.items.map((vm) => (
                      <VmCard
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
                        onMenu={() => setMenuVM(vm)}
                      />
                    ))}
                  </div>
                )}
              </section>
            ))}
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
                    磁盘会留下并继续占用存储空间；机器移入
                    <Link to="/trash" className="text-brand hover:underline">
                      回收站
                    </Link>
                    ，需要时可以恢复。
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
              <label className="flex cursor-pointer items-start gap-2.5">
                <input
                  type="radio"
                  className="mt-1"
                  name="disk-action"
                  checked={diskAction === 'transfer'}
                  onChange={() => setDiskAction('transfer')}
                />
                <span>
                  <span className="block text-base text-ink">转移到我的存储</span>
                  <span className="block text-sm text-ink-3">
                    磁盘文件搬进「我的存储 - 虚拟磁盘」，可再挂到别的机器或下载。
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

      <CreateVmWizard open={createOpen} onClose={() => setCreateOpen(false)} />

      <RowMenu vm={menuVM} onClose={() => setMenuVM(null)} onRemark={(v) => setRemarkVM(v)} />
      <RemarkModal vm={remarkVM} onClose={() => setRemarkVM(null)} />
    </div>
  )
}

function VmRow({
  vm,
  nodeName,
  selected,
  onToggle,
  onMenu,
}: {
  vm: VmView
  nodeName?: string
  selected: boolean
  onToggle: () => void
  onMenu: () => void
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
      <td className="px-4 py-2.5 text-right">
        {/* 行内只放「更多」：把制作模板、导出、重装、迁移这些低频动作全摆成
            按钮，会让每一行变成一排图标，反而找不到常用的那个。 */}
        <Button variant="ghost" size="sm" onClick={onMenu}>
          更多
        </Button>
      </td>
    </tr>
  )
}

/** 卡片视图。它呈现的是**同一批字段的另一种排布**，不是另一套数据。 */
function VmCard({
  vm,
  nodeName,
  selected,
  onToggle,
  onMenu,
}: {
  vm: VmView
  nodeName?: string
  selected: boolean
  onToggle: () => void
  onMenu: () => void
}) {
  return (
    <div
      className={
        'rounded-card border p-4 ' +
        (selected ? 'border-brand bg-brand/5' : 'border-line hover:border-brand/40')
      }
    >
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <div className="flex items-center gap-1.5">
            <input
              type="checkbox"
              aria-label={`选择 ${vm.name}`}
              checked={selected}
              onChange={onToggle}
            />
            <Link
              to={`/vm/${vm.id}`}
              className="truncate font-medium text-ink hover:text-brand hover:underline"
            >
              {vm.name}
            </Link>
          </div>
          <p className="mt-1 text-xs text-ink-3">{nodeName ?? `#${vm.node_id}`}</p>
        </div>
        <StatusBadge tone={VM_STATUS_TONE[vm.status]}>{VM_STATUS_LABEL[vm.status]}</StatusBadge>
      </div>

      <dl className="mt-3 grid grid-cols-2 gap-y-1 text-sm">
        <dt className="text-ink-3">规格</dt>
        <dd className="kc-nums text-ink-2">
          {vm.vcpu} 核 · {formatMemory(vm.memory_mb)}
        </dd>
        <dt className="text-ink-3">磁盘</dt>
        <dd className="kc-nums text-ink-2">{vm.disk_gb} GB</dd>
        <dt className="text-ink-3">IP</dt>
        <dd className="kc-mono truncate text-ink-2" title={vm.ip_summary}>
          {vm.ip_summary || '—'}
        </dd>
      </dl>

      <div className="mt-3 flex items-center justify-between gap-2">
        <div className="flex items-center gap-2">
          {vm.locked && (
            <span className="rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning">
              已锁定
            </span>
          )}
          {vm.stale && <span className="text-xs text-warning">数据可能陈旧</span>}
        </div>
        <Button variant="ghost" size="sm" onClick={onMenu}>
          更多
        </Button>
      </div>
    </div>
  )
}

/**
 * RowMenu 是行内「更多」菜单。
 *
 * 列出的动作都是**可以直接执行**的；制作模板、导出、重装、迁移这些需要在
 * 上下文里做的，指向详情页而不是在这里堆参数——同一个动作两处实现，迟早
 * 只有一处被修。
 */
function RowMenu({
  vm,
  onClose,
  onRemark,
}: {
  vm: VmView | null
  onClose: () => void
  onRemark: (vm: VmView) => void
}) {
  const queryClient = useQueryClient()
  const [error, setError] = useState('')

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ['vms'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const setLock = useMutation({
    mutationFn: (locked: boolean) => vmApi.setLock(vm?.id ?? 0, locked),
    onSuccess: () => {
      invalidate()
      onClose()
    },
    onError: (e) => setError(describe(e)),
  })

  const remove = useMutation({
    mutationFn: () => vmApi.remove(vm?.id ?? 0, 'keep'),
    onSuccess: () => {
      invalidate()
      onClose()
    },
    onError: (e) => setError(describe(e)),
  })

  return (
    <Modal open={vm !== null} title={vm ? vm.name : ''} onClose={onClose}>
      <div className="flex flex-col gap-1">
        <MenuItem to={`/vm/${vm?.id ?? 0}`}>查看详情</MenuItem>
        {vm?.has_console && (
          <MenuItem to={`/vm/${vm.id}/console`}>打开控制台</MenuItem>
        )}
        <button
          className="rounded-control px-2 py-1.5 text-left text-base text-ink-2 hover:bg-raised"
          onClick={() => {
            if (vm) onRemark(vm)
            onClose()
          }}
        >
          编辑备注
        </button>
        {/* 解锁需要二次验证：请求层会自动弹验证框并重放（f-10-01），
            这里不需要额外处理。 */}
        <button
          className="rounded-control px-2 py-1.5 text-left text-base text-ink-2 hover:bg-raised disabled:text-ink-3"
          disabled={setLock.isPending}
          onClick={() => setLock.mutate(!vm?.locked)}
        >
          {vm?.locked ? '解除锁定' : '锁定这台虚拟机'}
        </button>
        <button
          className="rounded-control px-2 py-1.5 text-left text-base text-danger hover:bg-danger/10 disabled:text-ink-3"
          disabled={vm?.locked || remove.isPending}
          title={vm?.locked ? '已锁定的虚拟机需先解锁' : undefined}
          onClick={() => remove.mutate()}
        >
          删除（移入回收站）
        </button>
      </div>
      {error && <p className="mt-2 text-sm text-danger">{error}</p>}
    </Modal>
  )
}

function MenuItem({ to, children }: { to: string; children: string }) {
  return (
    <Link
      to={to}
      className="rounded-control px-2 py-1.5 text-base text-ink-2 hover:bg-raised hover:text-ink"
    >
      {children}
    </Link>
  )
}

/**
 * RemarkModal 编辑备注。
 *
 * 它与分组一样是**纯控制面元数据**：改它不下发节点，因此不需要关机，
 * 也不必走任务队列。
 */
function RemarkModal({
  vm,
  onClose,
}: {
  vm: VmView | null
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [remark, setRemark] = useState('')
  const [error, setError] = useState('')

  const save = useMutation({
    mutationFn: () => vmApi.updateMetadata(vm?.id ?? 0, { remark: remark.trim() }),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['vms'] })
      onClose()
    },
    onError: (e) => setError(describe(e)),
  })

  return (
    <Modal
      open={vm !== null}
      title="编辑备注"
      description={vm ? `「${vm.name}」的备注只存在于面板，不影响虚拟机本身。` : undefined}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={save.isPending} onClick={() => save.mutate()}>
            保存
          </Button>
        </>
      }
    >
      <Input
        label="备注"
        value={remark}
        onChange={(e) => setRemark(e.target.value)}
        placeholder={vm?.remark || '例如：生产环境 · 数据库'}
        autoFocus
      />
      {error && <p className="mt-2 text-sm text-danger">{error}</p>}
    </Modal>
  )
}

/** 可排序的表头。排序由后端完成——当前页只有 20 条，页内排序没有意义。 */
function SortableTh({
  label,
  sortKey,
  active,
  desc,
  onSort,
}: {
  label: string
  sortKey: string
  active: string
  desc: boolean
  onSort: (key: string) => void
}) {
  const on = active === sortKey
  return (
    <th className="px-4 py-2.5 font-medium">
      <button
        type="button"
        onClick={() => onSort(sortKey)}
        className={on ? 'text-ink' : 'text-ink-2 hover:text-ink'}
      >
        {label}
        <span className="ml-1 text-xs">{on ? (desc ? '↓' : '↑') : '↕'}</span>
      </button>
    </th>
  )
}

/**
 * Segmented 是视图 / 分组切换。
 *
 * 用单选按钮的语义（而不是一排普通按钮）：同一时刻只有一种生效，而把它
 * 做成可多选的样子会让人以为可以同时按状态和模板分组。
 */
function Segmented<T extends string>({
  value,
  onChange,
  options,
}: {
  value: T
  onChange: (v: T) => void
  options: { value: T; label: string }[]
}) {
  return (
    <div className="inline-flex overflow-hidden rounded-control border border-line-strong">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          onClick={() => onChange(o.value)}
          aria-pressed={value === o.value}
          className={
            'px-2.5 py-1 text-sm ' +
            (value === o.value
              ? 'bg-brand/10 text-brand'
              : 'text-ink-2 hover:bg-raised hover:text-ink')
          }
        >
          {o.label}
        </button>
      ))}
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
