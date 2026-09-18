/**
 * StorageVolumePage 管理存储卷（F-5-02）。
 *
 * 页面上有一处反复出现的对照：**条带与镜像的方向是相反的**。
 *
 *   条带 = 拿可靠性换性能（更快，但坏一块盘全丢）
 *   镜像 = 拿容量换可靠性（可坏一块盘，但物理占用翻倍）
 *
 * 因此界面上做了三件事：每个卷都显示「有无冗余」这个**判断**（而不是让
 * 用户自己去推参数）、创建时实时显示需要的盘数与实际占用、纯条带配置在
 * 确认之前先把那句话说出来。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { volumeApi, VOLUME_STATUS_LABEL, VOLUME_STATUS_TONE, type VolumePlan, type VolumeView } from '@/api/storagevolume'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function StorageVolumePage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [createOpen, setCreateOpen] = useState(false)
  const [confirmDelete, setConfirmDelete] = useState<VolumeView | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const list = useQuery({
    queryKey: ['storage-volumes', effectiveNodeID],
    queryFn: () => volumeApi.list(effectiveNodeID),
    enabled: effectiveNodeID > 0,
    // 同步中的卷会自己变成正常，轮询让用户不必手动刷新。
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((v) => v.status === 'sync') ? 10000 : false,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['storage-volumes'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    void queryClient.invalidateQueries({ queryKey: ['audit'] })
  }

  const remove = useMutation({
    mutationFn: (id: number) => volumeApi.remove(id),
    onSuccess: () => {
      setConfirmDelete(null)
      setError('')
      setNotice('删除任务已提交')
      refresh()
    },
    onError: (err) => {
      setConfirmDelete(null)
      setError(describe(err))
    },
  })

  if (nodes.isPending) return <PageLoading />
  const items = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">存储卷</h1>
          <p className="mt-1 text-base text-ink-3">
            把多块物理盘聚合成一个卷。带
            <span className="text-ink-2">镜像</span>
            的卷能容忍一块盘故障；只带
            <span className="text-ink-2">条带</span>
            的不能——它更快，但坏一块盘全丢。
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
          <Button size="sm" disabled={effectiveNodeID === 0} onClick={() => setCreateOpen(true)}>
            创建存储卷
          </Button>
        </div>
      </header>

      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {list.isPending ? (
        <PageLoading />
      ) : items.length === 0 ? (
        <EmptyState
          title="还没有存储卷"
          description="存储卷会把整块物理盘从普通存储池里拿走——创建前请确认这些盘上没有需要保留的数据。"
        />
      ) : (
        <div className="flex flex-col gap-3">
          {items.map((v) => (
            <VolumeCard key={v.id} volume={v} onDelete={() => setConfirmDelete(v)} />
          ))}
        </div>
      )}

      <CreateVolumeModal
        open={createOpen}
        nodeID={effectiveNodeID}
        onClose={() => setCreateOpen(false)}
        onDone={() => {
          setCreateOpen(false)
          setError('')
          setNotice('创建任务已提交')
          refresh()
        }}
        onError={(msg) => {
          setCreateOpen(false)
          setError(msg)
        }}
      />

      {/* 删除会销毁数据 —— 必须说清后果，而不是问一句「确定吗」。 */}
      <Modal
        open={confirmDelete !== null}
        title={`删除「${confirmDelete?.name ?? ''}」`}
        description="这会销毁卷里的全部数据，且无法恢复。"
        onClose={() => setConfirmDelete(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setConfirmDelete(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={remove.isPending}
              onClick={() => confirmDelete && remove.mutate(confirmDelete.id)}
            >
              确认删除
            </Button>
          </>
        }
      >
        <dl className="grid gap-2 text-sm sm:grid-cols-2">
          <Row label="可用容量">{confirmDelete?.size_gb} GB</Row>
          <Row label="物理占用">{confirmDelete?.physical_gb} GB</Row>
          <Row label="设备">
            <span className="kc-mono">{(confirmDelete?.devices ?? []).join('、')}</span>
          </Row>
          <Row label="冗余">
            {confirmDelete?.has_redundancy ? '镜像，可容忍一块盘故障' : '无'}
          </Row>
        </dl>
        <p className="mt-3 text-sm text-ink-3">
          删除后这些设备会被释放，可以重新用于别的存储池或卷。
        </p>
      </Modal>
    </div>
  )
}

