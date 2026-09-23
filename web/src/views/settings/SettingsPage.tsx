import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  SOURCE_LABEL,
  UPDATE_STATUS_LABEL,
  authKeyApi,
  requestLogApi,
  settingsApi,
  type RequestLogItem,
  type SettingItem,
  type UpdateResult,
} from '@/api/settings'
import { relativeTime } from '@/utils/format'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function SettingsPage() {
  const queryClient = useQueryClient()
  const [draft, setDraft] = useState<Record<string, string>>({})
  const [results, setResults] = useState<UpdateResult[] | null>(null)
  const [error, setError] = useState('')
  const [rollbackTarget, setRollbackTarget] = useState<SettingItem | null>(null)

  const list = useQuery({ queryKey: ['settings'], queryFn: settingsApi.list })

  function refresh() {
    setDraft({})
    void queryClient.invalidateQueries({ queryKey: ['settings'] })
  }

  const update = useMutation({
    mutationFn: () => settingsApi.update(draft),
    onSuccess: (res) => {
      setResults(res.results)
      setError('')
      refresh()
    },
    onError: (err) => {
      setError(describe(err))
      setResults(null)
    },
  })

  const rollback = useMutation({
    mutationFn: (key: string) => settingsApi.rollback(key),
    onSuccess: () => {
      setRollbackTarget(null)
      setError('')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (list.isPending) return <PageLoading />
  if (list.isError) {
    return (
      <p className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
        {describe(list.error)}
      </p>
    )
  }

  const { groups, items } = list.data
  const dirty = Object.keys(draft).length > 0
  const failedKeys = new Set((results ?? []).filter((r) => r.status !== 'applied').map((r) => r.key))

  return (
    <div className="flex max-w-[760px] flex-col gap-5">
      <header>
        <h1 className="text-xl font-semibold text-ink">系统设置</h1>
        <p className="mt-1 text-base text-ink-3">
          标为「环境变量」的项由部署配置指定，面板中不可修改——
          环境变量代表部署者的意图，界面上的改动不应悄悄覆盖它。
        </p>
      </header>

      {error && (
        <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {groups.map((group) => {
        const groupItems = items.filter((it) => it.group === group.key)
        if (groupItems.length === 0) return null

        return (
          <section key={group.key} className="flex flex-col gap-2">
            <h2 className="text-sm font-medium text-ink-2">{group.label}</h2>
            <div className="flex flex-col divide-y divide-line rounded-card border border-line">
              {groupItems.map((item) => (
                <SettingRow
                  key={item.key}
                  item={item}
                  value={draft[item.key] ?? item.value}
                  dirty={Object.hasOwn(draft, item.key)}
                  failed={failedKeys.has(item.key)}
                  onChange={(v) => setDraft((d) => ({ ...d, [item.key]: v }))}
                  onRollback={() => setRollbackTarget(item)}
                />
              ))}
            </div>
            {/*
              邮件是唯一一个"改完必须验证"的分组：SMTP 配置写错时没有任何
              可见后果，直到有人找回密码才发现发不出信。因此把验证入口直接
              放在这一组下面，而不是塞进某个折叠区。
            */}
            {group.key === 'notification' && <MailTestPanel />}
            {/*
              会话密钥是安全组里唯一一个「点了就全员登出」的动作，因此把它
              放在这一组下面，而不是塞进某个折叠区：它需要和自己的说明在一起。
            */}
            {group.key === 'security' && <AuthKeyPanel />}
          </section>
        )
      })}

      {/* 请求日志：排查时才需要，因此默认关（开关在「安全」分组里）。 */}
      <RequestLogPanel />

      {results && results.length > 0 && (
        <div className="flex flex-col gap-1.5 rounded-card border border-line bg-raised px-4 py-3 text-base">
          <span className="text-ink-2">
            共 {results.length} 项，{results.filter((r) => r.status === 'applied').length} 项已生效
          </span>
          {results
            .filter((r) => r.status !== 'applied')
            .map((r) => (
              <span key={r.key} className="text-danger">
                {r.key}：{UPDATE_STATUS_LABEL[r.status]}
                {r.message ? ` — ${r.message}` : ''}
              </span>
            ))}
        </div>
      )}

      {/* 悬浮保存条：设置项分散在多个分组里，用户改完不一定记得滚到底部。 */}
      {dirty && (
        <div className="sticky bottom-0 flex items-center justify-between rounded-card border border-brand/40 bg-surface px-4 py-3">
          <span className="text-base text-ink-2">
            有 {Object.keys(draft).length} 项待保存
          </span>
          <div className="flex gap-2">
            <Button variant="secondary" size="sm" onClick={() => setDraft({})}>
              放弃
            </Button>
            <Button size="sm" loading={update.isPending} onClick={() => update.mutate()}>
              保存
            </Button>
          </div>
        </div>
      )}

      <Modal
        open={rollbackTarget !== null}
        title={`回滚「${rollbackTarget?.label ?? ''}」`}
        description="将该项恢复到最近一次变更前的值。回滚本身也是一次变更，因此还可以再回滚回来。"
        onClose={() => setRollbackTarget(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setRollbackTarget(null)}>
              取消
            </Button>
            <Button
              size="sm"
              loading={rollback.isPending}
              onClick={() => rollbackTarget && rollback.mutate(rollbackTarget.key)}
            >
              确认回滚
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          当前值：
          <span className="kc-mono ml-1 text-ink">{rollbackTarget?.value || '（空）'}</span>
        </p>
      </Modal>
    </div>
  )
}

/**
 * 测试发信。
 *
 * 失败时把**邮件服务器返回的真实原因**显示出来（后端已原样带出），而不是
 * 一句"发送失败"：主机名写错、端口不通、加密方式不匹配、认证被拒，这四者
 * 的处理方式完全不同，笼统报错等于让用户逐个试。
 */
function MailTestPanel() {
  const [to, setTo] = useState('')
  const [error, setError] = useState('')
  const [ok, setOk] = useState(false)

  const send = useMutation({
    mutationFn: () => settingsApi.testMail(to.trim()),
    onSuccess: () => {
      setOk(true)
      setError('')
    },
    onError: (err) => {
      setError(describe(err))
      setOk(false)
    },
  })

  return (
    <div className="flex flex-col gap-2.5 rounded-card border border-line bg-raised px-4 py-3">
      <p className="text-base text-ink-3">
        保存 SMTP 配置后发一封测试邮件，确认找回密码与邮箱绑定可用。
      </p>
      <div className="flex items-end gap-2">
        <div className="flex-1">
          <Input
            label="接收测试邮件的邮箱"
            type="email"
            value={to}
            onChange={(e) => {
              setTo(e.target.value)
              setOk(false)
            }}
            disabled={send.isPending}
          />
        </div>
        <Button
          size="sm"
          variant="secondary"
          loading={send.isPending}
          disabled={!to.trim()}
          onClick={() => send.mutate()}
        >
          发送测试邮件
        </Button>
      </div>
      {ok && <p className="text-base text-success">已发送，请查收收件箱。</p>}
      {error && (
        <p role="alert" className="text-base text-danger">
          {error}
        </p>
      )}
    </div>
  )
}

function SettingRow({
  item,
  value,
  dirty,
  failed,
  onChange,
  onRollback,
}: {
  item: SettingItem
  value: string
  dirty: boolean
  failed: boolean
  onChange: (value: string) => void
  onRollback: () => void
}) {
  return (
    <div className="flex flex-col gap-1.5 px-4 py-3">
      <div className="flex items-start justify-between gap-3">
        <label htmlFor={item.key} className="flex flex-col gap-0.5">
          <span className="flex items-center gap-2 text-base font-medium text-ink">
            {item.label}
            {dirty && <span className="text-xs text-brand">未保存</span>}
            {failed && <span className="text-xs text-danger">保存失败</span>}
          </span>
          {item.description && (
            <span className="text-sm text-ink-3">{item.description}</span>
          )}
        </label>

        <div className="flex shrink-0 items-center gap-2">
          {item.locked ? (
            <StatusBadge tone="warning">环境变量</StatusBadge>
          ) : (
            <span className="text-xs text-ink-3">{SOURCE_LABEL[item.source]}</span>
          )}
          {item.can_rollback && item.rollbackable && !item.locked && (
            <Button variant="ghost" size="sm" onClick={onRollback}>
              回滚
            </Button>
          )}
        </div>
      </div>

      <FieldControl id={item.key} item={item} value={value} onChange={onChange} />

      {item.locked && (
        <span className="text-xs text-warning">
          该项由 {item.locked_by} 指定，请在部署配置中修改
        </span>
      )}
      {item.apply === 'restart' && (
        <span className="text-xs text-ink-3">修改后需重启服务生效</span>
      )}
    </div>
  )
}

function FieldControl({
  id,
  item,
  value,
  onChange,
}: {
  id: string
  item: SettingItem
  value: string
  onChange: (value: string) => void
}) {
  const base =
    'h-8 rounded-control border border-line-strong bg-sunken px-2.5 text-base text-ink focus:outline-none focus-visible:border-brand disabled:opacity-60'

  // 被环境变量锁定：只读（R-002）。界面上不给编辑入口，服务端也会拒绝——
  // 两者都做，是为了让用户**当场**明白为什么改不了，而不是提交后才收到拒绝。
  const disabled = item.locked

  if (item.kind === 'bool') {
    return (
      <select
        id={id}
        value={value === 'true' ? 'true' : 'false'}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className={`${base} w-32`}
      >
        <option value="true">开启</option>
        <option value="false">关闭</option>
      </select>
    )
  }

  if (item.kind === 'select') {
    return (
      <select
        id={id}
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className={`${base} w-56`}
      >
        {(item.options ?? []).map((opt) => (
          <option key={opt.value} value={opt.value}>
            {opt.label}
          </option>
        ))}
      </select>
    )
  }

  return (
    <div className="flex items-center gap-2">
      <input
        id={id}
        type={item.kind === 'int' ? 'number' : 'text'}
        value={value}
        disabled={disabled}
        min={item.min_value}
        max={item.max_value}
        onChange={(e) => onChange(e.target.value)}
        className={`${base} ${item.kind === 'int' ? 'w-32' : 'w-72'}`}
      />
      {item.unit && <span className="text-sm text-ink-3">{item.unit}</span>}
    </div>
  )
}

/**
 * RequestLogPanel 请求日志的查看与清理（F-10-07）。
 *
 * 与审计的区别必须写在界面上：审计回答"谁改了什么"，请求日志回答"接口被
 * 调用了多少次、慢不慢"。量级差两个数量级，因此默认关闭。
 */
function RequestLogPanel() {
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)

  const list = useQuery({
    queryKey: ['request-logs'],
    queryFn: () => requestLogApi.list(undefined, 100),
    enabled: open,
  })

  const clear = useMutation({
    mutationFn: () => requestLogApi.clear(),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['request-logs'] })
    },
  })

  const items = list.data?.items ?? []

  return (
    <section className="rounded-card border border-line">
      <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
        <div>
          <h2 className="text-sm font-medium text-ink-2">请求日志</h2>
          <p className="mt-0.5 text-xs text-ink-3">
            记录每一次接口调用（方法、路径、状态码、耗时）。健康检查与静态资源不记。
          </p>
        </div>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="secondary" onClick={() => setOpen((v) => !v)}>
            {open ? '收起' : '查看'}
          </Button>
          <Button
            size="sm"
            variant="secondary"
            loading={clear.isPending}
            disabled={items.length === 0}
            onClick={() => clear.mutate()}
          >
            清空
          </Button>
        </div>
      </div>

      {open && (
        <div className="px-4 py-3">
          {list.data && !list.data.enabled && (
            <p className="text-base text-warning">
              记录开关当前是关闭的，下面这些是开启期间留下的数据。
            </p>
          )}

          {items.length === 0 ? (
            <p className="text-base text-ink-3">还没有请求日志。</p>
          ) : (
            <div className="max-h-80 overflow-auto">
              <table className="w-full border-collapse text-base">
                <thead>
                  <tr className="text-left text-xs text-ink-3">
                    <th className="px-2 py-1.5 font-normal">时间</th>
                    <th className="px-2 py-1.5 font-normal">用户</th>
                    <th className="px-2 py-1.5 font-normal">方法</th>
                    <th className="px-2 py-1.5 font-normal">路径</th>
                    <th className="px-2 py-1.5 font-normal">状态</th>
                    <th className="px-2 py-1.5 font-normal">耗时</th>
                  </tr>
                </thead>
                <tbody>
                  {items.map((it: RequestLogItem) => (
                    <tr key={it.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                      <td className="px-2 py-1.5 text-ink-3">{relativeTime(it.at)}</td>
                      <td className="px-2 py-1.5 text-ink-2">{it.username || '—'}</td>
                      <td className="px-2 py-1.5 text-ink-2">{it.method}</td>
                      <td className="kc-mono px-2 py-1.5 text-ink-2">{it.path}</td>
                      <td className="kc-nums px-2 py-1.5">{it.status}</td>
                      <td className="kc-nums px-2 py-1.5 text-ink-3">{it.duration_ms} ms</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      )}
    </section>
  )
}

/**
 * AuthKeyPanel 会话签名密钥的轮换（F-1-09）。
 *
 * 必须说清的一件事：**轮换等于全员登出**。轮换之后所有旧令牌立即失效，
 * 包括正在点这个按钮的人自己——所以按钮不是"改个配置"，而是一次需要
 * 二次验证的、影响所有人的动作。
 */
function AuthKeyPanel() {
  const queryClient = useQueryClient()

  const status = useQuery({
    queryKey: ['auth-key'],
    queryFn: authKeyApi.status,
  })

  const rotate = useMutation({
    mutationFn: authKeyApi.rotate,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['auth-key'] })
    },
  })

  const st = status.data

  return (
    <div className="flex flex-col gap-2 rounded-card border border-line bg-raised px-4 py-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div>
          <span className="text-base text-ink">会话签名密钥</span>
          <span className="ml-2 text-sm text-ink-3">
            {st ? `上次轮换 ${relativeTime(st.rotated_at)}` : '读取中…'}
          </span>
        </div>
        <Button
          size="sm"
          variant="secondary"
          loading={rotate.isPending}
          disabled={!st}
          onClick={() => rotate.mutate()}
        >
          立即轮换
        </Button>
      </div>

      <p className="text-sm text-ink-3">
        轮换会更换会话令牌的签名密钥，**所有人（包括你自己）都会被立即登出**，
        因此需要二次验证。自动轮换的间隔是上面的「会话密钥自动轮换间隔」，
        填 0 表示不自动轮换。
      </p>

      {st && st.auto_rotate_days > 0 && st.age_days >= st.auto_rotate_days && (
        <p className="text-sm text-warning">
          已超过自动轮换间隔（{st.auto_rotate_days} 天），下次检查时会自动轮换。
        </p>
      )}

      {rotate.isError && (
        <p role="alert" className="text-sm text-danger">
          {describe(rotate.error)}
        </p>
      )}
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
