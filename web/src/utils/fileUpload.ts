/**
 * 分片上传到「我的存储」（G-34 抽出的共享实现）。
 *
 * 协议与 MyStoragePage 的上传完全一致：
 *  1. **先算整个文件的 sha256**——服务端有机会命中秒传，一片也不用传；
 *  2. 创建上传会话，按 `missing_chunks` 只补缺失分片（断点续传）；
 *  3. 每片先算分片摘要再传（服务端比对，坏片当场拒绝，不拖到装系统时）；
 *  4. complete 收尾，返回落库后的文件记录。
 *
 * 抽成纯函数而不是留在页面组件里：导入页（F-2-13）需要同一条链路，而
 * 「上传」本身不关心调用方把它放进哪个界面。
 */
import { CHUNK_SIZE, sha256Hex, userStorageApi, type FileCategory, type FileView } from '@/api/userstorage'

export async function uploadFileToStorage(
  nodeID: number,
  category: FileCategory,
  file: File,
  onProgress?: (text: string) => void,
): Promise<FileView> {
  onProgress?.('计算摘要…')
  const digest = await sha256Hex(file)

  onProgress?.('创建上传会话…')
  const session = await userStorageApi.createUpload({
    node_id: nodeID,
    category,
    // 目录按类别分：iso 与 disk 混在一个目录里，用户很难辨认，
    // 也不利于将来按类别做清理策略。
    rel_dir: category,
    filename: file.name,
    total_size: file.size,
    chunk_size: CHUNK_SIZE,
    sha256: digest,
  })

  // 秒传：会话响应里**已带文件记录**（服务端 instantCopy 时登记好了），
  // 再调 complete 反而是对一个已完成会话的多余操作。
  if (session.instant && session.file) {
    onProgress?.('已完成（秒传）')
    return session.file
  }

  const missing = session.missing_chunks ?? range(session.total_chunks)
  for (const [i, idx] of missing.entries()) {
    const start = idx * CHUNK_SIZE
    const chunk = file.slice(start, Math.min(start + CHUNK_SIZE, file.size))
    onProgress?.(`上传中 ${i + 1}/${missing.length} 片`)
    await userStorageApi.putChunk(session.upload_id, idx, chunk, await sha256Hex(chunk))
  }

  onProgress?.('收尾…')
  const done = await userStorageApi.complete(session.upload_id)
  onProgress?.('上传完成')
  return done
}

function range(n: number): number[] {
  return Array.from({ length: n }, (_, i) => i)
}
