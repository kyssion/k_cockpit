import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { useLocation, useNavigate } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { authApi, type LoginResult } from '@/api/auth'
import { Button } from '@/components/common/Button'
import { Icon } from '@/components/common/Icon'
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
    <div className="relative flex min-h-full overflow-hidden bg-base">
      {/* 背景光斑：纯装饰，不承载内容也不响应指针；色相从令牌派生，随主题走 */}
      <div aria-hidden className="pointer-events-none absolute inset-0">
        <div className="absolute -top-24 left-[6%] h-80 w-80 rounded-full bg-brand/10 blur-3xl" />
        <div className="absolute -bottom-28 right-[8%] h-96 w-96 rounded-full bg-info/10 blur-3xl" />
        <div className="absolute left-[38%] top-[24%] h-64 w-64 rounded-full bg-success/8 blur-3xl" />
      </div>

      {/* 品牌面板：宽屏专属——窄屏上登录表单本身就是全部，不再挤营销内容 */}
      <aside className="relative hidden flex-1 flex-col justify-center px-14 lg:flex xl:px-20">
        <div className="max-w-[520px]">
          <div className="flex items-center gap-2.5">
            <span className="flex size-9 items-center justify-center rounded-card bg-brand/12 text-brand">
              <Icon name="vm" className="h-5 w-5" />
            </span>
            <div>
              <p className="text-md font-semibold text-ink">K Cockpit</p>
              <p className="text-xs text-ink-3">KVM 虚拟化管理控制台</p>
            </div>
          </div>
          <h2 className="mt-8 text-2xl font-semibold leading-snug text-ink">
            散落的 KVM 宿主机，
            <br />
            <span className="bg-linear-to-r from-brand to-info bg-clip-text text-transparent">
              一块控制台管完
            </span>
          </h2>
          <p className="mt-3 max-w-[440px] text-base leading-relaxed text-ink-2">
            虚拟机、镜像模板、VPC 网络、存储与配额，收进同一个面板；节点代理反向连接，宿主机无需开放入站端口。
          </p>
          <dl className="mt-8 grid grid-cols-2 gap-x-8 gap-y-5">
            {FEATURES.map((f) => (
              <div key={f.title} className="flex gap-3">
                <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-control bg-brand/10 text-brand">
                  <Icon name={f.icon} className="h-4 w-4" />
                </span>
                <div>
                  <dt className="text-base font-medium text-ink">{f.title}</dt>
                  <dd className="mt-0.5 text-sm leading-relaxed text-ink-3">{f.desc}</dd>
                </div>
              </div>
            ))}
          </dl>
        </div>

        {/* 示例卡：示意产品形态。数据是装饰（aria-hidden、不可交互），
            绿点呼吸正好向访客预演「活的面板」长什么样。xl 以下不显示：
            面板宽度不足以让它们避开文字。 */}
        <div
          aria-hidden
          className="absolute right-[4%] top-[13%] hidden w-60 animate-float rounded-card border border-line bg-surface/90 p-4 shadow-2 xl:block"
        >
          <div className="flex items-center gap-2">
            <span aria-hidden className="size-1.5 animate-breathe rounded-pill bg-success" />
            <span className="text-sm font-medium text-ink">web-prod-01</span>
            <Icon name="vm" className="ml-auto h-4 w-4 text-ink-3" />
          </div>
          <p className="kc-mono mt-2 text-xs text-ink-3">4C / 8G · 运行中 · 12 天</p>
        </div>
        <div
          aria-hidden
          className="absolute bottom-[12%] right-[16%] hidden w-52 animate-float-slow rounded-card border border-line bg-surface/90 p-4 shadow-2 xl:block"
        >
          <div className="flex items-center gap-2">
            <span aria-hidden className="size-1.5 rounded-pill bg-idle" />
            <span className="text-sm font-medium text-ink">db-main</span>
            <Icon name="storage" className="ml-auto h-4 w-4 text-ink-3" />
          </div>
          <p className="kc-mono mt-2 text-xs text-ink-3">8C / 16G · 已关机</p>
        </div>
      </aside>

      {/* 表单列：多阶段登录都在这里切换 */}
      <div className="relative flex flex-1 items-center justify-center px-4 py-10">
        <div className="w-full max-w-[360px]">
        <header className="mb-6 text-center">
          <h1 className="text-xl font-semibold text-ink">K Cockpit</h1>
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

            <Button type="submit" variant="hero" loading={login.isPending} className="mt-1 w-full">
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
    </div>
  )
}

/** 品牌面板的能力点：与 PRD 的域划分对应，只说产品形态，不复述功能页文案。 */
const FEATURES = [
  { icon: 'template', title: '模板秒级开机', desc: '系统盘沉淀为模板，克隆即得可用机器' },
  { icon: 'network', title: 'VPC 网络与安全组', desc: '交换机、ACL、公网 IP 一站式治理' },
  { icon: 'quota', title: '多租户与配额', desc: '三维配额与超限处置，防止单租户吃满' },
  { icon: 'task', title: '任务流与实时监控', desc: '异步任务全程可追踪，状态实时推送' },
] as const

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
