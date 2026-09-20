import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { useLocation, useNavigate } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { authApi, type LoginResult } from '@/api/auth'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { useSessionStore } from '@/stores/session'
import { BootstrapPanel } from './BootstrapPanel'

/**
 * 登录页（F-1-08：登录是一个**多阶段**流程）。
 *
 * 两点与后端约定一致：
 *   - 失败时后端返回统一文案，前端不做「用户名不存在 / 密码错误」的区分
 *     （区分会帮助攻击者枚举用户名）；
 *   - 成功后不保存任何令牌——访问令牌在 HttpOnly Cookie 中，由浏览器管理。
 *
 * 至于阶段令牌：它由后端下发、只存在于本页的内存里，**刻意不写
 * localStorage**。它离完整权限只差一步，落盘等于把半成品凭据留在浏览器里；
 * 代价是刷新页面就得重新登录，而那正是这件事应有的样子。
 */
export function LoginPage() {
  const navigate = useNavigate()
  const location = useLocation()
  const setAuthenticated = useSessionStore((s) => s.setAuthenticated)

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [formError, setFormError] = useState('')
  // 阶段令牌只留在内存里：见文件头注释。
  const [phase, setPhase] = useState<Phase>({ kind: 'credentials' })

  const login = useMutation({
    mutationFn: () => authApi.login(username, password),
    onSuccess: (result) => {
      setFormError('')
      if (result.stage === 'ok') {
        enter(result)
        return
      }
      const token = result.login_token ?? ''
      switch (result.stage) {
        case 'login_verify':
          setPhase({ kind: 'verify', token })
          break
        case 'force_password_change':
          setPhase({ kind: 'change', token })
          break
        case 'bootstrap_security':
          setPhase({ kind: 'bootstrap', token })
          break
      }
    },
    onError: (error) => setFormError(describe(error)),
  })

  /** 进入系统。from 由 401 拦截时记录，否则回工作台。 */
  function enter(result: LoginResult): boolean {
    if (!result.user) return false
    setAuthenticated(result.user)
    const from = (location.state as { from?: string } | null)?.from
    navigate(from ?? '/', { replace: true })
    return true
  }

  function restart(message?: string) {
    setPhase({ kind: 'credentials' })
    setPassword('')
    setFormError(message ?? '')
  }

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setFormError('')
    if (!username || !password) {
      setFormError('请输入用户名与密码')
      return
    }
    login.mutate()
  }

  return (
    <div className="flex min-h-full items-center justify-center bg-base px-4 py-10">
      <div className="w-full max-w-[360px]">
        <header className="mb-6 text-center">
          <h1 className="text-lg font-semibold text-ink">K Cockpit</h1>
          <p className="mt-1 text-sm text-ink-3">虚拟机管理面板</p>
        </header>

        {phase.kind === 'credentials' && (
          <form
            onSubmit={handleSubmit}
            className="flex flex-col gap-4 rounded-card border border-line bg-surface p-6 shadow-1"
          >
            <Input
              label="用户名"
              name="username"
              autoComplete="username"
              autoFocus
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              disabled={login.isPending}
            />

            <Input
              label="密码"
              name="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              disabled={login.isPending}
            />

            {formError && (
              <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
                {formError}
              </p>
            )}

            <Button type="submit" loading={login.isPending} className="mt-1 w-full">
              {login.isPending ? '登录中…' : '登录'}
            </Button>

            <div className="text-center">
              <button
                type="button"
                className="text-sm text-ink-3 underline decoration-dotted hover:text-ink-2"
                onClick={() => navigate('/forgot')}
              >
                忘记密码？
              </button>
            </div>
          </form>
        )}

        {phase.kind === 'verify' && (
          <VerifyPanel
            token={phase.token}
            onDone={enter}
            onCancel={() => restart()}
          />
        )}

        {phase.kind === 'change' && (
          <ChangePasswordPanel
            token={phase.token}
            onDone={enter}
            onCancel={() => restart()}
          />
        )}

        {phase.kind === 'bootstrap' && (
          <BootstrapPanel
            token={phase.token}
            onDone={enter}
            onCancel={() => restart()}
          />
        )}
      </div>
    </div>
  )
}

/** 登录阶段。credentials 是输入用户名密码，其余三种都需要继续走完。 */
type Phase =
  | { kind: 'credentials' }
  | { kind: 'verify'; token: string }
  | { kind: 'change'; token: string }
  | { kind: 'bootstrap'; token: string }

interface StagePanelProps {
  token: string
  onDone: (result: LoginResult) => boolean
  onCancel: (message?: string) => void
}

/** 二次验证：动态码或恢复码都收，用户此刻可能正是丢了手机才用恢复码。 */
function VerifyPanel({ token, onDone, onCancel }: StagePanelProps) {
  const [code, setCode] = useState('')
  const [error, setError] = useState('')

  const submit = useMutation({
    mutationFn: () => authApi.verifyLogin(token, code.trim()),
    onSuccess: (res) => {
      if (!onDone(res)) setError('登录未完成，请重新登录')
    },
    onError: (e) => setError(describe(e)),
  })

  return (
    <Panel title="二次验证" hint="该账号已启用两步验证，请输入验证器中的动态码或恢复码。">
      <form
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          setError('')
          if (!code.trim()) {
            setError('请输入验证码')
            return
          }
          submit.mutate()
        }}
      >
        <Input
          label="验证码"
          value={code}
          onChange={(e) => setCode(e.target.value)}
          placeholder="6 位动态码或恢复码"
          autoComplete="one-time-code"
          autoFocus
          disabled={submit.isPending}
        />
        {error && <Alert text={error} />}
        <div className="flex justify-between gap-2">
          <Button variant="secondary" size="sm" type="button" onClick={() => onCancel()}>
            返回
          </Button>
          <Button size="sm" type="submit" loading={submit.isPending}>
            验证并登录
          </Button>
        </div>
      </form>
    </Panel>
  )
}

/** 强制改密：初始密码是别人设的，这一步不能跳过。 */
function ChangePasswordPanel({ token, onDone, onCancel }: StagePanelProps) {
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')

  const submit = useMutation({
    mutationFn: () => authApi.forceChangePassword(token, next),
    onSuccess: (res) => {
      if (!onDone(res)) setError('登录未完成，请重新登录')
    },
    onError: (e) => setError(describe(e)),
  })

  return (
    <Panel title="修改初始密码" hint="该账号的初始密码由他人设置，请先设置一个只属于你自己的密码。">
      <form
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          setError('')
          if (next.length < 12) {
            setError('密码长度不得少于 12 个字符')
            return
          }
          if (next !== confirm) {
            setError('两次输入的密码不一致')
            return
          }
          submit.mutate()
        }}
      >
        <Input
          label="新密码"
          type="password"
          autoComplete="new-password"
          value={next}
          onChange={(e) => setNext(e.target.value)}
          autoFocus
          disabled={submit.isPending}
        />
        <Input
          label="确认新密码"
          type="password"
          autoComplete="new-password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
          disabled={submit.isPending}
        />
        {error && <Alert text={error} />}
        <div className="flex justify-between gap-2">
          <Button variant="secondary" size="sm" type="button" onClick={() => onCancel()}>
            返回
          </Button>
          <Button size="sm" type="submit" loading={submit.isPending}>
            修改并登录
          </Button>
        </div>
      </form>
    </Panel>
  )
}

function Panel({
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

function describe(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
