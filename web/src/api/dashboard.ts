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

export const dashboardApi = {
  summary: () => get<DashboardSummary>('/api/v1/dashboard/summary'),
}
