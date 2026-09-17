/**
 * 安全组接口（F-4-03 / F-4-04）。
 *
 * 两件界面必须如实表达的事：
 *
 * 1. **组内只有「允许」规则**。叠加生效意味着生效规则是各组的并集，而并集里
 *    没有「拒绝」的位置——A 组拒绝 22、B 组允许 22，合并后通不通取决于谁先算。
 *    默认拒绝由组的整体语义给出：不在任何允许规则里的流量一律不通。
 * 2. **预览与应用之间绑定版本**。用户确认的必须正是他看到的那一套规则。
 */
import { del, get, patch, post } from './client'

export type Direction = 'ingress' | 'egress'
export type Protocol = 'tcp' | 'udp' | 'icmp' | 'all'
export type TargetType = 'cidr' | 'switch' | 'group'

export interface GroupView {
  id: number
  node_id: number
  owner_id?: number
  name: string
  is_default: boolean
  remark?: string
  rule_count: number
  /** 挂载它的网口数。删除前要告诉用户这个数字。 */
  attached_count: number
  created_at: string
}

export interface RuleView {
  id: number
  group_id: number
  direction: Direction
  protocol: Protocol
  port_start?: number
  port_end?: number
  target_type: TargetType
  target_value?: string
  address_family: string
  /** 只影响**展示顺序**，不影响判定。 */
  priority: number
  remark?: string
  created_at: string
}

export interface EffectiveRule {
  direction: Direction
  protocol: Protocol
  port_start?: number
  port_end?: number
  target_type: TargetType
  target_value?: string
  address_family: string
  /** 贡献了这条规则的组名。必看——否则不知道该去哪儿改。 */
  sources: string[]
}

export interface EffectivePreview {
  vm_id: number
  vm_name: string
  /** 这份快照的指纹。应用时必须带回去。 */
  version: string
  groups: string[]
  rules: EffectiveRule[]
  warnings?: string[]
  interface_count: number
}

export interface RuleInput {
  direction: Direction
  protocol: Protocol
  port_start?: number
  port_end?: number
  target_type: TargetType
  target_value?: string
  priority?: number
  remark?: string
}

export const securityGroupApi = {
  list: (nodeID = 0) =>
    get<{ items: GroupView[] }>(
      '/api/v1/security-groups',
      nodeID > 0 ? { node_id: nodeID } : undefined,
    ),

  create: (input: { node_id: number; name: string; remark?: string }) =>
    post<GroupView>('/api/v1/security-groups', input),

  /**
   * 删除安全组。**仍被网口挂载时会被拒绝**——删掉一个正在被使用的组会
   * 静默地放开一批机器的流量，那是一次方向与预期相反的变更。
   */
  remove: (id: number) => del<{ deleted: boolean }>(`/api/v1/security-groups/${id}`),

  listRules: (groupID: number) =>
    get<{ items: RuleView[] }>(`/api/v1/security-groups/${groupID}/rules`),

  createRule: (groupID: number, input: RuleInput) =>
    post<RuleView>(`/api/v1/security-groups/${groupID}/rules`, input),

  updateRule: (groupID: number, ruleID: number, input: RuleInput) =>
    patch<RuleView>(`/api/v1/security-groups/${groupID}/rules/${ruleID}`, input),

  deleteRule: (groupID: number, ruleID: number) =>
    del<{ deleted: boolean }>(`/api/v1/security-groups/${groupID}/rules/${ruleID}`),

  /** 把安全组挂到网口上（附加组）。 */
  attach: (groupID: number, interfaceID: number) =>
    post<{ attached: boolean }>(
      `/api/v1/security-groups/${groupID}/interfaces/${interfaceID}`,
      {},
    ),

  /** 解除挂载。**主组不能从这里解除**——那等于把网口变成「没有安全组」。 */
  detach: (groupID: number, interfaceID: number) =>
    del<{ detached: boolean }>(
      `/api/v1/security-groups/${groupID}/interfaces/${interfaceID}`,
    ),

  /**
   * 汇总一台虚拟机的生效规则（F-4-04）。
   *
   * 多组叠加、去重，并标出每条规则来自哪些组。返回的 `version` 要在应用时
   * 带回去。
   */
  effective: (vmID: number) =>
    get<EffectivePreview>(`/api/v1/vms/${vmID}/security-groups/effective`),

  /**
   * 应用生效规则。`version` 必须与预览时一致。
   *
   * 版本不匹配会被拒绝而**不会**自动用最新规则继续——自动继续等于替用户
   * 批准了他没看过的东西，那正是要防的事。
   */
  apply: (vmID: number, version: string) =>
    post<{ task_id: number; status: string }>(
      `/api/v1/vms/${vmID}/security-groups/apply`,
      { version },
    ),
}

export const DIRECTION_LABEL: Record<Direction, string> = {
  ingress: '入站',
  egress: '出站',
}

export const PROTOCOL_LABEL: Record<Protocol, string> = {
  tcp: 'TCP',
  udp: 'UDP',
  icmp: 'ICMP',
  all: '全部',
}

/**
 * 协议是否使用端口。
 *
 * ICMP 没有端口概念，`all` 覆盖全部协议因而也不能限定端口——填了会得到一条
 * **看起来有限制、实际没有**的规则。界面上直接禁用端口输入框，而不是等后端
 * 拒绝：那样用户会以为是自己填错了格式。
 */
export function protocolUsesPorts(p: Protocol): boolean {
  return p === 'tcp' || p === 'udp'
}

export const TARGET_LABEL: Record<TargetType, string> = {
  cidr: '网段',
  switch: '交换机',
  group: '安全组',
}

/** 把一条规则渲染成可读的一行。 */
export function ruleText(r: {
  protocol: Protocol
  port_start?: number
  port_end?: number
  target_type: TargetType
  target_value?: string
}): string {
  const proto = PROTOCOL_LABEL[r.protocol] ?? r.protocol
  let port = ''
  if (protocolUsesPorts(r.protocol) && r.port_start != null) {
    port =
      r.port_end != null && r.port_end !== r.port_start
        ? `:${r.port_start}-${r.port_end}`
        : `:${r.port_start}`
  }
  const target = r.target_value ? `${TARGET_LABEL[r.target_type]} ${r.target_value}` : '任意来源'
  return `${proto}${port} ← ${target}`
}
