/**
 * UserAdminPage 的删除契约：**点「删除」只是打开确认弹窗**，不发请求。
 *
 * 这条用例守的是一个真实的事故面：删除是软删除、没有撤销入口，而此前那一行
 * 按钮点下去就直接 `remove.mutate()`——误点一个人就没了。同页的「封禁」早已
 * 有确认弹窗，删除却没有，这个不对称正是漏点所在。
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { UserView } from '@/api/useradmin'
import { UserAdminPage } from '@/views/user/UserAdminPage'

const listUsers = vi.fn()
const removeUser = vi.fn()

const alice: UserView = {
  id: 7,
  username: 'alice',
  role: 'tenant',
  status: 'active',
  email: 'alice@example.com',
  totp_enabled: false,
  created_at: '2024-01-01T00:00:00Z',
  max_bandwidth_mbps: 0,
  ssh_access_enabled: false,
}

vi.mock('@/api/useradmin', () => ({
  ROLE_LABEL: { admin: '管理员', tenant: '租户' },
  STATUS_LABEL: { pending: '待激活', active: '正常', banned: '已封禁' },
  STATUS_TONE: { pending: 'warning', active: 'success', banned: 'danger' },
  userApi: {
    list: (...args: unknown[]) => listUsers(...args),
    create: vi.fn(),
    update: vi.fn(),
    setStatus: vi.fn(),
    remove: (...args: unknown[]) => removeUser(...args),
  },
  userExtraApi: { setSSHAccess: vi.fn() },
}))

vi.mock('@/api/invite', () => ({
  INVITE_STATUS_LABEL: {
    pending: '待接受',
    accepted: '已接受',
    revoked: '已撤销',
    expired: '已过期',
  },
  INVITE_STATUS_TONE: { pending: 'warning', accepted: 'success', revoked: 'danger', expired: 'idle' },
  inviteApi: {
    list: vi.fn(async () => ({ items: [] })),
    create: vi.fn(),
    revoke: vi.fn(),
    resend: vi.fn(),
  },
}))

vi.mock('@/api/vm', () => ({
  vmApi: { list: vi.fn(async () => ({ items: [], pagination: { total: 0 } })) },
  vmOwnerApi: { assignOwner: vi.fn() },
}))

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <UserAdminPage />
    </QueryClientProvider>,
  )
}

describe('UserAdminPage 删除用户', () => {
  beforeEach(() => {
    listUsers.mockResolvedValue({ items: [alice], total: 1, page: 1, page_size: 100 })
    removeUser.mockReset()
    removeUser.mockResolvedValue({ deleted: true })
  })

  it('点「删除」只打开确认弹窗，不直接发请求', async () => {
    renderPage()
    await screen.findByText('alice')

    fireEvent.click(screen.getByRole('button', { name: '删除' }))

    // 弹窗必须出现，且请求一条都没发出去。
    expect(await screen.findByText('删除用户「alice」')).toBeInTheDocument()
    expect(removeUser).not.toHaveBeenCalled()
    // 后果要写清：软删除、不可撤销、名下有机器时删不掉。
    expect(screen.getByText(/用户名仍保留在审计记录中/)).toBeInTheDocument()
    expect(screen.getByText(/名下仍有虚拟机时无法删除/)).toBeInTheDocument()
  })

  it('确认后才真的删除', async () => {
    renderPage()
    await screen.findByText('alice')

    fireEvent.click(screen.getByRole('button', { name: '删除' }))
    fireEvent.click(await screen.findByRole('button', { name: '确认删除' }))

    await waitFor(() => expect(removeUser).toHaveBeenCalledWith(alice.id))
  })

  it('取消不会删除', async () => {
    renderPage()
    await screen.findByText('alice')

    fireEvent.click(screen.getByRole('button', { name: '删除' }))
    fireEvent.click(await screen.findByRole('button', { name: '取消' }))

    // 弹窗走 240ms 退场动画，等它消失再断言。
    await waitFor(() => expect(screen.queryByText('删除用户「alice」')).not.toBeInTheDocument())
    expect(removeUser).not.toHaveBeenCalled()
  })
})
