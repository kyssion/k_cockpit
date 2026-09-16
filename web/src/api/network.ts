/**
 * 网络后端接口（F-4-01）。
 *
 * 能力是**三态**而不是布尔值（`available` / `unavailable` / `unknown`）：
 * 探测失败与确认缺失是两回事——前者要重试，后者要装东西。用布尔值把它们
 * 混在一起，会让用户去装一个其实已经装好的包。
 */
import type { TaskRef } from './vm'

import { del, get, patch, post } from './client'

export type CapabilityState = 'available' | 'unavailable' | 'unknown'

export interface Capability {
  key: string
  label: string
  state: CapabilityState
  /** 该能力是否为基础网络的必需项；缺失即降级。 */
  required: boolean
  /** 缺的是什么。 */
  reason?: string
  /** 怎么修（可执行的命令）。 */
  fix?: string
  /** 影响哪些功能。 */
  affected_features?: string[]
}

export interface NetworkStatus {
  node_id: number
  mode: string
  mode_label: string
  /** 必需能力有缺失，功能受限。 */
  degraded: boolean
  /** 本次探测失败，所有能力为「未知」。 */
  probe_failed: boolean
  probe_message?: string
  capabilities: Capability[]
}

export interface SwitchView {
  id: number
  node_id: number
  name: string
  mode: string
  bridge_name: string
  cidr?: string
  gateway_ip?: string
  dhcp_start?: string
  dhcp_end?: string
  uplink_if?: string
  /** 为空表示不划 VLAN。**有无是两个不同的配置**，不能用 0 代替。 */
  vlan_id?: number
  is_system: boolean
  status: string
}

export const networkApi = {
  status: (nodeID: number) => get<NetworkStatus>(`/api/v1/nodes/${nodeID}/network`),
  networks: (nodeID: number) => get<SwitchView[]>(`/api/v1/nodes/${nodeID}/networks`),

  /**
   * 新建虚拟交换机。
   *
   * 返回任务标识而非创建好的交换机：建网桥是宿主机上的实际操作，接口
   * 不同步等待。记录由执行器在节点成功后写入，因此**受理成功的这一刻它
   * 还不存在于列表里**——界面据此提示「正在创建」，而不是让用户刷新后
   * 找不到、以为创建失败了。
   */
  createSwitch: (nodeID: number, input: SwitchInput) =>
    post<TaskRef>(`/api/v1/nodes/${nodeID}/vpc-switches`, input),

  updateSwitch: (id: number, input: SwitchInput) =>
    patch<TaskRef>(`/api/v1/vpc-switches/${id}`, input),

  /**
   * 删除交换机。
   *
   * 占用检查在服务端**同步**完成：还有网卡接在上面时立即返回 409 并说明
   * 数量，而不是排进队列等几分钟后再失败。
   */
  deleteSwitch: (id: number) => del<TaskRef>(`/api/v1/vpc-switches/${id}`),
}

/** 交换机的新建 / 修改输入。 */
export interface SwitchInput {
  name: string
  mode: string
  vlan_id?: number
  cidr?: string
  gateway_ip?: string
  dhcp_start?: string
  dhcp_end?: string
  uplink_if?: string
}

export const SWITCH_MODE_LABEL: Record<string, string> = {
  nat: 'NAT 出网（虚拟机经宿主上网）',
  empty: '隔离（仅虚拟机之间互通）',
  physical: '桥接物理网卡',
}

export const CAPABILITY_STATE_LABEL: Record<CapabilityState, string> = {
  available: '可用',
  unavailable: '不可用',
  unknown: '无法确认',
}

export const CAPABILITY_STATE_TONE: Record<CapabilityState, 'success' | 'danger' | 'idle'> = {
  available: 'success',
  unavailable: 'danger',
  // unknown 用 idle 而非 danger：它不是「坏了」，而是「还不知道」。
  // 标红会让人去修一个可能根本没坏的东西。
  unknown: 'idle',
}
