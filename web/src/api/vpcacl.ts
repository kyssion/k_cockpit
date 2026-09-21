import { del, get, post, put } from './client'

/**
 * VPC 网络的访问控制规则（F-4-05）。
 *
 * 作用域是**网段**（交换机），与安全组（挂在虚拟机网口上）并列但不同：
 * "这个网段整体上不许访问某个地址"用 ACL 表达一次即可，逐台配安全组既
 * 重复又容易漏。
 */

export interface ACLRuleView {
  id: number
  node_id: number
  switch_id?: number
  priority: number
  action: 'allow' | 'deny'
  direction: 'in' | 'out'
  protocol: string
  src_cidr?: string
  dst_cidr?: string
  port_start?: number
  port_end?: number
  enabled: boolean
  remark?: string
  /** 这条匹配全部地址（配合 deny 会遮住后面的规则）。 */
  matches_all: boolean
}

export interface ACLRuleInput {
  node_id: number
  switch_id?: number
  priority: number
  action: string
  direction: string
  protocol: string
  src_cidr?: string
  dst_cidr?: string
  port_start?: number
  port_end?: number
  enabled?: boolean
  remark?: string
}

export interface ACLPreviewView {
  node_id: number
  switch_id?: number
  rules: ACLRuleView[]
  /** 节点上真正会生成的条目——规则集到生效之间隔着一层归并，必须展示。 */
  rendered: string[]
  warnings: string[]
  /** 应用必须带回它：预览之后规则若变了，按旧预览去应用等于用没看过的结论改网络。 */
  version: string
  unavailable?: string
}

export const ACL_ACTION_LABEL: Record<string, string> = {
  allow: '允许',
  deny: '拒绝',
}

export const ACL_DIRECTION_LABEL: Record<string, string> = {
  in: '进入网段',
  out: '离开网段',
}

export const aclApi = {
  list: (nodeID: number, switchID?: number) =>
    get<{ items: ACLRuleView[] }>('/api/v1/vpc-acl', { node_id: nodeID, switch_id: switchID }),

  create: (input: ACLRuleInput) => post<ACLRuleView>('/api/v1/vpc-acl', input),
  update: (id: number, input: ACLRuleInput) => put<ACLRuleView>(`/api/v1/vpc-acl/${id}`, input),
  remove: (id: number) => del<void>(`/api/v1/vpc-acl/${id}`),

  preview: (nodeID: number, switchID?: number) =>
    get<ACLPreviewView>('/api/v1/vpc-acl/preview', { node_id: nodeID, switch_id: switchID }),

  apply: (input: { node_id: number; switch_id?: number; version: string; acknowledge: boolean }) =>
    post<{ task_id: number; status: string }>('/api/v1/vpc-acl/apply', input),
}
