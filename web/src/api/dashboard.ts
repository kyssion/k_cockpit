/**
 * 工作台概览（F-8-03 管理员 / F-8-04 租户）。
 *
 * 一个接口而不是多个：概览里的每个数字单独看都不值一次请求，而「宿主机
 * 资源」要是按节点去拉实时指标，就变成 N+1 次探测——节点一多，打开一次
 * 首页等于对全部节点探测一遍。聚合在服务端一次做完。
 *
 * 两个字段的缺失语义要分清：
 *   - `nodes` / `host` 为 undefined 表示**本角色看不到**（租户），是权限；
 *   - `host` 为 null 表示**还没有采样数据**，是数据没到，不是「用了 0%」。
 */
import { get } from './client'

export type AlertLevel = 'danger' | 'warning'

export interface DashboardAlert {
  level: AlertLevel
  text: string
  /** 面板内路径，点击直达能处理它的页面。 */
  link: string
}

export interface DashboardNodes {
  total: number
  online: number
  offline: number
  maintenance: number
  /** 已生成令牌但 agent 尚未注册。 */
  pending: number
}

export interface DashboardVMs {
  total: number
  running: number
  stopped: number
  /** 暂停 / 挂起 / 错误 / 未知。 */
  other: number
  locked: number
  /** 虚拟化层已不存在。 */
  missing: number
}

/** 已承诺给虚拟机的资源：不管用没用，已经划出去的量。 */
export interface DashboardAllocation {
  vcpu: number
  memory_mb: number
  disk_gb: number
  /** 只统计运行中的机器——关机机器占磁盘，但不占 CPU 与内存。 */
  running_vcpu: number
  running_memory_mb: number
}

/** 宿主机的实际用量，来自最近一次采样。 */
export interface DashboardHost {
  sampled_nodes: number
  cpu_percent: number
  cpu_cores: number
  mem_used_mb: number
  mem_total_mb: number
  disk_total_bytes: number
  disk_used_bytes: number
  sampled_at: string
}

export interface DashboardTasks {
  /** 尚未落定，含 unknown（它仍占着资源锁，用户仍在等）。 */
  active: number
  failed_24h: number
}

export interface DashboardRecentVM {
  id: number
  name: string
  status: string
  node_name: string
  vcpu: number
  memory_mb: number
  created_at: string
}

export interface DashboardSummary {
  generated_at: string
  scope: 'platform' | 'self'
  nodes?: DashboardNodes | null
  vms: DashboardVMs
  allocation: DashboardAllocation
  host?: DashboardHost | null
  tasks: DashboardTasks
  alerts: DashboardAlert[]
  recent_vms: DashboardRecentVM[]
}

/** KSM / zRAM 状态（取自宿主机调优，与调优页同一份口径）。 */
export interface TuningStateView {
  enabled?: boolean
  /** KSM 省下的内存（字节）。 */
  saved_bytes?: number
  shared_pages?: number
  scan_rounds?: number
  /** 挡位：保守 / 均衡 / 极致。 */
  level?: string
  supported?: boolean
}

export interface TuningView {
  node_id: number
  ksm: TuningStateView
  zram: TuningStateView
}

/** 一个内存插槽。 */
export interface MemSlotView {
  index: number
  size_mb: number
  populated: boolean
  label?: string
}

export interface HostHardwareView {
  cpu_model?: string
  sockets: number
  cores_per_socket: number
  threads_per_core: number
  /** 每个逻辑核心的占用百分比，顺序即核心编号。 */
  core_percent: number[]
  mem_slots: MemSlotView[]
  /** 非空表示**探测不到**，而不是"这台机器没有内存"。 */
  unavailable?: string
}

export interface BridgeStatView {
  name: string
  rx_bytes: number
  tx_bytes: number
  rx_packets: number
  tx_packets: number
}

export interface HostNetStatsView {
  nat_rules: number
  switch_ingress_bytes: number
  switch_egress_bytes: number
  dnat_rules: number
  bridges: BridgeStatView[]
  unavailable?: string
}

/** 某个节点的宿主机细节。三块各自独立失败，因此都可能带 unavailable。 */
export interface HostDetailView {
  node_id: number
  node_name: string
  tuning?: TuningView
  hardware?: HostHardwareView
  netstats?: HostNetStatsView
}

export const dashboardApi = {
  summary: () => get<DashboardSummary>('/api/v1/dashboard/summary'),

  /**
   * 宿主机细节。**按节点**查询：它需要向节点发请求，因此不并进概览——
   * 否则首页会变成一次探测风暴。
   */
  hostDetail: (nodeID: number) =>
    get<HostDetailView>('/api/v1/dashboard/host-detail', { node_id: nodeID }),

  /**
   * 我的配额总览（G-32）。按「用户 × 节点」聚合三类配额：
   * 计算（存量）、存储（存量）、流量与运行时长（UTC 月累计）。
   * `has_limit=false` 表示该维度不限。
   */
  myQuotas: () => get<QuotaOverview>('/api/v1/dashboard/quotas'),
}

/** 一个维度的「已用 / 上限」。 */
export interface UsedLimit {
  used: number
  limit: number
  has_limit: boolean
  unit?: string
}

/** 一个节点上的全部配额维度。 */
export interface NodeQuota {
  node_id: number
  node_name: string
  vcpu: UsedLimit
  memory_mb: UsedLimit
  vms: UsedLimit
  storage_gb: UsedLimit
  traffic_in_gb: UsedLimit
  traffic_out_gb: UsedLimit
  runtime_hours: UsedLimit
  /** ok / warned / limited，取全部维度中最差的。 */
  worst_status: string
}

export interface QuotaOverview {
  generated_at: string
  nodes: NodeQuota[]
}
