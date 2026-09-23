/**
 * SecurityGroupPage 管理安全组与其叠加生效（F-4-03 / F-4-04）。
 *
 * 页面分成两半，对应规格里的两件事：
 *
 *   左：**组与规则**——编辑单个组的内容。
 *   右：**生效规则**——选中一台虚拟机，看它实际放行了什么。
 *
 * 两者必须能对照着看，因为「我改了组，机器上到底变成什么样」是这里最容易
 * 出错的地方：一台机器可能挂着多个组，改其中一个是看不出全貌的。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import {
  DIRECTION_LABEL,
  PROTOCOL_LABEL,
  TARGET_LABEL,
  protocolUsesPorts,
  ruleText,
  securityGroupApi,
  type Direction,
  type EffectivePreview,
  type GroupView,
  type Protocol,
  type RuleInput,
  type RuleView,
  type TargetType,
} from '@/api/securitygroup'
import { vmApi, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'

export function SecurityGroupPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [selectedGroup, setSelectedGroup] = useState<GroupView | null>(null)
  const [createOpen, setCreateOpen] = useState(false)
  const [vmID, setVMID] = useState(0)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const groups = useQuery({
    queryKey: ['security-groups', effectiveNodeID],
    queryFn: () => securityGroupApi.list(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const vms = useQuery({
    queryKey: ['vms', { node_id: effectiveNodeID, page_size: 200 }],
    queryFn: () => vmApi.list({ node_id: effectiveNodeID, page_size: 200 }),
    enabled: effectiveNodeID > 0,
  })

  const showError = (err: unknown) => {
    setNotice('')
    setError(describe(err))
  }

  if (nodes.isPending) return <PageLoading />

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">安全组</h1>
          {/* 这句话必须显眼：它是这块最容易误解的地方。多组叠加生效，
              用户自然会想「那拒绝规则会覆盖允许吗」——答案是没有拒绝规则。 */}
          <p className="mt-1 text-base text-ink-3">
            组内只有<span className="text-ink-2">允许</span>规则，没有拒绝。
            一台机器挂多个组时，生效的是各组的<span className="text-ink-2">并集</span>
            ——不在任何允许规则里的流量一律不通。
          </p>
        </div>
        <div className="flex items-end gap-2">
          <div className="flex flex-col gap-1">
            <label className="text-xs text-ink-3">节点</label>
            <select
              value={effectiveNodeID}
              onChange={(e) => {
                setNodeID(Number(e.target.value))
                setSelectedGroup(null)
                setVMID(0)
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
          <Button size="sm" disabled={effectiveNodeID === 0} onClick={() => setCreateOpen(true)}>
            新建安全组
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

      <div className="grid gap-5 lg:grid-cols-2">
        {/* 左：组列表 + 选中组的规则 */}
        <section className="flex flex-col gap-3">
          <h2 className="text-sm text-ink-3">安全组与规则</h2>
          {groups.isPending ? (
            <PageLoading />
          ) : (groups.data?.items ?? []).length === 0 ? (
            <EmptyState title="还没有安全组" description="新建一个组，再往里加允许规则。" />
          ) : (
            <div className="flex flex-col gap-2">
              {(groups.data?.items ?? []).map((g) => (
                <button
                  key={g.id}
                  onClick={() => setSelectedGroup(g)}
                  className={`rounded-card border px-4 py-3 text-left transition-colors ${
                    selectedGroup?.id === g.id
                      ? 'border-brand bg-brand/5'
                      : 'border-line bg-surface hover:border-line-strong'
                  }`}
                >
                  <div className="flex items-baseline justify-between gap-3">
                    <span className="flex items-baseline gap-2">
                      <span className="text-base font-medium text-ink">{g.name}</span>
                      {g.is_default && (
                        <span className="rounded-pill bg-info/10 px-1.5 py-0.5 text-xs text-info">
                          默认
                        </span>
                      )}
                    </span>
                    <span className="shrink-0 text-xs text-ink-3">
                      {g.rule_count} 条规则 · 挂载 {g.attached_count} 个网口
                    </span>
                  </div>
                  {g.remark && <p className="mt-0.5 text-sm text-ink-3">{g.remark}</p>}
                </button>
              ))}
            </div>
          )}

          {selectedGroup && (
            <RuleEditor
              group={selectedGroup}
              onError={showError}
              onChanged={(msg) => {
                setNotice(msg)
                setError('')
                void queryClient.invalidateQueries({ queryKey: ['security-groups'] })
                void queryClient.invalidateQueries({ queryKey: ['sg-rules'] })
              }}
            />
          )}
        </section>

        {/* 右：生效规则 */}
        <section className="flex flex-col gap-3">
          <h2 className="text-sm text-ink-3">生效规则</h2>
          <EffectivePanel
            vms={vms.data?.items ?? []}
            vmID={vmID}
            onSelectVM={setVMID}
            onError={showError}
            onApplied={(msg) => {
              setNotice(msg)
              setError('')
            }}
          />
        </section>
      </div>

      <CreateGroupModal
        open={createOpen}
        nodeID={effectiveNodeID}
        onClose={() => setCreateOpen(false)}
        onDone={(name) => {
          setCreateOpen(false)
          setError('')
          setNotice(`已创建安全组「${name}」`)
          void queryClient.invalidateQueries({ queryKey: ['security-groups'] })
        }}
        onError={(msg) => {
          setCreateOpen(false)
          showError(msg)
        }}
      />
    </div>
  )
}

