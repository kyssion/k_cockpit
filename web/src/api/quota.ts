/**
 * 存储配额接口（F-9-02）。
 *
 * 一个贯穿始终的口径：**用量现算，不读缓存**。`used_bytes` 在服务端是一列
 * 缓存，但任何判断（能不能再建一台、能不能再导一次）都走现算——按一个偏小的
 * 旧值放行会让用户悄悄超额，按一个偏大的旧值拦截会让他莫名其妙地被拒。
 */
import { get, put } from './client'

/** 一个用户在一个节点上的用量明细。 */
export interface QuotaUsage {
  user_id: number
  node_id: number

  /** 名下虚拟机磁盘合计（按**配置大小**：配额要按承诺算，不是按当前占用）。 */
  vm_disks_bytes: number
  /** 导出产物合计（按**实际大小**：产物是确定的文件，不会长大）。 */
  exports_bytes: number
  /** 模板合计（按配置大小）。 */
  templates_bytes: number

  total_bytes: number
  /** 为 0 表示不限制。 */
  quota_bytes: number
  unlimited: boolean
  /** 为 -1 表示不限制——与「还有很多」刻意区分开。 */
  remaining_bytes: number
  /** 已经超出。它决定的是「能不能再新增」，而不是「已用的怎么办」。 */
  over_quota: boolean
  read_only: boolean
}

export const quotaApi = {
  /**
   * 当前用户在各节点上的用量。
   *
   * 不传 user_id：**只能查自己的**。少一个「用别人的 ID 去查」的入口，
   * 就少一次归属校验被写错的机会。
   */
  mine: (nodeID = 0) =>
    get<{ items: QuotaUsage[] }>('/api/v1/quota', nodeID > 0 ? { node_id: nodeID } : undefined),

  /** 全部配额记录（管理员）。 */
  list: (nodeID = 0) =>
    get<{ items: QuotaUsage[] }>('/api/v1/quotas', nodeID > 0 ? { node_id: nodeID } : undefined),

  /** 设置配额（管理员）。**同步生效**——纯控制面元数据。 */
  set: (input: {
    user_id: number
    node_id: number
    enabled: boolean
    quota_bytes: number
    read_only: boolean
  }) => put<QuotaUsage>('/api/v1/quotas', input),
}
