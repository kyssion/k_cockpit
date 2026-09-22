/**
 * 模板接口（F-3-01 / F-3-02）。
 *
 * 「模板」在本项目里就是**一块准备好并可写的系统盘**，外加一组默认硬件参数。
 * 克隆方式（`clone_mode`）是这个模块最需要留意的地方：
 *
 *   - `full`   复制整块磁盘。慢、占空间，但克隆体与模板**完全独立**；
 *   - `linked` 只记录与模板的差异。秒级完成、几乎不占空间，但**父盘缺失或
 *              被改，数据就不可用了**——而且不会立刻报错，要等到下次开机或
 *              读到某个未缓存的数据块时才暴露。
 *
 * 因此链式克隆必须是用户显式选择，界面上也要把依赖链显示出来。
 */
import { del, get, patch, post } from './client'
import type { TaskRef } from './vm'

export type TemplateStatus = 'preparing' | 'ready' | 'failed'
export type CloneMode = 'full' | 'linked'

export interface TemplateView {
  id: number
  node_id: number
  name: string
  version: number
  status: TemplateStatus
  disk_format: string
  disk_size_gb: number

  os_type?: string
  os_variant?: string
  /** 用它创建虚拟机时磁盘不能小于这个值。 */
  min_disk_gb: number

  default_cpu: number
  default_memory_mb: number

  published: boolean
  visibility: string
  /** 为 false 时不允许再克隆（准备下线但仍要保留已有克隆体）。 */
  clone_enabled: boolean
  immutable: boolean

  /** 非空表示它是从另一个模板派生的。 */
  parent_id?: number
  /**
   * 族：同一条派生链的**根**模板 ID。
   *
   * 它与 version 一起回答"这是第几代、和哪些模板同源"，界面据此把同一族
   * 的多个版本收在一起，而不是只显示一个扁平的"派生自 #N"。
   */
  family_id?: number

  /** 默认硬件，取自源虚拟机，克隆时作为默认值。 */
  default_disk_bus?: string
  default_nic_model?: string
  default_machine_type?: string
  default_firmware?: string
  default_video_model?: string

  error?: string
  remark?: string
  created_at: string
}

/** 删除派生链的策略。由服务端下发，前端不猜能不能用。 */
export type DeleteStrategy = 'cascade' | 'promote'

export const DELETE_STRATEGY_LABEL: Record<DeleteStrategy, { label: string; detail: string }> = {
  cascade: {
    label: '级联删除整条链',
    detail: '连同所有派生自它的版本一起删除。适合这一整条链都不再使用。',
  },
  promote: {
    label: '提升后只删这一个',
    detail:
      '把它的下一级改挂到自己的父级上，只删除这一个版本。适合中间一代已过时、而更新的版本还在用。',
  },
}

export interface TemplateListParams {
  node_id?: number
  keyword?: string
  /** 只返回可用于创建的模板。 */
  only_ready?: boolean
}

/** 模板导出产物（F-3-05）。 */
export interface TemplateExportView {
  id: number
  node_id: number
  template_id: number
  /** 模板名在导出时快照：模板随后可能被删，而包还在。 */
  template_name: string
  filename: string
  size_bytes: number
  status: 'pending' | 'running' | 'success' | 'failed'
  error?: string
  created_at: string
  finished_at?: string
}

export const TEMPLATE_EXPORT_STATUS_LABEL: Record<string, string> = {
  pending: '排队中',
  running: '打包中',
  success: '已完成',
  failed: '失败',
}

/** 模板包清单（导入预览时由节点解出）。 */
export interface TemplateManifestView {
  name: string
  version: number
  family_name?: string
  disk_format: string
  disk_size_gb: number
  os_type?: string
}

export interface ImportPreviewView {
  source_name: string
  manifest: TemplateManifestView
  can_import: boolean
  reason?: string
  message?: string
}

export interface CreateFromVmInput {
  vm_id: number
  name: string
  os_type?: string
  os_variant?: string
  remark?: string
  published?: boolean
  /** 非空表示制备的是**某个模板的新版本**（同一族，版本自增）。 */
  parent_id?: number
}

