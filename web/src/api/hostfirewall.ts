/**
 * 宿主机防火墙接口（F-4-11 第一层）。
 *
 * **与 `/firewall/*`（KVM 网络防火墙）是两套。** 这一套保护的是**宿主机自己
 * 与面板**——SSH、面板端口；那一套管的是虚拟机的入站流量。它们的配置项长得
 * 几乎一样，而攻击面完全不同：改 KVM 规则**不会**关掉面板的暴露面。
 *
 * 本层比 KVM 那层危险：它挡住的正是 SSH 与面板本身，没有"从里面绕过去"的
 * 余地。因此回滚接口**不要求二次验证**——它要在"已经出事了"的那一刻还能用。
 */
import { del, get, patch, post } from './client'

export interface HostFirewallRule {
  id: number
  node_id: number
  action: 'accept' | 'deny'
  protocol: string
  port_start?: number
  port_end?: number
  source_cidr?: string
  geoip_regions?: string[]
  /** 不可修改与删除——服务端强制。 */
  is_protected: boolean
  applied: boolean
  remark?: string
  /** 为 true 表示这条**不在表里**，由面板按当前配置合成。 */
  fixed: boolean
  /** 合成规则要说明"为什么不能改"。 */
  fixed_reason?: string
  describe: string
}

export interface HostFirewallPolicy {
  node_id: number
  enabled: boolean
  default_action: 'accept' | 'deny'
  whitelist: string[]
  version: number
  applied_at?: string
  /** 最近一次紧急回滚的时刻——排查"为什么规则和我配的不一样"时先看它。 */
  last_rollback_at?: string
  need_apply: boolean
}

export interface HostConnection {
  remote_addr: string
  local_port: number
  protocol: string
  state: string
  process: string
  /** 这条连接来自当前请求方——关掉它会让你以为面板挂了。 */
  own: boolean
}

export interface PrecheckResult {
  preview: string[]
  warnings: string[]
}

export const hostFirewallApi = {
  get: (nodeID: number) =>
    get<{ policy: HostFirewallPolicy; rules: HostFirewallRule[] }>('/api/v1/host-firewall', {
      node_id: nodeID,
    }),

  updatePolicy: (
    nodeID: number,
    req: { enabled?: boolean; default_action?: string; whitelist?: string },
  ) => patch<HostFirewallPolicy>(`/api/v1/host-firewall/policy?node_id=${nodeID}`, req),

  createRule: (
    nodeID: number,
    req: {
      action: string
      protocol: string
      port_start?: number
      port_end?: number
      source_cidr?: string
      remark?: string
    },
  ) => post<HostFirewallRule>(`/api/v1/host-firewall/rules?node_id=${nodeID}`, req),

  deleteRule: (nodeID: number, id: number) =>
    del<{ ok: boolean }>(`/api/v1/host-firewall/rules/${id}?node_id=${nodeID}`),

  precheck: (nodeID: number) =>
    get<PrecheckResult>(`/api/v1/host-firewall/precheck?node_id=${nodeID}`),

  /** **必须带回预览时的 version**——没有"不传就跳过校验"这一说。 */
  apply: (nodeID: number, version: number) =>
    post<{ task: unknown }>(`/api/v1/host-firewall/apply?node_id=${nodeID}`, { version }),

  /** 紧急回滚：撤销本系统写入的全部规则。**唯一的自救入口。** */
  rollback: (nodeID: number) =>
    post<{ task: unknown }>(`/api/v1/host-firewall/rollback?node_id=${nodeID}`, {}),

  connections: (nodeID: number) =>
    get<{ items: HostConnection[] }>(`/api/v1/host-firewall/connections`, { node_id: nodeID }),

  closeConnection: (nodeID: number, remoteAddr: string) =>
    post<{ ok: boolean }>(`/api/v1/host-firewall/connections/close?node_id=${nodeID}`, {
      remote_addr: remoteAddr,
    }),
}
