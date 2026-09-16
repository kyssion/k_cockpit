import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  SOURCE_LABEL,
  UPDATE_STATUS_LABEL,
  settingsApi,
  type SettingItem,
  type UpdateResult,
} from '@/api/settings'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
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
        <h1 className="text-lg font-semibold text-ink">系统设置</h1>
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
          </section>
        )
      })}

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

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