export interface DeleteBlocker {
  kind: 'linked_vm' | 'child_template'
  label: string
  unit: string
  count: number
  names: string[]
  /** 怎么解决。不能只说「不行」——那是用户在预览里唯一能照着做的。 */
  fix: string
}

export interface DeletePreview {
  can_delete: boolean
  blockers: DeleteBlocker[]
  /** 删除后会释放的磁盘文件。 */
  disk_path: string
  linked_vm_count: number
  child_template_count: number
  /** 存在派生模板时可选的删除策略；为空表示没有策略可用。 */
  strategies: DeleteStrategy[]
}

export const templateApi = {
  list: (params: TemplateListParams = {}) =>
    get<TemplateView[]>('/api/v1/templates', {
      node_id: params.node_id,
      keyword: params.keyword,
      only_ready: params.only_ready ? 'true' : undefined,
    }),

  get: (id: number) => get<TemplateView>(`/api/v1/templates/${id}`),

  /**
   * 从此模板批量创建虚拟机。
   *
   * **一次最多 5 台**，且服务端在超限时会把**理由**写在报错里——理由与存储
   * 有关：每台都要完整读一遍父盘再写一份新的，同时进行的台数越多，宿主机
   * 上的存储被占得越久，表现为所有虚拟机都变慢。
   *
   * **整批先查重**：只要有一个名字被占用就整批拒绝（逐台跳过重名会建出带洞
   * 的结果）。**部分失败不回滚**：已建好的那几台不会被撤销。
   */
  batchClone: (input: {
    name_prefix: string
    count: number
    node_id: number
    vcpu: number
    memory_mb: number
    disk_gb: number
    template_id: number
    clone_mode?: string
    group_name?: string
  }) =>
    post<{
      created: { name: string; task_id?: number }[] | null
      failed: { name: string; reason?: string }[] | null
      message: string
    }>('/api/v1/vms/batch-clone', input),

  /**
   * 从一台虚拟机的系统盘制备模板。
   *
   * **要求源虚拟机关机**：运行中的系统盘在被复制的同时还在被写入，复制出来
   * 的模板是崩溃一致性的快照——而它会被反复克隆成新机器，于是每一台克隆机
   * 开机都要做 fsck，运气不好就是只读挂载。
   */
  createFromVM: (input: CreateFromVmInput) => post<TaskRef>('/api/v1/templates', input),

  /**
   * 修改模板（发布 / 停止提供克隆 / 备注）。
   *
   * **同步生效**：这些都是纯控制面元数据，不影响宿主机上的任何东西。
   * 做成任务会制造一个「界面说已发布、实际还没发布」的窗口，而发布与否
   * 决定了别人能不能用。
   */
  update: (
    id: number,
    input: { published?: boolean; clone_enabled?: boolean; visibility?: string; remark?: string },
  ) => patch<TemplateView>(`/api/v1/templates/${id}`, input),

  /** 删除模板。仍有链式克隆依赖时会被**同步拒绝**并给出数量。 */
  /**
   * 删除前的检查。**只读**。
   *
   * 这些约束本来就有（删除时会被拒绝），但用户只有在点了删除之后才会撞上
   * ——而那时他看到的是一个错误提示，不是一份待办清单。预览把这件工作放在
   * 「按下按钮之前」，并且给出**具体是哪几台**，而不只是一个计数。
   */
  deletePreview: (id: number) =>
    get<DeletePreview>(`/api/v1/templates/${id}/delete-preview`),

  /**
   * 删除模板。
   *
   * 链式克隆会**同步拒绝**且没有策略可绕过（任何策略都意味着接受数据丢失）；
   * 派生模板则要在 `strategy` 里显式选择级联或提升——服务端不替用户决定。
   */
  remove: (id: number, strategy?: DeleteStrategy) =>
    del<TaskRef>(`/api/v1/templates/${id}${strategy ? `?strategy=${strategy}` : ''}`),

  /** 同一模板族的全部版本（F-3-04）。 */
  family: (id: number) => get<TemplateView[]>(`/api/v1/templates/${id}/family`),

  // --- 导出与导入（F-3-05）---

  /** 导出一个模板。打包几十 GB 的镜像，因此返回任务标识。 */
  exportTemplate: (id: number) => post<TaskRef>(`/api/v1/templates/${id}/exports`),

  listExports: (nodeID?: number) =>
    get<{ items: TemplateExportView[] }>('/api/v1/template-exports', { node_id: nodeID }),

  removeExport: (id: number) => del<TaskRef>(`/api/v1/template-exports/${id}`),

  /** 预览模板包。**先验后做**：名字撞了、格式不对、摘要不符都在这里看见。 */
  previewImport: (fileID: number) =>
    post<ImportPreviewView>('/api/v1/templates/imports/preview', { file_id: fileID }),

  importTemplate: (fileID: number) =>
    post<TaskRef>('/api/v1/templates/imports', { file_id: fileID }),
}

