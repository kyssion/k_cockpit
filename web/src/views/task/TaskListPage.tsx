import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { isActive, taskApi, type TaskView } from '@/api/task'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatDateTime, relativeTime } from '@/utils/format'
import { TASK_STATUS_LABEL, TASK_STATUS_TONE, taskTypeLabel } from '@/utils/labels'

const PAGE_SIZE = 20

const STATUS_OPTIONS = [
  { value: '', label: '全部状态' },
  { value: 'pending', label: '排队中' },
  { value: 'running', label: '执行中' },
  { value: 'success', label: '成功' },
  { value: 'failed', label: '失败' },
  { value: 'canceled', label: '已取消' },
  { value: 'unknown', label: '状态未知' },
]

export function TaskListPage() {
  const queryClient = useQueryClient()
  const [page, setPage] = useState(1)
  const [status, setStatus] = useState('')
  const [cancelTarget, setCancelTarget] = useState<TaskView | null>(null)
  const [error, setError] = useState('')

  const tasks = useQuery({
    queryKey: ['tasks', page, status],
    queryFn: () => taskApi.list({ page, page_size: PAGE_SIZE, status }),
    // 有进行中的任务时轮询刷新。实时通道（SSE）尚未实现，先以轮询替代；
    // 全部任务处于终态时停止轮询，避免空转。
    refetchInterval: (query) => {
      const items = query.state.data?.items ?? []
      return items.some((t) => isActive(t.status)) ? 2000 : false
    },
  })

  const cancel = useMutation({
    mutationFn: (id: number) => taskApi.cancel(id),
    onSuccess: () => {
      setCancelTarget(null)
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const total = tasks.data?.pagination.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE))

  return (
    <div className="flex flex-col gap-4">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">任务中心</h1>
          <p className="mt-1 text-base text-ink-3">
            所有耗时操作都以任务形式执行，共 {total} 条。
          </p>
        </div>

        <select
          value={status}
          onChange={(e) => {
            setStatus(e.target.value)
            setPage(1)
          }}
          className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
        >
          {STATUS_OPTIONS.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
      </header>

      {tasks.isPending && <PageLoading />}

      {tasks.isError && (
        <div className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(tasks.error)}
        </div>
      )}

      {tasks.data && tasks.data.items.length === 0 && (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState
            title="没有任务"
            description={status ? '当前筛选条件下没有任务，试试其他状态。' : '执行创建、开关机等操作后，任务会出现在这里。'}
          />
        </div>
      )}

      {tasks.data && tasks.data.items.length > 0 && (
        <>
          <div className="overflow-x-auto rounded-card border border-line">
            <table className="w-full border-collapse text-base">
              <thead>
                <tr className="bg-sunken text-left text-xs text-ink-2">
                  <th className="px-4 py-2.5 font-medium">ID</th>
                  <th className="px-4 py-2.5 font-medium">操作</th>
                  <th className="px-4 py-2.5 font-medium">状态</th>
                  <th className="px-4 py-2.5 font-medium">资源</th>
                  <th className="px-4 py-2.5 font-medium">进度</th>
                  <th className="px-4 py-2.5 font-medium">提交时间</th>
                  <th className="px-4 py-2.5 text-right font-medium">操作</th>
                </tr>
              </thead>
              <tbody>
                {tasks.data.items.map((t) => (
                  <TaskRow key={t.id} task={t} onCancel={() => setCancelTarget(t)} />
                ))}
              </tbody>
            </table>
          </div>

          {totalPages > 1 && (
            <div className="flex items-center justify-end gap-2 text-base text-ink-2">
              <Button
                variant="secondary"
                size="sm"
                disabled={page <= 1}
                onClick={() => setPage((p) => p - 1)}
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
                onClick={() => setPage((p) => p + 1)}
              >
                下一页
              </Button>
            </div>
          )}
        </>
      )}

      <Modal
        open={cancelTarget !== null}
        title="取消任务"
        description={
          cancelTarget
            ? cancelTarget.status === 'pending'
              ? `任务 #${cancelTarget.id} 尚未开始执行，取消后不会产生任何影响。`
              : `任务 #${cancelTarget.id} 正在执行，取消请求会下发给执行方。已经产生的副作用不会被回滚，因此任务可能仍会以成功或失败结束。`
            : undefined
        }
        onClose={() => {
          setCancelTarget(null)
          setError('')
        }}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setCancelTarget(null)}>
              返回
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={cancel.isPending}
              onClick={() => cancelTarget && cancel.mutate(cancelTarget.id)}
            >
              确认取消
            </Button>
          </>
        }
      >
        {error && <p className="text-base text-danger">{error}</p>}
      </Modal>
    </div>
  )
}

function TaskRow({ task, onCancel }: { task: TaskView; onCancel: () => void }) {
  const active = isActive(task.status)

  return (
    <tr className="border-t border-line hover:bg-raised">
      <td className="kc-mono px-4 py-2.5 text-ink-2">#{task.id}</td>
      <td className="px-4 py-2.5 text-ink">{taskTypeLabel(task.type)}</td>
      <td className="px-4 py-2.5">
        <div className="flex flex-col gap-1">
          <StatusBadge tone={TASK_STATUS_TONE[task.status]}>
            {TASK_STATUS_LABEL[task.status]}
          </StatusBadge>
          {task.cancel_requested && active && (
            <span className="text-xs text-warning">取消请求已下发</span>
          )}
        </div>
      </td>
      <td className="px-4 py-2.5 text-ink-2">
        {task.resource_name || (task.resource_id ? `#${task.resource_id}` : '—')}
      </td>
      <td className="px-4 py-2.5">
        <ProgressBar value={task.progress} active={task.status === 'running'} />
        {task.error && (
          <p className="mt-1 max-w-[280px] truncate text-xs text-danger" title={task.error}>
            {task.error}
          </p>
        )}
      </td>
      <td className="px-4 py-2.5 text-ink-2" title={formatDateTime(task.created_at)}>
        {relativeTime(task.created_at)}
      </td>
      <td className="px-4 py-2.5 text-right">
        {active ? (
          <Button
            variant="ghost"
            size="sm"
            disabled={task.cancel_requested}
            onClick={onCancel}
          >
            {task.cancel_requested ? '取消中' : '取消'}
          </Button>
        ) : (
          <Button variant="ghost" size="sm" disabled>
            已结束
          </Button>
        )}
      </td>
    </tr>
  )
}

function ProgressBar({ value, active }: { value: number; active: boolean }) {
  const percent = Math.max(0, Math.min(100, value))
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 w-24 overflow-hidden rounded-pill bg-sunken">
        <div
          className={`h-full rounded-pill bg-brand transition-[width] ${active && percent === 0 ? 'animate-pulse' : ''}`}
          style={{ width: `${active && percent === 0 ? 15 : percent}%` }}
        />
      </div>
      <span className="kc-nums text-xs text-ink-3">{percent}%</span>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
