import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { vmApi, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { describe } from '@/utils/describe'

/**
 * 关机状态下的磁盘扩容。
 *
 * 几处必须在按下按钮**之前**说清的事：
 *
 *   - **只能扩，不能缩**。缩容会丢数据——镜像变小后文件系统里超出新边界的
 *     块还在原地，而文件系统不再知道自己拥有它们。因此界面上只给扩容入口，
 *     不给一个点了会被拒的缩容框。
 *   - **运行中的机器请走「来宾自动化」**。那条路会顺带在来宾里扩好文件系统；
 *     这条只扩宿主机侧，**来宾里的分区要自己扩**。
 */
export function ResizeDiskModal({
  open,
  vm,
  onClose,
}: {
  open: boolean
  vm: VmView
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [size, setSize] = useState('')
  const [error, setError] = useState('')
  const [result, setResult] = useState<{ old_gb: number; new_gb: number } | null>(null)

  const running = vm.status === 'running'
  const newGB = Number(size) || 0
  // 只接受**扩大**：小于等于当前容量时禁用提交，而不是等后端拒绝——
  // 那种报错会让人以为是自己填错了格式。
  const valid = newGB > vm.disk_gb && newGB <= 65536

  const resize = useMutation({
    mutationFn: () => vmApi.resizeDisk(vm.id, newGB),
    onSuccess: (r) => {
      setResult({ old_gb: r.old_gb, new_gb: r.new_gb })
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['vm', vm.id] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (e) => setError(describe(e)),
  })

  const close = () => {
    setSize('')
    setResult(null)
    setError('')
    onClose()
  }

  return (
    <Modal
      open={open}
      title="扩容磁盘"
      description="把系统盘扩大。宿主机侧扩完之后，来宾里还需要扩分区与文件系统。"
      onClose={close}
      footer={
        result ? (
          <Button size="sm" onClick={close}>
            知道了
          </Button>
        ) : (
          <>
            <Button variant="secondary" size="sm" onClick={close}>
              取消
            </Button>
            <Button
              size="sm"
              disabled={!valid || running}
              loading={resize.isPending}
              onClick={() => resize.mutate()}
            >
              扩容
            </Button>
          </>
        )
      }
    >
      {result ? (
        <div className="flex flex-col gap-2">
          <p className="text-base text-ink">
            宿主机侧已从 {result.old_gb} GB 扩到 {result.new_gb} GB。
          </p>
          {/* **这一步不能省**：宿主机侧扩完只是「盘子变大了」，而操作系统
              看到的仍然是原来的分区。不提醒的话，用户会以为扩容失败。 */}
          <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            来宾里还需要扩分区与文件系统，否则系统看到的仍是原来的容量。
            启动虚拟机后在来宾里执行：
            <span className="kc-mono mt-1 block">growpart /dev/vda 1</span>
            <span className="kc-mono block">resize2fs /dev/vda1</span>
            Windows 用「磁盘管理」里的「扩展卷」。
          </p>
        </div>
      ) : (
        <div className="flex flex-col gap-3">
          <Input
            label="新容量（GB）"
            value={size}
            onChange={(e) => setSize(e.target.value)}
            hint={`当前 ${vm.disk_gb} GB。只能扩大——缩容会让文件系统里超出新边界的块失去归属，而它自己不知道，下一次写入就可能覆盖别的内容。需要更小的盘请新建一台再迁移过去。`}
          />

          {/* 运行中**直接禁用并指向另一个入口**，而不是等后端拒绝：
              那种报错会让人以为是自己填错了什么。 */}
          {running && (
            <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
              这台虚拟机正在运行。运行中的扩容请用「来宾自动化」页签里的
              「扩容磁盘」——它会**顺带在来宾里扩好文件系统**，一步到位。
              这里的入口用于关机状态的机器。
            </p>
          )}

          {error && (
            <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
              {error}
            </p>
          )}
        </div>
      )}
    </Modal>
  )
}
