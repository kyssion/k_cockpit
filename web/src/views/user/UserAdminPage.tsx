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
import { INVITE_STATUS_LABEL, INVITE_STATUS_TONE, inviteApi, type InviteView } from '@/api/invite'
import { vmApi, vmOwnerApi } from '@/api/vm'
import {
  ROLE_LABEL,
  STATUS_LABEL,
  STATUS_TONE,
  userApi,
  userExtraApi,
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
  const [assignTarget, setAssignTarget] = useState<UserView | null>(null)
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

  const ssh = useMutation({
    mutationFn: (vars: { id: number; enabled: boolean }) => userExtraApi.setSSHAccess(vars.id, vars.enabled),
    onSuccess: (r) => {
      setError('')
      setNotice(
        r.unavailable
          ? `已记录，但${r.unavailable}`
          : r.killed_sessions > 0
            ? `已生效，并结束了 ${r.killed_sessions} 个在线会话`
            : '已生效',
      )
      void queryClient.invalidateQueries({ queryKey: ['users'] })
    },
    onError: (err) => setError(describe(err)),
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
                <th className="px-4 py-2.5 font-medium">带宽</th>
                <th className="px-4 py-2.5 font-medium">SSH</th>
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
                  {/* 带宽是**速率型**配额：0 显示为「不限」而不是空白——
                      空白会被读成"没设置"，而不限是一个明确的选择。 */}
                  <td className="kc-nums px-4 py-2.5 text-ink-2">
                    {u.max_bandwidth_mbps > 0 ? `${u.max_bandwidth_mbps} Mbps` : '不限'}
                  </td>
                  <td className="px-4 py-2.5">
                    <button
                      className="text-sm text-ink-2 hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={ssh.isPending}
                      title={u.ssh_access_enabled ? '关闭后会把在线会话一并结束' : '允许该用户 SSH 登录宿主机'}
                      onClick={() => ssh.mutate({ id: u.id, enabled: !u.ssh_access_enabled })}
                    >
                      {u.ssh_access_enabled ? '已允许' : '已禁止'}
                    </button>
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
                      {/* 分配 VM：删除用户时"请先转移虚拟机"这个提示，
                          此前一直没有对应的动作可以点。 */}
                      <button
                        className="text-sm text-ink-2 hover:underline"
                        onClick={() => setAssignTarget(u)}
                      >
                        分配 VM
                      </button>
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

      <InviteSection />

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

      <AssignVMModal
        user={assignTarget}
        onClose={() => setAssignTarget(null)}
        onDone={(m) => {
          setAssignTarget(null)
          setError('')
          setNotice(m)
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

/**
 * AssignVMModal 把一台已有虚拟机指派给这个用户。
 *
 * 它补的是一个具体欠账：管理员删除用户时，如果该用户名下还有虚拟机，只能
 * 拒绝并提示"请先转移"——而"转移"这个动作此前没有入口可以点。
 *
 * 只列**未分配或属于别人的机器**：已经在这个用户名下的机器再分配一次是
 * 一个空操作，把它列出来只会让人以为那也是一个可选项。
 */
function AssignVMModal({
  user,
  onClose,
  onDone,
}: {
  user: UserView | null
  onClose: () => void
  onDone: (msg: string) => void
}) {
  const queryClient = useQueryClient()
  const [vmID, setVMID] = useState(0)
  const [error, setError] = useState('')

  const vms = useQuery({
    queryKey: ['vms'],
    queryFn: () => vmApi.list({ page_size: 100 }),
    enabled: user !== null,
  })
  const users = useQuery({
    queryKey: ['users'],
    queryFn: () => userApi.list({ page_size: 100 }),
    enabled: user !== null,
  })

  const assign = useMutation({
    mutationFn: () => vmOwnerApi.assignOwner(vmID, user?.id ?? 0),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['vms'] })
      onDone(`已将虚拟机分配给「${user?.username ?? ''}」`)
    },
    onError: (err) => setError(describe(err)),
  })

  const items = (vms.data?.items ?? []).filter((v) => v.owner_id !== user?.id)

  return (
    <Modal
      open={user !== null}
      title={`分配给「${user?.username ?? ''}」`}
      description="指派后，这台机器会出现在该用户的虚拟机列表里，并计入他的配额。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            loading={assign.isPending}
            disabled={vmID === 0}
            onClick={() => assign.mutate()}
          >
            分配
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <label className="flex flex-col gap-1">
          <span className="text-sm text-ink-2">虚拟机</span>
          <select
            value={vmID}
            onChange={(e) => setVMID(Number(e.target.value))}
            className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value={0}>请选择…</option>
            {items.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}（节点 #{v.node_id}）
              </option>
            ))}
          </select>
        </label>

        {items.length === 0 && !vms.isPending && (
          <p className="text-sm text-ink-3">没有可分配的虚拟机（它们都已属于这个用户）。</p>
        )}

        {/* 封禁中的用户不该再拿到机器：那会让"封禁"只生效在界面上。 */}
        {user?.status === 'banned' && (
          <p className="text-sm text-warning">该用户已被封禁，分配会被服务端拒绝。</p>
        )}

        {error && <p className="text-sm text-danger">{error}</p>}
        {users.isError && <p className="text-sm text-ink-3">用户列表加载失败，不影响分配。</p>}
      </div>
    </Modal>
  )
}


