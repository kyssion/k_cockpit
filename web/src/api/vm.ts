/**
 * 虚拟机接口（F-2-01 ~ F-2-04）。
 *
 * 两点需要留意：
 *   - `status` 是**投影字段**，权威在虚拟化层，控制面只是缓存。配合 `stale`
 *     一起看：`stale=true` 说明数据可能已经过期，界面必须提示而不是照常显示。
 *   - 创建是**异步**的：接口返回任务标识而非创建好的虚拟机，前端据任务进度
 *     跟踪结果（f-7-01 R-001）。
 */
import { del, get, getPaged, patch, post, type Pagination } from './client'
import type { TaskStatus } from './task'

export type VmStatus = 'running' | 'stopped' | 'paused' | 'suspended' | 'error' | 'unknown'

export interface VmView {
  id: number
  node_id: number
  name: string
  uuid?: string
  owner_id?: number

  status: VmStatus
  vcpu: number
  memory_mb: number
  disk_gb: number
  ip_summary?: string

  remark?: string
  group_name?: string

  present: boolean
  last_synced_at?: string
  /** 投影数据可能已过期：界面应提示「数据可能陈旧」。 */
  stale: boolean
  created_at: string

  /**
   * 按**投影状态**算出的可用电源操作。
   *
   * 可能与实际不一致（投影滞后），此时后端受理时会基于实时探测拒绝并说明
   * 原因。`stale` 为 true 时不应完全依赖它。
   */
  available_actions: PowerAction[]

  /** 是否有可用控制台（display != none）。为 false 时界面应隐藏入口。 */
  has_console: boolean

  /**
   * 是否被业务软锁保护（F-2-12）。锁定时禁止删除。
   *
   * 由**后端下发**，界面不自行判断：批量操作要提前提示「其中 N 台已锁定」
   * （f-2-01 R-010），而前端的依据只能来自列表接口本身——各页面各自再查
   * 一次，迟早会出现「没标锁定、点删除却被拒绝」的不一致。
   */
  locked: boolean
  /** 加锁原因，供界面解释「为什么锁着」。 */
  lock_reason?: string
  locked_at?: string

  /**
   * 是否处于救援模式（F-2-12）。
   *
   * 界面必须显示醒目提示：救援模式下看到的系统**不是用户自己的系统**
   * ——盘型、网卡、引导顺序都被改过，而磁盘内容原样保留。把它当成日常
   * 状态会让人做出错误判断（比如以为系统没问题了）。
   */
  rescue_active: boolean
  /** 进入救援的时刻。 */
  rescue_since?: string

  /** 来源模板（F-3-02）；为空表示从零安装或来源已删。 */
  template_id?: number
  /**
   * 克隆方式。
   *
   * `linked` 表示磁盘只是模板之上的一层覆盖——**模板被删后数据就不可用了**，
   * 而且不会立刻报错。界面必须把这条依赖显示出来，否则用户无法理解为什么
   * 「删掉一个模板」会让自己的机器出事。
   */
  clone_mode?: 'full' | 'linked'
}

export interface VmListParams {
  status?: string
  keyword?: string
  node_id?: number
  group_name?: string
  page?: number
  page_size?: number
}

export interface CreateVmInput {
  name: string
  node_id: number
  vcpu: number
  memory_mb: number
  disk_gb: number
  remark?: string
  group_name?: string

  /**
   * 从模板克隆（F-3-02）；留空表示从零安装。
   *
   * 两者在控制面是同一个入口、同一个任务类型——对用户来说「从模板建一台
   * 机器」与「新建一台机器」是同一件事，只在最后下发时分开。
   */
  template_id?: number
  /**
   * 克隆方式，`template_id` 存在时生效，留空按 `full` 处理。
   *
   * **链式克隆必须是显式选择**：它引入了「父盘没了数据就没了」这个依赖，
   * 不该是默认行为。
   */
  clone_mode?: 'full' | 'linked'
}

