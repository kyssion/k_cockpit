/**
 * 虚拟机编辑配置（F-2-05）。
 *
 * 两类修改走**不同路径**，因为它们本质不同：
 *
 *   - 元数据（备注、分组）只存在于控制面，虚拟化层不知道它们的存在，
 *     因此同步改库即可，且**不受运行态限制**；
 *   - 硬件配置（CPU、内存）需要下发到节点，因此走任务队列，且可能要求关机。
 */
import { get, patch, post } from './client'
import type { TaskRef, VmView } from './vm'

/** 控件类型。 */
export type EditKind = 'text' | 'number' | 'boolean' | 'select'

/** 枚举字段的一个可选值。 */
export interface EditOption {
  value: string
  label: string
}

/**
 * 一个可编辑的配置项。
 *
 * **由后端下发，前端不得硬编码第二份**——两份规则漂移的表现是「界面上能改、
 * 提交后被拒」，而只有真正动手操作的用户才会碰到，测试很难覆盖。
 */
export interface EditField {
  key: string
  label: string
  /** 决定用什么控件渲染。 */
  kind: EditKind
  /** 所属的子选项卡。 */
  group: string
  /** 修改是否需要下发到节点。为 false 的是纯控制面元数据。 */
  requires_node: boolean
  /** 是否需要先关机（仅在 requires_node 为 true 时有意义）。 */
  requires_shutdown: boolean
  /**
   * 只展示、不可编辑。
   *
   * 用于**探测结果**（如 Guest Agent 是否在运行）：它不是用户能设定的东西，
   * 做成可编辑的输入框会让人以为「勾上它就能让它跑起来」。
   */
  read_only: boolean
  options?: EditOption[]
  min?: number
  max?: number
  hint?: string
}

/** 一个子选项卡。 */
export interface EditGroupInfo {
  key: string
  label: string
  /** 内容尚未实现。界面据此显示说明而不是一个空表格。 */
  planned: boolean
  note?: string
}

export interface EditForm {
  /** **全部**可编辑项（含需要关机才能改的）：用户需要看到有哪些项可改。 */
  fields: EditField[]
  values: Record<string, unknown>
  /** 以当前运行态能否提交需要下发的修改。 */
  editable_now: boolean
  /**
   * 运行态下仍可热改的键（G-33，如 vcpu / memory_mb）。只在运行态有语义，
   * 键存在且为 true 表示该项当前可改（只能增加）。
   */
  hot_addition?: Record<string, boolean>
  current_status: string
  /** 子选项卡的顺序与名称，同样由后端下发。 */
  groups: EditGroupInfo[]
}

export const editApi = {
  form: (vmID: number) => get<EditForm>(`/api/v1/vms/${vmID}/edit-form`),

  updateMetadata: (vmID: number, input: { remark?: string; group_name?: string }) =>
    patch<VmView>(`/api/v1/vms/${vmID}/metadata`, input),

  /**
   * 提交配置变更。
   *
   * 传 `changes` 而不是具名参数：接口形状由**矩阵**决定，新增一个可编辑项
   * 时只需要改后端矩阵，这里不用动。逐个字段的写法会让新增一项变成三处修改。
   */
  updateConfig: (vmID: number, changes: Record<string, unknown>) =>
    post<TaskRef>(`/api/v1/vms/${vmID}/config-changes`, { changes }),
}
