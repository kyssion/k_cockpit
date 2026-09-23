/**
 * CDROMTab 管理虚拟机的光驱（可以有多个）。
 *
 * 界面上要把三件**只有后端才知道**的事说清楚——每一条都对应一种「用户会
 * 误解而查不出来」的情况：
 *
 *   1. **弹出 ≠ 移除**：弹出后光驱仍在，来宾里看得到一个空的托盘。因此列表
 *      里「空的光驱」与"没有光驱"是两种不同的行，而不是都显示成空白。
 *   2. **换盘之后来宾通常看不到新介质**（它缓存了介质信息），提示可以重新
 *      挂载或重启。不说的话用户会以为换盘失败而反复重试。
 *   3. **换总线几乎一定需要重启**。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { CDROM_BUSES, cdromApi, type CDROMView } from '@/api/cdrom'
import { ApiError, NetworkError } from '@/api/client'
import { userStorageApi } from '@/api/userstorage'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'

export function CDROMTab({ vmID, nodeID }: { vmID: number; nodeID: number }) {
  const queryClient = useQueryClient()
  const [adding, setAdding] = useState(false)
  const [loading, setLoading] = useState<CDROMView | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const list = useQuery({
    queryKey: ['vm-cdroms', vmID],
    queryFn: () => cdromApi.list(vmID),
  })

  const refresh = () => {
    setError('')
    void queryClient.invalidateQueries({ queryKey: ['vm-cdroms'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const eject = useMutation({
    mutationFn: (c: CDROMView) => cdromApi.eject(vmID, c.id),
    onSuccess: () => {
      setNotice('已弹出。光驱仍在——来宾里还能看到那个设备，只是里而没有盘')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const remove = useMutation({
    mutationFn: (c: CDROMView) => cdromApi.remove(vmID, c.id),
    onSuccess: () => {
      setNotice('已移除光驱。其余光驱的序号没有重排——设备名不会变')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const setBus = useMutation({
    mutationFn: (v: { c: CDROMView; bus: string }) => cdromApi.setBus(vmID, v.c.id, v.bus),
    onSuccess: () => {
      setNotice('已更换总线类型。这通常需要重启虚拟机才生效')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const items = list.data?.items ?? []
  const full = items.length >= (items[0]?.max_cdroms ?? 4)

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-medium text-ink">光驱</h2>
          <p className="mt-1 text-sm text-ink-3">
            一台虚拟机可以有多个光驱。**弹出**只是取下介质，光驱还在；**移除**
            才是把设备摘掉。
          </p>
        </div>
        <Button size="sm" disabled={full} onClick={() => setAdding(true)}>
          添加光驱
        </Button>
      </header>

      {full && (
        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
          光驱数量已达上限。每多一个光驱就多占一条总线通道，填满之后加不上磁盘——而那时的报错来自虚拟化层，与「你加了太多光驱」联系不起来。
        </p>
      )}
      {notice && <p className="rounded-control bg-success/10 px-3 py-2 text-sm text-success">{notice}</p>}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}

      {list.isPending ? (
        <PageLoading />
      ) : items.length === 0 ? (
        <EmptyState
          title="没有光驱"
          description="添加一个光驱之后可以挂载 ISO 安装镜像。"
        />
      ) : (
        <div className="overflow-hidden rounded-card border border-line">
          <table className="w-full text-left text-sm">
            <thead className="bg-sunken text-ink-3">
              <tr>
                <th className="px-3 py-2 font-normal">设备</th>
                <th className="px-3 py-2 font-normal">介质</th>
                <th className="px-3 py-2 font-normal">总线</th>
                <th className="px-3 py-2 font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((c) => (
                <tr key={c.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="kc-mono px-3 py-2 text-ink-2">{c.device}</td>
                  <td className="px-3 py-2">
                    {/* **空的光驱与没有光驱是两回事**：前者在来宾里看得到一个
                        空的托盘，后者连设备都没有。因此这里明确写出「未放盘」
                        而不是留空。 */}
                    {c.loaded ? (
                      <span className="text-ink-2">{c.iso_name || `#${c.iso_file_id}`}</span>
                    ) : (
                      <span className="text-ink-3">未放盘（托盘是空的）</span>
                    )}
                  </td>
                  <td className="px-3 py-2">
                    <select
                      value={c.bus}
                      onChange={(e) => setBus.mutate({ c, bus: e.target.value })}
                      className="h-7 rounded-control border border-line-strong bg-sunken px-1.5 text-sm text-ink"
                    >
                      {CDROM_BUSES.map((b) => (
                        <option key={b.value} value={b.value}>
                          {b.label}
                        </option>
                      ))}
                    </select>
                  </td>
                  <td className="whitespace-nowrap px-3 py-2 text-sm">
                    <button className="text-primary hover:underline" onClick={() => setLoading(c)}>
                      {c.loaded ? '换盘' : '放盘'}
                    </button>
                    <button
                      className="ml-3 text-primary hover:underline disabled:text-ink-3"
                      disabled={!c.loaded}
                      onClick={() => eject.mutate(c)}
                    >
                      弹出
                    </button>
                    {/* 移除与弹出并排，但文案与颜色都不同——它们的结果不同。 */}
                    <button
                      className="ml-3 text-danger hover:underline"
                      onClick={() => remove.mutate(c)}
                    >
                      移除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <p className="text-sm text-ink-3">
        换盘之后，来宾里可能仍显示旧的那一张——多数系统会缓存介质信息。在系统里
        重新挂载一次，或者重启虚拟机即可。
      </p>

      <AddModal
        open={adding}
        vmID={vmID}
        nodeID={nodeID}
        onClose={() => setAdding(false)}
        onDone={() => {
          setAdding(false)
          setNotice('光驱已添加')
          refresh()
        }}
        onError={(m) => {
          setAdding(false)
          setError(m)
        }}
      />

      <LoadModal
        vmID={vmID}
        nodeID={nodeID}
        target={loading}
        onClose={() => setLoading(null)}
        onDone={() => {
          setLoading(null)
          setNotice('已更换介质。来宾里可能需要重新挂载或重启才看到新盘')
          refresh()
        }}
        onError={(m) => {
          setLoading(null)
          setError(m)
        }}
      />
    </div>
  )
}

