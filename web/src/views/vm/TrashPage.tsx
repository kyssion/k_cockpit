/**
 * TrashPage 是回收站（F-2-16）。
 *
 * 语义必须说清，否则用户会按错误的预期操作：
 *
 *  - **删除 = 移入回收站**：记录还在、虚拟化层里的机器与磁盘也没动，只是
 *    从列表里消失。因此它是可逆的。
 *  - **彻底删除 = 删盘 + 物理删记录**：不可逆。
 *
 * 这两步分开的理由很实在：误删是常态，而删盘不可逆。把不可逆的那一步
 * 单独放在这里，用户才有反悔的余地。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { vmApi, type TrashItem } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { relativeTime } from '@/utils/format'
import { VM_STATUS_LABEL, VM_STATUS_TONE } from '@/utils/labels'

export function TrashPage() {
  const queryClient = useQueryClient()
  const [purging, setPurging] = useState<TrashItem | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const list = useQuery({ queryKey: ['vm-trash'], queryFn: vmApi.trash })

  const refresh = () => {
    setError('')
    void queryClient.invalidateQueries({ queryKey: ['vm-trash'] })
    void queryClient.invalidateQueries({ queryKey: ['vms'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const restore = useMutation({
    mutationFn: (id: number) => vmApi.restore(id),
    onSuccess: () => {
      setNotice('已恢复到虚拟机列表（未自动开机，需要的话请手动开机）')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const purge = useMutation({
    mutationFn: (id: number) => vmApi.purge(id),
    onSuccess: () => {
      setPurging(null)
      setNotice('彻底删除任务已提交，可在任务中心查看进度')
      refresh()
    },
    onError: (e) => {
      setPurging(null)
      setError(describe(e))
    },
  })

  const items = list.data?.items ?? []
  const nodeNames = new Map((nodes.data ?? []).map((n) => [n.id, n.name]))

  return (
    <div className="flex flex-col gap-4">
      <header>
        <h1 className="text-lg font-semibold text-ink">回收站</h1>
        <p className="mt-1 text-base text-ink-3">
          删除的虚拟机会先到这里：记录保留、磁盘未动，可以恢复。
          <span className="text-ink-2">彻底删除</span>
          才会删掉磁盘并物理删除记录——那一步不可逆。
        </p>
      </header>

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {list.isPending ? (
        <PageLoading />
      ) : items.length === 0 ? (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState
            title="回收站是空的"
            description="删除虚拟机时它会先移到这里，恢复后再回到列表。"
            action={
              <Link to="/vm">
                <Button size="sm">前往虚拟机</Button>
              </Link>
            }
          />
        </div>
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full text-left text-base">
            <thead className="bg-sunken text-xs text-ink-2">
              <tr>
                <th className="px-4 py-2.5 font-medium">名称</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">规格</th>
                <th className="px-4 py-2.5 font-medium">节点</th>
                <th className="px-4 py-2.5 font-medium">删除于</th>
                <th className="px-4 py-2.5 text-right font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((it) => (
                <tr key={it.id} className="border-t border-line hover:bg-raised">
                  <td className="px-4 py-2.5 text-ink">{it.name}</td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={VM_STATUS_TONE[it.status] ?? 'idle'}>
                      {VM_STATUS_LABEL[it.status] ?? it.status}
                    </StatusBadge>
                  </td>
                  <td className="kc-nums px-4 py-2.5 text-ink-2">
                    {it.vcpu} 核 · {it.memory_mb} MB · {it.disk_gb} GB
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {nodeNames.get(it.node_id) ?? `#${it.node_id}`}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{relativeTime(it.deleted_at)}</td>
                  <td className="whitespace-nowrap px-4 py-2.5 text-right text-sm">
                    <button
                      className="text-primary hover:underline disabled:text-ink-3"
                      disabled={restore.isPending}
                      onClick={() => restore.mutate(it.id)}
                    >
                      恢复
                    </button>
                    <button
                      className="ml-3 text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={!it.purgeable}
                      title={it.purgeable ? undefined : '还有任务在执行，请稍后再试'}
                      onClick={() => setPurging(it)}
                    >
                      彻底删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <Modal
        open={purging !== null}
        title="彻底删除"
        description={
          purging
            ? `将删除「${purging.name}」的磁盘并物理删除记录，此操作不可逆。`
            : undefined
        }
        onClose={() => setPurging(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setPurging(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={purge.isPending}
              onClick={() => purging && purge.mutate(purging.id)}
            >
              确认彻底删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          如果只是不想在列表里看到它，保持现状即可——它在回收站里，随时可以恢复。
        </p>
      </Modal>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
