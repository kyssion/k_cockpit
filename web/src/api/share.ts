/**
 * 目录共享接口（F-5-06，9p VirtFS）。
 *
 * **接口只接受相对路径**（相对于调用者的存储根）。这不是风格选择，而是
 * 整块功能的安全前提：共享是一个**跨越虚拟化边界的读取入口**，而参数来自
 * 用户。允许绝对路径意味着一个租户可以把 /etc 挂进自己的虚拟机读出来——
 * 而这不会触发任何权限检查，因为 qemu 是以一个有权读它的用户在跑。
 */
import { del, get, post } from './client'

/** 9p 的安全模型。 */
export type ShareSecurityModel = 'mapped' | 'passthrough' | 'none'

export interface ShareView {
  id: number
  vm_id: number
  node_id: number
  /** 相对于存储根的路径，也就是用户当初填的那个。 */
  rel_path: string
  /** 拼出来的绝对路径（宿主机上实际共享的目录）。 */
  host_path: string
  tag: string
  security_model: ShareSecurityModel
  read_only: boolean
  /** active = 已在节点上生效；pending = 已受理、尚未生效。 */
  status: 'active' | 'pending'
  mounted_at?: string
  created_at: string
}

export const shareApi = {
  list: (vmID: number) => get<{ items: ShareView[] }>(`/api/v1/vms/${vmID}/shares`),

  /**
   * 挂载一个目录共享。
   *
   * `relPath` 是**相对于调用者存储根**的路径。绝对路径与 `..` 一律被拒。
   */
  mount: (
    vmID: number,
    input: {
      rel_path: string
      tag?: string
      security_model?: ShareSecurityModel
      read_only?: boolean
    },
  ) => post<{ task_id: number; status: string }>(`/api/v1/vms/${vmID}/shares`, input),

  /**
   * 卸载。**不需要二次验证**——目录还在宿主机上，随时可以再挂回去，
   * 而来宾里的失效是立刻可见的。
   */
  unmount: (vmID: number, tag: string) =>
    del<{ task_id: number; status: string }>(
      `/api/v1/vms/${vmID}/shares/${encodeURIComponent(tag)}`,
    ),
}

/**
 * 各安全模型的说明。
 *
 * 文案写清**代价**而不只是特性：用户做选择时唯一关心的是「这会让来宾
 * 拥有多大的权限」。
 */
export const SHARE_SECURITY_HINT: Record<
  ShareSecurityModel,
  { label: string; detail: string; dangerous: boolean }
> = {
  mapped: {
    label: 'mapped（推荐）',
    detail:
      '来宾里的 root 写出的文件在宿主机上归运行虚拟机的用户所有，来宾无法伪造文件所有者。代价是大量小文件时略慢。',
    dangerous: false,
  },
  passthrough: {
    label: 'passthrough',
    detail:
      '直接用来宾的 uid/gid，更快。但来宾里的 root 会以 root 身份写出宿主机文件，共享目录与宿主机之间不再有权限边界——仅限完全受信的虚拟机。',
    dangerous: true,
  },
  none: {
    label: 'none',
    detail: '不做任何权限检查，任何人都能读写。仅用于调试。',
    dangerous: true,
  },
}
