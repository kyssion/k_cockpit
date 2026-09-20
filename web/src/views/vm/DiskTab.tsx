/**
 * DiskTab 管理虚拟机的磁盘（F-2-06）。
 *
 * 三件**只有后端知道**的事必须在界面上说清楚，每一条都对应一种「用户会
 * 误解而查不出来」的情况：
 *
 *   1. **配置容量 ≠ 实际占用**。qcow2 是稀疏文件，一台配 100 GB 的机器可能
 *      只占 20 GB。只显示一个数字，用户要么以为盘快满了，要么以为配额算错
 *      了——而配额恰恰是按**配置容量**算的。
 *   2. **换总线要重启**，而且来宾里的设备路径会变。运行中改会让盘符漂移。
 *   3. **系统盘不可卸载**，这个判定来自节点而不是「vda 一定是系统盘」这种
 *      猜测——机型不同时那种猜测会错，而错的代价是机器起不来。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { vmApi, type VmDiskList, type VmDiskView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { formatBytes } from '@/utils/format'

export function DiskTab({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [attaching, setAttaching] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const list = useQuery({
    queryKey: ['vm-disks', vmID],
    queryFn: () => vmApi.disks(vmID),
  })

  const refresh = () => {
    setError('')
    void queryClient.invalidateQueries({ queryKey: ['vm-disks'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const change = useMutation({
    mutationFn: (input: { action: 'detach' | 'bus'; dev: string; bus?: string }) =>
      vmApi.changeDisk(vmID, input),
    onSuccess: () => {
      setNotice('已提交，可在任务中心查看进度')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const disks = list.data?.disks ?? []

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-medium text-ink">磁盘</h2>
          <p className="mt-1 text-sm text-ink-3">
            磁盘来自节点实时探测。**配置容量**计入存储配额，实际占用通常更小
            （qcow2 是稀疏文件）。
          </p>
        </div>
        <Button size="sm" onClick={() => setAttaching(true)}>
          挂载磁盘
        </Button>
      </header>

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-sm text-success">{notice}</p>
      )}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}

      {list.isPending ? (
        <PageLoading />
      ) : disks.length === 0 ? (
        <EmptyState
          title="节点没有返回磁盘"
          description="磁盘列表由节点上报。若刚升级过节点，可能还没实现这个操作——这不是这台虚拟机没有磁盘。"
        />
      ) : (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full text-left text-sm">
            <thead className="bg-sunken text-ink-3">
              <tr>
                <th className="px-3 py-2 font-normal">设备</th>
                <th className="px-3 py-2 font-normal">容量</th>
                <th className="px-3 py-2 font-normal">格式</th>
                <th className="px-3 py-2 font-normal">总线</th>
                <th className="px-3 py-2 font-normal">镜像路径</th>
                <th className="px-3 py-2 font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {disks.map((d) => (
                <tr key={d.dev} className="border-t border-line">
                  <td className="kc-mono px-3 py-2 text-ink-2">
                    {d.dev}
                    {d.is_system && <span className="ml-2 text-xs text-ink-3">系统盘</span>}
                  </td>
                  <td className="px-3 py-2">
                    <span className="kc-nums text-ink">{d.capacity_gb} GB</span>
                    <span className="ml-1.5 text-xs text-ink-3">
                      实际 {formatBytes(d.actual_bytes)}
                    </span>
                  </td>
                  <td className="px-3 py-2 text-ink-2">{d.format || '—'}</td>
                  <td className="px-3 py-2">
                    <BusSelect
                      disk={d}
                      options={list.data?.bus_options ?? []}
                      disabled={!d.can_change_bus}
                      title={d.can_change_bus ? undefined : d.change_bus_reason}
                      onChange={(bus) => change.mutate({ action: 'bus', dev: d.dev, bus })}
                    />
                  </td>
                  <td className="kc-mono max-w-[220px] truncate px-3 py-2 text-ink-3" title={d.source}>
                    {d.source || '—'}
                  </td>
                  <td className="whitespace-nowrap px-3 py-2">
                    <button
                      className="text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={!d.can_detach}
                      title={d.can_detach ? undefined : d.detach_reason}
                      onClick={() => change.mutate({ action: 'detach', dev: d.dev })}
                    >
                      卸载
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <p className="text-sm text-ink-3">
        换总线后需要重启虚拟机：来宾里的设备路径会变。卸载之后，若来宾里有对应
        的挂载点，请先在系统内卸载再操作。
      </p>

      <AttachModal
        open={attaching}
        vmID={vmID}
        list={list.data}
        onClose={() => setAttaching(false)}
        onDone={() => {
          setAttaching(false)
          setNotice('挂载任务已提交，设备名由节点分配')
          refresh()
        }}
        onError={(m) => {
          setAttaching(false)
          setError(m)
        }}
      />
    </div>
  )
}

/** 总线下拉框。可改时是控件，不可改时是**带原因的只读文本**。 */
function BusSelect({
  disk,
  options,
  disabled,
  title,
  onChange,
}: {
  disk: VmDiskView
  options: { value: string; label: string }[]
  disabled: boolean
  title?: string
  onChange: (bus: string) => void
}) {
  if (disabled) {
    return (
      <span className="text-ink-3" title={title}>
        {disk.bus || '—'}
      </span>
    )
  }
  return (
    <select
      value={disk.bus}
      onChange={(e) => onChange(e.target.value)}
      className="h-7 rounded-control border border-line-strong bg-sunken px-1.5 text-sm text-ink"
    >
      {options.map((o) => (
        <option key={o.value} value={o.value}>
          {o.label}
        </option>
      ))}
    </select>
  )
}

/**
 * AttachModal 挂载一块虚拟磁盘。
 *
 * 候选来自「我的存储」里的 disk 类文件，且**必须与虚拟机同节点**——磁盘是
 * 宿主机上的一份具体文件，跨节点的路径在那里根本不存在。
 */
function AttachModal({
  open,
  vmID,
  list,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  vmID: number
  list?: VmDiskList
  onClose: () => void
  onDone: () => void
  onError: (m: string) => void
}) {
  const [fileID, setFileID] = useState(0)

  const attach = useMutation({
    mutationFn: () => vmApi.changeDisk(vmID, { action: 'attach', file_id: fileID }),
    onSuccess: onDone,
    onError: (e) => onError(describe(e)),
  })

  const items = list?.attachables ?? []

  return (
    <Modal
      open={open}
      title="挂载磁盘"
      description="把「我的存储」里的虚拟磁盘挂到这台虚拟机上。设备名由节点分配。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={fileID === 0}
            loading={attach.isPending}
            onClick={() => attach.mutate()}
          >
            挂载
          </Button>
        </>
      }
    >
      {items.length === 0 ? (
        <p className="text-base text-ink-2">
          这个节点上还没有可挂载的虚拟磁盘。先到「我的存储」上传一个（类别选
          「虚拟磁盘」）。
        </p>
      ) : (
        <div className="flex flex-col gap-1">
          <label htmlFor="disk-file" className="text-sm text-ink-2">
            磁盘文件
          </label>
          <select
            id="disk-file"
            value={fileID}
            onChange={(e) => setFileID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value={0}>请选择…</option>
            {items.map((f) => (
              <option key={f.id} value={f.id}>
                {f.filename}（{formatBytes(f.size_bytes)}）
              </option>
            ))}
          </select>
          <p className="mt-1 text-xs text-ink-3">
            只列出同一节点上的文件。挂载后需要在来宾里分区、格式化并挂载——
            「来宾自动化」里的「附加磁盘并自动挂载」可以一次做完。
          </p>
        </div>
      )}
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
