/**
 * 虚拟机接口（F-2-01 ~ F-2-04）。
 *
 * 两点需要留意：
 *   - `status` 是**投影字段**，权威在虚拟化层，控制面只是缓存。配合 `stale`
 *     一起看：`stale=true` 说明数据可能已经过期，界面必须提示而不是照常显示。
 *   - 创建是**异步**的：接口返回任务标识而非创建好的虚拟机，前端据任务进度
 *     跟踪结果（f-7-01 R-001）。
 */
import { del, get, getPaged, patch, post, type Pagination, put } from './client'
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
   * 引导固件（bios / uefi）。
   *
   * 快照页据此决定是否显示「修复 UEFI 启动项」：BIOS 机器没有启动项可修，
   * 给一个点了必然报错的按钮，只会让人以为修复失败了。
   */
  firmware?: string

  /**
   * 标签。**只在列表接口填充**——详情只有一台，由它的标签组件按需取。
   *
   * 列表带它是因为标签是"这台机器是干什么的"的主要表达（分组之外唯一的
   * 自由度）；后端用一次批量查询填充，不是逐台关联。
   */
  tags?: string[]

  /**
   * 最近一次采样的资源占用；没有采样时为 undefined。
   *
   * undefined 与 0 必须分开：0 看起来像"这台机器很闲"，而实际可能是还没
   * 采到或机器已关机（采集器只采运行中的）。
   */
  usage?: VmUsage

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

  /**
   * 重装留下了一份原系统盘备份，可以清理。
   *
   * **只有有无，没有路径**：路径是宿主机上的内部细节。界面需要知道的只是
   * 「有一份备份、可以清理」，以及「它挡着下一次重装」。
   */
  has_reinstall_backup: boolean
  reinstall_at?: string
}

/** 详情页时间线的一条（合并审计与任务流水）。 */
export interface VmTimelineItem {
  at: string
  kind: 'task' | 'audit'
  title: string
  detail?: string
  success?: boolean
  task_id?: number
}

/** PCIe 根端口情况（决定还能不能热插拔）。 */
export interface VmPcieInfo {
  vm_id: number
  total: number
  free: number
  used: number
  machine_type: string
  hotplug_supported: boolean
  reason?: string
  unavailable?: string
}

/** 邻居表的一条（ARP / NDP）。 */
export interface VmNeighbor {
  ip: string
  mac?: string
  interface?: string
  state?: string
  is_self: boolean
  vm_id?: number
  vm_name?: string
  bridge?: string
}

export interface VmNeighbors {
  vm_id: number
  items: VmNeighbor[]
  unavailable?: string
}

/** 创建时要一并建立的一块数据盘。 */
export interface DataDiskInput {
  size_gb: number
  format?: string
  bus?: string
}

/** 最近一次采样的资源占用。 */
export interface VmUsage {
  cpu_percent: number
  mem_percent: number
  mem_used_mb: number
  /** 采样时刻——列表上必须能看出这不是"此刻"的数字。 */
  at: string
}

export interface VmListParams {
  status?: string
  keyword?: string
  node_id?: number
  group_name?: string
  /** 排序字段：name / vcpu / memory / disk / ip / created_at。未知取值由后端回落到默认顺序。 */
  sort_by?: string
  /** desc 为降序，其余均按升序。 */
  order?: 'asc' | 'desc'
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

  // --- 创建向导的其余配置（F-2-02）---
  //
  // 键名与后端矩阵一致，取值与范围**以后端下发的表单为准**：这里不写死
  // 第二份可选值，界面只是照着渲染。

  disk_format?: string
  disk_bus?: string
  nic_model?: string
  os_type?: string
  /** 具体系统版本（libosinfo short id）。 */
  os_variant?: string
  /** 主机名；留空用虚拟机名称。 */
  hostname?: string
  /**
   * 初始登录密码。创建时注入，之后**只写不读**（R-005）。
   *
   * 它不进草稿（见向导里的说明）：把凭据留在 localStorage 里等于给它一个
   * 不出门的泄漏面。
   */
  initial_password?: string
  /** 首次启动初始化方式：none / nocloud / configdrive / openwrt。 */
  init_mode?: string
  /** 主网口静态地址；留空由 DHCP 分配。 */
  static_ip?: string
  /** 除系统盘之外要一并创建的数据盘。 */
  data_disks?: DataDiskInput[]
  machine_type?: string
  firmware?: string
  secure_boot?: boolean
  boot_order?: string
  auto_start?: boolean
  watchdog?: string
  cpu_type?: string
  cpu_limit_percent?: number
  apic?: boolean
  pae?: boolean
  freeze_on_start?: boolean
  disk_iops_total?: number
  disk_iops_read?: number
  disk_iops_write?: number

