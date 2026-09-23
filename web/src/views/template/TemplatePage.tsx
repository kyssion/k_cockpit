/**
 * 模板管理页（F-3-01 / F-3-02）。
 *
 * 两条贯穿本页的设计：
 *
 * 1. **制备中与失败分开显示**。前者要等，后者要删掉重建。都说成「不可用」
 *    会让用户在等待一个永远不会就绪的模板，或者反复重试一个已经失败的。
 * 2. **依赖关系必须可见**。链式克隆的虚拟机依赖模板盘；模板能否删除，
 *    完全取决于还有没有这样的依赖。把这条信息藏起来，用户会在删除被拒时
 *    一头雾水。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import {
  DELETE_STRATEGY_LABEL,
  TEMPLATE_EXPORT_STATUS_LABEL,
  TEMPLATE_STATUS_LABEL,
  TEMPLATE_STATUS_TONE,
  templateApi,
  templateMaintainApi,
  templatePreprocessApi,
  TEMPLATE_MAINTAIN_LABEL,
  type DeleteStrategy,
  type TemplateMaintainAction,
  type TemplateExportView,
  type TemplateView,
} from '@/api/template'
import { userStorageApi } from '@/api/userstorage'
import { vmApi, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatBytes } from '@/utils/format'

export function TemplatePage() {
  const queryClient = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [batchTarget, setBatchTarget] = useState<TemplateView | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<TemplateView | null>(null)
  // 删除派生链的策略。存在派生模板时必须显式选一个——两条出路的结果完全
  // 不同，服务端不替用户决定。
  const [strategy, setStrategy] = useState<DeleteStrategy | ''>('')
  // 族视图：按族把各版本收在一起。列表视图用于找模板，族视图用于看
  // "这条链上有几代、谁派生自谁"——这两件事在扁平列表里看不出来。
  const [view, setView] = useState<'list' | 'family'>('list')
  const [maintainTarget, setMaintainTarget] = useState<TemplateView | null>(null)
  const [preprocessTarget, setPreprocessTarget] = useState<TemplateView | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')
  const [exportTarget, setExportTarget] = useState<TemplateView | null>(null)
  // 导入面板：它要选「我的存储」里的模板包，因此是一个独立的弹窗而不是
  // 一个按钮直发请求。
  const [importOpen, setImportOpen] = useState(false)

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const templates = useQuery({ queryKey: ['templates'], queryFn: () => templateApi.list() })
  const exports = useQuery({
    queryKey: ['template-exports'],
    queryFn: () => templateApi.listExports(),
    // 打包是耗时操作，跟着刷才看得到状态推进。
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((e) => e.status === 'pending' || e.status === 'running')
        ? 5000
        : false,
  })

  // 导出与导入都进队列：打包与解包都要读写整块镜像。
  const startExport = useMutation({
    mutationFn: (id: number) => templateApi.exportTemplate(id),
    onSuccess: (r) => {
      setExportTarget(null)
      setError('')
      setNotice(`已提交导出，任务 #${r.task_id} 正在执行`)
      void queryClient.invalidateQueries({ queryKey: ['template-exports'] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => {
      setExportTarget(null)
      setError(describe(err))
    },
  })

  const removeExport = useMutation({
    mutationFn: (id: number) => templateApi.removeExport(id),
    onSuccess: () => {
      setError('')
      setNotice('已提交删除导出包')
      void queryClient.invalidateQueries({ queryKey: ['template-exports'] })
    },
    onError: (err) => setError(describe(err))
  })

  const publish = useMutation({
    mutationFn: (vars: { id: number; published: boolean }) =>
      templateApi.update(vars.id, { published: vars.published }),
    onSuccess: (view) => {
      setError('')
      setNotice(view.published ? '已发布，其他用户可以克隆它了' : '已取消发布')
      void queryClient.invalidateQueries({ queryKey: ['templates'] })
    },
    onError: (err) => {
      setNotice('')
      setError(describe(err))
    },
  })

  // 删除前先拉一次检查结果。**在弹窗打开时**取，而不是在点「确认删除」时
  // ——用户要在这个弹窗里做决定，而这些信息正是决定所需要的。
  const deleteCheck = useQuery({
    queryKey: ['template-delete-preview', deleteTarget?.id ?? 0],
    queryFn: () => templateApi.deletePreview(deleteTarget!.id),
    enabled: deleteTarget !== null,
  })

  const remove = useMutation({
    mutationFn: (vars: { tpl: TemplateView; strategy: DeleteStrategy | '' }) =>
      templateApi.remove(vars.tpl.id, vars.strategy || undefined),
    onSuccess: () => {
      setDeleteTarget(null)
      setError('')
      setNotice('已提交删除，正在下发到节点')
      setTimeout(() => {
        void queryClient.invalidateQueries({ queryKey: ['templates'] })
      }, 2000)
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => {
      setDeleteTarget(null)
      setNotice('')
      // 依赖检查是同步的，因此这里能立刻看到「还有 N 台链式克隆依赖它」。
      setError(describe(err))
    },
  })

  if (nodes.isPending || templates.isPending) return <PageLoading />

  const nodeName = (id: number) => (nodes.data ?? []).find((n) => n.id === id)?.name ?? `#${id}`

  return (
    <div className="flex flex-col gap-5">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold text-ink">模板</h1>
          <p className="mt-1 text-base text-ink-3">
            模板是一块可直接克隆的系统盘。用它创建虚拟机比从零安装快得多，
            而链式克隆几乎不占空间——代价是克隆体会依赖模板盘。
          </p>
        </div>
        <div className="flex items-center gap-2">
          {/*
            视图切换：列表视图用于"找某个模板"，族视图用于"看这条链上有几代、
            谁派生自谁"。后者在扁平列表里看不出来——只能看到一句"派生自 #N"。
          */}
          <Button
            size="sm"
            variant="secondary"
            onClick={() => setView(view === 'list' ? 'family' : 'list')}
          >
            {view === 'list' ? '族视图' : '列表视图'}
          </Button>
          {/* 导入：模板包是跨节点搬运模板的载体——在一个节点导出、在另一个导入。 */}
          <Button size="sm" variant="secondary" onClick={() => setImportOpen(true)}>
            导入模板包
          </Button>
          <Button size="sm" onClick={() => setCreateOpen(true)}>
            从虚拟机创建模板
          </Button>
        </div>
      </header>

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {templates.isError ? (
        <p className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(templates.error)}
        </p>
      ) : (templates.data ?? []).length === 0 ? (
        <EmptyState
          title="还没有模板"
          description="从一台已关机的虚拟机创建模板，之后就能快速克隆出新的虚拟机。"
        />
      ) : view === 'family' ? (
        <FamilyTree
          items={templates.data ?? []}
          onMaintain={(t) => setMaintainTarget(t)}
          onDelete={(t) => {
            setStrategy('')
            setDeleteTarget(t)
          }}
        />
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">名称</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">规格</th>
                <th className="px-4 py-2.5 font-medium">节点</th>
                <th className="px-4 py-2.5 font-medium">可见性</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {(templates.data ?? []).map((t) => (
                <tr key={t.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="px-4 py-2.5">
                    <span className="font-medium text-ink">{t.name}</span>
                    {/* 版本与族：v1/v2/v3 是"第几代"，比一句"派生自 #N"更能
                        说清这条链上发生过什么。 */}
                    <span className="ml-2 text-xs text-ink-3">v{t.version}</span>
                    {t.parent_id != null && (
                      <span className="ml-2 text-xs text-ink-3">派生自 #{t.parent_id}</span>
                    )}
                    {t.remark && <span className="block text-xs text-ink-3">{t.remark}</span>}
                  </td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={TEMPLATE_STATUS_TONE[t.status]}>
                      {TEMPLATE_STATUS_LABEL[t.status]}
                    </StatusBadge>
                    {/* 失败原因直接显示在状态旁边：只显示「制备失败」而不说
                        原因，用户只能靠猜或者去翻日志。 */}
                    {t.error && <span className="block text-xs text-danger">{t.error}</span>}
                  </td>
                  <td className="kc-nums px-4 py-2.5 text-ink-2">
                    {t.default_cpu} 核 · {formatBytes(t.default_memory_mb * 1024 * 1024)}
                    <span className="block text-xs text-ink-3">
                      最小磁盘 {t.min_disk_gb} GB
                    </span>
                    {/* 默认硬件来自源虚拟机。显示出来是因为"克隆出来的机器
                        为什么起不来"往往就差这一行——一台 SATA 的 Windows
                        模板按 VirtIO 克隆，开机就是蓝屏。 */}
                    {hardwareOf(t) && (
                      <span className="block text-xs text-ink-3">{hardwareOf(t)}</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{nodeName(t.node_id)}</td>
                  <td className="px-4 py-2.5">
                    {t.published ? (
                      <span className="text-xs text-success">已发布</span>
                    ) : (
                      <span className="text-xs text-ink-3">私有</span>
                    )}
                    {!t.clone_enabled && (
                      <span className="block text-xs text-warning">已停止提供克隆</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <span className="flex gap-2">
                      <button
                        className="text-sm text-brand hover:underline"
                        disabled={publish.isPending || t.status !== 'ready'}
                        onClick={() => publish.mutate({ id: t.id, published: !t.published })}
                      >
                        {t.published ? '取消发布' : '发布'}
                      </button>
                      {/* 批量创建放在这里而不是新建页：克隆的源头是模板，
                          入口跟着源头走才找得到。 */}
                      <button
                        className="text-sm text-brand hover:underline"
                        disabled={t.status !== 'ready' || !t.clone_enabled}
                        title={
                          !t.clone_enabled
                            ? '该模板已停止提供克隆'
                            : t.status !== 'ready'
                              ? '模板尚未就绪'
                              : undefined
                        }
                        onClick={() => setBatchTarget(t)}
                      >
                        批量创建
                      </button>
                      {/* 导出：把模板打包成 tar.gz，用于在别的节点上导入。 */}
                      <button
                        className="text-sm text-brand hover:underline disabled:text-ink-3 disabled:no-underline"
                        disabled={t.status !== 'ready'}
                        title={t.status !== 'ready' ? '模板尚未就绪，不能导出' : undefined}
                        onClick={() => setExportTarget(t)}
                      >
                        导出
                      </button>
                      <button
                        className="text-sm text-danger hover:underline"
                        onClick={() => {
                          // 每次打开都重置策略：上一次的选择留在那里会让
                          // 用户误以为"还是按上次那样删"。
                          setStrategy('')
                          setDeleteTarget(t)
                        }}
                      >
                        删除
                      </button>
                      {/* 派生链维护：改的是这条链本身，与"删除一个模板"
                          不是同一件事——直接删中间一代会让下游全部失效。 */}
                      <button
                        className="text-sm text-ink-2 hover:underline"
                        onClick={() => setMaintainTarget(t)}
                      >
                        维护
                      </button>
                      {/* 离线预处理：改写镜像内容（装 agent、注入 SSH 公钥、清 machine-id） */}
                      <button
                        className="text-sm text-ink-2 hover:underline"
                        onClick={() => setPreprocessTarget(t)}
                      >
                        预处理
                      </button>
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/*
        导出产物列表。放在模板列表**之后**：它是"模板的另一种形态"，
        而不是模板本身的一部分——模板删掉之后这些包依然存在。
      */}
      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">模板导出包</h2>
          <span className="text-xs text-ink-3">
            在别的节点上「导入模板包」即可把模板搬过去
          </span>
        </div>

        {(exports.data?.items ?? []).length === 0 ? (
          <div className="px-4 py-3 text-base text-ink-3">
            还没有导出包。在模板上点「导出」即可生成一个。
          </div>
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">文件</th>
                <th className="px-4 py-2 text-left font-normal">来源模板</th>
                <th className="px-4 py-2 text-left font-normal">大小</th>
                <th className="px-4 py-2 text-left font-normal">状态</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {(exports.data?.items ?? []).map((e) => (
                <ExportRow
                  key={e.id}
                  item={e}
                  pending={removeExport.isPending}
                  onDelete={() => removeExport.mutate(e.id)}
                />
              ))}
            </tbody>
          </table>
        )}
      </section>

      <PreprocessModal
        template={preprocessTarget}
        onClose={() => setPreprocessTarget(null)}
        onSubmitted={(m) => {
          setPreprocessTarget(null)
          setError('')
          setNotice(m)
          void queryClient.invalidateQueries({ queryKey: ['templates'] })
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(m) => {
          setPreprocessTarget(null)
          setError(m)
        }}
      />

      <MaintainModal
        template={maintainTarget}
        candidates={templates.data ?? []}
        onClose={() => setMaintainTarget(null)}
        onSubmitted={(m) => {
          setMaintainTarget(null)
          setError('')
          setNotice(m)
          void queryClient.invalidateQueries({ queryKey: ['templates'] })
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(m) => {
          setMaintainTarget(null)
          setError(m)
        }}
      />

      <BatchCloneModal
        template={batchTarget}
        onClose={() => setBatchTarget(null)}
        onDone={() => {
          setBatchTarget(null)
          setError('')
          setNotice('批量创建任务已提交')
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(m) => {
          setBatchTarget(null)
          setError(m)
        }}
      />

      <ExportModal
        template={exportTarget}
        pending={startExport.isPending}
        onClose={() => setExportTarget(null)}
        onConfirm={() => exportTarget && startExport.mutate(exportTarget.id)}
      />

      <ImportModal
        open={importOpen}
        nodes={nodes.data ?? []}
        onClose={() => setImportOpen(false)}
        onDone={(m) => {
          setImportOpen(false)
          setError('')
          setNotice(m)
          void queryClient.invalidateQueries({ queryKey: ['templates'] })
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
      />

      <CreateTemplateModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onDone={(msg) => {
          setCreateOpen(false)
          setError('')
          setNotice(msg)
          setTimeout(() => {
            void queryClient.invalidateQueries({ queryKey: ['templates'] })
          }, 2000)
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(msg) => {
          setCreateOpen(false)
          setNotice('')
          setError(msg)
        }}
      />

      <Modal
        open={deleteTarget != null}
        title={`删除模板「${deleteTarget?.name ?? ''}」`}
        description="删除会移除宿主机上的模板盘。下面列出当前会阻止删除的依赖。"
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
              // 链式克隆之类的依赖**直接禁用**：那份清单他刚刚才看过，再让
              // 他点一次收到一个错误等于把清单白给了。派生模板则不同——它
              // 有两条出路（级联 / 提升），必须让用户选一个才能提交。
              disabled={deleteCheck.data?.can_delete === false && strategy === ''}
              onClick={() => deleteTarget && remove.mutate({ tpl: deleteTarget, strategy })}
            >
              确认删除
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-2 text-base text-ink-2">
          <p>
            <span className="text-ink">完整克隆</span>出来的虚拟机不受影响——
            它们已经复制了完整的磁盘。
          </p>
          <p>
            <span className="font-medium text-danger">链式克隆</span>出来的虚拟机
            会失去数据，因为它们的磁盘只是模板之上的一层覆盖——
            这类损坏不会立刻报错，要等到下次开机或读到未缓存的数据块时才暴露。
          </p>

          {deleteCheck.isPending && <p className="text-ink-3">正在检查依赖…</p>}

          {(deleteCheck.data?.blockers ?? []).map((b) => (
            <div
              key={b.kind}
              className="rounded-control border border-danger/40 bg-danger/5 px-3 py-2"
            >
              <p className="text-sm text-danger">
                {b.count} {b.unit}{b.label}依赖它，暂时不能删除
              </p>
              {/* 列出**具体名字**：只说「有 3 台」用户还得自己去找是哪三台。 */}
              {b.names.length > 0 && (
                <p className="mt-1 text-xs text-ink-2">
                  {b.names.join('、')}
                  {b.count > b.names.length ? ` 等 ${b.count} 项` : ''}
                </p>
              )}
              <p className="mt-1 text-xs text-ink-3">{b.fix}</p>

              {/* 派生模板有两条出路，必须由用户选一条：服务端不替他决定，
                  因为"删掉整条链"和"只删这一代"是完全不同的结果。 */}
              {b.kind === 'child_template' && (deleteCheck.data?.strategies ?? []).length > 0 && (
                <div className="mt-2 flex flex-col gap-1.5">
                  {(deleteCheck.data?.strategies ?? []).map((s) => (
                    <label key={s} className="flex cursor-pointer items-start gap-2">
                      <input
                        type="radio"
                        className="mt-1"
                        name="delete-strategy"
                        checked={strategy === s}
                        onChange={() => setStrategy(s)}
                      />
                      <span>
                        <span className="block text-sm text-ink">
                          {DELETE_STRATEGY_LABEL[s]?.label ?? s}
                        </span>
                        <span className="block text-xs text-ink-3">
                          {DELETE_STRATEGY_LABEL[s]?.detail ?? ''}
                        </span>
                      </span>
                    </label>
                  ))}
                </div>
              )}
            </div>
          ))}

          {deleteCheck.data?.can_delete && (
            <p className="rounded-control bg-sunken px-3 py-2 text-sm text-ink-2">
              没有依赖，可以删除。
              {deleteCheck.data.disk_path && (
                <span className="mt-0.5 block text-xs text-ink-3">
                  会释放磁盘：{deleteCheck.data.disk_path}
                </span>
              )}
            </p>
          )}
        </div>
      </Modal>
    </div>
  )
}

/**
 * CreateTemplateModal 从一台已关机的虚拟机制备模板。
 *
 * 源虚拟机列表**只列出关机中的**：制备要求关机（运行中的系统盘在被复制的
 * 同时还在被写入），把运行中的机器列出来、点下去再被拒绝，是最容易让人
 * 烦躁的一种交互——用户会以为是自己哪里填错了。
 */
function CreateTemplateModal({
  open,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  onClose: () => void
  onDone: (message: string) => void
  onError: (message: string) => void
}) {
  const [vmID, setVmID] = useState(0)
  const [name, setName] = useState('')
  const [osType, setOsType] = useState('')
  const [published, setPublished] = useState(false)

  const vms = useQuery({
    queryKey: ['vms', { status: 'stopped', page_size: 100 }],
    queryFn: () => vmApi.list({ status: 'stopped', page_size: 100 }),
    enabled: open,
  })

  const candidates: VmView[] = vms.data?.items ?? []

  const save = useMutation({
    mutationFn: (e: FormEvent) => {
      e.preventDefault()
      return templateApi.createFromVM({
        vm_id: vmID,
        name: name.trim(),
        os_type: osType.trim(),
        published,
      })
    },
    onSuccess: () => onDone('已提交制备，正在复制系统盘'),
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title="从虚拟机创建模板"
      description="会复制源虚拟机的系统盘作为模板。制备完成后即可用于克隆新虚拟机。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            loading={save.isPending}
            disabled={vmID === 0 || name.trim() === ''}
            onClick={() => save.mutate(new Event('submit') as unknown as FormEvent)}
          >
            创建
          </Button>
        </>
      }
    >
      <form className="flex flex-col gap-3" onSubmit={(e) => save.mutate(e)}>
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">源虚拟机（仅列出已关机的）</label>
          <select
            value={vmID}
            onChange={(e) => setVmID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
          >
            <option value={0}>请选择…</option>
            {candidates.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
              </option>
            ))}
          </select>
          {candidates.length === 0 && (
            <p className="text-xs text-ink-3">
              没有已关机的虚拟机。制备模板需要关机——运行中的系统盘在被复制的
              同时还在被写入，做出来的模板会让每一台克隆机开机时都要做磁盘检查。
            </p>
          )}
        </div>

        <Input
          label="模板名称"
          value={name}
          maxLength={64}
          placeholder="例如：ubuntu-24.04-base"
          onChange={(e) => setName(e.target.value)}
          hint="在节点内唯一。规范的做法是带上系统与版本，方便日后辨认。"
        />

        <Input
          label="系统类型（可选）"
          value={osType}
          placeholder="例如：linux"
          onChange={(e) => setOsType(e.target.value)}
          hint="供筛选与展示，也是日后「重装系统」选择模板时的判断依据。"
        />

        <label className="flex cursor-pointer items-start gap-2.5">
          <input
            type="checkbox"
            className="mt-1"
            checked={published}
            onChange={(e) => setPublished(e.target.checked)}
          />
          <span>
            <span className="block text-base text-ink">制备完成后立即发布</span>
            <span className="block text-sm text-ink-3">
              发布后其他用户可以克隆它。不勾选则只有你自己能用——共享应当是
              一个显式动作。
            </span>
          </span>
        </label>
      </form>
    </Modal>
  )
}

/** 默认硬件的一句话摘要。没有采集到就返回空串——那时不显示这一行。 */
function hardwareOf(t: TemplateView): string {
  const parts: string[] = []
  if (t.default_disk_bus) parts.push(`磁盘 ${t.default_disk_bus}`)
  if (t.default_nic_model) parts.push(`网卡 ${t.default_nic_model}`)
  if (t.default_machine_type) parts.push(t.default_machine_type)
  if (t.default_firmware) parts.push(t.default_firmware)
  return parts.join(' · ')
}

function ExportRow({
  item,
  pending,
  onDelete,
}: {
  item: TemplateExportView
  pending: boolean
  onDelete: () => void
}) {
  const done = item.status === 'success'
  return (
    <tr className="border-t border-line">
      <td className="px-4 py-2.5 text-ink">{item.filename}</td>
      <td className="px-4 py-2.5 text-ink-2">{item.template_name}</td>
      <td className="kc-nums px-4 py-2.5 text-ink-2">
        {item.size_bytes > 0 ? formatBytes(item.size_bytes) : '—'}
      </td>
      <td className="px-4 py-2.5">
        <StatusBadge tone={done ? 'success' : item.status === 'failed' ? 'danger' : 'idle'}>
          {TEMPLATE_EXPORT_STATUS_LABEL[item.status] ?? item.status}
        </StatusBadge>
        {item.error && <span className="block text-xs text-danger">{item.error}</span>}
      </td>
      <td className="px-4 py-2.5 text-right">
        {/* 下载用链接而不是按钮：导出包是文件，走浏览器的下载通道即可，
            不必先读进 JS 内存。 */}
        {done && (
          <a
            className="text-sm text-brand hover:underline"
            href={`/api/v1/template-exports/${item.id}/download`}
          >
            下载
          </a>
        )}
        <button
          className="ml-3 text-sm text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
          disabled={pending}
          onClick={onDelete}
        >
          删除
        </button>
      </td>
    </tr>
  )
}

/**
 * ExportModal 确认一次导出。
 *
 * 说明里必须写清"导出的是模板盘本身"：它会被打包成 tar.gz，几十 GB 的
 * 模板要花一段时间，而用户需要先知道自己在等什么。
 */
function ExportModal({
  template,
  pending,
  onClose,
  onConfirm,
}: {
  template: TemplateView | null
  pending: boolean
  onClose: () => void
  onConfirm: () => void
}) {
  return (
    <Modal
      open={template !== null}
      title={`导出模板「${template?.name ?? ''}」`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={pending} onClick={onConfirm}>
            开始导出
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-1.5 text-base text-ink-2">
        <p>
          将把这块模板盘打包成一个 tar.gz 包（约 {template?.disk_size_gb ?? 0} GB 的镜像，
          压缩后通常更小）。
        </p>
        <p className="text-sm text-ink-3">
          导出包会放在「我的存储」里，因此占用你的存储配额。在另一个节点上用
          「导入模板包」即可把它变成那台机器上的模板。
        </p>
      </div>
    </Modal>
  )
}

/**
 * ImportModal 导入一个模板包。
 *
 * 流程强制为**先预览、后导入**：包是别人给的，名字撞了、格式不对、摘要不符
 * 这三类问题都必须在导入之前被看见——导完之后才发现，用户已经等完了整个
 * 解包过程。
 */
function ImportModal({
  open,
  nodes,
  onClose,
  onDone,
}: {
  open: boolean
  nodes: { id: number; name: string }[]
  onClose: () => void
  onDone: (message: string) => void
}) {
  const [nodeID, setNodeID] = useState(0)
  const [fileID, setFileID] = useState(0)
  const [error, setError] = useState('')

  const effectiveNodeID = nodeID || nodes[0]?.id || 0
  const files = useQuery({
    queryKey: ['storage-files', effectiveNodeID, 'template_package'],
    queryFn: () => userStorageApi.listFiles(effectiveNodeID, 'template_package'),
    enabled: open && effectiveNodeID > 0,
  })

  const preview = useQuery({
    queryKey: ['template-import-preview', fileID],
    queryFn: () => templateApi.previewImport(fileID),
    enabled: open && fileID > 0,
  })

  const run = useMutation({
    mutationFn: () => templateApi.importTemplate(fileID),
    onSuccess: (r) => {
      setError('')
      onDone(`已提交导入，任务 #${r.task_id} 正在执行`)
    },
    onError: (err) => setError(describe(err)),
  })

  if (!open) return null

  const items = files.data?.items ?? []

  return (
    <Modal
      open={open}
      title="导入模板包"
      description="选择一个已上传的模板包（.tar.gz），确认内容后再导入。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={fileID === 0 || preview.data?.can_import !== true}
            loading={run.isPending}
            onClick={() => run.mutate()}
          >
            导入
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3 text-base text-ink-2">
        <div className="flex gap-2">
          <div className="flex flex-col gap-1">
            <label className="text-xs text-ink-3">节点</label>
            <select
              value={effectiveNodeID}
              onChange={(e) => {
                setNodeID(Number(e.target.value))
                setFileID(0)
              }}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </select>
          </div>

          <div className="flex flex-1 flex-col gap-1">
            <label className="text-xs text-ink-3">模板包</label>
            <select
              value={fileID}
              onChange={(e) => setFileID(Number(e.target.value))}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value={0}>请选择…</option>
              {items.map((f) => (
                <option key={f.id} value={f.id}>
                  {f.filename}（{formatBytes(f.size_bytes)}）
                </option>
              ))}
            </select>
          </div>
        </div>

        {items.length === 0 && !files.isPending && (
          <p className="text-sm text-ink-3">
            这个节点上还没有模板包。先到「我的存储」上传一个（类别选「模板包」）。
          </p>
        )}

        {preview.data && (
          <div className="flex flex-col gap-1 rounded-control border border-line bg-sunken px-3 py-2 text-sm">
            <span className="text-ink">将导入为</span>
            <span>
              名称 {preview.data.manifest.name}（v{preview.data.manifest.version}）
            </span>
            <span>
              磁盘 {preview.data.manifest.disk_size_gb} GB ·{' '}
              {preview.data.manifest.disk_format}
              {preview.data.manifest.family_name
                ? ` · 族 ${preview.data.manifest.family_name}`
                : ''}
            </span>
            {!preview.data.can_import && (
              <span className="text-danger">{preview.data.reason}</span>
            )}
          </div>
        )}

        {error && <p className="text-base text-danger">{error}</p>}
      </div>
    </Modal>
  )
}

/**
 * MaintainModal 派生链维护（F-3-04）。
 *
 * 四个动作改的是同一条链，因此放在一个弹窗里选，而不是散在四处——它们的
 * 共同点是"会影响这条链上的其它模板"，用户需要先看到这一点再决定做哪个。
 *
 * 每个动作都要勾选确认：它们会改写数据或影响下游，而"我能不能现在做"
 * 只有使用者自己知道。
 */
function MaintainModal({
  template,
  candidates,
  onClose,
  onSubmitted,
  onError,
}: {
  template: TemplateView | null
  candidates: TemplateView[]
  onClose: () => void
  onSubmitted: (msg: string) => void
  onError: (msg: string) => void
}) {
  const [action, setAction] = useState<TemplateMaintainAction>('rebase')
  const [childID, setChildID] = useState(0)
  const [acknowledged, setAcknowledged] = useState(false)
  const [seeded, setSeeded] = useState<number | null>(null)

  if (seeded !== (template?.id ?? null)) {
    setSeeded(template?.id ?? null)
    setAction('rebase')
    setChildID(0)
    setAcknowledged(false)
  }

  const children = candidates.filter((t) => t.parent_id === template?.id)
  const derived = candidates.filter((t) => t.family_id !== undefined && t.family_id === template?.family_id && t.id !== template?.id)

  const run = useMutation({
    mutationFn: () => {
      switch (action) {
        case 'rebase':
          return templateMaintainApi.rebase(template?.id ?? 0, acknowledged)
        case 'flatten':
          return templateMaintainApi.flatten(template?.id ?? 0, acknowledged)
        case 'promote_child':
          return templateMaintainApi.promoteChild(template?.id ?? 0, childID, acknowledged)
        case 'promote_delete':
          return templateMaintainApi.promoteDelete(template?.id ?? 0, acknowledged)
      }
    },
    onSuccess: () => onSubmitted('已提交派生链维护'),
    onError: (err) => onError(describe(err)),
  })

  const ready =
    acknowledged && (action !== 'promote_child' || childID > 0)

  return (
    <Modal
      open={template !== null}
      title={`维护「${template?.name ?? ''}」`}
      description="这些动作改的是模板的派生链，会影响链上的其它模板。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            loading={run.isPending}
            disabled={!ready}
            onClick={() => run.mutate()}
          >
            提交
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {/* 先说清这条链上有什么：没有这一步，用户无法判断"会影响谁"。 */}
        <p className="text-sm text-ink-3">
          同一族共 {derived.length + 1} 个版本，直接派生自它的有 {children.length} 个。
        </p>

        <label className="flex flex-col gap-1">
          <span className="text-sm text-ink-2">动作</span>
          <select
            value={action}
            onChange={(e) => setAction(e.target.value as TemplateMaintainAction)}
            className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            {(Object.keys(TEMPLATE_MAINTAIN_LABEL) as TemplateMaintainAction[]).map((a) => (
              <option key={a} value={a}>
                {TEMPLATE_MAINTAIN_LABEL[a]}
              </option>
            ))}
          </select>
        </label>

        {action === 'promote_child' && (
          <label className="flex flex-col gap-1">
            <span className="text-sm text-ink-2">子模板</span>
            <select
              value={childID}
              onChange={(e) => setChildID(Number(e.target.value))}
              className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value={0}>请选择…</option>
              {children.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}（v{c.version}）
                </option>
              ))}
            </select>
          </label>
        )}

        {action === 'promote_delete' && (
          <p className="rounded-control bg-warning/10 px-3 py-2 text-sm text-warning">
            直接删除这一代会让下游模板全部失效，而节点上的表现是"读某个块时才
            报错"。这个动作先把下游改挂到上级，再删除。
          </p>
        )}

        <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={acknowledged}
            onChange={(e) => setAcknowledged(e.target.checked)}
          />
          <span>我已知悉该动作会影响派生链上的其它模板</span>
        </label>
      </div>
    </Modal>
  )
}

/**
 * FamilyTree 按族把各版本收在一起（F-3-04）。
 *
 * 它回答的是列表视图回答不了的两个问题：**这一族有几个版本**、**谁派生自
 * 谁**。列表里只能看到一句"派生自 #N"，而要看清整条链得逐个点进去。
 *
 * 缩进层级由 parent_id 推出；同一层按 version 排序——版本号才是"第几代"，
 * 而制备时间可能因重试而倒挂。
 */
function FamilyTree({
  items,
  onMaintain,
  onDelete,
}: {
  items: TemplateView[]
  onMaintain: (t: TemplateView) => void
  onDelete: (t: TemplateView) => void
}) {
  const byID = new Map(items.map((t) => [t.id, t]))
  const childrenOf = new Map<number, TemplateView[]>()
  for (const t of items) {
    if (t.parent_id != null && byID.has(t.parent_id)) {
      const list = childrenOf.get(t.parent_id) ?? []
      list.push(t)
      childrenOf.set(t.parent_id, list)
    }
  }
  for (const list of childrenOf.values()) {
    list.sort((a, b) => a.version - b.version || a.id - b.id)
  }

  // 根是"父不在本列表里"的那些：父被删了或不在当前筛选结果里时，剩下的这
  // 一代就是这条链可见的起点。
  const roots = items.filter((t) => !(t.parent_id != null && byID.has(t.parent_id)))
    .sort((a, b) => a.name.localeCompare(b.name))

  return (
    <div className="flex flex-col gap-3">
      {roots.map((root) => (
        <section key={root.id} className="rounded-card border border-line">
          <div className="flex flex-wrap items-baseline justify-between gap-2 border-b border-line px-4 py-2.5">
            <div>
              <span className="text-base font-medium text-ink">{root.name}</span>
              <span className="ml-2 text-xs text-ink-3">
                共 {countDescendants(root.id, childrenOf) + 1} 个版本
              </span>
            </div>
            <span className="text-xs text-ink-3">族 #{root.family_id ?? root.id}</span>
          </div>

          <ul className="px-2 py-2">
            <FamilyNode
              item={root}
              depth={0}
              childrenOf={childrenOf}
              onMaintain={onMaintain}
              onDelete={onDelete}
            />
          </ul>
        </section>
      ))}
    </div>
  )
}

function FamilyNode({
  item,
  depth,
  childrenOf,
  onMaintain,
  onDelete,
}: {
  item: TemplateView
  depth: number
  childrenOf: Map<number, TemplateView[]>
  onMaintain: (t: TemplateView) => void
  onDelete: (t: TemplateView) => void
}) {
  const children = childrenOf.get(item.id) ?? []

  return (
    <li className="flex flex-col">
      <div
        className="flex flex-wrap items-center justify-between gap-2 rounded-control px-2 py-1.5 hover:bg-raised"
        style={{ marginLeft: depth * 16 }}
      >
        <span className="flex flex-wrap items-center gap-2">
          {/* 有子代时给一个连接符：纯缩进在层级深了之后看不出谁是谁的子代。 */}
          {depth > 0 && <span className="text-ink-3">└</span>}
          <span className="text-ink">{item.name}</span>
          <span className="text-xs text-ink-3">v{item.version}</span>
          <StatusBadge tone={TEMPLATE_STATUS_TONE[item.status] ?? 'idle'}>
            {TEMPLATE_STATUS_LABEL[item.status] ?? item.status}
          </StatusBadge>
          <span className="text-xs text-ink-3">{item.disk_size_gb} GB</span>
        </span>

        <span className="flex gap-2">
          <button className="text-sm text-ink-2 hover:underline" onClick={() => onMaintain(item)}>
            维护
          </button>
          <button className="text-sm text-danger hover:underline" onClick={() => onDelete(item)}>
            删除
          </button>
        </span>
      </div>

      {children.length > 0 && (
        <ul>
          {children.map((c) => (
            <FamilyNode
              key={c.id}
              item={c}
              depth={depth + 1}
              childrenOf={childrenOf}
              onMaintain={onMaintain}
              onDelete={onDelete}
            />
          ))}
        </ul>
      )}
    </li>
  )
}

/** countDescendants 统计整棵子树的大小，用于在族标题上写清"有几个版本"。 */
function countDescendants(id: number, childrenOf: Map<number, TemplateView[]>): number {
  let n = 0
  for (const c of childrenOf.get(id) ?? []) {
    n += 1 + countDescendants(c.id, childrenOf)
  }
  return n
}

/**
 * PreprocessModal 离线预处理（F-3-06）。
 *
 * 选项做成开关而不是模式：这几项可以任意组合，而标准 / 完整的表达力不够——每次要做什么取决于那个镜像本身缺什么。
 *
 * reset_machine_id 的后果必须写清：克隆出的机器不重置会在网络里被当成同一台。
 */
function PreprocessModal({
  template,
  onClose,
  onSubmitted,
  onError,
}: {
  template: TemplateView | null
  onClose: () => void
  onSubmitted: (msg: string) => void
  onError: (msg: string) => void
}) {
  const [installAgent, setInstallAgent] = useState(true)
  const [injectSSHKey, setInjectSSHKey] = useState(false)
  const [resetMachineID, setResetMachineID] = useState(true)
  const [removeCloudInit, setRemoveCloudInit] = useState(false)
  const [acknowledged, setAcknowledged] = useState(false)
  const [seeded, setSeeded] = useState<number | null>(null)

  if (seeded !== (template?.id ?? null)) {
    setSeeded(template?.id ?? null)
    setInstallAgent(true)
    setInjectSSHKey(false)
    setResetMachineID(true)
    setRemoveCloudInit(false)
    setAcknowledged(false)
  }

  const run = useMutation({
    mutationFn: () =>
      templatePreprocessApi.run(template?.id ?? 0, {
        install_agent: installAgent,
        inject_ssh_key: injectSSHKey,
        reset_machine_id: resetMachineID,
        remove_cloud_init: removeCloudInit,
        acknowledge: acknowledged,
      }),
    onSuccess: () => onSubmitted('已提交预处理'),
    onError: (err) => onError(describe(err)),
  })

  const anyStep = installAgent || injectSSHKey || resetMachineID || removeCloudInit

  return (
    <Modal
      open={template !== null}
      title={`预处理「${template?.name ?? ''}」`}
      description="这些动作会改写模板镜像的内容，失败时由节点还原。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            loading={run.isPending}
            disabled={!anyStep || !acknowledged}
            onClick={() => run.mutate()}
          >
            提交
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-2.5">
        <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={installAgent}
            onChange={(e) => setInstallAgent(e.target.checked)}
          />
          <span>安装来宾代理（qemu-guest-agent）</span>
        </label>

        <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={injectSSHKey}
            onChange={(e) => setInjectSSHKey(e.target.checked)}
          />
          <span>注入 SSH 公钥</span>
        </label>

        <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={resetMachineID}
            onChange={(e) => setResetMachineID(e.target.checked)}
          />
          <span>
            重置 machine-id
            <span className="block text-xs text-ink-3">
              克隆出来的机器若不重置，它们在网络里会被当成同一台（DHCP 拿到同一个地址）。
            </span>
          </span>
        </label>

        <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={removeCloudInit}
            onChange={(e) => setRemoveCloudInit(e.target.checked)}
          />
          <span>
            清理 cloud-init 残留
            <span className="block text-xs text-ink-3">
              让下次开机重新走初始化。
            </span>
          </span>
        </label>

        <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={acknowledged}
            onChange={(e) => setAcknowledged(e.target.checked)}
          />
          <span>我已知悉这些动作会改写模板镜像</span>
        </label>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}

/**
 * BatchCloneModal 从此模板批量创建虚拟机。
 *
 * 界面上要把三件**只有后端才知道**的事先说给用户：
 *
 *   - **一次最多 5 台**，以及为什么（存储 IO：同时建的台数越多，宿主机上
 *     所有虚拟机都越慢）
 *   - **名称整批占用**：有一个重名就整批拒绝，不会跳过
 *   - **部分失败不回滚**：已建好的那几台不会被撤销
 */
function BatchCloneModal({
  template,
  onClose,
  onDone,
  onError,
}: {
  template: TemplateView | null
  onClose: () => void
  onDone: () => void
  onError: (m: string) => void
}) {
  const [prefix, setPrefix] = useState('')
  const [count, setCount] = useState('2')
  const [vcpu, setVCPU] = useState('')
  const [memory, setMemory] = useState('')
  const [disk, setDisk] = useState('')
  const [result, setResult] = useState<{ message: string } | null>(null)

  const n = Number(count) || 0
  // 与服务端一致的上限。**界面自己也拦一道**是为了在用户输入时给提示，
  // 而不是等他点了提交才收到一个错误——服务端仍然会再校验一次。
  const overLimit = n > 5
  const ready = prefix.trim() !== '' && n > 0 && !overLimit

  const run = useMutation({
    mutationFn: () =>
      templateApi.batchClone({
        name_prefix: prefix.trim(),
        count: n,
        node_id: template?.node_id ?? 0,
        vcpu: Number(vcpu) || 0,
        memory_mb: Number(memory) || 0,
        disk_gb: Number(disk) || 0,
        template_id: template?.id ?? 0,
      }),
    onSuccess: (r) => {
      // **部分失败也要让用户看到**，而不是笼统地报成功或失败。
      setResult({ message: r.message })
    },
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={template !== null}
      title={`从「${template?.name ?? ''}」批量创建`}
      onClose={result ? onDone : onClose}
      footer={
        result ? (
          <Button size="sm" onClick={onDone}>
            知道了
          </Button>
        ) : (
          <>
            <Button variant="secondary" size="sm" onClick={onClose}>
              取消
            </Button>
            <Button size="sm" disabled={!ready} loading={run.isPending} onClick={() => run.mutate()}>
              创建
            </Button>
          </>
        )
      }
    >
      {result ? (
        <p className="text-base text-ink-2">{result.message}</p>
      ) : (
        <div className="flex flex-col gap-3.5">
          <Input
            label="名称前缀"
            value={prefix}
            onChange={(e) => setPrefix(e.target.value)}
            hint="实际名称为「前缀-1」「前缀-2」…… 编号是必须的——不编号的话，克隆几台之后分不清哪台是哪台。这一批名称会先整批检查是否被占用：只要有一个重名就整批拒绝，因为逐台跳过重名会建出带洞的结果（只有 1、3、4 号），而你看不出少了哪一台。"
          />
          <div className="flex flex-col gap-1">
            <Input
              label="台数"
              value={count}
              onChange={(e) => setCount(e.target.value)}
            />
            {overLimit && (
              <p className="text-xs text-warning">
                一次最多 5 台。每台都要完整读一遍父盘再写一份新的，同时进行的台数越多，
                这台宿主机上的存储被占得越久——表现为所有虚拟机的 IO 都变慢，而那时
                很难把它和「我刚才点了克隆」联系起来。需要更多请分批做。
              </p>
            )}
          </div>
          <div className="flex gap-3">
            <Input label="CPU（核）" value={vcpu} placeholder="继承模板" onChange={(e) => setVCPU(e.target.value)} />
            <Input label="内存（MB）" value={memory} placeholder="继承模板" onChange={(e) => setMemory(e.target.value)} />
            <Input label="磁盘（GB）" value={disk} placeholder="继承模板" onChange={(e) => setDisk(e.target.value)} />
          </div>
          <p className="rounded-control bg-sunken px-3 py-2 text-xs text-ink-3">
            留空的项继承模板的配置。这一批任务在队列里依次执行，不会同时打满存储。
          </p>
        </div>
      )}
    </Modal>
  )
}
