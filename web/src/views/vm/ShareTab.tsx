/**
 * ShareTab 管理虚拟机的目录共享（F-5-06，9p VirtFS）。
 *
 * 界面要表达清楚两件容易被忽略的事：
 *
 * 1. **路径是相对于你的存储根的**。用户很自然会想填一个绝对路径，
 *    而绝对路径会被后端拒绝——界面要在他填之前就说清楚该填什么。
 * 2. **只读是默认值**。可写共享意味着来宾可以往宿主机写文件，而那些写入
 *    **不受控制面的配额约束**（配额是受理请求时算的，来宾绕过控制面直接
 *    写盘）。因此可写要用户显式选一次。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  SHARE_SECURITY_HINT,
  shareApi,
  type ShareSecurityModel,
  type ShareView,
} from '@/api/share'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { relativeTime } from '@/utils/format'

export function ShareTab({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [addOpen, setAddOpen] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const shares = useQuery({
    queryKey: ['vm-shares', vmID],
    queryFn: () => shareApi.list(vmID),
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['vm-shares', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const unmount = useMutation({
    mutationFn: (tag: string) => shareApi.unmount(vmID, tag),
    onSuccess: () => {
      setError('')
      setNotice('已提交卸载')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (shares.isPending) return <PageLoading />

  const items = shares.data?.items ?? []

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-start justify-between gap-4">
        <p className="max-w-2xl text-base text-ink-3">
          把宿主机上的一个目录以 <span className="kc-mono">9p VirtFS</span> 挂进虚拟机。
          路径填<span className="text-ink-2">相对于你自己存储根</span>的路径（例如{' '}
          <span className="kc-mono">data/iso</span>），不要填绝对路径——共享是一个跨越虚拟化
          边界的读取入口，系统不接受由调用方指定的宿主机绝对路径。
        </p>
        <Button size="sm" onClick={() => setAddOpen(true)}>
          新增共享
        </Button>
      </div>

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {items.length === 0 ? (
        <EmptyState
          title="还没有目录共享"
          description="新增后会往这台虚拟机的域配置里加一块 virtio-9p 设备。"
        />
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">tag</th>
                <th className="px-4 py-2.5 font-medium">目录</th>
                <th className="px-4 py-2.5 font-medium">权限</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <ShareRow key={s.id} share={s} onUnmount={() => unmount.mutate(s.tag)} busy={unmount.isPending} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      <p className="text-sm text-ink-3">
        在虚拟机内挂载：<span className="kc-mono">mount -t 9p -o trans=virtio,version=9p2000.L &lt;tag&gt; /mnt</span>
        {' '}。开机自动挂载请写进 <span className="kc-mono">/etc/fstab</span>。
      </p>

      <AddShareModal
        open={addOpen}
        vmID={vmID}
        onClose={() => setAddOpen(false)}
        onDone={() => {
          setAddOpen(false)
          setError('')
          setNotice('已受理，稍后生效')
          refresh()
        }}
        onError={(msg) => {
          setAddOpen(false)
          setError(msg)
        }}
      />
    </div>
  )
}

function ShareRow({
  share,
  onUnmount,
  busy,
}: {
  share: ShareView
  onUnmount: () => void
  busy: boolean
}) {
  return (
    <tr className="border-t border-line">
      <td className="px-4 py-2.5">
        <span className="kc-mono text-ink">{share.tag}</span>
      </td>
      <td className="px-4 py-2.5">
        <span className="text-ink">{share.rel_path}</span>
        {/* 绝对路径一并显示：用户需要在虚拟机的文档或 fstab 里写清楚这个共享
            实际对应宿主机的哪里，而只给相对路径他得自己去拼——拼错的结果是
            他以为共享的是 A 目录，实际共享的是 B。 */}
        <span className="kc-mono block text-xs text-ink-3">{share.host_path}</span>
      </td>
      <td className="px-4 py-2.5">
        {share.read_only ? (
          <span className="text-ink-2">只读</span>
        ) : (
          <span className="text-warning">可写</span>
        )}
        <span className="block text-xs text-ink-3">
          {SHARE_SECURITY_HINT[share.security_model]?.label ?? share.security_model}
        </span>
      </td>
      <td className="px-4 py-2.5">
        {share.status === 'active' ? (
          <span className="text-success">
            已生效
            {share.mounted_at && (
              <span className="ml-1.5 text-xs text-ink-3">{relativeTime(share.mounted_at)}</span>
            )}
          </span>
        ) : (
          <span className="text-ink-3">生效中…</span>
        )}
      </td>
      <td className="px-4 py-2.5">
        <button
          className="text-sm text-ink-2 hover:underline"
          disabled={busy}
          onClick={onUnmount}
        >
          卸载
        </button>
      </td>
    </tr>
  )
}