/**
 * InviteSection 邀请注册（F-1-10）。
 *
 * 链接只在**创建与重发时返回一次**，之后列表里看不到它 —— 这是刻意的：链接会出现在
 * 邮件与聊天记录里（都是不可控的地方），库里只留哈希，重发还会把旧链接作废。
 */
function InviteSection() {
  const queryClient = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [link, setLink] = useState('')
  const [error, setError] = useState('')

  const list = useQuery({ queryKey: ['invites'], queryFn: inviteApi.list })

  const revoke = useMutation({
    mutationFn: (id: number) => inviteApi.revoke(id),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['invites'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const resend = useMutation({
    mutationFn: (id: number) => inviteApi.resend(id),
    onSuccess: (v) => {
      setError('')
      if (v.link) setLink(v.link)
      void queryClient.invalidateQueries({ queryKey: ['invites'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const items = list.data?.items ?? []

  return (
    <section className="rounded-card border border-line">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-2.5">
        <div>
          <h2 className="text-sm font-medium text-ink-2">邀请注册</h2>
          <p className="mt-0.5 text-xs text-ink-3">
            发出一条有期限的邀请链接；受邀人自己设密码，管理员从头到尾不知道它。
          </p>
        </div>
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          新建邀请
        </Button>
      </div>

      <div className="px-4 py-3">
        {error && (
          <p role="alert" className="mb-2 rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}

        {/* 刚生成的链接必须在显眼处：这是唯一一次能看到它的机会。 */}
        {link && (
          <div className="mb-3 flex flex-col gap-1 rounded-control bg-success/10 px-3 py-2">
            <span className="text-sm text-success">
              邀请链接已生成，请复制并转交给对方（仅显示一次）。
            </span>
            <code className="kc-mono break-all text-xs text-ink-2">{link}</code>
          </div>
        )}

        {items.length === 0 ? (
          <p className="text-base text-ink-3">还没有邀请记录。</p>
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="text-left text-xs text-ink-3">
                <th className="px-2 py-1.5 font-normal">邮箱</th>
                <th className="px-2 py-1.5 font-normal">角色</th>
                <th className="px-2 py-1.5 font-normal">状态</th>
                <th className="px-2 py-1.5 font-normal">有效期</th>
                <th className="px-2 py-1.5 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((it) => (
                <tr key={it.id} className="border-t border-line">
                  <td className="px-2 py-1.5 text-ink">{it.email}</td>
                  <td className="px-2 py-1.5 text-ink-2">{it.role === 'admin' ? '管理员' : '租户'}</td>
                  <td className="px-2 py-1.5">
                    <StatusBadge tone={INVITE_STATUS_TONE[it.status]}>
                      {INVITE_STATUS_LABEL[it.status]}
                    </StatusBadge>
                  </td>
                  <td className="px-2 py-1.5 text-ink-3">{relativeTime(it.expires_at)}</td>
                  <td className="px-2 py-1.5 text-right">
                    {it.status !== 'accepted' && (
                      <>
                        <button
                          className="text-sm text-brand hover:underline disabled:text-ink-3 disabled:no-underline"
                          disabled={resend.isPending}
                          onClick={() => resend.mutate(it.id)}
                        >
                          重发
                        </button>
                        <button
                          className="ml-3 text-sm text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
                          disabled={revoke.isPending}
                          onClick={() => revoke.mutate(it.id)}
                        >
                          撤销
                        </button>
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <CreateInviteModal
        open={createOpen}
        onClose={() => setCreateOpen(false)}
        onCreated={(v) => {
          setCreateOpen(false)
          setError('')
          if (v.link) setLink(v.link)
          void queryClient.invalidateQueries({ queryKey: ['invites'] })
        }}
      />
    </section>
  )
}

/**
 * CreateInviteModal 新建邀请。
 *
 * 配额在这里一起定：先给额度再让人进来，而不是进来之后再谈额度 —— 那时他已经能建
 * 机器了。
 */
function CreateInviteModal({
  open,
  onClose,
  onCreated,
}: {
  open: boolean
  onClose: () => void
  onCreated: (v: InviteView) => void
}) {
  const [email, setEmail] = useState('')
  const [role, setRole] = useState('tenant')
  const [quotaEnabled, setQuotaEnabled] = useState(false)
  const [quotaGB, setQuotaGB] = useState('')
  const [remark, setRemark] = useState('')
  const [error, setError] = useState('')

  const create = useMutation({
    mutationFn: () =>
      inviteApi.create({
        email: email.trim(),
        role,
        quota_enabled: quotaEnabled,
        quota_bytes: quotaEnabled ? Number(quotaGB) * 1024 * 1024 * 1024 : 0,
        remark: remark.trim() || undefined,
      }),
    onSuccess: onCreated,
    onError: (err) => setError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title="新建邀请"
      description="生成一条邀请链接，发给受邀人让他自己完成注册。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            loading={create.isPending}
            disabled={!email.trim() || (quotaEnabled && !(Number(quotaGB) > 0))}
            onClick={() => create.mutate()}
          >
            生成邀请
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Input
          label="受邀人邮箱"
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="someone@example.com"
        />

        <label className="flex flex-col gap-1">
          <span className="text-sm text-ink-2">角色</span>
          <select
            value={role}
            onChange={(e) => setRole(e.target.value)}
            className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value="tenant">租户</option>
            <option value="admin">管理员</option>
          </select>
        </label>

        <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={quotaEnabled}
            onChange={(e) => setQuotaEnabled(e.target.checked)}
          />
          <span>
            同时设定存储配额
            <span className="block text-xs text-ink-3">额度在受邀人进来之前就定好；不设定表示不限额。</span>
          </span>
        </label>

        {quotaEnabled && (
          <Input
            label="配额（GB）"
            type="number"
            value={quotaGB}
            onChange={(e) => setQuotaGB(e.target.value)}
            placeholder="例如 100"
          />
        )}

        <Input label="备注（可选）" value={remark} onChange={(e) => setRemark(e.target.value)} />

        {error && <p role="alert" className="text-sm text-danger">{error}</p>}

        <p className="text-xs text-ink-3">
          链接默认三天有效。过期后可「重发」——重发会生成新链接并把旧链接作废。
        </p>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
