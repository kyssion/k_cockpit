import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'

import { vmApi, type DiskAction } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Modal } from '@/components/common/Modal'
import { useIsAdmin } from '@/stores/session'
import { describe } from '@/utils/describe'

export function DeleteVmModal({
  open,
  vmID,
  vmName,
  onClose,
  onDeleted,
}: {
  open: boolean
  vmID: number
  vmName: string
  onClose: () => void
  onDeleted: () => void
}) {
  const [diskAction, setDiskAction] = useState<DiskAction | ''>('')
  const [error, setError] = useState('')
  // 强制删除（兜底）：勾上之后提交走 force-delete。只有管理员能勾——
  // 后端同样只放管理员，这里先藏掉是为了一般用户不必看到一个永远用不上
  // 的危险选项。
  const [force, setForce] = useState(false)
  const isAdmin = useIsAdmin()

  const remove = useMutation({
    mutationFn: () =>
      force
        ? vmApi.forceRemove(vmID, diskAction as DiskAction)
        : vmApi.remove(vmID, diskAction as DiskAction),
    onSuccess: () => {
      setDiskAction('')
      setForce(false)
      setError('')
      onDeleted()
    },
    onError: (err) => setError(describe(err)),
  })

  function handleClose() {
    setDiskAction('')
    setForce(false)
    setError('')
    onClose()
  }

  return (
    <Modal
      open={open}
      title={`删除虚拟机 ${vmName}`}
      description="删除后该虚拟机将不再出现在列表中，操作不可撤销。"
      onClose={handleClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={handleClose}>
            取消
          </Button>
          <Button
            variant="danger"
            size="sm"
            // 未选择磁盘处理方式时禁用提交：默认值是「用户最可能接受的选项」，
            // 而连盘删除的误操作代价是数据永久丢失，这个选择必须由用户做出。
            disabled={diskAction === ''}
            loading={remove.isPending}
            onClick={() => remove.mutate()}
          >
            确认删除
          </Button>
        </>
      }
    >
      <fieldset className="flex flex-col gap-3">
        <legend className="mb-1 text-sm font-medium text-ink-2">磁盘处理方式</legend>

        <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-line-strong px-3 py-2.5 hover:bg-sunken">
          <input
            type="radio"
            name="disk_action"
            className="mt-0.5"
            checked={diskAction === 'keep'}
            onChange={() => setDiskAction('keep')}
          />
          <span>
            <span className="block text-base text-ink">保留磁盘</span>
            <span className="block text-sm text-ink-3">
              磁盘保留在存储池中，可事后手动清理。不删除数据。
            </span>
          </span>
        </label>

        <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-line-strong px-3 py-2.5 hover:bg-sunken">
          <input
            type="radio"
            name="disk_action"
            className="mt-0.5"
            checked={diskAction === 'delete'}
            onChange={() => setDiskAction('delete')}
          />
          <span>
            <span className="block text-base text-danger">连同磁盘删除</span>
            <span className="block text-sm text-ink-3">
              磁盘数据将被永久删除，无法恢复。
            </span>
          </span>
        </label>

        <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-line-strong px-3 py-2.5 hover:bg-sunken">
          <input
            type="radio"
            name="disk_action"
            className="mt-0.5"
            checked={diskAction === 'transfer'}
            onChange={() => setDiskAction('transfer')}
          />
          <span>
            <span className="block text-base text-ink">转移到我的存储</span>
            <span className="block text-sm text-ink-3">
              磁盘文件会被搬进「我的存储 - 虚拟磁盘」，之后可以挂到别的机器上，
              或下载下来。虚拟机本身的记录仍移入回收站。
            </span>
          </span>
        </label>

        {isAdmin && (
          <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-warning/40 bg-warning/5 px-3 py-2.5">
            <input
              type="checkbox"
              className="mt-1"
              checked={force}
              onChange={(e) => setForce(e.target.checked)}
            />
            <span>
              <span className="block text-base text-warning">强制删除（兜底）</span>
              <span className="block text-sm text-ink-3">
                状态探测不出、常规删除失败的机器才需要勾选：节点会跳过常规清理，
                直接删除域定义并重启 libvirt——同节点上其它虚拟机会经历一次服务重启。
                需要二次验证。
              </span>
            </span>
          </label>
        )}

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </fieldset>
    </Modal>
  )
}
