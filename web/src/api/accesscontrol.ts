/**
 * 公网访问与开发模式开关接口（F-10-06）。
 *
 * 它**不是普通设置项**：关掉公网访问时若调用方自己就在公网上，那一刻他的
 * 连接就断了——因此需要显式 `confirm`。不确认时服务端**不执行也不报错**，
 * 而是返回现状（那是一个岔路口，不是一次失败）。
 */
import { get, put } from './client'

export interface AccessView {
  public_enabled: boolean
  dev_mode: boolean
  /** 为 true 时界面上的开关应禁用并说明原因：环境变量优先于面板设置。 */
  env_locked: boolean
  /** 当前这个请求是否来自公网——它决定关掉公网访问会不会立刻切断本人。 */
  caller_is_public: boolean
  caller_ip: string
  /** 当前值来自哪里（环境变量 / 面板）。 */
  effected_by: string
}

export const accessControlApi = {
  get: () => get<AccessView>('/api/v1/settings/access'),

  set: (req: { public_enabled?: boolean; dev_mode?: boolean; confirm?: boolean }) =>
    put<AccessView>('/api/v1/settings/access', req),
}
