/**
 * PortMirrorPage 管理端口镜像（F-4-09）。
 *
 * 页面的核心是**看门狗窗口**。启用之后有一段倒计时，用户看着网络没问题
 * 就点「保持」；不点的话，节点会自己把镜像撤销掉。
 *
 * 倒计时用本地 `setInterval` 只是为了**显示流畅**，判断依据始终以服务端
 * 返回的 `watchdog_seconds_left` 为准——服务端每次刷新都会重算，因此即使
 * 本地时钟不准或页面被挂起了很久，重新拉取之后数字就是对的。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import {
  DIRECTION_LABEL,
  formatCountdown,
  portMirrorApi,
  type MirrorDirection,
  type MirrorEnableResult,
  type PortMirrorView,
} from '@/api/portmirror'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'

export function PortMirrorPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [formOpen, setFormOpen] = useState(false)
  const [editing, setEditing] = useState<PortMirrorView | null>(null)
  const [enableResult, setEnableResult] = useState<MirrorEnableResult | null>(null)
  const [enabling, setEnabling] = useState<PortMirrorView | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const list = useQuery({
    queryKey: ['port-mirrors', effectiveNodeID],
    queryFn: () => portMirrorApi.list(effectiveNodeID),
    enabled: effectiveNodeID > 0,
    // 有窗口在跑时定时刷新：倒计时以服务端为准，本地那份只是插值。
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((m) => m.awaiting_confirm) ? 5000 : false,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['port-mirrors'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const confirm = useMutation({
    mutationFn: (id: number) => portMirrorApi.confirm(id),
    onSuccess: () => {
      setError('')
      setNotice('已确认保持，自动撤销已取消')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const disable = useMutation({
    mutationFn: (id: number) => portMirrorApi.disable(id),
    onSuccess: () => {
      setError('')
      setNotice('镜像已关闭')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const remove = useMutation({
    mutationFn: (id: number) => portMirrorApi.remove(id),
    onSuccess: () => {
      setError('')
      setNotice('规则已删除')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (nodes.isPending) return <PageLoading />
  const items = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">端口镜像</h1>
          <p className="mt-1 text-base text-ink-3">
            把来源接口的流量复制到空交换机上供分析。
            启用后会有一个
            <span className="text-ink-2">自动撤销窗口</span>
            ——在那之前点「保持」，否则镜像会被节点自己关掉。
          </p>
        </div>
        <div className="flex items-end gap-2">
          <div className="flex flex-col gap-1">
            <label className="text-xs text-ink-3">节点</label>
            <select
              value={effectiveNodeID}
              onChange={(e) => {
                setNodeID(Number(e.target.value))
                setNotice('')
              }}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {(nodes.data ?? []).map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </select>
          </div>
          <Button
            size="sm"
            disabled={effectiveNodeID === 0}
            onClick={() => {
              setEditing(null)
              setFormOpen(true)
            }}
          >
            新建规则
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
          title="还没有镜像规则"
          description="新建一条规则，然后启用它——启用时会同时建立一个自动撤销窗口。"
        />
      ) : (
        <div className="flex flex-col gap-3">
          {items.map((m) => (
            <MirrorCard
              key={m.id}
              mirror={m}
              onEnable={() => setEnabling(m)}
              onConfirm={() => confirm.mutate(m.id)}
              onDisable={() => disable.mutate(m.id)}
              onEdit={() => {
                setEditing(m)
                setFormOpen(true)
              }}
              onDelete={() => remove.mutate(m.id)}
              busy={confirm.isPending || disable.isPending || remove.isPending}
            />
          ))}
        </div>
      )}

      <MirrorFormModal
        open={formOpen}
        nodeID={effectiveNodeID}
        editing={editing}
        onClose={() => setFormOpen(false)}
        onDone={() => {
          setFormOpen(false)
          setError('')
          setNotice(editing ? '规则已修改' : '规则已创建（尚未启用）')
          refresh()
        }}
        onError={(msg) => {
          setFormOpen(false)
          setError(msg)
        }}
      />

      <EnableModal
        target={enabling}
        onClose={() => setEnabling(null)}
        onResult={(result) => {
          setEnabling(null)
          if (result.applied) {
            setEnableResult(result)
            setError('')
          }
          refresh()
        }}
        onError={(msg) => {
          setEnabling(null)
          setError(msg)
        }}
      />

      {/* 启用后的收尾提示。看门狗是这个功能的兜底，因此这里要说的不是
          「成功了」，而是「你现在有 N 分钟决定要不要留下它」。 */}
      <Modal
        open={enableResult !== null}
        title="镜像已启用 —— 请在窗口内确认"
        description={`如果这段时间里网络正常，点「保持」；不点的话，节点会在 ${formatCountdown(
          enableResult?.watchdog_seconds ?? 0,
        )} 后自动把它撤销。`}
        onClose={() => setEnableResult(null)}
        footer={
          <Button size="sm" onClick={() => setEnableResult(null)}>
            知道了
          </Button>
        }
      >
        <p className="text-sm text-ink-2">
          自动撤销由<span className="text-ink">节点</span>执行，不依赖面板或网络
          ——因此就算你现在连不上面板，它也一样会生效。
        </p>
        {(enableResult?.warnings ?? []).length > 0 && (
          <ul className="mt-3 flex flex-col gap-1.5">
            {enableResult!.warnings!.map((w) => (
              <li key={w} className="rounded-control bg-warning/10 px-2.5 py-1.5 text-sm text-warning">
                {w}
              </li>
            ))}
          </ul>
        )}
      </Modal>
    </div>
  )
}

