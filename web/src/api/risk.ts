/**
 * 高风险操作二次验证接口（F-10-01 / F-10-02）。
 *
 * 清单由**后端下发**（R-002），前端只渲染与提示，不硬编码第二份——
 * 两份清单一旦不同步，用户会看到「提示说要验证，但实际没验证」这类
 * 难以解释的现象。
 */
import { get, post } from './client'

/** 清单中的一条受保护操作。 */
export interface RiskAction {
  action: string
  label: string
  reason: string
}

/** 高风险判定口径与清单（只读）。 */
export interface RiskPolicy {
  criteria: string
  actions: RiskAction[]
  grant_header: string
  grant_ttl_seconds: number
}

/** 二次验证方式的绑定进度。 */
export interface SetupStatus {
  totp_enabled: boolean
  pending_setup: boolean
  recovery_code_count: number
  has_recovery_codes: boolean
  /** 已绑定的验证方式，取值与后端 risk.Method 一致。 */
  methods: string[]
  session_id: number
}

/** 验证成功后返回的一次性许可。 */
export interface GrantResult {
  grant: string
  method: string
  expires_at: string
}

export const riskApi = {
  policy: () => get<RiskPolicy>('/api/v1/security/high-risk-policy'),

  verify: (challengeID: string, method: string, code: string) =>
    post<GrantResult>('/api/v1/auth/risk-verification', {
      challenge_id: challengeID,
      method,
      code,
    }),

  setupStatus: () => get<SetupStatus>('/api/v1/auth/security-setup'),

  beginTOTP: () => post<{ otpauth_uri: string }>('/api/v1/auth/totp/setup'),

  confirmTOTP: (code: string) =>
    post<{ recovery_codes: string[]; notice: string }>('/api/v1/auth/totp/confirm', { code }),
}

/** 验证方式的中文名。后端也会返回 label，这里只作为兜底。 */
export const METHOD_LABEL: Record<string, string> = {
  totp: '验证器动态码',
  recovery_code: '恢复码',
}

/** 输入框的提示语：两种方式的输入形态差别很大。 */
export const METHOD_PLACEHOLDER: Record<string, string> = {
  totp: '6 位动态码',
  recovery_code: 'XXXX-XXXX-XXXX',
}
