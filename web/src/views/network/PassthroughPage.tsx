/**
 * PassthroughPage 管理 PCIe 直通设备（GPU 等）。
 *
 * 页面上最要紧的一件事是**同组**：IOMMU 分组里的设备只能一起直通，而分组
 * 取决于主板拓扑与 BIOS 设置。用户想只拿走显卡，却会连带拿走同组的音频功能
 * ——那不是 bug，是硬件的事实，而界面上必须在**选择之前**就说出来。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { passthroughApi, type PCIDevice } from '@/api/passthrough'
import { vmApi } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function PassthroughPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [attaching, setAttaching] = useState<PCIDevice | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const vms = useQuery({ queryKey: ['vms'], queryFn: () => vmApi.list({ page_size: 200 }) })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const data = useQuery({
    queryKey: ['passthrough', effectiveNodeID],
    queryFn: () => passthroughApi.overview(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['passthrough'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const bind = useMutation({
    mutationFn: (d: PCIDevice) =>
      d.Driver === 'vfio-pci'
        ? passthroughApi.unbind(effectiveNodeID, d.Address)
        : passthroughApi.bind(effectiveNodeID, d.Address),
    onSuccess: (_r, d) => {
      setError('')
      setNotice(d.Driver === 'vfio-pci' ? '已解绑回宿主驱动' : '已绑定到 vfio-pci，可挂给虚拟机')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  if (nodes.isPending || vms.isPending) return <PageLoading />
  const devices = data.data?.devices ?? []
  const iommu = data.data?.iommu

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">硬件直通</h1>
          <p className="mt-1 text-base text-ink-3">
            把 PCIe 设备（显卡、网卡、NVMe）直接交给虚拟机使用。
            <span className="text-ink-2">
              IOMMU 分组里的设备只能一起直通
            </span>
            ——分组取决于主板与 BIOS，下面每个设备都标出了它的同组成员。
          </p>
        </div>
        <div className="flex items-end gap-2">
          <div className="flex flex-col gap-1">
            <label className="text-xs text-ink-3">节点</label>
            <select
              value={effectiveNodeID}
              onChange={(e) => setNodeID(Number(e.target.value))}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {(nodes.data ?? []).map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </select>
          </div>
        </div>
      </header>

      {notice && <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {/* IOMMU 不可用时给**可操作的指引**，而不是一句"不支持"——这一项
          面板做不了（它自己就跑在这台机器上），但告诉用户加哪个参数、重启
          之后回来，他几分钟就能解决。 */}
      {iommu && !iommu.Enabled && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base text-ink">
            这台宿主机尚未启用 IOMMU，无法直通。{iommu.Reason}
          </p>
          {iommu.Fix && <pre className="kc-mono mt-2 text-xs text-ink-2">{iommu.Fix}</pre>}
          {iommu.NeedReboot && (
            <p className="mt-2 text-sm text-warning">
              改完需要重启宿主机——上面所有虚拟机会停机。请安排窗口期再做。
            </p>
          )}
        </div>
      )}

      {data.isPending ? (
        <PageLoading />
      ) : devices.length === 0 ? (
        <EmptyState title="没有探测到 PCIe 设备" description="请确认节点已接入且探测正常。" />
      ) : (
        <div className="flex flex-col gap-3">
          {devices.map((d) => (
            <DeviceCard
              key={d.Address}
              device={d}
              busy={bind.isPending}
              onToggleBind={() => bind.mutate(d)}
              onAttach={() => setAttaching(d)}
            />
          ))}
        </div>
      )}

      <AttachModal
        open={attaching !== null}
        device={attaching}
        vms={(vms.data?.items ?? []).filter((v) => v.node_id === effectiveNodeID)}
        onClose={() => setAttaching(null)}
        onDone={() => {
          setAttaching(null)
          setError('')
          setNotice('已挂载，启动虚拟机后生效')
          refresh()
        }}
        onError={(m) => {
          setAttaching(null)
          setError(m)
        }}
      />
    </div>
  )
}

