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

  /**
   * 按**投影状态**算出的可用电源操作。
   *
   * 可能与实际不一致（投影滞后），此时后端受理时会基于实时探测拒绝并说明
   * 原因。`stale` 为 true 时不应完全依赖它。
   */
  available_actions: PowerAction[]
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

/** 创建/电源/删除的结果：都是任务标识，操作本身异步执行。 */
export interface TaskRef {
  task_id: number
  status: TaskStatus
}

/**
 * 电源动作。
 *
 * `shutdown`（优雅关机，等来宾配合）与 `poweroff`（强制断电）是**两个独立
 * 操作**：系统不会在关机超时后自动降级为断电，因为静默强杀可能造成来宾
 * 文件系统损坏（f-2-01 R-006）。
 */
export type PowerAction = 'start' | 'shutdown' | 'reboot' | 'poweroff' | 'reset'

/** 磁盘处理方式。没有默认值——必须由用户显式选择（f-2-01 R-009）。 */
export type DiskAction = 'delete' | 'keep'

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

  create: (input: CreateVmInput) => post<TaskRef>('/api/v1/vms', input),

  power: (id: number, action: PowerAction) =>
    post<TaskRef>(`/api/v1/vms/${id}/power-actions`, { action }),

  // 磁盘处理方式走查询参数：DELETE 携带 body 并非所有客户端都支持，
  // 走 query 更稳妥（服务端两种都接受）。
  remove: (id: number, diskAction: DiskAction) =>
    del<TaskRef>(`/api/v1/vms/${id}?disk_action=${diskAction}`),
}
