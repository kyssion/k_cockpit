import { get, post } from './client'

/** 站点维护状态（G-46）。 */
export interface SiteMaintenanceStatus {
  in_maintenance: boolean
  reason?: string
  shutdown_vms: boolean
  entered_at?: string
  entered_by_name?: string
  /** 只在进入维护的响应里返回：逐节点处理结果。 */
  nodes?: MaintenanceNodeResult[]
  /** 状态查询：当前被站点模式接管的节点名。 */
  managed_nodes?: string[]
}

export interface MaintenanceNodeResult {
  node_id: number
  node_name: string
  status: string
  vm_shutdowns: number
  error?: string
}

export interface MaintenanceEnterInput {
  reason: string
  shutdown_vms: boolean
}

export const maintenanceApi = {
  status: () => get<SiteMaintenanceStatus>('/api/v1/maintenance'),
  enter: (input: MaintenanceEnterInput) => post<SiteMaintenanceStatus>('/api/v1/maintenance/enter', input),
  exit: () => post<SiteMaintenanceStatus>('/api/v1/maintenance/exit', {}),
}
