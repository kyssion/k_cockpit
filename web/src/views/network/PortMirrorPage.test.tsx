/**
 * PortMirrorPage 的 VLAN 契约：节点侧镜像**原样复制帧，始终保留原始 VLAN
 * 标签**，本版本不做剥离。
 *
 * 因此这里曾有过的「保留 VLAN 标签」复选框是一个谎言：它让用户以为默认已经
 * 剥掉。用例把替换后的状态钉住——没有勾选框、界面上明确写出真实行为、提交时
 * 不再发送 `vlan_preserve`（服务端会归一化为 true，显式传 false 会被拒绝）。
 */
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { PortMirrorPage } from '@/views/network/PortMirrorPage'

const listMirrors = vi.fn()
const createMirror = vi.fn()
const updateMirror = vi.fn()

vi.mock('@/api/portmirror', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/portmirror')>()
  return {
    ...actual,
    portMirrorApi: {
      ...actual.portMirrorApi,
      list: (...args: unknown[]) => listMirrors(...args),
      create: (...args: unknown[]) => createMirror(...args),
      update: (...args: unknown[]) => updateMirror(...args),
    },
  }
})

vi.mock('@/api/node', () => ({
  nodeApi: { list: vi.fn(async () => [{ id: 5, name: 'node-5' }]) },
}))

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={queryClient}>
      <PortMirrorPage />
    </QueryClientProvider>,
  )
}

describe('PortMirrorPage 的 VLAN 标签', () => {
  beforeEach(() => {
    listMirrors.mockResolvedValue({ items: [] })
    createMirror.mockReset()
    createMirror.mockResolvedValue({ id: 1 })
  })

  it('新建表单不再给出「保留 VLAN 标签」开关，而是写清真实行为', async () => {
    renderPage()
    fireEvent.click(await screen.findByRole('button', { name: '新建规则' }))

    expect(
      screen.getByText('镜像帧原样复制，保留原始 VLAN 标签；本版本不做 VLAN 剥离。'),
    ).toBeInTheDocument()
    // 选择不存在，就不能有勾选框。
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
    expect(screen.queryByText(/默认剥掉/)).not.toBeInTheDocument()
  })

  it('提交时不发送 vlan_preserve', async () => {
    renderPage()
    fireEvent.click(await screen.findByRole('button', { name: '新建规则' }))

    // 文本框依次是：名称（单行）、来源接口、目标交换机。
    const areas = screen.getAllByRole('textbox')
    const sources = areas[1]
    const targets = areas[2]
    if (!sources || !targets) throw new Error('新建镜像的表单缺少来源/目标输入框')
    fireEvent.change(sources, { target: { value: 'eth0' } })
    fireEvent.change(targets, { target: { value: 'vswitch0' } })
    fireEvent.click(screen.getByRole('button', { name: '创建' }))

    await waitFor(() => expect(createMirror).toHaveBeenCalledTimes(1))
    const [nodeID, payload] = createMirror.mock.calls[0] as [number, Record<string, unknown>]
    expect(nodeID).toBe(5)
    expect(payload).toMatchObject({ source_ports: ['eth0'], target_switches: ['vswitch0'] })
    expect('vlan_preserve' in payload).toBe(false)
  })
})
