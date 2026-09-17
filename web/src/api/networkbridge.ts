/**
 * 网络底座接口（F-4-01 / F-4-13）。
 *
 * 与别的模块最大的不同：**这里几乎不会失败**。
 *
 * 探测不到节点、桥列表读不出来，都以字段形式返回（`probe_ok` /
 * `probe_error` / `bridges_error`），整体仍是 200。这是「网络配置失败不得
 * 阻断主流程」的具体实现——用户点进这个页面本来就是为了看网络出了什么
 * 问题，给他白屏等于把唯一的诊断入口也关掉了。
 *
 * 因此界面必须把「不知道」如实地画出来（一个明确的「状态未知」），而**不能**
 * 把探测失败渲染成「什么都没有」——那会让用户以为他的网络全没了。
 */
import { del, get, post } from './client'

export type BridgeBackend = 'bridge' | 'ovs'
export type BridgeMode = 'nat' | 'routed' | 'isolated'

export interface UplinkCandidate {
  name: string
  up: boolean
  /** **最重要的一个字段**：有 IP 的口很可能是管理口。 */
  has_ip: boolean
  speed?: string
}

export interface CapabilityView {
  ovs_available: boolean
  ovs_version?: string
  kernel_modules: string[]
  uplink_candidates: UplinkCandidate[]
}

export interface BridgeView {
  id: number
  node_id: number
  name: string
  backend: BridgeBackend
  mode: BridgeMode

  uplink_if?: string
  cidr?: string
  gateway_ip?: string
  dhcp_enabled: boolean
  dhcp_start?: string
  dhcp_end?: string
  vlan_id?: number

  is_system: boolean
  status: 'active' | 'pending' | 'error'
  /** 失败的具体原因。只说「出错」用户唯一的动作是重试。 */
  detail?: string
  remark?: string

  /** 该桥依赖 Open vSwitch——OVS 缺失时界面据此**具体指出**受影响的范围。 */
  needs_ovs: boolean
  awaiting_uplink_confirm: boolean
  uplink_watchdog_until?: string
  /** 由服务端算好（客户端时钟不可信）。 */
  uplink_seconds_left: number
  created_at: string
}

export interface Overview {
  node_id: number
  probe_ok: boolean
  probe_error?: string
  capability?: CapabilityView
  degraded: boolean
  /** 具体说明降级**影响了什么**，而不是一句「已降级」。 */
  degraded_reasons?: string[]
  bridges: BridgeView[]
  bridges_error?: string
  warnings?: string[]
}

export interface RepairResult {
  fixed: string[]
  /** 修复后仍存在的问题。与 fixed 分开——只报前者会让用户以为没事了。 */
  remaining: string[]
  probe_ok: boolean
  probe_error?: string
  message: string
}

export interface AttachUplinkResult {
  applied: boolean
  watchdog_seconds: number
  warnings?: string[]
  bridge?: BridgeView
}

export const networkApi = {
  overview: (nodeID: number) => get<Overview>('/api/v1/networks', { node_id: nodeID }),

  createBridge: (
    nodeID: number,
    input: {
      name: string
      backend?: BridgeBackend
      mode?: BridgeMode
      cidr?: string
      gateway_ip?: string
      dhcp_start?: string
      dhcp_end?: string
      dhcp_enabled?: boolean
      remark?: string
    },
  ) => post<BridgeView>(`/api/v1/networks/bridges?node_id=${nodeID}`, input),

  /** 系统预置网络会被拒绝——新建虚拟机默认接的就是它。 */
  removeBridge: (id: number) => del<{ deleted: boolean }>(`/api/v1/networks/bridges/${id}`),

  /**
   * 物理口入桥。`acknowledge` 为 false 时若有警告，服务端不下发也不报错，
   * 而是把警告返回。
   */
  attachUplink: (id: number, uplinkIf: string, watchdogSeconds: number, acknowledge: boolean) =>
    post<AttachUplinkResult>(`/api/v1/networks/bridges/${id}/uplink`, {
      uplink_if: uplinkIf,
      watchdog_seconds: watchdogSeconds,
      acknowledge,
    }),

  confirmUplink: (id: number) => post<BridgeView>(`/api/v1/networks/bridges/${id}/uplink/confirm`, {}),

  /** 摘出物理口。**不做确认**——一个已切断管理通道的桥要能一键摘掉。 */
  detachUplink: (id: number) => del<BridgeView>(`/api/v1/networks/bridges/${id}/uplink`),

  repair: (nodeID: number) => post<RepairResult>(`/api/v1/networks/repair?node_id=${nodeID}`, {}),
}

export const BRIDGE_MODE_LABEL: Record<BridgeMode, string> = {
  nat: '内置 DHCP + NAT',
  routed: '路由模式',
  isolated: '空交换机（不接外网）',
}

export const BRIDGE_STATUS_TONE: Record<BridgeView['status'], 'success' | 'warning' | 'danger'> = {
  active: 'success',
  pending: 'warning',
  error: 'danger',
}

export const BRIDGE_STATUS_LABEL: Record<BridgeView['status'], string> = {
  active: '正常',
  pending: '生效中',
  error: '异常',
}

export function formatCountdown(seconds: number): string {
  if (seconds <= 0) return '已到期'
  const m = Math.floor(seconds / 60)
  const s = seconds % 60
  return m === 0 ? `${s} 秒` : `${m} 分 ${String(s).padStart(2, '0')} 秒`
}