function LoadModal({
  vmID,
  nodeID,
  target,
  onClose,
  onDone,
  onError,
}: {
  vmID: number
  nodeID: number
  target: CDROMView | null
  onClose: () => void
  onDone: () => void
  onError: (m: string) => void
}) {
  const [isoID, setISOID] = useState<number>(0)

  // 复用**用户存储**的文件接口（category=iso），不另造一个「列出 ISO」的
  // 接口：那是同一份数据，两个入口会各自演化出不同的权限与过滤规则。
  const isos = useQuery({
    queryKey: ['iso-files', nodeID],
    queryFn: () => userStorageApi.listFiles(nodeID, 'iso'),
    enabled: target !== null,
  })

  const load = useMutation({
    mutationFn: () => cdromApi.load(vmID, target?.id ?? 0, isoID),
    onSuccess: onDone,
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={target !== null}
      title={`${target?.device ?? ''} 选择介质`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" disabled={isoID === 0} loading={load.isPending} onClick={() => load.mutate()}>
            {target?.loaded ? '换盘' : '放入'}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">安装镜像</label>
          <select
            value={isoID}
            onChange={(e) => setISOID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value={0}>请选择…</option>
            {(isos.data?.items ?? []).map((f) => (
              <option key={f.id} value={f.id}>
                {f.filename}
              </option>
            ))}
          </select>
          {(isos.data?.items ?? []).length === 0 && (
            <p className="text-xs text-ink-3">
              这个节点上还没有安装镜像。先到「我的存储」里上传一个 ISO。
            </p>
          )}
        </div>
        <p className="rounded-control bg-sunken px-3 py-2 text-xs text-ink-3">
          只列出与这台虚拟机同一节点上的镜像——光驱是宿主机上的设备，而文件
          在节点的存储里，跨节点的路径在这里根本不存在。
        </p>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}

/**
 * AddModal 添加一个光驱。
 *
 * 新光驱的**序号由服务端决定**（取当前最大值 +1，而不是已有数量）——
 * 用数量的话，删掉 0 号之后序号会与现有的一条撞上。界面只负责选介质与总线。
 */
function AddModal({
  open,
  vmID,
  nodeID,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  vmID: number
  nodeID: number
  onClose: () => void
  onDone: () => void
  onError: (m: string) => void
}) {
  const [isoID, setISOID] = useState(0)
  const [bus, setBus] = useState('sata')

  const isos = useQuery({
    queryKey: ['iso-files', nodeID],
    queryFn: () => userStorageApi.listFiles(nodeID, 'iso'),
    enabled: open,
  })

  const add = useMutation({
    mutationFn: () => cdromApi.attach(vmID, isoID, bus),
    onSuccess: onDone,
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title="添加光驱"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={add.isPending} onClick={() => add.mutate()}>
            添加
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">安装镜像（可留空）</label>
          <select
            value={isoID}
            onChange={(e) => setISOID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value={0}>暂不放盘（空的托盘）</option>
            {(isos.data?.items ?? []).map((f) => (
              <option key={f.id} value={f.id}>
                {f.filename}
              </option>
            ))}
          </select>
          <p className="text-xs text-ink-3">
            留空表示先加一个**空的光驱**——它在来宾里看得到一个空的托盘，
            与「没有光驱」不是同一件事。
          </p>
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">总线类型</label>
          <select
            value={bus}
            onChange={(e) => setBus(e.target.value)}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            {CDROM_BUSES.map((b) => (
              <option key={b.value} value={b.value}>
                {b.label}
              </option>
            ))}
          </select>
        </div>
      </div>
    </Modal>
  )
}
