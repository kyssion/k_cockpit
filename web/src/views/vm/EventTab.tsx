import { useQuery } from '@tanstack/react-query'

import { vmApi } from '@/api/vm'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { StatusBadge } from '@/components/common/StatusBadge'
import { formatDateTime } from '@/utils/format'

/**
 * 这台机器发生过什么。
 *
 * 后端把**审计**与**任务**两条流水合成一条返回：用户问的是"发生了什么"，
 * 而不是"审计里有什么"。让他自己按时间去对两个列表，等于把答案藏起来。
 */
export function EventTab({ vmID }: { vmID: number }) {
  const list = useQuery({
    queryKey: ['vm', vmID, 'timeline'],
    queryFn: () => vmApi.timeline(vmID, 50),
  })
  const items = list.data?.items ?? []

  if (list.isPending) return <PageLoading />
  if (items.length === 0) {
    return <EmptyState title="还没有事件" description="创建、电源操作、配置变更等会记录在这里。" />
  }

  return (
    <ol className="flex flex-col">
      {items.map((it, i) => (
        <li key={`${it.at}-${i}`} className="flex gap-3 border-b border-line py-2.5 last:border-b-0">
          <span className="kc-nums w-40 shrink-0 text-base text-ink-3">{formatDateTime(it.at)}</span>
          <span className="min-w-0 flex-1">
            <span className="flex items-center gap-2">
              <StatusBadge
                tone={it.kind === 'task' ? (it.success ? 'success' : 'danger') : 'idle'}
              >
                {it.kind === 'task' ? '任务' : '操作'}
              </StatusBadge>
              <span className="text-base text-ink">{it.title}</span>
            </span>
            {it.detail && <span className="block text-sm text-ink-3">{it.detail}</span>}
          </span>
        </li>
      ))}
    </ol>
  )
}
