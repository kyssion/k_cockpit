/**
 * 用户存储空间与文件接口（F-5-03/04/05）。
 *
 * 上传分三步：创建会话（可命中**秒传**）→ 登记分片 → 收尾。
 * 会话把「已经收到了哪些分片」变成可持久化的事实，于是断点续传只是
 * 「问服务端我还要传哪几片」——`missing_chunks` 给的是**序号列表**，
 * 客户端据此只传缺的那几片，而不是重传全部。
 */
import { del, get, post, put } from './client'

export type FileCategory = 'iso' | 'share' | 'disk'

export interface StorageView {
  node_id: number
  enabled: boolean
  rel_root?: string
  initialized_at?: string
  read_only: boolean
}

export interface FileView {
  id: number
  category: FileCategory
  filename: string
  rel_path: string
  size_bytes: number
  sha256?: string
  os_type?: string
  os_variant?: string
  min_disk_gb?: number
  uploaded_at?: string
  created_at: string
}

export interface UploadView {
  upload_id: string
  status: 'pending' | 'completed' | 'expired'
  /** 为 true 表示命中秒传，文件已经就绪、不必再传。 */
  instant: boolean
  missing_chunks?: number[]
  total_chunks: number
  file?: FileView
}

export const userStorageApi = {
  get: (nodeID: number) => get<StorageView>('/api/v1/my-storage', { node_id: nodeID }),

  /** 开通存储空间。幂等——已开通时直接返回，不重复写库。 */
  ensure: (nodeID: number) => post<StorageView>('/api/v1/my-storage', { node_id: nodeID }),

  listFiles: (nodeID: number, category?: FileCategory) =>
    get<{ items: FileView[] }>('/api/v1/my-storage/files', {
      node_id: nodeID,
      ...(category ? { category } : {}),
    }),

  // del 不接受 query，因此 node_id 拼进路径——它决定「删的是哪个节点上的
  // 那份记录」，不能省。
  removeFile: (nodeID: number, id: number) =>
    del<{ deleted: boolean }>(`/api/v1/my-storage/files/${id}?node_id=${nodeID}`),

  createUpload: (input: {
    node_id: number
    category: FileCategory
    rel_dir?: string
    filename: string
    total_size: number
    chunk_size: number
    sha256?: string
  }) => post<UploadView>('/api/v1/my-storage/uploads', input),

  getUpload: (uploadID: string) =>
    get<UploadView>(`/api/v1/my-storage/uploads/${uploadID}`),

  putChunk: (uploadID: string, index: number, sha256?: string) =>
    put<UploadView>(`/api/v1/my-storage/uploads/${uploadID}/chunks`, { index, sha256 }),

  complete: (uploadID: string) =>
    post<FileView>(`/api/v1/my-storage/uploads/${uploadID}/complete`, {}),
}

export const CATEGORY_LABEL: Record<FileCategory, string> = {
  iso: 'ISO 镜像',
  share: '文件共享',
  disk: '虚拟磁盘',
}

/**
 * 列表与上传共用的分片大小：8 MiB。
 *
 * 小文件一次传完，大文件也不会切出上万个分片——上万个分片意味着 bitmap
 * 与上百次请求，而每一次都要一次数据库往返。
 */
export const CHUNK_SIZE = 8 * 1024 * 1024

/** 计算一段内容的 sha256（十六进制）。 */
export async function sha256Hex(data: Blob): Promise<string> {
  const buf = await data.arrayBuffer()
  const digest = await crypto.subtle.digest('SHA-256', buf)
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, '0')).join('')
}

export function formatBytes(n: number): string {
  if (n <= 0) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`
}