function VolumeCard({ volume, onDelete }: { volume: VolumeView; onDelete: () => void }) {
  return (
    <div className="rounded-card border border-line bg-surface p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <span className="flex flex-wrap items-baseline gap-2">
          <span className="text-base font-medium text-ink">{volume.name}</span>
          {/* 「有无冗余」是一个判断，而不是让用户自己去推参数。 */}
          {volume.has_redundancy ? (
            <span className="rounded-pill bg-success/10 px-1.5 py-0.5 text-xs text-success">
              镜像 · 可坏一块盘
            </span>
          ) : (
            <span className="rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning">
              无冗余
            </span>
          )}
          <StatusBadge tone={VOLUME_STATUS_TONE[volume.status]}>
            {VOLUME_STATUS_LABEL[volume.status]}
          </StatusBadge>
        </span>
        <button className="text-sm text-danger hover:underline" onClick={onDelete}>
          删除
        </button>
      </div>

      <div className="mt-2 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-3">
        <Row label="可用容量">
          <span className="kc-nums">{volume.size_gb} GB</span>
        </Row>
        <Row label="物理占用">
          {/* 两个数字都给：只看可用容量会让人以为「还能再建一个这么大的」。 */}
          <span className={`kc-nums ${volume.physical_gb > volume.size_gb ? 'text-warning' : ''}`}>
            {volume.physical_gb} GB
          </span>
        </Row>
        <Row label="条带 / 镜像">
          {volume.stripe_count || 1} / {volume.mirror_count || 1}
        </Row>
      </div>

      <div className="mt-1.5 text-sm">
        <span className="text-ink-3">设备：</span>
        <span className="kc-mono text-ink-2">{volume.devices.join('、') || '—'}</span>
      </div>

      {/* 状态说明用人话写出来，尤其 sync 与 degraded。 */}
      {volume.status_note && (
        <p
          className={`mt-1.5 text-sm ${
            volume.status === 'degraded' || volume.status === 'failed'
              ? 'text-danger'
              : volume.status === 'sync'
                ? 'text-warning'
                : 'text-ink-3'
          }`}
        >
          {volume.status_note}
        </p>
      )}
    </div>
  )
}

