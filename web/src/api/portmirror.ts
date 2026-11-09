/**
 * 端口镜像接口（F-4-09）。
 *
 * 界面上必须表达清楚的是**看门狗窗口**：启用之后有一段倒计时，用户要在
 * 它跑完之前点「保持」，否则镜像会被节点**自动撤销**。
 *
 * 关键在于「自动撤销」是**节点**执行的，不是浏览器里的一段计时。这解释
 * 了两件事：为什么倒计时数由服务端给（客户端时钟不可信），以及为什么
 * 用户关掉页面也拦不住它——那正是这个机制的意义所在。
 */
import { del, get, patch, post } from './client'

export type MirrorDirection = 'both' | 'ingress' | 'egress'

export interface PortMirrorView {
  id: number
  node_id: number
  name?: string

  source_ports: string[]
  target_switches: string[]
  direction: MirrorDirection
  /**
   * 恒为 true：节点侧 OVS Mirror 原样复制帧，**始终保留原始 VLAN 标签**，
   * 本版本不做剥离。保留为可选字段只为兼容旧数据与旧调用点——界面不应再
   * 用它做任何显示或开关（那只会表达一个不存在的选择）。
   */
  vlan_preserve?: boolean

  enabled: boolean
  /** 为 true 表示正处于看门狗窗口里，用户需要点「保持」。 */
  awaiting_confirm: boolean
  watchdog_until?: string
  /** 距自动撤销还剩的秒数，**由服务端算好**（客户端时钟不可信）。 */
  watchdog_seconds_left: number
  last_applied_at?: string
  created_at: string
}

export interface MirrorEnableResult {
  applied: boolean
  watchdog_seconds: number
  warnings?: string[]
  mirror?: PortMirrorView
}

export interface MirrorInput {
  name?: string
  source_ports: string[]
  target_switches: string[]
  direction?: MirrorDirection
  /**
   * 可选、且不该再传：服务端会把缺省值归一化为 true，并**拒绝** `false`
   * （节点侧镜像不读这个字段，不剥标签，`false` 只会是一个谎）。
   */
  vlan_preserve?: boolean
}

export const portMirrorApi = {
  list: (nodeID: number) => get<{ items: PortMirrorView[] }>('/api/v1/port-mirrors', { node_id: nodeID }),

  create: (nodeID: number, input: MirrorInput) =>
    post<PortMirrorView>(`/api/v1/port-mirrors?node_id=${nodeID}`, input),

  update: (id: number, input: MirrorInput) =>
    patch<PortMirrorView>(`/api/v1/port-mirrors/${id}`, input),

  remove: (id: number) => del<{ deleted: boolean }>(`/api/v1/port-mirrors/${id}`),

  precheck: (id: number) =>
    get<{ warnings: string[] }>(`/api/v1/port-mirrors/${id}/precheck`),

  /**
   * 启用并**同时**建立看门狗。
   *
   * `acknowledge` 为 false 时若有风险警告，服务端**不下发也不报错**，
   * 而是把警告返回——那是一个需要用户做决定的岔路口。
   */
  enable: (id: number, watchdogSeconds: number, acknowledge: boolean) =>
    post<MirrorEnableResult>(`/api/v1/port-mirrors/${id}/enable`, {
      watchdog_seconds: watchdogSeconds,
      acknowledge,
    }),

  /** 确认保持。不做它，镜像会被节点自动撤销。 */
  confirm: (id: number) => post<PortMirrorView>(`/api/v1/port-mirrors/${id}/confirm`, {}),

  /** 关闭。**幂等**，且不需要任何确认——一个正在打垮网络的镜像要能一键停。 */
  disable: (id: number) => post<PortMirrorView>(`/api/v1/port-mirrors/${id}/disable`, {}),
}

export const DIRECTION_LABEL: Record<MirrorDirection, string> = {
  both: '双向',
  ingress: '仅入站',
  egress: '仅出站',
}

/** 把秒数渲染成「3 分 12 秒」。 */
export function formatCountdown(seconds: number): string {
  if (seconds <= 0) return '已到期'
  const m = Math.floor(seconds / 60)
  const s = seconds % 60
  if (m === 0) return `${s} 秒`
  return `${m} 分 ${String(s).padStart(2, '0')} 秒`
}
