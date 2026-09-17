/**
 * APIKeyPage 管理用户的 API 凭证（F-1-10）。
 *
 * 页面的重点不是「怎么生成一个 Key」（那只是一次点击），而是让用户
 * **看清这个凭据的分量**：用它调用接口不会触发二次验证，因此一旦泄漏，
 * 所有高风险操作的验证都被绕过。界面上把这一点写在最上面，并把
 * 「来源 IP」与「到期」作为主要选项而不是高级设置。
 *
 * 另一处刻意的设计：明文只在生成后的那一次显示。因此生成成功后弹出一个
 * **必须手动关闭**的对话框，而不是一个会自动消失的提示。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { UNVERIFIED_EXAMPLES, apiKeyApi, type APIKeyCreated } from '@/api/apikey'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { relativeTime } from '@/utils/format'

export function APIKeyPage() {
  const queryClient = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [created, setCreated] = useState<APIKeyCreated | null>(null)
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const key = useQuery({ queryKey: ['api-key'], queryFn: apiKeyApi.get })

  const revoke = useMutation({
    mutationFn: apiKeyApi.revoke,
    onSuccess: () => {
      setError('')
      setNotice('凭证已撤销。记录仍保留，可用于事后追查。')
      void queryClient.invalidateQueries({ queryKey: ['api-key'] })
    },
    onError: (err) => setError(describe(err)),
  })

  if (key.isPending) return <PageLoading />

  const data = key.data

  return (
    <div className="flex max-w-3xl flex-col gap-5">
      <header>
        <h1 className="text-lg font-semibold text-ink">API 凭证</h1>
        <p className="mt-1 text-base text-ink-3">
          用 API Key 调用接口时可以不带会话 Cookie，
          <span className="text-ink-2">但也不会触发二次验证</span>。
        </p>
      </header>

      {/* 这一块是本页面的重点。把代价写清楚，而不是藏在文档里。 */}
      <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
        <p className="text-base font-medium text-warning">关于「不触发二次验证」</p>
        <p className="mt-1 text-base text-ink-2">
          二次验证要求有人在那一端输入，而 API 的使用场景恰恰是「没有人在那一端」——
          因此这是一个必要的取舍。但它的代价需要你知道：
          <span className="text-ink">一个泄漏的 Key 等同于绕过下列操作的验证</span>
          ，它们在浏览器里都是要过一次验证码的。
        </p>
        <ul className="mt-1.5 flex flex-wrap gap-x-4 gap-y-1">
          {UNVERIFIED_EXAMPLES.map((e) => (
            <li key={e} className="text-sm text-ink-2">
              · {e}
            </li>
          ))}
        </ul>
        <p className="mt-1.5 text-sm text-ink-3">
          因此建议：<span className="text-ink-2">绑定来源 IP</span>
          （Key 泄漏后唯一还能挡住攻击者的东西），并设置一个合理的到期时间。
        </p>
      </div>

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      <section className="rounded-card border border-line bg-surface p-4">
        {!data?.exists ? (
          <div className="flex flex-col items-start gap-3">
            <p className="text-base text-ink">你还没有生成 API 凭证</p>
            <p className="text-sm text-ink-3">
              生成后可以用于脚本与自动化任务。每个账号只有一个 Key，
              重新生成会<span className="text-ink-2">立即让旧的失效</span>。
            </p>
            <Button size="sm" onClick={() => setCreateOpen(true)}>
              生成凭证
            </Button>
          </div>
        ) : (
          <div className="flex flex-col gap-3">
            <div className="flex items-baseline justify-between gap-3">
              <span className="kc-mono text-base text-ink">{data.prefix}…</span>
              <span className="text-xs">
                {data.usable ? (
                  <span className="text-success">可用</span>
                ) : data.revoked_at ? (
                  <span className="text-ink-3">已撤销</span>
                ) : (
                  <span className="text-danger">已过期</span>
                )}
              </span>
            </div>

            <dl className="grid gap-2 text-sm sm:grid-cols-2">
              <Row label="来源限制">
                {data.allowed_ips.length > 0 ? (
                  <span className="kc-mono">{data.allowed_ips.join('、')}</span>
                ) : (
                  // 不限制时明确提示，而不是显示一个空格——空白看起来像
                  // 还没加载出来。
                  <span className="text-warning">不限来源（建议设置）</span>
                )}
              </Row>
              <Row label="到期">
                {data.expires_at ? (
                  formatDate(data.expires_at)
                ) : (
                  <span className="text-warning">永不过期（一次泄漏永久有效）</span>
                )}
              </Row>
              <Row label="创建于">{data.created_at ? relativeTime(data.created_at) : '—'}</Row>
              <Row label="最后使用">
                {/* 「从没用过」是一个有用的信号：说明可以撤掉了。 */}
                {data.last_used_at ? relativeTime(data.last_used_at) : '从未使用'}
              </Row>
            </dl>

            <div className="flex gap-2 border-t border-line pt-3">
              <Button variant="secondary" size="sm" onClick={() => setCreateOpen(true)}>
                轮换
              </Button>
              <Button
                variant="danger"
                size="sm"
                disabled={!data.usable}
                loading={revoke.isPending}
                onClick={() => revoke.mutate()}
              >
                撤销
              </Button>
            </div>
          </div>
        )}
      </section>

      <CreateKeyModal
        open={createOpen}
        rotating={data?.exists === true}
        onClose={() => setCreateOpen(false)}
        onDone={(result) => {
          setCreateOpen(false)
          setError('')
          setCreated(result)
          setCopied(false)
          void queryClient.invalidateQueries({ queryKey: ['api-key'] })
        }}
        onError={(msg) => {
          setCreateOpen(false)
          setError(msg)
        }}
      />

      {/* 明文只出现这一次。因此这个对话框**不自动关闭**，也不在点背景时
          消失——用户随手点一下关掉，就再也拿不回这个 Key 了。 */}
      <Modal
        open={created !== null}
        title="凭证已生成 —— 请立即复制"
        description="这是它唯一一次出现。服务端只保存它的哈希，之后无法再显示，也无法找回；丢失只能重新生成。"
        onClose={() => setCreated(null)}
        dismissable={false}
        footer={
          <>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                if (created) {
                  void navigator.clipboard?.writeText(created.plain_key)
                  setCopied(true)
                }
              }}
            >
              {copied ? '已复制' : '复制'}
            </Button>
            <Button size="sm" onClick={() => setCreated(null)}>
              我已保存，关闭
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-3">
          <pre className="kc-mono overflow-x-auto rounded-control border border-line bg-sunken px-3 py-2 text-sm text-ink">
            {created?.plain_key}
          </pre>
          <p className="text-sm text-ink-3">
            调用时放在请求头里：
            <span className="kc-mono ml-1">Authorization: Bearer &lt;key&gt;</span>
          </p>
          {created?.allowed_ips.length === 0 && (
            <p className="text-sm text-warning">
              这个凭证没有绑定来源 IP，任何地址都可以用它——建议撤销后重新生成一个
              带来源限制的。
            </p>
          )}
        </div>
      </Modal>
    </div>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-ink-3">{label}</dt>
      <dd className="text-ink">{children}</dd>
    </div>
  )
}

