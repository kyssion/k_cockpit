/**
 * 端口安全接口（F-4-08）。
 *
 * 三项保护的性质**不同**，界面必须区别对待：
 *
 *   防伪造  安全边界，而且它是其它一切的前提——不防欺骗的话，网络里所有
 *           "按地址区分机器"的策略（隔离、防火墙、端口转发）都能被绕过。
 *           它不会让任何现有连接断开，因此**不该**被当成危险开关。
 *   隔离    也是安全边界，但代价大：同网段内**所有**机器之间都不通了，
 *           包括用户自己放在一起的应用集群。
 *   限速    资源保护，不是安全措施。默认不限——默认限流会在用户什么都没做
 *           的时候开始丢包，而那种丢包看起来像应用的问题。
 */
import { del, get, post } from './client'

export interface PortSecurityPolicyView {
  id: number
  node_id: number
  port_ref: string
  switch_id?: number
  vm_id?: number
  vm_name?: string

  spoofing_guard: boolean
  isolation: boolean
  pps_limit: number

  status: 'pending' | 'active' | 'failed'
  /** 为空表示尚未生效。与「已启用」是两回事。 */
  applied_at?: string
  detail?: string
  created_at: string
}

export interface CapabilityState {
  key: string
  label: string
  required: boolean
  missing: boolean
  reason?: string
  fix?: string
}

export interface PortSecurityPrecheck {
  /** 为 false 时**不能启用**。 */
  can_apply: boolean
  capabilities: CapabilityState[]
  /** 将要下发的规则，来自节点。 */
  rules: string[]
  warnings: string[]
}

export interface PortSecurityRequest {
  port_ref: string
  switch_id?: number
  vm_id?: number
  spoofing_guard: boolean
  isolation: boolean
  pps_limit: number
}

export const portSecurityApi = {
  list: (nodeID: number) =>
    get<{ items: PortSecurityPolicyView[] }>('/api/v1/port-security', { node_id: nodeID }),

  /** 预检：探测能力并给出将下发的规则。**只读**。 */
  preview: (nodeID: number, req: PortSecurityRequest) =>
    post<PortSecurityPrecheck>(`/api/v1/port-security/preview?node_id=${nodeID}`, req),

  /**
   * 配置并启用。`can_apply` 为 false 时服务端**直接拒绝**；
   * 有警告而未确认时**不执行也不报错**，而是返回预检结果。
   */
  apply: (nodeID: number, req: PortSecurityRequest, acknowledge: boolean) =>
    post<{ precheck: PortSecurityPrecheck; task_id?: string }>(
      `/api/v1/port-security?node_id=${nodeID}`,
      { ...req, acknowledge },
    ),

  disable: (id: number) => del<{ task_id?: string }>(`/api/v1/port-security/${id}`),
}

export const PS_STATUS_LABEL: Record<string, string> = {
  pending: '未生效',
  active: '已生效',
  failed: '失败',
}

/**
 * PS_LIMIT_BOUNDS 与服务端的边界保持一致。
 *
 * 界面自己先拦一道，是为了在**用户输入时**就给出提示，而不是等他点了
 * 提交才收到一个错误。服务端仍然会再校验一次——界面上的校验只是体验，
 * 不是约束。
 */
export const PS_LIMIT_BOUNDS = { min: 10, max: 1_000_000 }
