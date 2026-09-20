import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'

import { authApi, type LoginResult } from '@/api/auth'
import { ApiError, NetworkError } from '@/api/client'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'

/**
 * 安全初始化引导（F-1-08）。
 *
 * 只有管理员会走到这里：他能接管宿主机上的全部虚拟机，邮箱与两步验证对
 * 他不是"可选的安全加分项"。
 *
 * 两件事都做成**可分别完成**而不是一个必须一次填完的表单：管理员未必手边
 * 就有可用的邮箱服务，逼他在绑定邮箱失败时连 2FA 都做不了，只会诱导他
 * 直接点跳过。
 *
 * 允许跳过也是刻意的：自建自用的面板里，"没有邮箱"是真实存在的情况。
 * 不给出口的话，他唯一的选择是去改数据库——那比跳过危险得多。
 */
export function BootstrapPanel({
  token,
  onDone,
  onCancel,
}: {
  token: string
  onDone: (result: LoginResult) => boolean
  onCancel: (message?: string) => void
}) {
  const [otpauthURI, setOtpauthURI] = useState('')
  const [totpCode, setTotpCode] = useState('')
  const [email, setEmail] = useState('')
  const [emailCode, setEmailCode] = useState('')
  const [sent, setSent] = useState(false)
  const [boundEmail, setBoundEmail] = useState(false)
  const [recoveryCodes, setRecoveryCodes] = useState<string[]>([])
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  // --- 两步验证 ---
  const begin = useMutation({
    mutationFn: () => authApi.bootstrapBeginTOTP(token),
    onSuccess: (res) => {
      setOtpauthURI(res.otpauth_uri)
      setError('')
    },
    onError: (e) => setError(describe(e)),
  })

  const confirm = useMutation({
    mutationFn: () => authApi.bootstrapConfirmTOTP(token, totpCode.trim()),
    onSuccess: (res) => {
      setRecoveryCodes(res.recovery_codes)
      // 绑定会让既有会话失效（安全信息变更），中间态令牌也随之作废：
      // 此时只能回到登录页重新走一遍——而这一次登录会停在二次验证，
      // 用户正好用刚绑好的验证器进去。
      setOtpauthURI('')
      setNotice('两步验证已启用。安全设置变更会让当前登录流程失效，请用新绑定的验证器重新登录。')
    },
    onError: (e) => setError(describe(e)),
  })

  // --- 邮箱 ---
  const sendCode = useMutation({
    mutationFn: () => authApi.bootstrapSendEmailCode(token, email.trim()),
    onSuccess: () => {
      setSent(true)
      setError('')
    },
    onError: (e) => setError(describe(e)),
  })

  const confirmEmail = useMutation({
    mutationFn: () => authApi.bootstrapConfirmEmail(token, email.trim(), emailCode.trim()),
    onSuccess: () => {
      setBoundEmail(true)
      setError('')
    },
    onError: (e) => setError(describe(e)),
  })

  const skip = useMutation({
    mutationFn: () => authApi.skipBootstrap(token),
    onSuccess: (res) => {
      if (!onDone(res)) setError('登录未完成，请重新登录')
    },
    onError: (e) => setError(describe(e)),
  })

  if (recoveryCodes.length > 0) {
    return (
      <Section title="保存恢复码">
        <p className="text-sm text-ink-2">
          下面是一次性恢复码：验证器不可用时，每一个都能用一次。请现在保存
          起来——这一页关掉之后不会再显示。
        </p>
        <ul className="kc-mono grid grid-cols-2 gap-1.5 rounded-control bg-sunken px-3 py-2.5 text-base text-ink">
          {recoveryCodes.map((c) => (
            <li key={c}>{c}</li>
          ))}
        </ul>
        {notice && <p className="text-sm text-ink-3">{notice}</p>}
        <div className="flex justify-end">
          <Button size="sm" onClick={() => onCancel(notice)}>
            我已保存，重新登录
          </Button>
        </div>
      </Section>
    )
  }

  return (
    <Section title="安全初始化" hint="你是管理员，建议先绑定邮箱并启用两步验证，再进入系统。">
      {/* 邮箱 */}
      <div className="flex flex-col gap-3">
        <h3 className="text-sm font-medium text-ink-2">绑定邮箱</h3>
        {boundEmail ? (
          <p className="text-sm text-success">邮箱已绑定</p>
        ) : (
          <form
            className="flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault()
              setError('')
              if (!email.trim()) {
                setError('请输入邮箱')
                return
              }
              if (!sent) {
                sendCode.mutate()
                return
              }
              if (!emailCode.trim()) {
                setError('请输入邮件中的验证码')
                return
              }
              confirmEmail.mutate()
            }}
          >
            <Input
              label="邮箱"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              disabled={sendCode.isPending || confirmEmail.isPending || sent}
            />
            {sent && (
              <Input
                label="验证码"
                value={emailCode}
                onChange={(e) => setEmailCode(e.target.value)}
                placeholder="邮件中的 6 位数字"
                inputMode="numeric"
                disabled={confirmEmail.isPending}
              />
            )}
            <div className="flex justify-end">
              <Button size="sm" type="submit" loading={sendCode.isPending || confirmEmail.isPending}>
                {sent ? '确认绑定' : '发送验证码'}
              </Button>
            </div>
          </form>
        )}
      </div>

      {/* 两步验证 */}
      <div className="flex flex-col gap-3 border-t border-line pt-3">
        <h3 className="text-sm font-medium text-ink-2">启用两步验证</h3>
        {!otpauthURI ? (
          <div className="flex justify-end">
            <Button size="sm" variant="secondary" loading={begin.isPending} onClick={() => begin.mutate()}>
              开始绑定
            </Button>
          </div>
        ) : (
          <form
            className="flex flex-col gap-3"
            onSubmit={(e) => {
              e.preventDefault()
              setError('')
              if (!totpCode.trim()) {
                setError('请输入 6 位动态码')
                return
              }
              confirm.mutate()
            }}
          >
            <p className="text-sm text-ink-3">
              在验证器 App 中添加账户，输入下面的密钥：
            </p>
            <code className="kc-mono select-all rounded-control bg-sunken px-3 py-2.5 text-base tracking-wider text-ink">
              {secretOf(otpauthURI)}
            </code>
            <Input
              label="动态码"
              value={totpCode}
              onChange={(e) => setTotpCode(e.target.value)}
              placeholder="6 位数字"
              inputMode="numeric"
              autoComplete="one-time-code"
              disabled={confirm.isPending}
            />
            <div className="flex justify-end gap-2">
              <Button variant="secondary" size="sm" type="button" onClick={() => setOtpauthURI('')}>
                取消
              </Button>
              <Button size="sm" type="submit" loading={confirm.isPending}>
                确认绑定
              </Button>
            </div>
          </form>
        )}
      </div>

      {error && <Alert text={error} />}
      {notice && <p className="text-sm text-ink-3">{notice}</p>}

      <div className="flex items-center justify-between border-t border-line pt-3">
        <button
          type="button"
          className="text-sm text-ink-3 underline decoration-dotted hover:text-ink-2"
          onClick={() => skip.mutate()}
          disabled={skip.isPending}
        >
          跳过，直接进入
        </button>
        <Button variant="secondary" size="sm" onClick={() => onCancel()}>
          返回
        </Button>
      </div>
    </Section>
  )
}

function Section({
  title,
  hint,
  children,
}: {
  title: string
  hint?: string
  children: React.ReactNode
}) {
  return (
    <section className="flex flex-col gap-4 rounded-card border border-line bg-surface p-6 shadow-1">
      <div>
        <h2 className="text-base font-semibold text-ink">{title}</h2>
        {hint && <p className="mt-1.5 text-sm text-ink-3">{hint}</p>}
      </div>
      {children}
    </section>
  )
}

function Alert({ text }: { text: string }) {
  return (
    <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
      {text}
    </p>
  )
}

function secretOf(uri: string): string {
  try {
    return new URL(uri).searchParams.get('secret') ?? ''
  } catch {
    return ''
  }
}

function describe(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
