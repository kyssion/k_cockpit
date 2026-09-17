/**
 * NetworkPage2 展示网络底座状态与自愈入口（F-4-13）。
 *
 * 这一页与别处最大的不同：**它必须能在一个「半坏」的状态下工作**。
 *
 * 探测失败、桥列表读不出来，后端都会以字段形式返回而不是报错。因此界面上
 * 有三处刻意的处理：
 *
 *   - 探测失败时显示明确的「状态未知」，而**不是**把能力数据画成「什么都没有」
 *     ——后者会让用户以为他的网络全没了，而真实情况是面板连不上节点；
 *   - 桥列表读不出来时只让那一块显示错误，页头与修复入口仍然可用；
 *   - 降级提示**具体列出**受影响的网络，而不是一句「已降级」。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import {
  BRIDGE_MODE_LABEL,
  BRIDGE_STATUS_LABEL,
  BRIDGE_STATUS_TONE,
  formatCountdown,
  networkApi,
  type AttachUplinkResult,
  type BridgeView,
  type RepairResult,
} from '@/api/networkbridge'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function NetworkPage2() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [repairResult, setRepairResult] = useState<RepairResult | null>(null)
  const [uplinkTarget, setUplinkTarget] = useState<BridgeView | null>(null)
  const [uplinkResult, setUplinkResult] = useState<AttachUplinkResult | null>(null)

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const overview = useQuery({
    queryKey: ['network-overview', effectiveNodeID],
    queryFn: () => networkApi.overview(effectiveNodeID),
    enabled: effectiveNodeID > 0,
    // 有窗口在跑时定时刷新：倒计时以服务端为准。
    refetchInterval: (q) =>
      (q.state.data?.bridges ?? []).some((b) => b.awaiting_uplink_confirm) ? 5000 : false,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['network-overview'] })
  }

  const repair = useMutation({
    mutationFn: () => networkApi.repair(effectiveNodeID),
    onSuccess: (result) => {
      setError('')
      setRepairResult(result)
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const confirmUplink = useMutation({
    mutationFn: (id: number) => networkApi.confirmUplink(id),
    onSuccess: () => {
      setError('')
      setNotice('已确认保持，自动回滚已取消')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const detachUplink = useMutation({
    mutationFn: (id: number) => networkApi.detachUplink(id),
    onSuccess: () => {
      setError('')
      setNotice('物理口已摘出')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (nodes.isPending || overview.isPending) return <PageLoading />
  const data = overview.data

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">网络</h1>
          <p className="mt-1 text-base text-ink-3">
            节点的网络底座状态、能力与修复入口。
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
          <Button size="sm" loading={repair.isPending} onClick={() => repair.mutate()}>
            修复
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

      {/* 探测失败：这是**状态未知**，不是「什么都没有」。两者的界面必须
          长得不一样——否则用户会以为他的网络全没了。 */}
      {!data?.probe_ok && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base font-medium text-warning">网络状态未知</p>
          <p className="mt-1 text-base text-ink-2">
            {data?.probe_error || '无法完成网络探测'}
          </p>
          <p className="mt-1 text-sm text-ink-3">
            这不代表节点上没有网络——只代表面板此刻读不到它的状态。
            下面的列表来自控制面的记录，仍可作为参考；点击「修复」可尝试重新探测并收敛。
          </p>
        </div>
      )}

      {/* 降级提示：必须说明**影响了什么**，而不是一句「已降级」。 */}
      {data?.degraded && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base font-medium text-warning">已降级运行</p>
          <ul className="mt-1 flex flex-col gap-1">
            {(data.degraded_reasons ?? []).map((r) => (
              <li key={r} className="text-base text-ink-2">
                · {r}
              </li>
            ))}
          </ul>
        </div>
      )}

      {(data?.warnings ?? []).length > 0 && (
        <div className="rounded-card border border-danger/40 bg-danger/5 px-4 py-3">
          {data!.warnings!.map((w) => (
            <p key={w} className="text-base text-danger">
              {w}
            </p>
          ))}
        </div>
      )}

      {/* 能力 */}
      {data?.probe_ok && data.capability && (
        <section className="rounded-card border border-line bg-surface p-4">
          <h2 className="text-sm text-ink-3">网络能力</h2>
          <dl className="mt-2 grid gap-2 text-sm sm:grid-cols-3">
            <div>
              <dt className="text-ink-3">Open vSwitch</dt>
              <dd className={data.capability.ovs_available ? 'text-success' : 'text-warning'}>
                {data.capability.ovs_available
                  ? `可用${data.capability.ovs_version ? `（${data.capability.ovs_version}）` : ''}`
                  : '不可用 · 已降级到 Linux 网桥'}
              </dd>
            </div>
            <div>
              <dt className="text-ink-3">内核模块</dt>
              <dd className="kc-mono text-ink-2">
                {data.capability.kernel_modules.join('、') || '—'}
              </dd>
            </div>
            <div>
              <dt className="text-ink-3">可用上行口</dt>
              <dd className="kc-mono text-ink-2">
                {data.capability.uplink_candidates.map((c) => c.name).join('、') || '—'}
              </dd>
            </div>
          </dl>
        </section>
      )}

      {/* 桥列表。这一块失败时只让这块显示错误，页头与修复入口仍然可用。 */}
      <section className="flex flex-col gap-3">
        <h2 className="text-sm text-ink-3">网络</h2>
        {data?.bridges_error ? (
          <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-base text-warning">
            {data.bridges_error}
          </p>
        ) : (data?.bridges ?? []).length === 0 ? (
          <EmptyState title="还没有自定义网络" description="默认网络由节点在纳管时创建。" />
        ) : (
          <div className="flex flex-col gap-3">
            {data!.bridges.map((b) => (
              <BridgeCard
                key={b.id}
                bridge={b}
                onAttach={() => setUplinkTarget(b)}
                onConfirm={() => confirmUplink.mutate(b.id)}
                onDetach={() => detachUplink.mutate(b.id)}
                busy={confirmUplink.isPending || detachUplink.isPending}
              />
            ))}
          </div>
        )}
      </section>

      <UplinkModal
        target={uplinkTarget}
        candidates={data?.capability?.uplink_candidates ?? []}
        onClose={() => setUplinkTarget(null)}
        onResult={(result) => {
          setUplinkTarget(null)
          if (result.applied) setUplinkResult(result)
          refresh()
        }}
        onError={(msg) => {
          setUplinkTarget(null)
          setError(msg)
        }}
      />

      {/* 修复结果：fixed 与 remaining **分开呈现**——只报前者会让用户以为
          没事了，只报后者又看不出这次做了什么。 */}
      <Modal
        open={repairResult !== null}
        title="修复完成"
        onClose={() => setRepairResult(null)}
        footer={
          <Button size="sm" onClick={() => setRepairResult(null)}>
            关闭
          </Button>
        }
      >
        <p className="text-base text-ink-2">{repairResult?.message}</p>
        {(repairResult?.fixed ?? []).length > 0 && (
          <div className="mt-3">
            <p className="text-sm text-success">已修复</p>
            <ul className="mt-0.5">
              {repairResult!.fixed.map((f) => (
                <li key={f} className="text-base text-ink-2">
                  · {f}
                </li>
              ))}
            </ul>
          </div>
        )}
        {(repairResult?.remaining ?? []).length > 0 && (
          <div className="mt-3">
            <p className="text-sm text-warning">仍需处理</p>
            <ul className="mt-0.5">
              {repairResult!.remaining.map((r) => (
                <li key={r} className="text-base text-ink-2">
                  · {r}
                </li>
              ))}
            </ul>
          </div>
        )}
        {repairResult && !repairResult.probe_ok && (
          <p className="mt-3 text-sm text-warning">
            探测未成功（{repairResult.probe_error}），因此上面这些状态无法确认。
          </p>
        )}
      </Modal>

      {/* 入桥后的收尾：说的是「你有 N 分钟决定要不要留下它」。 */}
      <Modal
        open={uplinkResult !== null}
        title="物理口已入桥 —— 请在窗口内确认"
        description={`如果网络正常，点「保持」；不点的话，节点会在 ${formatCountdown(
          uplinkResult?.watchdog_seconds ?? 0,
        )} 后自动把口摘出来。`}
        onClose={() => setUplinkResult(null)}
        footer={
          <Button size="sm" onClick={() => setUplinkResult(null)}>
            知道了
          </Button>
        }
      >
        <p className="text-sm text-ink-2">
          自动回滚由<span className="text-ink">节点</span>执行，不依赖面板或网络
          ——因此就算你现在连不上面板，它也一样会生效。
        </p>
      </Modal>
    </div>
  )
}

