/**
 * MyStoragePage 是「我的存储」（F-5-03/04/05）。
 *
 * 上传的实现要点：
 *
 *   - **先算整个文件的 sha256**，让服务端有机会命中秒传。对一个几百 MB 的
 *     镜像来说，本地算摘要是几百毫秒，而省下的是几分钟的传输——这笔账
 *     在文件够大时永远划算。小文件则几乎无感。
 *   - **分片串行上传**，每片传完就登记。串行而不是并发：并发会让服务端
 *     的 bitmap 更新竞争，而登记本身很轻，瓶颈在网络带宽而不是请求数。
 *   - **中断可续传**：`missing_chunks` 给的是序号列表，重连时只补缺的
 *     那几片，而不是从头再来。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import {
  CATEGORY_LABEL,
  CHUNK_SIZE,
  formatBytes,
  sha256Hex,
  userStorageApi,
  type FileCategory,
  type FileView,
} from '@/api/userstorage'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { relativeTime } from '@/utils/format'

export function MyStoragePage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [category, setCategory] = useState<FileCategory | ''>('')
  const [uploadOpen, setUploadOpen] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const storage = useQuery({
    queryKey: ['my-storage', effectiveNodeID],
    queryFn: () => userStorageApi.get(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const files = useQuery({
    queryKey: ['my-storage-files', effectiveNodeID, category],
    queryFn: () => userStorageApi.listFiles(effectiveNodeID, category || undefined),
    // 未开通时不必查文件。
    enabled: effectiveNodeID > 0 && storage.data?.enabled === true,
  })

  const ensure = useMutation({
    mutationFn: () => userStorageApi.ensure(effectiveNodeID),
    onSuccess: () => {
      setError('')
      setNotice('存储空间已开通')
      void queryClient.invalidateQueries({ queryKey: ['my-storage'] })
      void queryClient.invalidateQueries({ queryKey: ['my-storage-files'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const remove = useMutation({
    mutationFn: (file: FileView) => userStorageApi.removeFile(effectiveNodeID, file.id),
    onSuccess: () => {
      setError('')
      setNotice('文件已删除')
      void queryClient.invalidateQueries({ queryKey: ['my-storage-files'] })
    },
    onError: (err) => setError(describe(err)),
  })

  if (nodes.isPending || storage.isPending) return <PageLoading />

  const nodeSelector = (
    <div className="flex flex-col gap-1">
      <label className="text-xs text-ink-3">节点</label>
      <select
        value={effectiveNodeID}
        onChange={(e) => {
          setNodeID(Number(e.target.value))
          setCategory('')
        }}
        className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
      >
        {(nodes.data ?? []).map((n) => (
          <option key={n.id} value={n.id}>
            {n.name}
          </option>
        ))}
      </select>
    </div>
  )

  // 未开通时不展示空的文件列表，而是明确引导开通——「列表为空」与
  // 「你没开通」看起来一样，而前者会让人以为自己没有文件。
  if (!storage.data?.enabled) {
    return (
      <div className="flex flex-col gap-5">
        <header className="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h1 className="text-lg font-semibold text-ink">我的存储</h1>
            <p className="mt-1 text-base text-ink-3">
              存储空间按<span className="text-ink-2">节点</span>开通——磁盘就在那台宿主机上，你在 A 节点的文件与 B 节点无关。
            </p>
          </div>
          {nodeSelector}
        </header>

        {error && (
          <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
            {error}
          </p>
        )}

        <div className="rounded-card border border-line bg-surface p-6 text-center">
          <p className="text-base text-ink">这个节点上还没有你的存储空间</p>
          <p className="mt-1 text-sm text-ink-3">
            开通后可以上传 ISO 镜像、文件与虚拟磁盘，然后把它们挂到你的虚拟机上。
          </p>
          <Button size="sm" className="mt-4" loading={ensure.isPending} onClick={() => ensure.mutate()}>
            开通存储空间
          </Button>
        </div>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">我的存储</h1>
          <p className="mt-1 text-base text-ink-3">
            上传的文件会计入你的存储配额。相同内容的文件会自动<span className="text-ink-2">秒传</span>——不必重复传输。
          </p>
        </div>
        <div className="flex items-end gap-2">
          {nodeSelector}
          <Button size="sm" onClick={() => setUploadOpen(true)}>
            上传文件
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

      <div className="flex flex-wrap gap-2">
        {(['', 'iso', 'share', 'disk'] as const).map((c) => (
          <button
            key={c || 'all'}
            onClick={() => setCategory(c)}
            className={`rounded-pill px-3 py-1 text-sm ${
              category === c ? 'bg-brand/10 text-brand' : 'text-ink-2 hover:text-ink'
            }`}
          >
            {c === '' ? '全部' : CATEGORY_LABEL[c]}
          </button>
        ))}
      </div>

      {files.isPending ? (
        <PageLoading />
      ) : (files.data?.items ?? []).length === 0 ? (
        <EmptyState
          title="还没有文件"
          description="上传 ISO 镜像后可以挂到虚拟机的光驱，虚拟磁盘可以作为数据盘。"
        />
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">文件名</th>
                <th className="px-4 py-2.5 font-medium">类别</th>
                <th className="px-4 py-2.5 font-medium">大小</th>
                <th className="px-4 py-2.5 font-medium">上传时间</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {(files.data?.items ?? []).map((f) => (
                <tr key={f.id} className="border-t border-line">
                  <td className="px-4 py-2.5">
                    <span className="text-ink">{f.filename}</span>
                    {/* ISO 的识别结果就显示在文件名下面：它是「用这个镜像建机器」
                        时唯一需要提前知道的参数，而 MinDiskGB 尤其重要——它让
                        磁盘过小的配置能被提前拦住，而不是等安装到一半才失败。 */}
                    {f.os_type && (
                      <span className="block text-xs text-ink-3">
                        {f.os_type}
                        {f.os_variant && ` · ${f.os_variant}`}
                        {f.min_disk_gb != null && f.min_disk_gb > 0 &&
                          ` · 至少 ${f.min_disk_gb} GB 磁盘`}
                      </span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{CATEGORY_LABEL[f.category]}</td>
                  <td className="kc-nums px-4 py-2.5 text-ink-2">{formatBytes(f.size_bytes)}</td>
                  <td className="px-4 py-2.5 text-ink-3">
                    {f.uploaded_at ? relativeTime(f.uploaded_at) : '—'}
                  </td>
                  <td className="px-4 py-2.5">
                    <button
                      className="text-sm text-danger hover:underline"
                      disabled={remove.isPending}
                      onClick={() => remove.mutate(f)}
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <UploadModal
        open={uploadOpen}
        nodeID={effectiveNodeID}
        onClose={() => setUploadOpen(false)}
        onDone={(message) => {
          setUploadOpen(false)
          setError('')
          setNotice(message)
          void queryClient.invalidateQueries({ queryKey: ['my-storage-files'] })
        }}
        onError={(message) => {
          setUploadOpen(false)
          setError(message)
        }}
      />
    </div>
  )
}

