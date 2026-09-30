/**
 * 通用件的交互契约：Modal 是全站弹窗的地基（Esc 关闭、不可关闭场景的
 * 防护），Button 的禁用与加载态是所有表单的提交语义。坏了会波及每一页。
 */
import { useState } from 'react'
import { describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'

import { Button } from '@/components/common/Button'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

describe('Modal', () => {
  it('open 时渲染标题与内容，onClose 可关闭（退场动画播完再卸载）', async () => {
    function Host() {
      const [open, setOpen] = useState(true)
      return (
        <Modal open={open} title="确认删除" onClose={() => setOpen(false)}>
          <p>该操作不可撤销</p>
        </Modal>
      )
    }
    render(<Host />)
    expect(screen.getByText('确认删除')).toBeInTheDocument()
    expect(screen.getByText('该操作不可撤销')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '关闭' }))
    // 关闭走 240ms 退场动画（§4.6），播完才卸载——立即断言内容还在。
    await waitFor(() => expect(screen.queryByText('该操作不可撤销')).not.toBeInTheDocument())
  })

  // 回归用例：从「关闭」翻到「打开」必须真的渲染出对话框。挂载同步曾用
  // 渲染期 setState 实现，在真实浏览器里永远打不开而 jsdom 全绿；真实
  // 浏览器侧的守卫在 E2E 冒烟（创建虚拟机向导可打开）。
  it('从关闭翻到打开会渲染内容（挂载回归）', () => {
    function Host() {
      const [open, setOpen] = useState(false)
      return (
        <>
          <button onClick={() => setOpen(true)}>打开</button>
          <Modal open={open} title="新建虚拟机" onClose={() => setOpen(false)}>
            <p>向导内容</p>
          </Modal>
        </>
      )
    }
    render(<Host />)
    expect(screen.queryByText('新建虚拟机')).not.toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '打开' }))
    expect(screen.getByText('新建虚拟机')).toBeInTheDocument()
    expect(screen.getByText('向导内容')).toBeInTheDocument()
  })

  it('按 Esc 触发关闭（键盘用户不被弹窗困住）', () => {
    const onClose = vi.fn()
    render(
      <Modal open title="标题" onClose={onClose}>
        <p>内容</p>
      </Modal>,
    )
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).toHaveBeenCalledTimes(1)
  })

  it('dismissable=false 时 Esc 不关闭——只出现一次的内容不能被误关', () => {
    const onClose = vi.fn()
    render(
      <Modal open title="凭证" dismissable={false} onClose={onClose}>
        <p>内容</p>
      </Modal>,
    )
    fireEvent.keyDown(document, { key: 'Escape' })
    expect(onClose).not.toHaveBeenCalled()
  })
})

describe('Button', () => {
  it('disabled 时不触发点击', () => {
    const onClick = vi.fn()
    render(
      <Button disabled onClick={onClick}>
        提交
      </Button>,
    )
    fireEvent.click(screen.getByRole('button', { name: '提交' }))
    expect(onClick).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: '提交' })).toBeDisabled()
  })
})

describe('StatusBadge', () => {
  it('渲染文本与语义色调', () => {
    const { container } = render(<StatusBadge tone="success">运行中</StatusBadge>)
    expect(screen.getByText('运行中')).toBeInTheDocument()
    // 语义色落在 class 上（设计令牌由 CSS 提供，测试只验证它被带上）。
    expect(container.firstElementChild?.className).toContain('success')
  })
})
