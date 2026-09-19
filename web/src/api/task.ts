/**
 * 任务接口（F-7-02）。
 *
 * 任务状态**以控制面记录为权威**（f-7-01 R-002）：`unknown` 与 `failed` 是
 * 不同的东西——前者是「节点失联导致结果未知」，后者是「确定失败」。
 * 界面上混为一谈会让用户去排查一个可能已经成功的操作。
 */
import { get, getPaged, post, type Pagination } from './client'

export type TaskStatus = 'pending' | 'running' | 'success' | 'failed' | 'canceled' | 'unknown'

export interface TaskView {
  id: number
  type: string
  status: TaskStatus
  node_id?: number
  owner_id?: number

  resource_type?: string
  resource_id?: number
  resource_name?: string

  progress: number
  current_stage?: string
  error?: string

  params?: unknown
  result?: unknown

  cancel_requested: boolean
  started_at?: string
  finished_at?: string
  created_at: string

  /**
   * 执行阶段流水，**仅在详情接口返回**。
   *
   * 列表不带它：一页 20 个任务就是 20 次额外查询，而列表上也显示不下一条
   * 时间线——它只会被白白查出来再丢掉。
   */
  stages?: TaskStage[]
}

/**
 * 任务的一个执行阶段。
 *
 * 阶段由**节点**上报（控制面只是记录），因此它描述的是宿主机上实际发生的事，
 * 而不是控制面按操作类型猜出来的步骤。`key` 以 `local.` 开头的是控制面自己
 * 的步骤（下发指令、回写投影），界面据此把两者区分开——出问题时第一件要判断
 * 的就是「指令到底有没有送到节点」。
 */
export interface TaskStage {
  seq: number
  key: string
  name: string
  status: 'pending' | 'running' | 'success' | 'failed' | 'skipped'
  message?: string
  /** 耗时毫秒。换算成秒是展示层的事。 */
  duration_ms: number
  started_at?: string
  finished_at?: string
  retryable: boolean
  retry_of_stage_id?: number
}

export interface TaskListParams {
  status?: string
  type?: string
  resource_id?: number
  page?: number
  page_size?: number
}

export const taskApi = {
  list: (params: TaskListParams = {}): Promise<{ items: TaskView[]; pagination: Pagination }> =>
    getPaged<TaskView>('/api/v1/tasks', {
      status: params.status,
      type: params.type,
      resource_id: params.resource_id,
      page: params.page,
      page_size: params.page_size,
    }),

  get: (id: number) => get<TaskView>(`/api/v1/tasks/${id}`),

  cancel: (id: number) => post<TaskView>(`/api/v1/tasks/${id}/cancel`),

  /**
   * 清理已完成的旧任务。
   *
   * **只清终态**（执行中与待执行的不会被删——删除一个执行中的任务会让它的
   * 结果永远无处落定），而且**被引用的任务不删**：定时任务的 last_task_id
   * 与抓包记录的 task_id 是当前状态的一部分，清掉它们指向的任务之后那些
   * 引用会悬空。
   *
   * 默认保留 7 天：用户点「清理」多半是想清掉旧的，而不是「把刚才那条也删了」。
   */
  clear: (keepDays = 7) =>
    post<{ cleared: number; before: string; keep_days: number }>('/api/v1/tasks/clear', {
      keep_days: keepDays,
    }),
}

/** 任务是否仍在进行（决定是否需要轮询刷新）。 */
export function isActive(status: TaskStatus): boolean {
  return status === 'pending' || status === 'running' || status === 'unknown'
}
