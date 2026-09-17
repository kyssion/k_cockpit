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
  type PublicIPMode,
  type PublicIPView,
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

  if (nodes.isPending) return <PageLoading />
  const items = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">公网 IP</h1>
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
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">地址</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">指向</th>
                <th className="px-4 py-2.5 font-medium">模式</th>
                <th className="px-4 py-2.5 font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((ip) => (
                <tr key={ip.id} className="border-t border-line">
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
      )}

      <CreateIPModal
        open={createOpen}
        nodeID={effectiveNodeID}
        onClose={() => setCreateOpen(false)}
        onDone={(count) => {
          setCreateOpen(false)
          setError('')
          setNotice(`已录入 ${count} 个地址`)
          refresh()
        }}
        onError={(msg) => {
          setCreateOpen(false)
          setError(msg)
        }}
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

/** CreateIPModal 录入地址，支持单个或 CIDR 批量。 */
function CreateIPModal({
  open,
  nodeID,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  onClose: () => void
  onDone: (count: number) => void
  onError: (message: string) => void
}) {
  const [ip, setIP] = useState('')
  const [gateway, setGateway] = useState('')
  const [egressIf, setEgressIf] = useState('')
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
          label="地址或网段"
          value={ip}
          placeholder="203.0.113.10 或 203.0.113.0/29"
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

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
