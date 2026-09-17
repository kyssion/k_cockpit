/**
 * MigrateSection 处理虚拟机跨节点迁移（F-2-09）。
 *
 * 界面上最需要说清的是一件反直觉的事：**迁移要求关机**。用户对「迁移」的
 * 直觉多半来自其它平台的热迁移，而这里运行中迁移会让磁盘在被写入的同时被
 * 复制，两侧都不可用。不把这条写在按钮旁边，用户会一次次点下去然后被拒。
 *
 * 其次是**冲突要提前列出**：静态地址与端口转发在节点内唯一，换一台宿主机
 * 就可能撞上别人。后端在受理时就会返回具体是哪一项冲突，因此这里只需把它
 * 原样显示出来——让用户在两个节点之间逐项对照是最没必要的一种负担。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { vmApi, type MigrationView, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatDateTime } from '@/utils/format'

export function MigrateSection({ vm }: { vm: VmView }) {
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const [error, setError] = useState('')

  const migrations = useQuery({
    queryKey: ['vm-migrations', vm.id],
    queryFn: () => vmApi.migrations(vm.id),
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((m) => m.status === 'pending' || m.status === 'running')
        ? 3000
        : false,
  })

  const items = migrations.data?.items ?? []
  const busy = items.some((m) => m.status === 'pending' || m.status === 'running')

  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h3 className="text-sm text-ink-3">跨节点迁移</h3>
          <p className="mt-1 text-base text-ink-2">
            把磁盘搬到另一台宿主机。硬件配置、网卡、静态地址与端口转发会一起搬走。
            <span className="text-ink-3">
              （要求关机——运行中迁移会让磁盘在被写入的同时被复制，两侧都不可用。）
            </span>
          </p>
        </div>
        <Button
          size="sm"
          variant="secondary"
          disabled={vm.status !== 'stopped' || busy}
          title={vm.status !== 'stopped' ? '迁移需要先关机' : busy ? '已有进行中的迁移' : ''}
          onClick={() => setOpen(true)}
        >
          迁移
        </Button>
      </div>

      {error && (
        <p className="mt-2.5 rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {items.length > 0 && (
        <ul className="mt-3 flex flex-col">
          {items.map((m) => (
            <li
              key={m.id}
              className="flex items-start justify-between gap-3 border-t border-line py-2.5 text-base"
            >
              <span>
                <span className="text-ink">
                  节点 #{m.from_node_id} → #{m.to_node_id}
                </span>
                {/* 结果里说明跟着搬了些什么——只给一个「成功」会让用户
                    不确定「我原来接的网络、配的转发还在不在」。 */}
                {m.result && <span className="block text-xs text-ink-3">{m.result}</span>}
                {m.error && <span className="block text-xs text-danger">{m.error}</span>}
              </span>
              <span className="flex shrink-0 items-center gap-2">
                <span className="text-xs text-ink-3">{formatDateTime(m.created_at)}</span>
                <MigrationBadge status={m.status} />
              </span>
            </li>
          ))}
        </ul>
      )}

      <MigrateModal
        open={open}
        vm={vm}
        onClose={() => setOpen(false)}
        onDone={() => {
          setOpen(false)
          setError('')
          void queryClient.invalidateQueries({ queryKey: ['vm-migrations', vm.id] })
          void queryClient.invalidateQueries({ queryKey: ['vm', vm.id] })
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(msg) => {
          setOpen(false)
          setError(msg)
        }}
      />
    </section>
  )
}

function MigrationBadge({ status }: { status: MigrationView['status'] }) {
  const map: Record<MigrationView['status'], { label: string; tone: 'idle' | 'warning' | 'success' | 'danger' }> = {
    pending: { label: '排队中', tone: 'idle' },
    running: { label: '迁移中', tone: 'warning' },
    success: { label: '已完成', tone: 'success' },
    failed: { label: '失败', tone: 'danger' },
  }
  const m = map[status]
  return <StatusBadge tone={m.tone}>{m.label}</StatusBadge>
}

function MigrateModal({
  open,
  vm,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  vm: VmView
  onClose: () => void
  onDone: () => void
  onError: (message: string) => void
}) {
  const [toNodeID, setToNodeID] = useState(0)
  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list, enabled: open })

  const migrate = useMutation({
    mutationFn: () => vmApi.migrate(vm.id, toNodeID),
    onSuccess: () => onDone(),
    onError: (err) => onError(describe(err)),
  })

  // 只列**可作为目标**的节点：源节点本身、维护中的节点、未接入的节点
  // 列出来再被拒绝，只会让人以为是自己操作错了。
  const candidates = (nodes.data ?? []).filter(
    (n) => n.id !== vm.node_id && !n.maintenance_mode && n.enroll_state === 'enrolled',
  )

  return (
    <Modal
      open={open}
      title={`迁移「${vm.name}」`}
      description="迁移会把磁盘与节点内资源一起搬到目标宿主机。失败时源侧的数据保留，虚拟机在原处仍然可用。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={toNodeID === 0}
            loading={migrate.isPending}
            onClick={() => migrate.mutate()}
          >
            开始迁移
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Input label="当前节点" value={`#${vm.node_id}`} readOnly disabled />
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">目标节点</label>
          <select
            value={toNodeID}
            onChange={(e) => setToNodeID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
          >
            <option value={0}>请选择…</option>
            {candidates.map((n) => (
              <option key={n.id} value={n.id}>
                {n.name}
              </option>
            ))}
          </select>
          {candidates.length === 0 && (
            <p className="text-xs text-ink-3">
              没有可作为目标的节点。目标须已接入且不在维护模式，并且不能是当前节点。
            </p>
          )}
        </div>

        <div className="rounded-control border border-line bg-raised px-3 py-2 text-sm text-ink-3">
          <p className="text-ink-2">迁移前会检查：</p>
          <ul className="mt-1 flex flex-col gap-0.5">
            <li>目标节点是否处于维护模式</li>
            <li>静态地址在目标节点上是否已被占用</li>
            <li>宿主机端口在目标节点上是否已被其它转发占用</li>
          </ul>
          <p className="mt-1.5">
            有冲突时会**直接拒绝并列出是哪一项**，而不是迁过去之后再出问题。
          </p>
        </div>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
