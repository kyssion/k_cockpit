import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { isActive, taskApi, type TaskStage, type TaskView } from '@/api/task'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
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
  const [clearOpen, setClearOpen] = useState(false)
  const [status, setStatus] = useState('')
  const [cancelTarget, setCancelTarget] = useState<TaskView | null>(null)
  // 详情只记 ID 而不是整条任务：这样打开期间的后台刷新能反映到面板上
  // （进行中的任务会持续变化），而不是停在被点开那一刻的快照。
  const [detailID, setDetailID] = useState<number | null>(null)
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

        <span className="flex items-center gap-2">
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
        {/* 清理是**手动**的一次性动作，因此放在这里而不是自动跑：
            用户看到的条数变少时，他需要知道那是自己做的。 */}
        <Button size="sm" variant="secondary" onClick={() => setClearOpen(true)}>
          清理旧任务
        </Button>
        </span>
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
                  <TaskRow
                    key={t.id}
                    task={t}
                    onOpen={() => setDetailID(t.id)}
                    onCancel={() => setCancelTarget(t)}
                  />
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

      <TaskDetailDrawer
        taskID={detailID}
        onClose={() => setDetailID(null)}
        onCancel={(t) => {
          setDetailID(null)
          setCancelTarget(t)
        }}
      />

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
          <ClearTasksModal
        open={clearOpen}
        onClose={() => setClearOpen(false)}
        onDone={() => {
          setClearOpen(false)
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
      />
</div>
  )
}

function TaskRow({
  task,
  onOpen,
  onCancel,
}: {
  task: TaskView
  onOpen: () => void
  onCancel: () => void
}) {
  const active = isActive(task.status)

  return (
    <tr className="border-t border-line hover:bg-raised">
      <td className="kc-mono px-4 py-2.5">
        {/* ID 本身是入口：任务号是用户在各处会看到的标识（「任务 #12 正在执行」），
            从这里点进去比在行尾再放一个按钮更自然。 */}
        <button className="text-ink-2 hover:text-brand hover:underline" onClick={onOpen}>
          #{task.id}
        </button>
      </td>
      <td className="px-4 py-2.5">
        <button className="text-ink hover:text-brand hover:underline" onClick={onOpen}>
          {taskTypeLabel(task.type)}
        </button>
      </td>
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
      <td className="px-4 py-2.5 text-right whitespace-nowrap">
        {/* 终态任务也保留「详情」入口：失败原因与参数就在面板里，
            而用户正是在看到一条失败记录时才最需要它。 */}
        <Button variant="ghost" size="sm" onClick={onOpen}>
          详情
        </Button>
        {active && (
          <Button
            variant="ghost"
            size="sm"
            disabled={task.cancel_requested}
            onClick={onCancel}
          >
            {task.cancel_requested ? '取消中' : '取消'}
          </Button>
        )}
      </td>
    </tr>
  )
}

/**
 * TaskDetailDrawer 展示单个任务的完整信息。
 *
 * 从右侧滑出而不是弹窗：任务是**列表里的一个条目**，抽屉保留了列表的上下文，
 * 用户看完一条可以立刻点下一条；弹窗会遮住列表，每次都要先关掉。
 *
 * 只持有 ID 而不是整条任务：打开期间进行中的任务会持续变化，
 * 让面板跟着刷新，而不是停在被点开那一刻的快照。
 */
