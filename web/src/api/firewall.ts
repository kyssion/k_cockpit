/**
 * 防火墙接口（F-4-11）。
 *
 * 界面上必须表达清楚的一条：
 *
 *   **防火墙最重要的性质不是「能挡住什么」，而是「不会把管理员挡在外面」。**
 *
 * 一次配错无法通过面板恢复——那时已经连不上了，只能上宿主机敲命令。因此
 * 界面上有两处不太一样的地方：
 *
 *   - 保护规则**不给删除按钮**（服务端还会再拦一次）；
 *   - 「紧急回滚」按钮**不弹确认框**，一键关闭。
 */
import { del, get, patch, post, put } from './client'
import type { Protocol } from './securitygroup'

export type FirewallAction = 'accept' | 'deny'

export interface PolicyView {
  node_id: number
  enabled: boolean
  default_action: FirewallAction
  geoip_regions: string[]
  whitelist: string[]
  version: number
  applied_at?: string
  /** 有改动还没下发。 */
  pending: boolean
  /** 尚未下发的规则条数——比一句「有改动」有用得多。 */
  unapplied_rules: number
}

export interface FirewallRuleView {
  id: number
  action: FirewallAction
  protocol: Protocol
  port_start?: number
  port_end?: number
  source_cidr?: string
  /** 系统保护规则：保护管理通道，**不可删除**。 */
  is_protected: boolean
  order_no: number
  applied: boolean
  remark?: string
}

export interface VMPolicyView {
  vm_id: number
  vm_name: string
  /** node = 用节点基线；vm = 有自己的覆盖。 */
  source: 'node' | 'vm'
  enabled: boolean
  default_action: FirewallAction
  geoip_regions: string[]
  whitelist: string[]
  overridden: boolean
  rules: FirewallRuleView[]
  warnings?: string[]
}

export interface PrecheckResult {
  warnings: string[]
}

export interface ApplyResult {
  version: number
  warnings?: string[]
  /** 为 false 表示因为有未确认的警告而没有下发。 */
  applied: boolean
}

export const firewallApi = {
  getPolicy: (nodeID: number) => get<PolicyView>('/api/v1/firewall/policy', { node_id: nodeID }),

  /**
   * 更新策略。**只改配置，不下发**——下发有真实的网络影响，而改配置不该有。
   */
  updatePolicy: (
    nodeID: number,
    input: {
      enabled?: boolean
      default_action?: FirewallAction
      geoip_regions?: string[]
      whitelist?: string[]
    },
  ) => patch<PolicyView>(`/api/v1/firewall/policy?node_id=${nodeID}`, input),

  listRules: (nodeID: number) => get<{ items: FirewallRuleView[] }>('/api/v1/firewall/rules', { node_id: nodeID }),

  createRule: (
    nodeID: number,
    input: {
      action: FirewallAction
      protocol: Protocol
      port_start?: number
      port_end?: number
      source_cidr?: string
      order_no?: number
      remark?: string
    },
  ) => post<FirewallRuleView>(`/api/v1/firewall/rules?node_id=${nodeID}`, input),

  /** 删除规则。**保护规则会被服务端拒绝**——不可恢复的操作不能交给一次点击。 */
  deleteRule: (nodeID: number, ruleID: number) =>
    del<{ deleted: boolean }>(`/api/v1/firewall/rules/${ruleID}?node_id=${nodeID}`),

  /** 预检：这次改动会不会切断管理通道。 */
  precheck: (nodeID: number) => get<PrecheckResult>('/api/v1/firewall/precheck', { node_id: nodeID }),

  /**
   * 下发策略。
   *
   * `acknowledge` 为 false 时若预检有警告，服务端**不下发也不报错**，
   * 而是把警告返回——那是一个需要用户做决定的岔路口，不是一次失败。
   */
  apply: (nodeID: number, acknowledge: boolean) =>
    post<ApplyResult>(`/api/v1/firewall/apply?node_id=${nodeID}`, { acknowledge }),

  /**
   * 紧急回滚：一键关闭。**不做任何确认**——见文件头说明。
   */
  rollback: (nodeID: number) =>
    post<ApplyResult>(`/api/v1/firewall/rollback?node_id=${nodeID}`, {}),

  getVMPolicy: (vmID: number) => get<VMPolicyView>(`/api/v1/vms/${vmID}/firewall`),

  setVMPolicy: (
    vmID: number,
    input: { action?: FirewallAction; whitelist?: string; geoip_regions?: string[]; enabled?: boolean },
  ) => put<VMPolicyView>(`/api/v1/vms/${vmID}/firewall`, input),

  clearVMPolicy: (vmID: number) => del<VMPolicyView>(`/api/v1/vms/${vmID}/firewall`),
}

export const ACTION_LABEL: Record<FirewallAction, string> = {
  accept: '放行',
  deny: '拒绝',
}

/** 把一条规则渲染成可读的一行。 */
export function ruleText(r: {
  protocol: string
  port_start?: number
  port_end?: number
  source_cidr?: string
}): string {
  const port =
    r.port_start == null
      ? ''
      : r.port_end != null && r.port_end !== r.port_start
        ? `:${r.port_start}-${r.port_end}`
        : `:${r.port_start}`
  return `${r.protocol.toUpperCase()}${port} ← ${r.source_cidr || '任意来源'}`
}
