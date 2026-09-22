/**
 * PortSecurityPage 配置端口安全（F-4-08）。
 *
 * 页面上要处理的核心是**三项保护的性质不同**：
 *
 *   防伪造  → 默认应当开。它是其它隔离措施的前提，且不会断开任何连接。
 *   隔离    → 后果最大的一项：同网段内所有机器互不可见，包括用户自己
 *             放在一起的应用集群。必须让他在按下去之前就看到这一点。
 *   限速    → 资源保护而非安全措施。默认不限。
 *
 * 另一件必须在按下"启用"之前就看到的事：**能力是否具备**。端口安全依赖
 * Open vSwitch，而节点上很可能没装——等到点下去才被告知"不支持"，用户
 * 已经以为配好了。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { platformCheckApi } from '@/api/platformcheck'
import {
  portSecurityApi,
  PS_LIMIT_BOUNDS,
  PS_STATUS_LABEL,
  type PortSecurityPrecheck,
  type PortSecurityPolicyView,
} from '@/api/portsecurity'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function PortSecurityPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [editing, setEditing] = useState<PortSecurityPolicyView | null>(null)
  const [creating, setCreating] = useState(false)
  const [error, setError] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const list = useQuery({
    queryKey: ['port-security', effectiveNodeID],
    queryFn: () => portSecurityApi.list(effectiveNodeID),
    enabled: effectiveNodeID > 0,
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((p) => p.status === 'pending') ? 8000 : false,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['port-security'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const disable = useMutation({
    mutationFn: (id: number) => portSecurityApi.disable(id),
    onSuccess: () => {
      setError('')
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
          <h1 className="text-lg font-semibold text-ink">端口安全</h1>
          <p className="mt-1 text-base text-ink-3">
            <span className="text-ink-2">源地址防伪造</span>阻止虚拟机冒用别人的地址
            ——它是别的隔离措施的前提；
            <span className="text-ink-2">端口隔离</span>让该网口与同网段所有机器互不可见；
            <span className="text-ink-2">包速率限制</span>限制该网口的发包速率。
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
          <Button size="sm" disabled={effectiveNodeID === 0} onClick={() => setCreating(true)}>
            配置端口安全
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
          title="还没有端口安全策略"
          description="默认情况下虚拟机的源地址不受校验，也不会与邻居隔离——这是为了不改变现有网络的连通性。需要收紧时在这里配置。"
        />
      ) : (
        <div className="flex flex-col gap-3">
          {items.map((p) => (
            <PolicyCard
              key={p.id}
              policy={p}
              onEdit={() => setEditing(p)}
              onDisable={() => disable.mutate(p.id)}
              busy={disable.isPending}
            />
          ))}
        </div>
      )}

      <PolicyModal
        open={creating || editing !== null}
        nodeID={effectiveNodeID}
        policy={editing}
        onClose={() => {
          setCreating(false)
          setEditing(null)
        }}
        onDone={() => {
          setCreating(false)
          setEditing(null)
          setError('')
          refresh()
        }}
        onError={(m) => {
          setCreating(false)
          setEditing(null)
          setError(m)
        }}
      />
    </div>
  )
}

function PolicyCard({
  policy,
  onEdit,
  onDisable,
  busy,
}: {
  policy: PortSecurityPolicyView
  onEdit: () => void
  onDisable: () => void
  busy: boolean
}) {
  const tags: { label: string; tone: 'good' | 'warn' }[] = []
  if (policy.spoofing_guard) tags.push({ label: '源地址防伪造', tone: 'good' })
  if (policy.isolation) tags.push({ label: '端口隔离', tone: 'warn' })
  if (policy.pps_limit > 0) tags.push({ label: `${policy.pps_limit} pps 限速`, tone: 'good' })

  return (
    <div className="rounded-card border border-line bg-surface p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <span className="flex flex-wrap items-baseline gap-2">
          <span className="kc-mono text-base text-ink">{policy.port_ref}</span>
          {policy.vm_name && <span className="text-sm text-ink-3">{policy.vm_name}</span>}
          <StatusBadge tone={policy.status === 'active' ? 'success' : policy.status === 'failed' ? 'danger' : 'warning'}>
            {PS_STATUS_LABEL[policy.status] ?? policy.status}
          </StatusBadge>
        </span>
        <span className="flex gap-3 text-sm">
          <button className="text-primary hover:underline" onClick={onEdit}>
            修改
          </button>
          <button className="text-danger hover:underline" onClick={onDisable} disabled={busy}>
            停用
          </button>
        </span>
      </div>

      <div className="mt-2 flex flex-wrap gap-1.5">
        {tags.length === 0 ? (
          <span className="text-sm text-ink-3">未启用任何保护</span>
        ) : (
          tags.map((t) => (
            <span
              key={t.label}
              className={`rounded-pill px-1.5 py-0.5 text-xs ${
                t.tone === 'warn' ? 'bg-warning/10 text-warning' : 'bg-success/10 text-success'
              }`}
            >
              {t.label}
            </span>
          ))
        )}
      </div>

      {/* 「未生效」与「已启用」是两回事，分开说。 */}
      {policy.status === 'pending' && (
        <p className="mt-1.5 text-sm text-warning">
          配置已保存但尚未在节点上生效——节点上跑的还是上一次的规则。
        </p>
      )}
      {policy.status === 'failed' && policy.detail && (
        <p className="mt-1.5 text-sm text-danger">下发失败：{policy.detail}</p>
      )}
    </div>
  )
}

