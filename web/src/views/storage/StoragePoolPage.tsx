import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import {
  FS_TYPES,
  POOL_STATUS_LABEL,
  POOL_STATUS_TONE,
  storageApi,
  storageExtraApi,
  type DiskView,
  type PartitionView,
  type PoolView,
} from '@/api/storage'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatBytes } from '@/utils/format'

export function StoragePoolPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [createTarget, setCreateTarget] = useState<DiskView | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<PoolView | null>(null)
  const [configTarget, setConfigTarget] = useState<PoolView | null>(null)
  const [unmountTarget, setUnmountTarget] = useState<PoolView | null>(null)
  const [partitionDisk, setPartitionDisk] = useState<DiskView | null>(null)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })

  // 默认选中第一个节点：绝大多数部署只有一个节点，让用户先选一次是多余的。
  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const pools = useQuery({
    queryKey: ['storage-pools', effectiveNodeID],
    queryFn: () => storageApi.pools(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const disks = useQuery({
    queryKey: ['disks', effectiveNodeID],
    queryFn: () => storageApi.disks(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: ['storage-pools'] })
    void queryClient.invalidateQueries({ queryKey: ['disks'] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  const trim = useMutation({
    mutationFn: () => storageExtraApi.trim(effectiveNodeID),
    onSuccess: (r) => setNotice(r.message || 'trim 已完成'),
    onError: (err) => setError(describe(err)),
  })

  const setDefault = useMutation({
    mutationFn: ({ id, value }: { id: number; value: boolean }) =>
      storageApi.setDefault(id, value),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (err) => setError(describe(err)),
  })

  if (nodes.isPending) return <PageLoading />

  return (
    <div className="flex flex-col gap-5">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-xl font-semibold text-ink">存储池</h1>
          <p className="mt-1 text-base text-ink-3">
            存储池是把节点上的一块设备格式化并挂载后提供的存储空间。创建会
            <span className="text-warning">销毁设备上的原有数据</span>，操作前需要完成二次验证。
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

      {error && (
        <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}
      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}

      {(nodes.data ?? []).length === 0 && (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState title="还没有节点" description="存储池建立在节点上，请先接入节点。" />
        </div>
      )}

      {effectiveNodeID > 0 && (
        <>
          {/* 空间分配视图（F-5-01 §1.2 的目标之一）。
              各盘的状态标签与各池的容量在下面都有，但**没有一处回答
              「这台机器一共有多少存储、分别用在哪」**——而那正是管理员
              决定「要不要加盘」时要看的东西。

              分档按「先看是否已纳入存储池」的顺序判断：一块在池里的盘
              同时也是已挂载的，顺序反了会把池占用的容量记到「已挂载但
              未纳入池」那一档里，而两档的含义完全不同。 */}
          <AllocationSummary pools={pools.data ?? []} disks={disks.data ?? []} />

          <section className="flex flex-col gap-2">
            <h2 className="text-sm font-medium text-ink-2">存储池</h2>

            {pools.isPending && <PageLoading />}

            {pools.data && pools.data.length === 0 && (
              <div className="rounded-card border border-dashed border-line-strong">
                <EmptyState
                  title="该节点还没有存储池"
                  description="从下方磁盘清单中选择一块空闲设备创建。"
                />
              </div>
            )}

            {pools.data && pools.data.length > 0 && (
              <div className="overflow-x-auto rounded-card border border-line">
                <table className="w-full border-collapse text-base">
                  <thead>
                    <tr className="bg-sunken text-left text-xs text-ink-2">
                      <th className="px-4 py-2.5 font-medium">设备</th>
                      <th className="px-4 py-2.5 font-medium">挂载点</th>
                      <th className="px-4 py-2.5 font-medium">文件系统</th>
                      <th className="px-4 py-2.5 font-medium">容量</th>
                      <th className="px-4 py-2.5 font-medium">状态</th>
                      <th className="px-4 py-2.5 text-right font-medium">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pools.data.map((pool) => (
                      <tr key={pool.id} className="border-t border-line hover:bg-raised">
                        <td className="px-4 py-2.5">
                          <span className="kc-mono text-ink">{pool.device_path || pool.device_id}</span>
                          {pool.is_default && (
                            <span className="ml-2 text-xs text-brand">默认</span>
                          )}
                        </td>
                        <td className="kc-mono px-4 py-2.5 text-ink-2">{pool.mount_path || '—'}</td>
                        <td className="px-4 py-2.5 text-ink-2">{pool.fs_type || '—'}</td>
                        <td className="px-4 py-2.5">
                          <div className="flex flex-col">
                            <span className="kc-nums text-ink">
                              {formatBytes(pool.usable_bytes)} 可用
                            </span>
                            <span className="kc-nums text-xs text-ink-3">
                              共 {formatBytes(pool.total_bytes)}
                            </span>
                          </div>
                        </td>
                        <td className="px-4 py-2.5">
                          <div className="flex flex-col gap-1">
                            <StatusBadge tone={POOL_STATUS_TONE[pool.status]}>
                              {POOL_STATUS_LABEL[pool.status]}
                            </StatusBadge>
                            {pool.stale && (
                              <span className="text-xs text-warning">容量数据可能已过期</span>
                            )}
                          </div>
                        </td>
                        <td className="px-4 py-2.5 text-right">
                          <div className="flex justify-end gap-1">
                            <Button
                              variant="ghost"
                              size="sm"
                              disabled={pool.is_default || setDefault.isPending}
                              onClick={() => setDefault.mutate({ id: pool.id, value: true })}
                            >
                              {pool.is_default ? '已是默认' : '设为默认'}
                            </Button>
                            {/* 配置：挂载点 / 开机自动挂载。它要下发到节点改
                                挂载与 fstab，因此是异步的，与「设为默认」那
                                种只改一列的操作不是一回事。 */}
                            <Button variant="ghost" size="sm" onClick={() => setConfigTarget(pool)}>
                              配置
                            </Button>
                            <Button
                              variant="ghost"
                              size="sm"
                              disabled={pool.status === 'unmounted'}
                              title={pool.status === 'unmounted' ? '该池已卸载' : '卸载（保留数据）'}
                              onClick={() => setUnmountTarget(pool)}
                            >
                              卸载
                            </Button>
                            <Button variant="ghost" size="sm" onClick={() => setDeleteTarget(pool)}>
                              删除
                            </Button>
                          </div>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>

          <section className="flex flex-col gap-2">
            <h2 className="text-sm font-medium text-ink-2">磁盘</h2>
            {/* trim：删掉虚拟磁盘之后，宿主机的块设备往往还占着那些块；
                没有它，"删了 200 GB 但可用空间没变"无法解释。 */}
            <div className="flex justify-end">
              <Button
                size="sm"
                variant="secondary"
                disabled={effectiveNodeID === 0 || trim.isPending}
                onClick={() => trim.mutate()}
              >
                回收空间（trim）
              </Button>
            </div>

            {disks.isPending && <PageLoading />}

            {disks.isError && (
              <p className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
                {describe(disks.error)}
              </p>
            )}

            {disks.data && disks.data.length === 0 && (
              <div className="rounded-card border border-dashed border-line-strong">
                <EmptyState title="该节点没有可用磁盘" description="新插入设备后刷新即可看到。" />
              </div>
            )}

            {disks.data && disks.data.length > 0 && (
              <div className="overflow-x-auto rounded-card border border-line">
                <table className="w-full border-collapse text-base">
                  <thead>
                    <tr className="bg-sunken text-left text-xs text-ink-2">
                      <th className="px-4 py-2.5 font-medium">设备</th>
                      <th className="px-4 py-2.5 font-medium">容量</th>
                      <th className="px-4 py-2.5 font-medium">当前状态</th>
                      <th className="px-4 py-2.5 text-right font-medium">操作</th>
                    </tr>
                  </thead>
                  <tbody>
                    {disks.data.map((disk) => (
                      <DiskRow
                        key={disk.device_id}
                        disk={disk}
                        onCreate={() => setCreateTarget(disk)}
                        onPartitions={() => setPartitionDisk(disk)}
                      />
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </section>
        </>
      )}

      <CreatePoolModal
        disk={createTarget}
        nodeID={effectiveNodeID}
        onClose={() => setCreateTarget(null)}
        onCreated={() => {
          setCreateTarget(null)
          refresh()
        }}
      />

      <PoolConfigModal
        pool={configTarget}
        onClose={() => setConfigTarget(null)}
        onSubmitted={() => {
          setConfigTarget(null)
          refresh()
        }}
      />

      <UnmountPoolModal
        pool={unmountTarget}
        onClose={() => setUnmountTarget(null)}
        onSubmitted={() => {
          setUnmountTarget(null)
          refresh()
        }}
      />

      <PartitionModal
        disk={partitionDisk}
        nodeID={effectiveNodeID}
        onClose={() => setPartitionDisk(null)}
      />

      <DeletePoolModal
        pool={deleteTarget}
        onClose={() => {
          setDeleteTarget(null)
          setError('')
        }}
        onDeleted={() => {
          setDeleteTarget(null)
          setError('')
          refresh()
        }}
      />
    </div>
  )
}

function DiskRow({
  disk,
  onCreate,
  onPartitions,
}: {
  disk: DiskView
  onCreate: () => void
  onPartitions: () => void
}) {
  // 状态标签按「最需要用户注意」的顺序只显示一个：一块盘同时是系统盘
  // 又已挂载时，把两个标签都堆上去只会让表格难以扫读。
  let label = '空闲'
  let tone: 'success' | 'idle' | 'warning' | 'info' = 'success'
  if (disk.in_use_by) {
    label = '已被存储池使用'
    tone = 'idle'
  } else if (disk.is_system) {
    label = '系统盘'
    tone = 'idle'
  } else if (disk.mounted) {
    label = '已挂载'
    tone = 'warning'
  } else if (disk.has_data) {
    label = '含数据'
    tone = 'warning'
  }

  return (
    <tr className="border-t border-line hover:bg-raised">
      <td className="px-4 py-2.5">
        <span className="kc-mono text-ink">{disk.path}</span>
        {disk.mount_point && (
          <span className="ml-2 text-xs text-ink-3">挂载于 {disk.mount_point}</span>
        )}
      </td>
      <td className="kc-nums px-4 py-2.5 text-ink-2">{formatBytes(disk.size_bytes)}</td>
      <td className="px-4 py-2.5">
        <StatusBadge tone={tone}>{label}</StatusBadge>
      </td>
      <td className="px-4 py-2.5 text-right">
        <div className="flex justify-end gap-1">
          {/* 分区：整盘建池之外的一条路——把一块大盘切成几个区分别纳管。 */}
          <Button variant="ghost" size="sm" onClick={onPartitions}>
            分区
          </Button>
          <Button variant="ghost" size="sm" disabled={!disk.usable} onClick={onCreate}>
            {disk.usable ? '创建存储池' : '不可用'}
          </Button>
        </div>
      </td>
    </tr>
  )
}

function CreatePoolModal({
  disk,
  nodeID,
  onClose,
  onCreated,
}: {
  disk: DiskView | null
  nodeID: number
  onClose: () => void
  onCreated: () => void
}) {
  const [fsType, setFSType] = useState<string>('ext4')
  const [confirmName, setConfirmName] = useState('')
  const [confirmDataLoss, setConfirmDataLoss] = useState(false)
  const [error, setError] = useState('')

  const create = useMutation({
    mutationFn: () =>
      storageApi.create({
        node_id: nodeID,
        device_id: disk!.device_id,
        fs_type: fsType,
        confirm_device_name: confirmName,
        confirm_data_loss: confirmDataLoss,
      }),
    onSuccess: () => {
      setConfirmName('')
      setConfirmDataLoss(false)
      setError('')
      onCreated()
    },
    onError: (err) => setError(describe(err)),
  })

  function handleClose() {
    setFSType('ext4')
    setConfirmName('')
    setConfirmDataLoss(false)
    setError('')
    onClose()
  }

  function submit(event: FormEvent) {
    event.preventDefault()
    setError('')
    create.mutate()
  }

  if (!disk) return null

  // 设备名确认必须逐字符匹配，前端也照此禁用提交：让按钮在输入正确之前
  // 就不可点，比让用户提交后再收到「确认不一致」更清楚。
  const nameMatches = confirmName.trim() === disk.path
  const dataLossNotConfirmed = disk.has_data && !confirmDataLoss

  return (
    <Modal
      open
      title={`在 ${disk.path} 上创建存储池`}
      description={`将格式化为 ${fsType} 并挂载，该设备上的数据会被销毁。`}
      onClose={handleClose}
    >
      <form onSubmit={submit} className="flex flex-col gap-4">
        <div className="rounded-control border border-warning/40 bg-warning/10 px-3 py-2.5 text-base text-ink-2">
          <p>
            设备：<span className="kc-mono text-ink">{disk.path}</span>
            <span className="mx-2 text-ink-3">·</span>
            容量：<span className="kc-nums text-ink">{formatBytes(disk.size_bytes)}</span>
          </p>
          {disk.has_data && (
            <p className="mt-1.5 text-warning">
              该设备上已存在{disk.filesystem ? ` ${disk.filesystem} ` : ''}
              文件系统，其中的数据将被永久销毁。
            </p>
          )}
        </div>

        <div className="flex flex-col gap-1.5">
          <label htmlFor="fs-type" className="text-sm font-medium text-ink-2">
            文件系统
          </label>
          <select
            id="fs-type"
            value={fsType}
            onChange={(e) => setFSType(e.target.value)}
            disabled={create.isPending}
            className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
          >
            {FS_TYPES.map((fs) => (
              <option key={fs} value={fs}>
                {fs}
              </option>
            ))}
          </select>
        </div>

        {disk.has_data && (
          <label className="flex cursor-pointer items-start gap-2.5 rounded-control border border-warning/40 px-3 py-2.5">
            <input
              type="checkbox"
              className="mt-0.5"
              checked={confirmDataLoss}
              onChange={(e) => setConfirmDataLoss(e.target.checked)}
            />
            <span className="text-base text-ink">
              我确认该设备上的数据可以被销毁
            </span>
          </label>
        )}

        <Input
          label="输入设备名以确认"
          value={confirmName}
          onChange={(e) => setConfirmName(e.target.value)}
          placeholder={disk.path}
          hint={`请完整输入 ${disk.path}。这一步是为了确认你选中的确实是这块设备。`}
          autoComplete="off"
          disabled={create.isPending}
        />

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}

        <div className="flex justify-end gap-2">
          <Button variant="secondary" size="sm" type="button" onClick={handleClose}>
            取消
          </Button>
          <Button
            variant="danger"
            size="sm"
            type="submit"
            disabled={!nameMatches || dataLossNotConfirmed}
            loading={create.isPending}
          >
            格式化并创建
          </Button>
        </div>
      </form>
    </Modal>
  )
}

function DeletePoolModal({
  pool,
  onClose,
  onDeleted,
}: {
  pool: PoolView | null
  onClose: () => void
  onDeleted: () => void
}) {
  const [error, setError] = useState('')

  const remove = useMutation({
    mutationFn: () => storageApi.remove(pool!.id),
    onSuccess: () => {
      setError('')
      onDeleted()
    },
    onError: (err) => setError(describe(err)),
  })

  if (!pool) return null

  return (
    <Modal
      open
      title={`删除存储池 ${pool.device_path || pool.device_id}`}
      description="池内的磁盘会被一并销毁，操作不可撤销。若池内仍有虚拟机磁盘，删除会被拒绝并列出占用者。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button variant="danger" size="sm" loading={remove.isPending} onClick={() => remove.mutate()}>
            确认删除
          </Button>
        </>
      }
    >
      {error && (
        <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      )}
      <PoolSummary pool={pool} />
    </Modal>
  )
}

function PoolSummary({ pool }: { pool: PoolView }) {
  return (
    <dl className="flex flex-col gap-2 text-base">
      <div className="flex gap-3">
        <dt className="w-24 text-ink-3">挂载点</dt>
        <dd className="kc-mono text-ink">{pool.mount_path || '—'}</dd>
      </div>
      <div className="flex gap-3">
        <dt className="w-24 text-ink-3">当前容量</dt>
        <dd className="kc-nums text-ink">
          {formatBytes(pool.usable_bytes)} 可用 / 共 {formatBytes(pool.total_bytes)}
        </dd>
      </div>
    </dl>
  )
}

/**
 * PoolConfigModal 编辑池配置（挂载点 / 开机自动挂载 / 备注）。
 *
 * 它走**异步**下发，与 PATCH（设为默认）分开：后者只改控制面的一列并同步
 * 返回，而这里要动宿主机上的挂载与 fstab。把两者放进同一个"保存"会让不同
 * 字段有完全不同的耗时与失败语义，用户没法从界面上预知。
 */
function PoolConfigModal({
  pool,
  onClose,
  onSubmitted,
}: {
  pool: PoolView | null
  onClose: () => void
  onSubmitted: () => void
}) {
  const [mountPath, setMountPath] = useState('')
  const [autoMount, setAutoMount] = useState(true)
  const [remark, setRemark] = useState('')
  const [seeded, setSeeded] = useState<number | null>(null)

  if (seeded !== (pool?.id ?? null)) {
    setSeeded(pool?.id ?? null)
    setMountPath(pool?.mount_path ?? '')
    setAutoMount(true)
    setRemark(pool?.remark ?? '')
  }

  const save = useMutation({
    mutationFn: () =>
      storageExtraApi.updatePoolConfig(pool?.id ?? 0, {
        mount_path: mountPath.trim() || undefined,
        auto_mount: autoMount,
        remark: remark.trim() || undefined,
      }),
    onSuccess: onSubmitted,
  })

  return (
    <Modal
      open={pool !== null}
      title={`配置「${pool?.device_path || pool?.device_id || ''}」`}
      description="挂载点与开机自动挂载会下发到节点，因此是异步的。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={save.isPending} onClick={() => save.mutate()}>
            保存
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Input
          label="挂载点"
          value={mountPath}
          onChange={(e) => setMountPath(e.target.value)}
          placeholder="留空表示不改"
        />
        {/* 默认 true 必须可见：默认 false 的表现是"重启之后所有虚拟机找不到
            磁盘"——那是一个要重启才会暴露的默认值。 */}
        <label className="flex cursor-pointer items-start gap-2 text-sm text-ink-2">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={autoMount}
            onChange={(e) => setAutoMount(e.target.checked)}
          />
          <span>
            开机自动挂载
            <span className="block text-xs text-ink-3">
              关闭后宿主机重启时该池不会自动挂载，其上的虚拟机将无法读写磁盘。
            </span>
          </span>
        </label>
        <Input label="备注" value={remark} onChange={(e) => setRemark(e.target.value)} />
        {save.isError && <p className="text-sm text-danger">{describe(save.error)}</p>}
      </div>
    </Modal>
  )
}

/** 卸载存储池（保留数据）。与删除的区别必须在弹窗里说清。 */
function UnmountPoolModal({
  pool,
  onClose,
  onSubmitted,
}: {
  pool: PoolView | null
  onClose: () => void
  onSubmitted: () => void
}) {
  const run = useMutation({
    mutationFn: () => storageExtraApi.unmountPool(pool?.id ?? 0),
    onSuccess: onSubmitted,
  })

  return (
    <Modal
      open={pool !== null}
      title={`卸载「${pool?.device_path || pool?.device_id || ''}」`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button variant="danger" size="sm" loading={run.isPending} onClick={() => run.mutate()}>
            卸载
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-2">
        <p className="text-base text-ink-2">卸载只摘掉挂载并清除开机挂载项，**数据保留**。</p>
        <p className="text-sm text-ink-3">
          它与「删除」不同：删除会清掉池内的数据，卸载不会。卸载后该池上的虚拟机
          无法读写磁盘，直到重新挂载。
        </p>
        {run.isError && <p className="text-sm text-danger">{describe(run.error)}</p>}
      </div>
    </Modal>
  )
}

/**
 * PartitionModal 查看与管理一块磁盘上的分区。
 *
 * 分区是**节点侧的事实**，控制面不缓存：手工改过分区表之后，任何缓存都会
 * 变成错的——而"面板说有 3 个分区、实际只有 1 个"比"读不到"更危险。
 *
 * 删除**全部**分区与删单个分成两个按钮：一次点击抹掉整张分区表，与"删掉
 * 第三个"不是一个量级的动作。
 */
function PartitionModal({
  disk,
  nodeID,
  onClose,
}: {
  disk: DiskView | null
  nodeID: number
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [sizeGB, setSizeGB] = useState('')
  const [error, setError] = useState('')

  const list = useQuery({
    queryKey: ['partitions', nodeID, disk?.device_id],
    queryFn: () => storageExtraApi.partitions(nodeID, disk?.device_id ?? ''),
    enabled: disk !== null && nodeID > 0,
  })

  const create = useMutation({
    mutationFn: () =>
      storageExtraApi.createPartition({
        node_id: nodeID,
        device_id: disk?.device_id ?? '',
        size_gb: Number(sizeGB) || 0,
      }),
    onSuccess: () => {
      setSizeGB('')
      void queryClient.invalidateQueries({ queryKey: ['partitions'] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const remove = useMutation({
    mutationFn: (input: { index?: number; all?: boolean }) =>
      storageExtraApi.deletePartitions({
        node_id: nodeID,
        device_id: disk?.device_id ?? '',
        index: input.index,
        all: input.all,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['partitions'] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const items = list.data?.items ?? []

  return (
    <Modal
      open={disk !== null}
      title={`${disk?.path ?? ''} 的分区`}
      onClose={onClose}
      footer={
        <Button size="sm" onClick={onClose}>
          关闭
        </Button>
      }
    >
      <div className="flex flex-col gap-3">
        {list.data?.unavailable && <p className="text-sm text-ink-3">{list.data.unavailable}</p>}

        {items.length > 0 && (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="text-left text-xs text-ink-3">
                <th className="px-2 py-1.5 font-normal">分区</th>
                <th className="px-2 py-1.5 font-normal">容量</th>
                <th className="px-2 py-1.5 font-normal">文件系统</th>
                <th className="px-2 py-1.5 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {items.map((p: PartitionView) => (
                <tr key={p.index} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="kc-mono px-2 py-1.5 text-ink">
                    {p.path}
                    {p.system && <span className="ml-1.5 text-xs text-ink-3">系统</span>}
                    {p.in_use_by_pool && (
                      <span className="ml-1.5 text-xs text-ink-3">被池占用</span>
                    )}
                  </td>
                  <td className="kc-nums px-2 py-1.5 text-ink-2">{formatBytes(p.size_bytes)}</td>
                  <td className="px-2 py-1.5 text-ink-2">{p.fs_type || '未格式化'}</td>
                  <td className="px-2 py-1.5 text-right">
                    <button
                      className="text-sm text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
                      // 系统分区不给删除入口：删它的后果不是"少一块空间"，
                      // 而是宿主机可能起不来。
                      disabled={p.system || p.in_use_by_pool !== ''}
                      onClick={() => remove.mutate({ index: p.index })}
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}

        <div className="flex items-end gap-2">
          <Input
            label="新建分区大小（GB）"
            value={sizeGB}
            onChange={(e) => setSizeGB(e.target.value)}
            placeholder="例如 200"
          />
          <Button size="sm" loading={create.isPending} onClick={() => create.mutate()}>
            创建分区
          </Button>
        </div>

        <div className="border-t border-line pt-2">
          <Button
            variant="danger"
            size="sm"
            loading={remove.isPending}
            onClick={() => remove.mutate({ all: true })}
          >
            删除全部分区
          </Button>
          <p className="mt-1 text-xs text-ink-3">
            会清空整张分区表，其上所有数据都会丢失；需要二次验证。
          </p>
        </div>

        {error && <p className="text-sm text-danger">{error}</p>}
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}

/**
 * AllocationSummary 是「空间分配视图」（F-5-01 §1.2）。
 *
 * 各盘的标签与各池的容量在下面都有，但没有一处回答**「这台机器一共有多少
 * 存储、分别用在哪」**——而那正是管理员决定「要不要加盘」时要看的东西。
 *
 * 分档顺序有讲究：**先判断是否已纳入存储池**。一块在池里的盘同时也是已挂载
 * 的，顺序反了就会把池占用的容量记进「已挂载但未纳入池」那一档，而两档对
 * 管理员意味着完全不同的事——前者是「正在提供存储」，后者是「装着呢但没在用」。
 */
function AllocationSummary({ pools, disks }: { pools: PoolView[]; disks: DiskView[] }) {
  const poolTotal = pools.reduce((s, p) => s + p.total_bytes, 0)
  const poolUsable = pools.reduce((s, p) => s + p.usable_bytes, 0)

  let inPool = 0
  let system = 0
  let mountedNotPool = 0
  let free = 0
  for (const d of disks) {
    if (d.in_use_by) inPool += d.size_bytes
    else if (d.is_system) system += d.size_bytes
    else if (d.mounted) mountedNotPool += d.size_bytes
    else free += d.size_bytes
  }

  const cell = (label: string, value: number, hint: string, tone = 'text-ink') => (
    <div className="flex flex-col gap-0.5">
      <span className="text-xs text-ink-3">{label}</span>
      <span className={`kc-nums text-base ${tone}`}>{formatBytes(value)}</span>
      <span className="text-xs text-ink-3">{hint}</span>
    </div>
  )

  return (
    <section className="rounded-card border border-line bg-surface px-4 py-3">
      <div className="flex flex-wrap items-start justify-between gap-2">
        <h2 className="text-sm font-medium text-ink-2">空间分配</h2>
        {pools.length > 0 && (
          <span className="text-xs text-ink-3">
            池内合计 {formatBytes(poolUsable)} 可用 / 共 {formatBytes(poolTotal)}
          </span>
        )}
      </div>

      <div className="mt-2 grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-4">
        {cell('已纳入存储池', inPool, `${pools.length} 个池`, 'text-ink')}
        {cell('系统盘占用', system, '不可用于存储池', 'text-ink-3')}
        {cell('已挂载未纳池', mountedNotPool, '装着呢但没在用', 'text-warning')}
        {cell('未分配（可用）', free, '可以建池', 'text-success')}
      </div>

      {mountedNotPool > 0 && (
        <p className="mt-2 text-xs text-warning">
          有磁盘已挂载但未纳入任何存储池：它们占着容量却不提供存储。若这些
          挂载点不再需要，可以先卸载再建池。
        </p>
      )}
      {disks.length > 0 && free === 0 && mountedNotPool === 0 && (
        <p className="mt-2 text-xs text-ink-3">
          这台节点上已经没有可分配的磁盘了。需要扩容时先接入新设备。
        </p>
      )}
    </section>
  )
}
