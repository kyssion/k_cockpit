/**
 * 服务端日志接口（F-9-02）。
 *
 * 导出的内容是**脱敏后**的：脱敏发生在写入时，因此磁盘上那份本身就
 * 不含明文（见 internal/logging）。这意味着导出接口不需要再做一遍处理，
 * 也**不可能**因为"忘了处理"而泄漏。
 */
import { get, post, put } from './client'

export interface LogEntry {
  at: string
  level: 'DEBUG' | 'INFO' | 'WARN' | 'ERROR'
  line: string
}

export interface RotatedFile {
  name: string
  size_bytes: number
  mod_time: string
}

export interface LogStatus {
  level: string
  dir?: string
  file_path?: string
  file_size: number
  max_size_bytes: number
  keep_files: number
  rotated: RotatedFile[] | null
  ring_lines: number
  /** 各级别的条数（仅内存范围）——回答"最近有没有错误"。 */
  lines: Record<string, number> | null
}

export const LOG_LEVELS = [
  { value: 'debug', label: 'DEBUG（最详细，日志量最大）' },
  { value: 'info', label: 'INFO（默认）' },
  { value: 'warn', label: 'WARN' },
  { value: 'error', label: 'ERROR（只看错误）' },
]

export const logApi = {
  status: () => get<LogStatus>('/api/v1/settings/log/status'),

  /** 在线查看只读内存，不扫磁盘——它要即时响应。 */
  read: (q: { limit?: number; level?: string; keyword?: string }) =>
    get<{ items: LogEntry[] }>('/api/v1/settings/log/read', {
      limit: q.limit ?? 300,
      level: q.level ?? '',
      keyword: q.keyword ?? '',
    }),

  setLevel: (level: string) => put<{ level: string }>('/api/v1/settings/log/level', { level }),

  /** 清理：删轮转文件并截断当前文件（**不删除文件本身**）。 */
  purge: () => post<{ freed_bytes: number }>('/api/v1/settings/log/delete', {}),

  /** 交给浏览器下载：同源带会话 Cookie，自己处理大文件落盘。 */
  exportUrl: () => '/api/v1/settings/log/export',
}

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
