/**
 * 安全中心：二次验证方式的绑定（F-10-01 的前置条件）。
 *
 * 没有绑定渠道，428 就会永远无法通过——用户被挡在操作之外却没有出路。
 */
import { useMutation, useQuery } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { METHOD_LABEL, riskApi } from '@/api/risk'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { StatusBadge } from '@/components/common/StatusBadge'

export function SecurityPage() {
  const status = useQuery({ queryKey: ['security-setup'], queryFn: riskApi.setupStatus })

  const [otpauthURI, setOtpauthURI] = useState('')
  const [code, setCode] = useState('')
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null)
  const [error, setError] = useState('')

  const begin = useMutation({
    mutationFn: riskApi.beginTOTP,
    onSuccess: (result) => {
      setOtpauthURI(result.otpauth_uri)
      setRecoveryCodes(null)
      setError('')
    },
    onError: (err) => setError(describe(err)),
  })

  const confirm = useMutation({
    mutationFn: () => riskApi.confirmTOTP(code),
    onSuccess: (result) => {
      setRecoveryCodes(result.recovery_codes)
      setOtpauthURI('')
      setCode('')
      setError('')
      void status.refetch()
    },
    onError: (err) => setError(describe(err)),
  })

  function submitConfirm(event: FormEvent) {
    event.preventDefault()
    setError('')
    confirm.mutate()
  }

  if (status.isPending) return <PageLoading />

  const info = status.data
  const secret = otpauthURI ? secretOf(otpauthURI) : ''

  return (
    <div className="flex max-w-[720px] flex-col gap-4">
      <header>
        <h1 className="text-lg font-semibold text-ink">安全中心</h1>
        <p className="mt-1 text-base text-ink-3">
          删除虚拟机、移除节点等操作会造成不可逆的结果，执行前需要完成一次二次验证。
        </p>
      </header>

      {/*
        开发模式提示放在最上面，因为它改变的是「这个页面其余部分在说什么」：
        万能码开着时，「已绑定验证器」只表示流程走过了，不代表防护真的生效。
        把它藏进折叠区或只留在日志里，等于让所有人看着界面误判当前的安全状态。
      */}
      {info?.dev_bypass && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base font-medium text-warning">开发模式：万能验证码已启用</p>
          <p className="mt-1 text-base text-ink-2">
            当前配置下，二次验证可被一个固定的开发码直接通过，无需真实验证器。
            此处的「已绑定」只表示绑定流程走完了，
            <span className="font-medium text-ink">不代表防护生效</span>。
            部署前请清除环境变量 <code className="rounded bg-surface px-1">SECURITY_DEV_BYPASS_CODE</code>
            （生产环境配置它会导致服务启动失败）。
          </p>
        </div>
      )}

      {info && (
        <section className="rounded-card border border-line">
          <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">
            当前状态
          </h2>
          <dl className="flex flex-col gap-3 px-4 py-3.5 text-base">
            <div className="flex items-center gap-3">
              <dt className="w-32 text-ink-3">验证器 App</dt>
              <dd>
                <StatusBadge tone={info.totp_enabled ? 'success' : 'idle'}>
                  {info.totp_enabled ? '已绑定' : '未绑定'}
                </StatusBadge>
              </dd>
            </div>
            <div className="flex items-center gap-3">
              <dt className="w-32 text-ink-3">恢复码</dt>
              <dd className="text-ink">
                {info.has_recovery_codes ? `剩余 ${info.recovery_code_count} 个` : '未生成'}
                {info.has_recovery_codes && info.recovery_code_count <= 2 && (
                  <span className="ml-2 text-warning">
                    数量不足，建议尽快重新绑定以生成新的恢复码
                  </span>
                )}
              </dd>
            </div>
            <div className="flex items-center gap-3">
              <dt className="w-32 text-ink-3">可用方式</dt>
              <dd className="text-ink">
                {info.methods.length > 0
                  ? info.methods.map((m) => METHOD_LABEL[m] ?? m).join('、')
                  : '无（需先绑定验证器）'}
              </dd>
            </div>
          </dl>
        </section>
      )}

      {/* 恢复码只在生成的那一刻显示：服务端只存哈希，之后不可能再取回。 */}
      {recoveryCodes && (
        <section className="rounded-card border border-warning/40 bg-warning/10">
          <h2 className="border-b border-warning/30 px-4 py-2.5 text-sm font-medium text-ink">
            恢复码（只显示这一次）
          </h2>
          <div className="flex flex-col gap-3 px-4 py-3.5">
            <p className="text-base text-ink-2">
              请立即保存到安全的地方。手机丢失时，这是你唯一的登录途径。
              <strong className="font-medium">每个恢复码只能使用一次</strong>，服务端
              只保存了它们的哈希值，关闭后无法再次查看。
            </p>
            <ul className="kc-mono grid grid-cols-2 gap-1.5 rounded-control bg-surface px-3 py-3 text-base text-ink">
              {recoveryCodes.map((c) => (
                <li key={c}>{c}</li>
              ))}
            </ul>
            <div className="flex justify-end">
              <Button variant="secondary" size="sm" onClick={() => setRecoveryCodes(null)}>
                我已保存
              </Button>
            </div>
          </div>
        </section>
      )}

      {!recoveryCodes && (
        <section className="rounded-card border border-line">
          <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">
            {info?.totp_enabled ? '重新绑定验证器' : '绑定验证器'}
          </h2>

          <div className="flex flex-col gap-4 px-4 py-3.5">
            {info?.totp_enabled && !otpauthURI && (
              <p className="text-base text-ink-3">
                重新绑定会替换现有密钥并重新生成一批恢复码。此前的密钥立即失效，
                所有登录会话也会被登出。
              </p>
            )}

            {!otpauthURI && (
              <div className="flex justify-end">
                <Button
                  size="sm"
                  variant={info?.totp_enabled ? 'secondary' : 'primary'}
                  loading={begin.isPending}
                  onClick={() => begin.mutate()}
                >
                  {info?.totp_enabled ? '重新绑定' : '开始绑定'}
                </Button>
              </div>
            )}

            {otpauthURI && (
              <form onSubmit={submitConfirm} className="flex flex-col gap-4">
                <p className="text-base text-ink-2">
                  在验证器 App（Google Authenticator、1Password 等）中手动添加账户，
                  输入下面的密钥：
                </p>
                <code className="kc-mono select-all rounded-control bg-sunken px-3 py-2.5 text-base tracking-wider text-ink">
                  {secret}
                </code>
                <p className="text-base text-ink-3">
                  添加完成后，输入 App 中显示的 6 位动态码以确认绑定。这一步是为了
                  确保你确实能算出码——否则下次登录时你才会发现自己被锁在外面。
                </p>

                <Input
                  label="动态码"
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  placeholder="6 位数字"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  autoFocus
                  disabled={confirm.isPending}
                />

                <div className="flex justify-end gap-2">
                  <Button
                    variant="secondary"
                    size="sm"
                    type="button"
                    onClick={() => setOtpauthURI('')}
                  >
                    取消
                  </Button>
                  <Button size="sm" type="submit" loading={confirm.isPending}>
                    确认绑定
                  </Button>
                </div>
              </form>
            )}

            {error && (
              <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
                {error}
              </p>
            )}
          </div>
        </section>
      )}
    </div>
  )
}

/** 从 otpauth URI 中取出密钥，供用户手动输入。 */
function secretOf(uri: string): string {
  try {
    return new URL(uri).searchParams.get('secret') ?? ''
  } catch {
    return ''
  }
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
