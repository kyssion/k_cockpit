/**
 * UserAdminPage 管理用户（F-1-07）。
 *
 * 页面有两处刻意与普通增删改页面不同：
 *
 *   - **封禁前说清它会级联做什么**（撤销会话、停运行中的虚拟机），因为它
 *     不是一个只改一个字段的操作——那个人会立刻掉线，机器会被标记停止；
 *   - **封禁结果是两类**：操作成败，以及级联里没做成的部分。两者分开显示，
 *     否则「已封禁，但有 2 台虚拟机未能关机」会被渲染成一次失败。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  ROLE_LABEL,
  STATUS_LABEL,
  STATUS_TONE,
  userApi,
  type SetStatusResult,
  type UserView,
} from '@/api/useradmin'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { relativeTime } from '@/utils/format'

export function UserAdminPage() {
  const queryClient = useQueryClient()
  const [keyword, setKeyword] = useState('')
  const [query, setQuery] = useState('')
  const [createOpen, setCreateOpen] = useState(false)
  const [editing, setEditing] = useState<UserView | null>(null)
  const [confirmBan, setConfirmBan] = useState<UserView | null>(null)
  const [banResult, setBanResult] = useState<SetStatusResult | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const list = useQuery({
    queryKey: ['users', query],
    queryFn: () => userApi.list({ keyword: query, page_size: 100 }),
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['users'] })
    void queryClient.invalidateQueries({ queryKey: ['audit'] })
  }

  const setStatus = useMutation({
    mutationFn: (v: { id: number; status: 'active' | 'banned' }) =>
      userApi.setStatus(v.id, v.status),
    onSuccess: (result) => {
      setConfirmBan(null)
      setError('')
      if (result.warnings?.length) {
        // 有级联失败时弹一个**说明框**而不是错误框：操作是成功的。
        setBanResult(result)
      } else {
        setNotice(`已${result.user.status === 'banned' ? '封禁' : '解封'}「${result.user.username}」`)
      }
      refresh()
    },
    onError: (err) => {
      setConfirmBan(null)
      setError(describe(err))
    },
  })

  const remove = useMutation({
    mutationFn: (u: UserView) => userApi.remove(u.id),
    onSuccess: () => {
      setError('')
      setNotice('用户已删除')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (list.isPending) return <PageLoading />
  const items = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">用户管理</h1>
          <p className="mt-1 text-base text-ink-3">
            建号、封禁、删除。<span className="text-ink-2">封禁会级联</span>：撤销该用户的全部会话，并把正在运行的虚拟机标记停止。
          </p>
        </div>
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          新建用户
        </Button>
      </header>

      <div className="flex items-end gap-2">
        <div className="flex min-w-[220px] flex-1 flex-col gap-1">
          <label className="text-xs text-ink-3">搜索（用户名或邮箱）</label>
          <input
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && setQuery(keyword.trim())}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          />
        </div>
        <Button variant="secondary" size="sm" onClick={() => setQuery(keyword.trim())}>
          查询
        </Button>
        {query && (
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              setKeyword('')
              setQuery('')
            }}
          >
            清除
          </Button>
        )}
      </div>

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {items.length === 0 ? (
        <EmptyState title="没有匹配的用户" description="调整搜索条件，或新建一个账号。" />
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">用户名</th>
                <th className="px-4 py-2.5 font-medium">角色</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">最后登录</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((u) => (
                <tr key={u.id} className="border-t border-line">
                  <td className="px-4 py-2.5">
                    <span className="text-ink">{u.username}</span>
                    {u.totp_enabled && (
                      <span className="ml-2 rounded-pill bg-success/10 px-1.5 py-0.5 text-xs text-success">
                        已开二次验证
                      </span>
                    )}
                    {u.email && <span className="block text-xs text-ink-3">{u.email}</span>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{ROLE_LABEL[u.role]}</td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={STATUS_TONE[u.status]}>
                      {STATUS_LABEL[u.status]}
                    </StatusBadge>
                  </td>
                  <td className="px-4 py-2.5 text-ink-3">
                    {/* 「从没用过」是一个有用的信号：这个账号大概可以删了。 */}
                    {u.last_login_at ? relativeTime(u.last_login_at) : '从未登录'}
                  </td>
                  <td className="px-4 py-2.5">
                    <span className="flex flex-wrap gap-2">
                      <button
                        className="text-sm text-brand hover:underline"
                        onClick={() => setEditing(u)}
                      >
                        编辑
                      </button>
                      {u.status === 'banned' ? (
                        <button
                          className="text-sm text-ink-2 hover:underline"
                          disabled={setStatus.isPending}
                          onClick={() => setStatus.mutate({ id: u.id, status: 'active' })}
                        >
                          解封
                        </button>
                      ) : (
                        <button
                          className="text-sm text-warning hover:underline"
                          onClick={() => setConfirmBan(u)}
                        >
                          封禁
                        </button>
                      )}
                      <button
                        className="text-sm text-danger hover:underline"
                        disabled={remove.isPending}
                        onClick={() => remove.mutate(u)}
                      >
                        删除
                      </button>
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <CreateUserModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onDone={(name) => {
          setCreateOpen(false)
          setError('')
          setNotice(`已创建「${name}」，状态为待激活且要求首次登录改密`)
          refresh()
        }}
        onError={(msg) => {
          setCreateOpen(false)
          setError(msg)
        }}
      />

      <EditUserModal
        user={editing}
        onClose={() => setEditing(null)}
        onDone={() => {
          setEditing(null)
          setError('')
          setNotice('已保存')
          refresh()
        }}
        onError={(msg) => {
          setEditing(null)
          setError(msg)
        }}
      />

      {/* 封禁前说清它会级联做什么——它不是一个只改一个字段的操作。 */}
      <Modal
        open={confirmBan !== null}
        title={`封禁「${confirmBan?.username ?? ''}」`}
        description="封禁会立即执行，并级联做两件事："
        onClose={() => setConfirmBan(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setConfirmBan(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={setStatus.isPending}
              onClick={() => confirmBan && setStatus.mutate({ id: confirmBan.id, status: 'banned' })}
            >
              确认封禁
            </Button>
          </>
        }
      >
        <ul className="flex flex-col gap-1.5 text-base text-ink-2">
          <li>· 撤销该用户的全部登录会话——他手上已经打开的页面会立刻失效</li>
          <li>· 把该用户正在运行的虚拟机标记为停止</li>
        </ul>
        <p className="mt-2 text-sm text-ink-3">
          第二件事只改状态、不会立即在宿主机上关机（封禁不等待节点）。
          如果这一步没做成，封禁本身<span className="text-ink-2">仍然生效</span>，你会看到一条说明。
        </p>
      </Modal>

      {/* 级联失败：这不是错误，是一次成功但有遗留的操作。 */}
      <Modal
        open={banResult !== null}
        title={`已封禁「${banResult?.user.username ?? ''}」`}
        description="账号已经无法登录。以下级联动作没有完全成功："
        onClose={() => setBanResult(null)}
        footer={
          <Button size="sm" onClick={() => setBanResult(null)}>
            知道了
          </Button>
        }
      >
        <ul className="flex flex-col gap-2">
          {(banResult?.warnings ?? []).map((w) => (
            <li
              key={w}
              className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-base text-warning"
            >
              {w}
            </li>
          ))}
        </ul>
      </Modal>
    </div>
  )
}

