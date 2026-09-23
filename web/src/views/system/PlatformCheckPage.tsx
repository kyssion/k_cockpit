/**
 * PlatformCheckPage 平台自检与修复（F-4-13）。
 *
 * 页面的重心是**偏差**：面板说"已启用"而节点上其实没了的那类问题。它不
 * 报警、不被发现，直到用户自己撞上——因此这一页要做的第一件事就是把它们
 * 按影响程度排出来。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { platformCheckApi, type CheckItem } from '@/api/platformcheck'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { StatusBadge } from '@/components/common/StatusBadge'

export function PlatformCheckPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const enabled = effectiveNodeID > 0
  const ovs = useQuery({
    queryKey: ['ovs-status', effectiveNodeID],
    queryFn: () => platformCheckApi.ovsStatus(effectiveNodeID),
    enabled,
  })
  const ports = useQuery({
    queryKey: ['ovs-ports', effectiveNodeID],
    queryFn: () => platformCheckApi.ovsPorts(effectiveNodeID),
    enabled,
  })
  const leases = useQuery({
    queryKey: ['ovs-leases', effectiveNodeID],
    queryFn: () => platformCheckApi.leases(effectiveNodeID),
    enabled,
  })
  const clientIP = useQuery({ queryKey: ['client-ip'], queryFn: platformCheckApi.clientIP })

  const [result, setResult] = useState<CheckItem[] | null>(null)

  const runCheck = useMutation({
    mutationFn: () => platformCheckApi.check(effectiveNodeID),
    onSuccess: (r) => {
      setResult(r.Items ?? [])
      setError('')
      setNotice('')
    },
    onError: (e) => setError(describe(e)),
  })

  const repair = useMutation({
    mutationFn: () => platformCheckApi.repair(effectiveNodeID, []),
    onSuccess: () => {
      setError('')
      setNotice('已提交重新下发，稍后重新自检查看结果')
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (e) => setError(describe(e)),
  })

  if (nodes.isPending) return <PageLoading />
  const items = result ?? []
  const drifts = items.filter((i) => !i.OK)
  const repairable = drifts.filter((i) => i.Repairable)

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold text-ink">平台自检</h1>
          <p className="mt-1 text-base text-ink-3">
            核对控制面记录与节点实际状态是否一致。面板显示"已启用"而
            节点上其实没了的配置不会自己报警——自检就是去找它们。
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
          <Button size="sm" loading={runCheck.isPending} onClick={() => runCheck.mutate()}>
            开始自检
          </Button>
        </div>
      </header>

      {notice && <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {/* 自检结果：偏差排在最前，因为那是这一页存在的理由。 */}
      {result !== null && (
        <section className="flex flex-col gap-3">
          <header className="flex flex-wrap items-center justify-between gap-2">
            <h2 className="text-base font-medium text-ink-2">
              检查结果
              {drifts.length === 0 ? (
                <span className="ml-2 text-success">全部一致</span>
              ) : (
                <span className="ml-2 text-danger">{drifts.length} 处偏差</span>
              )}
            </h2>
            {repairable.length > 0 && (
              <Button size="sm" variant="secondary" loading={repair.isPending} onClick={() => repair.mutate()}>
                重新下发可修复的 {repairable.length} 项
              </Button>
            )}
          </header>

          {drifts.length === 0 ? (
            <EmptyState
              title="没有偏差"
              description="控制面记录的每一项都在节点上存在。"
            />
          ) : (
            <div className="overflow-hidden rounded-card border border-line">
              <table className="w-full text-left text-sm">
                <thead className="bg-sunken text-ink-3">
                  <tr>
                    <th className="px-3 py-2 font-normal">类别</th>
                    <th className="px-3 py-2 font-normal">对象</th>
                    <th className="px-3 py-2 font-normal">期望 / 实际</th>
                    <th className="px-3 py-2 font-normal">严重程度</th>
                    <th className="px-3 py-2 font-normal">可修复</th>
                  </tr>
                </thead>
                <tbody>
                  {drifts.map((it, i) => (
                    <tr key={`${it.Category}-${it.Target}-${i}`} className="border-t border-line">
                      <td className="px-3 py-2 text-ink-3">{it.Category}</td>
                      <td className="kc-mono px-3 py-2 text-ink-2">{it.Target}</td>
                      <td className="px-3 py-2">
                        <span className="text-ink-3">{it.Expected}</span>
                        <span className="mx-1 text-ink-3">/</span>
                        <span className="text-danger">{it.Actual}</span>
                      </td>
                      <td className="px-3 py-2">
                        <StatusBadge tone={it.Severity === 'critical' ? 'danger' : 'warning'}>
                          {it.Severity === 'critical' ? '严重' : '提醒'}
                        </StatusBadge>
                      </td>
                      {/* **不可修复的必须说明**：缺 OVS 装不上。标成可修复
                          会让用户点一下按钮，然后发现什么都没变。 */}
                      <td className="px-3 py-2 text-xs">
                        {it.Repairable ? (
                          <span className="text-success">可重新下发</span>
                        ) : (
                          <span className="text-ink-3" title={it.Fix}>
                            需在宿主机上处理
                          </span>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {drifts.some((i) => !i.Repairable) && (
            <p className="text-sm text-ink-3">
              标为「需在宿主机上处理」的项，面板做不了——它自己就跑在这台机器上。
              把鼠标移到那一栏可以看到具体要做什么。
            </p>
          )}
        </section>
      )}

      {/* 客户端 IP：**用途是防火墙白名单**，把这句话写出来才有价值。 */}
      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="text-base font-medium text-ink-2">你当前的地址</h2>
        <p className="mt-1 kc-mono text-base text-ink">{clientIP.data?.client_ip ?? '—'}</p>
        <p className="mt-1 text-sm text-ink-3">
          设置宿主机防火墙白名单时可以填这个地址。它与防火墙实际比对的口径一致
          ——自己另找的地址（如出口 IP）很可能对不上。
        </p>
        {clientIP.data?.note && (
          <p className="mt-2 rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            {clientIP.data.note}
          </p>
        )}
      </section>

      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="text-base font-medium text-ink-2">Open vSwitch</h2>
        {ovs.isPending ? (
          <PageLoading />
        ) : !ovs.data?.Available ? (
          <p className="mt-2 text-sm text-danger">
            {ovs.data?.Reason}
            {ovs.data?.Fix && <span className="ml-2 text-ink-3">{ovs.data.Fix}</span>}
          </p>
        ) : (
          <>
            <div className="mt-2 grid gap-x-6 gap-y-2 sm:grid-cols-4">
              <Stat label="版本" value={`v${ovs.data.Version}`} />
              {/* **可用与在跑分开显示**：装好了但服务挂了，所有依赖它的
                  功能都不生效——而探测只会说"可用"。 */}
              <Stat
                label="服务"
                value={ovs.data.ServiceActive ? '运行中' : '未运行'}
                tone={ovs.data.ServiceActive ? undefined : 'danger'}
              />
              <Stat
                label="包速率 meter"
                value={ovs.data.MeterAvailable ? '支持' : '不支持'}
                tone={ovs.data.MeterAvailable ? undefined : 'warning'}
              />
              <Stat
                label="规模"
                value={`${ovs.data.BridgeCount} 网桥 / ${ovs.data.PortCount} 端口 / ${ovs.data.FlowCount} 流表`}
              />
            </div>
            {!ovs.data.ServiceActive && (
              <p className="mt-2 rounded-control border border-danger/40 bg-danger/5 px-3 py-2 text-sm text-danger">
                OVS 已安装但服务没有运行。所有依赖它的功能（VPC 隔离、端口安全、
                端口镜像、包速率限制）都不会生效，而能力探测只会说"可用"。
              </p>
            )}
          </>
        )}
      </section>

      <section className="flex flex-col gap-3">
        <h2 className="text-base font-medium text-ink-2">
          端口（{ports.data?.items?.length ?? 0}）
        </h2>
        {(ports.data?.items ?? []).length === 0 ? (
          <EmptyState title="没有端口" description="该节点上还没有 OVS 端口。" />
        ) : (
          <div className="overflow-hidden rounded-card border border-line">
            <table className="w-full text-left text-sm">
              <thead className="bg-sunken text-ink-3">
                <tr>
                  <th className="px-3 py-2 font-normal">端口</th>
                  <th className="px-3 py-2 font-normal">网桥</th>
                  <th className="px-3 py-2 font-normal">类型</th>
                  <th className="px-3 py-2 font-normal">VLAN</th>
                  <th className="px-3 py-2 font-normal">虚拟机</th>
                </tr>
              </thead>
              <tbody>
                {(ports.data?.items ?? []).map((p) => (
                  <tr key={`${p.Bridge}-${p.Name}`} className="border-t border-line">
                    <td className="kc-mono px-3 py-1.5 text-ink-2">{p.Name}</td>
                    <td className="kc-mono px-3 py-1.5 text-ink-3">{p.Bridge}</td>
                    <td className="px-3 py-1.5 text-ink-3">{p.Type}</td>
                    <td className="px-3 py-1.5 text-ink-3">{p.Tag || '—'}</td>
                    <td className="px-3 py-1.5 text-ink-2">{p.VMName || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="flex flex-col gap-3">
        <h2 className="text-base font-medium text-ink-2">
          DHCP 租约（{leases.data?.items?.length ?? 0}）
        </h2>
        <p className="-mt-2 text-xs text-ink-3">
          排查 IP 冲突的第一步：看某个地址现在分配给了谁。
        </p>
        {(leases.data?.items ?? []).length === 0 ? (
          <EmptyState title="没有租约" description="当前没有活动租约。" />
        ) : (
          <div className="overflow-hidden rounded-card border border-line">
            <table className="w-full text-left text-sm">
              <thead className="bg-sunken text-ink-3">
                <tr>
                  <th className="px-3 py-2 font-normal">IP</th>
                  <th className="px-3 py-2 font-normal">MAC</th>
                  <th className="px-3 py-2 font-normal">主机名</th>
                  <th className="px-3 py-2 font-normal">到期</th>
                </tr>
              </thead>
              <tbody>
                {(leases.data?.items ?? []).map((l, i) => (
                  <tr key={`${l.IP}-${l.MAC}-${i}`} className="border-t border-line">
                    <td className="kc-mono px-3 py-1.5 text-ink-2">{l.IP}</td>
                    <td className="kc-mono px-3 py-1.5 text-ink-3">{l.MAC}</td>
                    <td className="px-3 py-1.5">
                      {/* **没有主机名的租约要标出来**：多台机器抢同一个地址时
                          它是最先需要看到的那条，而"主机名空着"正是它的特征。 */}
                      {l.Hostname ? (
                        <span className="text-ink-2">{l.Hostname}</span>
                      ) : (
                        <span className="text-warning" title="没有主机名的租约常见于手工配置或地址冲突">
                          未知（可能是手工配置或冲突）
                        </span>
                      )}
                    </td>
                    <td className="px-3 py-1.5 text-ink-3">{formatTime(l.ExpiresAt)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  )
}

function Stat({ label, value, tone }: { label: string; value: string; tone?: 'danger' | 'warning' }) {
  return (
    <div className="flex flex-col gap-0.5">
      <span className="text-xs text-ink-3">{label}</span>
      <span
        className={`text-sm ${
          tone === 'danger' ? 'text-danger' : tone === 'warning' ? 'text-warning' : 'text-ink'
        }`}
      >
        {value}
      </span>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}

function formatTime(s: string): string {
  if (!s) return '—'
  const t = Date.parse(s)
  if (!Number.isFinite(t)) return s
  return new Date(t).toLocaleString()
}
