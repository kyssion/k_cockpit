/**
 * FirewallPage 管理节点级防火墙（F-4-11）。
 *
 * 页面结构按「改配置 → 预检 → 应用」三步走，与后端的分工一致：
 * 改配置不产生任何网络影响，应用才有。合成一步会让管理员没法先看看
 * 改完是什么样。
 *
 * 两处刻意与其它页面不同：
 *
 *   - 保护规则**不渲染删除按钮**（服务端还会再拦一次，两处都有才对）；
 *   - **紧急回滚一键生效、不弹确认框**——被关在门外的人此刻只需要
 *     「先让我进去」。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  ACTION_LABEL,
  firewallApi,
  ruleText,
  type FirewallAction,
  type PolicyView,
} from '@/api/firewall'
import { nodeApi } from '@/api/node'
import { PROTOCOL_LABEL, type Protocol } from '@/api/securitygroup'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'

export function FirewallPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [draft, setDraft] = useState<PolicyView | null>(null)
  const [whitelistText, setWhitelistText] = useState('')
  const [regionsText, setRegionsText] = useState('')
  const [ruleOpen, setRuleOpen] = useState(false)
  const [warnings, setWarnings] = useState<string[] | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const policy = useQuery({
    queryKey: ['firewall-policy', effectiveNodeID],
    queryFn: () => firewallApi.getPolicy(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const rules = useQuery({
    queryKey: ['firewall-rules', effectiveNodeID],
    queryFn: () => firewallApi.listRules(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['firewall-policy'] })
    void queryClient.invalidateQueries({ queryKey: ['firewall-rules'] })
  }

  const current = draft ?? policy.data ?? null

  // 服务端数据变化时重置本地草稿（React 官方的「随 prop 变化调整 state」
  // 写法：在渲染期比对**上一次见到的**服务端对象，**用守卫避免循环**）。
  //
  // 不用 effect：effect 里 setState 会额外渲染一轮，而且切换节点时会先
  // 渲染一次**上一个节点**的旧数据——那一帧里用户看到的用户名与选中的
  // 节点对不上。渲染期重置没有这个问题，它和这次渲染是同一帧。
  const [seenPolicy, setSeenPolicy] = useState<PolicyView | undefined>(policy.data)
  if (policy.data !== seenPolicy) {
    setSeenPolicy(policy.data)
    if (policy.data) {
      setDraft(policy.data)
      setWhitelistText(policy.data.whitelist.join('\n'))
      setRegionsText(policy.data.geoip_regions.join(','))
    }
  }

  const save = useMutation({
    mutationFn: () =>
      firewallApi.updatePolicy(effectiveNodeID, {
        enabled: current?.enabled,
        default_action: current?.default_action,
        whitelist: splitList(whitelistText),
        geoip_regions: splitList(regionsText),
      }),
    onSuccess: (view) => {
      setDraft(view)
      setError('')
      setNotice('配置已保存；改动尚未下发到节点')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const precheck = useMutation({
    mutationFn: () => firewallApi.precheck(effectiveNodeID),
    onSuccess: (result) => {
      setError('')
      // 没有警告时直接下发，不必让用户多点一次——预检的意义是拦住危险，
      // 而不是给每一步都加一个确认框。
      if (result.warnings.length === 0) {
        apply.mutate(true)
        return
      }
      setWarnings(result.warnings)
    },
    onError: (err) => setError(describe(err)),
  })

  const apply = useMutation({
    mutationFn: (ack: boolean) => firewallApi.apply(effectiveNodeID, ack),
    onSuccess: (result) => {
      setWarnings(null)
      setError('')
      setNotice(result.applied ? '已下发到节点' : '未下发')
      refresh()
    },
    onError: (err) => {
      setWarnings(null)
      setError(describe(err))
    },
  })

  const rollback = useMutation({
    mutationFn: () => firewallApi.rollback(effectiveNodeID),
    onSuccess: () => {
      setWarnings(null)
      setError('')
      setNotice('已紧急关闭防火墙并撤销本次下发')
      setDraft(null)
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const removeRule = useMutation({
    mutationFn: (ruleID: number) => firewallApi.deleteRule(effectiveNodeID, ruleID),
    onSuccess: () => {
      setError('')
      setNotice('规则已删除；改动尚未下发')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (nodes.isPending || policy.isPending) return <PageLoading />

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold text-ink">防火墙</h1>
          <p className="mt-1 text-base text-ink-3">
            节点级策略作用在该节点上的所有虚拟机。默认处置为
            <span className="text-ink-2">拒绝</span>，未被规则放行的来源一律不通；
            白名单里的来源<span className="text-ink-2">永远放行</span>。
          </p>
        </div>
        <div className="flex items-end gap-2">
          <div className="flex flex-col gap-1">
            <label className="text-xs text-ink-3">节点</label>
            <select
              value={effectiveNodeID}
              onChange={(e) => {
                setNodeID(Number(e.target.value))
                setDraft(null)
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
            variant="danger"
            size="sm"
            loading={rollback.isPending}
            // **不弹确认框。** 被自己配错的防火墙关在门外的管理员，
            // 此刻唯一的诉求是「先让我进去」，而任何一道额外确认都会
            // 成为压垮他的那一步。
            onClick={() => rollback.mutate()}
          >
            紧急关闭
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

      {/* 未下发的改动要明确标出：它是排查「为什么规则没生效」的第一个路口。 */}
      {current?.pending && (
        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-base text-warning">
          有改动尚未下发到节点
          {current.unapplied_rules > 0 && `（${current.unapplied_rules} 条规则未生效）`}
          。
        </p>
      )}

      <div className="grid gap-5 lg:grid-cols-2">
        <section className="flex flex-col gap-3 rounded-card border border-line bg-surface p-4">
          <h2 className="text-sm text-ink-3">策略配置</h2>

          <label className="flex cursor-pointer items-center gap-2.5">
            <input
              type="checkbox"
              checked={current?.enabled ?? false}
              onChange={(e) => setDraft({ ...current!, enabled: e.target.checked })}
            />
            <span className="text-base text-ink">
              启用防火墙
              <span className="ml-2 text-sm text-ink-3">
                关闭时不拦截任何流量，规则保留
              </span>
            </span>
          </label>

          <div className="flex flex-col gap-1.5">
            <span className="text-base text-ink">默认处置</span>
            {(['deny', 'accept'] as FirewallAction[]).map((a) => (
              <label key={a} className="flex cursor-pointer items-start gap-2.5">
                <input
                  type="radio"
                  className="mt-1"
                  name="fw-default"
                  checked={current?.default_action === a}
                  onChange={() => setDraft({ ...current!, default_action: a })}
                />
                <span>
                  <span className="block text-base text-ink">{ACTION_LABEL[a]}</span>
                  <span className="block text-sm text-ink-3">
                    {a === 'deny'
                      ? '不匹配任何放行规则的流量一律拒绝。防火墙的价值就在这一档。'
                      : '只拒绝匹配到「拒绝」规则的流量。看似宽松，实际能防住的比想象中少。'}
                  </span>
                </span>
              </label>
            ))}
          </div>

          <div className="flex flex-col gap-3">
            <WhitelistInput value={whitelistText} onChange={setWhitelistText} />
            <Input
              label="允许的区域（可选）"
              value={regionsText}
              placeholder="JP,SG —— 留空表示不按区域限制"
              onChange={(e) => setRegionsText(e.target.value)}
              hint="区域数据不总是准确。若你的来源被误判，你会连不上面板——把管理 IP 加进白名单。"
            />
          </div>

          <div className="flex items-center justify-between gap-3">
            <span className="text-xs text-ink-3">保存只改配置，不产生网络影响。</span>
            <div className="flex gap-2">
              <Button
                variant="secondary"
                size="sm"
                disabled={draft === null}
                onClick={() => {
                  setDraft(policy.data ?? null)
                  setWhitelistText(policy.data?.whitelist.join('\n') ?? '')
                  setRegionsText(policy.data?.geoip_regions.join(',') ?? '')
                }}
              >
                撤销修改
              </Button>
              <Button size="sm" loading={save.isPending} onClick={() => save.mutate()}>
                保存
              </Button>
            </div>
          </div>

          <div className="border-t border-line pt-3">
            <Button
              size="sm"
              loading={precheck.isPending || apply.isPending}
              onClick={() => precheck.mutate()}
            >
              预检并下发
            </Button>
            <p className="mt-1.5 text-xs text-ink-3">
              预检会检查这次改动会不会切断管理通道（当前来源、白名单、保护规则）。
              有风险时会先让你确认。
            </p>
          </div>
        </section>

        <section className="flex flex-col gap-3">
          <div className="flex items-baseline justify-between">
            <h2 className="text-sm text-ink-3">节点级规则</h2>
            <Button variant="secondary" size="sm" onClick={() => setRuleOpen(true)}>
              新增规则
            </Button>
          </div>

          {rules.isPending ? (
            <PageLoading />
          ) : (rules.data?.items ?? []).length === 0 ? (
            <EmptyState
              title="还没有规则"
              description="启用防火墙前，建议先确认管理通道（面板与 SSH 端口）有规则放行。"
            />
          ) : (
            <div className="overflow-x-auto rounded-card border border-line">
              <table className="w-full border-collapse text-base">
                <thead>
                  <tr className="bg-sunken text-left text-xs text-ink-2">
                    <th className="px-4 py-2.5 font-medium">动作</th>
                    <th className="px-4 py-2.5 font-medium">规则</th>
                    <th className="px-4 py-2.5 font-medium">状态</th>
                    <th className="px-4 py-2.5 font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {(rules.data?.items ?? []).map((r) => (
                    <tr
                      key={r.id}
                      // 保护规则整行加底色：它不是一个普通条目，而是「不要动这里」。
                      className={`border-t border-line ${r.is_protected ? 'bg-warning/5' : ''}`}
                    >
                      <td className="px-4 py-2.5">
                        <span
                          className={r.action === 'accept' ? 'text-success' : 'text-danger'}
                        >
                          {ACTION_LABEL[r.action]}
                        </span>
                      </td>
                      <td className="px-4 py-2.5">
                        <span className="kc-mono text-ink">{ruleText(r)}</span>
                        {r.is_protected && (
                          <span className="ml-2 rounded-pill bg-warning/15 px-1.5 py-0.5 text-xs text-warning">
                            系统保护
                          </span>
                        )}
                        {r.remark && (
                          <span className="block text-xs text-ink-3">{r.remark}</span>
                        )}
                      </td>
                      <td className="px-4 py-2.5 text-sm">
                        {r.applied ? (
                          <span className="text-ink-3">已生效</span>
                        ) : (
                          <span className="text-warning">未下发</span>
                        )}
                      </td>
                      <td className="px-4 py-2.5">
                        {/* 保护规则**不渲染删除按钮**。服务端还会再拦一次
                            ——两处都有才对：界面不给入口是为了不诱导点击，
                            服务端拦截是为了让绕过界面也删不掉。 */}
                        {r.is_protected ? (
                          <span
                            className="text-sm text-ink-3"
                            title="保护管理通道的规则，不可删除"
                          >
                            不可删除
                          </span>
                        ) : (
                          <button
                            className="text-sm text-danger hover:underline"
                            disabled={removeRule.isPending}
                            onClick={() => removeRule.mutate(r.id)}
                          >
                            删除
                          </button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </section>
      </div>

      {/* 预检警告：必须显式确认才下发。 */}
      <Modal
        open={warnings !== null}
        title="这次改动可能切断管理通道"
        description="以下问题不代表配置错了——有时「就是要挡掉这些」正是你的意图。但请确认你看过它们。"
        onClose={() => setWarnings(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setWarnings(null)}>
              返回修改
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={apply.isPending}
              onClick={() => apply.mutate(true)}
            >
              我已确认，仍然下发
            </Button>
          </>
        }
      >
        <ul className="flex flex-col gap-2">
          {(warnings ?? []).map((w) => (
            <li
              key={w}
              className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-base text-warning"
            >
              {w}
            </li>
          ))}
        </ul>
        <p className="mt-3 text-sm text-ink-3">
          如果下发之后连不上面板，用页面右上角的「紧急关闭」——它会立刻关闭防火墙，
          不需要任何确认。
        </p>
      </Modal>

      <AddRuleModal
        open={ruleOpen}
        nodeID={effectiveNodeID}
        onClose={() => setRuleOpen(false)}
        onDone={() => {
          setRuleOpen(false)
          setError('')
          setNotice('规则已添加；改动尚未下发')
          refresh()
        }}
        onError={(msg) => {
          setRuleOpen(false)
          setError(msg)
        }}
      />
    </div>
  )
}

/** WhitelistInput 是一个多行白名单输入。 */
function WhitelistInput({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <div className="flex flex-col gap-1">
      <label className="text-sm text-ink-2">白名单（每行一个 IP 或网段）</label>
      <textarea
        value={value}
        rows={3}
        placeholder={'203.0.113.9\n10.0.0.0/8'}
        onChange={(e) => onChange(e.target.value)}
        className="rounded-control border border-line-strong bg-sunken px-2 py-1.5 font-mono text-base text-ink"
      />
      <p className="text-xs text-ink-3">
        白名单<span className="text-ink-2">优先于一切拒绝</span>，包括区域限制。
        建议把你自己当前的出口 IP 放进来——万一区域数据把管理来源判错了，
        这条能让你还进得来。
      </p>
    </div>
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
  onError: (message: string) => void
}) {
  const [action, setAction] = useState<FirewallAction>('accept')
  const [protocol, setProtocol] = useState<Protocol>('tcp')
  const [portStart, setPortStart] = useState('')
  const [portEnd, setPortEnd] = useState('')
  const [source, setSource] = useState('0.0.0.0/0')
  const [remark, setRemark] = useState('')

  const usesPorts = protocol === 'tcp' || protocol === 'udp'

  const create = useMutation({
    mutationFn: () =>
      firewallApi.createRule(nodeID, {
        action,
        protocol,
        ...(usesPorts && portStart.trim() !== ''
          ? {
              port_start: Number(portStart),
              port_end: portEnd.trim() === '' ? Number(portStart) : Number(portEnd),
            }
          : {}),
        source_cidr: source.trim(),
        remark: remark.trim() || undefined,
      }),
    onSuccess: onDone,
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title="新增防火墙规则"
      description="默认处置为拒绝时，规则就是一份放行清单——只写需要放行的，其余自然不通。"
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
      <div className="flex flex-col gap-3">
        <div className="flex gap-4">
          <div className="flex flex-1 flex-col gap-1">
            <label className="text-sm text-ink-2">动作</label>
            <select
              value={action}
              onChange={(e) => setAction(e.target.value as FirewallAction)}
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
              onChange={(e) => setProtocol(e.target.value as Protocol)}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {(Object.keys(PROTOCOL_LABEL) as Protocol[]).map((p) => (
                <option key={p} value={p}>
                  {PROTOCOL_LABEL[p]}
                </option>
              ))}
            </select>
          </div>
        </div>

        <div className="flex gap-4">
          <div className="flex flex-1">
            <Input
              label="起始端口"
              value={portStart}
              disabled={!usesPorts}
              placeholder={usesPorts ? '22' : '该协议无端口'}
              onChange={(e) => setPortStart(e.target.value)}
            />
          </div>
          <div className="flex flex-1">
            <Input
              label="结束端口"
              value={portEnd}
              disabled={!usesPorts}
              placeholder={usesPorts ? '留空即等于起始端口' : '该协议无端口'}
              onChange={(e) => setPortEnd(e.target.value)}
            />
          </div>
        </div>

        <Input
          label="来源"
          value={source}
          onChange={(e) => setSource(e.target.value)}
          hint="IP 或网段。裸地址会自动补成 /32 或 /128。"
        />
        <Input label="备注（可选）" value={remark} onChange={(e) => setRemark(e.target.value)} />
      </div>
    </Modal>
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
