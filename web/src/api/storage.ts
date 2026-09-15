/**
 * 存储池接口（F-5-01）。
 *
 * 两点需要在界面上体现：
 *   - `usable` 只排除**绝不允许**的情况（系统盘、已挂载）。含数据的设备
 *     仍然是 usable 的——它需要用户显式确认，而不是被直接禁止；
 *   - `stale` 为 true 时空间数据可能已过期。显示一个陈旧容量比不显示更
 *     危险：用户可能基于「还有 500G」去创建虚拟机，而实际早已写满。
 */
import type { StatusTone } from '@/components/common/StatusBadge'

import { del, get, patch, post } from './client'
import type { TaskRef } from './vm'

export type PoolStatus = 'ready' | 'unmounted' | 'unallocated' | 'error'

/** 块设备。由 agent 实时探测，不做缓存（设备可热插拔）。 */
export interface DiskView {
  device_id: string
  /** 当前设备路径，仅供展示——唯一标识是 device_id。 */
  path: string
  size_bytes: number
  is_system: boolean
  mounted: boolean
  has_data: boolean
  filesystem?: string
  mount_point?: string
  /** 在显式确认的前提下可用于创建存储池。 */
  usable: boolean
  /** 非空表示已被某个存储池占用。 */
  in_use_by?: string
}

export interface PoolView {
  id: number
  node_id: number
  device_id: string
  device_path?: string
  kind: string
  fs_type?: string
  mount_path?: string
  total_bytes: number
  usable_bytes: number
  is_default: boolean
  status: PoolStatus
  remark?: string
  stale: boolean
  last_reported_at?: string
  created_at: string
}

export interface CreatePoolInput {
  node_id: number
  device_id: string
  fs_type: string
  /**
   * 用户手工输入的设备路径，须与服务端认定的路径完全一致。
   *
   * 格式化不可逆，这一步的意义是让用户在动手前看清自己选中的是哪块盘；
   * 仅仅点一下「我确认」起不到这个作用。
   */
  confirm_device_name: string
  /** 设备已有数据时必须为 true。 */
  confirm_data_loss: boolean
  is_default?: boolean
}

export const storageApi = {
  disks: (nodeID: number) => get<DiskView[]>(`/api/v1/nodes/${nodeID}/disks`),

  pools: (nodeID: number) => get<PoolView[]>(`/api/v1/nodes/${nodeID}/storage-pools`),

  create: (input: CreatePoolInput) => post<TaskRef>('/api/v1/storage-pools', input),

  // 设为默认是同步操作：只改控制面元数据，不触碰宿主机。
  setDefault: (id: number, isDefault: boolean) =>
    patch<PoolView>(`/api/v1/storage-pools/${id}`, { is_default: isDefault }),

  remove: (id: number) => del<TaskRef>(`/api/v1/storage-pools/${id}`),
}

export const FS_TYPES = ['ext4', 'xfs', 'btrfs'] as const

export const POOL_STATUS_LABEL: Record<PoolStatus, string> = {
  ready: '可用',
  unmounted: '未挂载',
  unallocated: '未分配',
  error: '异常',
}

/**
 * 状态 → 语义色。
 *
 * `unmounted` 用 warning 而非 danger：它可用但需要先挂载，标红会让用户
 * 以为池坏了而去做无谓的排查。
 */
export const POOL_STATUS_TONE: Record<PoolStatus, StatusTone> = {
  ready: 'success',
  unmounted: 'warning',
  unallocated: 'info',
  error: 'danger',
}
