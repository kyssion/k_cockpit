/**
 * 节点接口（F-6-01 / F-6-02）。
 *
 * 注意 `status` 与 `enroll_state` 是两件事：
 *   - `enroll_state` 是元数据——该节点是否已接入（pending / enrolled）；
 *   - `status` 是**运行态**，由心跳时间推导。
 * 一个 enrolled 的节点可能处于 offline，一个 pending 的节点则必然是 unknown。
 */
import { del, get, patch, post } from './client'

export type NodeStatus = 'online' | 'offline' | 'unknown'
export type EnrollState = 'pending' | 'enrolled'

export interface NodeView {
  id: number
  name: string
  status: NodeStatus
  enroll_state: EnrollState
  enabled: boolean
  maintenance_mode: boolean
  /**
   * 是否可作为迁移目标。
   *
   * 进入维护模式时界面要提示这一条：维护中的节点不该继续承接迁移——
   * 迁移会把新虚拟机放到它上面，而那正是「引入变更」。
   */
  is_migration_target: boolean
  /** 仅在处于维护模式时有值；退出时后端会清空。 */
  maintenance_reason?: string
  maintenance_at?: string
  agent_version?: string
  protocol_version: number
  capabilities?: string[]
  last_heartbeat_at?: string
  last_error?: string
  remark?: string
  /**
   * 控制台的**对外可达**地址（管理员填写）。
   *
   * 它不是宿主机上的监听地址——那通常是 127.0.0.1。这里填的是"用户在自己
   * 的网络里连接控制台时该用的地址"，只影响能否生成 SPICE 连接文件。
   */
  console_host?: string
  created_at: string
}

export interface EnrollTokenResult {
  /** 明文令牌，**只返回一次**；库里存的是哈希。 */
  token: string
  expires_at: string
  node: NodeView
  /** agent 安装命令；agent 尚未实现，接入后填充。 */
  install_command: string
  /** 开发期的模拟注册命令（仅 AGENT_TRANSPORT=mock 时返回）。 */
  simulate_command?: string
}

export const nodeApi = {
  list: () => get<NodeView[]>('/api/v1/nodes'),

  get: (id: number) => get<NodeView>(`/api/v1/nodes/${id}`),

  /** 生成一次性注册令牌并创建待接入节点。ttlHours 为 0 时用后端默认值（24h）。 */
  createEnrollToken: (name: string, ttlHours = 0) =>
    post<EnrollTokenResult>('/api/v1/nodes/registration-tokens', {
      name,
      ttl_hours: ttlHours,
    }),

  /**
   * 进入 / 退出维护模式（F-6-05）。**同步生效**——它纯粹是控制面的标志，
   * 所有拦截都发生在受理请求那一刻，因此没有「等任务跑完才生效」的窗口。
   */
  setMaintenance: (id: number, enabled: boolean, reason?: string) =>
    patch<NodeView>(`/api/v1/nodes/${id}/maintenance`, { enabled, reason }),

  /** 设置控制台对外地址；留空表示不提供连接文件。 */
  setConsoleHost: (id: number, host: string) =>
    patch<NodeView>(`/api/v1/nodes/${id}/console-host`, { host }),

  /**
   * 宿主机实时指标（F-6-03）。只读探测，不入队。
   *
   * 响应含 `at`（采集时刻）：指标是瞬时值，轮询失败时界面会继续显示上一组
   * 数字，没有采集时刻就分不清「当前」与「几分钟前」。
   */
  stats: (id: number) => get<NodeStats>(`/api/v1/nodes/${id}/stats`),

  remove: (id: number) => del<void>(`/api/v1/nodes/${id}`),
}

/** 宿主机的实时指标。 */
export interface NodeStats {
  cpu_percent: number
  cpu_cores: number
  /** 一分/五分/十五分钟平均负载。与 CPU 占用率**一起看**才有意义： */
  load_avg_1: number
  load_avg_5: number
  load_avg_15: number

  mem_total_mb: number
  mem_used_mb: number

  disk_total_bytes: number
  disk_used_bytes: number

  /** 该节点上的虚拟机数量——由**控制面统计**，与列表页数字一致。 */
  vm_count: number
  vm_running: number

  uptime_seconds: number
  /** 与 uptime 分开：agent 重启过往往是排查一连串异常的第一条线索。 */
  agent_started_at: string
  at: string
}
