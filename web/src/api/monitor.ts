/**
 * 指标历史接口（F-8-01/02）。
 *
 * 时间是**必须显式给的**：服务端默认最近 1 小时，因为一次不带范围的全表
 * 扫描在数据攒了几个月之后会慢到让人以为接口挂了。这里同样始终带上范围，
 * 让"图上看到的那段"与"实际查的那段"一致。
 */
import { get } from './client'

export interface MetricPoint {
  at: string
  cpu_percent: number
  mem_used_mb: number
  mem_total_mb: number
  /** 网络与磁盘都是**累计值**：速率由相邻两点相减得到，由界面负责换算。 */
  net_in_bytes: number
  net_out_bytes: number
  disk_read_bytes: number
  disk_write_bytes: number
  /** 仅虚拟机有：宿主机侧没有按点记录的 IOPS。 */
  disk_iops?: number
}

/** 一个可筛选的物理设备（网卡 / 磁盘）。 */
export interface MetricDevice {
  name: string
  kind: 'net' | 'disk'
}

export interface MetricSeries {
  points: MetricPoint[]
  /** 相邻两点的间隔秒数，画图时用来判断哪里是缺口。 */
  interval_seconds: number
}

/** 时间范围预设。 */
export type RangeKey = '1h' | '6h' | '24h' | '7d'

export const RANGES: { key: RangeKey; label: string }[] = [
  { key: '1h', label: '近 1 小时' },
  { key: '6h', label: '近 6 小时' },
  { key: '24h', label: '近 24 小时' },
  { key: '7d', label: '近 7 天' },
]

const RANGE_HOURS: Record<RangeKey, number> = { '1h': 1, '6h': 6, '24h': 24, '7d': 24 * 7 }

/** rangeOf 把预设换算成 from/to。 */
export function rangeOf(key: RangeKey): { from: string; to: string } {
  const to = new Date()
  const from = new Date(to.getTime() - RANGE_HOURS[key] * 3600 * 1000)
  return { from: from.toISOString(), to: to.toISOString() }
}

export const monitorApi = {
  /**
   * 宿主机指标序列（API-230）。
   *
   * `device` 按物理设备筛选，**只影响网络与磁盘**——CPU 与内存是整机概念，
   * 一块网卡没有自己的内存。
   */
  host: (nodeID: number, key: RangeKey, device = '') => {
    const { from, to } = rangeOf(key)
    return get<MetricSeries>('/api/v1/monitor/host', { node_id: nodeID, from, to, device })
  },

  /** 该节点上可筛选的物理设备。清单来自最近一次采样。 */
  hostDevices: (nodeID: number) =>
    get<{ devices: MetricDevice[] }>('/api/v1/monitor/host/devices', { node_id: nodeID }),

  /** 虚拟机指标序列（API-231）。 */
  vm: (vmID: number, key: RangeKey) => {
    const { from, to } = rangeOf(key)
    return get<MetricSeries>(`/api/v1/vms/${vmID}/monitor`, { from, to })
  },

  /** 虚拟机在区间内的运行时长（API-232）。 */
  runtime: (vmID: number, key: RangeKey) => {
    const { from, to } = rangeOf(key)
    return get<{ seconds: number }>(`/api/v1/vms/${vmID}/runtime`, { from, to })
  },
}

/** formatBytes 把字节数变成人能读的量级。 */
export function formatBytes(v: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let n = Math.abs(v)
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  const sign = v < 0 ? '-' : ''
  return `${sign}${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`
}

/** formatMB 把 MB 数变成人能读的量级。 */
export function formatMB(v: number): string {
  return v >= 1024 ? `${(v / 1024).toFixed(1)} GB` : `${Math.round(v)} MB`
}

/** formatPercent 输出百分比。 */
export function formatPercent(v: number): string {
  return `${v.toFixed(1)}%`
}

/** formatDuration 把秒数变成人话。 */
export function formatDuration(seconds: number): string {
  if (seconds <= 0) return '0 分钟'
  const d = Math.floor(seconds / 86400)
  const h = Math.floor((seconds % 86400) / 3600)
  const m = Math.floor((seconds % 3600) / 60)
  const parts: string[] = []
  if (d > 0) parts.push(`${d} 天`)
  if (h > 0) parts.push(`${h} 小时`)
  if (m > 0 && d === 0) parts.push(`${m} 分钟`)
  return parts.join(' ') || '不到 1 分钟'
}
