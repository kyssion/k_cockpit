/**
 * 调度器与调度事件接口（F-7-04）。
 *
 * 这块界面上最容易误读的一件事是**空事件列表**：
 *
 *   事件表**只记录实际发生的动作**（规格明确要求不记普通轮询）。
 *   一个每 30 秒醒一次的清理器，如果今天确实无事可做，它的事件列表就是空的。
 *
 * 「没事件」与「没在运行」是两回事，界面必须把它们分开说——否则用户会把
 * 一个正常工作的调度器当成坏的。因此列表来自**注册表**（有哪些东西在跑），
 * 而不是来自事件表。
 */
import { get } from './client'

export interface SchedulerInfo {
  key: string
  name: string
  group: string
  /** 说明它做什么、以及多久做一次。 */
  description: string
  /** 周期（秒）。界面上没有别的字段能告诉用户「多久做一次」。 */
  interval_seconds: number
  /** 恒为 true：只在有动作时才记录事件。 */
  records_only_on_action: boolean
}

export interface SchedulerEventView {
  id: number
  scheduler_key: string
  scheduler_name?: string
  group?: string
  node_id?: number
  /** 这次动作影响的范围。 */
  scope?: string
  status: 'done' | 'failed'
  message?: string
  at: string
}

export interface SchedulerView {
  scheduler: SchedulerInfo
  /** 最近几次**实际发生的动作**，可能为空。 */
  events: SchedulerEventView[]
  /** 最近一次动作的时刻；为空表示**还没有过动作**，不是没在运行。 */
  last_action_at?: string
  failed_count: number
}

export const schedulerApi = {
  overview: () => get<{ items: SchedulerView[] }>('/api/v1/schedulers', { per_scheduler: 5 }),
  events: (key?: string, status?: string) =>
    get<{ items: SchedulerEventView[] }>('/api/v1/scheduler-events', {
      scheduler_key: key ?? '',
      status: status ?? '',
      limit: 100,
    }),
}

/** formatInterval 把秒数变成人话的周期。 */
export function formatInterval(seconds: number): string {
  if (seconds <= 0) return '—'
  if (seconds < 60) return `每 ${seconds} 秒`
  if (seconds < 3600) return `每 ${Math.round(seconds / 60)} 分钟`
  return `每 ${(seconds / 3600).toFixed(seconds % 3600 === 0 ? 0 : 1)} 小时`
}

/** formatAgo 把时刻变成「多久之前」。 */
export function formatAgo(at: string): string {
  const t = Date.parse(at)
  if (!Number.isFinite(t)) return at
  const diff = Math.max(0, Date.now() - t)
  const min = Math.floor(diff / 60000)
  if (min < 1) return '刚刚'
  if (min < 60) return `${min} 分钟前`
  const h = Math.floor(min / 60)
  if (h < 24) return `${h} 小时前`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d} 天前`
  return new Date(t).toLocaleDateString()
}
