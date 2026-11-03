/**
 * 错误文案：ApiError / NetworkError 显示后端或网络给出的原文，
 * 其余一律通用文案——把内部异常直接抛给用户既无用也可能泄漏实现细节。
 */
import { describe as describeSuite, expect, it } from 'vitest'

import { ApiError, NetworkError } from '@/api/client'
import { describe } from '@/utils/describe'

describeSuite('describe', () => {
  it('ApiError 用它自己的 message', () => {
    const err = new ApiError(404, 'RESOURCE_NOT_FOUND', '虚拟机不存在', 'req-1')
    expect(describe(err)).toBe('虚拟机不存在')
  })

  it('NetworkError 用网络异常文案', () => {
    expect(describe(new NetworkError(new Error('offline')))).toBe('网络异常，请检查连接后重试')
  })

  it('普通 Error 与未知值给通用文案，不泄漏内部信息也不抛错', () => {
    expect(describe(new Error('sql: no rows in result set'))).toBe('操作失败，请稍后重试')
    expect(describe('boom')).toBe('操作失败，请稍后重试')
    expect(describe(null)).toBe('操作失败，请稍后重试')
    expect(describe(undefined)).toBe('操作失败，请稍后重试')
  })

  it('形似错误的对象（有 message 字段但不是 Error）也给通用文案', () => {
    expect(describe({ message: 'looks like an error' })).toBe('操作失败，请稍后重试')
  })
})