/** 创建/电源/删除的结果：都是任务标识，操作本身异步执行。 */
export interface TaskRef {
  task_id: number
  status: TaskStatus
}

/** 批量操作中单台的结果。 */
export interface BatchItem {
  vm_id: number
  vm_name?: string
  ok: boolean
  task_id?: number
  /** 这一台失败的原因。**必看**：只标一个红叉会让用户去猜是权限、状态还是网络问题。 */
  error?: string
}

/** 批量操作的整体结果。 */
export interface BatchResult {
  items: BatchItem[]
  /** 汇总由后端算好，界面不自己数一遍 items——两处各数一遍迟早不一致。 */
  succeeded: number
  failed: number
}

/**
 * 电源动作。
 *
 * `shutdown`（优雅关机，等来宾配合）与 `poweroff`（强制断电）是**两个独立
 * 操作**：系统不会在关机超时后自动降级为断电，因为静默强杀可能造成来宾
 * 文件系统损坏（f-2-01 R-006）。
 */
export type PowerAction = 'start' | 'shutdown' | 'reboot' | 'poweroff' | 'reset'

/** 磁盘处理方式。没有默认值——必须由用户显式选择（f-2-01 R-009）。 */
export type DiskAction = 'delete' | 'keep'

export const vmApi = {
  list: (params: VmListParams = {}): Promise<{ items: VmView[]; pagination: Pagination }> =>
    getPaged<VmView>('/api/v1/vms', {
      status: params.status,
      keyword: params.keyword,
      node_id: params.node_id,
      group_name: params.group_name,
      page: params.page,
      page_size: params.page_size,
    }),

  get: (id: number) => get<VmView>(`/api/v1/vms/${id}`),

  create: (input: CreateVmInput) => post<TaskRef>('/api/v1/vms', input),

  power: (id: number, action: PowerAction) =>
    post<TaskRef>(`/api/v1/vms/${id}/power-actions`, { action }),

  // 磁盘处理方式走查询参数：DELETE 携带 body 并非所有客户端都支持，
  // 走 query 更稳妥（服务端两种都接受）。
  remove: (id: number, diskAction: DiskAction) =>
    del<TaskRef>(`/api/v1/vms/${id}?disk_action=${diskAction}`),

  /**
   * 批量操作（F-2-01）：电源与删除。
   *
   * 部分成功语义：某一台失败不影响其它台，因此**不会抛出异常**——
   * 调用方要检查 `failed` 而不是只依赖 catch。
   *
   * 删除时 `diskAction` 必填且**不给默认值**（R-009）：连盘删除不可逆、
   * 保留磁盘会留下孤儿数据，两者代价完全不同，界面上必须让用户自己选。
   */
  batchAction: (vmIDs: number[], action: PowerAction | 'delete', diskAction?: DiskAction) =>
    post<BatchResult>('/api/v1/vms/batch-actions', {
      vm_ids: vmIDs,
      action,
      disk_action: diskAction,
    }),

  /**
   * 加锁 / 解锁（F-2-12）。**同步生效**——锁只在控制面，不需要下发节点。
   *
   * 解锁需要二次验证，但这一点对调用方**透明**：请求层收到 428 时会自动
   * 唤起验证弹框并重放，页面代码不必感知。
   */
  setLock: (id: number, locked: boolean, reason?: string) =>
    patch<VmView>(`/api/v1/vms/${id}/lock`, { locked, reason }),

  /** 网卡列表（详情页「网络管理」标签页）。 */
  interfaces: (id: number) => get<{ items: VMInterface[] }>(`/api/v1/vms/${id}/interfaces`),

  /** 静态地址列表。 */
  staticIPs: (id: number) => get<{ items: StaticIP[] }>(`/api/v1/vms/${id}/static-ips`),

  /**
   * 进入救援模式（F-2-12）。
   *
   * **必须已关机**：救援改动的是引导顺序与盘型，热改会让控制面记录的配置
   * 与虚拟化层实际的配置分叉。进入前会把当前配置存成快照，退出时按它还原。
   */
  enterRescue: (id: number) => post<TaskRef>(`/api/v1/vms/${id}/rescue`),

  /** 退出救援并按快照还原配置。同样需要先关机。 */
  exitRescue: (id: number) => del<TaskRef>(`/api/v1/vms/${id}/rescue`),

  /** 实时运行指标（Hero 资源卡）。只读探测，不入队。 */
  stats: (id: number) => get<VmStats>(`/api/v1/vms/${id}/stats`),

  /**
   * 控制台预览画面的地址。
   *
   * 返回 URL 而不是一个请求函数：这个接口返回的是 **image/png 字节**，
   * 直接给 `<img src>` 用最省事——浏览器会带上会话 Cookie（同源），也能
   * 走它自己的图片解码与缓存。改成 fetch 再转 data URL 等于把这份工作
   * 重做一遍，还多出一次内存拷贝。
   *
   * `stamp` 用于绕过缓存：画面是「此刻的样子」，缓存住会让用户盯着一张
   * 几分钟前的图，还以为虚拟机画面卡死了。
   */
  consoleFrameUrl: (id: number, stamp: number) =>
    `/api/v1/vms/${id}/console/frame?t=${stamp}`,
}

