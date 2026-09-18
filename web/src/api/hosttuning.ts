/**
 * 宿主机性能调优接口（KSM / ZRAM / 嵌套虚拟化 / CPU 亲和）。
 *
 * **开关与效果一起返回**：只给「已启用」的话，用户无法回答两个最实际的
 * 问题——该不该开、开了有没有用。KSM 尤其如此：它**持续消耗 CPU**，在什么
 * 都没合并的时候照样扫描内存，因此"开着但一无所获"是一个真实存在、且用户
 * 完全看不出来的坏状态。
 */
import { del, get, post, put } from './client'

export interface KSMState {
  Enabled: boolean
  PagesShared: number
  PagesSharing: number
  PagesUnshared: number
  /** 估算省下的内存——**由节点算好**，页大小随架构不同。 */
  SavedBytes: number
  /** 完整扫描轮次：回答"开了但省了 0"是正常还是异常。 */
  FullScans: number
  RunMode: string
}

export interface ZRAMState {
  Enabled: boolean
  DisksizeBytes: number
  UsedBytes: number
  OrigDataBytes: number
  /** lz4 快而压缩率低，zstd 反之——是一个取舍。 */
  Algorithm: string
  MemLimitBytes: number
}

export interface NestedState {
  Enabled: boolean
  Supported: boolean
  Reason: string
  /** 为 false 表示**重启后会失效**。 */
  Persistent: boolean
  Fix: string
}

export interface TuningItem {
  key: string
  label: string
  Benefit: string
  Cost: string
  Metric: string
}

export interface CPUPreset {
  id: number
  node_id: number
  name: string
  cpuset: string
  /** cpuset 展开成人话——「0-3,8」要用户在脑子里展开，而展开错了不报错。 */
  cpuset_desc: string
  remark?: string
  created_at: string
}

export interface TuningView {
  node_id: number
  ksm: KSMState
  zram: ZRAMState
  nested: NestedState
  items: TuningItem[]
  presets: CPUPreset[] | null
}

export const hostTuningApi = {
  get: (nodeID: number) => get<TuningView>('/api/v1/host/tuning', { node_id: nodeID }),

  apply: (nodeID: number, req: { item: string; enabled?: boolean; disksize_mb?: number; algorithm?: string; mem_limit_mb?: number }) =>
    put<{ task: unknown }>(`/api/v1/host/tuning?node_id=${nodeID}`, req),

  createPreset: (nodeID: number, req: { name: string; cpuset: string; remark?: string }) =>
    post<CPUPreset>(`/api/v1/host/cpu-affinity-presets?node_id=${nodeID}`, req),

  deletePreset: (nodeID: number, id: number) =>
    del<{ ok: boolean }>(`/api/v1/host/cpu-affinity-presets/${id}?node_id=${nodeID}`),
}

/** formatBytes 把字节数变成人能读的量级。 */
export function formatBytes(v: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let n = Math.abs(v)
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${v < 0 ? '-' : ''}${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`
}