export const TEMPLATE_STATUS_LABEL: Record<TemplateStatus, string> = {
  preparing: '制备中',
  ready: '可用',
  failed: '制备失败',
}

export const TEMPLATE_STATUS_TONE: Record<TemplateStatus, 'warning' | 'success' | 'danger'> = {
  preparing: 'warning',
  ready: 'success',
  failed: 'danger',
}

/**
 * 克隆方式的说明文案。
 *
 * 写成「代价是什么」而不是「有什么特性」：用户选 linked 之前必须知道
 * 自己的虚拟机从此**依赖另一块盘**，而那块盘被删掉时不会立刻报错。
 */
export const CLONE_MODE_HINT: Record<CloneMode, { label: string; detail: string }> = {
  full: {
    label: '完整克隆',
    detail: '复制整块系统盘。慢一些、占空间，但与模板完全独立——模板删掉也不影响这台机器。',
  },
  linked: {
    label: '链式克隆（快，但有依赖）',
    detail:
      '只记录与模板的差异，秒级完成、几乎不占空间。但磁盘只是模板之上的一个覆盖层：模板被删除后这台机器的数据会不可用，而且不会立刻报错。',
  },
}

/** 派生链维护的动作。 */
export type TemplateMaintainAction = 'rebase' | 'flatten' | 'promote_child' | 'promote_delete'

export const TEMPLATE_MAINTAIN_LABEL: Record<TemplateMaintainAction, string> = {
  rebase: '把 backing 切到上级',
  flatten: '在线拉平到上级',
  promote_child: '提升子模板',
  promote_delete: '删除这一代（下游改挂上级）',
}

export const templateMaintainApi = {
  rebase: (id: number, acknowledge: boolean) =>
    post<{ task_id: number; status: string }>(`/api/v1/templates/${id}/rebase`, { acknowledge }),
  flatten: (id: number, acknowledge: boolean) =>
    post<{ task_id: number; status: string }>(`/api/v1/templates/${id}/flatten`, { acknowledge }),
  promoteChild: (id: number, childID: number, acknowledge: boolean) =>
    post<{ task_id: number; status: string }>(`/api/v1/templates/${id}/promote-child`, {
      child_id: childID,
      acknowledge,
    }),
  promoteDelete: (id: number, acknowledge: boolean) =>
    post<{ task_id: number; status: string }>(`/api/v1/templates/${id}/promote-delete`, {
      acknowledge,
    }),
}

/**
 * 离线预处理（F-3-06）：判定与执行都在节点侧——要挂载镜像并改写里面的内容，控制面既没有工具链也不挂载镜像，这里只把选项固化进任务参数。
 */
export interface PreprocessInput {
  install_agent?: boolean
  inject_ssh_key?: boolean
  reset_machine_id?: boolean
  remove_cloud_init?: boolean
  acknowledge?: boolean
}

export const templatePreprocessApi = {
  run: (id: number, input: PreprocessInput) =>
    post<{ task_id: number; status: string }>(`/api/v1/templates/${id}/preprocess`, input),
}
