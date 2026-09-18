/**
 * HostFirewallPage 管理宿主机防火墙（F-4-11 第一层）。
 *
 * 页面上第一件要说清的事是**它和 KVM 防火墙不是一套**：这一套保护的是
 * 宿主机自己与面板（SSH、面板端口），而「防火墙」那一页管的是虚拟机的
 * 入站流量。两者的配置项长得几乎一样，而攻击面完全不同——用户很容易以为
 * 改了那一页就关掉了面板的暴露面。
 *
 * 第二件是**保护规则不能删**：面板端口与 SSH 由服务端合成，删掉它们等于
 * 把管理员关在门外，而那种事故无法通过面板恢复。
 *
 * 第三件是**回滚入口要显眼**：它是"应用之后连不上了"时唯一的自救方式。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { hostFirewallApi, type HostFirewallRule, type PrecheckResult } from '@/api/hostfirewall'
import { nodeApi } from '@/api/node'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function HostFirewallPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [whitelist, setWhitelist] = useState('')
  const [addOpen, setAddOpen] = useState(false)
  const [precheck, setPrecheck] = useState<PrecheckResult | null>(null)
  const [confirmRollback, setConfirmRollback] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const data = useQuery({
    queryKey: ['host-firewall', effectiveNodeID],
    queryFn: () => hostFirewallApi.get(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })
  const conns = useQuery({
    queryKey: ['host-firewall-connections', effectiveNodeID],
    queryFn: () => hostFirewallApi.connections(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['host-firewall'] })
    void queryClient.invalidateQueries({ queryKey: ['host-firewall-connections'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const savePolicy = useMutation({
    mutationFn: (req: { enabled?: boolean; default_action?: string; whitelist?: string }) =>
      hostFirewallApi.updatePolicy(effectiveNodeID, req),
    onSuccess: () => {
      setError('')
      setNotice('策略已更新，记得点「预览并应用」才会生效')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const doPrecheck = useMutation({
    mutationFn: () => hostFirewallApi.precheck(effectiveNodeID),
    onSuccess: (r) => {
      setPrecheck(r)
      setError('')
    },
    onError: (e) => setError(describe(e)),
  })

  const doApply = useMutation({
    mutationFn: (version: number) => hostFirewallApi.apply(effectiveNodeID, version),
    onSuccess: () => {
      setPrecheck(null)
      setError('')
      setNotice('已下发到节点')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const doRollback = useMutation({
    mutationFn: () => hostFirewallApi.rollback(effectiveNodeID),
    onSuccess: () => {
      setConfirmRollback(false)
      setError('')
      setNotice('已回滚：本系统写入的规则全部撤销')
      refresh()
    },
    onError: (e) => {
      setConfirmRollback(false)
      setError(describe(e))
    },
  })

  const removeRule = useMutation({
    mutationFn: (id: number) => hostFirewallApi.deleteRule(effectiveNodeID, id),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const closeConn = useMutation({
    mutationFn: (addr: string) => hostFirewallApi.closeConnection(effectiveNodeID, addr),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  if (nodes.isPending) return <PageLoading />
  const policy = data.data?.policy
  const rules = data.data?.rules ?? []

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">宿主机防火墙</h1>
          <p className="mt-1 text-base text-ink-3">
            控制谁能连上这台宿主机——SSH、面板端口、节点上直接对外的服务。
            它与
            <a className="mx-1 text-primary hover:underline" href="/firewall">
              防火墙
            </a>
            那一页不是同一件事：那一页管的是虚拟机的入站流量。
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
          {/* 回滚是**紧急入口**，放在显眼处以备随时可用。 */}
          <Button variant="danger" size="sm" onClick={() => setConfirmRollback(true)}>
            紧急回滚
          </Button>
        </div>
      </header>

      {notice && <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>}
      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {data.isPending || !policy ? (
        <PageLoading />
      ) : (
        <>
          {policy.last_rollback_at && (
            <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
              这条策略在 {formatTime(policy.last_rollback_at)} 被紧急回滚过——
              如果你发现规则和自己配的不一样，先看这里。
            </p>
          )}

          <section className="rounded-card border border-line bg-surface p-4">
            <div className="flex flex-wrap items-center justify-between gap-3">
              <label className="flex cursor-pointer items-center gap-2.5">
                <input
                  type="checkbox"
                  checked={policy.enabled}
                  onChange={(e) => savePolicy.mutate({ enabled: e.target.checked })}
                />
                <span className="text-base text-ink">
                  启用宿主机防火墙
                  <span className="ml-2 text-sm text-ink-3">
                    （默认处置：{policy.default_action === 'deny' ? '拒绝' : '放行'}）
                  </span>
                </span>
              </label>
              <StatusBadge tone={policy.need_apply ? 'warning' : policy.applied_at ? 'success' : 'warning'}>
                {policy.need_apply || !policy.applied_at ? '有改动未应用' : '已生效'}
              </StatusBadge>
            </div>

            <div className="mt-3 flex flex-col gap-2">
              <label className="text-sm text-ink-2">
                管理白名单（每行一个 CIDR）
                <span className="ml-2 text-xs text-ink-3">
                  永远放行，优先于一切拒绝规则
                </span>
              </label>
              <textarea
                rows={2}
                value={whitelist || policy.whitelist.join('\n')}
                onChange={(e) => setWhitelist(e.target.value)}
                placeholder="203.0.113.9/32"
                className="kc-mono rounded-control border border-line-strong bg-sunken px-2 py-1.5 text-base text-ink"
              />
              <p className="text-xs text-warning">
                这一条是安全底线：宿主机防火墙挡住的正是 SSH 与面板本身，
                一旦被自己的规则挡在外面，就只剩进机房这一条路。
              </p>
              <div className="flex gap-2">
                <Button
                  size="sm"
                  variant="secondary"
                  loading={savePolicy.isPending}
                  onClick={() => savePolicy.mutate({ whitelist: whitelist || policy.whitelist.join('\n') })}
                >
                  保存白名单
                </Button>
                <select
                  value={policy.default_action}
                  onChange={(e) => savePolicy.mutate({ default_action: e.target.value })}
                  className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
                >
                  <option value="deny">默认拒绝（推荐）</option>
                  <option value="accept">默认放行</option>
                </select>
              </div>
            </div>
          </section>

          <section className="flex flex-col gap-3">
            <header className="flex flex-wrap items-center justify-between gap-2">
              <h2 className="text-base font-medium text-ink-2">规则（{rules.length}）</h2>
              <div className="flex gap-2">
                <Button size="sm" variant="secondary" loading={doPrecheck.isPending} onClick={() => doPrecheck.mutate()}>
                  预览并应用
                </Button>
                <Button size="sm" onClick={() => setAddOpen(true)}>
                  添加规则
                </Button>
              </div>
            </header>

            <div className="overflow-hidden rounded-card border border-line">
              <table className="w-full text-left text-sm">
                <thead className="bg-sunken text-ink-3">
                  <tr>
                    <th className="px-3 py-2 font-normal">动作</th>
                    <th className="px-3 py-2 font-normal">匹配</th>
                    <th className="px-3 py-2 font-normal">状态</th>
                    <th className="px-3 py-2 font-normal">说明</th>
                    <th className="px-3 py-2" />
                  </tr>
                </thead>
                <tbody>
                  {rules.map((r, i) => (
                    <RuleRow key={`${r.id}-${i}`} r={r} onDelete={() => removeRule.mutate(r.id)} />
                  ))}
                </tbody>
              </table>
            </div>
          </section>

          <section className="flex flex-col gap-3">
            <h2 className="text-base font-medium text-ink-2">
              当前连接（{conns.data?.items?.length ?? 0}）
            </h2>
            <p className="-mt-2 text-xs text-ink-3">
              关闭连接会切断正在进行的会话——包括你自己这条。
            </p>
            {(conns.data?.items ?? []).length === 0 ? (
              <EmptyState title="没有入站连接" description="当前没有活动连接。" />
            ) : (
              <div className="overflow-hidden rounded-card border border-line">
                <table className="w-full text-left text-sm">
                  <tbody>
                    {(conns.data?.items ?? []).map((c) => (
                      <tr key={c.remote_addr} className="border-b border-line last:border-0">
                        <td className="kc-mono px-3 py-1.5 text-ink-2">{c.remote_addr}</td>
                        <td className="px-3 py-1.5 text-ink-3">端口 {c.local_port}</td>
                        <td className="px-3 py-1.5 text-ink-3">{c.process}</td>
                        <td className="px-3 py-1.5">
                          {c.own && (
                            <span className="rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning">
                              你当前的连接
                            </span>
                          )}
                        </td>
                        <td className="px-3 py-1.5 text-right">
                          <button
                            className={c.own ? 'text-ink-3' : 'text-danger hover:underline'}
                            onClick={() => closeConn.mutate(c.remote_addr)}
                          >
                            关闭
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      )}

      <AddRuleModal
        open={addOpen}
        nodeID={effectiveNodeID}
        onClose={() => setAddOpen(false)}
        onDone={() => {
          setAddOpen(false)
          setError('')
          refresh()
        }}
        onError={(m) => {
          setAddOpen(false)
          setError(m)
        }}
      />

      {/* 预览弹窗：**应用之前**必须让人看到将要发生什么。 */}
      <Modal
        open={precheck !== null}
        title="将要下发"
        description="这些规则会追加到宿主机的 INPUT 链上。"
        onClose={() => setPrecheck(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setPrecheck(null)}>
              取消
            </Button>
            <Button
              size="sm"
              variant={(precheck?.warnings?.length ?? 0) > 0 ? 'danger' : 'primary'}
              loading={doApply.isPending}
              onClick={() => policy && doApply.mutate(policy.version)}
            >
              {(precheck?.warnings?.length ?? 0) > 0 ? '我已确认，仍然应用' : '应用'}
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-3">
          {(precheck?.warnings ?? []).map((w) => (
            <p
              key={w}
              className="rounded-control border border-danger/40 bg-danger/5 px-3 py-2 text-sm text-danger"
            >
              {w}
            </p>
          ))}
          <pre className="kc-mono max-h-72 overflow-auto whitespace-pre-wrap rounded-control bg-sunken px-3 py-2 text-xs text-ink-3">
            {(precheck?.preview ?? []).join('\n')}
          </pre>
        </div>
      </Modal>

      <Modal
        open={confirmRollback}
        title="紧急回滚"
        description="撤销本系统写入的全部宿主机规则，回到「没有宿主机防火墙」的状态。"
        onClose={() => setConfirmRollback(false)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setConfirmRollback(false)}>
              取消
            </Button>
            <Button variant="danger" size="sm" loading={doRollback.isPending} onClick={() => doRollback.mutate()}>
              确认回滚
            </Button>
          </>
        }
      >
        <p className="text-sm text-ink-3">
          这是唯一的自救入口：应用之后如果连不上面板或 SSH 断了，而你又进不去
          宿主机，回滚是唯一能把你放回来的操作。它不校验配置版本、也不要求二次验证
          ——刻意如此，因为它要在「已经出事了」的那一刻还能用。
        </p>
      </Modal>
    </div>
  )
}

function RuleRow({ r, onDelete }: { r: HostFirewallRule; onDelete: () => void }) {
  return (
    <tr className={`border-b border-line last:border-0 ${r.is_protected ? 'bg-warning/5' : ''}`}>
      <td className="px-3 py-2">
        <StatusBadge tone={r.action === 'accept' ? 'success' : 'warning'}>
          {r.action === 'accept' ? '放行' : '拒绝'}
        </StatusBadge>
      </td>
      <td className="kc-mono px-3 py-2 text-ink-2">{r.describe}</td>
      <td className="px-3 py-2">
        {r.fixed ? (
          <span className="text-xs text-ink-3">随配置生成</span>
        ) : r.applied ? (
          <span className="text-xs text-success">已生效</span>
        ) : (
          <span className="text-xs text-warning">未应用</span>
        )}
      </td>
      <td className="px-3 py-2 text-xs text-ink-3">
        {r.is_protected ? (
          <span className="text-warning">
            保护规则：{r.fixed_reason ?? '删除它可能让你连不上这台机器'}
          </span>
        ) : (
          r.remark ?? ''
        )}
      </td>
      <td className="px-3 py-2 text-right">
        {/* 保护规则**不给删除入口**：服务端也会拒绝，但让按钮可点再去报错，
            是在鼓励用户去试一个不可恢复的操作。 */}
        {r.is_protected ? (
          <span className="text-xs text-ink-3" title="保护规则不可删除">
            不可删除
          </span>
        ) : (
          <button className="text-danger hover:underline" onClick={onDelete}>
            删除
          </button>
        )}
      </td>
    </tr>
  )
}

function AddRuleModal({
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
  const [action, setAction] = useState<'accept' | 'deny'>('accept')
  const [protocol, setProtocol] = useState('tcp')
  const [portStart, setPortStart] = useState('')
  const [portEnd, setPortEnd] = useState('')
  const [source, setSource] = useState('')
  const [remark, setRemark] = useState('')

  const create = useMutation({
    mutationFn: () =>
      hostFirewallApi.createRule(nodeID, {
        action,
        protocol,
        port_start: portStart ? Number(portStart) : undefined,
        port_end: portEnd ? Number(portEnd) : undefined,
        source_cidr: source || undefined,
        remark: remark || undefined,
      }),
    onSuccess: onDone,
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title="添加宿主机规则"
      description="规则会追加到宿主机的 INPUT 链上，需要「预览并应用」之后才生效。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={create.isPending} onClick={() => create.mutate()}>
            添加
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex gap-4">
          <div className="flex flex-1 flex-col gap-1">
            <label className="text-sm text-ink-2">动作</label>
            <select
              value={action}
              onChange={(e) => setAction(e.target.value as 'accept' | 'deny')}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value="accept">放行</option>
              <option value="deny">拒绝</option>
            </select>
          </div>
          <div className="flex flex-1 flex-col gap-1">
            <label className="text-sm text-ink-2">协议</label>
            <select
              value={protocol}
              onChange={(e) => setProtocol(e.target.value)}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
              <option value="icmp">ICMP</option>
            </select>
          </div>
        </div>

        <div className="flex gap-3">
          <Input
            label="起始端口"
            value={portStart}
            onChange={(e) => setPortStart(e.target.value)}
            hint={protocol === 'icmp' ? 'ICMP 不区分端口，留空' : '留空表示全部端口'}
          />
          <Input label="结束端口" value={portEnd} onChange={(e) => setPortEnd(e.target.value)} />
        </div>

        <Input
          label="来源 CIDR"
          value={source}
          placeholder="留空表示任意来源"
          onChange={(e) => setSource(e.target.value)}
          hint="例如 203.0.113.0/24。留空会放行/拒绝**所有**来源——留空与忘了填在界面上长得一样，请确认。"
        />
        <Input label="备注" value={remark} onChange={(e) => setRemark(e.target.value)} />
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}

function formatTime(s: string): string {
  const t = Date.parse(s)
  if (!Number.isFinite(t)) return s
  return new Date(t).toLocaleString()
}
