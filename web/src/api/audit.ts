/**
 * 审计流水接口（F-1-12）。
 *
 * **租户只能看到自己的记录**，这条隔离在服务端做，界面拿不到别人的数据。
 * 因此界面不需要、也不应该自己去判断可见性——它只管展示拿到的结果。
 */
import { get } from './client'

export interface AuditEntry {
  id: number
  at: string

  operator_id?: number
  operator_name?: string
  /**
   * 记录来源：web 还是 API 凭证。
   *
   * 它比看起来重要——用 API Key 执行的操作**不会触发二次验证**，
   * 因此「这条记录是怎么来的」是判断风险时的第一手信息。
   */
  source: string

  node_id?: number
  resource_type: string
  resource_id?: number
  resource_name?: string
  action: string

  /** 原始 JSON 文本，未解析——保留原始形态便于比对。 */
  params?: string
  before_state?: string
  after_state?: string

  success: boolean
  error?: string
  client_ip?: string
}

export interface AuditPage {
  items: AuditEntry[]
  total: number
  page: number
  page_size: number
  /** 结果被截断——审计少几条会让「没发生过」这个结论变成错的。 */
  truncated: boolean
  note?: string
}

export interface AuditFilter {
  from?: string
  to?: string
  operator_id?: number
  resource_type?: string
  action?: string
  /**
   * 来源筛选（G-38）：web（界面）/ api（API 凭证）/ emergency（带外脚本）。
   * 不传表示不限——「从哪里发起的」是判断一条记录风险的第一手信息。
   */
  source?: string
  keyword?: string
  /** 三态：不传表示不限。 */
  success?: boolean
  page?: number
  page_size?: number
}

export interface AuditFacets {
  actions: string[]
  resource_types: string[]
}

export const auditApi = {
  list: (f: AuditFilter) =>
    get<AuditPage>('/api/v1/audit', {
      ...f,
      success: f.success === undefined ? undefined : String(f.success),
    }),

  /** 筛选项由服务端给出，避免界面里的列表与后端对不上。 */
  facets: () => get<AuditFacets>('/api/v1/audit/facets'),

  get: (id: number) => get<AuditEntry>(`/api/v1/audit/${id}`),
}

export const SOURCE_LABEL: Record<string, string> = {
  web: '界面',
  api: 'API 凭证',
  system: '系统',
}

/** 把动作名渲染成可读的中文；未登记的直接显示原值以便排查。 */
export function actionLabel(action: string): string {
  const KNOWN: Record<string, string> = {
    'user.login': '登录',
    'user.logout': '登出',
    'session.revoke': '撤销会话',
    'api_key.create': '生成 API 凭证',
    'api_key.revoke': '撤销 API 凭证',
    'vm.create': '创建虚拟机',
    'vm.delete': '删除虚拟机',
    'vm.start': '开机',
    'vm.shutdown': '关机',
    'vm.migrate': '迁移虚拟机',
    'vm.reinstall': '重装系统',
    'vm.snapshot.create': '创建快照',
    'vm.snapshot.delete': '删除快照',
    'vm.snapshot.restore': '恢复快照',
    'vm.lock.release': '解锁虚拟机',
    'node.maintenance.enter': '进入维护模式',
    'node.maintenance.exit': '退出维护模式',
    'firewall.apply': '下发防火墙',
    'firewall.rollback': '回滚防火墙',
    'port_mirror.enable': '启用端口镜像',
    'port_mirror.disable': '关闭端口镜像',
    'network.repair': '修复网络',
    'network_bridge.uplink_attach': '物理口入桥',
    'storage_file.upload': '上传文件',
    'storage_file.delete': '删除文件',
    'share.mount': '挂载目录共享',
  }
  return KNOWN[action] ?? action
}
