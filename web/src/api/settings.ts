/**
 * 系统设置接口（F-9-01）。
 *
 * 设置项的**元数据由后端下发**（R-004），前端只渲染、不硬编码第二份——
 * 两份清单一旦不同步，用户会看到「界面显示的选项与实际校验规则不符」，
 * 而这类问题只会在提交时才暴露。
 */
import { get, patch, post } from './client'

export type SettingKind = 'string' | 'int' | 'bool' | 'select'
export type ApplyMode = 'immediate' | 'restart'
export type SettingSource = 'env' | 'setting' | 'default'

export interface SettingOption {
  value: string
  label: string
}

export interface SettingItem {
  key: string
  group: string
  label: string
  description?: string
  kind: SettingKind
  default: string
  /** 对应的环境变量名。非空且已设置时该项被锁定。 */
  env_var: string
  apply: ApplyMode
  secret: boolean
  rollbackable: boolean
  options?: SettingOption[]
  min_value?: number
  max_value?: number
  unit?: string

  /** 当前生效值。敏感项为空字符串，见 is_set。 */
  value: string
  is_set?: boolean
  source: SettingSource
  /** 被环境变量锁定：界面必须只读。 */
  locked: boolean
  locked_by?: string
  /** 存在可回滚的前值。 */
  can_rollback: boolean
  updated_at?: string
}

export interface SettingGroup {
  key: string
  label: string
  order: number
}

export type UpdateStatus = 'applied' | 'failed' | 'rolled_back'

export interface UpdateResult {
  key: string
  status: UpdateStatus
  message?: string
}

export interface SettingsList {
  groups: SettingGroup[]
  items: SettingItem[]
}

export const settingsApi = {
  list: () => get<SettingsList>('/api/v1/settings'),

  update: (values: Record<string, string>) =>
    patch<{ results: UpdateResult[]; applied: number; total: number }>('/api/v1/settings', { values }),

  rollback: (key: string) => post<SettingItem>('/api/v1/settings/rollback', { key }),

  /**
   * 发送测试邮件（F-1-08）。
   *
   * 这是验证 SMTP 配置是否正确的**唯一**手段：配置写错的表现是"什么都没
   * 发生"，直到某天有人找回密码才发现——那时已经晚了。
   */
  testMail: (to: string) => post<{ sent: boolean }>('/api/v1/settings/mail/test', { to }),
}

export const SOURCE_LABEL: Record<SettingSource, string> = {
  env: '环境变量',
  setting: '面板设置',
  default: '默认值',
}

export const UPDATE_STATUS_LABEL: Record<UpdateStatus, string> = {
  applied: '已生效',
  failed: '失败',
  rolled_back: '已回滚',
}
