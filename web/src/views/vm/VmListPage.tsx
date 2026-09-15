import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Link } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { vmApi, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatDateTime, relativeTime } from '@/utils/format'
import { VM_STATUS_LABEL, VM_STATUS_TONE } from '@/utils/labels'

const PAGE_SIZE = 20

export function VmListPage() {
  const [page, setPage] = useState(1)
  const [keyword, setKeyword] = useState('')
  const [search, setSearch] = useState('')
  const [createOpen, setCreateOpen] = useState(false)

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })
  const vms = useQuery({
    queryKey: ['vms', page, search],
    queryFn: () => vmApi.list({ page, page_size: PAGE_SIZE, keyword: search }),
  })

  // node_id → 名称。虚拟机只存节点 ID，界面上要显示人能认出的名字。
  const nodeNames = new Map((nodes.data ?? []).map((n) => [n.id, n.name]))
  const total = vms.data?.pagination.total ?? 0
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE))

  function submitSearch(event: FormEvent) {
    event.preventDefault()
    setPage(1)
    setSearch(keyword)
  }

  return (
    <div className="flex flex-col gap-4">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">虚拟机</h1>
          <p className="mt-1 text-base text-ink-3">
            共 {total} 台。状态来自最近一次与虚拟化层对账的结果。
          </p>
        </div>
        <Button size="sm" onClick={() => setCreateOpen(true)}>
          创建虚拟机
        </Button>
      </header>

      <form onSubmit={submitSearch} className="flex gap-2">
        <input
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          placeholder="搜索名称"
          className="h-8 w-64 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink placeholder:text-ink-3 focus:outline-none focus-visible:border-brand"
        />
        <Button variant="secondary" size="sm" type="submit">
          搜索
        </Button>
      </form>

      {vms.isPending && <PageLoading />}

      {vms.isError && (
        <div className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(vms.error)}
        </div>
      )}

      {vms.data && vms.data.items.length === 0 && (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState
            title={search ? '没有匹配的虚拟机' : '还没有虚拟机'}
            description={
              search
                ? '试试其他关键词，或清空搜索条件。'
                : '先接入节点，然后就可以在节点上创建虚拟机。'
            }
            action={
              search ? (
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => {
                    setKeyword('')
                    setSearch('')
                    setPage(1)
                  }}
                >
                  清空搜索
                </Button>
              ) : (
                <Button size="sm" onClick={() => setCreateOpen(true)}>
                  创建第一台虚拟机
                </Button>
              )
            }
          />
        </div>
      )}

      {vms.data && vms.data.items.length > 0 && (
        <>
          <div className="overflow-x-auto rounded-card border border-line">
            <table className="w-full border-collapse text-base">
              <thead>
                <tr className="bg-sunken text-left text-xs text-ink-2">
                  <th className="px-4 py-2.5 font-medium">名称</th>
                  <th className="px-4 py-2.5 font-medium">状态</th>
                  <th className="px-4 py-2.5 font-medium">配置</th>
                  <th className="px-4 py-2.5 font-medium">IP</th>
                  <th className="px-4 py-2.5 font-medium">节点</th>
                  <th className="px-4 py-2.5 font-medium">创建时间</th>
                </tr>
              </thead>
              <tbody>
                {vms.data.items.map((vm) => (
                  <VmRow key={vm.id} vm={vm} nodeName={nodeNames.get(vm.node_id)} />
                ))}
              </tbody>
            </table>
          </div>

          {totalPages > 1 && (
            <div className="flex items-center justify-end gap-2 text-base text-ink-2">
              <Button
                variant="secondary"
                size="sm"
                disabled={page <= 1}
                onClick={() => setPage((p) => p - 1)}
              >
                上一页
              </Button>
              <span className="kc-nums">
                {page} / {totalPages}
              </span>
              <Button
                variant="secondary"
                size="sm"
                disabled={page >= totalPages}
                onClick={() => setPage((p) => p + 1)}
              >
                下一页
              </Button>
            </div>
          )}
        </>
      )}

      <CreateVmModal open={createOpen} onClose={() => setCreateOpen(false)} />
    </div>
  )
}

function VmRow({ vm, nodeName }: { vm: VmView; nodeName?: string }) {
  return (
    <tr className="border-t border-line hover:bg-raised">
      <td className="px-4 py-2.5">
        <Link to={`/vm/${vm.id}`} className="font-medium text-ink hover:text-brand hover:underline">
          {vm.name}
        </Link>
        {vm.group_name && <span className="ml-2 text-xs text-ink-3">{vm.group_name}</span>}
      </td>
      <td className="px-4 py-2.5">
        <div className="flex items-center gap-2">
          <StatusBadge tone={VM_STATUS_TONE[vm.status]}>{VM_STATUS_LABEL[vm.status]}</StatusBadge>
          {/* 投影过期时明确提示：把陈旧数据显示成当前状态，排障时比没有数据更危险。 */}
          {vm.stale && (
            <span className="text-xs text-warning" title={`最近对账：${formatDateTime(vm.last_synced_at)}`}>
              数据可能陈旧
            </span>
          )}
        </div>
      </td>
      <td className="kc-nums px-4 py-2.5 text-ink-2">
        {vm.vcpu} 核 · {formatMemory(vm.memory_mb)} · {vm.disk_gb} GB
      </td>
      <td className="kc-mono px-4 py-2.5 text-ink-2">{vm.ip_summary || '—'}</td>
      <td className="px-4 py-2.5 text-ink-2">{nodeName ?? `#${vm.node_id}`}</td>
      <td className="px-4 py-2.5 text-ink-2">{relativeTime(vm.created_at)}</td>
    </tr>
  )
}

