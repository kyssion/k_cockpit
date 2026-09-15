/**
 * 节点接口（F-6-01 / F-6-02）。
 *
 * 注意 `status` 与 `enroll_state` 是两件事：
 *   - `enroll_state` 是元数据——该节点是否已接入（pending / enrolled）；
 *   - `status` 是**运行态**，由心跳时间推导。
 * 一个 enrolled 的节点可能处于 offline，一个 pending 的节点则必然是 unknown。
 */
import { del, get, post } from './client'

export type NodeStatus = 'online' | 'offline' | 'unknown'
export type EnrollState = 'pending' | 'enrolled'

export interface NodeView {
  id: number
  name: string
  status: NodeStatus
  enroll_state: EnrollState
  enabled: boolean
  maintenance_mode: boolean
  agent_version?: string
  protocol_version: number
  capabilities?: string[]
  last_heartbeat_at?: string
  last_error?: string
  remark?: string
  created_at: string
}

export interface EnrollTokenResult {
  /** 明文令牌，**只返回一次**；库里存的是哈希。 */
  token: string
  expires_at: string
  node: NodeView
  /** agent 安装命令；agent 尚未实现，接入后填充。 */
  install_command: string
  /** 开发期的模拟注册命令（仅 AGENT_TRANSPORT=mock 时返回）。 */
  simulate_command?: string
}

export const nodeApi = {
  list: () => get<NodeView[]>('/api/v1/nodes'),

  get: (id: number) => get<NodeView>(`/api/v1/nodes/${id}`),

  /** 生成一次性注册令牌并创建待接入节点。ttlHours 为 0 时用后端默认值（24h）。 */
  createEnrollToken: (name: string, ttlHours = 0) =>
    post<EnrollTokenResult>('/api/v1/nodes/registration-tokens', {
      name,
      ttl_hours: ttlHours,
    }),

  remove: (id: number) => del<void>(`/api/v1/nodes/${id}`),
}
