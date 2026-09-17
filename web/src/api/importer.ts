/**
 * 磁盘与镜像导入接口（F-2-13）。
 *
 * **本面板不接收真实文件内容**：受理时只提交文件名、大小与格式（都是浏览器
 * 通过 `<input type="file">` 能直接读到的元数据）。整条链路——受理、状态
 * 流转、转换后建模板、配额记账——都可验证，唯独「字节怎么从浏览器到宿主机」
 * 这一段留空。那一段必须真实实现，且是最难的部分之一。
 */
import { del, get, post } from './client'
import type { TaskRef } from './vm'

export type ImportFormat = 'qcow2' | 'raw' | 'vmdk' | 'vhd' | 'vhdx' | 'img' | 'ova'
export type ImportStatus = 'pending' | 'running' | 'success' | 'failed'

/** 解析出来的配置预览。 */
export interface ImportPreview {
  vcpu: number
  memory_mb: number
  disk_gb: number
  os_type?: string
  os_variant?: string
  /**
   * 每个字段的来源：`ovf`（从包里解析）或 `user`（用户填写）。
   *
   * 用户需要知道「这个 4 核是我自己选的，还是从包里读出来的」——两者对
   * 错误的含义完全不同。
   */
  sources: Record<string, string>
  notes?: string[]
}

export interface ImportView {
  id: number
  node_id: number
  name: string
  source_filename: string
  source_format: ImportFormat
  source_size_bytes: number
  status: ImportStatus
  /** 产出的模板——**导入成功后才会有值**。界面据此给出「去克隆一台」的入口。 */
  template_id?: number
  preview?: ImportPreview
  error?: string
  created_at: string
  finished_at?: string
}

export const importerApi = {
  /** 按文件名推断格式。由**后端**推断而不是信前端传的值。 */
  guessFormat: (filename: string) =>
    get<{ format: ImportFormat | ''; label?: string; bundled?: boolean; supported: boolean }>(
      '/api/v1/imports/format',
      { filename },
    ),

  /** 解析待导入的文件，返回配置预览。**发生在受理之前**（f-2-13）。 */
  parse: (input: {
    node_id: number
    source_filename: string
    source_format: ImportFormat
    source_size_bytes: number
  }) => post<ImportPreview>('/api/v1/imports/parse', input),

  list: (nodeID = 0) =>
    get<{ items: ImportView[] }>('/api/v1/imports', nodeID > 0 ? { node_id: nodeID } : undefined),

  create: (input: {
    node_id: number
    name: string
    source_filename: string
    source_format: ImportFormat
    source_size_bytes: number
    vcpu: number
    memory_mb: number
    disk_gb: number
    os_type?: string
    os_variant?: string
    preview?: ImportPreview
  }) => post<TaskRef>('/api/v1/imports', input),

  remove: (id: number) => del<{ deleted: boolean }>(`/api/v1/imports/${id}`),
}

export const IMPORT_STATUS_LABEL: Record<ImportStatus, string> = {
  pending: '排队中',
  running: '导入中',
  success: '已完成',
  failed: '失败',
}

export const IMPORT_STATUS_TONE: Record<
  ImportStatus,
  'idle' | 'warning' | 'success' | 'danger'
> = {
  pending: 'idle',
  running: 'warning',
  success: 'success',
  failed: 'danger',
}

/**
 * 各格式的说明。
 *
 * OVA 与其它几种**不是同一类东西**：其余都是裸磁盘镜像，而 OVA 里带着一份
 * 机器描述（CPU、内存、磁盘、网络），因此只有它能「先解析预览」。
 */
export const IMPORT_FORMAT_HINT: Record<ImportFormat, string> = {
  qcow2: 'QCOW2 镜像（原生格式，无需转换）',
  raw: 'RAW 裸镜像（会转换为 QCOW2）',
  vmdk: 'VMDK（VMware，会转换为 QCOW2）',
  vhd: 'VHD（Hyper-V，会转换为 QCOW2）',
  vhdx: 'VHDX（Hyper-V，会转换为 QCOW2）',
  img: 'IMG 镜像（会转换为 QCOW2）',
  ova: 'OVA 包（含机器描述，可解析预览）',
}