function MirrorCard({
  mirror,
  onEnable,
  onConfirm,
  onDisable,
  onEdit,
  onDelete,
  busy,
}: {
  mirror: PortMirrorView
  onEnable: () => void
  onConfirm: () => void
  onDisable: () => void
  onEdit: () => void
  onDelete: () => void
  busy: boolean
}) {
  // 本地插值只是为了显示流畅，基准值始终来自服务端。
  //
  // 服务端值变化时用**带守卫的渲染期重置**同步（不是 effect）：effect 里
  // setState 会额外渲染一轮，而倒计时这类每秒都在变的东西会因此持续多渲染
  // 一次；渲染期重置和这次渲染是同一帧。
  const [left, setLeft] = useState(mirror.watchdog_seconds_left)
  const [seenLeft, setSeenLeft] = useState(mirror.watchdog_seconds_left)
  if (mirror.watchdog_seconds_left !== seenLeft) {
    setSeenLeft(mirror.watchdog_seconds_left)
    setLeft(mirror.watchdog_seconds_left)
  }
  useEffect(() => {
    if (!mirror.awaiting_confirm) return
    const timer = setInterval(() => setLeft((v) => (v > 0 ? v - 1 : 0)), 1000)
    return () => clearInterval(timer)
  }, [mirror.awaiting_confirm])

  return (
    <div
      className={`rounded-card border p-4 ${
        mirror.awaiting_confirm ? 'border-warning/50 bg-warning/5' : 'border-line bg-surface'
      }`}
    >
      <div className="flex items-baseline justify-between gap-3">
        <span className="flex items-baseline gap-2">
          <span className="text-base font-medium text-ink">{mirror.name || `镜像#${mirror.id}`}</span>
          <span className="text-xs text-ink-3">{DIRECTION_LABEL[mirror.direction]}</span>
          {mirror.vlan_preserve && <span className="text-xs text-ink-3">保留 VLAN</span>}
        </span>
        <span className="text-xs">
          {mirror.enabled ? (
            mirror.awaiting_confirm ? (
              <span className="text-warning">等待确认 · 剩余 {formatCountdown(left)}</span>
            ) : (
              <span className="text-success">生效中</span>
            )
          ) : (
            <span className="text-ink-3">未启用</span>
          )}
        </span>
      </div>

      <div className="mt-2 flex flex-col gap-0.5 text-sm">
        <span className="text-ink-2">
          来源：<span className="kc-mono">{mirror.source_ports.join('、')}</span>
        </span>
        <span className="text-ink-2">
          目标：<span className="kc-mono">{mirror.target_switches.join('、')}</span>
        </span>
      </div>

      <div className="mt-3 flex flex-wrap gap-2 border-t border-line pt-3">
        {!mirror.enabled ? (
          <>
            <Button size="sm" onClick={onEnable} disabled={busy}>
              启用
            </Button>
            <Button variant="secondary" size="sm" onClick={onEdit} disabled={busy}>
              修改
            </Button>
            <Button variant="danger" size="sm" onClick={onDelete} disabled={busy}>
              删除
            </Button>
          </>
        ) : (
          <>
            {mirror.awaiting_confirm && (
              <Button size="sm" onClick={onConfirm} loading={busy}>
                保持
              </Button>
            )}
            {/* 关闭永远可点、不要确认、不会失败——一个正在打垮网络的
                镜像，用户需要的是一键停掉。 */}
            <Button
              variant={mirror.awaiting_confirm ? 'secondary' : 'danger'}
              size="sm"
              onClick={onDisable}
              loading={busy}
            >
              关闭
            </Button>
          </>
        )}
      </div>
    </div>
  )
}

