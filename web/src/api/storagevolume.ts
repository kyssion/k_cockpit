/**
 * 存储卷接口（F-5-02，LVM 多盘聚合）。
 *
 * 这一块最容易想错的是**条带与镜像的区别**，而两者的方向正好相反：
 *
 *   条带（stripe）是拿**可靠性**换性能——数据分散在多块盘上，吞吐更高，
 *                 但任何一块盘故障都会让整个卷不可用，而且因为数据是分散的，
 *                 剩下那些盘上的内容也无法单独恢复。
 *   镜像（mirror）是拿**容量**换可靠性——每份数据写多遍，可容忍坏盘，
 *                 代价是物理占用变成可用容量的 MirrorCount 倍。
 *
 * 界面上必须把这两句话说得同样清楚：用户看到「用了 4 块盘」时，很自然地
 * 会以为那是 4 块盘的冗余。
 */
import { del, get, post } from './client'

export type VolumeStatus = 'active' | 'sync' | 'degraded' | 'failed'

export interface VolumeView {
  id: number
  node_id: number
  name: string
  kind: string

  /** 可用容量。 */
  size_gb: number
  /** **实际占用的物理空间**——镜像会把它变成 size × mirror_count。 */
  physical_gb: number

  stripe_count: number
  mirror_count: number
  /** 为 true 表示能容忍一块盘故障。**只有镜像能。** */
  has_redundancy: boolean

  devices: string[]
  vg_name?: string
  lv_name?: string

  status: VolumeStatus
  /** 用人话解释状态，尤其是 sync 与 degraded。 */
  status_note?: string
  created_at: string
}

export interface VolumePlan {
  /** 聚合**至少**需要的设备数（stripe × mirror）。 */
  required_devices: number
  given_devices: number
  size_gb: number
  physical_gb: number
  has_redundancy: boolean
  /** 有内容时创建需要显式确认。 */
  warnings?: string[]
}

export interface VolumeRequest {
  name: string
  size_gb: number
  stripe_count?: number
  mirror_count?: number
  devices: string[]
}

export const volumeApi = {
  list: (nodeID: number) => get<{ items: VolumeView[] }>('/api/v1/storage-volumes', { node_id: nodeID }),

  /** 预检：只看会得到什么，不产生任何改动。 */
  preview: (nodeID: number, req: VolumeRequest) =>
    post<VolumePlan>(`/api/v1/storage-volumes/preview?node_id=${nodeID}`, req),

  /**
   * 创建。`acknowledge` 为 false 时若有警告，服务端**不创建也不报错**，
   * 而是把计划原样返回——那是一个需要用户做决定的岔路口。
   */
  create: (nodeID: number, req: VolumeRequest, acknowledge: boolean) =>
    post<{ plan: VolumePlan; task?: unknown }>(`/api/v1/storage-volumes?node_id=${nodeID}`, {
      ...req,
      acknowledge,
    }),

  /** 删除。**会销毁卷里的全部数据**，不可恢复。 */
  remove: (id: number) => del<{ task: unknown }>(`/api/v1/storage-volumes/${id}`),
}

export const VOLUME_STATUS_LABEL: Record<VolumeStatus, string> = {
  active: '正常',
  sync: '同步中',
  degraded: '降级',
  failed: '故障',
}

export const VOLUME_STATUS_TONE: Record<VolumeStatus, 'success' | 'warning' | 'danger'> = {
  active: 'success',
  // sync 用 warning 而不是 success：它确实可用，但写会明显变慢几小时。
  sync: 'warning',
  // degraded 用 danger：它**看起来一切正常**却已经没有任何冗余了。
  degraded: 'danger',
  failed: 'danger',
}
