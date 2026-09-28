/**
 * MigrateSection 处理虚拟机跨节点迁移（F-2-09 / F-2-15）。
 *
 * 迁移方式由机器的实时状态决定并要写明白：**运行中走热迁移**（停顿窗口
 * 毫秒级、收敛性有量化预检），**已关机走停机迁移**（停机时长≈复制时长）。
 * 不把"这次走哪条路"写在按钮旁边，用户会拿对一种方式的预期去接受另一种
 * 方式的后果。
 *
 * 其次是**冲突与余量要提前列出**（F-6-03 / F-6-04）：静态地址与端口转发
 * 在节点内唯一，换一台宿主机就可能撞上别人；目标的容量余量来自周期采样，
 * 后端给出建议但**放哪儿终究是人的决定**。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { vmApi, type MigrationView, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatDateTime } from '@/utils/format'

export function MigrateSection({ vm }: { vm: VmView }) {
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const [error, setError] = useState('')

  const migrations = useQuery({
    queryKey: ['vm-migrations', vm.id],
    queryFn: () => vmApi.migrations(vm.id),
    refetchInterval: (q) =>
      (q.state.data?.items ?? []).some((m) => m.status === 'pending' || m.status === 'running')
        ? 3000
        : false,
  })

  const items = migrations.data?.items ?? []
  const busy = items.some((m) => m.status === 'pending' || m.status === 'running')

  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h3 className="text-sm text-ink-3">跨节点迁移</h3>
          <p className="mt-1 text-base text-ink-2">
            把磁盘搬到另一台宿主机。硬件配置、网卡、静态地址与端口转发会一起搬走。
            <span className="text-ink-3">
              （运行中走热迁移、停顿窗口毫秒级；已关机走停机迁移、停机时长约等于复制时长。）
            </span>
          </p>
        </div>
        <Button
          size="sm"
          variant="secondary"
          disabled={(vm.status !== 'stopped' && vm.status !== 'running') || busy}
          title={
            vm.status !== 'stopped' && vm.status !== 'running'
              ? '暂停等状态需要先恢复或关机再迁移'
              : busy
                ? '已有进行中的迁移'
                : ''
          }
          onClick={() => setOpen(true)}
        >
          迁移
        </Button>
      </div>

      {error && (
        <p className="mt-2.5 rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {items.length > 0 && (
        <ul className="mt-3 flex flex-col">
          {items.map((m) => (
            <li
              key={m.id}
              className="flex items-start justify-between gap-3 border-t border-line py-2.5 text-base"
            >
              <span>
                <span className="text-ink">
                  节点 #{m.from_node_id} → #{m.to_node_id}
                  {m.mode === 'live' && (
                    <span className="ml-1.5 rounded-pill bg-brand/10 px-1.5 py-0.5 text-xs text-brand">
                      热迁移
                    </span>
                  )}
                </span>
                {/* 结果里说明跟着搬了些什么——只给一个「成功」会让用户
                    不确定「我原来接的网络、配的转发还在不在」。 */}
                {m.result && <span className="block text-xs text-ink-3">{m.result}</span>}
                {m.error && <span className="block text-xs text-danger">{m.error}</span>}
              </span>
              <span className="flex shrink-0 items-center gap-2">
                <span className="text-xs text-ink-3">{formatDateTime(m.created_at)}</span>
                <MigrationBadge status={m.status} />
              </span>
            </li>
          ))}
        </ul>
      )}

      <MigrateModal
        open={open}
        vm={vm}
        onClose={() => setOpen(false)}
        onDone={() => {
          setOpen(false)
          setError('')
          void queryClient.invalidateQueries({ queryKey: ['vm-migrations', vm.id] })
          void queryClient.invalidateQueries({ queryKey: ['vm', vm.id] })
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(msg) => {
          setOpen(false)
          setError(msg)
        }}
      />
    </section>
  )
}

function MigrationBadge({ status }: { status: MigrationView['status'] }) {
  const map: Record<MigrationView['status'], { label: string; tone: 'idle' | 'warning' | 'success' | 'danger' }> = {
    pending: { label: '排队中', tone: 'idle' },
    running: { label: '迁移中', tone: 'warning' },
    success: { label: '已完成', tone: 'success' },
    failed: { label: '失败', tone: 'danger' },
  }
  const m = map[status]
  return <StatusBadge tone={m.tone}>{m.label}</StatusBadge>
}

