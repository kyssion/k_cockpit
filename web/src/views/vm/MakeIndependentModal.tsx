import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { vmApi, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Modal } from '@/components/common/Modal'
import { describe } from '@/utils/describe'

/**
 * MakeIndependentModal 把链接克隆的磁盘合并为独立盘。
 *
 * 这个操作**本身没有可选项**（不像扩容要填数字），因此弹窗的职责只有两件：
 * 说清它要花什么代价（时间与空间），以及说清做完之后**能做什么之前做不到的
 * 事**——删掉那个模板。
 */
export function MakeIndependentModal({
  open,
  vm,
  onClose,
}: {
  open: boolean
  vm: VmView
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [error, setError] = useState('')

  const running = vm.status === 'running'

  const run = useMutation({
    mutationFn: () => vmApi.makeDisksIndependent(vm.id),
    onSuccess: () => {
      setError('')
      onClose()
      void queryClient.invalidateQueries({ queryKey: ['vm', vm.id] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (e) => setError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title="解除模板依赖"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={running}
            loading={run.isPending}
            onClick={() => run.mutate()}
          >
            开始合并
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <p className="text-base text-ink-2">
          这一步会把这台机器的磁盘从「模板之上的一层覆盖」合并成一个独立的完整镜像。合并之后它不再依赖任何模板，那个模板也就可以安全删除了。
        </p>

        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
          代价有两项，都取决于磁盘有多大：
          <span className="mt-1 block">· 需要时间——要把整个镜像复制一遍，期间机器保持关机</span>
          <span className="block">· 需要空间——合并后会占用一个完整镜像的空间，而不是现在的增量大小</span>
        </p>

        {/* 运行中**直接禁用**并说明原因，而不是等后端拒绝——那种报错会让
            人以为是自己哪里操作错了。 */}
        {running && (
          <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            这台虚拟机正在运行，需要先关机。合并要复制整个镜像，而运行中的机器
            还在往那层覆盖里写——复制出来的内容会与任何一个时刻都对不上。
          </p>
        )}

        {error && (
          <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </Modal>
  )
}
