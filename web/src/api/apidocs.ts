/**
 * 接口清单（F-9-07）。
 *
 * 清单由服务端**从已注册的路由表**生成，而不是在前端或文档里再维护一份：
 * 手写清单的命运只有一个——第一次新增接口时忘了同步，而那时它错得毫无
 * 痕迹（页面照样显示，只是少了一条）。
 */
import { get } from './client'

export interface ApiEndpoint {
  method: string
  path: string
}

export const apiDocsApi = {
  list: () => get<{ endpoints: ApiEndpoint[] }>('/api/v1/api-endpoints'),
}
