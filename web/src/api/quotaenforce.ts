/**
 * 资源配额与超限处置接口（F-4-10）。
 *
 * 用量**不在这张配额表里**：它来自按天累计的统计表，判定时现算。这里返回的
 * `used_value` 就是算出来的结果——它与处置依据同源，因此用户看到的数字与
 * "为什么被限速"是同一个东西。
 */
import { del, get, put } from './client'

export type QuotaDim = 'traffic_in' | 'traffic_out' | 'runtime'
export type QuotaStatus = 'ok' | 'warned' | 'limited'

export interface QuotaView {
  id: number
  node_id: number
  user_id: number
  username?: string

  dimension: QuotaDim
  dimension_label: string
  dimension_unit: string
  /** 0 表示不限。 */
  limit_value: number
  /** throttle（限速，默认）或 block（断网）。 */
  action: 'throttle' | 'block'

  /** 当前周期的实际用量。 */
  used_value: number
  used_percent: number

  status: QuotaStatus
  period?: string
  warned_at?: string
  /** **处置发生的时刻**，而不只是"超了"——用户问"网什么时候开始变慢的"。 */
  limited_at?: string
  detail?: string
}

export const RESOURCE_QUOTAS = {
  dimensions: [
    { value: 'traffic_in', label: '月入站流量', unit: 'GB' },
    { value: 'traffic_out', label: '月出站流量', unit: 'GB' },
    { value: 'runtime', label: '月运行时长', unit: '小时' },
  ],
  /** 预警阈值（与服务端一致）。 */
  warnPercent: 80,
}

export const quotaEnforceApi = {
  list: (nodeID: number) =>
    get<{ items: QuotaView[] }>('/api/v1/resource-quotas', { node_id: nodeID }),

  /** 设置配额。**提高上限会清除该维度的处置状态**。 */
  set: (
    nodeID: number,
    req: { user_id: number; dimension: string; limit_value: number; action: string },
  ) => put<QuotaView>(`/api/v1/resource-quotas?node_id=${nodeID}`, req),

  remove: (id: number) => del<{ ok: boolean }>(`/api/v1/resource-quotas/${id}`),
}
