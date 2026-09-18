/**
 * CapturePage 发起与查看抓包（F-4-12）。
 *
 * 抓包与项目里其它操作不同：它是**异步且要等**的。下发之后要等 duration
 * 秒才有文件，这期间界面必须显示"进行中，还剩 N 秒"，而不是"文件不存在"
 * ——后者会让用户以为抓包失败了，然后重复发起。
 *
 * 另一处要在界面上说清的是**过期**：文件里有完整的流量内容（含明文密码与
 * 会话令牌），因此它在 24 小时后自动消失。用户想下载时如果文件已经没了，
 * 他需要事先知道这是设计如此，而不是系统出了问题。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { CAPTURE_DURATION, captureApi, formatBytes, formatExpiry, type CaptureView } from '@/api/capture'
import { nodeApi } from '@/api/node'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function CapturePage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [open, setOpen] = useState(false)
  const [error, setError] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const list = useQuery({
    queryKey: ['captures', effectiveNodeID],
    queryFn: () => captureApi.list(effectiveNodeID),
    enabled: effectiveNodeID > 0,
    // 进行中的抓包会自己变成"可下载"，轮询让用户不必手动刷新。
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((c) => c.status === 'running') ? 3000 : false,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['captures'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const remove = useMutation({
    mutationFn: (id: number) => captureApi.remove(id),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  if (nodes.isPending) return <PageLoading />
  const items = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">抓包诊断</h1>
          <p className="mt-1 text-base text-ink-3">
            在指定网口上抓取一段时间的流量。抓包文件含完整流量内容，因此
            <span className="text-ink-2">24 小时后自动从节点上删除</span>
            ，请及时下载。
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
          <Button size="sm" disabled={effectiveNodeID === 0} onClick={() => setOpen(true)}>
            发起抓包
          </Button>
        </div>
      </header>

      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {list.isPending ? (
        <PageLoading />
      ) : items.length === 0 ? (
        <EmptyState
          title="还没有抓包记录"
          description="抓包会在指定网口上运行一段固定时长。同时进行的抓包有限制——它会占用节点 CPU 与磁盘带宽，进而影响那台机器上虚拟机的网络性能。"
        />
      ) : (
        <div className="flex flex-col gap-3">
          {items.map((c) => (
            <CaptureCard key={c.id} capture={c} onDelete={() => remove.mutate(c.id)} />
          ))}
        </div>
      )}

      <StartModal
        open={open}
        nodeID={effectiveNodeID}
        onClose={() => setOpen(false)}
        onDone={() => {
          setOpen(false)
          setError('')
          refresh()
        }}
        onError={(m) => {
          setOpen(false)
          setError(m)
        }}
      />
    </div>
  )
}

function CaptureCard({ capture: c, onDelete }: { capture: CaptureView; onDelete: () => void }) {
  return (
    <div className="rounded-card border border-line bg-surface p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <span className="flex flex-wrap items-baseline gap-2">
          <span className="kc-mono text-base text-ink">{c.interface || '—'}</span>
          {c.vm_name && <span className="text-sm text-ink-3">{c.vm_name}</span>}
          <StatusBadge tone={c.status === 'ready' ? 'success' : 'warning'}>
            {c.status === 'ready' ? '可下载' : `抓包中 · 还剩 ${c.seconds_left ?? 0} 秒`}
          </StatusBadge>
          {c.status === 'ready' && (
            <span className="text-sm text-ink-3">{formatBytes(c.size_bytes)}</span>
          )}
        </span>
        <button className="text-sm text-danger hover:underline" onClick={onDelete}>
          删除
        </button>
      </div>

      <div className="mt-1.5 flex flex-wrap gap-x-4 text-sm">
        <span className="text-ink-3">
          过滤器：
          <span className="kc-mono text-ink-2">{c.filter || '（全部流量）'}</span>
        </span>
        <span className="text-ink-3">时长：{c.duration_sec} 秒</span>
        {c.expires_at && <span className="text-ink-3">{formatExpiry(c.expires_at)}</span>}
      </div>

      {/* 空文件是一个看不出原因的结果——必须把最常见的原因说出来。 */}
      {c.hint && <p className="mt-1.5 text-sm text-warning">{c.hint}</p>}
    </div>
  )
}

function StartModal({
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
  onError: (m: string) => void
}) {
  const [iface, setIface] = useState('')
  const [filter, setFilter] = useState('')
  const [duration, setDuration] = useState('30')

  const dur = Number(duration) || 0
  const durBad = dur < CAPTURE_DURATION.min || dur > CAPTURE_DURATION.max
  const ready = iface.trim() !== '' && !durBad

  const start = useMutation({
    mutationFn: () =>
      captureApi.start(nodeID, {
        interface: iface.trim(),
        filter: filter.trim(),
        duration_sec: dur,
      }),
    onSuccess: onDone,
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title="发起抓包"
      description="抓包在节点上运行固定时长后自动停止。文件含完整流量内容，24 小时后自动删除。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" disabled={!ready} loading={start.isPending} onClick={() => start.mutate()}>
            开始抓包
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="网口"
          value={iface}
          onChange={(e) => setIface(e.target.value)}
          hint="虚拟机网口名，例如 vnet0。"
        />
        <Input
          label="BPF 过滤器"
          value={filter}
          placeholder="留空表示抓全部流量"
          onChange={(e) => setFilter(e.target.value)}
          hint="例如 tcp port 80、host 10.0.0.5。留空会抓到该网口上的一切——先用空过滤器确认有没有流量，往往是排查的第一步。"
        />
        <div className="flex flex-col gap-1">
          <Input
            label="抓包时长（秒）"
            value={duration}
            onChange={(e) => setDuration(e.target.value)}
            hint={`${CAPTURE_DURATION.min} ~ ${CAPTURE_DURATION.max} 秒。必须有上限：不限时的抓包会把宿主机磁盘写满，而且通常会被忘记停。`}
          />
          {durBad && (
            <p className="text-xs text-warning">
              超出范围（{CAPTURE_DURATION.min} ~ {CAPTURE_DURATION.max} 秒）。
            </p>
          )}
        </div>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
