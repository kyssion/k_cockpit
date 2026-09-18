/**
 * PCIe 直通接口。
 *
 * **IOMMU 分组里的设备只能一起直通。** 分组关系取决于主板拓扑与 BIOS 设置，
 * 只有节点探测得到——因此同组成员随设备一起返回，界面上必须在用户选择的
 * 那一刻就把"还有谁会一起被拿走"说出来。用户选了显卡之后发现鼠标键盘也一起
 * 被拿走了，正是"同组"的表现。
 */
import { del, get, post } from './client'

export interface PCIDevice {
  Address: string
  VendorDevice: string
  Class: string
  Description: string
  /** 当前驱动。`vfio-pci` 表示已绑定、可直通。 */
  Driver: string
  /** 同一组的设备只能一起直通。 */
  IOMUGroup: number
  /** 同组其它设备的描述——**必须在选择时展示**。 */
  group_peers: string[]
  CanPassthrough: boolean
  /** 不可直通的原因。 */
  Reason: string
  /** 被哪台虚拟机挂着。 */
  attached_to_vm_id?: number
  attached_to_vm_name?: string
}

export interface IOMMUStatus {
  Enabled: boolean
  VFIOAvailable: boolean
  Reason: string
  /** 要做什么（加哪个内核参数、是否要重启）。 */
  Fix: string
  /** 改完需要重启宿主机——**上面所有虚拟机会停机**。 */
  NeedReboot: boolean
}

export interface VMPassthroughRow {
  ID: number
  VMID: number
  NodeID: number
  PCIAddress: string
  DeviceDesc?: string
  IOMUGroup?: number
  Remark?: string
  AttachedAt: string
}

export const passthroughApi = {
  overview: (nodeID: number) =>
    get<{ devices: PCIDevice[]; iommu: IOMMUStatus | null }>('/api/v1/host/passthrough', {
      node_id: nodeID,
    }),

  bind: (nodeID: number, address: string) =>
    post<{ ok: boolean }>(`/api/v1/host/passthrough/bind?node_id=${nodeID}`, {
      pci_address: address,
    }),

  unbind: (nodeID: number, address: string) =>
    post<{ ok: boolean }>(`/api/v1/host/passthrough/unbind?node_id=${nodeID}`, {
      pci_address: address,
    }),

  listVM: (vmID: number) => get<{ items: VMPassthroughRow[] }>(`/api/v1/vms/${vmID}/passthrough`),

  /** **要求虚拟机处于关机状态**——直通设备不支持热插拔。 */
  attach: (vmID: number, address: string) =>
    post<VMPassthroughRow>(`/api/v1/vms/${vmID}/passthrough`, { pci_address: address }),

  detach: (vmID: number, address: string) =>
    del<{ ok: boolean }>(
      `/api/v1/vms/${vmID}/passthrough?pci_address=${encodeURIComponent(address)}`,
    ),
}
