/**
 * 虚拟机网络管理的写操作（F-2-03）。
 *
 * 全部走任务队列：这些变更都要下发到节点才能生效，且与电源操作共用资源锁
 * （`vm:<id>`），因此不会出现「改完网卡正好赶上关机」。
 *
 * 三者都**不需要关机**——网卡支持热插拔，地址与转发规则属于配置层。
 */
import { del, get, patch, post } from './client'
import type { NICModel, StaticIP, TaskRef, VMInterface } from './vm'

/** 网卡的型号、限速与接入网络。 */
export interface InterfaceInput {
  model: NICModel
  /** 为空表示使用节点默认网络。 */
  switch_id?: number
  /** 0 表示不限速。 */
  rate_limit_mbps: number
  /** 允许的源地址（防 IP 欺骗），逗号分隔；为空表示不限制。 */
  allowed_addresses?: string
}

export interface BindStaticIPInput {
  ip: string
  interface_order?: number
  /** 通过 DHCP 静态租约下发，而不是让用户在来宾里手工配置。 */
  is_dhcp_reservation: boolean
}

/** 端口转发：把宿主机的一个端口转发到虚拟机。 */
export interface PortForward {
  id: number
  protocol: 'tcp' | 'udp'
  host_port: number
  /** 为空表示由节点按虚拟机的实际地址决定。 */
  target_ip?: string
  target_port: number
  /** 为空表示**不限制来源**。界面必须显式提示这一点。 */
  allowed_ips?: string
  /** 当前尚未生效（需要节点侧 GeoIP 能力），界面应如实标注。 */
  allowed_regions?: string
  enabled: boolean
  /** 为 false 表示规则尚未下发，即**当前并不生效**。 */
  applied: boolean
  last_applied_at?: string
  created_at: string
}

export interface AddPortForwardInput {
  protocol: 'tcp' | 'udp'
  host_port: number
  target_ip?: string
  target_port: number
  allowed_ips?: string
}

export const netApi = {
  // --- 网卡 ---
  addInterface: (vmID: number, input: InterfaceInput) =>
    post<TaskRef>(`/api/v1/vms/${vmID}/interfaces`, input),

  updateInterface: (vmID: number, nicID: number, input: InterfaceInput) =>
    patch<TaskRef>(`/api/v1/vms/${vmID}/interfaces/${nicID}`, input),

  removeInterface: (vmID: number, nicID: number) =>
    del<TaskRef>(`/api/v1/vms/${vmID}/interfaces/${nicID}`),

  // --- 静态地址 ---
  bindStaticIP: (vmID: number, input: BindStaticIPInput) =>
    post<TaskRef>(`/api/v1/vms/${vmID}/static-ips`, input),

  unbindStaticIP: (vmID: number, ipID: number) =>
    del<TaskRef>(`/api/v1/vms/${vmID}/static-ips/${ipID}`),

  // --- 端口转发 ---
  listPortForwards: (vmID: number) =>
    get<{ items: PortForward[] }>(`/api/v1/vms/${vmID}/port-forwards`),

  addPortForward: (vmID: number, input: AddPortForwardInput) =>
    post<TaskRef>(`/api/v1/vms/${vmID}/port-forwards`, input),

  /**
   * 批量删除端口转发。
   *
   * 每条**必须带 vm_id**：删除要做归属校验，而归属挂在虚拟机上。
   *
   * **逐条如实报告**：失败的项带 id 与原因。已成功的那几条不会被撤销
   * ——撤销意味着再做一次网络变更。
   */
  batchRemovePortForwards: (
    items: { vm_id: number; pf_id: number }[],
  ) =>
    post<{
      removed: { vm_id: number; pf_id: number }[] | null
      failed: { ref: { vm_id: number; pf_id: number }; reason: string }[] | null
      message: string
    }>('/api/v1/vms/port-forwards/batch-delete', { items }),

  removePortForward: (vmID: number, pfID: number) =>
    del<TaskRef>(`/api/v1/vms/${vmID}/port-forwards/${pfID}`),
}

/** 网卡型号的可选值（与后端 model.NICModel* 对应）。 */
export const NIC_MODEL_OPTIONS: { value: NICModel; label: string }[] = [
  { value: 'virtio', label: 'virtio（半虚拟化，性能最好）' },
  { value: 'e1000', label: 'e1000（Intel 千兆，兼容性最好）' },
  { value: 'rtl8139', label: 'rtl8139（老旧系统兼容）' },
]

/** 类型别名，供页面使用。 */
export type { StaticIP, VMInterface }
