/**
 * 公网 IP 接口（F-4-06）。
 *
 * 两条贯穿本模块的事实，界面必须如实反映：
 *
 * 1. **一个地址同一时刻只能指向一个地方**（网络层的事实，不是面板的偏好）。
 *    因此「绑定」在已绑定时会被拒绝，换目标要走**浮动迁移**。
 * 2. **迁移是故障转移的实现方式**——把地址从主机器挪到备机，外部访问几乎
 *    无感。它必须原子：解绑与绑定之间插进失败会让地址悬空，而那时外部
 *    访问已经断了。
 */
import { del, get, post } from './client'
import type { TaskRef } from './vm'

export type PublicIPMode = 'nat_1to1' | 'routed' | 'bridged'
export type PublicIPStatus = 'available' | 'bound' | 'disabled'
export type RuntimeStatus = 'pending' | 'active' | 'failed'

export interface PublicIPView {
  id: number
  node_id: number
  ip: string
  cidr?: string
  gateway?: string
  egress_if?: string
  address_family: string
  /** 该地址支持哪些绑定模式。不是每个地址都能用每种模式。 */
  supported_modes: string[]
  status: PublicIPStatus
  remark?: string

  /** 当前绑定；无绑定时全部为空。 */
  binding_id?: number
  vm_id?: number
  /** 供界面直接显示「这个地址指向哪台机器」。 */
  vm_name?: string
  mode?: PublicIPMode
  /** 节点上的实际生效状态，与「有没有绑定记录」是两回事。 */
  runtime_status?: RuntimeStatus
  bound_at?: string
}

/** 节点算出的规则预览。 */
export interface PublicIPPreview {
  /** 本次改动**将要新增**的规则。 */
  added: string[]
  /** 本次改动**将要移除**的规则。 */
  removed: string[]
  /** 节点发现的、值得先看一眼的问题。 */
  warnings?: string[]
}

export interface BatchItemResult {
  id: number
  /** 地址本身，让用户能对上号——只给 id 的话失败清单里一列数字看不出是哪几个。 */
  address?: string
  task_id?: number
  /** 失败原因（仅失败时有）。 */
  reason?: string
}

export interface BatchResult {
  ok: BatchItemResult[] | null
  failed: BatchItemResult[] | null
  /** 服务端给的总结，部分失败时写明了「已成功的那几条不会被撤销」。 */
  message: string
}

export const publicIPApi = {
  list: (nodeID = 0) =>
    get<{ items: PublicIPView[] }>(
      '/api/v1/public-ips',
      nodeID > 0 ? { node_id: nodeID } : undefined,
    ),

  /**
   * 录入地址。**支持单个地址或 CIDR 批量**。
   *
   * 展开时会自动跳过网络地址、广播地址与网关——它们不能分配给虚拟机，
   * 不过滤的话用户会得到一个看起来正常、绑定时才失败的地址。
   */
  create: (input: {
    node_id: number
    ip: string
    gateway?: string
    egress_if?: string
    supported_modes?: PublicIPMode[]
    remark?: string
  }) => post<{ items: PublicIPView[] }>('/api/v1/public-ips', input),

  /**
   * 批量解绑。
   *
   * **逐条如实报告**：失败的项带 id 与原因。一个笼统的「批量操作失败」
   * 会让用户不知道该处理哪几个——而那正是他要处理的东西。
   *
   * 已成功的那几条**不会被撤销**：回滚意味着把已经下发好的规则再撤掉，
   * 那会让一次失败的批量操作变成两次网络变更。
   */
  batchUnbind: (ids: number[]) =>
    post<BatchResult>('/api/v1/public-ips/batch/unbind', { ids }),

  /**
   * 批量绑定到**同一台**虚拟机。
   *
   * 只支持「多个地址 → 一台机器」这一个方向：绑定要求地址与虚拟机在同一
   * 节点，而任意组合会让用户在面对一台失败时无法判断是地址不对还是机器不对。
   */
  batchBind: (ids: number[], vmID: number, mode: PublicIPMode) =>
    post<BatchResult>('/api/v1/public-ips/batch/bind', { ids, vm_id: vmID, mode }),

  /** 从池中移除。**已绑定会被拒绝**——否则绑定记录会指向一条不存在的地址。 */
  remove: (id: number) => del<{ deleted: boolean }>(`/api/v1/public-ips/${id}`),

  /** 绑定到虚拟机。已绑定时会被拒绝，换目标请用 `migrate`。 */
  bind: (id: number, vmID: number, mode: PublicIPMode) =>
    post<TaskRef>(`/api/v1/public-ips/${id}/bind`, { vm_id: vmID, mode }),

  /**
   * 浮动迁移（故障转移）。`mode` 留空时沿用当前模式。
   *
   * 迁移不会造成不可逆的结果——地址还在池里，随时可以迁回来。因此
   * **不需要二次验证**：它影响的是连通性，而中断是立刻可见的。
   */
  migrate: (id: number, toVMID: number, mode?: PublicIPMode) =>
    post<TaskRef>(`/api/v1/public-ips/${id}/migrate`, { to_vm_id: toVMID, mode }),

  unbind: (id: number) => del<TaskRef>(`/api/v1/public-ips/${id}/bind`),

  /**
   * 规则预览。**只读探测**，不产生任何改动。
   *
   * 它必须来自节点：规则的最终形态取决于宿主机上已有的链、路由表与网卡
   * 配置——控制面按模板拼一段出来，看起来对、实际可能相差很远，而用户
   * 正是拿这份预览做「改还是不改」的判断。
   */
  preview: (id: number, vmID: number, mode: PublicIPMode) =>
    get<PublicIPPreview>(`/api/v1/public-ips/${id}/preview`, { vm_id: vmID, mode }),
}

/**
 * 各绑定模式的说明。
 *
 * 文案写清**代价与前提**，而不只是特性：三种模式的区别对用户而言是
 * 「我要不要进去宾改配置」，那正是他做选择时唯一关心的。
 */
export const PUBLIC_IP_MODE_HINT: Record<PublicIPMode, { label: string; detail: string }> = {
  nat_1to1: {
    label: '1:1 NAT',
    detail: '宿主机做地址转换，来宾无需改任何配置。换绑另一台机器时目标也不需要准备。',
  },
  routed: {
    label: '经典路由',
    detail: '地址直接路由到虚拟机网卡。**来宾必须自己配好这个地址**，否则绑了也不通。',
  },
  bridged: {
    label: '经典桥接',
    detail: '地址直接出现在虚拟机的桥上。要求来宾与宿主机在同一二层网络。',
  },
}

export const PUBLIC_IP_STATUS_LABEL: Record<PublicIPStatus, string> = {
  available: '可用',
  bound: '已绑定',
  disabled: '已停用',
}

export const PUBLIC_IP_STATUS_TONE: Record<PublicIPStatus, 'success' | 'info' | 'idle'> = {
  available: 'success',
  bound: 'info',
  disabled: 'idle',
}

/**
 * 绑定在节点上的实际生效状态。
 *
 * 与「有没有绑定记录」分开显示：记录是控制面的**意图**，这里是宿主机上的
 * **结果**。两者不一致时（如 pending）用户需要知道流量还没通。
 */
export const RUNTIME_STATUS_LABEL: Record<RuntimeStatus, string> = {
  pending: '生效中',
  active: '已生效',
  failed: '生效失败',
}