function CreateUserModal({
  open,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  onClose: () => void
  onDone: (username: string) => void
  onError: (message: string) => void
}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [role, setRole] = useState<'tenant' | 'admin'>('tenant')
  const [email, setEmail] = useState('')

  const create = useMutation({
    mutationFn: () =>
      userApi.create({
        username: username.trim(),
        password,
        role,
        email: email.trim() || undefined,
      }),
    onSuccess: (u) => onDone(u.username),
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title="新建用户"
      description="新账号是「待激活」状态，并要求首次登录时修改密码——管理员给的是初始密码。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={username.trim() === '' || password.length < 8}
            loading={create.isPending}
            onClick={() => create.mutate()}
          >
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input label="用户名" value={username} onChange={(e) => setUsername(e.target.value)} />
        <Input
          label="初始密码"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          hint="至少 8 位。用户首次登录时会被要求改掉它。这个密码不会出现在审计流水里。"
        />
        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">角色</span>
          {(['tenant', 'admin'] as const).map((r) => (
            <label key={r} className="flex cursor-pointer items-start gap-2.5">
              <input
                type="radio"
                className="mt-1"
                name="role"
                checked={role === r}
                onChange={() => setRole(r)}
              />
              <span>
                <span className="block text-base text-ink">{ROLE_LABEL[r]}</span>
                <span className="block text-sm text-ink-3">
                  {r === 'tenant'
                    ? '只能操作自己名下的资源。'
                    : '可以管理节点、网络、其他用户等全局资源。'}
                </span>
              </span>
            </label>
          ))}
        </div>
        <Input label="邮箱（可选）" value={email} onChange={(e) => setEmail(e.target.value)} />
      </div>
    </Modal>
  )
}

function EditUserModal({
  user,
  onClose,
  onDone,
  onError,
}: {
  user: UserView | null
  onClose: () => void
  onDone: () => void
  onError: (message: string) => void
}) {
  const [email, setEmail] = useState('')
  const [remark, setRemark] = useState('')
  const [role, setRole] = useState<'tenant' | 'admin'>('tenant')

  // 打开时同步一次（带守卫的渲染期重置，避免 effect 里 setState）。
  const [seen, setSeen] = useState<number | null>(null)
  if (user && user.id !== seen) {
    setSeen(user.id)
    setEmail(user.email ?? '')
    setRemark(user.remark ?? '')
    setRole(user.role)
  }

  const save = useMutation({
    mutationFn: () =>
      userApi.update(user!.id, {
        email: email.trim(),
        remark: remark.trim(),
        role,
      }),
    onSuccess: onDone,
    onError: (err) => onError(describe(err)),
  })

  if (!user) return null

  return (
    <Modal
      open
      title={`编辑「${user.username}」`}
      description="这里不含密码——改密会让该用户的全部会话失效，因此它是一个单独的动作。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={save.isPending} onClick={() => save.mutate()}>
            保存
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input label="邮箱" value={email} onChange={(e) => setEmail(e.target.value)} />
        <Input label="备注" value={remark} onChange={(e) => setRemark(e.target.value)} />

        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">角色</span>
          <select
            value={role}
            onChange={(e) => setRole(e.target.value as 'tenant' | 'admin')}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value="tenant">租户</option>
            <option value="admin">管理员</option>
          </select>
          <p className="text-xs text-ink-3">
            不能修改自己的角色；也不能把最后一个可用的管理员降级——那之后就再也没人能进管理界面了。
          </p>
        </div>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
