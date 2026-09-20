/**
 * 计算资源配额：vCPU / 内存 / 实例数。
 *
 * 它与「资源配额」（月流量、月运行时长，见 api/quotaenforce.ts）是**两类
 * 约束**，因此接口与类型都分开：那边按周期累计、超限后限速或断网；这里
 * 看的是"此刻占着多少"，超限的处置是**拒绝新建**。
 */
import { get, put } from './client'

export interface ComputeQuotaView {
  user_id: number
  node_id: number
  username?: string

  /** 当前占用。 */
  vcpu: number
  memory_mb: number
  vm_count: number

  /** 上限。0 表示不限。 */
  has_quota: boolean
  quota_vcpu: number
  quota_memory_mb: number
  quota_vm_count: number
}

export interface ComputeQuotaInput {
  node_id: number
  user_id: number
  vcpu: number
  memory_mb: number
  vm_count: number
}

export const computeQuotaApi = {
  list: (nodeID: number) =>
    get<{ items: ComputeQuotaView[] }>('/api/v1/compute-quotas', { node_id: nodeID }),

  /** 三个上限全为 0 表示删除这条配额（回到不限）。 */
  set: (input: ComputeQuotaInput) => put<{ ok: boolean }>('/api/v1/compute-quotas', input),
}