function formatMemory(mb: number): string {
  if (mb >= 1024) return `${(mb / 1024).toFixed(mb % 1024 === 0 ? 0 : 1)} GB`
  return `${mb} MB`
}

function CreateVmModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [nodeID, setNodeID] = useState(0)
  const [vcpu, setVcpu] = useState(2)
  const [memoryMB, setMemoryMB] = useState(2048)
  const [diskGB, setDiskGB] = useState(20)
  const [error, setError] = useState('')
  const [taskID, setTaskID] = useState<number | null>(null)

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })

  const create = useMutation({
    mutationFn: () =>
      vmApi.create({
        name: name.trim(),
        node_id: nodeID,
        vcpu,
        memory_mb: memoryMB,
        disk_gb: diskGB,
      }),
    onSuccess: (result) => {
      setTaskID(result.task_id)
      setError('')
      // 列表要刷新才能看到新虚拟机——创建是异步的，此刻它可能还没出现。
      void queryClient.invalidateQueries({ queryKey: ['vms'] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => setError(describe(err)),
  })

  function handleClose() {
    setName('')
    setNodeID(0)
    setError('')
    setTaskID(null)
    onClose()
  }

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setError('')
    if (!name.trim()) {
      setError('请输入虚拟机名')
      return
    }
    if (nodeID <= 0) {
      setError('请选择节点')
      return
    }
    create.mutate()
  }

  const availableNodes = nodes.data ?? []

  return (
    <Modal
      open={open}
      title={taskID === null ? '创建虚拟机' : '创建任务已提交'}
      description={
        taskID === null
          ? '创建是异步操作，提交后会生成一个任务，可在任务中心查看进度。'
          : '任务正在执行。任务完成后虚拟机才会出现在列表中。'
      }
      onClose={handleClose}
    >
      {taskID === null && (
        <form onSubmit={handleSubmit} className="flex flex-col gap-4">
          <Input
            label="名称"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="web-01"
            hint="1-63 位字母、数字或连字符，同一节点内不可重名"
            autoFocus
            disabled={create.isPending}
          />

          <div className="flex flex-col gap-1.5">
            <label htmlFor="vm-node" className="text-sm font-medium text-ink-2">
              节点
            </label>
            <select
              id="vm-node"
              value={nodeID}
              onChange={(e) => setNodeID(Number(e.target.value))}
              disabled={create.isPending}
              className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
            >
              <option value={0}>请选择节点</option>
              {availableNodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                  {n.status !== 'online' ? '（离线）' : ''}
                </option>
              ))}
            </select>
            {availableNodes.length === 0 && (
              <p className="text-sm text-ink-3">
                还没有可用节点，请先到
                <Link to="/node" className="mx-1 text-brand hover:underline">
                  节点管理
                </Link>
                接入。
              </p>
            )}
          </div>

          <div className="grid grid-cols-3 gap-3">
            <Input
              label="CPU（核）"
              type="number"
              min={1}
              value={vcpu}
              onChange={(e) => setVcpu(Number(e.target.value))}
              disabled={create.isPending}
            />
            <Input
              label="内存（MB）"
              type="number"
              min={128}
              step={128}
              value={memoryMB}
              onChange={(e) => setMemoryMB(Number(e.target.value))}
              disabled={create.isPending}
            />
            <Input
              label="磁盘（GB）"
              type="number"
              min={1}
              value={diskGB}
              onChange={(e) => setDiskGB(Number(e.target.value))}
              disabled={create.isPending}
            />
          </div>

          {error && (
            <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
              {error}
            </p>
          )}

          <div className="flex justify-end gap-2">
            <Button variant="secondary" size="sm" type="button" onClick={handleClose}>
              取消
            </Button>
            <Button size="sm" type="submit" loading={create.isPending}>
              提交创建
            </Button>
          </div>
        </form>
      )}

      {taskID !== null && (
        <div className="flex flex-col gap-4">
          <p className="text-base text-ink-2">
            任务 <span className="kc-mono text-ink">#{taskID}</span> 已提交。
          </p>
          <div className="flex justify-end gap-2">
            <Button variant="secondary" size="sm" onClick={handleClose}>
              留在本页
            </Button>
            <Link to="/task">
              <Button size="sm">查看任务</Button>
            </Link>
          </div>
        </div>
      )}
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
