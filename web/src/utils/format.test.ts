/**
 * 展示层格式化工具：错一个单位（KB/MB）整页数字都会骗人，值得钉住。
 */
import { describe, expect, it } from 'vitest'

import { formatBytes, relativeTime } from '@/utils/format'

describe('formatBytes', () => {
  it('按 1024 进制换算并带单位', () => {
    expect(formatBytes(0)).toBe('0 B')
    expect(formatBytes(1024)).toBe('1.0 KB')
    expect(formatBytes(1536)).toBe('1.5 KB')
    expect(formatBytes(1024 * 1024 * 1024)).toBe('1.0 GB')
  })

  it('空值与 0 统一显示 0 B（与负值一致，不抛错）', () => {
    expect(formatBytes(undefined)).toBe('0 B')
    expect(formatBytes(null)).toBe('0 B')
    expect(formatBytes(-1)).toBe('0 B')
  })
})

describe('relativeTime', () => {
  it('给出人能读懂的相对时长', () => {
    const now = Date.parse('2026-09-28T12:00:00Z')
    expect(relativeTime('2026-09-28T11:59:30Z', now)).toBe('30 秒前')
    expect(relativeTime('2026-09-28T11:00:00Z', now)).toBe('1 小时前')
  })

  it('空值与非法时间显示占位，不抛错', () => {
    expect(relativeTime(undefined)).toBe('—')
    expect(relativeTime('not-a-date')).toBe('—')
  })
})