function AddShareModal({
  open,
  vmID,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  vmID: number
  onClose: () => void
  onDone: () => void
  onError: (message: string) => void
}) {
  const [relPath, setRelPath] = useState('')
  const [tag, setTag] = useState('')
  const [model, setModel] = useState<ShareSecurityModel>('mapped')
  // 只读是**默认值**，且用户要显式关掉它——见文件头说明。
  const [readOnly, setReadOnly] = useState(true)

  const mount = useMutation({
    mutationFn: () =>
      shareApi.mount(vmID, {
        rel_path: relPath.trim(),
        tag: tag.trim() || undefined,
        security_model: model,
        read_only: readOnly,
      }),
    onSuccess: onDone,
    onError: (err) => onError(describe(err)),
  })

  const invalid = relPath.trim() === '' || relPath.trim().startsWith('/')

  return (
    <Modal
      open={open}
      title="新增目录共享"
      description="目录必须位于你的存储空间内。系统会把它与你自己的存储根拼成宿主机上的绝对路径——你不能直接指定那个绝对路径。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={invalid}
            loading={mount.isPending}
            onClick={() => mount.mutate()}
          >
            确认
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="目录（相对于你的存储根）"
          value={relPath}
          placeholder="例如：data/iso"
          onChange={(e) => setRelPath(e.target.value)}
          hint="不要以 / 开头。填 /etc 这类绝对路径会被拒绝。"
        />

        <Input
          label="tag（可选）"
          value={tag}
          placeholder="留空则取目录名"
          onChange={(e) => setTag(e.target.value)}
          hint="虚拟机内用它识别这个挂载点。只能由字母、数字、下划线、点和短横线组成。"
        />

        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">权限</span>
          <label className="flex cursor-pointer items-start gap-2.5">
            <input
              type="radio"
              className="mt-1"
              name="share-ro"
              checked={readOnly}
              onChange={() => setReadOnly(true)}
            />
            <span>
              <span className="block text-base text-ink">只读（推荐）</span>
              <span className="block text-sm text-ink-3">
                虚拟机只能读取，无法回写宿主机。
              </span>
            </span>
          </label>
          <label className="flex cursor-pointer items-start gap-2.5">
            <input
              type="radio"
              className="mt-1"
              name="share-ro"
              checked={!readOnly}
              onChange={() => setReadOnly(false)}
            />
            <span>
              <span className="block text-base text-ink">可写</span>
              <span className="block text-sm text-ink-3">
                <span className="text-warning">虚拟机写入的文件会占用宿主机空间，</span>
                而这部分写入<span className="text-warning">不受存储配额约束</span>
                ——配额只在控制面受理请求时计算，虚拟机是绕过控制面直接写盘的。
              </span>
            </span>
          </label>
        </div>

        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">安全模型</span>
          {(Object.keys(SHARE_SECURITY_HINT) as ShareSecurityModel[]).map((m) => (
            <label key={m} className="flex cursor-pointer items-start gap-2.5">
              <input
                type="radio"
                className="mt-1"
                name="share-model"
                checked={model === m}
                onChange={() => setModel(m)}
              />
              <span>
                <span
                  className={`block text-base ${SHARE_SECURITY_HINT[m].dangerous ? 'text-warning' : 'text-ink'}`}
                >
                  {SHARE_SECURITY_HINT[m].label}
                </span>
                <span className="block text-sm text-ink-3">{SHARE_SECURITY_HINT[m].detail}</span>
              </span>
            </label>
          ))}
        </div>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
