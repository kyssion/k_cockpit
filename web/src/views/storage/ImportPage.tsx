/**
 * ImportPage 导入已有磁盘与镜像（F-2-13）。
 *
 * 流程按规格要求做成**先解析预览、再创建**：用户选好文件后先看到「将要创建
 * 的是什么」，确认后才受理。对 OVA 而言那些值来自包内的 OVF 描述；对裸镜像
 * 而言是用户自己填的——预览里会标出每一项的来源，因为「这个 4 核是我选的
 * 还是包里读的」对错误的含义完全不同。
 *
 * **上传是模拟的**：只提交文件名、大小与格式（浏览器能直接读到的元数据），
 * 不传文件内容。界面里如实说明这一点，避免让人以为已经传完了。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  IMPORT_FORMAT_HINT,
  IMPORT_STATUS_LABEL,
  IMPORT_STATUS_TONE,
  importerApi,
  type ImportFormat,
  type ImportPreview,
  type ImportView,
} from '@/api/importer'
import { nodeApi } from '@/api/node'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatBytes, formatDateTime } from '@/utils/format'

export function ImportPage() {
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const [error, setError] = useState('')

  const list = useQuery({
    queryKey: ['imports'],
    queryFn: () => importerApi.list(),
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((i) => i.status === 'pending' || i.status === 'running')
        ? 3000
        : false,
  })

  const remove = useMutation({
    mutationFn: (i: ImportView) => importerApi.remove(i.id),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['imports'] })
      void queryClient.invalidateQueries({ queryKey: ['templates'] })
    },
    onError: (err) => setError(describe(err)),
  })

  if (list.isPending) return <PageLoading />
  const items = list.data?.items ?? []

  return (
    <div className="flex max-w-[880px] flex-col gap-5">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">导入</h1>
          <p className="mt-1 text-base text-ink-3">
            导入已有磁盘或 OVA 包，产出**一个模板**——之后就能用它克隆出多台
            虚拟机，而不是只能开一台。
          </p>
        </div>
        <Button size="sm" onClick={() => setOpen(true)}>
          导入镜像
        </Button>
      </header>

      <p className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3 text-sm text-ink-2">
        当前为**模拟上传**：只提交文件名与大小，不传输文件内容。
        整条链路（受理、格式转换、产出模板、配额记账）与真实流程一致，
        唯一未接通的是「文件怎么传到宿主机」。
      </p>

      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {items.length === 0 ? (
        <EmptyState
          title="还没有导入记录"
          description="支持 qcow2 / raw / vmdk / vhd / vhdx / img / ova，导入时统一转换为 QCOW2。"
        />
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">名称</th>
                <th className="px-4 py-2.5 font-medium">来源</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">时间</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((i) => (
                <tr key={i.id} className="border-t border-line">
                  <td className="px-4 py-2.5 text-ink">{i.name}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    <span className="block text-sm">{i.source_filename}</span>
                    <span className="text-xs text-ink-3">
                      {IMPORT_FORMAT_HINT[i.source_format] ?? i.source_format} ·{' '}
                      {formatBytes(i.source_size_bytes)}
                    </span>
                  </td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={IMPORT_STATUS_TONE[i.status]}>
                      {IMPORT_STATUS_LABEL[i.status]}
                    </StatusBadge>
                    {i.error && <span className="block text-xs text-danger">{i.error}</span>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{formatDateTime(i.created_at)}</td>
                  <td className="px-4 py-2.5">
                    {/* 有产出模板时给出**下一步的入口**：导入的产物是模板，
                        而模板页在另一个地方——不给这一步用户会停在这里
                        不知道接下来做什么。 */}
                    {i.status === 'success' && i.template_id != null ? (
                      <span className="flex gap-2">
                        <a href="/template" className="text-sm text-brand hover:underline">
                          去克隆
                        </a>
                        <button
                          className="text-sm text-danger hover:underline"
                          onClick={() => remove.mutate(i)}
                        >
                          删除记录
                        </button>
                      </span>
                    ) : (
                      <span className="text-xs text-ink-3">
                        {i.status === 'failed' ? '可删除后重试' : '导入完成后可克隆'}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <ImportModal
        open={open}
        onClose={() => setOpen(false)}
        onDone={() => {
          setOpen(false)
          setError('')
          void queryClient.invalidateQueries({ queryKey: ['imports'] })
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(msg) => {
          setOpen(false)
          setError(msg)
        }}
      />
    </div>
  )
}

/** ImportModal 走「选文件 → 解析预览 → 确认」三步。 */
function ImportModal({
  open,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  onClose: () => void
  onDone: () => void
  onError: (message: string) => void
}) {
  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list, enabled: open })

  const [nodeID, setNodeID] = useState(0)
  const [filename, setFilename] = useState('')
  const [sizeBytes, setSizeBytes] = useState(0)
  const [format, setFormat] = useState<ImportFormat | ''>('')
  const [name, setName] = useState('')
  const [preview, setPreview] = useState<ImportPreview | null>(null)
  const [error, setError] = useState('')

  const parse = useMutation({
    mutationFn: () =>
      importerApi.parse({
        node_id: nodeID,
        source_filename: filename,
        source_format: format as ImportFormat,
        source_size_bytes: sizeBytes,
      }),
    onSuccess: (p) => {
      setPreview(p)
      setError('')
    },
    onError: (err) => setError(describe(err)),
  })

  const create = useMutation({
    mutationFn: () =>
      importerApi.create({
        node_id: nodeID,
        name: name.trim(),
        source_filename: filename,
        source_format: format as ImportFormat,
        source_size_bytes: sizeBytes,
        vcpu: preview?.vcpu ?? 2,
        memory_mb: preview?.memory_mb ?? 2048,
        disk_gb: preview?.disk_gb ?? 20,
        os_type: preview?.os_type,
        os_variant: preview?.os_variant,
        preview: preview ?? undefined,
      }),
    onSuccess: () => onDone(),
    onError: (err) => onError(describe(err)),
  })

  /** 选文件：只读元数据，不读取内容。 */
  function pickFile(file: File | undefined) {
    if (!file) return
    setFilename(file.name)
    setSizeBytes(file.size)
    setPreview(null)
    // 扩展名由**后端**识别——用户唯一会认真看的就是它，自己解析容易与
    // 后端的支持列表不一致。
    void importerApi
      .guessFormat(file.name)
      .then((r) => setFormat(r.supported ? (r.format as ImportFormat) : ''))
      .catch(() => setFormat(''))
  }

  const sourceOf = (key: string) =>
    preview?.sources?.[key] === 'ovf' ? '从包内解析' : '需你指定'

  return (
    <Modal
      open={open}
      title="导入镜像"
      description="选择文件后会先解析它的配置，确认无误再受理。导入是异步的长任务，可在任务中心跟踪。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          {preview === null ? (
            <Button
              size="sm"
              disabled={nodeID === 0 || !format || parse.isPending}
              loading={parse.isPending}
              onClick={() => parse.mutate()}
            >
              解析
            </Button>
          ) : (
            <Button
              size="sm"
              disabled={name.trim() === ''}
              loading={create.isPending}
              onClick={() => create.mutate()}
            >
              确认导入
            </Button>
          )}
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">目标节点</label>
          <select
            value={nodeID}
            onChange={(e) => setNodeID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
          >
            <option value={0}>请选择…</option>
            {(nodes.data ?? []).map((n) => (
              <option key={n.id} value={n.id}>
                {n.name}
              </option>
            ))}
          </select>
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">源文件</label>
          <input
            type="file"
            accept=".qcow2,.raw,.vmdk,.vhd,.vhdx,.img,.ova"
            onChange={(e) => pickFile(e.target.files?.[0])}
            className="text-base text-ink-2 file:mr-3 file:rounded-control file:border file:border-line-strong file:bg-sunken file:px-2.5 file:py-1 file:text-sm file:text-ink"
          />
          {filename && (
            <p className="text-xs text-ink-3">
              {filename} · {formatBytes(sizeBytes)}
              {format ? ` · 识别为 ${IMPORT_FORMAT_HINT[format]}` : ' · 无法识别格式'}
            </p>
          )}
        </div>

        {error && (
          <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}

        {preview && (
          <div className="flex flex-col gap-2 rounded-control border border-line px-3 py-2.5">
            <p className="text-base text-ink">配置预览</p>
            <dl className="flex flex-col gap-1 text-sm">
              <Row label="CPU" value={`${preview.vcpu} 核`} source={sourceOf('vcpu')} />
              <Row label="内存" value={`${preview.memory_mb} MB`} source={sourceOf('memory_mb')} />
              <Row label="磁盘" value={`${preview.disk_gb} GB`} source={sourceOf('disk_gb')} />
              {preview.os_type && (
                <Row label="系统" value={preview.os_type} source={sourceOf('os_type')} />
              )}
            </dl>
            {(preview.notes ?? []).map((n) => (
              <p key={n} className="text-xs text-ink-3">
                {n}
              </p>
            ))}
          </div>
        )}

        {preview && (
          <Input
            label="模板名称"
            value={name}
            maxLength={64}
            placeholder="例如：ubuntu-24.04-imported"
            onChange={(e) => setName(e.target.value)}
            hint="导入的产物是一个模板——之后可以用它克隆出多台虚拟机。"
          />
        )}
      </div>
    </Modal>
  )
}

function Row({ label, value, source }: { label: string; value: string; source: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-ink-3">{label}</dt>
      <dd className="text-ink">
        {value}
        <span className="ml-2 text-xs text-ink-3">{source}</span>
      </dd>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
