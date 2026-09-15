/**
 * 格式化工具（FRONTEND §4.3）。
 *
 * 表格里的时间一律用相对时间：运维关心的是「多久没心跳了」，
 * 而不是「最后一次心跳是 2026-09-15 22:44:27」——后者还需要用户自己做减法。
 */

/** 把时间格式化为「刚刚 / N 秒前 / N 分钟前 / N 小时前 / N 天前」。 */
export function relativeTime(iso: string | undefined, now = Date.now()): string {
  if (!iso) return '—'

  const at = new Date(iso).getTime()
  if (Number.isNaN(at)) return '—'

  const seconds = Math.floor((now - at) / 1000)
  if (seconds < 0) return '刚刚'
  if (seconds < 10) return '刚刚'
  if (seconds < 60) return `${seconds} 秒前`

  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟前`

  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours} 小时前`

  const days = Math.floor(hours / 24)
  if (days < 30) return `${days} 天前`

  return formatDateTime(iso)
}

/**
 * 把字节数格式化为可读容量。
 *
 * 用 1024 进制：存储设备的标称容量是十进制（1 TB = 1000 GB），而操作系统
 * 与虚拟化层报告的可用空间是二进制。界面跟随后者——用户要判断的是「还剩
 * 多少能用」，而不是「按厂商口径应该是多少」。
 */
export function formatBytes(bytes: number | undefined | null): string {
  if (bytes === undefined || bytes === null || bytes <= 0) return '0 B'

  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  let value = bytes
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit++
  }

  // 小于 10 时保留一位小数（1.5 GB 与 1 GB 对判断差别很大），
  // 大于 10 时取整（「512.3 GB」是没有意义的精度）。
  const digits = unit === 0 ? 0 : value < 10 ? 1 : 0
  return `${value.toFixed(digits)} ${units[unit]}`
}

/** 把时间格式化为本地日期时间，用于需要精确值的场景（如详情页）。 */
export function formatDateTime(iso: string | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'

  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`
}