/** 虚拟机的实时运行指标。 */
export interface VmStats {
  /** 相对**全部 vCPU** 的占用率（0-100）。 */
  cpu_percent: number
  /** 配置的核数，用于显示「2 核 · 37%」。 */
  cpu_cores: number

  mem_total_mb: number
  mem_used_mb: number

  net_rx_kbps: number
  net_tx_kbps: number
  disk_read_kbps: number
  disk_write_kbps: number

  /** 为 0 表示未运行。 */
  uptime_seconds: number

  /**
   * 采集时刻。
   *
   * 界面**必须**用它判断新鲜度：轮询失败时会继续显示上一组数字，不标出
   * 时刻，用户就分不清「当前」与「几分钟前」。
   */
  at: string
}

/**
 * 网卡型号。取值与后端 `model.NICModel*` 对应。
 *
 * 这些不是可以随便增减的枚举：每个型号对应一个 QEMU 设备类型，来宾系统里
 * 是否有它的驱动决定网卡能不能用。装完系统发现没网，原因通常就是选错了这里。
 */
export type NICModel = 'virtio' | 'e1000' | 'rtl8139'

/** 虚拟机网卡。 */
export interface VMInterface {
  id: number
  /**
   * 网卡序号，同时是它在**来宾系统里的设备顺序**。
   *
   * 界面以它为主标识而不是 `id`：重建网卡会得到新 id，而 eth0/eth1 是按
   * 顺序认的。
   */
  order: number
  is_primary: boolean
  node_id: number
  model: NICModel
  /** 为空表示使用节点默认网络。 */
  switch_id?: number
  switch_name?: string
  mac?: string
  allowed_addresses?: string
  /** 限速上限（Mbps）；0 表示不限速。 */
  rate_limit_mbps: number
  /** 配置是否已下发到节点。false 表示**尚未生效**，不是失败。 */
  applied: boolean
  last_applied_at?: string
}

/** 分配的静态地址。 */
export interface StaticIP {
  id: number
  ip: string
  address_family: 'ipv4' | 'ipv6'
  interface_order?: number
  mac?: string
  /**
   * 区分 DHCP 静态租约与来宾内手工配置的地址。
   * 两者的排查方向完全不同：前者查 DHCP 服务，后者要进系统看配置文件。
   */
  is_dhcp_reservation: boolean
  applied: boolean
  applied_at?: string
}

/** 网卡型号的中文说明。选错型号是「装完系统没网」最常见的原因。 */
export const NIC_MODEL_LABEL: Record<NICModel, string> = {
  virtio: 'virtio（半虚拟化，性能最好）',
  e1000: 'e1000（Intel 千兆，兼容性最好）',
  rtl8139: 'rtl8139（老旧系统兼容）',
}
