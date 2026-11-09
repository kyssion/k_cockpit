/**
 * SecurityGroupPage 的删组契约：**点「删除组」只是打开确认弹窗**，不发请求。
 *
 * 删组是连带删除——组内规则一起没，且不可撤销；此外仍被网口使用时服务端会
 * 409。确认弹窗要同时把这两件事说清，并且规则条数只能来自服务端（有就写具体
 * 数字，没有就只说通用后果，不编）。
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { GroupView, RuleView } from '@/api/securitygroup'
import { SecurityGroupPage } from '@/views/network/SecurityGroupPage'

const listGroups = vi.fn()
const listRules = vi.fn()
const removeGroup = vi.fn()

const group: GroupView = {
  id: 3,
  node_id: 1,
  name: 'web',
  is_default: false,
  rule_count: 2,
  attached_count: 0,
  created_at: '2024-01-01T00:00:00Z',
}

const rules: RuleView[] = [
  {
    id: 11,
    group_id: 3,
    direction: 'ingress',
    protocol: 'tcp',
    port_start: 80,
    port_end: 80,
    target_type: 'cidr',
    target_value: '0.0.0.0/0',
    address_family: 'inet',
    priority: 0,
    created_at: '2024-01-01T00:00:00Z',
  },
  {
    id: 12,
    group_id: 3,
    direction: 'ingress',
    protocol: 'tcp',
    port_start: 443,
    port_end: 443,
    target_type: 'cidr',
    target_value: '0.0.0.0/0',
    address_family: 'inet',
    priority: 1,
    created_at: '2024-01-01T00:00:00Z',
  },
]

// 标签表与 ruleText 用真实实现：这里要测的是「删除前要不要确认」，
// 不是把展示逻辑在测试里再写一遍。
vi.mock('@/api/securitygroup', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/securitygroup')>()
  return {
    ...actual,
    securityGroupApi: {
      ...actual.securityGroupApi,
      list: (...args: unknown[]) => listGroups(...args),
      listRules: (...args: unknown[]) => listRules(...args),
      remove: (...args: unknown[]) => removeGroup(...args),
    },
  }
})

vi.mock('@/api/node', () => ({
  nodeApi: { list: vi.fn(async () => [{ id: 1, name: 'node-1' }]) },
}))

vi.mock('@/api/vm', () => ({
  vmApi: { list: vi.fn(async () => ({ items: [], pagination: { total: 0 } })) },
}))

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <SecurityGroupPage />
    </QueryClientProvider>,
  )
}

/** 选中组，让详情面板（规则列表 + 删除组按钮）出现。 */
async function openGroupDetail() {
  fireEvent.click(await screen.findByRole('button', { name: /web/ }))
  await screen.findByText('web 的规则')
}

describe('SecurityGroupPage 删除安全组', () => {
  beforeEach(() => {
    listGroups.mockResolvedValue({ items: [group] })
    listRules.mockResolvedValue({ items: rules })
    removeGroup.mockReset()
    removeGroup.mockResolvedValue({ deleted: true })
  })

  it('点「删除组」只打开确认弹窗，不直接发请求', async () => {
    renderPage()
    await openGroupDetail()

    fireEvent.click(screen.getByRole('button', { name: '删除组' }))

    expect(await screen.findByText('删除安全组「web」')).toBeInTheDocument()
    // 条数来自服务端已加载的规则，不是编的。
    expect(screen.getByText(/将同时删除 2 条规则/)).toBeInTheDocument()
    // 连带删除 + 不可撤销 + 被挂载时删不掉，三件事都要说到。
    expect(screen.getByText(/该组仍被网口使用时无法删除/)).toBeInTheDocument()
    expect(screen.getByText(/不会进入任何回收站/)).toBeInTheDocument()
    expect(removeGroup).not.toHaveBeenCalled()
  })

  it('确认后才真的删除，且详情面板随列表收起', async () => {
    renderPage()
    await openGroupDetail()

    fireEvent.click(screen.getByRole('button', { name: '删除组' }))
    // 删除成功后列表不再包含这个组，详情面板按派生逻辑自然收起。
    listGroups.mockResolvedValue({ items: [] })
    fireEvent.click(await screen.findByRole('button', { name: '确认删除' }))

    await waitFor(() => expect(removeGroup).toHaveBeenCalledWith(group.id))
    await waitFor(() => expect(screen.queryByText('web 的规则')).not.toBeInTheDocument())
  })

  it('取消不会删除', async () => {
    renderPage()
    await openGroupDetail()

    fireEvent.click(screen.getByRole('button', { name: '删除组' }))
    fireEvent.click(await screen.findByRole('button', { name: '取消' }))

    await waitFor(() => expect(screen.queryByText('删除安全组「web」')).not.toBeInTheDocument())
    expect(removeGroup).not.toHaveBeenCalled()
  })
})
