/**
 * 接口清单（F-9-07）。
 *
 * 清单由服务端**从已注册的路由表**生成，而不是在前端或文档里再维护一份：
 * 手写清单的命运只有一个——第一次新增接口时忘了同步，而那时它错得毫无
 * 痕迹（页面照样显示，只是少了一条）。
 *
 * 摘要、认证方式与 curl 由服务端补齐（G-50）：摘要按方法与路径形状生成、
 * 关键接口人工校准；认证按注册处的显式数据表判定。
 */
import { get } from './client'

export interface ApiDocParam {
  name: string
  in: 'path' | 'query' | 'body' | string
  note?: string
}

export interface ApiEndpoint {
  method: string
  path: string
  summary: string
  module: string
  /** public = 无需登录；user = 登录即可；admin = 仅管理员。 */
  auth: 'public' | 'user' | 'admin' | string
  params?: ApiDocParam[]
  curl: string
}

export const AUTH_LABEL: Record<string, string> = {
  public: '公开',
  user: '登录用户',
  admin: '管理员',
}

export const apiDocsApi = {
  list: () => get<{ endpoints: ApiEndpoint[] }>('/api/v1/api-endpoints'),
}
