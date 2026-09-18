/**
 * 抓包接口（F-4-12）。
 *
 * 两条约束决定了界面上的呈现：
 *
 *   **必须有**时长——不限时的抓包会把宿主机磁盘写满，它的失败不是"没抓到"
 *   而是"把宿主机写挂了"。
 *   **文件会过期消失**——里面有完整的流量内容（明文密码、会话令牌）。
 *   剩余时间要让用户看得到，否则他会在想下载时发现文件已经没了。
 */
import { del, get, post } from './client'

export interface CaptureView {
  id: number
  node_id: number
  vm_id?: number
  vm_name?: string
  interface?: string
  filter?: string
  duration_sec: number
  /** running（进行中） / ready（可下载） */
  status: 'running' | 'ready'
  /** 剩余秒数由服务端算好——客户端时钟不准是常态。 */
  seconds_left?: number
  size_bytes: number
  expires_at?: string
  created_at: string
  /** 文件为空时给出原因。 */
  hint?: string
}

export interface StartCaptureRequest {
  vm_id?: number
  interface: string
  filter: string
  duration_sec: number
}

export const captureApi = {
  list: (nodeID: number) => get<{ items: CaptureView[] }>('/api/v1/captures', { node_id: nodeID }),
  start: (nodeID: number, req: StartCaptureRequest) =>
    post<{ capture: CaptureView }>(`/api/v1/captures?node_id=${nodeID}`, req),
  remove: (id: number) => del<{ task?: unknown }>(`/api/v1/captures/${id}`),
}

/** 抓包时长边界，与服务端一致。 */
export const CAPTURE_DURATION = { min: 5, max: 300 }

/** formatBytes 把字节数变成人能读的量级。 */
export function formatBytes(v: number): string {
  const units = ['B', 'KB', 'MB', 'GB']
  let n = Math.abs(v)
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${v < 0 ? '-' : ''}${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`
}

/** formatExpiry 说明文件还能下载多久。 */
export function formatExpiry(at?: string): string {
  if (!at) return ''
  const t = Date.parse(at)
  if (!Number.isFinite(t)) return ''
  const left = t - Date.now()
  if (left <= 0) return '已过期'
  const h = Math.floor(left / 3600000)
  const m = Math.floor((left % 3600000) / 60000)
  if (h > 0) return `${h} 小时 ${m} 分钟后过期`
  return `${m} 分钟后过期`
}
