/**
 * 账号自管理接口（F-1-03 / F-10-01）。
 *
 * 三件事都要提供**当前密码**。这不是形式主义：拿到一个未锁屏的浏览器、或
 * 偷到一个会话令牌，就能改掉密码并把主人锁在外面，而那时主人连"怎么进不去
 * 了"都查不出来——会话是合法的，日志里看不出异常。
 *
 * **刻意不要求二次验证**（TOTP）：用户重新生成恢复码的常见原因恰恰是
 * "手机丢了、恢复码快用完了"，那时他刚用一个恢复码登进来。要求 TOTP
 * 就是要求他拿出已经丢了的东西。
 */
import { post, put } from './client'

export const accountApi = {
  /**
   * 改密码。**会退出其它设备上的全部登录**——改密码最常见的动机就是
   * "我怀疑账号被盗"，而旧会话如果继续有效，这个动作就白做了。
   */
  changePassword: (currentPassword: string, newPassword: string) =>
    put<{ revoked_sessions: number; notice: string }>('/api/v1/auth/password', {
      current_password: currentPassword,
      new_password: newPassword,
    }),

  changeUsername: (currentPassword: string, username: string) =>
    put<{ username: string }>('/api/v1/auth/username', {
      current_password: currentPassword,
      username,
    }),

  /**
   * 重新生成恢复码。**之前的所有恢复码会全部作废**——发一批新的"万能钥匙"
   * 而旧的还能用，等于把可用入口翻了一倍，而用户以为只换了新的那张纸。
   */
  regenerateRecoveryCodes: (currentPassword: string) =>
    post<{ recovery_codes: string[]; notice: string }>('/api/v1/auth/recovery-codes', {
      current_password: currentPassword,
    }),
}
