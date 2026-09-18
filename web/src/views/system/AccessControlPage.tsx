/**
 * AccessControlPage 公网访问与开发模式开关（F-10-06）。
 *
 * 它看起来像普通设置项，但有一处别的设置没有的性质：**关掉公网访问会切断
 * 你自己**——如果你此刻正是从公网访问的，那一刻连接就断了，而界面还没来得及
 * 显示"已保存"。
 *
 * 因此界面上要在按下开关**之前**就把这件事说出来，而不是等后端返回一个
 * 需要确认的响应。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { accessControlApi } from '@/api/accesscontrol'
import { ApiError, NetworkError } from '@/api/client'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'

export function AccessControlPage() {
  const queryClient = useQueryClient()
  const [confirmClose, setConfirmClose] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const { data, isPending } = useQuery({
    queryKey: ['access-control'],
    queryFn: accessControlApi.get,
  })

  const save = useMutation({
    mutationFn: (req: { public_enabled?: boolean; dev_mode?: boolean; confirm?: boolean }) =>
      accessControlApi.set(req),
    onSuccess: (v) => {
      setConfirmClose(false)
      setError('')
      setNotice('已保存')
      void queryClient.invalidateQueries({ queryKey: ['access-control'] })
      void queryClient.setQueryData(['access-control'], v)
    },
    onError: (e) => {
      setConfirmClose(false)
      setError(describe(e))
    },
  })

  if (isPending || !data) return <PageLoading />

  const locked = data.env_locked

  return (
    <div className="flex flex-col gap-5">
      <header>
        <h1 className="text-lg font-semibold text-ink">访问控制</h1>
        <p className="mt-1 text-base text-ink-3">
          控制面板是否接受公网访问，以及是否开启开发模式。
        </p>
      </header>

      {notice && <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {/* 被环境变量锁定时**禁用开关并说明原因**：不说明的话，用户会反复点
          一个永远不生效的开关，然后怀疑是面板坏了。 */}
      {locked && (
        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-base text-warning">
          这两项由环境变量指定，面板无法修改——环境变量优先于面板设置。
          要改请调整部署参数并重启。
        </p>
      )}

      {data.caller_is_public && (
        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-base text-warning">
          你当前正是从公网访问（{data.caller_ip}）。关闭公网访问会立刻断开你
          这条连接，而这次改动本身是生效的——之后需要从内网重新访问面板。
        </p>
      )}

      <section className="rounded-card border border-line bg-surface p-4">
        <Toggle
          label="允许公网访问"
          checked={data.public_enabled}
          disabled={locked}
          hint="关闭后只接受内网来源。它是安全边界，不是普通开关：环境变量可覆盖此处。"
          onToggle={(next) => {
            // 只有在**从公网关掉**时才需要确认——那正是会切断自己的情形。
            if (!next && data.caller_is_public) {
              setConfirmClose(true)
              return
            }
            save.mutate({ public_enabled: next, confirm: true })
          }}
        />
      </section>

      <section className="rounded-card border border-line bg-surface p-4">
        <Toggle
          label="开发模式"
          checked={data.dev_mode}
          disabled={locked}
          hint="开发模式下二次验证会接受一个万能码，便于本地调试。生产环境不要开启。"
          onToggle={(next) => save.mutate({ dev_mode: next, confirm: true })}
        />
      </section>

      <p className="text-sm text-ink-3">
        当前值来源：{data.effected_by}
      </p>

      <Modal
        open={confirmClose}
        title="关闭公网访问"
        description="这会立刻断开你当前的连接。"
        onClose={() => setConfirmClose(false)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setConfirmClose(false)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={save.isPending}
              onClick={() => save.mutate({ public_enabled: false, confirm: true })}
            >
              确认关闭
            </Button>
          </>
        }
      >
        <p className="text-sm text-ink-3">
          你此刻正是从公网访问（{data.caller_ip}）。确认之后这条连接会断开，
          而改动**已经生效**——需要从内网重新打开面板。如果内网访问还没验证过，
          建议先在内网确认能打开再关。
        </p>
      </Modal>
    </div>
  )
}

function Toggle({
  label,
  checked,
  disabled,
  hint,
  onToggle,
}: {
  label: string
  checked: boolean
  disabled: boolean
  hint: string
  onToggle: (next: boolean) => void
}) {
  return (
    <label className={`flex gap-2.5 ${disabled ? 'cursor-not-allowed opacity-60' : 'cursor-pointer'}`}>
      <input
        type="checkbox"
        checked={checked}
        disabled={disabled}
        onChange={(e) => onToggle(e.target.checked)}
        className="mt-1"
      />
      <span className="flex flex-col gap-0.5">
        <span className="text-base text-ink">{label}</span>
        <span className="text-xs text-ink-3">{hint}</span>
      </span>
    </label>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
