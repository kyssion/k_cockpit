/**
 * 虚拟机控制台接口（F-2-08）。
 *
 * 两个不该被界面掩盖的事实：
 *   - **密码只写不读**（R-005）：接口只告诉界面「是否已设置」，拿不到明文。
 *     因此连接控制台时密码需要用户自己输入；
 *   - **对外暴露是高危操作**（R-004）：它会把宿主机端口开放到网络上，
 *     等于给这台虚拟机开了一扇绕过面板的后门。
 */
import { get, patch } from './client'

export interface ConsoleConfig {
  vm_id: number
  /** 该虚拟机是否有控制台。显示设备为 none 时为 false（R-011）。 */
  available: boolean
  unavailable_reason?: string

  /** **可用**的控制台协议（SPICE 是 libvirt 编译期可选项，要探测；VNC 恒可用）。 */
  protocols: string[]
  /** 当前所选协议。 */
  protocol: string

  enabled: boolean
  port?: number
  /** 监听地址。对外暴露时为 0.0.0.0，否则为 127.0.0.1。 */
  bind?: string
  exposed: boolean
  /** 是否已设置密码。**拿不到明文**（R-005）。 */
  has_password: boolean
  display_device: string

  active_sessions: number
  session_limit: number
  /** 当前 agent 是否支持流式转发。 */
  stream_supported: boolean
}

export interface ConsoleUpdateInput {
  /** 要配置哪一种控制台（vnc / spice），留空按 VNC。 */
  protocol?: string
  enabled?: boolean
  /** 只写不读。VNC 协议限制最长 8 位；SPICE 无此限制。 */
  password?: string
  /** 高危：仅变更此项时服务端会要求二次验证。 */
  exposed?: boolean
}

export const consoleApi = {
  get: (vmID: number, protocol?: string) =>
    get<ConsoleConfig>(
      `/api/v1/vms/${vmID}/console${protocol ? `?protocol=${protocol}` : ''}`,
    ),
  update: (vmID: number, input: ConsoleUpdateInput) =>
    patch<ConsoleConfig>(`/api/v1/vms/${vmID}/console`, input),
}

/**
 * 构造控制台 WebSocket 地址。
 *
 * 与页面同源：控制面代理控制台流量（R-002），浏览器不直连宿主机。
 * 协议随页面在 http/https 之间切换，避免在 HTTPS 页面里连 ws:// 被浏览器拦截。
 */
export function consoleSocketURL(vmID: number): string {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${window.location.host}/api/v1/vms/${vmID}/console/ws`
}