function DeviceCard({
  device: d,
  busy,
  onToggleBind,
  onAttach,
}: {
  device: PCIDevice
  busy: boolean
  onToggleBind: () => void
  onAttach: () => void
}) {
  const bound = d.Driver === 'vfio-pci'
  const inUse = !!d.attached_to_vm_id

  return (
    <div className="rounded-card border border-line bg-surface p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <span className="flex flex-wrap items-baseline gap-2">
          <span className="kc-mono text-base text-ink">{d.Address}</span>
          <span className="text-sm text-ink-2">{d.Description || d.Class}</span>
          {!d.CanPassthrough && (
            <StatusBadge tone="warning">不可直通</StatusBadge>
          )}
          {bound && <StatusBadge tone="success">已绑定 vfio</StatusBadge>}
          {inUse && <StatusBadge tone="success">{`已挂到 ${d.attached_to_vm_name ?? ""}`}</StatusBadge>}
        </span>
        <span className="flex gap-3 text-sm">
          <button
            className={d.CanPassthrough ? 'text-primary hover:underline' : 'text-ink-3'}
            disabled={!d.CanPassthrough || busy || inUse}
            onClick={onToggleBind}
            title={inUse ? '设备正被虚拟机使用，请先卸载' : undefined}
          >
            {bound ? '解绑' : '绑定到 vfio'}
          </button>
          <button
            className={d.CanPassthrough ? 'text-primary hover:underline' : 'text-ink-3'}
            disabled={!d.CanPassthrough || inUse}
            onClick={onAttach}
          >
            挂给虚拟机
          </button>
        </span>
      </div>

      <div className="mt-1.5 flex flex-wrap gap-x-4 text-sm">
        <span className="text-ink-3">
          IOMMU 分组：<span className="kc-nums text-ink-2">{d.IOMUGroup}</span>
        </span>
        <span className="text-ink-3">
          当前驱动：<span className="kc-mono text-ink-2">{d.Driver || '—'}</span>
        </span>
        <span className="kc-mono text-xs text-ink-3">{d.VendorDevice}</span>
      </div>

      {/* **同组成员必须展示**：这是用户最容易踩到的一个坑。 */}
      {d.group_peers.length > 0 && (
        <p className="mt-2 rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
          同组设备（会一起被直通拿走）：
          <span className="kc-mono ml-1">{d.group_peers.join('、')}</span>
        </p>
      )}
      {!d.CanPassthrough && d.Reason && (
        <p className="mt-2 text-sm text-danger">{d.Reason}</p>
      )}
    </div>
  )
}

function AttachModal({
  open,
  device,
  vms,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  device: PCIDevice | null
  vms: { id: number; name: string; status: string }[]
  onClose: () => void
  onDone: () => void
  onError: (m: string) => void
}) {
  const [vmID, setVMID] = useState(0)
  const target = vms.find((v) => v.id === vmID)
  const running = target?.status === 'running'

  const attach = useMutation({
    mutationFn: () => passthroughApi.attach(vmID, device?.Address ?? ''),
    onSuccess: onDone,
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title={`把 ${device?.Address ?? ''} 挂给虚拟机`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={vmID === 0 || running}
            loading={attach.isPending}
            onClick={() => attach.mutate()}
          >
            挂载
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">目标虚拟机</label>
          <select
            value={vmID}
            onChange={(e) => setVMID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value={0}>请选择…</option>
            {vms.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}（{v.status === 'running' ? '运行中' : '已关机'}）
              </option>
            ))}
          </select>
        </div>

        {/* 运行中直接禁用而不是等后端拒绝：报错会让人以为是自己选错了什么。 */}
        {running && (
          <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            这台虚拟机正在运行。直通设备不支持热插拔——请先关机再挂载。
          </p>
        )}

        {device && device.group_peers.length > 0 && (
          <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            这块设备与 <span className="kc-mono">{device.group_peers.join('、')}</span> 同属
            IOMMU 分组 {device.IOMUGroup}。同组设备只能一起直通，因此这个分组会
            整体归这台虚拟机——而那几块设备从此宿主机也看不到了。
          </p>
        )}
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
