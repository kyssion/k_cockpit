import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Link, useNavigate } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { setupApi } from '@/api/setup'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { useSessionStore } from '@/stores/session'

/** 与后端 MinPasswordLength 保持一致；两处不一致会让用户白填一次。 */
const MIN_PASSWORD_LENGTH = 12

/**
 * 首次初始化页：创建首个管理员（ADR-0008）。
 *
 * 与登录页的区别在于——它需要一个**只能从服务端日志获取**的一次性令牌。
 * 这不是多余的步骤：没有它，任何能访问端口的人都可以抢先把自己设成管理员。
 */
export function SetupPage() {
  const navigate = useNavigate()
  const setAuthenticated = useSessionStore((s) => s.setAuthenticated)

  const [token, setToken] = useState('')
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [formError, setFormError] = useState('')
  // 协议确认。初始化是这个面板的**第一次使用**，因此在这一步确认最合适——
  // 之后每个账号都是管理员授权的产物，不需要各自再确认一次。
  const [agreed, setAgreed] = useState(false)

  const create = useMutation({
    mutationFn: () => setupApi.createAdmin(token, username, password),
    onSuccess: (result) => {
      if (result.auto_login) {
        setAuthenticated(result.user)
        navigate('/', { replace: true })
        return
      }
      // 账号已创建但自动登录失败：初始化本身是成功的，引导去登录即可。
      navigate('/login', { replace: true })
    },
    onError: (error) => {
      if (error instanceof ApiError || error instanceof NetworkError) {
        setFormError(error.message)
      } else {
        setFormError('初始化失败，请稍后重试')
      }
    },
  })

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setFormError('')

    if (!token || !username || !password) {
      setFormError('请填写全部字段')
      return
    }
    if (password.length < MIN_PASSWORD_LENGTH) {
      setFormError(`密码长度不得少于 ${MIN_PASSWORD_LENGTH} 个字符`)
      return
    }
    if (password !== confirm) {
      setFormError('两次输入的密码不一致')
      return
    }
    if (!agreed) {
      setFormError('请先阅读并同意《用户协议》与《公测协议》')
      return
    }
    create.mutate()
  }

  return (
    <div className="flex min-h-full items-center justify-center bg-base px-4 py-10">
      <div className="w-full max-w-[460px]">
        <header className="mb-6 text-center">
          <h1 className="text-lg font-semibold text-ink">初始化 K Cockpit</h1>
          <p className="mt-1 text-sm text-ink-3">创建首个管理员账号以完成部署</p>
        </header>

        <div className="mb-4 rounded-card border border-line bg-sunken p-4">
          <p className="text-sm font-medium text-ink-2">初始化令牌从哪里来？</p>
          <p className="mt-1.5 text-sm text-ink-3">
            服务首次启动时会在控制台日志中打印一次性令牌。
            容器部署可执行 <code className="kc-mono text-xs">docker logs &lt;容器名&gt;</code>；
            systemd 部署执行 <code className="kc-mono text-xs">journalctl -u k-cockpit</code>。
          </p>
        </div>

        <form
          onSubmit={handleSubmit}
          className="flex flex-col gap-4 rounded-card border border-line bg-surface p-6 shadow-1"
        >
          <Input
            label="初始化令牌"
            value={token}
            onChange={(e) => setToken(e.target.value.trim())}
            disabled={create.isPending}
            hint="仅在本次服务运行期间有效，创建成功后立即失效"
            className="kc-mono"
            autoFocus
          />

          <Input
            label="管理员用户名"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            disabled={create.isPending}
            hint="3-64 位字母、数字、下划线或连字符"
            autoComplete="username"
          />

          <Input
            label="密码"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            disabled={create.isPending}
            hint={`至少 ${MIN_PASSWORD_LENGTH} 个字符，且不能包含用户名`}
            autoComplete="new-password"
          />

          <Input
            label="确认密码"
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            disabled={create.isPending}
            autoComplete="new-password"
          />

          {formError && (
            <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
              {formError}
            </p>
          )}

          <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={agreed}
              onChange={(e) => setAgreed(e.target.checked)}
            />
            <span>
              我已阅读并同意
              <Link to="/about" target="_blank" className="mx-1 text-brand hover:underline">
                《用户协议》
              </Link>
              与
              <Link to="/about" target="_blank" className="mx-1 text-brand hover:underline">
                《公测协议》
              </Link>
            </span>
          </label>

          <Button
            type="submit"
            loading={create.isPending}
            disabled={!agreed}
            className="mt-1 w-full"
          >
            {create.isPending ? '创建中…' : '创建管理员并进入'}
          </Button>
        </form>

        <p className="mt-4 text-center text-sm text-ink-3">
          该令牌等同于初始化权限，请勿写入公开渠道。
        </p>
      </div>
    </div>
  )
}