function TaskDetailDrawer({
  taskID,
  onClose,
  onCancel,
}: {
  taskID: number | null
  onClose: () => void
  onCancel: (t: TaskView) => void
}) {
  const open = taskID !== null

  const detail = useQuery({
    queryKey: ['task', taskID],
    queryFn: () => taskApi.get(taskID as number),
    enabled: open,
    // 进行中的任务会持续变化（进度、阶段、终态），2 秒刷新一次；
    // 终态时停止——没必要为一个不会再变的东西持续请求。
    refetchInterval: (q) => (q.state.data && isActive(q.state.data.status) ? 2000 : false),
  })

  // ESC 关闭：抽屉遮住了部分内容，键盘用户需要一个不依赖鼠标的退路。
  useEffect(() => {
    if (!open) return
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [open, onClose])

  if (!open) return null

  const t = detail.data

  return (
    <div className="fixed inset-0 z-40 flex justify-end">
      {/* 遮罩：点击关闭。半透明而不是全黑——让用户仍然能看到后面的列表，
          知道自己还在任务中心。 */}
      <button
        aria-label="关闭"
        className="flex-1 cursor-default bg-black/20"
        onClick={onClose}
      />

      <aside className="flex w-full max-w-xl flex-col overflow-y-auto border-l border-line bg-surface shadow-lg">
        <header className="sticky top-0 flex items-center justify-between border-b border-line bg-surface px-4 py-3">
          <h2 className="text-base font-medium text-ink">
            任务 #{taskID}
            {t && <span className="ml-2 text-ink-2">{taskTypeLabel(t.type)}</span>}
          </h2>
          <button className="text-sm text-ink-3 hover:text-ink" onClick={onClose}>
            关闭
          </button>
        </header>

        {detail.isPending && <PageLoading />}

        {detail.isError && (
          <div className="p-4">
            <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-base text-danger">
              {describe(detail.error)}
            </p>
          </div>
        )}

        {t && (
          <div className="flex flex-col gap-4 p-4">
            <div className="flex items-center gap-2">
              <StatusBadge tone={TASK_STATUS_TONE[t.status]}>
                {TASK_STATUS_LABEL[t.status]}
              </StatusBadge>
              {t.cancel_requested && isActive(t.status) && (
                <span className="text-xs text-warning">取消请求已下发</span>
              )}
            </div>

            <ProgressBar value={t.progress} active={t.status === 'running'} />

            {t.error && (
              <div className="rounded-control bg-danger/10 px-3 py-2">
                <p className="text-xs text-ink-3">失败原因</p>
                <p className="mt-0.5 text-base text-danger">{t.error}</p>
              </div>
            )}

            <section>
              <h3 className="text-sm text-ink-3">基本信息</h3>
              <dl className="mt-1.5 grid grid-cols-2 gap-x-4 gap-y-2 text-base">
                <Field label="资源">
                  {t.resource_name || (t.resource_id ? `#${t.resource_id}` : '—')}
                </Field>
                <Field label="节点">{t.node_id ? `#${t.node_id}` : '—'}</Field>
                <Field label="提交时间">{formatDateTime(t.created_at)}</Field>
                <Field label="开始时间">{t.started_at ? formatDateTime(t.started_at) : '尚未开始'}</Field>
                <Field label="结束时间">
                  {t.finished_at ? formatDateTime(t.finished_at) : '—'}
                </Field>
                <Field label="当前阶段">{currentStageName(t.stages) ?? t.current_stage ?? '—'}</Field>
              </dl>
            </section>

            {/* 阶段时间线。它在「基本信息」之后、「参数」之前：基本信息回答
                「这是什么任务」，时间线回答「它卡在哪」，而参数只有排查到
                具体一步时才需要看。 */}
            {t.stages && t.stages.length > 0 && (
              <section>
                <h3 className="text-sm text-ink-3">执行阶段</h3>
                <StageTimeline stages={t.stages} />
              </section>
            )}

            {/* 参数与结果只在有内容时出现：一堆空的 JSON 块会让面板显得
                比实际信息量更大，用户还得逐个扫过去才能确认「确实没有」。 */}
            {t.params != null && (
              <JsonBlock title="参数" value={t.params} hint="已脱敏：口令、密钥等不会出现在这里。" />
            )}
            {t.result != null && <JsonBlock title="执行结果" value={t.result} />}

            {isActive(t.status) && (
              <div>
                <Button
                  variant="danger"
                  size="sm"
                  disabled={t.cancel_requested}
                  onClick={() => onCancel(t)}
                >
                  {t.cancel_requested ? '取消中' : '取消任务'}
                </Button>
              </div>
            )}
          </div>
        )}
      </aside>
    </div>
  )
}

/** currentStageName 取最近一个阶段的**中文名**。 */
function currentStageName(stages?: TaskStage[]): string | undefined {
  const last = stages?.[stages.length - 1]
  return last?.name
}

/**
 * StageTimeline 渲染任务阶段流水。
 *
 * 只渲染**已上报**的阶段，不预留后续步骤：节点是边做边报的，控制面并不知道
 * 后面还有几步。把未上报的步骤画成灰色的「待执行」看起来更完整，但那是在
 * 编造——一个任务在第 3 步失败时，界面会显示「第 4、5 步待执行」，而实际上
 * 那两步根本不存在。
 */
function StageTimeline({ stages }: { stages: TaskStage[] }) {
  return (
    <ol className="mt-2 flex flex-col">
      {stages.map((s, i) => {
        const last = i === stages.length - 1
        // `local.` 前缀的是控制面自己的步骤。区分开是因为出问题时第一件要
        // 判断的事就是「指令到底有没有送到节点」，而这两类步骤给出的答案不同。
        const isLocal = s.key.startsWith('local.')
        return (
          <li key={s.seq} className="flex gap-2.5">
            <div className="flex flex-col items-center pt-1.5">
              <StageDot status={s.status} />
              {!last && <span className="mt-1 w-px flex-1 bg-line" />}
            </div>

            <div className={last ? 'flex-1 pb-0.5' : 'flex-1 pb-3'}>
              <div className="flex items-baseline justify-between gap-3">
                <span className="flex items-baseline gap-1.5">
                  <span className={STAGE_TEXT[s.status]}>{s.name}</span>
                  {isLocal && <span className="text-xs text-ink-3">控制面</span>}
                </span>
                <span className="kc-nums shrink-0 text-xs text-ink-3">
                  {formatDuration(s.duration_ms)}
                </span>
              </div>
              {s.message && (
                <p className="mt-0.5 text-sm text-danger">{s.message}</p>
              )}
            </div>
          </li>
        )
      })}
    </ol>
  )
}

const STAGE_TEXT: Record<TaskStage['status'], string> = {
  pending: 'text-ink-3',
  running: 'text-ink',
  success: 'text-ink-2',
  failed: 'text-danger',
  skipped: 'text-ink-3 line-through',
}

function StageDot({ status }: { status: TaskStage['status'] }) {
  const base = 'mt-1 h-2 w-2 shrink-0 rounded-full'
  switch (status) {
    case 'running':
      return <span className={`${base} animate-pulse bg-brand`} />
    case 'success':
      return <span className={`${base} bg-success/70`} />
    case 'failed':
      return <span className={`${base} bg-danger`} />
    default:
      return <span className={`${base} bg-line-strong`} />
  }
}

/**
 * formatDuration 把毫秒换算成可读的时长。
 *
 * 不足 1 秒的显示毫秒而不是「0.2 秒」：阶段之间的差异常常就在几十毫秒，
 * 统一成秒会把它们抹平成同一个数字，而「哪一步特别慢」正是要看的东西。
 */
function formatDuration(ms: number): string {
  if (!ms || ms <= 0) return '—'
  if (ms < 1000) return `${ms} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`
  return `${Math.floor(ms / 60_000)} 分 ${Math.round((ms % 60_000) / 1000)} 秒`
}

/** JsonBlock 以可读的形式展示一段结构化数据。 */
function JsonBlock({
  title,
  value,
  hint,
}: {
  title: string
  value: unknown
  hint?: string
}) {
  return (
    <section>
      <h3 className="text-sm text-ink-3">{title}</h3>
      {hint && <p className="mt-0.5 text-xs text-ink-3">{hint}</p>}
      <pre className="kc-mono mt-1.5 max-h-64 overflow-auto rounded-control bg-sunken px-3 py-2 text-sm text-ink-2">
        {JSON.stringify(value, null, 2)}
      </pre>
    </section>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-xs text-ink-3">{label}</dt>
      <dd className="text-ink">{children}</dd>
    </div>
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

/**
 * ClearTasksModal 清理已完成的旧任务。
 *
 * 弹窗要写清两条**只有后端才知道**的约束，因为用户点「清理」时的预期多半是
 * 「全都清掉」：
 *
 *   - **执行中与待执行的不删**（删除一个执行中的任务会让它的结果永远无处落定）
 *   - **被引用的不删**（定时任务的「上次执行」与抓包记录都指着它们）
 */
function ClearTasksModal({
  open,
  onClose,
  onDone,
}: {
  open: boolean
  onClose: () => void
  onDone: () => void
}) {
  const [days, setDays] = useState('7')

  const run = useMutation({
    mutationFn: () => taskApi.clear(Number(days) || 7),
    onSuccess: onDone,
  })

  return (
    <Modal
      open={open}
      title="清理旧任务"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={run.isPending} onClick={() => run.mutate()}>
            清理
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Input
          label="保留最近几天"
          value={days}
          onChange={(e) => setDays(e.target.value)}
          hint="只清理在这个天数之前就已完成的任务。刚跑完的任务不会被删——你多半还想回头看一眼它的结果。"
        />
        <p className="rounded-control bg-sunken px-3 py-2 text-xs text-ink-3">
          执行中与待执行的任务不会被清理——删掉一个执行中的任务，它的结果就永远
          无处落定。定时任务的「上次执行」与抓包记录所指向的任务也会**保留**：
          清掉它们会让那些引用悬空，而界面点过去只会看到一个不存在的任务。
        </p>
      </div>
    </Modal>
  )
}
