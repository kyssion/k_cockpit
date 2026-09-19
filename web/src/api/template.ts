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
  error?: string
  remark?: string
  created_at: string
}

export interface TemplateListParams {
  node_id?: number
  keyword?: string
  /** 只返回可用于创建的模板。 */
  only_ready?: boolean
}

export interface CreateFromVmInput {
  vm_id: number
  name: string
  os_type?: string
  os_variant?: string
  remark?: string
  published?: boolean
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

  remove: (id: number) => del<TaskRef>(`/api/v1/templates/${id}`),
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
