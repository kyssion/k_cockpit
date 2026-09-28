/**
 * 请求层（client.ts）是全部页面的数据出入口，它的行为就是"核心链路"本身：
 * 统一解包、401 登出、428 二次验证后重放。这些横切逻辑坏一处，所有页面
 * 一起坏——因此值得第一批被钉住。
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  ApiError,
  NetworkError,
  get,
  post,
  setRiskVerificationHandler,
  setUnauthorizedHandler,
} from '@/api/client'

function jsonResponse(status: number, body: unknown) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

const okBody = (data: unknown) => ({ data, request_id: 'req-1' })

beforeEach(() => {
  vi.restoreAllMocks()
  setUnauthorizedHandler(undefined as never)
  setRiskVerificationHandler(undefined as never)
})

describe('统一响应解包', () => {
  it('成功响应只返回 data 字段', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(jsonResponse(200, okBody({ hello: 'world' }))),
    )
    await expect(get('/api/v1/x')).resolves.toEqual({ hello: 'world' })
  })

  it('非 2xx 的 JSON 错误体归一成 ApiError（带 code 与 message）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(422, {
          error: { code: 'VALIDATION_FAILED', message: '名字不合法', details: [{ field: 'name', reason: '太短' }] },
          request_id: 'req-2',
        }),
      ),
    )
    const err = (await post('/api/v1/x', {}).catch((e) => e)) as ApiError
    expect(err).toBeInstanceOf(ApiError)
    expect(err.code).toBe('VALIDATION_FAILED')
    expect(err.message).toBe('名字不合法')
    expect(err.details).toEqual([{ field: 'name', reason: '太短' }])
  })

  it('网络层失败转成 NetworkError（与业务错误分开提示）', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('fetch failed')))
    const err = await get('/api/v1/x').catch((e) => e)
    expect(err).toBeInstanceOf(NetworkError)
  })
})

describe('401 处理', () => {
  it('收到 401 时触发注册的登出处理器', async () => {
    const onUnauthorized = vi.fn()
    setUnauthorizedHandler(onUnauthorized)
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue(
        jsonResponse(401, { error: { code: 'UNAUTHENTICATED', message: '未登录' }, request_id: 'r' }),
      ),
    )
    await expect(get('/api/v1/x')).rejects.toBeInstanceOf(ApiError)
    expect(onUnauthorized).toHaveBeenCalledTimes(1)
  })
})

describe('428 高风险二次验证（f-10-01）', () => {
  const challenge = {
    action: 'vm.delete',
    challenge_id: 'ch-1',
    methods: [{ method: 'totp', label: 'TOTP' }],
    expires_at: '2099-01-01T00:00:00Z',
  }

  it('验证通过后自动重放原请求（仅一次）', async () => {
    // 每次调用都造新的 Response：body 只能被读取一次，复用同一个对象
    // 会让第二次解析以网络错误收场——那测的是 mock 的坑，不是业务。
    const fetchMock = vi
      .fn()
      .mockImplementationOnce(() => Promise.resolve(jsonResponse(428, { error: { code: 'RISK', message: '需验证' }, data: challenge, request_id: 'r1' })))
      .mockImplementationOnce(() => Promise.resolve(jsonResponse(200, okBody({ deleted: true }))))
    vi.stubGlobal('fetch', fetchMock)

    setRiskVerificationHandler(vi.fn().mockResolvedValue('grant-token'))
    await expect(post('/api/v1/vms/1/delete', {})).resolves.toEqual({ deleted: true })

    expect(fetchMock).toHaveBeenCalledTimes(2)
    // 重放必须带上一次性许可头，且方法/路径/请求体与原请求一致。
    const replayed = (fetchMock.mock.calls as unknown as unknown[][])[1]?.[1] as
      | (RequestInit & { headers: Record<string, string> })
      | undefined
    expect(replayed?.headers?.['X-Risk-Grant']).toBe('grant-token')
  })

  it('用户取消验证时以 428 错误结束，不再重放', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(428, { error: { code: 'RISK', message: '需验证' }, data: challenge, request_id: 'r1' }),
    )
    vi.stubGlobal('fetch', fetchMock)
    setRiskVerificationHandler(vi.fn().mockResolvedValue(null))

    const err = (await post('/api/v1/vms/1/delete', {}).catch((e) => e)) as ApiError
    expect(err).toBeInstanceOf(ApiError)
    expect(err.status).toBe(428)
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('重放后仍 428（许可失效）时直接报错，不进入验证循环', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockImplementation(() =>
        Promise.resolve(
          jsonResponse(428, { error: { code: 'RISK', message: '需验证' }, data: challenge, request_id: 'r' }),
        ),
      ),
    )
    setRiskVerificationHandler(vi.fn().mockResolvedValue('used-up-grant'))

    const err = (await post('/api/v1/vms/1/delete', {}).catch((e) => e)) as ApiError
    expect(err).toBeInstanceOf(ApiError)
    expect(err.message).toContain('验证状态已失效')
  })
})
