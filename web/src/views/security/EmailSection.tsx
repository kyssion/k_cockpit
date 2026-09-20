import { useMutation } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'

import { authApi } from '@/api/auth'
import { ApiError, NetworkError } from '@/api/client'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { StatusBadge } from '@/components/common/StatusBadge'

/**
 * 邮箱绑定（F-1-08）。
 *
 * 邮箱在这里的意义不是"联系方式"，而是**找回密码的唯一通道**：验证器丢了、
 * 邮箱也没绑，账号就真的进不去了。放在安全中心而不是"个人资料"，是因为
 * 它属于"我能不能从事故里恢复"这一类问题。
 *
 * 未配置邮件服务时接口返回"尚未配置"，界面如实显示这句话——新建的部署
 * 本来就没有 SMTP，把"没配"说成"绑定失败"会让人以为自己填错了地址。
 */
export function EmailSection({
  email,
  verified,
  bootstrapSkipped,
  onChanged,
}: {
  email?: string
  verified: boolean
  bootstrapSkipped: boolean
  onChanged: () => void
}) {
  const [value, setValue] = useState('')
  const [code, setCode] = useState('')
  const [sent, setSent] = useState(false)
  const [error, setError] = useState('')

  const send = useMutation({
    mutationFn: () => authApi.sendEmailCode(value.trim()),
    onSuccess: () => {
      setSent(true)
      setError('')
    },
    onError: (e) => setError(describe(e)),
  })

  const confirm = useMutation({
    mutationFn: () => authApi.confirmEmail(value.trim(), code.trim()),
    onSuccess: () => {
      setSent(false)
      setCode('')
      setError('')
      onChanged()
    },
    onError: (e) => setError(describe(e)),
  })

  const complete = useMutation({
    mutationFn: () => authApi.completeBootstrap(),
    onSuccess: () => {
      setError('')
      onChanged()
    },
    onError: (e) => setError(describe(e)),
  })

  function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    if (!value.trim()) {
      setError('请输入邮箱')
      return
    }
    if (!sent) {
      send.mutate()
      return
    }
    if (!code.trim()) {
      setError('请输入邮件中的验证码')
      return
    }
    confirm.mutate()
  }

  return (
    <section className="rounded-card border border-line">
      <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">邮箱</h2>

      <div className="flex flex-col gap-3.5 px-4 py-3.5">
        <div className="flex items-center gap-3 text-base">
          <span className="w-32 text-ink-3">当前邮箱</span>
          <span className="text-ink">{email || '未绑定'}</span>
          <StatusBadge tone={verified ? 'success' : 'idle'}>
            {verified ? '已验证' : '未验证'}
          </StatusBadge>
        </div>

        <p className="text-base text-ink-3">
          绑定邮箱后可用于找回密码。验证码发往待绑定的地址，因此当前没有
          邮箱也能完成绑定。
        </p>

        <form onSubmit={submit} className="flex flex-col gap-3">
          <Input
            label="邮箱"
            type="email"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            disabled={send.isPending || confirm.isPending || sent}
          />
          {sent && (
            <Input
              label="验证码"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="邮件中的 6 位数字"
              inputMode="numeric"
              autoFocus
              disabled={confirm.isPending}
            />
          )}
          <div className="flex justify-end">
            <Button size="sm" type="submit" loading={send.isPending || confirm.isPending}>
              {sent ? '确认绑定' : '发送验证码'}
            </Button>
          </div>
        </form>

        {/*
          跳过引导的管理员补齐邮箱与两步验证之后，可以在这里把标记清掉。
          它唯一的后果是"下次登录还提不提醒"，但留着它会让人以为自己一直
          处于未做安全设置的状态。
        */}
        {bootstrapSkipped && verified && (
          <div className="flex items-center justify-between border-t border-line pt-3">
            <span className="text-base text-ink-3">
              你曾跳过安全初始化引导。补齐后可以清除该标记。
            </span>
            <Button
              size="sm"
              variant="secondary"
              loading={complete.isPending}
              onClick={() => complete.mutate()}
            >
              清除标记
            </Button>
          </div>
        )}

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </section>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
