/**
 * PublicIPPage 管理节点上的公网地址池（F-4-06）。
 *
 * 两处与其它页面不同的处理：
 *
 * 1. **「绑定」与「迁移」是两个入口**，不是一个「改绑」。已绑定的地址再点
 *    绑定时会被拒绝并指向迁移——因为「这个地址原来指向谁」在一次静默改绑里
 *    就消失了，而如果用户点错了一行，他连错在哪里都看不到。
 * 2. **改动之前有规则预览**。f-4-06 明确要求它，而它的全部意义就是让用户
 *    在动网络之前看到将要发生什么。预览放在确认框里、改动按钮之前。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import {
  PUBLIC_IP_MODE_HINT,
  PUBLIC_IP_STATUS_LABEL,
  PUBLIC_IP_STATUS_TONE,
  RUNTIME_STATUS_LABEL,
  publicIPApi,
  publicIPExtraApi,
  type PublicIPMode,
  type PublicIPView,
  type BatchResult,
} from '@/api/publicip'
import { vmApi, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function PublicIPPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [createOpen, setCreateOpen] = useState(false)
  const [target, setTarget] = useState<PublicIPView | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')
  // 多选。用 Set 而不是数组：勾选/取消是逐项的，数组每次都要查一遍再过滤。
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [batchBindOpen, setBatchBindOpen] = useState(false)
  // 批量结果单独放：它可能**部分成功**，而那种情况不能用一句 notice 说完
  // ——用户需要看到哪几条失败了、为什么。
  const [batchResult, setBatchResult] = useState<BatchResult | null>(null)
  const [prefixOpen, setPrefixOpen] = useState(false)
  // IPv6 引导（G-38）：从前缀检测带入录入——预填出口网卡与地址前缀，
  // 用户补全后缀即可。检测只读、录入要人确认，两步分开是有意的。
  const [ipv6Prefill, setIpv6Prefill] = useState<{ ip: string; egress_if: string } | null>(null)

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const list = useQuery({
    queryKey: ['public-ips', effectiveNodeID],
    queryFn: () => publicIPApi.list(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['public-ips'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const reload = useMutation({
    mutationFn: () => publicIPExtraApi.reloadRules(effectiveNodeID),
    onSuccess: (r) => {
      setError('')
      setNotice(
        r.failed > 0
          ? `已重载 ${r.applied} 条，${r.failed} 条失败${r.detail ? `：${r.detail}` : ''}`
          : `已重载 ${r.applied} 条规则`,
      )
    },
    onError: (err) => setError(describe(err)),
  })

  const unbind = useMutation({
    mutationFn: (ip: PublicIPView) => publicIPApi.unbind(ip.id),
    onSuccess: () => {
      setError('')
      setNotice('已提交解绑')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const remove = useMutation({
    mutationFn: (ip: PublicIPView) => publicIPApi.remove(ip.id),
    onSuccess: () => {
      setError('')
      setNotice('已从地址池移除')
      refresh()
    },
    // 已绑定时会被拒绝——移除一个正在被使用的地址会让绑定记录指向一条
    // 不存在的地址，而节点上那条规则仍然生效，两边就此分叉。
    onError: (err) => setError(describe(err)),
  })

  const toggle = (id: number) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const batchUnbind = useMutation({
    mutationFn: (ids: number[]) => publicIPApi.batchUnbind(ids),
    onSuccess: (r) => {
      setError('')
      setBatchResult(r)
      setSelected(new Set())
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  const batchBind = useMutation({
    mutationFn: (v: { ids: number[]; vmID: number; mode: PublicIPMode }) =>
      publicIPApi.batchBind(v.ids, v.vmID, v.mode),
    onSuccess: (r) => {
      setError('')
      setBatchBindOpen(false)
      setBatchResult(r)
      setSelected(new Set())
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
          <h1 className="text-xl font-semibold text-ink">公网 IP</h1>
          <p className="mt-1 text-base text-ink-3">
            一个地址在同一时刻只能指向一台虚拟机——这是网络层的事实。
            要把地址从一台机器挪到另一台（故障转移），用「迁移」。
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
          {/* 前缀检测：一个 /64 有 2^64 个地址，手填既容易错、也回答不了
              「还剩多少能分」。 */}
          <Button
            size="sm"
            variant="secondary"
            disabled={effectiveNodeID === 0}
            onClick={() => setPrefixOpen(true)}
          >
            检测 IPv6 前缀
          </Button>
          {/* 重载：记录是对的、节点上漂了时用的。它按记录重新下发，因此不需要参数。 */}
          <Button
            size="sm"
            variant="secondary"
            loading={reload.isPending}
            disabled={effectiveNodeID === 0}
            onClick={() => reload.mutate()}
          >
            重载规则
          </Button>
          <Button size="sm" disabled={effectiveNodeID === 0} onClick={() => setCreateOpen(true)}>
            录入地址
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
          title="地址池为空"
          description="录入单个地址，或填一个网段批量展开（会自动跳过网络地址、广播地址与网关）。"
        />
      ) : (
        <div className="flex flex-col gap-3">
          {/* 选中之后才出现的工具条。常驻会让表格上方多一条平时无用的行，
              而这一页的主要操作仍是逐条处理。 */}
          {selected.size > 0 && (
            <div className="flex flex-wrap items-center gap-3 rounded-control border border-line bg-surface px-4 py-2">
              <span className="text-sm text-ink-2">已选 {selected.size} 个地址</span>
              <Button size="sm" variant="secondary" onClick={() => setBatchBindOpen(true)}>
                批量绑定
              </Button>
              <Button
                size="sm"
                variant="secondary"
                loading={batchUnbind.isPending}
                onClick={() => batchUnbind.mutate(Array.from(selected))}
              >
                批量解绑
              </Button>
              <button
                className="text-sm text-ink-3 hover:underline"
                onClick={() => setSelected(new Set())}
              >
                取消选择
              </button>
            </div>
          )}

          <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="w-10 px-4 py-2.5">
                  <input
                    type="checkbox"
                    checked={selected.size === items.length && items.length > 0}
                    onChange={(e) =>
                      setSelected(
                        e.target.checked ? new Set(items.map((i) => i.id)) : new Set(),
                      )
                    }
                  />
                </th>
                <th className="px-4 py-2.5 font-medium">地址</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">指向</th>
                <th className="px-4 py-2.5 font-medium">模式</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((ip) => (
                <tr key={ip.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="px-4 py-2.5">
                    <input
                      type="checkbox"
                      checked={selected.has(ip.id)}
                      onChange={() => toggle(ip.id)}
                    />
                  </td>
                  <td className="px-4 py-2.5">
                    <span className="kc-mono text-ink">{ip.ip}</span>
                    {ip.cidr && <span className="ml-2 text-xs text-ink-3">{ip.cidr}</span>}
                    {ip.gateway && (
                      <span className="block text-xs text-ink-3">网关 {ip.gateway}</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={PUBLIC_IP_STATUS_TONE[ip.status]}>
                      {PUBLIC_IP_STATUS_LABEL[ip.status]}
                    </StatusBadge>
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {ip.vm_name ? (
                      <>
                        {ip.vm_name}
                        {/* 生效状态与绑定记录是两回事：记录是控制面的意图，
                            这里是宿主机上的结果。不一致时流量还没通。 */}
                        {ip.runtime_status && ip.runtime_status !== 'active' && (
                          <span className="ml-2 text-xs text-warning">
                            {RUNTIME_STATUS_LABEL[ip.runtime_status]}
                          </span>
                        )}
                      </>
                    ) : (
                      <span className="text-ink-3">未绑定</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {ip.mode ? PUBLIC_IP_MODE_HINT[ip.mode]?.label ?? ip.mode : '—'}
                  </td>
                  <td className="px-4 py-2.5">
                    <span className="flex flex-wrap gap-2">
                      <button
                        className="text-sm text-brand hover:underline"
                        onClick={() => setTarget(ip)}
                      >
                        {ip.vm_id ? '迁移' : '绑定'}
                      </button>
                      {ip.vm_id && (
                        <button
                          className="text-sm text-ink-2 hover:underline"
                          disabled={unbind.isPending}
                          onClick={() => unbind.mutate(ip)}
                        >
                          解绑
                        </button>
                      )}
                      <button
                        className="text-sm text-danger hover:underline"
                        disabled={!!ip.vm_id}
                        title={ip.vm_id ? '已绑定的地址需先解绑才能移除' : ''}
                        onClick={() => remove.mutate(ip)}
                      >
                        移除
                      </button>
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          </div>
        </div>
      )}

      <IPv6PrefixModal
        open={prefixOpen}
        nodeID={effectiveNodeID}
        onClose={() => setPrefixOpen(false)}
        onUse={(prefix, egressIf) => {
          setIpv6Prefill({ ip: prefix, egress_if: egressIf })
          setPrefixOpen(false)
          setCreateOpen(true)
        }}
      />

      <CreateIPModal
        key={`create-ip-${createOpen}-${ipv6Prefill?.ip ?? ''}`}
        open={createOpen}
        nodeID={effectiveNodeID}
        initial={createOpen ? ipv6Prefill : null}
        onClose={() => {
          setCreateOpen(false)
          setIpv6Prefill(null)
        }}
        onDone={(count) => {
          setCreateOpen(false)
          setIpv6Prefill(null)
          setError('')
          setNotice(`已录入 ${count} 个地址`)
          refresh()
        }}
        onError={(msg) => {
          setCreateOpen(false)
          setIpv6Prefill(null)
          setError(msg)
        }}
      />

      <BatchBindModal
        open={batchBindOpen}
        nodeID={effectiveNodeID}
        count={selected.size}
        busy={batchBind.isPending}
        onClose={() => setBatchBindOpen(false)}
        onSubmit={(vmID, mode) =>
          batchBind.mutate({ ids: Array.from(selected), vmID, mode })
        }
      />

      <BatchResultModal
        result={batchResult}
        onClose={() => setBatchResult(null)}
      />

      <BindModal
        key={target?.id ?? 'none'}
        target={target}
        nodeID={effectiveNodeID}
        onClose={() => setTarget(null)}
        onDone={(msg) => {
          setTarget(null)
          setError('')
          setNotice(msg)
          refresh()
        }}
        onError={(msg) => {
          setTarget(null)
          setError(msg)
        }}
      />
    </div>
  )
}

/** CreateIPModal 录入地址，支持单个或 CIDR 批量，IPv4 与 IPv6 皆可（G-38）。 */
function CreateIPModal({
  open,
  nodeID,
  initial,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  /** 从 IPv6 前缀检测带入的预填值；打开时应用一次。 */
  initial?: { ip: string; egress_if: string } | null
  onClose: () => void
  onDone: (count: number) => void
  onError: (message: string) => void
}) {
  const [ip, setIP] = useState(initial?.ip ?? '')
  const [gateway, setGateway] = useState('')
  const [egressIf, setEgressIf] = useState(initial?.egress_if ?? '')
  const [modes, setModes] = useState<PublicIPMode[]>(['nat_1to1'])
  const [remark, setRemark] = useState('')

  const create = useMutation({
    mutationFn: () =>
      publicIPApi.create({
        node_id: nodeID,
        ip: ip.trim(),
        gateway: gateway.trim() || undefined,
        egress_if: egressIf.trim() || undefined,
        supported_modes: modes,
        remark: remark.trim() || undefined,
      }),
    onSuccess: (res) => onDone(res.items.length),
    onError: (err) => onError(describe(err)),
  })

  const toggleMode = (m: PublicIPMode) =>
    setModes((prev) => (prev.includes(m) ? prev.filter((x) => x !== m) : [...prev, m]))

  return (
    <Modal
      open={open}
      title="录入公网地址"
      description="填单个地址，或填一个网段批量展开。网络地址、广播地址与网关会被自动跳过——它们不能分配给虚拟机。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={create.isPending} onClick={() => create.mutate()}>
            录入
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Input
          label="地址或网段（IPv4 / IPv6）"
          value={ip}
          placeholder="203.0.113.10、203.0.113.0/29 或 2001:db8:ab::10"
          onChange={(e) => setIP(e.target.value)}
          hint="一次最多展开 4096 个地址。填 /8 这种超大的网段会被拒绝——那多半是掩码写错了一位，而照做会在库里留下一批你并不打算录入的地址。"
        />
        <Input
          label="网关（可选）"
          value={gateway}
          placeholder="203.0.113.1"
          onChange={(e) => setGateway(e.target.value)}
          hint="填了之后，批量展开时会把这个地址跳过——它属于宿主机。"
        />
        <Input
          label="出口网卡（可选）"
          value={egressIf}
          placeholder="例如：eth0"
          onChange={(e) => setEgressIf(e.target.value)}
        />

        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">支持的绑定模式</span>
          {/* 让用户声明这个地址能用哪些模式：不是每个地址都能用每种——
              上游可能只给了一条静态路由。不声明的话，用户会在绑定时收到
              一句来自内核的报错，而不是「这个地址不支持这种模式」。 */}
          {(Object.keys(PUBLIC_IP_MODE_HINT) as PublicIPMode[]).map((m) => (
            <label key={m} className="flex cursor-pointer items-start gap-2.5">
              <input
                type="checkbox"
                className="mt-1"
                checked={modes.includes(m)}
                onChange={() => toggleMode(m)}
              />
              <span>
                <span className="block text-base text-ink">{PUBLIC_IP_MODE_HINT[m].label}</span>
                <span className="block text-sm text-ink-3">{PUBLIC_IP_MODE_HINT[m].detail}</span>
              </span>
            </label>
          ))}
        </div>

        <Input
          label="备注（可选）"
          value={remark}
          onChange={(e) => setRemark(e.target.value)}
        />
      </div>
    </Modal>
  )
}

/**
 * BindModal 处理绑定与迁移。
 *
 * 两者的区别只在于「有没有当前持有者」，因此共用一个弹框——但**标题与
 * 说明必须不同**：迁移会中断当前机器的对外访问，而绑定不会。
 */
function BindModal({
  target,
  nodeID,
  onClose,
  onDone,
  onError,
}: {
  target: PublicIPView | null
  nodeID: number
  onClose: () => void
  onDone: (message: string) => void
  onError: (message: string) => void
}) {
  const migrating = target?.vm_id != null
  // 迁移时沿用当前模式：用户点「迁移」时想的是「把地址挪过去」，
  // 而不是「顺便换个工作方式」。
  const [mode, setMode] = useState<PublicIPMode>(
    (target?.mode as PublicIPMode) ?? 'nat_1to1',
  )
  const [vmID, setVmID] = useState(0)
  const [preview, setPreview] = useState<Awaited<
    ReturnType<typeof publicIPApi.preview>
  > | null>(null)
  const [previewError, setPreviewError] = useState('')

  const vms = useQuery({
    queryKey: ['vms', { node_id: nodeID, page_size: 200 }],
    queryFn: () => vmApi.list({ node_id: nodeID, page_size: 200 }),
    enabled: target != null,
  })

  // 只列同一节点上、且不是当前持有者的虚拟机：地址只能绑定同节点的
  // 虚拟机（跨节点没有网络路径），而选中当前持有者是无意义的操作。
  const candidates: VmView[] = (vms.data?.items ?? []).filter(
    (v) => v.id !== target?.vm_id,
  )

  const doPreview = useMutation({
    mutationFn: () => publicIPApi.preview(target!.id, vmID, mode),
    onSuccess: (p) => {
      setPreview(p)
      setPreviewError('')
    },
    onError: (err) => {
      setPreview(null)
      setPreviewError(describe(err))
    },
  })

  const submit = useMutation({
    mutationFn: () =>
      migrating
        ? publicIPApi.migrate(target!.id, vmID, mode)
        : publicIPApi.bind(target!.id, vmID, mode),
    onSuccess: () =>
      onDone(migrating ? '已提交迁移，连通会短暂中断后恢复' : '已提交绑定'),
    onError: (err) => onError(describe(err)),
  })

  if (!target) return null

  const available = target.supported_modes ?? []

  return (
    <Modal
      open
      title={
        migrating
          ? `迁移 ${target.ip} 到另一台虚拟机`
          : `绑定 ${target.ip} 到虚拟机`
      }
      description={
        migrating
          ? `该地址当前指向「${target.vm_name}」。迁移会撤掉它的规则再指向新机器——对外访问会短暂中断。`
          : '地址会指向选定的虚拟机。不同模式对来宾的配置要求不同，请按说明确认。'
      }
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={vmID === 0}
            loading={submit.isPending}
            onClick={() => submit.mutate()}
          >
            {migrating ? '确认迁移' : '确认绑定'}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">目标虚拟机</label>
          <select
            value={vmID}
            onChange={(e) => {
              setVmID(Number(e.target.value))
              // 换了目标或模式，之前的预览就失效了——留着会让用户拿一份
              // 与当前选择无关的规则去做判断。
              setPreview(null)
            }}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value={0}>请选择…</option>
            {candidates.map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
              </option>
            ))}
          </select>
          {candidates.length === 0 && (
            <p className="text-xs text-ink-3">
              该节点上还没有可绑定的虚拟机。地址只能绑定同一宿主机上的虚拟机——
              跨节点之间没有网络路径。
            </p>
          )}
        </div>

        <div className="flex flex-col gap-1.5">
          <span className="text-base text-ink">绑定模式</span>
          {(Object.keys(PUBLIC_IP_MODE_HINT) as PublicIPMode[]).map((m) => {
            const supported = available.includes(m)
            return (
              <label
                key={m}
                className={`flex items-start gap-2.5 ${supported ? 'cursor-pointer' : 'opacity-50'}`}
              >
                <input
                  type="radio"
                  className="mt-1"
                  name="pip-mode"
                  disabled={!supported}
                  checked={mode === m}
                  onChange={() => {
                    setMode(m)
                    setPreview(null)
                  }}
                />
                <span>
                  <span className="block text-base text-ink">
                    {PUBLIC_IP_MODE_HINT[m].label}
                    {!supported && <span className="ml-2 text-xs text-ink-3">该地址不支持</span>}
                  </span>
                  <span className="block text-sm text-ink-3">{PUBLIC_IP_MODE_HINT[m].detail}</span>
                </span>
              </label>
            )
          })}
        </div>

        {/* 规则预览。放在确认按钮**之前**——它的全部意义就是让用户在动
            网络之前看到将要发生什么。 */}
        <div className="rounded-control border border-line px-3 py-2.5">
          <div className="flex items-center justify-between gap-3">
            <span className="text-base text-ink">规则预览</span>
            <Button
              variant="secondary"
              size="sm"
              disabled={vmID === 0}
              loading={doPreview.isPending}
              onClick={() => doPreview.mutate()}
            >
              生成预览
            </Button>
          </div>

          {previewError && <p className="mt-1.5 text-sm text-warning">{previewError}</p>}

          {preview && (
            <div className="mt-2 flex flex-col gap-2">
              {(preview.warnings ?? []).length > 0 && (
                <div className="rounded-control bg-warning/10 px-2.5 py-1.5">
                  {preview.warnings!.map((w) => (
                    <p key={w} className="text-sm text-warning">
                      {w}
                    </p>
                  ))}
                </div>
              )}
              <DiffList title="将新增" items={preview.added} tone="text-success" />
              <DiffList title="将移除" items={preview.removed} tone="text-danger" />
            </div>
          )}

          {!preview && !previewError && (
            <p className="mt-1.5 text-xs text-ink-3">
              预览由节点根据宿主机上已有的规则算出，只读、不产生任何改动。
            </p>
          )}
        </div>
      </div>
    </Modal>
  )
}

function DiffList({
  title,
  items,
  tone,
}: {
  title: string
  items: string[]
  tone: string
}) {
  if (items.length === 0) return null
  return (
    <div>
      <p className={`text-xs ${tone}`}>{title}</p>
      <ul className="mt-0.5 flex flex-col gap-0.5">
        {items.map((r) => (
          <li key={r} className="kc-mono break-all text-xs text-ink-2">
            {r}
          </li>
        ))}
      </ul>
    </div>
  )
}

/**
 * IPv6PrefixModal 展示节点上检测到的 IPv6 前缀。
 *
 * **未授信的前缀也要列出来**：过滤掉它们会让人以为"检测到的都能用"，而那
 * 恰恰是需要用户自己判断的部分——能不能用取决于本地路由与上游通告，只有
 * 节点能给出这个判断。
 */
function IPv6PrefixModal({
  open,
  nodeID,
  onClose,
  onUse,
}: {
  open: boolean
  nodeID: number
  onClose: () => void
  /** 对可用前缀点「带入录入」：把前缀与出口网卡预填进录入表单（G-38）。 */
  onUse: (prefix: string, egressIf: string) => void
}) {
  const list = useQuery({
    queryKey: ['ipv6-prefixes', nodeID],
    queryFn: () => publicIPExtraApi.detectIPv6Prefixes(nodeID),
    enabled: open && nodeID > 0,
  })

  const items = list.data?.items ?? []

  return (
    <Modal
      open={open}
      title="IPv6 前缀"
      description="来自宿主机外网网卡的实际配置，而不是手动录入的值。"
      onClose={onClose}
      footer={
        <Button size="sm" onClick={onClose}>
          关闭
        </Button>
      }
    >
      {items.length === 0 ? (
        <p className="text-base text-ink-3">没有检测到 IPv6 前缀。</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {items.map((p) => (
            <li
              key={p.prefix}
              className="flex flex-wrap items-center justify-between gap-2 rounded-control border border-line px-3 py-2"
            >
              <span className="kc-mono text-ink">{p.prefix}</span>
              <span className="flex items-center gap-2">
                {p.egress_if && <span className="text-xs text-ink-3">{p.egress_if}</span>}
                <StatusBadge tone={p.trusted ? 'success' : 'warning'}>
                  {p.trusted ? '可用' : '不可用'}
                </StatusBadge>
                {p.assignable >= 0 && (
                  <span className="kc-nums text-xs text-ink-3">可分配 {p.assignable}</span>
                )}
                {/* 只对可用前缀给入口：不可用的前缀带进录入表单，只会在
                    创建时被拒绝——那一步的错误对定位没有帮助。 */}
                {p.trusted && (
                  <Button
                    size="sm"
                    variant="secondary"
                    onClick={() => onUse(p.prefix, p.egress_if ?? '')}
                  >
                    带入录入
                  </Button>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}

/**
 * BatchBindModal 把选中的多个地址绑到**同一台**虚拟机。
 *
 * 只支持「多个地址 → 一台机器」这一个方向：绑定要求地址与虚拟机在同一节点，
 * 而任意组合会让用户在面对一台失败时无法判断是「地址不对」还是「机器不对」。
 */
function BatchBindModal({
  open,
  nodeID,
  count,
  busy,
  onClose,
  onSubmit,
}: {
  open: boolean
  nodeID: number
  count: number
  busy: boolean
  onClose: () => void
  onSubmit: (vmID: number, mode: PublicIPMode) => void
}) {
  const [vmID, setVMID] = useState(0)
  const [mode, setMode] = useState<PublicIPMode>('nat_1to1')

  const vms = useQuery({
    queryKey: ['vms', nodeID],
    queryFn: () => vmApi.list({ node_id: nodeID, page_size: 200 }),
    enabled: open && nodeID > 0,
  })

  return (
    <Modal
      open={open}
      title={`把 ${count} 个地址绑到同一台虚拟机`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={vmID === 0}
            loading={busy}
            onClick={() => onSubmit(vmID, mode)}
          >
            绑定
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">目标虚拟机</label>
          <select
            value={vmID}
            onChange={(e) => setVMID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value={0}>请选择…</option>
            {(vms.data?.items ?? []).map((v) => (
              <option key={v.id} value={v.id}>
                {v.name}
              </option>
            ))}
          </select>
          <p className="text-xs text-ink-3">
            只列出这个节点上的虚拟机——地址与虚拟机必须在同一节点，否则那条 NAT
            规则指向的地方根本不存在。
          </p>
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">绑定模式</label>
          <select
            value={mode}
            onChange={(e) => setMode(e.target.value as PublicIPMode)}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            {(Object.keys(PUBLIC_IP_MODE_HINT) as PublicIPMode[]).map((k) => (
              <option key={k} value={k}>
                {PUBLIC_IP_MODE_HINT[k].label}
              </option>
            ))}
          </select>
          {/* 把模式的**代价**写出来：它决定了用户在来宾里还要不要做别的事，
              而这一步做漏了的表现是「绑上了但不通」。 */}
          <p className="text-xs text-ink-3">{PUBLIC_IP_MODE_HINT[mode].detail}</p>
        </div>

        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-xs text-warning">
          部分地址可能绑不上（已被占用、模式不支持等）。那些会逐条列出来，
          而已经绑成功的那几条不会被撤销——撤销意味着再做一次网络变更。
        </p>
      </div>
    </Modal>
  )
}

/**
 * BatchResultModal 展示批量的逐条结果。
 *
 * **部分成功必须逐条显示**：一个笼统的「批量操作失败」会让用户不知道该处理
 * 哪几个，而那正是他要处理的东西。总结里那句「已成功的那几条不会被撤销」
 * 直接来自服务端——用户看到部分失败时第一个问题就是「那前面那几条还算数吗」。
 */
function BatchResultModal({
  result,
  onClose,
}: {
  result: BatchResult | null
  onClose: () => void
}) {
  return (
    <Modal
      open={result !== null}
      title="批量操作结果"
      onClose={onClose}
      footer={
        <Button size="sm" onClick={onClose}>
          知道了
        </Button>
      }
    >
      {result && (
        <div className="flex flex-col gap-3">
          <p className="text-base text-ink-2">{result.message}</p>

          {(result.failed?.length ?? 0) > 0 && (
            <div className="rounded-control border border-danger/40 bg-danger/5 px-3 py-2">
              <p className="text-sm text-danger">失败的条目：</p>
              <ul className="mt-1 flex flex-col gap-0.5">
                {result.failed?.map((f) => (
                  <li key={f.id} className="text-xs text-danger">
                    #{f.id}
                    {f.address ? `（${f.address}）` : ''}：{f.reason}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </Modal>
  )
}
