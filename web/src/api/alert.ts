/**
 * 告警中心（F-8-07）。
 *
 * 与工作台横幅同源，但用途不同：横幅是首页的一瞥，这里是能翻、能确认、
 * 能事后复盘的一份清单。
 */
import { get, post } from './client'

export interface AlertItem {
  id: number
  node_id?: number
  kind: string
  level: 'warning' | 'danger'
  title: string
  detail?: string
  /** active 未确认 / acked 已确认（问题仍在）/ 列表里不返回已关闭的。 */
  status: 'active' | 'acked'
  resource?: string
  first_at: string
  last_at: string
  ack_at?: string
}

export interface AlertList {
  items: AlertItem[]
  /** 未确认条数与其中的严重条数，用于界面徽标。 */
  active: number
  danger: number
}

/** 告警种类 → 中文名。新增种类时**必须同步这里**，否则界面会显示原始 key。 */
export const ALERT_KIND_LABEL: Record<string, string> = {
  node_offline: '节点离线',
  node_pending: '节点未接入',
  node_maintenance: '节点维护中',
  vm_missing: '虚拟机已失效',
  task_failed: '任务失败',
  quota_limited: '配额超限',
  storage_low: '存储不足',
  host_cpu_high: 'CPU 使用率偏高',
  host_memory_high: '内存使用率偏高',
}

export const alertApi = {
  list: (params: { status?: string; level?: string } = {}) =>
    get<AlertList>('/api/v1/alerts', { status: params.status, level: params.level }),

  ack: (id: number) => post<{ ok: boolean }>(`/api/v1/alerts/${id}/ack`),

  ackAll: () => post<{ acknowledged: number }>('/api/v1/alerts/ack-all'),
}
