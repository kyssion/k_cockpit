/**
 * 平台自检与修复接口（F-4-13）。
 *
 * 自检与探测的区别：探测回答「这台机器有没有 OVS」（看**环境**），自检回答
 * 「我们配的那些东西现在还在不在」（看**偏差**）。后者才是用户需要的——面板
 * 显示「端口安全已启用」而节点上的流表早被一次重启清掉了，这个状态不会以
 * 任何形式报警。
 */
import { get, post } from './client'

export interface OVSStatus {
  Available: boolean
  Reason: string
  Version: string
  /** 与 Available **分开**：装好了但服务挂了，所有依赖它的功能都不生效。 */
  ServiceActive: boolean
  OpenFlow13: boolean
  MeterAvailable: boolean
  BridgeCount: number
  PortCount: number
  FlowCount: number
  Fix: string
}

export interface OVSPort {
  Name: string
  Bridge: string
  Type: string
  Tag: number
  /** 由**节点**填——只有它能从 libvirt 拿到 vnet 口与虚拟机的对应关系。 */
  VMName: string
}

export interface DHCPLease {
  ExpiresAt: string
  MAC: string
  IP: string
  Hostname: string
  ClientID: string
}

export interface CheckItem {
  Category: string
  Target: string
  Expected: string
  Actual: string
  OK: boolean
  /** info / warning / critical。 */
  Severity: string
  Fix: string
  /** 为 false 表示**装不上、加载不了**——标成可修复会让用户白点一次。 */
  Repairable: boolean
}

export interface CheckResult {
  Items: CheckItem[] | null
  /** 偏差数量：它是自检最核心的一个数字。 */
  Drifts: number
}

export interface ClientIPResult {
  client_ip: string
  forwarded_for?: string
  note?: string
}

export const platformCheckApi = {
  clientIP: () => get<ClientIPResult>('/api/v1/network/client-ip'),
  ovsStatus: (nodeID: number) => get<OVSStatus>('/api/v1/ovs/status', { node_id: nodeID }),
  ovsPorts: (nodeID: number) => get<{ items: OVSPort[] }>('/api/v1/ovs/ports', { node_id: nodeID }),
  leases: (nodeID: number) => get<{ items: DHCPLease[] }>('/api/v1/ovs/leases', { node_id: nodeID }),
  check: (nodeID: number) => post<CheckResult>(`/api/v1/ovs/check?node_id=${nodeID}`, {}),
  repair: (nodeID: number, kinds: string[]) =>
    post<{ task: unknown }>(`/api/v1/ovs/repair?node_id=${nodeID}`, { kinds }),
}
