import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { useLocation, useNavigate } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { authApi } from '@/api/auth'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { useSessionStore } from '@/stores/session'

/**
 * 登录页。
 *
 * 两点与后端约定一致：
 *   - 失败时后端返回**统一文案**，前端不做「用户名不存在 / 密码错误」的区分
 *     （区分会帮助攻击者枚举用户名）；
 *   - 成功后不保存任何令牌——令牌在 HttpOnly Cookie 中，由浏览器管理。
 */
export function LoginPage() {
  const navigate = useNavigate()
  const location = useLocation()
  const setAuthenticated = useSessionStore((s) => s.setAuthenticated)

  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [formError, setFormError] = useState('')

  const login = useMutation({
    mutationFn: () => authApi.login(username, password),
    onSuccess: (result) => {
      setAuthenticated(result.user)
      // 回跳：401 拦截时记录的原始地址优先，否则进工作台。
      const from = (location.state as { from?: string } | null)?.from
      navigate(from ?? '/', { replace: true })
    },
    onError: (error) => {
      if (error instanceof ApiError) {
        setFormError(error.message)
      } else if (error instanceof NetworkError) {
        setFormError(error.message)
      } else {
        setFormError('登录失败，请稍后重试')
      }
    },
  })

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
        </form>
      </div>
    </div>
  )
}