function PolicyModal({
  open,
  nodeID,
  policy,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  policy: PortSecurityPolicyView | null
  onClose: () => void
  onDone: () => void
  onError: (m: string) => void
}) {
  const [portRef, setPortRef] = useState('')
  const [spoofing, setSpoofing] = useState(true)
  const [isolation, setIsolation] = useState(false)
  const [pps, setPPS] = useState('')
  const [check, setCheck] = useState<PortSecurityPrecheck | null>(null)

  // 节点的 OVS 端口列表（G-38）：给网口输入提供候选，也顺带展示口与
  // 虚拟机的对应关系。
  const ports = useQuery({
    queryKey: ['ovs-ports', nodeID],
    queryFn: () => platformCheckApi.ovsPorts(nodeID),
    enabled: open && nodeID > 0,
  })

  // 编辑时用已有值初始化。
  const [seeded, setSeeded] = useState<number | null>(null)
  if (seeded !== (policy?.id ?? 0)) {
    setSeeded(policy?.id ?? 0)
    setPortRef(policy?.port_ref ?? '')
    setSpoofing(policy ? policy.spoofing_guard : true)
    setIsolation(policy?.isolation ?? false)
    setPPS(policy && policy.pps_limit > 0 ? String(policy.pps_limit) : '')
    setCheck(null)
  }

  const ppsNum = Number(pps) || 0
  const ppsBad =
    ppsNum > 0 && (ppsNum < PS_LIMIT_BOUNDS.min || ppsNum > PS_LIMIT_BOUNDS.max)
  const request = {
    port_ref: portRef.trim(),
    spoofing_guard: spoofing,
    isolation,
    pps_limit: ppsNum,
  }
  const ready = request.port_ref !== '' && !ppsBad

  const preview = useMutation({
    mutationFn: () => portSecurityApi.preview(nodeID, request),
    onSuccess: setCheck,
    onError: (e) => onError(describe(e)),
  })
  const apply = useMutation({
    mutationFn: (ack: boolean) => portSecurityApi.apply(nodeID, request, ack),
    onSuccess: (r) => {
      // 有警告而未确认：服务端返回预检但没有任务。
      if (r.precheck?.warnings?.length && !r.task_id) {
        setCheck(r.precheck)
        return
      }
      onDone()
    },
    onError: (e) => onError(describe(e)),
  })

  const blocked = check !== null && !check.can_apply
  const needConfirm = !!check?.warnings?.length && !blocked

  return (
    <Modal
      open={open}
      title={policy ? `修改 ${policy.port_ref} 的端口安全` : '配置端口安全'}
      description="规则会写入节点的 OpenFlow 流表。应用后以控制面的规则为准。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          {needConfirm ? (
            <Button variant="danger" size="sm" loading={apply.isPending} onClick={() => apply.mutate(true)}>
              我已确认，仍然启用
            </Button>
          ) : (
            <Button
              size="sm"
              disabled={!ready || blocked}
              loading={preview.isPending || apply.isPending}
              onClick={() => (check ? apply.mutate(true) : preview.mutate())}
            >
              {check ? '启用' : '预检'}
            </Button>
          )}
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">网口</label>
          <input
            list="port-ref-options"
            value={portRef}
            onChange={(e) => {
              setPortRef(e.target.value)
              setCheck(null)
            }}
            placeholder="例如 vnet0"
            className="h-9 rounded-control border border-line-strong bg-sunken px-2.5 text-base text-ink focus:outline-none focus-visible:border-brand"
          />
          {/* 从节点的 OVS 端口列表联动带出（G-38）：vnet 口与虚拟机的对应
              关系只有节点知道，手填的话拼错一个字符，策略就会套在一个
              不存在的口上——预检能发现，但那已经是第二次点击。 */}
          <datalist id="port-ref-options">
            {(ports.data?.items ?? []).map((p) => (
              <option key={p.Name} value={p.Name}>
                {p.VMName ? `虚拟机 ${p.VMName}` : p.Bridge}
              </option>
            ))}
          </datalist>
          <span className="text-xs text-ink-3">
            虚拟机网口的接口名，可从下拉候选中选择（来自节点 OVS 端口列表）。
          </span>
        </div>

        <Toggle
          checked={spoofing}
          onChange={(v) => {
            setSpoofing(v)
            setCheck(null)
          }}
          label="源地址防伪造"
          description="只允许该网口使用属于它的源 IP 与源 MAC。这是其它隔离措施的前提——不防欺骗的话，针对邻居做的隔离、防火墙、端口转发都会指向错误的机器。不会断开任何现有连接，建议保持开启。"
          tone="good"
        />

        <Toggle
          checked={isolation}
          onChange={(v) => {
            setIsolation(v)
            setCheck(null)
          }}
          label="端口隔离"
          description="该网口与其所在二层网段内的其它机器互不可见。代价比看起来大：同网段内所有机器之间都不通了，包括您自己放在一起、需要互通的应用（集群、主从、心跳）——它们的连接会在启用后立刻断开。"
          tone="warn"
        />

        <div className="flex flex-col gap-1">
          <Input
            label="包速率限制（pps）"
            value={pps}
            placeholder="留空表示不限"
            onChange={(e) => {
              setPPS(e.target.value)
              setCheck(null)
            }}
            hint={`每秒包数，${PS_LIMIT_BOUNDS.min} ~ ${PS_LIMIT_BOUNDS.max}。留空表示不限速——默认不限，因为默认限流会在您什么都没做的时候开始丢包，而那种丢包看起来像应用的问题。`}
          />
          {ppsBad && (
            <p className="text-xs text-warning">
              超出范围。低于 {PS_LIMIT_BOUNDS.min} pps 会让 SSH 这类交互式会话卡到不可用
              ——请确认没有把单位想错（这里填的是每秒包数，不是每秒千包）。
            </p>
          )}
        </div>

        {check && (
          <>
            {/* **能力状态先于一切**：不具备时不能启用，而且要说清装什么。 */}
            <div className="rounded-control border border-line bg-sunken px-3 py-2">
              <p className="mb-1.5 text-sm text-ink-2">节点能力</p>
              <ul className="flex flex-col gap-1">
                {check.capabilities.map((c) => (
                  <li key={c.key} className="text-sm">
                    <span className={c.missing && c.required ? 'text-danger' : 'text-ink-2'}>
                      {c.missing ? '缺失' : '可用'}
                    </span>
                    <span className="ml-2 text-ink-3">{c.label}</span>
                    {c.missing && c.required && (
                      <span className="ml-1 text-xs text-danger">
                        ——必需{c.reason ? `（${c.reason}）` : ''}
                        {c.fix ? `，安装：${c.fix}` : ''}
                      </span>
                    )}
                    {c.missing && !c.required && (
                      <span className="ml-1 text-xs text-ink-3">
                        ——非必需，仅影响对应的那一项能力
                      </span>
                    )}
                  </li>
                ))}
              </ul>
            </div>

            {blocked && (
              <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
                该节点不具备必需能力，无法启用。收下一份不会生效的配置比明说不支持更危险
                ——界面会显示"已启用"，而实际上一条规则都没写下去。
              </p>
            )}

            {check.rules.length > 0 && (
              <div className="rounded-control border border-line bg-sunken px-3 py-2">
                <p className="mb-1 text-sm text-ink-2">将要下发的规则（来自节点）</p>
                <pre className="kc-mono whitespace-pre-wrap text-xs text-ink-3">
                  {check.rules.join('\n')}
                </pre>
              </div>
            )}

            {check.warnings.map((w) => (
              <p
                key={w}
                className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning"
              >
                {w}
              </p>
            ))}
          </>
        )}
      </div>
    </Modal>
  )
}

function Toggle({
  checked,
  onChange,
  label,
  description,
  tone,
}: {
  checked: boolean
  onChange: (v: boolean) => void
  label: string
  description: string
  tone: 'good' | 'warn'
}) {
  return (
    <label className="flex cursor-pointer gap-2.5 rounded-control border border-line px-3 py-2.5">
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        className="mt-1"
      />
      <span className="flex flex-col gap-0.5">
        <span className={tone === 'warn' && checked ? 'text-base text-warning' : 'text-base text-ink'}>
          {label}
        </span>
        <span className="text-xs text-ink-3">{description}</span>
      </span>
    </label>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
