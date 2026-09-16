import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'

import { Link } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi, type EnrollTokenResult, type NodeView } from '@/api/node'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { relativeTime } from '@/utils/format'
import { NODE_STATUS_LABEL, NODE_STATUS_TONE } from '@/utils/labels'

/** 运行态 → 语义色与中文名统一取自 utils/labels（列表与详情页共用一份）。 */

export function NodeListPage() {
  const queryClient = useQueryClient()
  const [enrollOpen, setEnrollOpen] = useState(false)
  const [removeTarget, setRemoveTarget] = useState<NodeView | null>(null)
  const [error, setError] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })

  const remove = useMutation({
    mutationFn: (id: number) => nodeApi.remove(id),
    onSuccess: () => {
      setRemoveTarget(null)
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['nodes'] })
    },
    onError: (err) => setError(describe(err)),
  })

  return (
    <div className="flex flex-col gap-4">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">节点管理</h1>
          <p className="mt-1 text-base text-ink-3">
            接入宿主机后，节点上的虚拟机、存储与网络资源才可被管理。
          </p>
        </div>
        <Button size="sm" onClick={() => setEnrollOpen(true)}>
          接入节点
        </Button>
      </header>

      {nodes.isPending && <PageLoading />}

      {nodes.isError && (
        <div className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(nodes.error)}
        </div>
      )}

      {nodes.data && nodes.data.length === 0 && (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState
            title="还没有接入任何节点"
            description="节点是虚拟机的运行载体。点击「接入节点」生成一次性令牌，再在目标宿主机上完成接入。"
            action={
              <Button size="sm" onClick={() => setEnrollOpen(true)}>
                接入第一个节点
              </Button>
            }
          />
        </div>
      )}

      {nodes.data && nodes.data.length > 0 && (
        <div className="overflow-x-auto rounded-card border border-line">
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="bg-sunken text-left text-xs text-ink-2">
                <th className="px-4 py-2.5 font-medium">名称</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">Agent 版本</th>
                <th className="px-4 py-2.5 font-medium">最后心跳</th>
                <th className="px-4 py-2.5 font-medium">能力</th>
                <th className="px-4 py-2.5 text-right font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {nodes.data.map((node) => (
                <tr key={node.id} className="border-t border-line hover:bg-raised">
                  <td className="px-4 py-2.5">
                    <Link
                      to={`/node/${node.id}`}
                      className="font-medium text-ink hover:text-brand hover:underline"
                    >
                      {node.name}
                    </Link>
                    {node.enroll_state === 'pending' && (
                      <span className="ml-2 text-xs text-warning">等待接入</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <StatusBadge
                      tone={NODE_STATUS_TONE[node.status]}
                      striped={node.maintenance_mode}
                    >
                      {node.maintenance_mode ? '维护中' : NODE_STATUS_LABEL[node.status]}
                    </StatusBadge>
                  </td>
                  <td className="kc-mono px-4 py-2.5 text-ink-2">{node.agent_version || '—'}</td>
                  <td className="px-4 py-2.5 text-ink-2">{relativeTime(node.last_heartbeat_at)}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {node.capabilities?.length ? `${node.capabilities.length} 项` : '—'}
                  </td>
                  <td className="px-4 py-2.5 text-right">
                    <Button variant="ghost" size="sm" onClick={() => setRemoveTarget(node)}>
                      移除
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <EnrollNodeModal open={enrollOpen} onClose={() => setEnrollOpen(false)} />

      <Modal
        open={removeTarget !== null}
        title="移除节点"
        description={
          removeTarget
            ? `将从面板中移除「${removeTarget.name}」。该节点上的虚拟机不会被停止，但面板将不再管理它们。`
            : undefined
        }
        onClose={() => {
          setRemoveTarget(null)
          setError('')
        }}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setRemoveTarget(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={remove.isPending}
              onClick={() => removeTarget && remove.mutate(removeTarget.id)}
            >
              确认移除
            </Button>
          </>
        }
      >
        {error && <p className="text-base text-danger">{error}</p>}
      </Modal>
    </div>
  )
}

function EnrollNodeModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const [result, setResult] = useState<EnrollTokenResult | null>(null)
  const [error, setError] = useState('')

  const create = useMutation({
    mutationFn: () => nodeApi.createEnrollToken(name),
    onSuccess: (data) => {
      setResult(data)
      setError('')
      // 节点记录已创建（状态为「等待接入」），列表需要刷新才能看到。
      void queryClient.invalidateQueries({ queryKey: ['nodes'] })
    },
    onError: (err) => setError(describe(err)),
  })

  function handleSubmit(event: FormEvent) {
    event.preventDefault()
    setError('')
    if (!name.trim()) {
      setError('请输入节点名')
      return
    }
    create.mutate()
  }

  function handleClose() {
    // 关闭时重置：令牌只在本次响应中出现一次，下次打开必须是全新的流程。
    setName('')
    setResult(null)
    setError('')
    onClose()
  }

  return (
    <Modal
      open={open}
      title={result ? '节点已创建，等待接入' : '接入节点'}
      description={
        result
          ? '以下命令只能使用一次，接入成功后令牌立即失效。'
          : '生成一次性令牌后，在目标宿主机上执行接入命令。'
      }
      size="lg"
      onClose={handleClose}
    >
      {!result && (
        <form onSubmit={handleSubmit} className="flex flex-col gap-4">
          <Input
            label="节点名"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="node-01"
            hint="1-64 位字母、数字、点、下划线或连字符"
            autoFocus
            disabled={create.isPending}
          />
          {error && <p className="text-base text-danger">{error}</p>}
          <div className="flex justify-end gap-2">
            <Button variant="secondary" size="sm" type="button" onClick={handleClose}>
              取消
            </Button>
            <Button size="sm" type="submit" loading={create.isPending}>
              生成令牌
            </Button>
          </div>
        </form>
      )}

      {result && (
        <div className="flex flex-col gap-4">
          <CopyField label="注册令牌" value={result.token} mono />

          {result.simulate_command && (
            <>
              <CopyField label="模拟接入命令（开发期）" value={result.simulate_command} mono wrap />
              <p className="text-sm text-ink-3">
                当前 agent 尚未实现，该命令会走与真实 agent 相同的注册逻辑，
                用于验证完整流程。接入真实 agent 后此处将变为安装命令。
              </p>
            </>
          )}

          <p className="text-sm text-ink-3">
            令牌有效期至 {new Date(result.expires_at).toLocaleString('zh-CN')}。
          </p>
        </div>
      )}
    </Modal>
  )
}

/** 带复制按钮的只读字段。 */
function CopyField({
  label,
  value,
  mono = false,
  wrap = false,
}: {
  label: string
  value: string
  mono?: boolean
  wrap?: boolean
}) {
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      // 剪贴板权限被拒时用户仍可手动选中复制，不必打断流程。
    }
  }

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center justify-between">
        <span className="text-sm font-medium text-ink-2">{label}</span>
        <Button variant="ghost" size="sm" onClick={copy}>
          {copied ? '已复制' : '复制'}
        </Button>
      </div>
      <code
        className={`rounded-control border border-line bg-sunken px-3 py-2 text-sm text-ink ${
          mono ? 'kc-mono' : ''
        } ${wrap ? 'break-all whitespace-pre-wrap' : 'overflow-x-auto whitespace-nowrap'}`}
      >
        {value}
      </code>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
