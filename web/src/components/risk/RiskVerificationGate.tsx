/**
 * 高风险操作的验证弹窗（f-10-01）。
 *
 * 它是全局单例：由请求层在收到 428 时唤起，验证通过后原请求由请求层自动
 * 重放。页面代码既不感知 428，也不需要自己弹框。
 *
 * 三条来自规格的硬约束：
 *   - **按后端返回的方式渲染**（R-007/Q-005）：用户可能只绑了 TOTP、或用完
 *     了恢复码，硬编码三种方式会弹出一个无法完成的验证框；
 *   - **重试仅一次**（Q-010）：重放后仍 428 时由请求层直接报错，不再弹框；
 *   - **失败时给出剩余次数**（R-006），但不区分「码错误」与「码过期」（R-009）。
 */
import { useQuery } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Link } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { METHOD_LABEL, METHOD_PLACEHOLDER, riskApi } from '@/api/risk'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { useRiskStore, type PendingVerification } from '@/stores/risk'

export function RiskVerificationGate() {
  const pending = useRiskStore((s) => s.pending)

  if (!pending) return null

  // 用 key 让弹窗在每次新的验证流程开始时重新挂载：输入框与错误提示随
  // 之重置，不必在 effect 里同步 setState（那会多一次渲染，也更容易在
  // 条件判断上出错——例如把上一次的验证码带进这一次）。
  return <RiskVerificationDialog key={pending.required.challenge_id} pending={pending} />
}

function RiskVerificationDialog({ pending }: { pending: PendingVerification }) {
  const settle = useRiskStore((s) => s.settle)

  const [method, setMethod] = useState(pending.required.methods[0]?.method ?? '')
  const [code, setCode] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  // 清单用于把动作标识翻译成用户看得懂的名字。查一次即可（清单是静态的）。
  const policy = useQuery({
    queryKey: ['risk-policy'],
    queryFn: riskApi.policy,
    staleTime: Infinity,
  })

  const required = pending.required
  const actionLabel =
    policy.data?.actions.find((a) => a.action === required.action)?.label ?? required.action
  const actionReason = policy.data?.actions.find((a) => a.action === required.action)?.reason

  async function submit(event: FormEvent) {
    event.preventDefault()
    if (!code.trim()) {
      setError('请输入验证码')
      return
    }

    setSubmitting(true)
    setError('')
    try {
      const result = await riskApi.verify(required.challenge_id, method, code)
      // 许可交给请求层，由它重放原请求。
      settle(result.grant)
    } catch (err) {
      setError(describe(err))
      // 清空输入：验证码错了一次多半是打错了，让用户重新输入比让他先删掉
      // 旧内容更顺手。
      setCode('')
    } finally {
      setSubmitting(false)
    }
  }

  function cancel() {
    // 返回 null 让原请求以 428 结束，页面上会显示「该操作需要完成二次验证」。
    settle(null)
  }

  return (
    <Modal
      open
      title={`完成验证后继续：${actionLabel}`}
      description={actionReason}
      onClose={cancel}
    >
      {required.methods.length === 0 ? (
        // 未绑定任何方式：给出出路，而不是让用户面对一个没有输入框的弹框。
        <div className="flex flex-col gap-4">
          <p className="text-base text-ink-2">
            该操作需要二次验证，但当前账号尚未绑定验证方式。请先绑定验证器 App，
            之后再进行此操作。
          </p>
          <div className="flex justify-end gap-2">
            <Button variant="secondary" size="sm" onClick={cancel}>
              取消
            </Button>
            <Link to="/security">
              <Button size="sm">去绑定</Button>
            </Link>
          </div>
        </div>
      ) : (
        <form onSubmit={submit} className="flex flex-col gap-4">
          {required.methods.length > 1 && (
            <div className="flex flex-col gap-1.5">
              <label htmlFor="risk-method" className="text-sm font-medium text-ink-2">
                验证方式
              </label>
              <select
                id="risk-method"
                value={method}
                onChange={(e) => setMethod(e.target.value)}
                disabled={submitting}
                className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
              >
                {required.methods.map((m) => (
                  <option key={m.method} value={m.method}>
                    {m.label || METHOD_LABEL[m.method] || m.method}
                  </option>
                ))}
              </select>
            </div>
          )}

          <Input
            label={METHOD_LABEL[method] ?? '验证码'}
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder={METHOD_PLACEHOLDER[method]}
            autoFocus
            autoComplete="one-time-code"
            disabled={submitting}
          />

          {method === 'recovery_code' && (
            <p className="text-sm text-ink-3">
              每个恢复码只能使用一次，用掉后剩余数量会减少。
            </p>
          )}

          {error && (
            <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
              {error}
            </p>
          )}

          <div className="flex justify-end gap-2">
            <Button variant="secondary" size="sm" type="button" onClick={cancel}>
              取消
            </Button>
            <Button size="sm" type="submit" loading={submitting}>
              验证并继续
            </Button>
          </div>
        </form>
      )}
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '验证失败，请稍后重试'
}
