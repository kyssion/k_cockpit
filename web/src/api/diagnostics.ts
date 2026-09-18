/**
 * 诊断导出接口（F-9-03）。
 *
 * 包本身**可以直接外发**：敏感设置项（密钥、密码、令牌）在生成的那一刻就
 * 被打码，包里不含明文。这不是"提醒用户自己检查"，而是设计上就做到了
 * ——排障包的用途就是发给别人，任何依赖用户记得先删密码的安排都会失败。
 */
import { get } from './client'

export interface DiagnosticCategory {
  key: 'config' | 'runtime' | 'logs'
  label: string
  description: string
  /** 这一类里可能含敏感内容（地址、账号、来源 IP），导出前需要用户确认。 */
  sensitive: boolean
}

export const diagnosticsApi = {
  categories: () => get<{ items: DiagnosticCategory[] }>('/api/v1/diagnostics/categories'),

  /**
   * 生成并把 URL 交给浏览器下载。
   *
   * 返回 URL 而不是请求函数：接口返回的是**产物字节**（一个 zip），交给
   * 浏览器最省事——它会带上会话 Cookie（同源），并自己处理大文件落盘与
   * 进度。走 fetch 再转 Blob 等于把这些重做一遍，还要把整个包读进内存。
   */
  exportUrl: (keys: string[]) =>
    `/api/v1/diagnostics/export${
      keys.length > 0 ? `?categories=${encodeURIComponent(keys.join(','))}` : ''
    }`,
}
