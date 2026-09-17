/**
 * API 凭证接口（F-1-10）。
 *
 * 界面上必须显眼说明的一件事：
 *
 *   **用 API Key 调用接口时不会触发二次验证。**
 *
 * 这不是缺陷而是必要的取舍——二次验证要求「有人在那一端输入」，而 API 的
 * 使用场景恰恰是「没有人在那一端」。但它的代价必须让用户看见：
 *
 *   一个泄漏的 Key 等同于**绕过所有高风险操作的验证**——删除虚拟机、
 *   解锁业务软锁、开放防火墙端口，那些在浏览器里都要过一次验证码的
 *   操作，用 Key 调用时直接执行。
 *
 * 因此界面把「绑定来源 IP」与「设置到期」放在显眼位置，并在没有限制时
 * 明确提示风险。
 */
import { del, get, post } from './client'

export interface APIKeyView {
  /** 为 false 表示还没生成过。 */
  exists: boolean
  /** 明文前几位，用于识别是哪一个。 */
  prefix?: string
  /** 为空表示不限来源。 */
  allowed_ips: string[]
  /** 为空表示永不过期。 */
  expires_at?: string
  expired: boolean
  revoked_at?: string
  created_at?: string
  last_used_at?: string
  /** 当前是否可用（未撤销、未过期）。 */
  usable: boolean
}

export interface APIKeyCreated extends APIKeyView {
  /**
   * 明文凭证。**唯一一次出现的地方**——库里只存哈希，服务端之后再也拿不到
   * 它。界面必须明确告知「现在就复制」。
   */
  plain_key: string
}

export const apiKeyApi = {
  get: () => get<APIKeyView>('/api/v1/api-keys'),

  /**
   * 生成或**轮换**。
   *
   * 一人一个（数据库唯一索引），因此重新生成即轮换、**旧的立即失效**。
   * 这一点必须在界面上说清楚，否则用户会以为新旧能并存，直到某个自动化
   * 任务突然开始报 401。
   */
  create: (input: { allowed_ips?: string[]; expires_in_days?: number }) =>
    post<APIKeyCreated>('/api/v1/api-keys', input),

  /** 撤销。记录会保留，供事后追查「什么时候被撤掉的」。 */
  revoke: () => del<{ revoked: boolean }>('/api/v1/api-keys'),
}

/**
 * 凭证认证下**不会**触发二次验证的操作，用于界面提示。
 *
 * 这份清单不是完整的（每个高风险动作都在 risk 包里声明），但它足以让用户
 * 意识到「这个凭据的分量」。
 */
export const UNVERIFIED_EXAMPLES = [
  '删除虚拟机',
  '解锁业务软锁',
  '开放防火墙端口',
  '恢复快照',
]
