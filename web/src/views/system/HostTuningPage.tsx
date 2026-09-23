/**
 * HostTuningPage 管理宿主机性能调优（KSM / ZRAM / 嵌套虚拟化 / CPU 亲和）。
 *
 * 页面的重心不是开关，而是**让人能判断该不该开**：
 *
 *   - 每一项都写出**收益**、**代价**、**怎么判断有没有用**（由后端下发，
 *     不在前端硬编码——否则那句"代价很小"可能描述着一个已经很贵的机制）
 *   - KSM 的**收益数字与扫描轮次**一起显示：「开着但省了 0」是一个真实存在
 *     的坏状态，而它只有在两个数字都给出时才看得出来
 *   - ZRAM 的**压缩率**才是它的收益，而算法是快与省之间的取舍
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { formatBytes, hostTuningApi, type TuningItem } from '@/api/hosttuning'
import { nodeApi } from '@/api/node'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function HostTuningPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [presetOpen, setPresetOpen] = useState(false)
  const [error, setError] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const data = useQuery({
    queryKey: ['host-tuning', effectiveNodeID],
    queryFn: () => hostTuningApi.get(effectiveNodeID),
    enabled: effectiveNodeID > 0,
    // KSM 的收益是慢慢累积的，轮询让用户能看到它有没有在起作用。
    refetchInterval: 30000,
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['host-tuning'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const apply = useMutation({
    mutationFn: (req: { item: string; enabled?: boolean }) => hostTuningApi.apply(effectiveNodeID, req),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  const removePreset = useMutation({
    mutationFn: (id: number) => hostTuningApi.deletePreset(effectiveNodeID, id),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (e) => setError(describe(e)),
  })

  if (nodes.isPending) return <PageLoading />
  const v = data.data

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold text-ink">宿主机调优</h1>
          <p className="mt-1 text-base text-ink-3">
            内存与虚拟化的性能取舍。每一项都标出
            <span className="text-ink-2">收益、代价与判断依据</span>
            ——开关本身说明不了该不该开。
          </p>
        </div>
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
      </header>

      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {data.isPending || !v ? (
        <PageLoading />
      ) : (
        <>
          <TuningCard item={cardOf(v.items, 'ksm')} enabled={v.ksm.Enabled} busy={apply.isPending} onToggle={() => apply.mutate({ item: 'ksm', enabled: !v.ksm.Enabled })}>
            {v.ksm.Enabled ? (
              <>
                <Metric label="已省内存" value={formatBytes(v.ksm.SavedBytes)} />
                <Metric label="共享页 / 引用" value={`${v.ksm.PagesShared} / ${v.ksm.PagesSharing}`} />
                <Metric label="扫描轮次" value={String(v.ksm.FullScans)} />
                <Metric label="模式" value={v.ksm.RunMode || '—'} />
                {/* **"开着但省了 0" 必须被识别出来**：它是真实存在的坏状态
                    ——KSM 在白白消耗 CPU，而开关显示"已启用"。 */}
                {v.ksm.FullScans > 10 && v.ksm.SavedBytes < 1024*1024 && (
                  <p className="col-span-2 rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
                    已扫描 {v.ksm.FullScans} 轮但几乎没有省下内存。这台机器上可能没有
                    可合并的页——此时它只是在持续消耗 CPU，建议关掉。
                  </p>
                )}
              </>
            ) : null}
          </TuningCard>

          <ZRAMCard
            item={cardOf(v.items, 'zram')}
            state={v.zram}
            nodeID={effectiveNodeID}
            onDone={refresh}
            onError={setError}
          />

          <TuningCard
            item={cardOf(v.items, 'nested')}
            enabled={v.nested.Enabled}
            busy={apply.isPending}
            onToggle={() => apply.mutate({ item: 'nested', enabled: !v.nested.Enabled })}
          >
            {!v.nested.Supported && v.nested.Reason && (
              <p className="col-span-2 text-sm text-danger">{v.nested.Reason}</p>
            )}
            {/* 「重启就没了」必须说出来：用户看到"已启用"而重启之后变回去，
                会以为是面板没保存成功。 */}
            {v.nested.Enabled && !v.nested.Persistent && (
              <p className="col-span-2 rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
                <span className="font-medium">重启后会失效。</span>
                {v.nested.Fix}
              </p>
            )}
          </TuningCard>

          <section className="flex flex-col gap-3">
            <header className="flex items-center justify-between">
              <h2 className="text-base font-medium text-ink-2">
                CPU 亲和性预设（{v.presets?.length ?? 0}）
              </h2>
              <Button size="sm" variant="secondary" onClick={() => setPresetOpen(true)}>
                新建预设
              </Button>
            </header>
            {(v.presets ?? []).length === 0 ? (
              <EmptyState
                title="还没有预设"
                description="把常用的 CPU 绑定组合存成预设——cpuset 的写法不直观，而写错了不会报错，只表现为性能不如预期。"
              />
            ) : (
              <div className="overflow-hidden rounded-card border border-line">
                <table className="w-full text-left text-sm">
                  <tbody>
                    {(v.presets ?? []).map((p) => (
                      <tr key={p.id} className="border-b border-line last:border-0">
                        <td className="px-3 py-2 text-ink">{p.name}</td>
                        <td className="kc-mono px-3 py-2 text-ink-2">{p.cpuset}</td>
                        <td className="px-3 py-2 text-xs text-ink-3">{p.cpuset_desc}</td>
                        <td className="px-3 py-2 text-right">
                          <button
                            className="text-danger hover:underline"
                            onClick={() => removePreset.mutate(p.id)}
                          >
                            删除
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

      <PresetModal
        open={presetOpen}
        nodeID={effectiveNodeID}
        onClose={() => setPresetOpen(false)}
        onDone={() => {
          setPresetOpen(false)
          setError('')
          refresh()
        }}
        onError={(m) => {
          setPresetOpen(false)
          setError(m)
        }}
      />
    </div>
  )
}

function TuningCard({
  item,
  enabled,
  busy,
  onToggle,
  children,
}: {
  item: TuningItem | undefined
  enabled: boolean
  busy: boolean
  onToggle: () => void
  children?: React.ReactNode
}) {
  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <span className="flex items-baseline gap-2">
            <span className="text-base font-medium text-ink">{item?.label ?? ''}</span>
            <StatusBadge tone={enabled ? 'success' : 'warning'}>
              {enabled ? '已启用' : '未启用'}
            </StatusBadge>
          </span>
          <span className="text-sm text-ink-3">{item?.Benefit}</span>
        </div>
        <Button size="sm" variant={enabled ? 'secondary' : 'primary'} loading={busy} onClick={onToggle}>
          {enabled ? '停用' : '启用'}
        </Button>
      </div>

      {/* **代价**单独用一行标出来：它不是补充说明，而是决策依据的一半。 */}
      <p className="mt-2 rounded-control bg-warning/5 px-3 py-2 text-sm text-warning">
        {item?.Cost}
      </p>

      {enabled && children && (
        <div className="mt-3 grid gap-x-6 gap-y-2 sm:grid-cols-2">{children}</div>
      )}
      {enabled && item?.Metric && (
        <p className="mt-2 text-xs text-ink-3">判断依据：{item.Metric}</p>
      )}
    </section>
  )
}

function ZRAMCard({
  item,
  state,
  nodeID,
  onDone,
  onError,
}: {
  item: TuningItem | undefined
  state: { Enabled: boolean; DisksizeBytes: number; UsedBytes: number; OrigDataBytes: number; Algorithm: string; MemLimitBytes: number }
  nodeID: number
  onDone: () => void
  onError: (m: string) => void
}) {
  const [open, setOpen] = useState(false)
  const ratio = state.UsedBytes > 0 ? state.OrigDataBytes / state.UsedBytes : 0

  const apply = useMutation({
    mutationFn: (req: { item: string; enabled?: boolean; disksize_mb?: number; algorithm?: string; mem_limit_mb?: number }) =>
      hostTuningApi.apply(nodeID, req),
    onSuccess: () => {
      setOpen(false)
      onDone()
    },
    onError: (e) => onError(describe(e)),
  })

  return (
    <>
      <TuningCard item={item} enabled={state.Enabled} busy={apply.isPending} onToggle={() => setOpen(true)}>
        <Metric label="已分配" value={formatBytes(state.DisksizeBytes)} />
        <Metric label="实际占用" value={formatBytes(state.UsedBytes)} />
        <Metric
          label="压缩率"
          value={ratio > 0 ? `${ratio.toFixed(1)} ×` : '—'}
        />
        <Metric label="压缩算法" value={state.Algorithm || '—'} />
        {/* 压缩率低于 2 倍时，它占的内存可能不如直接用掉划算。 */}
        {state.Enabled && ratio > 0 && ratio < 2 && (
          <p className="col-span-2 rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
            压缩率只有 {ratio.toFixed(1)} 倍。ZRAM 挤占的是同一块物理内存——
            压缩率低时，它省下的可能还不如它占的多。
          </p>
        )}
      </TuningCard>

      <ZRAMModal open={open} state={state} busy={apply.isPending} onClose={() => setOpen(false)} onApply={(r) => apply.mutate(r)} />
    </>
  )
}

function ZRAMModal({
  open,
  state,
  busy,
  onClose,
  onApply,
}: {
  open: boolean
  state: { Enabled: boolean }
  busy: boolean
  onClose: () => void
  onApply: (r: { item: string; enabled?: boolean; disksize_mb?: number; algorithm?: string; mem_limit_mb?: number }) => void
}) {
  const [size, setSize] = useState('4096')
  const [algo, setAlgo] = useState('lz4')
  const [limit, setLimit] = useState('2048')

  return (
    <Modal
      open={open}
      title="配置 ZRAM"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          {state.Enabled && (
            <Button variant="secondary" size="sm" loading={busy} onClick={() => onApply({ item: 'zram', enabled: false })}>
              停用
            </Button>
          )}
          <Button
            size="sm"
            loading={busy}
            onClick={() =>
              onApply({
                item: 'zram',
                enabled: true,
                disksize_mb: Number(size) || 0,
                algorithm: algo,
                mem_limit_mb: Number(limit) || 0,
              })
            }
          >
            应用
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="容量（MB）"
          value={size}
          onChange={(e) => setSize(e.target.value)}
          hint="分配给压缩区的容量。注意：它挤占的是同一块物理内存——分配 4GB 就少了 4GB 可用内存，只是那 4GB 里的内容被压缩后占得更少。"
        />
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">压缩算法</label>
          <select
            value={algo}
            onChange={(e) => setAlgo(e.target.value)}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value="lz4">lz4（更快，压缩率低）</option>
            <option value="zstd">zstd（压缩率高，更吃 CPU）</option>
            <option value="lzo">lzo（折中）</option>
          </select>
          <p className="text-xs text-ink-3">
            这是一个取舍：CPU 吃紧选 lz4，内存吃紧选 zstd。
          </p>
        </div>
        <Input
          label="内存阈值（MB）"
          value={limit}
          onChange={(e) => setLimit(e.target.value)}
          hint="内存用到这个量才开始压缩换出。留空或 0 表示不限制。"
        />
      </div>
    </Modal>
  )
}

function PresetModal({
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
  const [name, setName] = useState('')
  const [cpuset, setCpuset] = useState('')
  const [remark, setRemark] = useState('')

  const create = useMutation({
    mutationFn: () =>
      hostTuningApi.createPreset(nodeID, { name: name.trim(), cpuset: cpuset.trim(), remark: remark || undefined }),
    onSuccess: onDone,
    onError: (e) => onError(describe(e)),
  })

  return (
    <Modal
      open={open}
      title="新建 CPU 亲和性预设"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" disabled={!name.trim() || !cpuset.trim()} loading={create.isPending} onClick={() => create.mutate()}>
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input label="名称" value={name} onChange={(e) => setName(e.target.value)} />
        <Input
          label="CPU 集合"
          value={cpuset}
          placeholder="0-3 或 0,2,4"
          onChange={(e) => setCpuset(e.target.value)}
          hint="只允许数字、逗号与连字符。把这台虚拟机固定到这几个物理核上——避免它在核之间漂移。写错了不会报错，只会表现为性能不如预期，因此创建时会展开成核数给你确认。"
        />
        <Input label="备注" value={remark} onChange={(e) => setRemark(e.target.value)} />
      </div>
    </Modal>
  )
}

function Metric({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex gap-2 text-sm">
      <span className="w-24 shrink-0 text-ink-3">{label}</span>
      <span className="kc-nums text-ink">{value}</span>
    </div>
  )
}

function cardOf(items: TuningItem[], key: string): TuningItem | undefined {
  return items.find((i) => i.key === key)
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
