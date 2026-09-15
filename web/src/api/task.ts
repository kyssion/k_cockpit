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
}

/** 任务是否仍在进行（决定是否需要轮询刷新）。 */
export function isActive(status: TaskStatus): boolean {
  return status === 'pending' || status === 'running' || status === 'unknown'
}
