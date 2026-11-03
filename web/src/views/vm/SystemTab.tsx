import { useState } from 'react'
import { Link } from 'react-router'

import type { TaskView } from '@/api/task'
import type { VmView } from '@/api/vm'
import { EmptyState } from '@/components/common/Feedback'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatDateTime, formatMemory, relativeTime } from '@/utils/format'
import {
  TASK_STATUS_LABEL,
  TASK_STATUS_PULSE,
  TASK_STATUS_TONE,
  taskTypeLabel,
} from '@/utils/labels'

import { ResizeDiskModal } from './ResizeDiskModal'

/** 汇总系统信息与最近任务。 */
export function SystemTab({
  vm,
  nodeName,
  tasks,
}: {
  vm: VmView
  nodeName?: string
  tasks: TaskView[]
}) {
  const [resizeOpen, setResizeOpen] = useState(false)
  return (
    <>
      <section className="rounded-card border border-line">
        <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">配置</h2>
        <dl className="grid grid-cols-2 gap-x-6 gap-y-3 px-4 py-3.5 text-base sm:grid-cols-3">
          <Field label="CPU">{vm.vcpu} 核</Field>
          <Field label="内存">{formatMemory(vm.memory_mb)}</Field>
          <Field label="磁盘">
            <span className="flex items-baseline gap-2">
              {vm.disk_gb} GB
              {/* **只能扩不能缩**：缩容会丢数据，因此那个方向根本不给入口
                  ——给一个点了会被拒的按钮，等于让用户去试一次不可逆的操作。 */}
              <button
                className="text-xs text-primary hover:underline"
                onClick={() => setResizeOpen(true)}
              >
                扩容
              </button>
            </span>
          </Field>
          <Field label="IP">{vm.ip_summary || '—'}</Field>
          <Field label="所属节点">{nodeName ?? `#${vm.node_id}`}</Field>
          <Field label="分组">{vm.group_name || '—'}</Field>
          <Field label="UUID">
            <span className="kc-mono text-sm">{vm.uuid || '—'}</span>
          </Field>
          <Field label="归属">{vm.owner_id ? `用户 #${vm.owner_id}` : '—'}</Field>
          <Field label="创建时间">{formatDateTime(vm.created_at)}</Field>
          <Field label="备注">{vm.remark || '—'}</Field>
          <Field label="最近对账">{formatDateTime(vm.last_synced_at)}</Field>
          <Field label="控制台">
            {vm.has_console ? '可用' : '无（display=none）'}
          </Field>
        </dl>
      </section>

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">最近任务</h2>
          <Link to="/task" className="text-sm text-brand hover:underline">
            全部任务 →
          </Link>
        </div>

        {tasks.length === 0 && (
          <EmptyState title="没有相关任务" description="对该虚拟机的操作会记录在这里。" />
        )}

        {tasks.length > 0 && (
          <table className="w-full border-collapse text-base">
            <tbody>
              {tasks.map((t) => (
                <tr key={t.id} className="border-t border-line first:border-t-0">
                  <td className="kc-mono px-4 py-2.5 text-ink-3">#{t.id}</td>
                  <td className="px-4 py-2.5 text-ink">{taskTypeLabel(t.type)}</td>
                  <td className="px-4 py-2.5">
                    <StatusBadge tone={TASK_STATUS_TONE[t.status]} pulse={TASK_STATUS_PULSE[t.status]}>
                      {TASK_STATUS_LABEL[t.status]}
                    </StatusBadge>
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{relativeTime(t.created_at)}</td>
                  <td className="px-4 py-2.5 text-xs text-danger">{t.error || ''}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {/* 弹窗挂在 Tab 内容的末尾，而不是嵌进上面那张卡的标题栏里——
          混在标题栏里读起来像是「最近任务」的一部分，而且一旦弹窗改成
          内联渲染（非 portal），它就会真的出现在那一行里。 */}
      <ResizeDiskModal
        open={resizeOpen}
        vm={vm}
        onClose={() => setResizeOpen(false)}
      />
    </>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-xs text-ink-3">{label}</dt>
      <dd className="text-ink">{children}</dd>
    </div>
  )
}