function UploadModal({
  open,
  nodeID,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  onClose: () => void
  onDone: (message: string) => void
  onError: (message: string) => void
}) {
  const [category, setCategory] = useState<FileCategory>('iso')
  const [progress, setProgress] = useState('')
  const [busy, setBusy] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)

  const run = async () => {
    const file = inputRef.current?.files?.[0]
    if (!file) return
    setBusy(true)
    setProgress('计算摘要…')
    try {
      // 先算整个文件的摘要：命中的话服务端直接返回 ready，一片也不用传。
      const digest = await sha256Hex(file)

      setProgress('创建上传会话…')
      const session = await userStorageApi.createUpload({
        node_id: nodeID,
        category,
        // 目录按类别分：iso 与 disk 混在一个目录里，用户很难辨认，
        // 也不利于将来按类别做清理策略。
        rel_dir: category,
        filename: file.name,
        total_size: file.size,
        chunk_size: CHUNK_SIZE,
        sha256: digest,
      })

      if (session.instant) {
        onDone(`「${file.name}」已秒传完成——服务端已有相同内容`)
        return
      }

      // 只补缺失的分片：断线重连时不必从头再传。
      const missing = session.missing_chunks ?? range(session.total_chunks)
      for (const [i, idx] of missing.entries()) {
        const start = idx * CHUNK_SIZE
        const chunk = file.slice(start, Math.min(start + CHUNK_SIZE, file.size))
        setProgress(`上传中 ${i + 1}/${missing.length} 片`)
        await userStorageApi.putChunk(session.upload_id, idx, await sha256Hex(chunk))
      }

      setProgress('收尾…')
      await userStorageApi.complete(session.upload_id)
      onDone(`「${file.name}」上传完成`)
    } catch (err) {
      onError(describe(err))
    } finally {
      setBusy(false)
      setProgress('')
    }
  }

  return (
    <Modal
      open={open}
      title="上传文件"
      description="上传前会先在本地计算文件摘要，服务端已有相同内容时直接秒传——不必重复传输。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" disabled={busy} onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={busy} onClick={() => void run()}>
            开始上传
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">类别</span>
          {(Object.keys(CATEGORY_LABEL) as FileCategory[]).map((c) => (
            <label key={c} className="flex cursor-pointer items-start gap-2.5">
              <input
                type="radio"
                className="mt-1"
                name="upload-category"
                checked={category === c}
                onChange={() => setCategory(c)}
              />
              <span>
                <span className="block text-base text-ink">{CATEGORY_LABEL[c]}</span>
                <span className="block text-sm text-ink-3">
                  {c === 'iso' && '可以挂到虚拟机的光驱上，用于安装系统。'}
                  {c === 'share' && '普通文件，在虚拟机里按需取用。'}
                  {c === 'disk' && '可以作为数据盘挂到虚拟机上。'}
                </span>
              </span>
            </label>
          ))}
        </div>

        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">文件</span>
          <input
            ref={inputRef}
            type="file"
            disabled={busy}
            className="text-base text-ink-2 file:mr-2 file:rounded-control file:border file:border-line-strong file:bg-sunken file:px-2 file:py-1 file:text-sm file:text-ink"
          />
          {/* 类别由用户明确选择，而不是按扩展名猜：猜错的结果是一个看起来
              可用、实际挂上去虚拟机起不来的镜像。 */}
          <p className="text-xs text-ink-3">
            类别请按用途选择——系统不会按扩展名推断，因为猜错的结果是一个看起来可用、
            实际用不了的镜像。
          </p>
        </div>

        {progress && <p className="text-sm text-ink-2">{progress}</p>}
      </div>
    </Modal>
  )
}

function range(n: number): number[] {
  return Array.from({ length: n }, (_, i) => i)
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
