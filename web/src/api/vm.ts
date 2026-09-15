/**
 * 虚拟机接口（F-2-01 ~ F-2-04）。
 *
 * 两点需要留意：
 *   - `status` 是**投影字段**，权威在虚拟化层，控制面只是缓存。配合 `stale`
 *     一起看：`stale=true` 说明数据可能已经过期，界面必须提示而不是照常显示。
 *   - 创建是**异步**的：接口返回任务标识而非创建好的虚拟机，前端据任务进度
 *     跟踪结果（f-7-01 R-001）。
 */
import { del, get, getPaged, post, type Pagination } from './client'
import type { TaskStatus } from './task'

export type VmStatus = 'running' | 'stopped' | 'paused' | 'suspended' | 'error' | 'unknown'

export interface VmView {
  id: number
  node_id: number
  name: string
  uuid?: string
  owner_id?: number

  status: VmStatus
  vcpu: number
  memory_mb: number
  disk_gb: number
  ip_summary?: string

  remark?: string
  group_name?: string

  present: boolean
  last_synced_at?: string
  /** 投影数据可能已过期：界面应提示「数据可能陈旧」。 */
  stale: boolean
  created_at: string
}

export interface VmListParams {
  status?: string
  keyword?: string
  node_id?: number
  group_name?: string
  page?: number
  page_size?: number
}

export interface CreateVmInput {
  name: string
  node_id: number
  vcpu: number
  memory_mb: number
  disk_gb: number
  remark?: string
  group_name?: string
}

/** 创建结果：返回任务标识，而非创建好的虚拟机。 */
export interface CreateVmResult {
  task_id: number
  status: TaskStatus
}

export const vmApi = {
  list: (params: VmListParams = {}): Promise<{ items: VmView[]; pagination: Pagination }> =>
    getPaged<VmView>('/api/v1/vms', {
      status: params.status,
      keyword: params.keyword,
      node_id: params.node_id,
      group_name: params.group_name,
      page: params.page,
      page_size: params.page_size,
    }),

  get: (id: number) => get<VmView>(`/api/v1/vms/${id}`),

  create: (input: CreateVmInput) => post<CreateVmResult>('/api/v1/vms', input),

  remove: (id: number) => del<void>(`/api/v1/vms/${id}`),
}
