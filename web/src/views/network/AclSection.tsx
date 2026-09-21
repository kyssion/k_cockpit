/**
 * AclSection VPC 网络的访问控制规则（F-4-05）。
 *
 * 作用域是**网段**，与安全组（挂在虚拟机网口上）并列但不同："这个网段整体上
 * 不许访问某个地址"用 ACL 表达一次即可，逐台配安全组既重复又容易漏。
 *
 * 流程强制为**先预览、后应用**，且应用要带回预览的版本号：预览之后规则若
 * 被改动，按旧预览去应用等于用一个没人看过的结论去改真实网络。安全组
 * （F-4-04）用的是同一条约定。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import {
  ACL_ACTION_LABEL,
  ACL_DIRECTION_LABEL,
  aclApi,
  type ACLRuleInput,
  type ACLRuleView,
} from '@/api/vpcacl'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function AclSection({ nodeID, onError }: { nodeID: number; onError: (msg: string) => void }) {
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState<ACLRuleView | null | undefined>(undefined)
  const [confirmDelete, setConfirmDelete] = useState<ACLRuleView | null>(null)
  const [previewOpen, setPreviewOpen] = useState(false)

  const rules = useQuery({
    queryKey: ['vpc-acl', nodeID],
    queryFn: () => aclApi.list(nodeID),
    enabled: nodeID > 0,
  })

  const remove = useMutation({
    mutationFn: (id: number) => aclApi.remove(id),
    onSuccess: () => {
      setConfirmDelete(null)
      void queryClient.invalidateQueries({ queryKey: ['vpc-acl'] })
      void queryClient.invalidateQueries({ queryKey: ['vpc-acl-preview'] })
    },
    onError: (err) => onError(describe(err)),
  })

  const items = rules.data?.items ?? []

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-medium text-ink-2">访问控制（ACL）</h2>
        <div className="flex items-center gap-2">
          <Button size="sm" variant="secondary" onClick={() => setPreviewOpen(true)}>
            预览并应用
          </Button>
          <Button size="sm" onClick={() => setEditing(null)}>
            新增规则
          </Button>
        </div>
      </div>

      {items.length === 0 ? (
        <div className="rounded-card border border-line px-4 py-3 text-base text-ink-3">
          还没有规则。留空表示不限制——ACL 与安全组一样，只写「要放行什么」。
        </div>
      ) : (
        <table className="w-full border-collapse rounded-card border border-line text-base">
          <thead>
            <tr className="border-b border-line text-xs text-ink-3">
              <th className="px-4 py-2 text-left font-normal">优先级</th>
              <th className="px-4 py-2 text-left font-normal">动作</th>
              <th className="px-4 py-2 text-left font-normal">方向</th>
              <th className="px-4 py-2 text-left font-normal">来源</th>
              <th className="px-4 py-2 text-left font-normal">目标</th>
              <th className="px-4 py-2 text-left font-normal">端口</th>
              <th className="px-4 py-2 text-right font-normal">操作</th>
            </tr>
          </thead>
          <tbody>
            {items.map((r) => (
              <tr key={r.id} className="border-t border-line">
                <td className="kc-nums px-4 py-2.5 text-ink-2">{r.priority}</td>
                <td className="px-4 py-2.5">
                  <StatusBadge tone={r.action === 'allow' ? 'success' : 'danger'}>
                    {ACL_ACTION_LABEL[r.action] ?? r.action}
                  </StatusBadge>
                  {r.matches_all && r.action === 'deny' && (
                    // 这是"设了白名单却全不通"最常见的原因，必须在列表上就看到。
                    <span className="ml-1.5 text-xs text-warning">会遮住后面的规则</span>
                  )}
                </td>
                <td className="px-4 py-2.5 text-ink-2">
                  {ACL_DIRECTION_LABEL[r.direction] ?? r.direction}
                </td>
                <td className="kc-mono px-4 py-2.5 text-ink-2">{r.src_cidr || '任意'}</td>
                <td className="kc-mono px-4 py-2.5 text-ink-2">{r.dst_cidr || '任意'}</td>
                <td className="kc-nums px-4 py-2.5 text-ink-2">
                  {r.port_start != null ? `${r.port_start}-${r.port_end ?? r.port_start}` : '全部'}
                  <span className="ml-1.5 text-xs text-ink-3">{r.protocol}</span>
                </td>
                <td className="px-4 py-2.5 text-right">
                  <button className="text-sm text-brand hover:underline" onClick={() => setEditing(r)}>
                    编辑
                  </button>
                  <button
                    className="ml-3 text-sm text-danger hover:underline"
                    onClick={() => setConfirmDelete(r)}
                  >
                    删除
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <RuleModal
        nodeID={nodeID}
        rule={editing}
        onClose={() => setEditing(undefined)}
        onError={onError}
      />

      <Modal
        open={confirmDelete !== null}
        title="删除规则"
        onClose={() => setConfirmDelete(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setConfirmDelete(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={remove.isPending}
              onClick={() => confirmDelete && remove.mutate(confirmDelete.id)}
            >
              删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">删除后需要重新应用才会生效。</p>
      </Modal>

      <PreviewModal nodeID={nodeID} open={previewOpen} onClose={() => setPreviewOpen(false)} onError={onError} />
    </section>
  )
}

/** 规则的编辑弹窗。 */
function RuleModal({
  nodeID,
  rule,
  onClose,
  onError,
}: {
  nodeID: number
  rule: ACLRuleView | null | undefined
  onClose: () => void
  onError: (msg: string) => void
}) {
  const queryClient = useQueryClient()
  const [priority, setPriority] = useState('')
  const [action, setAction] = useState('allow')
  const [direction, setDirection] = useState('in')
  const [protocol, setProtocol] = useState('any')
  const [srcCIDR, setSrcCIDR] = useState('')
  const [dstCIDR, setDstCIDR] = useState('')
  const [ports, setPorts] = useState('')
  const [remark, setRemark] = useState('')
  const [seeded, setSeeded] = useState<string>('init')

  const key = rule === undefined ? 'closed' : rule === null ? 'new' : `r-${rule.id}`
  if (seeded !== key) {
    setSeeded(key)
    setPriority(rule ? String(rule.priority) : '100')
    setAction(rule?.action ?? 'allow')
    setDirection(rule?.direction ?? 'in')
    setProtocol(rule?.protocol ?? 'any')
    setSrcCIDR(rule?.src_cidr ?? '')
    setDstCIDR(rule?.dst_cidr ?? '')
    setPorts(rule?.port_start != null ? `${rule.port_start}-${rule.port_end ?? rule.port_start}` : '')
    setRemark(rule?.remark ?? '')
  }

  const save = useMutation({
    mutationFn: () => {
      // 端口列允许 "80" 与 "8000-8100" 两种写法：一次填两个框对"就一个端口"
      // 这种最常见的情况是纯粹的额外负担。
      let portStart: number | undefined
      let portEnd: number | undefined
      const raw = ports.trim()
      if (raw !== '') {
        const [a, b] = raw.split('-')
        portStart = Number(a)
        portEnd = Number(b ?? a)
      }
      const input: ACLRuleInput = {
        node_id: nodeID,
        priority: Number(priority) || 100,
        action,
        direction,
        protocol,
        src_cidr: srcCIDR.trim() || undefined,
        dst_cidr: dstCIDR.trim() || undefined,
        port_start: portStart,
        port_end: portEnd,
        enabled: true,
        remark: remark.trim() || undefined,
      }
      return rule ? aclApi.update(rule.id, input) : aclApi.create(input)
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['vpc-acl'] })
      void queryClient.invalidateQueries({ queryKey: ['vpc-acl-preview'] })
      onClose()
    },
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={rule !== undefined}
      title={rule ? '编辑规则' : '新增规则'}
      description="优先级小的先匹配。留空地址表示任意。"
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
        <div className="flex gap-2">
          <Input label="优先级" value={priority} onChange={(e) => setPriority(e.target.value)} />
          <div className="flex flex-col gap-1">
            <label className="text-sm font-medium text-ink-2">动作</label>
            <select
              value={action}
              onChange={(e) => setAction(e.target.value)}
              className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value="allow">允许</option>
              <option value="deny">拒绝</option>
            </select>
          </div>
          <div className="flex flex-col gap-1">
            <label className="text-sm font-medium text-ink-2">方向</label>
            <select
              value={direction}
              onChange={(e) => setDirection(e.target.value)}
              className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value="in">进入网段</option>
              <option value="out">离开网段</option>
            </select>
          </div>
        </div>

        <div className="flex gap-2">
          <Input label="来源 CIDR" value={srcCIDR} onChange={(e) => setSrcCIDR(e.target.value)} placeholder="留空=任意" />
          <Input label="目标 CIDR" value={dstCIDR} onChange={(e) => setDstCIDR(e.target.value)} placeholder="留空=任意" />
        </div>

        <div className="flex gap-2">
          <Input label="端口" value={ports} onChange={(e) => setPorts(e.target.value)} placeholder="80 或 8000-8100" />
          <div className="flex flex-col gap-1">
            <label className="text-sm font-medium text-ink-2">协议</label>
            <select
              value={protocol}
              onChange={(e) => setProtocol(e.target.value)}
              className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              <option value="any">任意</option>
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
              <option value="icmp">ICMP</option>
            </select>
          </div>
        </div>

        <Input label="备注" value={remark} onChange={(e) => setRemark(e.target.value)} />
        {save.isError && <p className="text-sm text-danger">{describe(save.error)}</p>}
      </div>
    </Modal>
  )
}

