import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import {
  SCHEDULE_ACTION_LABEL,
  SCHEDULE_RESULT_LABEL,
  WEEKDAY_LABEL,
  scheduleApi,
  type ScheduleAction,
  type ScheduleType,
  type VMSchedule,
} from '@/api/schedule'
import { Button } from '@/components/common/Button'
import { ErrorBox } from '@/components/common/ErrorBox'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { describe } from '@/utils/describe'
import { formatDateTime } from '@/utils/format'

/** 管理虚拟机的定时操作（F-7-05）。 */
export function ScheduleTab({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState('')

  const list = useQuery({
    queryKey: ['vm-schedules', vmID],
    queryFn: () => scheduleApi.list(vmID),
  })

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: ['vm-schedules', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const setEnabled = useMutation({
    mutationFn: ({ id, enabled }: { id: number; enabled: boolean }) =>
      scheduleApi.setEnabled(vmID, id, enabled),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const remove = useMutation({
    mutationFn: (id: number) => scheduleApi.remove(vmID, id),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (list.isPending) return <PageLoading />
  if (list.isError) return <ErrorBox message={describe(list.error)} />

  const items = list.data.items

  return (
    <div className="flex flex-col gap-4">
      {error && <ErrorBox message={error} />}

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">定时任务</h2>
          <Button size="sm" onClick={() => setCreating(true)}>
            新增
          </Button>
        </div>

        {items.length === 0 ? (
          <EmptyState
            title="没有定时任务"
            description="可以设置定时开机或关机，例如每晚定时关机以节省资源。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">动作</th>
                <th className="px-4 py-2 text-left font-normal">计划</th>
                <th className="px-4 py-2 text-left font-normal">下次执行</th>
                <th className="px-4 py-2 text-left font-normal">上次结果</th>
                <th className="px-4 py-2 text-left font-normal">状态</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <tr key={s.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="px-4 py-2.5 text-ink">
                    {SCHEDULE_ACTION_LABEL[s.action] ?? s.action}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{describeSchedule(s)}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {s.next_run_at ? formatDateTime(s.next_run_at) : '—'}
                  </td>
                  <td className="px-4 py-2.5">
                    <LastResultCell result={s.last_result} taskID={s.last_task_id} />
                  </td>
                  <td className="px-4 py-2.5">
                    <span className={s.enabled ? 'text-success' : 'text-ink-3'}>
                      {s.enabled ? '已启用' : '已停用'}
                    </span>
                  </td>
                  <td className="px-4 py-2.5 text-right">
                    <button
                      className="text-sm text-brand hover:underline"
                      disabled={setEnabled.isPending}
                      onClick={() => setEnabled.mutate({ id: s.id, enabled: !s.enabled })}
                    >
                      {s.enabled ? '停用' : '启用'}
                    </button>
                    <button
                      className="ml-3 text-sm text-danger hover:underline"
                      disabled={remove.isPending}
                      onClick={() => remove.mutate(s.id)}
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
        这里的「删除」删的是**定时任务本身**，不会删除虚拟机。
        <br />
        服务停机期间错过的时间点不会被补执行（记为「已跳过」）——补执行会让
        恢复后连着做几次本该分散在不同时间的操作。
        <br />
        「删除虚拟机」类任务仅支持一次性，创建时需要二次验证；它固定保留磁盘
        （可从回收站恢复），需要连盘删除请在虚拟机上手动执行。
      </p>

      <CreateScheduleModal
        open={creating}
        vmID={vmID}
        onClose={() => setCreating(false)}
        onCreated={() => {
          setCreating(false)
          refresh()
        }}
      />
    </div>
  )
}

/** 显示上次执行结果。 */
function LastResultCell({ result, taskID }: { result?: string; taskID?: number }) {
  if (!result) return <span className="text-ink-3">尚未执行</span>

  // 「已跳过」用中性色而不是红色：它不是失败，而是**刻意不执行**。
  // 标红会让人去排查一个按设计就没有运行的任务。
  const tone =
    result === 'success' ? 'text-success' : result === 'skipped' ? 'text-ink-2' : 'text-danger'

  return (
    <span className={tone}>
      {SCHEDULE_RESULT_LABEL[result as keyof typeof SCHEDULE_RESULT_LABEL] ?? result}
      {taskID !== undefined && (
        <Link to="/task" className="ml-1.5 text-xs text-ink-3 hover:text-brand">
          #{taskID}
        </Link>
      )}
    </span>
  )
}

/** 把调度配置写成人话。 */
function describeSchedule(s: VMSchedule): string {
  const at = s.time_of_day
  switch (s.schedule_type) {
    case 'once':
      return `一次性 · ${at}`
    case 'daily':
      return `每天 ${at}`
    case 'weekly': {
      const days = s.weekdays.map((d) => WEEKDAY_LABEL[d] ?? d).join('、')
      return `${days || '未选'} ${at}`
    }
    default:
      return at
  }
}

/** 新建定时任务。 */
function CreateScheduleModal({
  open,
  vmID,
  onClose,
  onCreated,
}: {
  open: boolean
  vmID: number
  onClose: () => void
  onCreated: () => void
}) {
  const [action, setAction] = useState<ScheduleAction>('shutdown')
  const [kind, setKind] = useState<ScheduleType>('daily')
  const [weekdays, setWeekdays] = useState<number[]>([1])
  const [timeOfDay, setTimeOfDay] = useState('03:00')
  const [date, setDate] = useState('')
  const [snapshotName, setSnapshotName] = useState('')
  const [includeMemory, setIncludeMemory] = useState(false)
  const [error, setError] = useState('')

  const create = useMutation({
    mutationFn: () =>
      scheduleApi.create(vmID, {
        action,
        schedule_type: kind,
        weekdays: kind === 'weekly' ? weekdays : undefined,
        time_of_day: timeOfDay,
        date: kind === 'once' ? date : undefined,
        snapshot_name: action === 'snapshot' ? snapshotName : undefined,
        include_memory: action === 'snapshot' ? includeMemory : undefined,
      }),
    onSuccess: () => {
      reset()
      onCreated()
    },
    onError: (err) => setError(describe(err)),
  })

  function reset() {
    setAction('shutdown')
    setKind('daily')
    setWeekdays([1])
    setTimeOfDay('03:00')
    setDate('')
    setSnapshotName('')
    setIncludeMemory(false)
    setError('')
  }

  function handleClose() {
    reset()
    onClose()
  }

  // 提交按钮的可用性：把「后端一定会拒绝的请求」拦在本地，让用户不用
  // 提交一次才知道少填了东西。真正的校验仍在服务端。
  const ready =
    timeOfDay !== '' &&
    (kind !== 'once' || date !== '') &&
    (kind !== 'weekly' || weekdays.length > 0) &&
    // 周期快照必须带名字模板：它让自动快照在列表里能与手动快照区分。
    (action !== 'snapshot' || snapshotName.trim() !== '')

  return (
    <Modal
      open={open}
      title="新增定时任务"
      onClose={handleClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={handleClose}>
            取消
          </Button>
          <Button size="sm" disabled={!ready} loading={create.isPending} onClick={() => create.mutate()}>
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <label className="flex flex-col gap-1">
          <span className="text-sm text-ink-2">动作</span>
          <select
            className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            value={action}
            onChange={(e) => {
              const next = e.target.value as ScheduleAction
              setAction(next)
              // 删除只支持一次性（后端同样拒绝周期删除）：定时删除的破坏性
              // 与一次误配的重复周期叠加，是没有人要求过的风险。
              if (next === 'delete' && kind !== 'once') setKind('once')
            }}
          >
            <option value="shutdown">关机</option>
            <option value="start">开机</option>
            {/* 创建快照与删除相反：快照是累加且可再删的，因此允许周期执行。 */}
            <option value="snapshot">创建快照</option>
            {/* G-36：删除属高风险动作——仅一次性，执行时走二次验证。 */}
            <option value="delete">删除虚拟机</option>
          </select>
        </label>

        {action === 'delete' && (
          <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            到达时间点时会删除这台虚拟机（磁盘处理方式为「保留」，可从回收站恢复）。
            创建本任务时需要完成一次二次验证；仅支持一次性任务。
          </p>
        )}

        {action === 'snapshot' && (
          <label className="flex flex-col gap-1">
            <span className="text-sm text-ink-2">快照名模板</span>
            <input
              value={snapshotName}
              onChange={(e) => setSnapshotName(e.target.value)}
              placeholder="例如 nightly"
              className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            />
            <span className="text-xs text-ink-3">
              实际名字会追加时间（如 nightly-20260921-0300），因此周期快照不会重名。
            </span>
          </label>
        )}

        <label className="flex flex-col gap-1">
          <span className="text-sm text-ink-2">重复方式</span>
          <select
            className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            value={kind}
            onChange={(e) => setKind(e.target.value as ScheduleType)}
          >
            <option value="once">仅一次</option>
            <option value="daily">每天</option>
            <option value="weekly">每周</option>
          </select>
        </label>

        {kind === 'weekly' && (
          <fieldset className="flex flex-col gap-1.5">
            <legend className="text-sm text-ink-2">星期</legend>
            <div className="flex flex-wrap gap-1.5">
              {[1, 2, 3, 4, 5, 6, 7].map((d) => {
                const on = weekdays.includes(d)
                return (
                  <button
                    key={d}
                    type="button"
                    onClick={() =>
                      setWeekdays(on ? weekdays.filter((x) => x !== d) : [...weekdays, d].sort())
                    }
                    className={[
                      'rounded-control border px-2.5 py-1 text-sm',
                      on
                        ? 'border-brand bg-brand/10 text-brand'
                        : 'border-line-strong text-ink-3 hover:text-ink',
                    ].join(' ')}
                  >
                    {WEEKDAY_LABEL[d]}
                  </button>
                )
              })}
            </div>
          </fieldset>
        )}

        {kind === 'once' && (
          <Input
            label="日期"
            type="date"
            value={date}
            onChange={(e) => setDate(e.target.value)}
          />
        )}

        <Input
          label="时刻"
          type="time"
          value={timeOfDay}
          onChange={(e) => setTimeOfDay(e.target.value)}
          hint="按服务器本地时间执行"
        />

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </Modal>
  )
}
