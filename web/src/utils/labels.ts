/**
 * 状态与类型的中文映射。
 *
 * 集中在一处：同一状态在不同页面若用了不同措辞，用户会以为是两回事。
 * 颜色语义见 `StatusBadge`（对应 FRONTEND §4.2 的状态色）。
 */
import type { TaskStatus } from '@/api/task'
import type { VmStatus } from '@/api/vm'
import type { StatusTone } from '@/components/common/StatusBadge'

export const VM_STATUS_LABEL: Record<VmStatus, string> = {
  running: '运行中',
  stopped: '已关机',
  paused: '已暂停',
  suspended: '已挂起',
  error: '错误',
  unknown: '未知',
}

/** 虚拟机状态 → 语义色（FRONTEND §4.2：运行中→success，暂停/挂起→warning，已关机/未知→idle）。 */
export const VM_STATUS_TONE: Record<VmStatus, StatusTone> = {
  running: 'success',
  stopped: 'idle',
  paused: 'warning',
  suspended: 'warning',
  error: 'danger',
  unknown: 'idle',
}

export const TASK_STATUS_LABEL: Record<TaskStatus, string> = {
  pending: '排队中',
  running: '执行中',
  success: '成功',
  failed: '失败',
  canceled: '已取消',
  unknown: '状态未知',
}

/** 任务状态 → 语义色（FRONTEND §4.2：排队→info，执行中→brand，成功→success，失败→danger，取消→idle）。 */
export const TASK_STATUS_TONE: Record<TaskStatus, StatusTone> = {
  pending: 'info',
  running: 'brand',
  success: 'success',
  failed: 'danger',
  canceled: 'idle',
  // unknown 用 idle 而非 danger：它不是失败，只是还没有结论。
  // 标成红色会让人去排查一个可能已经成功的操作。
  unknown: 'idle',
}

export const TASK_TYPE_LABEL: Record<string, string> = {
  'vm.create': '创建虚拟机',
  'vm.power': '电源操作',
  'vm.delete': '删除虚拟机',
}

/** 电源动作的中文名（与后端 vm.PowerAction 的取值对应）。 */
export const POWER_ACTION_LABEL: Record<string, string> = {
  start: '开机',
  shutdown: '关机',
  reboot: '重启',
  poweroff: '强制断电',
  reset: '重置',
}

/** 需要二次确认的动作：会造成不可逆后果或中断业务。 */
export const POWER_ACTION_DANGEROUS: Record<string, string> = {
  poweroff: '强制断电会立即中断虚拟机，来宾文件系统可能损坏。请优先使用「关机」。',
}

/** 未登记的类型直接显示原值——比显示「未知操作」更有助于排查。 */
export function taskTypeLabel(type: string): string {
  return TASK_TYPE_LABEL[type] ?? type
}