/** EffectivePanel 展示一台虚拟机的汇总生效规则，并可下发。 */
function EffectivePanel({
  vms,
  vmID,
  onSelectVM,
  onApplied,
  onError,
}: {
  vms: VmView[]
  vmID: number
  onSelectVM: (id: number) => void
  onApplied: (message: string) => void
  onError: (error: unknown) => void
}) {
  const [preview, setPreview] = useState<EffectivePreview | null>(null)

  const load = useMutation({
    mutationFn: () => securityGroupApi.effective(vmID),
    onSuccess: setPreview,
    onError: (err) => {
      setPreview(null)
      onError(err)
    },
  })

  const apply = useMutation({
    // 带上**当前预览**的版本号：中间被别人改过的话请求会被拒绝，
    // 而不会下发一套用户没看过的规则。
    mutationFn: () => securityGroupApi.apply(vmID, preview!.version),
    onSuccess: () => {
      setPreview(null)
      onApplied('已提交下发；可在任务列表查看进度')
    },
    onError: (err) => {
      setPreview(null)
      onError(err)
    },
  })

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-end gap-2">
        <div className="flex flex-1 flex-col gap-1">
          <label className="text-xs text-ink-3">虚拟机</label>
          <select
            value={vmID}
            onChange={(e) => {
              onSelectVM(Number(e.target.value))
              // 换了虚拟机，之前的预览就失效了——留着会让用户拿一台机器的
              // 规则去判断另一台。
              setPreview(null)
            }}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value={0}>请选择…</option>
            {vms.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
              </option>
            ))}
          </select>
        </div>
        <Button
          variant="secondary"
          size="sm"
          disabled={vmID === 0}
          loading={load.isPending}
          onClick={() => load.mutate()}
        >
          汇总生效规则
        </Button>
      </div>

      {!preview && (
        <p className="text-sm text-ink-3">
          汇总会把该虚拟机挂载的多个组的规则合并去重，并标出每一条来自哪些组
          ——否则看到合并结果之后，你不知道该去哪个组里改。
        </p>
      )}

      {preview && (
        <div className="rounded-card border border-line bg-surface p-4">
          <div className="flex items-baseline justify-between gap-3">
            <span className="text-base text-ink">
              {preview.vm_name}
              <span className="ml-2 text-xs text-ink-3">
                {preview.interface_count} 个网口 · {preview.groups.length} 个组
              </span>
            </span>
            <span className="kc-mono text-xs text-ink-3">{preview.version}</span>
          </div>

          {preview.groups.length > 0 && (
            <p className="mt-1 text-sm text-ink-2">参与叠加：{preview.groups.join('、')}</p>
          )}

          {(preview.warnings ?? []).length > 0 && (
            <div className="mt-2 rounded-control bg-warning/10 px-2.5 py-1.5">
              {preview.warnings!.map((w) => (
                <p key={w} className="text-sm text-warning">
                  {w}
                </p>
              ))}
            </div>
          )}

          {preview.rules.length === 0 ? (
            <p className="mt-2 text-sm text-ink-3">没有生效规则。</p>
          ) : (
            <table className="mt-2 w-full border-collapse text-sm">
              <thead>
                <tr className="text-left text-xs text-ink-3">
                  <th className="py-1 font-normal">方向</th>
                  <th className="py-1 font-normal">规则</th>
                  <th className="py-1 font-normal">来源</th>
                </tr>
              </thead>
              <tbody>
                {preview.rules.map((r, i) => (
                  <tr key={i} className="border-t border-line transition-colors hover:bg-sunken/70">
                    <td className="py-1.5 text-ink-2">{DIRECTION_LABEL[r.direction]}</td>
                    <td className="kc-mono py-1.5 text-ink">{ruleText(r)}</td>
                    {/* 来源是这张表存在的理由：没有它，用户只能去每个组里翻。 */}
                    <td className="py-1.5 text-xs text-ink-3">{r.sources.join('、')}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}

          <div className="mt-3 flex items-center justify-between gap-3">
            <span className="text-xs text-ink-3">
              下发会整台机器重写一次规则链，期间不改动虚拟机运行状态。
            </span>
            <Button size="sm" loading={apply.isPending} onClick={() => apply.mutate()}>
              应用到节点
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}

/** RuleEditor 编辑选中组的规则。 */
function RuleEditor({
  group,
  onChanged,
  onError,
}: {
  group: GroupView
  onChanged: (message: string) => void
  onError: (error: unknown) => void
}) {
  const [addOpen, setAddOpen] = useState(false)

  const rules = useQuery({
    queryKey: ['sg-rules', group.id],
    queryFn: () => securityGroupApi.listRules(group.id),
  })

  const removeRule = useMutation({
    mutationFn: (ruleID: number) => securityGroupApi.deleteRule(group.id, ruleID),
    onSuccess: () => onChanged('规则已删除'),
    onError,
  })

  const removeGroup = useMutation({
    mutationFn: () => securityGroupApi.remove(group.id),
    onSuccess: () => onChanged(`已删除安全组「${group.name}」`),
    onError,
  })

  return (
    <div className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between gap-3">
        <h3 className="text-base font-medium text-ink">{group.name} 的规则</h3>
        <div className="flex gap-2">
          <Button variant="secondary" size="sm" onClick={() => setAddOpen(true)}>
            加规则
          </Button>
          <Button
            variant="danger"
            size="sm"
            // 被挂载时直接禁用并说明原因——点下去才被后端拒绝会让用户
            // 以为是系统出了问题。
            disabled={group.attached_count > 0 || group.is_default}
            title={
              group.is_default
                ? '默认安全组不可删除'
                : group.attached_count > 0
                  ? `仍被 ${group.attached_count} 个网口使用，需先解除挂载`
                  : ''
            }
            loading={removeGroup.isPending}
            onClick={() => removeGroup.mutate()}
          >
            删除组
          </Button>
        </div>
      </div>

      {rules.isPending ? (
        <p className="mt-2 text-sm text-ink-3">加载中…</p>
      ) : (rules.data?.items ?? []).length === 0 ? (
        <p className="mt-2 text-sm text-ink-3">
          这个组里还没有规则——挂上它的机器不会收到任何放行。
        </p>
      ) : (
        <table className="mt-2 w-full border-collapse text-sm">
          <tbody>
            {(rules.data?.items ?? []).map((r) => (
              <RuleRow
                key={r.id}
                rule={r}
                onDelete={() => removeRule.mutate(r.id)}
                busy={removeRule.isPending}
              />
            ))}
          </tbody>
        </table>
      )}

      <AddRuleModal
        open={addOpen}
        group={group}
        onClose={() => setAddOpen(false)}
        onDone={() => {
          setAddOpen(false)
          onChanged('规则已添加')
        }}
        onError={(err) => {
          setAddOpen(false)
          onError(err)
        }}
      />
    </div>
  )
}

function RuleRow({
  rule,
  onDelete,
  busy,
}: {
  rule: RuleView
  onDelete: () => void
  busy: boolean
}) {
  return (
    <tr className="border-t border-line">
      <td className="py-1.5 pr-3 text-ink-2">{DIRECTION_LABEL[rule.direction]}</td>
      <td className="kc-mono py-1.5 text-ink">{ruleText(rule)}</td>
      <td className="py-1.5 text-right">
        <button
          className="text-sm text-danger hover:underline"
          disabled={busy}
          onClick={onDelete}
        >
          删除
        </button>
      </td>
    </tr>
  )
}

function AddRuleModal({
  open,
  group,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  group: GroupView
  onClose: () => void
  onDone: () => void
  onError: (error: unknown) => void
}) {
  const [direction, setDirection] = useState<Direction>('ingress')
  const [protocol, setProtocol] = useState<Protocol>('tcp')
  const [portStart, setPortStart] = useState('')
  const [portEnd, setPortEnd] = useState('')
  const [targetType, setTargetType] = useState<TargetType>('cidr')
  const [targetValue, setTargetValue] = useState('0.0.0.0/0')
  const [remark, setRemark] = useState('')

  const usesPorts = protocolUsesPorts(protocol)

  const submit = useMutation({
    mutationFn: () => {
      const input: RuleInput = {
        direction,
        protocol,
        target_type: targetType,
        target_value: targetValue.trim(),
        remark: remark.trim() || undefined,
      }
      if (usesPorts && portStart.trim() !== '') {
        input.port_start = Number(portStart)
        input.port_end = portEnd.trim() === '' ? Number(portStart) : Number(portEnd)
      }
      return securityGroupApi.createRule(group.id, input)
    },
    onSuccess: onDone,
    onError,
  })

  return (
    <Modal
      open={open}
      title={`在「${group.name}」中新增允许规则`}
      description="安全组里只能添加允许规则。要「只放行特定来源」，做法是只写那几条允许，而不是写一条「拒绝其他」——叠加生效下后者没有确定的含义。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={submit.isPending} onClick={() => submit.mutate()}>
            添加
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex gap-4">
          <div className="flex flex-1 flex-col gap-1">
            <label className="text-sm text-ink-2">方向</label>
            <select
              value={direction}
              onChange={(e) => setDirection(e.target.value as Direction)}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value="ingress">入站</option>
              <option value="egress">出站</option>
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
              // ICMP 与「全部」下端口无意义，直接禁用输入框而不是等后端拒绝：
              // 那样用户会以为是自己填错了格式。填了端口会得到一条看起来有
              // 限制、实际没有的规则，而这种偏差不会以任何形式报错。
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
              placeholder={usesPorts ? '留空表示等于起始端口' : '该协议无端口'}
              onChange={(e) => setPortEnd(e.target.value)}
            />
          </div>
        </div>

        <div className="flex gap-4">
          <div className="flex flex-1 flex-col gap-1">
            <label className="text-sm text-ink-2">目标类型</label>
            <select
              value={targetType}
              onChange={(e) => {
                const next = e.target.value as TargetType
                setTargetType(next)
                setTargetValue(next === 'cidr' ? '0.0.0.0/0' : '')
              }}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {(Object.keys(TARGET_LABEL) as TargetType[]).map((t) => (
                <option key={t} value={t}>
                  {TARGET_LABEL[t]}
                </option>
              ))}
            </select>
          </div>
          <div className="flex flex-1">
            <Input
              label="目标"
              value={targetValue}
              onChange={(e) => setTargetValue(e.target.value)}
              // 留空表示「任意来源」在界面上很好理解，但它与「忘了填」长得
              // 一模一样。默认填上 0.0.0.0/0 能让两者分开。
              hint={targetType === 'cidr' ? '任意来源请显式写 0.0.0.0/0' : '填写目标 ID'}
            />
          </div>
        </div>

        <Input label="备注（可选）" value={remark} onChange={(e) => setRemark(e.target.value)} />
      </div>
    </Modal>
  )
}

function CreateGroupModal({
  open,
  nodeID,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  onClose: () => void
  onDone: (name: string) => void
  onError: (error: unknown) => void
}) {
  const [name, setName] = useState('')
  const [remark, setRemark] = useState('')

  const create = useMutation({
    mutationFn: () =>
      securityGroupApi.create({ node_id: nodeID, name: name.trim(), remark: remark.trim() || undefined }),
    onSuccess: (g) => onDone(g.name),
    onError,
  })

  return (
    <Modal
      open={open}
      title="新建安全组"
      description="组名在同一节点内唯一（跨节点可以重名），因为安全组是节点级资源。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={name.trim() === ''}
            loading={create.isPending}
            onClick={() => create.mutate()}
          >
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Input label="名称" value={name} onChange={(e) => setName(e.target.value)} />
        <Input label="备注（可选）" value={remark} onChange={(e) => setRemark(e.target.value)} />
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
