import { ApiError, NetworkError } from '@/api/client'

/**
 * 把未知错误转成可展示的文案。
 *
 * 只认两种：`ApiError`（后端给出的稳定错误码与可读文案）与 `NetworkError`
 * （请求没到达服务端）。其余一律给通用文案——内部异常直接展示给用户既
 * 帮不上忙，也可能泄漏实现细节。
 *
 * 放在 `utils/` 而不是某个页面：虚拟机详情页有 12 个组件调用它，其它域
 * 组合同样需要，与虚拟化领域无关。
 */
export function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