/** 预览并应用。 */
function PreviewModal({
  nodeID,
  open,
  onClose,
  onError,
}: {
  nodeID: number
  open: boolean
  onClose: () => void
  onError: (msg: string) => void
}) {
  const queryClient = useQueryClient()
  const [acknowledged, setAcknowledged] = useState(false)

  const preview = useQuery({
    queryKey: ['vpc-acl-preview', nodeID],
    queryFn: () => aclApi.preview(nodeID),
    enabled: open && nodeID > 0,
  })

  const apply = useMutation({
    mutationFn: () =>
      aclApi.apply({
        node_id: nodeID,
        version: preview.data?.version ?? '',
        acknowledge: acknowledged,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
      onClose()
    },
    onError: (err) => onError(describe(err)),
  })

  const data = preview.data

  return (
    <Modal
      open={open}
      title="预览并应用 ACL"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            关闭
          </Button>
          <Button
            size="sm"
            loading={apply.isPending}
            disabled={!!data?.unavailable || (data?.warnings ?? []).length > 0 && !acknowledged}
            onClick={() => apply.mutate()}
          >
            应用
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {data?.unavailable && <p className="text-base text-warning">{data.unavailable}</p>}

        {/* 渲染结果：规则集到生效之间隔着一层归并，不展示这一步，用户只能
            在网络不通之后才发现问题。 */}
        <div>
          <p className="text-sm text-ink-3">节点上将生成的条目</p>
          <pre className="kc-mono mt-1 overflow-x-auto rounded-control bg-sunken px-3 py-2 text-xs text-ink-2">
            {(data?.rendered ?? []).join('\n') || '（无）'}
          </pre>
        </div>

        {(data?.warnings ?? []).length > 0 && (
          <div className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2">
            <p className="text-sm font-medium text-warning">需要注意</p>
            <ul className="mt-0.5">
              {data!.warnings.map((w) => (
                <li key={w} className="text-base text-ink-2">
                  · {w}
                </li>
              ))}
            </ul>
            <label className="mt-2 flex cursor-pointer items-center gap-2 text-sm text-ink-2">
              <input
                type="checkbox"
                checked={acknowledged}
                onChange={(e) => setAcknowledged(e.target.checked)}
              />
              我已知悉，仍然应用
            </label>
          </div>
        )}

        {apply.isError && <p className="text-sm text-danger">{describe(apply.error)}</p>}
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  return error instanceof Error ? error.message : '操作失败'
}
