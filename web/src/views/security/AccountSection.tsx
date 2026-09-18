/**
 * AccountSection 提供账号自管理：改密码、改用户名、重新生成恢复码。
 *
 * 三处要在界面上说清的后果：
 *
 *  改密码   → **会退出其它设备上的全部登录**。不说的话，用户第二天在公司
 *             电脑上发现自己被登出，会以为是系统出了问题。
 *  改用户名 → **不会**退出登录。用户名只是标识，密码才是凭据——把它和
 *             改密码写得一样严重反而会让人不敢改。
 *  恢复码   → 旧的会**全部作废**。用户以为只换了新的那张纸，而抽屉里那张
 *             旧的（可能已被人抄走）依然能进来。
 */
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { accountApi } from '@/api/account'
import { ApiError, NetworkError } from '@/api/client'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'

export function AccountSection({ username }: { username: string }) {
  const [modal, setModal] = useState<'password' | 'username' | 'recovery' | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const close = () => setModal(null)

  return (
    <section className="rounded-card border border-line">
      <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">
        账号
      </h2>

      <div className="flex flex-col gap-3 px-4 py-3">
        <Row
          title="登录名"
          value={username}
          action="修改"
          hint="只改标识，不影响登录状态与凭据。"
          onClick={() => {
            setError('')
            setNotice('')
            setModal('username')
          }}
        />
        <Row
          title="密码"
          value="••••••••"
          action="修改密码"
          hint="修改后会退出其它设备上的全部登录。"
          onClick={() => {
            setError('')
            setNotice('')
            setModal('password')
          }}
        />
        <Row
          title="恢复码"
          value="一批一次性备用码"
          action="重新生成"
          hint="用于验证器不可用时登录。重新生成会让之前的所有恢复码立即作废。"
          onClick={() => {
            setError('')
            setNotice('')
            setModal('recovery')
          }}
        />
      </div>

      {notice && (
        <p className="border-t border-line px-4 py-2.5 text-sm text-success">{notice}</p>
      )}
      {error && (
        <p className="border-t border-line px-4 py-2.5 text-sm text-danger">{error}</p>
      )}

      <PasswordModal
        open={modal === 'password'}
        onClose={close}
        onDone={(msg) => {
          close()
          setNotice(msg)
        }}
        onError={(m) => {
          close()
          setError(m)
        }}
      />
      <UsernameModal
        open={modal === 'username'}
        current={username}
        onClose={close}
        onDone={(msg) => {
          close()
          setNotice(msg)
        }}
        onError={(m) => {
          close()
          setError(m)
        }}
      />
      <RecoveryModal
        open={modal === 'recovery'}
        onClose={close}
        onDone={() => close()}
        onError={(m) => {
          close()
          setError(m)
        }}
      />
    </section>
  )
}

function Row({
  title,
  value,
  action,
  hint,
  onClick,
}: {
  title: string
  value: string
  action: string
  hint: string
  onClick: () => void
}) {
  return (
    <div className="flex flex-wrap items-baseline justify-between gap-2">
      <div className="flex flex-col gap-0.5">
        <span className="flex items-baseline gap-2 text-sm">
          <span className="text-ink-3">{title}</span>
          <span className="text-ink">{value}</span>
        </span>
        <span className="text-xs text-ink-3">{hint}</span>
      </div>
      <Button size="sm" variant="secondary" onClick={onClick}>
        {action}
      </Button>
    </div>
  )
}

function PasswordModal({
  open,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  onClose: () => void
  onDone: (notice: string) => void
  onError: (m: string) => void
}) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const mismatch = confirm !== '' && next !== confirm
  const ready = current !== '' && next.length >= 8 && next === confirm

  const change = useMutation({
    mutationFn: () => accountApi.changePassword(current, next),
    onSuccess: (r) => onDone(r.notice || '密码已修改'),
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title="修改密码"
      description="修改后，其它设备上的登录会立即失效——你需要在那台设备上重新登录。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" disabled={!ready} loading={change.isPending} onClick={() => change.mutate()}>
            确认修改
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="当前密码"
          type="password"
          value={current}
          onChange={(e) => setCurrent(e.target.value)}
        />
        <Input
          label="新密码"
          type="password"
          value={next}
          onChange={(e) => setNext(e.target.value)}
          hint="至少 8 位。"
        />
        <Input
          label="确认新密码"
          type="password"
          value={confirm}
          onChange={(e) => setConfirm(e.target.value)}
        />
        {mismatch && <p className="text-sm text-warning">两次输入的新密码不一致。</p>}
      </div>
    </Modal>
  )
}

function UsernameModal({
  open,
  current,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  current: string
  onClose: () => void
  onDone: (notice: string) => void
  onError: (m: string) => void
}) {
  const [password, setPassword] = useState('')
  const [name, setName] = useState('')
  const ready = password !== '' && name.trim().length >= 3

  const change = useMutation({
    mutationFn: () => accountApi.changeUsername(password, name.trim()),
    onSuccess: (r) => onDone(`登录名已改为 ${r.username}`),
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title="修改登录名"
      description="登录名只是一个标识，修改后不会影响当前登录状态。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" disabled={!ready} loading={change.isPending} onClick={() => change.mutate()}>
            确认修改
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input label="当前登录名" value={current} disabled onChange={() => {}} />
        <Input
          label="新登录名"
          value={name}
          onChange={(e) => setName(e.target.value)}
          hint="3~32 位，可含字母、数字、点、下划线与连字符。"
        />
        <Input
          label="当前密码"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          hint="改标识也要验证身份——会话可能是被人拿到的。"
        />
      </div>
    </Modal>
  )
}

function RecoveryModal({
  open,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  onClose: () => void
  onDone: () => void
  onError: (m: string) => void
}) {
  const [password, setPassword] = useState('')
  const [codes, setCodes] = useState<string[] | null>(null)
  const client = useQueryClient()

  const regen = useMutation({
    mutationFn: () => accountApi.regenerateRecoveryCodes(password),
    onSuccess: (r) => {
      setCodes(r.recovery_codes)
      // 恢复码数量变了，安全中心的那个计数要跟着更新。
      void client.invalidateQueries({ queryKey: ['security-setup'] })
    },
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title={codes ? '新的恢复码' : '重新生成恢复码'}
      description={
        codes
          ? undefined
          : '之前的所有恢复码会立即作废，包括你已经打印或保存的那些。'
      }
      onClose={codes ? onDone : onClose}
      footer={
        codes ? (
          <Button size="sm" onClick={onDone}>
            我已保存
          </Button>
        ) : (
          <>
            <Button variant="secondary" size="sm" onClick={onClose}>
              取消
            </Button>
            <Button
              size="sm"
              disabled={password === ''}
              loading={regen.isPending}
              onClick={() => regen.mutate()}
            >
              生成新的一批
            </Button>
          </>
        )
      }
    >
      {codes ? (
        <div className="flex flex-col gap-3">
          <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            这串码只显示这一次。请立即保存到安全的地方。
            <span className="block">之前的所有恢复码已经作废——包括你手上那张旧纸条。</span>
          </p>
          <div className="grid grid-cols-2 gap-2">
            {codes.map((c) => (
              <span key={c} className="kc-mono rounded-control bg-sunken px-2 py-1 text-sm text-ink">
                {c}
              </span>
            ))}
          </div>
        </div>
      ) : (
        <Input
          label="当前密码"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          hint="验证器丢了也能重新生成——否则「手机丢了」这件事会让你彻底出不去。"
        />
      )}
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
