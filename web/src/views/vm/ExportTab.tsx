/**
 * ExportTab 管理虚拟机的导出产物（F-2-14）。
 *
 * 独立成文件而不是塞进 VmDetailPage：那个文件已经 2600 行，而导出页签自带
 * 一整套查询、变更与两个弹框。继续堆下去只会让任何一次改动都要在几千行里
 * 定位——按「一个页签一个文件」拆开，后续再加页签也有地方放。
 *
 * 两点与其它页签不同：
 *
 * 1. **导出要求关机**，因此发起按钮在运行中时禁用并说明原因。这不是可以在
 *    执行时再补的约束——导出的是「此刻磁盘上的内容」，运行中拿到的是崩溃
 *    一致性快照，而产物会被搬到别处使用，出了问题很难回头。
 * 2. **进行中的导出要显示出来**。记录在受理时就创建（pending），界面据此
 *    显示「导出中」而不是一个空列表——导出可能跑几十分钟，列表空着会让
 *    用户以为刚才那一下没点上，转头再点一次。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import {
  EXPORT_FORMAT_HINT,
  EXPORT_STATUS_LABEL,
  EXPORT_STATUS_TONE,
  vmApi,
  type ExportFormat,
  type VmExport,
  type VmView,
} from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatBytes, formatDateTime } from '@/utils/format'

export function ExportTab({ vm }: { vm: VmView }) {
  const queryClient = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<VmExport | null>(null)
  const [error, setError] = useState('')

  const list = useQuery({
    queryKey: ['vm-exports', vm.id],
    queryFn: () => vmApi.exports(vm.id),
    // 有导出在进行时持续刷新：它是长任务，用户需要看到状态推进。
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some(
        (e) => e.status === 'pending' || e.status === 'running',
      )
        ? 3000
        : false,
  })

  const remove = useMutation({
    mutationFn: (e: VmExport) => vmApi.deleteExport(vm.id, e.id),
    onSuccess: () => {
      setDeleteTarget(null)
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['vm-exports', vm.id] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => {
      setDeleteTarget(null)
      setError(describe(err))
    },
  })

  const items = list.data?.items ?? []
  const busy = items.some((e) => e.status === 'pending' || e.status === 'running')
  const canExport = vm.status === 'stopped' && !busy

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-start justify-between gap-4">
        <p className="text-base text-ink-3">
          导出会生成一份可带走副本——用于迁移到别处、当作模板，或者交给别人。
          产物计入你的存储配额。
        </p>
        <Button
          size="sm"
          disabled={!canExport}
          title={
            vm.status !== 'stopped'
              ? '导出需要先关机：产物会被搬到别处使用，运行中导出得到的是不一致的镜像'
              : busy
                ? '已有正在进行中的导出'
                : ''
          }
          onClick={() => setCreateOpen(true)}
        >
          新建导出
        </Button>
      </div>

      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {list.isPending ? (
        <PageLoading />
      ) : items.length === 0 ? (
        <EmptyState
          title="还没有导出"
          description="关机后即可导出系统盘；数据盘可选，默认不包含。"
        />
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">格式</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">大小</th>
                <th className="px-4 py-2.5 font-medium">发起时间</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((e) => (
                <tr key={e.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="px-4 py-2.5">
                    <span className="text-ink">
                      {EXPORT_FORMAT_HINT[e.format]?.label ?? e.format}
                    </span>
                    {e.include_data_disks && (
                      <span className="ml-2 text-xs text-ink-3">含数据盘</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={EXPORT_STATUS_TONE[e.status]}>
                      {EXPORT_STATUS_LABEL[e.status]}
                    </StatusBadge>
                    {/* 失败原因直接显示在状态旁边：只说「失败」不说原因，
                        用户只能靠猜或者去翻日志。 */}
                    {e.error && <span className="block text-xs text-danger">{e.error}</span>}
                  </td>
                  <td className="kc-nums px-4 py-2.5 text-ink-2">
                    {e.status === 'success' ? formatBytes(e.size_bytes) : '—'}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{formatDateTime(e.created_at)}</td>
                  <td className="px-4 py-2.5">
                    {/* 未完成时**不给下载链接**：给了会让界面显示一个点了
                        拿不到东西的按钮。 */}
                    {e.status === 'success' ? (
                      <span className="flex gap-2">
                        <a
                          href={vmApi.exportDownloadUrl(vm.id, e.id)}
                          className="text-sm text-brand hover:underline"
                          download
                        >
                          下载
                        </a>
                        <button
                          className="text-sm text-danger hover:underline"
                          onClick={() => setDeleteTarget(e)}
                        >
                          删除
                        </button>
                      </span>
                    ) : (
                      <span className="text-xs text-ink-3">
                        {e.status === 'failed' ? '可删除后重试' : '完成后可下载'}
                      </span>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <CreateExportModal
        open={createOpen}
        vmID={vm.id}
        onClose={() => setCreateOpen(false)}
        onDone={() => {
          setCreateOpen(false)
          setError('')
          void queryClient.invalidateQueries({ queryKey: ['vm-exports', vm.id] })
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(msg) => {
          setCreateOpen(false)
          setError(msg)
        }}
      />

      <Modal
        open={deleteTarget != null}
        title="删除导出产物"
        description="删除的是这份副本，虚拟机本身不受影响。"
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
        <p className="text-base text-ink-2">
          删除后这份产物无法恢复，占用的存储配额会相应释放。需要时重新导出即可。
        </p>
      </Modal>
    </div>
  )
}

/** CreateExportModal 选择导出格式并确认。 */
function CreateExportModal({
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
  const [format, setFormat] = useState<ExportFormat>('qcow2')
  const [withData, setWithData] = useState(false)

  const create = useMutation({
    mutationFn: () => vmApi.createExport(vmID, { format, include_data_disks: withData }),
    onSuccess: () => onDone(),
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title="新建导出"
      description="导出是异步的长任务，可能跑到几十分钟；进度可在任务中心与列表里跟踪。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={create.isPending} onClick={() => create.mutate()}>
            开始导出
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">格式</span>
          {(['qcow2', 'ova'] as const).map((f) => (
            <label key={f} className="flex cursor-pointer items-start gap-2.5">
              <input
                type="radio"
                className="mt-1"
                name="export-format"
                checked={format === f}
                onChange={() => setFormat(f)}
              />
              <span>
                <span className="block text-base text-ink">{EXPORT_FORMAT_HINT[f].label}</span>
                <span className="block text-sm text-ink-3">{EXPORT_FORMAT_HINT[f].detail}</span>
              </span>
            </label>
          ))}
        </div>

        <label className="flex cursor-pointer items-start gap-2.5">
          <input
            type="checkbox"
            className="mt-1"
            checked={withData}
            onChange={(e) => setWithData(e.target.checked)}
          />
          <span>
            <span className="block text-base text-ink">连同数据盘一起导出</span>
            <span className="block text-sm text-ink-3">
              默认不包含。数据盘可能远大于系统盘，而多数导出是为了复用系统环境，
              不是搬数据——包含它会让产物与导出耗时都成倍增加。
            </span>
          </span>
        </label>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  const e = error as { message?: string }
  return e?.message ?? '操作失败，请稍后重试'
}
