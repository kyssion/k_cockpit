/**
 * 版本与构建信息接口（F-9-05）。
 *
 * 全部字段来自**二进制的构建信息**，没有任何一个是手写的：手写的清单在第一次
 * `go mod tidy` 之后就漂移了，而它恰恰是排查「这个版本用的是哪个库」时要看的
 * 东西。一份漂移的清单比没有更糟——它给出一个看起来很具体的错误答案。
 */
import { get } from './client'

export interface Dependency {
  path: string
  version: string
}

export interface VersionInfo {
  /** 未由构建注入时为空——如实留空，而不是填一个假的版本号。 */
  panel_version: string
  go_version: string
  platform: string
  /** VCS 提交号。 */
  revision?: string
  /** 工作区有未提交改动——这样的构建无法用提交号复现。 */
  dirty?: boolean
  build_time?: string
  /** 为 false 表示二进制里没有构建信息（如 go run 启动）。 */
  build_available: boolean
  dependencies: Dependency[]
  /** 被 go.mod 的 replace 覆盖的模块——**实际跑的不是官方版本**。 */
  replacements?: Dependency[]
}

export const versionApi = {
  get: () => get<VersionInfo>('/api/v1/version'),
}
