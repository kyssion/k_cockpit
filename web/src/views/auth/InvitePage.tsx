/**
 * InvitePage 公开接受邀请（F-1-10）。
 *
 * 它是**未登录**就能访问的页面：拿到链接的人必须先看到"这条邀请是给谁的、什么
 * 角色"，然后才能决定要不要注册。没有这一步，用户点开一个链接会直接落到一
 * 个要求填用户名密码的空表单，而那正是钓鱼链接长什么样。
 *
 * 顺序上刻意**先预览后注册**：预览失败（过期 / 已使用 / 被撤销）时表单根本不
 * 出现 —— 让用户填完十几个字段再告诉他链接失效是很糟的体验，而且会让用户以
 * 为自己填错了。
 */
import { useMutation, useQuery } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Link, useParams } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { inviteApi } from '@/api/invite'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { StatusBadge } from '@/components/common/StatusBadge'
import { INVITE_STATUS_LABEL, INVITE_STATUS_TONE } from '@/api/invite'
import { relativeTime } from '@/utils/format'

export function InvitePage() {
  const { token } = useParams<{ token: string }>()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [done, setDone] = useState(false)

  const preview = useQuery({
    queryKey: ['invite', token],
    queryFn: () => inviteApi.preview(token ?? ''),
    enabled: !!token,
    // 失败不重试：链接无效就是无效，重试三次只会让用户多等三秒才看到"已过期"。
    retry: false,
  })

  const accept = useMutation({
    mutationFn: () =>
      inviteApi.accept({
        token: token ?? '',
        username: username.trim(),
        password,
      }),
    onSuccess: () => setDone(true),
    onError: (err) => setError(describe(err)),
  })

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setError('')
    if (!username.trim() || !password) {
      setError('请填写用户名与密码')
      return
    }
    if (password.length < 12) {
      setError('密码长度不得少于 12 个字符')
      return
    }
    if (password !== confirm) {
      setError('两次输入的密码不一致')
      return
    }
    accept.mutate()
  }

  if (preview.isPending) return <PageLoading />

  if (preview.isError || !preview.data) {
    return (
      <div className="flex min-h-full items-center justify-center bg-base px-4 py-10">
        <div className="w-full max-w-[360px] rounded-card border border-line bg-surface p-6">
          <EmptyState
            title="邀请无效"
            description="这条邀请链接已过期、已被使用或被撤销。请让管理员重新发一封邀请邮件。"
          />
          <div className="mt-4 text-center">
            <Link to="/login" className="text-sm text-brand hover:underline">
              返回登录
            </Link>
          </div>
        </div>
      </div>
    )
  }

  const invite = preview.data

  if (done) {
    return (
      <div className="flex min-h-full items-center justify-center bg-base px-4 py-10">
        <div className="w-full max-w-[360px] rounded-card border border-line bg-surface p-6">
          <h1 className="text-base font-medium text-ink">注册完成</h1>
          <p className="mt-1.5 text-base text-ink-2">
            账号已创建。请用刚设置的用户名与密码登录。
          </p>
          <div className="mt-4 text-center">
            <Link to="/login" className="text-sm text-brand hover:underline">
              前往登录 →
            </Link>
          </div>
        </div>
      </div>
    )
  }

  return (
    <div className="flex min-h-full items-center justify-center bg-base px-4 py-10">
      <div className="w-full max-w-[360px] rounded-card border border-line bg-surface p-6">
        <header>
          <h1 className="text-base font-semibold text-ink">接受邀请</h1>
          <p className="mt-1 text-sm text-ink-3">设置用户名与密码，之后即可登录。</p>
        </header>

        {/* 先说清这是给谁的：用户据此判断自己是不是该点这个链接。 */}
        <div className="mt-4 flex flex-col gap-1.5 rounded-control border border-line bg-sunken px-3 py-2.5 text-base">
          <div className="flex items-center justify-between gap-2">
            <span className="text-ink-3">受邀邮箱</span>
            <span className="kc-nums text-ink-2">{invite.email}</span>
          </div>
          <div className="flex items-center justify-between gap-2">
            <span className="text-ink-3">角色</span>
            <span className="flex items-center gap-2">
              <StatusBadge tone={INVITE_STATUS_TONE[invite.status]}>
                {INVITE_STATUS_LABEL[invite.status]}
              </StatusBadge>
              <span className="text-ink-2">{invite.role === 'admin' ? '管理员' : '租户'}</span>
            </span>
          </div>
          <div className="flex items-center justify-between gap-2">
            <span className="text-ink-3">有效期至</span>
            <span className="text-ink-2">{relativeTime(invite.expires_at)}</span>
          </div>
        </div>

        <form onSubmit={handleSubmit} className="mt-4 flex flex-col gap-3">
          <Input
            label="用户名"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoFocus
            disabled={accept.isPending}
          />
          <Input
            label="密码"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            disabled={accept.isPending}
            // 说明为什么至少要 12 位：受邀人只设这一次密码（不走强制改密）。
            hint="至少 12 位。这个密码由你自己设定，之后不会要求你首次登录改密 —— 所以这次就要设好。"

          />
          <Input
            label="确认密码"
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            disabled={accept.isPending}
          />

          {error && (
            <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
              {error}
            </p>
          )}

          <Button type="submit" className="mt-1 w-full" loading={accept.isPending}>
            {accept.isPending ? '注册中…' : '完成注册'}
          </Button>
        </form>

        <p className="mt-4 text-center text-sm text-ink-3">
          已有账号？
          <Link to="/login" className="ml-1 text-brand hover:underline">
            直接登录
          </Link>
        </p>
      </div>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '注册失败，请稍后重试'
}
