import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import {
  SNAPSHOT_KIND_HINT,
  SNAPSHOT_KIND_LABEL,
  SNAPSHOT_STATUS_LABEL,
  SNAPSHOT_STATUS_TONE,
  nvramApi,
  snapshotApi,
  type Snapshot,
} from '@/api/snapshot'
import { Button } from '@/components/common/Button'
import { ErrorBox } from '@/components/common/ErrorBox'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { describe } from '@/utils/describe'
import { formatBytes, formatDateTime } from '@/utils/format'

/** SnapshotTab 管理虚拟机的快照（F-2-07）。 */
export function SnapshotTab({ vmID, firmware }: { vmID: number; firmware: string }) {
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [restoreTarget, setRestoreTarget] = useState<Snapshot | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<Snapshot | null>(null)
  const [deleteAllOpen, setDeleteAllOpen] = useState(false)
  const [skipCurrent, setSkipCurrent] = useState(false)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const list = useQuery({
    queryKey: ['vm-snapshots', vmID],
    queryFn: () => snapshotApi.list(vmID),
    // 有快照处于中间状态时持续刷新：创建与恢复都要几十秒到几分钟，
    // 不刷新的话用户会对着「创建中」反复点刷新。
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((s) => s.status === 'creating' || s.status === 'restoring')
        ? 3000
        : false,
  })

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: ['vm-snapshots', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const restore = useMutation({
    mutationFn: (id: number) => snapshotApi.restore(vmID, id),
    onSuccess: (r) => {
      setRestoreTarget(null)
      setError('')
      setNotice(`已提交恢复，任务 #${r.task_id} 正在执行`)
      refresh()
    },
    onError: (err) => {
      setRestoreTarget(null)
      setError(describe(err))
    },
  })

  const deleteAll = useMutation({
    mutationFn: () => snapshotApi.deleteAll(vmID, skipCurrent),
    onSuccess: (r) => {
      setDeleteAllOpen(false)
      setError('')
      setNotice(
        r.skipped > 0
          ? `已提交删除 ${r.total} 个快照（跳过 ${r.skipped} 个），任务 #${r.task_id} 正在执行`
          : `已提交删除 ${r.total} 个快照，任务 #${r.task_id} 正在执行`,
      )
      refresh()
    },
    onError: (err) => {
      setDeleteAllOpen(false)
      setError(describe(err))
    },
  })

  // 启动项修复只在 UEFI 机器上有意义：BIOS 没有 NVRAM 启动项，给一个点了
  // 必然报错的按钮，只会让人以为"修复失败了"。
  const repair = useMutation({
    mutationFn: () => nvramApi.repair(vmID),
    onSuccess: (r) => {
      setError('')
      setNotice(`已提交启动项修复，任务 #${r.task_id} 正在执行；完成后请重启虚拟机确认`)
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const remove = useMutation({
    mutationFn: (id: number) => snapshotApi.remove(vmID, id),
    onSuccess: (r) => {
      setDeleteTarget(null)
      setError('')
      setNotice(`已提交删除，任务 #${r.task_id} 正在执行`)
      refresh()
    },
    onError: (err) => {
      setDeleteTarget(null)
      setError(describe(err))
    },
  })

  if (list.isPending) return <PageLoading />
  if (list.isError) return <ErrorBox message={describe(list.error)} />

  const { items, quota, used } = list.data

  return (
    <div className="flex flex-col gap-4">
      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && <ErrorBox message={error} />}

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <div className="flex items-center gap-2">
            <h2 className="text-sm font-medium text-ink-2">快照</h2>
            <span className={used >= quota ? 'text-xs text-warning' : 'text-xs text-ink-3'}>
              {used} / {quota}
            </span>
          </div>
          <div className="flex items-center gap-2">
            {firmware === 'uefi' && (
              <Button
                size="sm"
                variant="secondary"
                loading={repair.isPending}
                title="恢复快照后若开机直接进入 UEFI Shell，多半是启动项指向了不存在的路径"
                onClick={() => repair.mutate()}
              >
                修复 UEFI 启动项
              </Button>
            )}
            {/* 删除全部只在真的有快照时出现：一个点了必然失败的按钮，
                比没有这个按钮更糟。 */}
            {items.length > 0 && (
              <Button
                size="sm"
                variant="secondary"
                onClick={() => {
                  setSkipCurrent(false)
                  setDeleteAllOpen(true)
                }}
              >
                删除全部
              </Button>
            )}
            <Button
              size="sm"
              // 配额用完时直接禁用，而不是让用户填完名字才被拒绝。
              disabled={used >= quota}
              onClick={() => setCreating(true)}
            >
              创建快照
            </Button>
          </div>
        </div>

        {items.length === 0 ? (
          <EmptyState
            title="没有快照"
            description="快照可以保留某个时刻的磁盘状态，改动出问题时可回滚到它。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">名称</th>
                <th className="px-4 py-2 text-left font-normal">类型</th>
                <th className="px-4 py-2 text-left font-normal">大小</th>
                <th className="px-4 py-2 text-left font-normal">状态</th>
                <th className="px-4 py-2 text-left font-normal">创建时间</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <tr key={s.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="px-4 py-2.5">
                    <span className="text-ink">{s.name}</span>
                    {/* 当前快照要一眼看出来，否则用户会在恢复后又点一次恢复。 */}
                    {s.is_current && (
                      <span className="ml-2 rounded-pill bg-brand/10 px-1.5 py-0.5 text-xs text-brand">
                        当前状态
                      </span>
                    )}
                    {s.description && (
                      <span className="block text-sm text-ink-3">{s.description}</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {SNAPSHOT_KIND_LABEL[s.kind] ?? s.kind}
                    {s.include_memory && <span className="ml-1 text-xs text-ink-3">含内存</span>}
                    {/* 说明写"代价"而不是"技术名词"：外部快照不含内存，
                        用户如果没注意到，恢复后会发现运行现场不在。 */}
                    <span className="block text-xs text-ink-3">
                      {SNAPSHOT_KIND_HINT[s.kind] ?? ''}
                    </span>
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {s.size_bytes > 0 ? formatBytes(s.size_bytes) : '—'}
                  </td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={SNAPSHOT_STATUS_TONE[s.status]}>
                      {SNAPSHOT_STATUS_LABEL[s.status] ?? s.status}
                    </StatusBadge>
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{formatDateTime(s.created_at)}</td>
                  <td className="px-4 py-2.5 text-right">
                    <button
                      className="text-sm text-brand hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={!s.can_restore || restore.isPending}
                      title={restoreDisabledReason(s)}
                      onClick={() => setRestoreTarget(s)}
                    >
                      恢复
                    </button>
                    <button
                      className="ml-3 text-sm text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={!s.can_delete || remove.isPending}
                      title={deleteDisabledReason(s)}
                      onClick={() => setDeleteTarget(s)}
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <p className="rounded-card border border-line bg-raised px-4 py-3 text-sm text-ink-3">
        快照类型由系统选择：含内存的用内部快照（可恢复运行现场），运行中且不含
        内存的用外部快照（对持续写入的磁盘更可靠）。
        <br />
        <span className="text-warning">恢复快照会丢弃快照之后的所有磁盘改动</span>
        ，且不可撤销。有子快照或正处于「当前状态」的快照不能删除。
      </p>

      <Modal
        open={deleteAllOpen}
        title="删除全部快照"
        description="这台虚拟机的还原点会被一次性删除。删除进行中的快照与当前所处的快照会被跳过。"
        onClose={() => setDeleteAllOpen(false)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeleteAllOpen(false)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={deleteAll.isPending}
              onClick={() => deleteAll.mutate()}
            >
              确认删除
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-2 text-base text-ink-2">
          <p>
            将删除 <span className="kc-nums text-ink">{items.length}</span> 个快照
            {used >= quota && <span className="ml-1 text-xs text-warning">（配额已满）</span>}
          </p>
          <label className="flex cursor-pointer items-start gap-2">
            <input
              type="checkbox"
              className="mt-1"
              checked={skipCurrent}
              onChange={(e) => setSkipCurrent(e.target.checked)}
            />
            <span>
              保留当前所处的快照
              <span className="block text-xs text-ink-3">
                勾选后只清掉历史还原点，虚拟机当前所在的那个不动。
              </span>
            </span>
          </label>
          <p className="text-sm text-danger">
            删除后无法回滚到这些时间点。这一操作需要二次验证。
          </p>
        </div>
      </Modal>

      <CreateSnapshotModal
        open={creating}
        vmID={vmID}
        onClose={() => setCreating(false)}
        onCreated={(taskID) => {
          setCreating(false)
          setNotice(`已提交创建，任务 #${taskID} 正在执行`)
          refresh()
        }}
      />

      <Modal
        open={restoreTarget !== null}
        title={`恢复到快照「${restoreTarget?.name ?? ''}」`}
        onClose={() => setRestoreTarget(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setRestoreTarget(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={restore.isPending}
              onClick={() => restoreTarget && restore.mutate(restoreTarget.id)}
            >
              确认恢复
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          <span className="font-medium text-danger">
            该快照之后产生的所有磁盘改动都会丢失，且无法撤销。
          </span>
        </p>
        <p className="mt-2 text-base text-ink-3">
          创建于 {formatDateTime(restoreTarget?.created_at)}
          {restoreTarget?.include_memory
            ? '，包含内存状态，将恢复到当时的运行现场。'
            : '，不含内存，恢复后虚拟机处于关机状态。'}
        </p>
      </Modal>

      <Modal
        open={deleteTarget !== null}
        title={`删除快照「${deleteTarget?.name ?? ''}」`}
        description="删除快照不会影响虚拟机当前的数据，只是失去这个还原点。"
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
              onClick={() => deleteTarget && remove.mutate(deleteTarget.id)}
            >
              确认删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          这是一个还原点，删除后无法再用它恢复。虚拟机当前的数据不受影响。
        </p>
      </Modal>
    </div>
  )
}

/**
 * 下面两个函数给出按钮被禁用的原因。
 *
 * 用 title 提示而不是只在界面上写一段通用说明：用户盯着某一个灰掉的按钮时，
 * 需要知道的是「这一个为什么不能点」，而不是「一般来说什么情况下不能点」。
 */
function restoreDisabledReason(s: Snapshot): string {
  if (s.is_current) return '虚拟机当前正运行在这个快照上，无需恢复'
  if (s.status === 'creating') return '快照正在创建中'
  if (s.status === 'restoring') return '正在恢复中'
  if (s.status === 'error') return '该快照创建失败，不能用于恢复'
  if (s.status === 'deleting') return '快照正在删除中'
  return ''
}

function deleteDisabledReason(s: Snapshot): string {
  if (s.has_children) return '该快照存在子快照，请先删除子快照'
  if (s.is_current) return '虚拟机当前正运行在这个快照上，不能删除'
  if (s.status === 'creating') return '快照正在创建中'
  if (s.status === 'deleting') return '快照正在删除中'
  return ''
}

/** CreateSnapshotModal 新建快照。 */
function CreateSnapshotModal({
  open,
  vmID,
  onClose,
  onCreated,
}: {
  open: boolean
  vmID: number
  onClose: () => void
  onCreated: (taskID: number) => void
}) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [includeMemory, setIncludeMemory] = useState(false)
  const [error, setError] = useState('')

  const create = useMutation({
    mutationFn: () => snapshotApi.create(vmID, { name, description, include_memory: includeMemory }),
    onSuccess: (r) => {
      reset()
      onCreated(r.task_id)
    },
    onError: (err) => setError(describe(err)),
  })

  function reset() {
    setName('')
    setDescription('')
    setIncludeMemory(false)
    setError('')
  }

  return (
    <Modal
      open={open}
      title="创建快照"
      onClose={() => {
        reset()
        onClose()
      }}
      footer={
        <>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              reset()
              onClose()
            }}
          >
            取消
          </Button>
          <Button
            size="sm"
            disabled={name.trim() === ''}
            loading={create.isPending}
            onClick={() => create.mutate()}
          >
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="名称"
          value={name}
          maxLength={128}
          placeholder="例如 before-upgrade"
          onChange={(e) => setName(e.target.value)}
        />

        <Input
          label="描述（可选）"
          value={description}
          maxLength={255}
          onChange={(e) => setDescription(e.target.value)}
        />

        <label className="flex cursor-pointer items-start gap-2.5">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={includeMemory}
            onChange={(e) => setIncludeMemory(e.target.checked)}
          />
          <span>
            <span className="block text-base text-ink">同时保存运行状态（含内存）</span>
            <span className="block text-sm text-ink-3">
              开启后可以把虚拟机恢复到按下快照那一刻的运行现场，代价是快照体积
              可能数倍于磁盘本身。关闭则只能恢复到关机状态。
            </span>
          </span>
        </label>

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </Modal>
  )
}