function CreateKeyModal({
  open,
  rotating,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  rotating: boolean
  onClose: () => void
  onDone: (result: APIKeyCreated) => void
  onError: (message: string) => void
}) {
  const [ips, setIPs] = useState('')
  const [days, setDays] = useState('365')

  const create = useMutation({
    mutationFn: () =>
      apiKeyApi.create({
        allowed_ips: splitList(ips),
        expires_in_days: Number(days) || 0,
      }),
    onSuccess: onDone,
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title={rotating ? '轮换 API 凭证' : '生成 API 凭证'}
      description={
        rotating
          ? '每个账号只有一个凭证：生成新的会**立即让旧的失效**。正在使用旧 Key 的脚本会开始报 401，请先做好准备。'
          : '生成后可以在脚本与自动化任务里使用。'
      }
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={create.isPending} onClick={() => create.mutate()}>
            {rotating ? '轮换' : '生成'}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        {/* 来源限制放在第一个而不是高级设置里：它是 Key 泄漏之后**唯一
            还能挡住攻击者**的东西。 */}
        <Input
          label="来源限制（每行一个 IP 或网段，留空表示不限）"
          value={ips}
          placeholder={'203.0.113.7\n10.0.0.0/8'}
          onChange={(e) => setIPs(e.target.value)}
          hint="强烈建议填写。留空意味着这个凭证从任何地址都能用。"
        />
        <Input
          label="有效天数（0 表示永不过期）"
          value={days}
          onChange={(e) => setDays(e.target.value)}
          hint="永不过期意味着一次泄漏永久有效。长期运行的自动化可以设一年，到期前轮换。"
        />
      </div>
    </Modal>
  )
}

function splitList(raw: string): string[] {
  return raw
    .split(/[\n,;]/)
    .map((s) => s.trim())
    .filter(Boolean)
}

function formatDate(iso: string): string {
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(
    d.getDate(),
  ).padStart(2, '0')}`
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
