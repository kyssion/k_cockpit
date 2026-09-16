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

/**
 * 一个可编辑的配置项。
 *
 * **由后端下发，前端不得硬编码第二份**——两份规则漂移的表现是「界面上能改、
 * 提交后被拒」，而只有真正动手操作的用户才会碰到，测试很难覆盖。
 */
export interface EditField {
  key: string
  label: string
  kind: 'number' | 'text'
  /** 所属的子选项卡。 */
  group: string
  /** 修改是否需要下发到节点。为 false 的是纯控制面元数据。 */
  requires_node: boolean
  /** 是否需要先关机（仅在 requires_node 为 true 时有意义）。 */
  requires_shutdown: boolean
  min?: number
  max?: number
  hint?: string
}

export interface EditForm {
  /** **全部**可编辑项（含需要关机才能改的）：用户需要看到有哪些项可改。 */
  fields: EditField[]
  values: Record<string, unknown>
  /** 以当前运行态能否提交需要下发的修改。 */
  editable_now: boolean
  current_status: string
}

export const editApi = {
  form: (vmID: number) => get<EditForm>(`/api/v1/vms/${vmID}/edit-form`),

  updateMetadata: (vmID: number, input: { remark?: string; group_name?: string }) =>
    patch<VmView>(`/api/v1/vms/${vmID}/metadata`, input),

  updateConfig: (vmID: number, input: { vcpu?: number; memory_mb?: number }) =>
    post<TaskRef>(`/api/v1/vms/${vmID}/config-changes`, input),
}

/** 子选项卡的中文名，与后端 `EditGroup*` 常量对应。 */
export const EDIT_GROUP_LABEL: Record<string, string> = {
  basic: '基础配置',
  disk: '磁盘与驱动器',
  boot: '启动与安全',
  network: '网口',
  passthru: '硬件直通',
  advanced: '高级设置',
}

/** 子选项卡的展示顺序（与 FRONTEND.md §5.3.3 一致）。 */
export const EDIT_GROUP_ORDER = ['basic', 'disk', 'boot', 'network', 'passthru', 'advanced']