function EnableModal({
  target,
  onClose,
  onResult,
  onError,
}: {
  target: PortMirrorView | null
  onClose: () => void
  onResult: (result: MirrorEnableResult) => void
  onError: (message: string) => void
}) {
  const [seconds, setSeconds] = useState('300')
  const [warnings, setWarnings] = useState<string[] | null>(null)

  const enable = useMutation({
    mutationFn: (ack: boolean) =>
      portMirrorApi.enable(target!.id, Number(seconds) || 0, ack),
    onSuccess: (result) => {
      if (!result.applied && result.warnings?.length) {
        setWarnings(result.warnings)
        return
      }
      setWarnings(null)
      onResult(result)
    },
    onError: (err) => onError(describe(err)),
  })

  if (!target) return null

  return (
    <Modal
      open
      title={`启用「${target.name || `镜像#${target.id}`}」`}
      description="启用会同时建立一个自动撤销窗口，由节点侧计时。窗口内点「保持」才会长期生效。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={enable.isPending} onClick={() => enable.mutate(false)}>
            启用
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="自动撤销窗口（秒）"
          value={seconds}
          onChange={(e) => setSeconds(e.target.value)}
          hint="60 ~ 1800 秒。窗口内不点「保持」，节点会自己把镜像撤销——这是配错时唯一的兜底。"
        />
        <p className="text-sm text-ink-3">
          为什么不设「永不自动撤销」：镜像配错时可能形成环路，流量会自我放大
          直到把宿主机网络打垮。<span className="text-ink-2">那种情况下面板也连不上</span>
          ，因此兜底必须由节点自己执行。
        </p>

        {warnings && (
          <div className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2">
            <p className="text-base font-medium text-warning">启用前请确认</p>
            <ul className="mt-1 flex flex-col gap-1">
              {warnings.map((w) => (
                <li key={w} className="text-sm text-ink-2">
                  · {w}
                </li>
              ))}
            </ul>
            <Button
              variant="danger"
              size="sm"
              className="mt-2"
              loading={enable.isPending}
              onClick={() => enable.mutate(true)}
            >
              我已确认，仍然启用
            </Button>
          </div>
        )}
      </div>
    </Modal>
  )
}

function MirrorFormModal({
  open,
  nodeID,
  editing,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  editing: PortMirrorView | null
  onClose: () => void
  onDone: () => void
  onError: (message: string) => void
}) {
  const [name, setName] = useState('')
  const [sources, setSources] = useState('')
  const [targets, setTargets] = useState('')
  const [direction, setDirection] = useState<MirrorDirection>('ingress')
  const [vlan, setVlan] = useState(false)

  // 打开时同步一次编辑对象。
  const [seen, setSeen] = useState<string | null>(null)
  const key = open ? (editing ? `e${editing.id}` : 'new') : null
  if (key !== seen) {
    setSeen(key)
    if (key) {
      setName(editing?.name ?? '')
      setSources((editing?.source_ports ?? []).join('\n'))
      setTargets((editing?.target_switches ?? []).join('\n'))
      setDirection(editing?.direction ?? 'ingress')
      setVlan(editing?.vlan_preserve ?? false)
    }
  }

  const submit = useMutation({
    mutationFn: () => {
      const input = {
        name: name.trim() || undefined,
        source_ports: splitList(sources),
        target_switches: splitList(targets),
        direction,
        vlan_preserve: vlan,
      }
      return editing ? portMirrorApi.update(editing.id, input) : portMirrorApi.create(nodeID, input)
    },
    onSuccess: onDone,
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title={editing ? '修改镜像规则' : '新建镜像规则'}
      description="多个来源接口把流量复制到多个目标空交换机。规则创建后默认不启用。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={submit.isPending} onClick={() => submit.mutate()}>
            {editing ? '保存' : '创建'}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input label="名称（可选）" value={name} onChange={(e) => setName(e.target.value)} />
        <Area
          label="来源接口（每行一个）"
          value={sources}
          onChange={setSources}
          hint="重复填写会被拒绝——同一个口的流量被复制两份是静默的流量放大，抓包里看起来像对端在重传。"
        />
        <Area
          label="目标交换机（每行一个）"
          value={targets}
          onChange={setTargets}
          hint="镜像是把流量复制出去，因此目标必须存在——没有目标时流量会堆在宿主机的发送队列里。"
        />

        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">方向</span>
          {(['ingress', 'egress', 'both'] as MirrorDirection[]).map((d) => (
            <label key={d} className="flex cursor-pointer items-start gap-2.5">
              <input
                type="radio"
                className="mt-1"
                name="mirror-dir"
                checked={direction === d}
                onChange={() => setDirection(d)}
              />
              <span>
                <span className="block text-base text-ink">{DIRECTION_LABEL[d]}</span>
                {d === 'both' && (
                  <span className="block text-sm text-ink-3">
                    双向：任一方向上的环路都会被两个方向同时放大。只分析一个方向时选单向更安全。
                  </span>
                )}
              </span>
            </label>
          ))}
        </div>

        <label className="flex cursor-pointer items-start gap-2.5">
          <input type="checkbox" className="mt-1" checked={vlan} onChange={(e) => setVlan(e.target.checked)} />
          <span>
            <span className="block text-base text-ink">保留 VLAN 标签</span>
            <span className="block text-sm text-ink-3">
              默认剥掉：多数抓包工具对带标签的帧处理得不好。要分析 VLAN 本身的行为时才打开。
            </span>
          </span>
        </label>
      </div>
    </Modal>
  )
}

function Area({
  label,
  value,
  onChange,
  hint,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  hint?: string
}) {
  return (
    <div className="flex flex-col gap-1">
      <label className="text-sm text-ink-2">{label}</label>
      <textarea
        value={value}
        rows={3}
        onChange={(e) => onChange(e.target.value)}
        className="rounded-control border border-line-strong bg-sunken px-2 py-1.5 font-mono text-base text-ink"
      />
      {hint && <p className="text-xs text-ink-3">{hint}</p>}
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