function MigrateModal({
  open,
  vm,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  vm: VmView
  onClose: () => void
  onDone: () => void
  onError: (message: string) => void
}) {
  const [toNodeID, setToNodeID] = useState(0)
  // 目标清单来自**聚合接口**（F-6-03）：容量、冲突、可否作为目标与建议都在
  // 服务端算好，前端不再自己拼节点列表——口径只有一份。
  const targets = useQuery({
    queryKey: ['vm-migrate-targets', vm.id],
    queryFn: () => vmApi.migrateTargets(vm.id),
    enabled: open,
  })

  const migrate = useMutation({
    mutationFn: () => vmApi.migrate(vm.id, toNodeID),
    onSuccess: () => onDone(),
    onError: (err) => onError(describe(err)),
  })

  // **选完目标节点就先预检**，而不是等点下去才看到一堆错误。
  //
  // 预检回答的是「这次要停多久」——那是用户在这一步真正要判断的事。把它放到
  // 点按钮之后，等于让他在没有依据的情况下做决定。
  const preview = useQuery({
    queryKey: ['migration-preview', vm.id, toNodeID],
    queryFn: () => vmApi.previewMigration(vm.id, toNodeID),
    enabled: open && toNodeID > 0,
  })

  const candidates = targets.data?.items ?? []

  return (
    <Modal
      open={open}
      title={`迁移「${vm.name}」`}
      description="迁移会把磁盘与节点内资源一起搬到目标宿主机。失败时源侧的数据保留，虚拟机在原处仍然可用。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            // 预检未通过时**直接禁用**，而不是让用户点了再被拒——那种报错
            // 会让他以为是自己选错了什么，而问题在于目标节点当前不可作为
            // 迁移目标。
            disabled={toNodeID === 0 || preview.data?.ready === false}
            loading={migrate.isPending}
            onClick={() => migrate.mutate()}
          >
            开始迁移
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Input label="当前节点" value={`#${vm.node_id}`} readOnly disabled />
        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">目标节点</label>
          <select
            value={toNodeID}
            onChange={(e) => setToNodeID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
          >
            <option value={0}>请选择…</option>
            {candidates.map((t) => (
              <option key={t.node_id} value={t.node_id}>
                {t.node_name}
                {t.recommended ? '（建议）' : ''}
                {t.maintenance ? '（维护中）' : ''}
              </option>
            ))}
          </select>
          {candidates.length === 0 && (
            <p className="text-xs text-ink-3">
              没有其它已接入的节点可作为目标。
            </p>
          )}

          {/* 目标节点的容量与冲突（F-6-03）：容量来自最近一次采样，标注
              "建议"的那一个是余量最大的——但只是建议。 */}
          {(() => {
            const sel = candidates.find((t) => t.node_id === toNodeID)
            if (!sel) return null
            return (
              <div className="flex flex-col gap-1 rounded-control border border-line bg-sunken px-3 py-2 text-sm">
                <span className="text-ink-2">
                  {sel.stats_known ? (
                    <>
                      容量：{sel.cpu_cores} 核 · 空闲内存{' '}
                      <span className="kc-nums">{Math.round(sel.mem_free_mb / 1024)}</span> GB ·
                      存储余量 <span className="kc-nums">{sel.storage_free_gb}</span> GB
                    </>
                  ) : (
                    <span className="text-ink-3">该节点还没有采样数据，容量暂不可知</span>
                  )}
                  {sel.recommended && (
                    <span className="ml-1.5 rounded-pill bg-success/10 px-1.5 py-0.5 text-xs text-success">
                      建议目标（余量最大）
                    </span>
                  )}
                </span>
                {!sel.suitable && sel.reason && <span className="text-danger">{sel.reason}</span>}
              </div>
            )
          })()}

          {/* 预检结果。**放在确认按钮之前**——它的全部意义就是让用户在动
              之前看到会付出什么代价。 */}
          {preview.isPending && <p className="text-xs text-ink-3">正在预检…</p>}
          {preview.data && (
            <div className="flex flex-col gap-2 rounded-control border border-line bg-sunken px-3 py-2.5">
              {(preview.data.blockers ?? []).length > 0 ? (
                <>
                  <span className="text-sm font-medium text-danger">当前无法迁移</span>
                  <ul className="flex flex-col gap-1">
                    {(preview.data.blockers ?? []).map((b) => (
                      <li key={b} className="text-sm text-danger">
                        {b}
                      </li>
                    ))}
                  </ul>
                </>
              ) : (
                <>
                  <span className="text-sm font-medium text-success">可以迁移</span>
                  {/* **「会怎么迁」比「能不能迁」更影响决定**：停机时长由它
                      决定，而用户很可能以为迁移是「不断服务地挪过去」。 */}
                  <p className="text-sm text-ink-2">{preview.data.mode_note}</p>
                  {/* 热迁移的收敛性结论（F-2-15）：带数字，用户拿它判断
                      这次热迁移是否平稳、要不要改停机迁移。 */}
                  {preview.data.mode === 'live' && preview.data.converge_note && (
                    <p className="text-sm text-ink-2">
                      {preview.data.converge_note}
                      {preview.data.auto_converge && (
                        <span className="ml-1.5 rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning">
                          将开启 CPU 限流
                        </span>
                      )}
                    </p>
                  )}
                  {/* 实测带宽（G-35）：标注来源，实测与估算的置信度不同。 */}
                  {preview.data.bandwidth_mbps ? (
                    <p className="text-sm text-ink-2">
                      实测带宽：
                      <span className="kc-nums">{preview.data.bandwidth_mbps} Mbps</span>
                      {preview.data.bandwidth_source === 'speedtest' && (
                        <span className="ml-1.5 rounded-pill bg-success/10 px-1.5 py-0.5 text-xs text-success">
                          实测
                        </span>
                      )}
                      {preview.data.bandwidth_source === 'estimate' && (
                        <span className="ml-1.5 rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning">
                          估算
                        </span>
                      )}
                    </p>
                  ) : null}
                  {preview.data.downtime_hint && (
                    <p className="text-sm text-ink-3">{preview.data.downtime_hint}</p>
                  )}
                </>
              )}
              {(preview.data.warnings ?? []).map((w) => (
                <p key={w} className="text-xs text-warning">
                  {w}
                </p>
              ))}
            </div>
          )}
        </div>

        <div className="rounded-control border border-line bg-raised px-3 py-2 text-sm text-ink-3">
          <p className="text-ink-2">迁移前会检查：</p>
          <ul className="mt-1 flex flex-col gap-0.5">
            <li>目标节点是否处于维护模式</li>
            <li>静态地址在目标节点上是否已被占用</li>
            <li>宿主机端口在目标节点上是否已被其它转发占用</li>
          </ul>
          <p className="mt-1.5">
            有冲突时会**直接拒绝并列出是哪一项**，而不是迁过去之后再出问题。
          </p>
        </div>
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
