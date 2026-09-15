import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  CAPABILITY_STATE_LABEL,
  CAPABILITY_STATE_TONE,
  networkApi,
  type Capability,
} from '@/api/network'
import { nodeApi } from '@/api/node'
import { PageLoading } from '@/components/common/Feedback'
import { StatusBadge } from '@/components/common/StatusBadge'

export function NetworkPage() {
  const [nodeID, setNodeID] = useState(0)
  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })

  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const status = useQuery({
    queryKey: ['network-status', effectiveNodeID],
    queryFn: () => networkApi.status(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const networks = useQuery({
    queryKey: ['networks', effectiveNodeID],
    queryFn: () => networkApi.networks(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  if (nodes.isPending) return <PageLoading />

  return (
    <div className="flex max-w-[880px] flex-col gap-5">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">网络中心</h1>
          <p className="mt-1 text-base text-ink-3">
            节点的网络后端与能力状态。缺少依赖不会影响面板本身，只会让对应的网络功能不可用。
          </p>
        </div>

        <select
          value={effectiveNodeID}
          onChange={(e) => setNodeID(Number(e.target.value))}
          className="h-8 shrink-0 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
        >
          {(nodes.data ?? []).map((n) => (
            <option key={n.id} value={n.id}>
              {n.name}
            </option>
          ))}
        </select>
      </header>

      {effectiveNodeID === 0 && (
        <p className="rounded-card border border-dashed border-line-strong px-4 py-6 text-center text-base text-ink-3">
          还没有节点。网络能力建立在节点上，请先接入节点。
        </p>
      )}

      {status.isPending && effectiveNodeID > 0 && <PageLoading />}

      {status.isError && (
        <p className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(status.error)}
        </p>
      )}

      {status.data && (
        <>
          <section className="rounded-card border border-line">
            <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
              <h2 className="text-sm font-medium text-ink-2">后端模式</h2>
              {/* 探测失败时不能宣称「降级」——我们并不知道它缺什么，
                  断言降级会诱导用户去做无谓的修复。 */}
              {status.data.probe_failed ? (
                <StatusBadge tone="idle">无法确认</StatusBadge>
              ) : status.data.degraded ? (
                <StatusBadge tone="warning">功能受限</StatusBadge>
              ) : (
                <StatusBadge tone="success">正常</StatusBadge>
              )}
            </div>

            <div className="flex flex-col gap-2 px-4 py-3.5 text-base">
              <div className="flex gap-3">
                <span className="w-24 text-ink-3">当前模式</span>
                <span className="text-ink">{status.data.mode_label}</span>
              </div>
              {status.data.probe_failed && (
                <p className="rounded-control bg-warning/10 px-3 py-2 text-warning">
                  无法探测节点网络能力：{status.data.probe_message}
                  <span className="mt-1 block text-ink-2">
                    这不代表缺少依赖——只说明这次没能确认。节点恢复后可重新查看。
                  </span>
                </p>
              )}
            </div>
          </section>

          <section className="flex flex-col gap-2">
            <h2 className="text-sm font-medium text-ink-2">能力清单</h2>
            <div className="flex flex-col gap-2">
              {status.data.capabilities.map((cap) => (
                <CapabilityCard key={cap.key} capability={cap} />
              ))}
            </div>
          </section>
        </>
      )}

      {networks.data && (
        <section className="flex flex-col gap-2">
          <h2 className="text-sm font-medium text-ink-2">网络</h2>
          <div className="overflow-x-auto rounded-card border border-line">
            <table className="w-full border-collapse text-base">
              <thead>
                <tr className="bg-sunken text-left text-xs text-ink-2">
                  <th className="px-4 py-2.5 font-medium">名称</th>
                  <th className="px-4 py-2.5 font-medium">网桥</th>
                  <th className="px-4 py-2.5 font-medium">模式</th>
                  <th className="px-4 py-2.5 font-medium">网段</th>
                  <th className="px-4 py-2.5 font-medium">说明</th>
                </tr>
              </thead>
              <tbody>
                {networks.data.map((nw) => (
                  <tr key={nw.id} className="border-t border-line">
                    <td className="px-4 py-2.5">
                      <span className="text-ink">{nw.name}</span>
                      {nw.is_system && <span className="ml-2 text-xs text-brand">系统</span>}
                    </td>
                    <td className="kc-mono px-4 py-2.5 text-ink-2">{nw.bridge_name}</td>
                    <td className="px-4 py-2.5 text-ink-2">{modeLabel(nw.mode)}</td>
                    <td className="kc-mono px-4 py-2.5 text-ink-2">{nw.cidr || '—'}</td>
                    <td className="px-4 py-2.5 text-xs text-ink-3">
                      {nw.is_system ? '未指定网络时的默认落点，不可删除' : '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </div>
  )
}

/**
 * 能力卡片。
 *
 * 缺失时给三样东西：缺什么、影响什么、怎么修（f-4-01 R-011）。
 * 只说「不可用」会让用户只能靠猜——而能打开这个页面的人，本来就是要
 * 执行修复命令的人。
 */
function CapabilityCard({ capability }: { capability: Capability }) {
  const missing = capability.state === 'unavailable'

  return (
    <div className="rounded-card border border-line px-4 py-3">
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <span className="text-base font-medium text-ink">{capability.label}</span>
          {!capability.required && <span className="text-xs text-ink-3">可选</span>}
        </div>
        <StatusBadge tone={CAPABILITY_STATE_TONE[capability.state]}>
          {CAPABILITY_STATE_LABEL[capability.state]}
        </StatusBadge>
      </div>

      {missing && (
        <div className="mt-2 flex flex-col gap-2 text-base">
          <p className="text-ink-2">{capability.reason}</p>

          {capability.affected_features && capability.affected_features.length > 0 && (
            <p className="text-ink-3">
              受影响的功能：
              <span className="text-ink-2">{capability.affected_features.join('、')}</span>
            </p>
          )}

          {capability.fix && (
            <div className="flex flex-col gap-1">
              <span className="text-xs text-ink-3">修复方式</span>
              <code className="kc-mono select-all rounded-control bg-sunken px-3 py-2 text-sm text-ink">
                {capability.fix}
              </code>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function modeLabel(mode: string): string {
  switch (mode) {
    case 'nat':
      return 'NAT 出网'
    case 'physical':
      return '桥接物理网卡'
    case 'empty':
      return '隔离'
    default:
      return mode
  }
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