  /** 非零表示创建成功后把该镜像挂到光驱——ISO 安装路径。 */
  iso_file_id?: number
  switch_id?: number
  security_group_ids?: number[]

  /** 批量台数（默认 1）；多台时 name 作为前缀。 */
  count?: number
  /** 幂等键：重复提交只产生一个任务（F-2-02 Q-003）。 */
  client_token?: string
  batch_key?: string
}

/** 一块磁盘。列表来自**节点实时探测**，控制面不持有磁盘记录。 */
export interface VmDiskView {
  dev: string
  capacity_gb: number
  /** 宿主机上的实际占用。qcow2 是稀疏文件，它通常远小于配置容量。 */
  actual_bytes: number
  format: string
  bus: string
  source: string
  is_system: boolean
  /** 当前能否热插拔，由节点按机型与空闲槽位判断。 */
  hotpluggable: boolean

  /** 以下由控制面按运行态算出：界面按它禁用按钮，后端按它拒绝请求。 */
  can_detach: boolean
  detach_reason?: string
  can_change_bus: boolean
  change_bus_reason?: string
  /** 系统盘也可以迁移；能否迁移只取决于有没有别的目标池。 */
  can_migrate: boolean
  migrate_reason?: string
}

/** 可迁移过去的存储池。 */
export interface DiskTarget {
  id: number
  name: string
  path?: string
  usable_gb: number
}

/** 磁盘限速：IOPS 与吞吐并存，它们限制的是不同性质的负载。 */
export interface DiskLimits {
  iops_total: number
  iops_read: number
  iops_write: number
  /** 单位 MB/s。 */
  bytes_total: number
  bytes_read: number
  bytes_write: number
}

export interface VmDiskList {
  /** 探测到的运行态——操作可用性以它为依据，而不是可能滞后的投影。 */
  status: string
  disks: VmDiskView[]
  bus_options: { value: string; label: string }[]
  /** 可挂载的虚拟磁盘文件（我的存储里的 disk 类，同节点）。 */
  attachables: { id: number; filename: string; size_bytes: number }[]
  /** 迁移目标。为空时界面不显示迁移入口。 */
  migrate_targets: DiskTarget[]
  limits: DiskLimits
}

export type DiskChangeAction = 'attach' | 'detach' | 'bus' | 'migrate'

/** 磁盘的挂载 / 卸载 / 换总线 / 迁移。四者共用一个接口，由 action 区分。 */
export interface DiskChangeInput {
  action: DiskChangeAction
  /** 卸载、换总线与迁移时必填；挂载时由节点分配。 */
  dev?: string
  bus?: string
  file_id?: number
  /** 迁移目标存储池。 */
  target_pool_id?: number
  /**
   * 运行中仍继续（热迁移）。
   *
   * 由用户确认而不是服务端默认：热迁移期间磁盘仍在使用，是否接受那段
   * 抖动只有使用者知道。
   */
  allow_hot?: boolean
}

/** 创建向导的一个配置项（由后端下发，界面只负责渲染）。 */
export interface CreateFormField {
  key: string
  label: string
  kind: 'text' | 'number' | 'boolean' | 'select'
  group: string
  requires_node: boolean
  requires_shutdown: boolean
  read_only: boolean
  in_create: boolean
  create_only: boolean
  default?: string
  options?: { value: string; label: string }[]
  min?: number
  max?: number
  hint?: string
}

/** 创建向导的一个步骤。 */
export interface CreateFormGroup {
  key: string
  label: string
  /** 该步骤的内容尚未实现；界面据此显示说明而不是空表格。 */
  planned?: boolean
  note?: string
}

/** 一项创建前置条件。 */
export interface CreatePrerequisite {
  key: string
  label: string
  ok: boolean
  message?: string
  link?: string
}