function CreateVolumeModal({
  open,
  nodeID,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  onClose: () => void
  onDone: () => void
  onError: (message: string) => void
}) {
  const [name, setName] = useState('')
  const [sizeGB, setSizeGB] = useState('100')
  const [stripe, setStripe] = useState('1')
  const [mirror, setMirror] = useState('1')
  const [devicesText, setDevicesText] = useState('')
  const [plan, setPlan] = useState<VolumePlan | null>(null)

  const devices = splitList(devicesText)
  const request = {
    name: name.trim(),
    size_gb: Number(sizeGB) || 0,
    stripe_count: Number(stripe) || 1,
    mirror_count: Number(mirror) || 1,
    devices,
  }
  // 需要几块盘是 **stripe × mirror** —— 这个乘法是直觉最容易出错的地方：
  // 用户会想「镜像要 2 块、条带要 2 块，给 2 块就行了吧」。
  const required = (Number(stripe) || 1) * (Number(mirror) || 1)

  const preview = useMutation({
    mutationFn: () => volumeApi.preview(nodeID, request),
    onSuccess: setPlan,
    onError: (err) => onError(describe(err)),
  })

  const create = useMutation({
    mutationFn: (ack: boolean) => volumeApi.create(nodeID, request, ack),
    onSuccess: (result) => {
      if (result.plan?.warnings?.length && !result.task) {
        // 有警告而未确认：服务端返回了计划但没有任务。
        setPlan(result.plan)
        return
      }
      onDone()
    },
    onError: (err) => onError(describe(err)),
  })

  const ready = name.trim() !== '' && Number(sizeGB) > 0 && devices.length >= required

  return (
    <Modal
      open={open}
      title="创建存储卷"
      description="参与聚合的物理盘会被整块占用——请确认上面没有需要保留的数据。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          {plan?.warnings?.length ? (
            <Button
              variant="danger"
              size="sm"
              loading={create.isPending}
              onClick={() => create.mutate(true)}
            >
              我已确认，仍然创建
            </Button>
          ) : (
            <Button
              size="sm"
              disabled={!ready}
              loading={preview.isPending}
              onClick={() => preview.mutate()}
            >
              预检
            </Button>
          )}
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input label="卷名称" value={name} onChange={(e) => setName(e.target.value)} />
        <Input
          label="可用容量（GB）"
          value={sizeGB}
          onChange={(e) => setSizeGB(e.target.value)}
          hint="这是卷的可用容量。镜像会让实际占用变成它的倍数——见下方换算。"
        />

        <div className="flex gap-4">
          <div className="flex flex-1 flex-col gap-1">
            <label className="text-sm text-ink-2">条带数</label>
            <select
              value={stripe}
              onChange={(e) => setStripe(e.target.value)}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {['1', '2', '3', '4'].map((n) => (
                <option key={n} value={n}>
                  {n === '1' ? '不使用条带' : `${n} 条带`}
                </option>
              ))}
            </select>
            <p className="text-xs text-warning">条带提升吞吐，但没有冗余</p>
          </div>
          <div className="flex flex-1 flex-col gap-1">
            <label className="text-sm text-ink-2">镜像份数</label>
            <select
              value={mirror}
              onChange={(e) => setMirror(e.target.value)}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {['1', '2', '3', '4'].map((n) => (
                <option key={n} value={n}>
                  {n === '1' ? '不使用镜像' : `${n} 份镜像`}
                </option>
              ))}
            </select>
            <p className="text-xs text-ink-3">镜像可容忍坏盘，但物理占用翻倍</p>
          </div>
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">物理设备（每行一个）</label>
          <textarea
            value={devicesText}
            rows={3}
            placeholder={'/dev/sdb\n/dev/sdc'}
            onChange={(e) => setDevicesText(e.target.value)}
            className="rounded-control border border-line-strong bg-sunken px-2 py-1.5 font-mono text-base text-ink"
          />
          <p className={`text-xs ${devices.length < required ? 'text-warning' : 'text-ink-3'}`}>
            需要 {required} 块（条带 {Number(stripe) || 1} × 镜像 {Number(mirror) || 1}），
            已填 {devices.length} 块
          </p>
        </div>

        {/* 换算结果：这几个数正是这块功能最容易想错的地方。 */}
        {plan && (
          <div className="rounded-control border border-line bg-sunken px-3 py-2 text-sm">
            <div className="flex justify-between">
              <span className="text-ink-3">需要设备</span>
              <span className="kc-nums text-ink">
                {plan.required_devices} 块（已给 {plan.given_devices}）
              </span>
            </div>
            <div className="flex justify-between">
              <span className="text-ink-3">可用容量 / 物理占用</span>
              <span className="kc-nums text-ink">
                {plan.size_gb} GB / <span className={plan.physical_gb > plan.size_gb ? 'text-warning' : ''}>{plan.physical_gb} GB</span>
              </span>
            </div>
            <div className="flex justify-between">
              <span className="text-ink-3">能否容忍一块盘故障</span>
              <span className={plan.has_redundancy ? 'text-success' : 'text-warning'}>
                {plan.has_redundancy ? '可以' : '不能'}
              </span>
            </div>
          </div>
        )}

        {(plan?.warnings ?? []).length > 0 && (
          <ul className="flex flex-col gap-1.5">
            {plan!.warnings!.map((w) => (
              <li
                key={w}
                className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning"
              >
                {w}
              </li>
            ))}
          </ul>
        )}
      </div>
    </Modal>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex gap-2">
      <span className="shrink-0 text-ink-3">{label}</span>
      <span className="text-ink">{children}</span>
    </div>
  )
}

function splitList(raw: string): string[] {
  return raw
    .split(/[\n,;]/)
    .map((s) => s.trim())
    .filter(Boolean)
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
