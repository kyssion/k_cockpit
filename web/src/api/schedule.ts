/**
 * 虚拟机定时任务（F-7-05）。
 *
 * 三种动作都复用已有的任务类型（vm.power / vm.delete），因此定时任务不需要
 * 任何新的执行能力——它只是「到点了替用户点一次按钮」。
 */
import { del, get, patch, post } from './client'

/** 定时执行的动作。 */
export type ScheduleAction = 'start' | 'shutdown' | 'delete' | 'snapshot'

/** 调度类型。 */
export type ScheduleType = 'once' | 'daily' | 'weekly'

/**
 * 上次执行的结果。
 *
 * `skipped` 是一个会真实出现的值：服务停机期间错过的时间点**不补执行**，
 * 那些点记为 skipped。界面必须把它显示出来——否则用户会以为任务没跑过，
 * 反复重建同一条定时任务。
 */
export type ScheduleResult = 'success' | 'failed' | 'skipped'

export interface VMSchedule {
  id: number
  action: ScheduleAction
  schedule_type: ScheduleType
  /** 星期列表，1=周一 … 7=周日。非每周模式为空数组。 */
  weekdays: number[]
  /** 执行时刻，形如 `03:00`。 */
  time_of_day: string

  next_run_at?: string
  last_run_at?: string
  last_result?: ScheduleResult
  /** 上次执行产生的任务标识，可据此跳到任务中心排查。 */
  last_task_id?: number
  enabled: boolean
}

export interface CreateScheduleInput {
  action: ScheduleAction
  schedule_type: ScheduleType
  /** 每周模式必填，取值 1=周一 … 7=周日。 */
  weekdays?: number[]
  /** 执行时刻，形如 `03:00`。 */
  time_of_day: string
  /** 一次性任务的日期，形如 `2026-09-20`。 */
  date?: string
  /**
   * 快照名模板，仅 snapshot 动作使用（服务端要求必填）。
   *
   * 它让自动快照在列表里能与手动快照区分：名字里带时间，一眼就能看出
   * 这批是定时建的。
   */
  snapshot_name?: string
  /** 是否保存运行现场，仅 snapshot 动作使用。 */
  include_memory?: boolean
}

export const scheduleApi = {
  list: (vmID: number) => get<{ items: VMSchedule[] }>(`/api/v1/vms/${vmID}/schedules`),

  create: (vmID: number, input: CreateScheduleInput) =>
    post<VMSchedule>(`/api/v1/vms/${vmID}/schedules`, input),

  setEnabled: (vmID: number, id: number, enabled: boolean) =>
    patch<VMSchedule>(`/api/v1/vms/${vmID}/schedules/${id}`, { enabled }),

  /** 删除的是**任务定义**，不是虚拟机。 */
  remove: (vmID: number, id: number) => del<void>(`/api/v1/vms/${vmID}/schedules/${id}`),
}

/** 动作的中文名。 */
export const SCHEDULE_ACTION_LABEL: Record<ScheduleAction, string> = {
  start: '开机',
  shutdown: '关机',
  delete: '删除虚拟机',
  snapshot: '创建快照',
}

/** 调度类型的中文名。 */
export const SCHEDULE_TYPE_LABEL: Record<ScheduleType, string> = {
  once: '一次性',
  daily: '每天',
  weekly: '每周',
}

/** 执行结果的中文名。 */
export const SCHEDULE_RESULT_LABEL: Record<ScheduleResult, string> = {
  success: '成功',
  failed: '失败',
  skipped: '已跳过',
}

/** 星期的中文名，下标即 ISO 编号（1=周一 … 7=周日）。 */
export const WEEKDAY_LABEL: Record<number, string> = {
  1: '周一',
  2: '周二',
  3: '周三',
  4: '周四',
  5: '周五',
  6: '周六',
  7: '周日',
}