/** 创建向导的表单元数据（F-2-02 R-002：规则由后端下发）。 */
export interface CreateForm {
  fields: CreateFormField[]
  values: Record<string, unknown>
  groups: CreateFormGroup[]
  prerequisites: CreatePrerequisite[]
  iso_files: { id: number; filename: string; size_bytes: number; os_type?: string; min_disk_gb: number }[]
  switches: { id: number; name: string; mode: string; cidr?: string; is_system: boolean }[]
  security_groups: { id: number; name: string; is_default: boolean }[]
  can_submit: boolean
}

/** 创建的结果：单台与批量都返回全部任务标识。 */
export interface CreateResult {
  task_id: number
  task_ids: number[]
  count: number
  batch_key?: string
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

/**
 * 磁盘处理方式。没有默认值——必须由用户显式选择（f-2-01 R-009）。
 *
 * transfer 把磁盘文件搬回「我的存储 - 虚拟磁盘」：keep 只是把文件留在原地
 * 不删，那块盘会继续占着宿主机的空间，却不属于任何虚拟机，谁也看不见它。
 */
export type DiskAction = 'delete' | 'keep' | 'transfer'

export const DISK_ACTION_LABEL: Record<DiskAction, string> = {
  delete: '连同磁盘删除',
  keep: '保留磁盘文件',
  transfer: '转移到我的存储',
}

/** 回收站里的一台虚拟机。 */
export interface TrashItem {
  id: number
  name: string
  node_id: number
  vcpu: number
  memory_mb: number
  disk_gb: number
  status: VmStatus
  deleted_at?: string
  /** false 表示还有任务在跑，暂时不能彻底删除。 */
  purgeable: boolean
}

export const vmApi = {
  /** 回收站列表（F-2-16）。删除只是移出列表，磁盘未动。 */
  trash: () => get<{ items: TrashItem[] }>('/api/v1/vms/trash'),

  /** 恢复。恢复**不自动开机**。 */
  restore: (id: number) => post<{ ok: boolean }>(`/api/v1/vms/trash/${id}/restore`),

  /** 彻底删除：删盘 + 物理删记录，入队执行。 */
  purge: (id: number) => del<TaskRef>(`/api/v1/vms/trash/${id}`),

  list: (params: VmListParams = {}): Promise<{ items: VmView[]; pagination: Pagination }> =>
    getPaged<VmView>('/api/v1/vms', {
      status: params.status,
      keyword: params.keyword,
      node_id: params.node_id,
      group_name: params.group_name,
      sort_by: params.sort_by,
      order: params.order,
      page: params.page,
      page_size: params.page_size,
    }),

  get: (id: number) => get<VmView>(`/api/v1/vms/${id}`),

  /** 修改备注与分组。它们是**纯控制面元数据**，改它们不下发节点、不用关机。 */
  updateMetadata: (id: number, input: { remark?: string; group_name?: string }) =>
    patch<VmView>(`/api/v1/vms/${id}/metadata`, input),

  /** 磁盘列表（F-2-06）。实时向节点查询：磁盘是虚拟化层的状态，控制面存一份就要与它对账。 */
  disks: (id: number) => get<VmDiskList>(`/api/v1/vms/${id}/disks`),

  /** 挂载 / 卸载 / 换总线。三者都入队，返回任务标识。 */
  changeDisk: (id: number, input: DiskChangeInput) =>
    post<TaskRef>(`/api/v1/vms/${id}/disks`, input),

  /** 表单元数据：字段、取值、默认值与前置条件全部由后端下发（F-2-02 R-002）。 */
  createForm: (nodeID: number) => get<CreateForm>('/api/v1/vms/create-form', { node_id: nodeID }),

  /** 创建一台或多台。返回值含全部任务标识，便于逐台跟踪。 */
  create: (input: CreateVmInput) => post<CreateResult>('/api/v1/vms', input),

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

  /**
   * 重装系统（F-2-11）。用模板重建系统盘，**保留硬件配置、数据盘与主网口
   * 绑定**，只替换系统盘。
   *
   * 高风险，需要二次验证——但这一点对调用方**透明**：请求层收到 428 时会
   * 自动唤起验证弹框并重放。整块系统盘会被替换，原系统上的软件与配置全部
   * 消失（数据盘保留）。
   *
   * 前置条件：已关机、无在途任务、**没有未清理的备份**（第二次重装会覆盖
   * 上一次的备份，而那是用户唯一的退路）。
   */
  reinstall: (id: number, templateID: number) =>
    post<TaskRef>(`/api/v1/vms/${id}/reinstall`, { template_id: templateID }),

  /** 清理重装留下的备份盘。需要先关机，但**不需要**二次验证。 */
  purgeReinstallBackup: (id: number) =>
    del<TaskRef>(`/api/v1/vms/${id}/reinstall/backup`),

  /**
   * 导出记录列表（F-2-14）。
   *
   * 记录**在受理时**就创建（pending），不是等导出完才建——导出可能跑
   * 几十分钟，用户需要在那段时间里看到「有一个导出在进行」。
   */
  exports: (id: number) => get<{ items: VmExport[] }>(`/api/v1/vms/${id}/exports`),

  /**
   * 发起导出。
   *
   * **要求关机**，理由比其它操作更强：产物会被搬到别的地方使用，因此必须是
   * 一个干净的、自洽的镜像。运行中导出得到的是崩溃一致性快照，而导入方
   * 往往不在你手边。
   */
  createExport: (id: number, input: { format: ExportFormat; include_data_disks: boolean }) =>
    post<TaskRef>(`/api/v1/vms/${id}/exports`, input),

  /** 删除导出产物。产物是副本，删掉不影响虚拟机本身。 */
  deleteExport: (id: number, exportID: number) =>
    del<TaskRef>(`/api/v1/vms/${id}/exports/${exportID}`),

  /**
   * 导出产物的下载地址。
   *
   * 返回 URL 而不是请求函数：这个接口返回的是**产物字节**，交给浏览器直接
   * 处理最省事——它会带上会话 Cookie（同源），并自己处理大文件落盘、进度
   * 与断点。走 fetch 再转 Blob 等于把这些重做一遍。
   */
  exportDownloadUrl: (id: number, exportID: number) =>
    `/api/v1/vms/${id}/exports/${exportID}/download`,

  /**
   * 当前可用的来宾自动化动作（F-2-10）。
   *
   * 由**后端按当前状态算好下发**，界面不自行判断：可用性与「是否装了
   * Guest Agent」「是否运行中」都相关，前端各判一遍迟早会与后端不一致——
   * 那会出现「按钮可点、点下去被拒」这类最让人烦躁的交互。
   */
  guestActions: (id: number) => get<GuestCapabilities>(`/api/v1/vms/${id}/guest-actions`),

  /**
   * 执行一次来宾自动化。
   *
   * 四种动作共用一个入口：它们都要「先做宿主机侧的准备、再进来宾执行」。
   * **密码只在请求里传一次**——入队时写入任务参数供执行器取用，执行完成后
   * 立即从参数里清除，审计流水里也不含它（R-009）。
   */
  runGuestAction: (
    id: number,
    input: { action: GuestAction; username?: string; password?: string; disk_id?: string; disk_gb?: number },
  ) => post<TaskRef>(`/api/v1/vms/${id}/guest-actions`, input),

  /**
   * 受理一次跨节点迁移（F-2-09）。
   *
   * **要求关机**：运行中迁移会让磁盘在被写入的同时被复制，两侧都不可用。
   * 受理时会同步检查目标节点是否维护中、静态地址与端口转发是否冲突——
   * 有冲突直接拒绝并列出是哪一项，而不是迁过去之后再出问题。
   *
   * 不需要二次验证：失败时源侧数据保留，虚拟机在原处仍然可用。
   */
  /**
   * 迁移预检。**只读**，不产生任何记录。
   *
   * 它回答点下按钮**之前**唯一想知道的那件事：**这次要停多久。** 因此除了
   * 「能不能迁」（ready / blockers），还有「会怎么迁」（mode / disk_gb /
   * downtime_hint）——停机时长由后者决定。
   *
   * 它**与 migrate 共用同一套校验**，所以预览说可以、点下去就不会被拒。
   */
  previewMigration: (id: number, toNodeID: number) =>
    post<MigrationPreview>(`/api/v1/vms/${id}/migration/preview`, { to_node_id: toNodeID }),

  migrate: (id: number, toNodeID: number) =>
    post<TaskRef>(`/api/v1/vms/${id}/migrate`, { to_node_id: toNodeID }),

  /**
   * 迁移记录。
   *
   * 用来回答「这台机器原来在哪台宿主机上」——那是排查存储、网络、性能问题
   * 时第一条要看的东西，而 `node_id` 只记录了「现在在哪」。
   */
  migrations: (id: number) => get<{ items: MigrationView[] }>(`/api/v1/vms/${id}/migrations`),

  /** 实时运行指标（Hero 资源卡）。只读探测，不入队。 */
  timeline: (id: number, limit?: number) =>
    get<{ items: VmTimelineItem[] }>(`/api/v1/vms/${id}/timeline`, { limit }),
  pcieInfo: (id: number) => get<VmPcieInfo>(`/api/v1/vms/${id}/pcie-info`),
  neighbors: (id: number) => get<VmNeighbors>(`/api/v1/vms/${id}/neighbors`),
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
  /**
   * 虚拟机的 libvirt 定义（**只读**）。
   *
   * live 区分运行中与持久定义：热插拔一块盘之后两份会不同，而"看的是哪一份"
   * 决定用户能不能据此判断"重启后还在不在"。
   */
  xml: (id: number, live: boolean) =>
    get<{ vm_id: number; live: boolean; xml: string; redacted?: string[] }>(
      `/api/v1/vms/${id}/xml?live=${live}`,
    ),

  /**
   * 关机状态下的磁盘扩容（**只能扩，不能缩**）。
   *
   * 运行中的机器会被拒并指向「来宾自动化」里的 expand_disk——那条路会顺带
   * 在来宾里扩好文件系统。这条路径只扩宿主机侧，来宾里的分区要自己扩
   * （响应里的 guest_grow_needed 标出这一点）。
   */
  resizeDisk: (id: number, sizeGB: number) =>
    post<{ old_gb: number; new_gb: number; guest_grow_needed: boolean }>(
      `/api/v1/vms/${id}/disk/resize`,
      { size_gb: sizeGB },
    ),

  /**
   * 把链接克隆的磁盘合并为独立盘。
   *
   * **它存在的理由是一件事：链接克隆的父盘删不掉。** 模板的管理需要能删掉
   * 旧的父盘，而这一步就是让这台机器不再依赖它。
   *
   * **需要停机**：合并要复制整个镜像，而运行中的机器还在往那层覆盖里写。
   */
  makeDisksIndependent: (id: number) =>
    post<{ task: unknown }>(`/api/v1/vms/${id}/disks/independent`, {}),

  /** 校验一份新的定义并给出 diff。**只读**，不应用，也不需要二次验证。 */
  xmlPrecheck: (id: number, xml: string) =>
    post<XMLPrecheck>(`/api/v1/vms/${id}/xml/precheck`, { xml }),

  /**
   * 应用一份新的定义。
   *
   * **需要二次验证**：它绕过控制面建立的其它全部校验（配额、地址唯一性、
   * 前置条件）。服务端在 428 上会说明这一点，前端据此弹出验证流程。
   */
  xmlUpdate: (id: number, xml: string) =>
    put<XMLPrecheck>(`/api/v1/vms/${id}/xml`, { xml }),

  consoleFrameUrl: (id: number, stamp: number) =>
    `/api/v1/vms/${id}/console/frame?t=${stamp}`,
}

/** 来宾自动化动作（F-2-10）。 */
export type GuestAction = 'password_online' | 'password_offline' | 'disk_attach' | 'expand_disk'

export interface XMLDiffLine {
  kind: 'same' | 'add' | 'del'
  old_no?: number
  new_no?: number
  text: string
}

export interface XMLPrecheck {
  diff: { lines: XMLDiffLine[]; added: number; removed: number; identical: boolean }
  valid: boolean
  /** 节点给的校验问题，**原样返回不改写**——里面有行号与元素名。 */
  errors?: string[]
  warnings?: string[]
}

export interface GuestCapabilities {
  action?: string
  /** 该动作是否依赖来宾里的 QEMU Guest Agent。 */
  requires_guest_agent: boolean
  /** **探测到的** agent 状态。与上面分开：前者是「要不要」，后者是「有没有」。 */
  guest_agent_ready: boolean
  requires_running: boolean
  /** 当前状态下可执行的动作。 */
  available_actions: string[]
}

/**
 * 来宾自动化动作的说明。
 *
 * 文案写清「走哪条路」：在线与离线改密对用户来说都是「改密码」，但前者经
 * Guest Agent 进来宾执行，后者绕开来宾直接挂盘——事后追查「为什么改完密码
 * 后 SELinux 上下文不对」时，这一条是第一个要看的线索。
 */
export const GUEST_ACTION_HINT: Record<GuestAction, { label: string; detail: string; requiresRunning: boolean }> = {
  password_online: {
    label: '在线改密',
    detail: '经 Guest Agent 在运行的来宾里修改。最安全：不挂载磁盘，也不碰文件系统元数据。需要虚拟机运行中且装了 Guest Agent。',
    requiresRunning: true,
  },
  password_offline: {
    label: '离线改密',
    detail:
      '把系统盘挂到宿主机上直接改。用在来宾起不来或没装 agent 的时候——但它是绕开来宾系统改的，改完首次开机时可能要做一次上下文修复。需要关机。',
    requiresRunning: false,
  },
  disk_attach: {
    label: '附加磁盘并自动挂载',
    detail: '为磁盘分区、格式化并挂载进来宾。**格式化不可逆**，请先确认目标盘上没有需要的数据。需要运行中且装了 Guest Agent。',
    requiresRunning: true,
  },
  expand_disk: {
    label: '系统盘扩容',
    detail:
      '先加长虚拟磁盘，再进来宾扩展文件系统。部分文件系统不支持在线扩容，那时需要重启后再做一次。需要关机。',
    requiresRunning: false,
  },
}

/** 一次跨节点迁移的记录。 */
export interface MigrationPreview {
  vm_id: number
  vm_name: string
  from_node_id: number
  from_node_name: string
  to_node_id: number
  to_node_name: string
  /** false 时 blockers 说明原因。 */
  ready: boolean
  /** 会**阻止**迁移的条件。 */
  blockers?: string[]
  /** 不阻止但应当知道的事。 */
  warnings?: string[]
  mode: string
  /** 用人话解释这种方式意味着什么。 */
  mode_note: string
  /** 要复制的数据量——停机时长的**主要因素**。 */
  disk_gb: number
  /** 停机时长的**量级与依据**，而不是精确数字。 */
  downtime_hint: string
}

export interface MigrationView {
  id: number
  vm_id: number
  vm_name: string
  from_node_id: number
  to_node_id: number
  status: 'pending' | 'running' | 'success' | 'failed'
  /** 说明跟着搬了些什么（网卡、静态地址、端口转发的数量）。 */
  result?: string
  error?: string
  created_at: string
  finished_at?: string
}

/** 导出格式。 */
export type ExportFormat = 'qcow2' | 'ova'

/** 一次导出的产物记录。 */
export interface VmExport {
  id: number
  vm_id: number
  vm_name: string
  format: ExportFormat
  status: 'pending' | 'running' | 'success' | 'failed'
  include_data_disks: boolean
  /** 只在导出完成后有值——进行中时给了会让界面显示一个点了拿不到东西的链接。 */
  file_name?: string
  /** 计入用户的存储配额。 */
  size_bytes: number
  error?: string
  created_at: string
  finished_at?: string
}

export const EXPORT_FORMAT_HINT: Record<ExportFormat, { label: string; detail: string }> = {
  qcow2: {
    label: 'QCOW2 系统盘',
    detail: '得到一块可直接被 QEMU 使用的磁盘镜像。体积小，但只有懂 QEMU 的人能用。',
  },
  ova: {
    label: 'OVA 包',
    detail:
      '标准 OVA 包（含 OVF 描述）。比裸镜像大一些，但可被 VirtualBox、ESXi 等直接导入——可移植性的代价是体积。',
  },
}

export const EXPORT_STATUS_LABEL: Record<VmExport['status'], string> = {
  pending: '排队中',
  running: '导出中',
  success: '已完成',
  failed: '失败',
}

export const EXPORT_STATUS_TONE: Record<
  VmExport['status'],
  'idle' | 'warning' | 'success' | 'danger'
> = {
  pending: 'idle',
  running: 'warning',
  success: 'success',
  failed: 'danger',
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

/**
 * 分配虚拟机归属（管理员）。
 *
 * 它补的是一个已知欠账：管理员删除用户时若该用户名下还有虚拟机，只能拒绝
 * 并提示"请先转移"——而"转移"此前没有实现。
 */
export const vmOwnerApi = {
  assignOwner: (vmID: number, userID: number) =>
    put<VmView>(`/api/v1/vms/${vmID}/owner`, { user_id: userID }),
}
