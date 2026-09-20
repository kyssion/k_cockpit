import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { authApi } from '@/api/auth'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'

/**
 * 找回密码（F-1-08）：邮箱 → 验证码 → 重置票据 → 新密码。
 *
 * 三步而不是两步的理由写在后端，这里关注前端的表现：
 *
 *  1. **邮箱不存在也提示"已发送"**。否则这个页面就是"哪些邮箱注册过本面板"
 *     的查询入口。代价是输错邮箱的人收不到任何提示——只能由"收不到邮件"
 *     承担这个反馈，界面上如实说明这一点。
 *  2. 重置票据只存在于内存：它由验证码换来，不属于浏览器里任何持久状态。
 */
export function ForgotPasswordPage() {
  const navigate = useNavigate()

  const [step, setStep] = useState<'email' | 'code' | 'password'>('email')
  const [email, setEmail] = useState('')
  const [code, setCode] = useState('')
  const [resetToken, setResetToken] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')

  const send = useMutation({
    mutationFn: () => authApi.requestPasswordReset(email.trim()),
    onSuccess: () => {
      setError('')
      setStep('code')
    },
    onError: (e) => setError(describe(e)),
  })

  const verify = useMutation({
    mutationFn: () => authApi.verifyResetCode(email.trim(), code.trim()),
    onSuccess: (res) => {
      setResetToken(res.reset_token)
      setError('')
      setStep('password')
    },
    onError: (e) => setError(describe(e)),
  })

  const reset = useMutation({
    mutationFn: () => authApi.resetPassword(resetToken, next),
    onSuccess: () => navigate('/login', { replace: true }),
    onError: (e) => setError(describe(e)),
  })

  function submit(event: FormEvent) {
    event.preventDefault()
    setError('')

    if (step === 'email') {
      if (!email.trim()) {
        setError('请输入邮箱')
        return
      }
      send.mutate()
      return
    }
    if (step === 'code') {
      if (!code.trim()) {
        setError('请输入邮件中的验证码')
        return
      }
      verify.mutate()
      return
    }
    if (next.length < 12) {
      setError('密码长度不得少于 12 个字符')
      return
    }
    if (next !== confirm) {
      setError('两次输入的密码不一致')
      return
    }
    reset.mutate()
  }

  return (
    <div className="flex min-h-full items-center justify-center bg-base px-4 py-10">
      <form
        onSubmit={submit}
        className="flex w-full max-w-[360px] flex-col gap-4 rounded-card border border-line bg-surface p-6 shadow-1"
      >
        <header>
          <h1 className="text-base font-semibold text-ink">找回密码</h1>
          <p className="mt-1.5 text-sm text-ink-3">
            {step === 'email' && '输入账号绑定的邮箱，我们会发送一封验证码邮件。'}
            {step === 'code' && '验证码已发送，请查收邮件（可能在垃圾箱里）。'}
            {step === 'password' && '验证通过，请设置新密码。'}
          </p>
        </header>

        {step === 'email' && (
          <Input
            label="邮箱"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoFocus
            disabled={send.isPending}
          />
        )}

        {step === 'code' && (
          <Input
            label="验证码"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder="6 位数字"
            inputMode="numeric"
            autoComplete="one-time-code"
            autoFocus
            disabled={verify.isPending}
          />
        )}

        {step === 'password' && (
          <>
            <Input
              label="新密码"
              type="password"
              autoComplete="new-password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
              autoFocus
              disabled={reset.isPending}
            />
            <Input
              label="确认新密码"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              disabled={reset.isPending}
            />
          </>
        )}

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}

        {/* 邮箱不存在时后端同样返回成功，界面如实说明——否则用户会以为
            自己输对了，一直等一封不会来的邮件。 */}
        {step !== 'email' && (
          <p className="text-sm text-ink-3">
            没收到？请确认邮箱地址是否正确（该邮箱必须已通过绑定验证）。
          </p>
        )}

        <Button
          type="submit"
          className="mt-1 w-full"
          loading={send.isPending || verify.isPending || reset.isPending}
        >
          {step === 'email' ? '发送验证码' : step === 'code' ? '验证' : '设置新密码'}
        </Button>

        <div className="text-center">
          <button
            type="button"
            className="text-sm text-ink-3 underline decoration-dotted hover:text-ink-2"
            onClick={() => navigate('/login')}
          >
            想起密码了？返回登录
          </button>
        </div>
      </form>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
