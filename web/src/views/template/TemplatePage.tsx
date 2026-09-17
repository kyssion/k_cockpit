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
  TEMPLATE_STATUS_LABEL,
  TEMPLATE_STATUS_TONE,
  templateApi,
  type TemplateView,
} from '@/api/template'
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
  const [deleteTarget, setDeleteTarget] = useState<TemplateView | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const templates = useQuery({ queryKey: ['templates'], queryFn: () => templateApi.list() })

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

  const remove = useMutation({
    mutationFn: (tpl: TemplateView) => templateApi.remove(tpl.id),
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
          <h1 className="text-lg font-semibold text-ink">模板</h1>
          <p className="mt-1 text-base text-ink-3">
            模板是一块可直接克隆的系统盘。用它创建虚拟机比从零安装快得多，
            而链式克隆几乎不占空间——代价是克隆体会依赖模板盘。
          </p>
        </div>
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          从虚拟机创建模板
        </Button>
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
                <tr key={t.id} className="border-t border-line">
                  <td className="px-4 py-2.5">
                    <span className="font-medium text-ink">{t.name}</span>
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
                      <button
                        className="text-sm text-danger hover:underline"
                        onClick={() => setDeleteTarget(t)}
                      >
                        删除
                      </button>
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

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
        description="删除会移除宿主机上的模板盘。仍有链式克隆依赖它时会被拒绝，并告诉你还有几台。"
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
              onClick={() => deleteTarget && remove.mutate(deleteTarget)}
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

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
