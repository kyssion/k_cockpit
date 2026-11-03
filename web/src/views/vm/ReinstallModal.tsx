import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { templateApi } from '@/api/template'
import { Button } from '@/components/common/Button'
import { Modal } from '@/components/common/Modal'

/**
 * ReinstallModal 选择模板并确认重装（F-2-11）。
 *
 * 确认框里把「会丢什么、会留什么」写清楚：整块系统盘被替换，而硬件配置、
 * 数据盘与主网口绑定都保留。用户点下这个按钮之前必须知道边界在哪——
 * 「重装会不会把我挂的数据盘也格了」是最先要回答的问题。
 */
export function ReinstallModal({
  open,
  nodeID,
  vmName,
  pending,
  onClose,
  onConfirm,
}: {
  open: boolean
  nodeID: number
  vmName: string
  pending: boolean
  onClose: () => void
  onConfirm: (templateID: number) => void
}) {
  const [templateID, setTemplateID] = useState(0)

  // 只列**同一节点**上可用的模板：模板盘就在它所属节点的存储池里，
  // 跨节点使用需要先导出再导入。列出来再被拒绝只会让人以为是自己操作错了。
  const templates = useQuery({
    queryKey: ['templates', { node_id: nodeID, only_ready: true }],
    queryFn: () => templateApi.list({ node_id: nodeID, only_ready: true }),
    enabled: open && nodeID > 0,
  })

  const candidates = templates.data ?? []

  return (
    <Modal
      open={open}
      title={`重装「${vmName}」的系统`}
      description="用选定的模板重建系统盘。原系统盘会先被备份保留，确认不再需要后可在详情页清理。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            variant="danger"
            size="sm"
            disabled={templateID === 0}
            loading={pending}
            onClick={() => onConfirm(templateID)}
          >
            确认重装
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2.5">
          <p className="text-base font-medium text-danger">
            整块系统盘会被替换
          </p>
          <p className="mt-1 text-base text-ink-2">
            原系统上的软件、配置与没放在数据盘上的数据都会消失。
            这一步会要求二次验证。
          </p>
        </div>

        <div className="rounded-control border border-line px-3 py-2.5">
          <p className="text-base text-ink">保留的内容</p>
          <ul className="mt-1 flex flex-col gap-0.5 text-sm text-ink-3">
            <li>硬件配置（CPU、内存、机型、固件）</li>
            <li>数据盘及其上的数据</li>
            <li>主网口的绑定关系</li>
          </ul>
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">用于重建的模板</label>
          <select
            value={templateID}
            onChange={(e) => setTemplateID(Number(e.target.value))}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
          >
            <option value={0}>请选择…</option>
            {candidates.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name} · {t.default_cpu} 核 {t.default_memory_mb} MB
              </option>
            ))}
          </select>
          {candidates.length === 0 && !templates.isPending && (
            <p className="text-xs text-ink-3">
              该节点上没有可用于重装的模板。请先从一个已关机的虚拟机创建模板。
            </p>
          )}
        </div>
      </div>
    </Modal>
  )
}