function BridgeCard({
  bridge,
  onAttach,
  onConfirm,
  onDetach,
  busy,
}: {
  bridge: BridgeView
  onAttach: () => void
  onConfirm: () => void
  onDetach: () => void
  busy: boolean
}) {
  return (
    <div
      className={`rounded-card border p-4 ${
        bridge.awaiting_uplink_confirm ? 'border-warning/50 bg-warning/5' : 'border-line bg-surface'
      }`}
    >
      <div className="flex items-baseline justify-between gap-3">
        <span className="flex items-baseline gap-2">
          <span className="text-base font-medium text-ink">{bridge.name}</span>
          {bridge.is_system && (
            <span className="rounded-pill bg-info/10 px-1.5 py-0.5 text-xs text-info">系统预置</span>
          )}
          {bridge.needs_ovs && (
            <span className="rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning">
              依赖 OVS
            </span>
          )}
          <StatusBadge tone={BRIDGE_STATUS_TONE[bridge.status]}>
            {BRIDGE_STATUS_LABEL[bridge.status]}
          </StatusBadge>
        </span>
        {bridge.awaiting_uplink_confirm && (
          <span className="text-xs text-warning">
            等待确认 · 剩余 {formatCountdown(bridge.uplink_seconds_left)}
          </span>
        )}
      </div>

      <div className="mt-2 grid gap-x-4 gap-y-1 text-sm sm:grid-cols-2">
        <Row label="模式">{BRIDGE_MODE_LABEL[bridge.mode]}</Row>
        <Row label="后端">{bridge.backend === 'ovs' ? 'Open vSwitch' : 'Linux 网桥'}</Row>
        <Row label="网段">
          <span className="kc-mono">{bridge.cidr || '—'}</span>
        </Row>
        <Row label="网关">
          <span className="kc-mono">{bridge.gateway_ip || '—'}</span>
        </Row>
        <Row label="物理口">
          <span className="kc-mono">{bridge.uplink_if || '未接外网'}</span>
        </Row>
        <Row label="DHCP">{bridge.dhcp_enabled ? '已启用' : '未启用'}</Row>
      </div>

      {/* 失败原因必须显示出来：只说「异常」用户唯一的动作是重试。 */}
      {bridge.detail && <p className="mt-2 text-sm text-danger">{bridge.detail}</p>}

      <div className="mt-3 flex flex-wrap gap-2 border-t border-line pt-3">
        {bridge.awaiting_uplink_confirm ? (
          <>
            <Button size="sm" onClick={onConfirm} loading={busy}>
              保持
            </Button>
            <Button variant="danger" size="sm" onClick={onDetach} loading={busy}>
              立即摘出
            </Button>
          </>
        ) : (
          <>
            <Button variant="secondary" size="sm" onClick={onAttach} disabled={busy}>
              {bridge.uplink_if ? '更换物理口' : '接入物理口'}
            </Button>
            {bridge.uplink_if && (
              <Button variant="secondary" size="sm" onClick={onDetach} loading={busy}>
                摘出物理口
              </Button>
            )}
          </>
        )}
      </div>
    </div>
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

function UplinkModal({
  target,
  candidates,
  onClose,
  onResult,
  onError,
}: {
  target: BridgeView | null
  candidates: { name: string; up: boolean; has_ip: boolean; speed?: string }[]
  onClose: () => void
  onResult: (result: AttachUplinkResult) => void
  onError: (message: string) => void
}) {
  const [iface, setIface] = useState('')
  const [seconds, setSeconds] = useState('300')
  const [warnings, setWarnings] = useState<string[] | null>(null)

  const attach = useMutation({
    mutationFn: (ack: boolean) =>
      networkApi.attachUplink(target!.id, iface, Number(seconds) || 0, ack),
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
      title={`把物理口接入「${target.name}」`}
      description="入桥会重置该口的 IP 配置。如果它承载管理流量，你会立刻失去连接——因此有一个自动回滚窗口兜底。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={iface === ''}
            loading={attach.isPending}
            onClick={() => attach.mutate(false)}
          >
            接入
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">物理口</span>
          {candidates.length === 0 ? (
            <p className="text-sm text-warning">
              没有读到可用的物理口。若刚做过探测失败，请先「修复」。
            </p>
          ) : (
            candidates.map((c) => (
              <label key={c.name} className="flex cursor-pointer items-start gap-2.5">
                <input
                  type="radio"
                  className="mt-1"
                  name="uplink"
                  checked={iface === c.name}
                  onChange={() => setIface(c.name)}
                />
                <span>
                  <span className="kc-mono text-base text-ink">
                    {c.name}
                    {c.speed && <span className="ml-2 text-xs text-ink-3">{c.speed}</span>}
                  </span>
                  {/* 有 IP 的候选标红：那通常意味着它是管理口，入桥就是
                      把自己关在门外。 */}
                  {c.has_ip && (
                    <span className="block text-sm text-danger">
                      该口上已配置 IP——很可能是管理口，入桥后会失去连接
                    </span>
                  )}
                  {!c.up && (
                    <span className="block text-sm text-warning">
                      链路未接通：入桥不会报错，但流量走不通
                    </span>
                  )}
                </span>
              </label>
            ))
          )}
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">自动回滚窗口（秒）</label>
          <input
            value={seconds}
            onChange={(e) => setSeconds(e.target.value)}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          />
          <p className="text-xs text-ink-3">
            60 ~ 1800 秒。窗口内不点「保持」，节点会自己把口摘出来——
            这是入桥之后发现网络断了时唯一的退路。
          </p>
        </div>

        {warnings && (
          <div className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2">
            <p className="text-base font-medium text-warning">接入前请确认</p>
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
              loading={attach.isPending}
              onClick={() => attach.mutate(true)}
            >
              我已确认，仍然接入
            </Button>
          </div>
        )}
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
