/**
 * 跨资源检索（F-9-08）。
 *
 * 结果由服务端按角色收敛：租户搜不到节点。搜索框看着只是"帮你找东西"，
 * 实际能枚举出整个平台的资源名，因此过滤必须落在服务端。
 */
import { get } from './client'

export type SearchKind = 'vm' | 'node' | 'template'

export interface SearchMatch {
  kind: SearchKind
  id: number
  name: string
  subtitle?: string
  /** 面板内路径，可直接跳转。 */
  link: string
}

export const searchApi = {
  query: (q: string) => get<{ matches: SearchMatch[] }>('/api/v1/search', { q }),
}
