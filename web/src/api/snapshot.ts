/**
 * 虚拟机快照（F-2-07）。
 *
 * 创建与恢复都是耗时操作（要复制或回滚整个磁盘镜像），因此接口只返回任务
 * 标识，进度在任务中心与列表里的状态字段上体现。
 */
import { del, get, post } from './client'
import type { TaskRef } from './vm'

/**
 * 快照种类。
 *
 * 这是从「是否含内存」与「创建时的运行态」**推导**出来的，不是用户选的：
 * 它们是虚拟化层的实现细节，用户关心的是「要不要保存运行现场」。
 */
export type SnapshotKind = 'internal' | 'external'

export type SnapshotStatus = 'creating' | 'ready' | 'restoring' | 'deleting' | 'error'

export interface Snapshot {
  id: number
  name: string
  description?: string
  kind: SnapshotKind
  include_memory: boolean
  /** 创建快照时虚拟机的状态。 */
  vm_status?: string
  size_bytes: number
  status: SnapshotStatus
  created_at: string

  has_children: boolean
  is_current: boolean

  /**
   * 是否可删除 / 可恢复。**由后端算好**。
   *
   * 界面不自己按 has_children / is_current 拼判断：那样「哪些条件下能删」
   * 这条规则会存在两份，而界面上的那份迟早与后端不一致——表现为按钮可点、
   * 但点下去必然失败。
   */
  can_delete: boolean
  can_restore: boolean
}

export interface SnapshotList {
  items: Snapshot[]
  /** 上限与已用数量一起返回，界面才能显示「已用/上限」。 */
  quota: number
  used: number
}

export interface CreateSnapshotInput {
  name: string
  description?: string
  /** 是否保存运行现场（含内存）。含内存的快照体积可能数倍于磁盘。 */
  include_memory: boolean
}

export const snapshotApi = {
  list: (vmID: number) => get<SnapshotList>(`/api/v1/vms/${vmID}/snapshots`),

  create: (vmID: number, input: CreateSnapshotInput) =>
    post<TaskRef>(`/api/v1/vms/${vmID}/snapshots`, input),

  restore: (vmID: number, id: number) =>
    post<TaskRef>(`/api/v1/vms/${vmID}/snapshots/${id}/restore`),

  remove: (vmID: number, id: number) => del<TaskRef>(`/api/v1/vms/${vmID}/snapshots/${id}`),
}

/** 快照状态的中文名。 */
export const SNAPSHOT_STATUS_LABEL: Record<SnapshotStatus, string> = {
  creating: '创建中',
  ready: '就绪',
  restoring: '恢复中',
  deleting: '删除中',
  error: '失败',
}

/** 快照状态对应的配色。 */
export const SNAPSHOT_STATUS_TONE: Record<SnapshotStatus, 'success' | 'idle' | 'warning' | 'danger'> =
  {
    creating: 'idle',
    ready: 'success',
    // 恢复中用 warning 而不是 danger：它是一个**正在进行**的动作，
    // 不是出了问题。
    restoring: 'warning',
    deleting: 'idle',
    error: 'danger',
  }

/** 快照种类的中文名。 */
export const SNAPSHOT_KIND_LABEL: Record<SnapshotKind, string> = {
  internal: '内部快照',
  external: '外部快照',
}
