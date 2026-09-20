/**
 * TaskTray 是底部的常驻任务栏（F-7-02）。
 *
 * 为什么要有它：任务一旦被派发，用户最想知道的是"它还在跑吗"。此前只有
 * 任务中心一个入口，而用户在创建虚拟机、做快照之后多半留在原来的页面——
 * 要么切走去看，要么一直猜。常驻一条能让"有任务在跑"这件事始终可见。
 *
 * 三条刻意的取舍：
 *
 *  1. **没有进行中的任务时完全不渲染**。占着一条空白的任务栏，等于长期
 *     拿走一块屏幕，而它 99% 的时间里没有内容。
 *  2. **只显示进行中**，完成的由事件驱动消失：留在栏里的完成项会让人以为
 *     还有事没做完。
 *  3. **不在这里做取消**。取消是需要确认的动作，塞进一条窄栏里很容易误点，
 *     而误点的代价是中断一个正在搬运磁盘的任务。
 */
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import { isActive, taskApi, type TaskView } from '@/api/task'
import { TASK_TYPE_LABEL } from '@/utils/labels'

export function TaskTray({ connected }: { connected: boolean }) {
  const [collapsed, setCollapsed] = useState(false)

  const list = useQuery({
    queryKey: ['tasks', 'active'],
    queryFn: () => taskApi.list({ status: 'pending,running,unknown', page_size: 20 }),
  })

  const items = (list.data?.items ?? []).filter((t) => isActive(t.status))
  if (items.length === 0) return null

  return (
    <div className="shrink-0 border-t border-line bg-surface">
      <div className="flex items-center gap-3 px-6 py-2">
        <button
          type="button"
          className="text-sm text-ink-2 hover:text-ink"
          onClick={() => setCollapsed((v) => !v)}
          title={collapsed ? '展开' : '收起'}
        >
          {collapsed ? '展开' : '收起'}
        </button>

        <span className="text-sm text-ink-2">
          进行中任务
          <span className="kc-nums ml-1.5 text-ink">{items.length}</span>
        </span>

        {/* 连接状态要显示：断了之后界面会安静地停在旧状态上，而用户会以为
            任务卡住了——那是实时通道最容易被误解的地方。 */}
        {!connected && <span className="text-sm text-warning">实时连接已断开，正在重连…</span>}

        <Link to="/task" className="ml-auto text-sm text-brand hover:underline">
          任务中心
        </Link>
      </div>

      {!collapsed && (
        <ul className="flex flex-col">
          {items.slice(0, 4).map((t) => (
            <li key={t.id} className="flex items-center gap-3 border-t border-line px-6 py-1.5">
              <span className="w-40 truncate text-sm text-ink-2">
                {TASK_TYPE_LABEL[t.type] ?? t.type}
              </span>
              <span className="min-w-0 flex-1 truncate text-sm text-ink-3">
                {t.resource_name || '—'}
              </span>
              <span className="kc-nums w-12 text-right text-sm text-ink-2">{t.progress}%</span>
              <span className="text-sm text-ink-3">{stageLabel(t)}</span>
            </li>
          ))}
          {items.length > 4 && (
            <li className="border-t border-line px-6 py-1.5 text-sm text-ink-3">
              还有 {items.length - 4} 个…
            </li>
          )}
        </ul>
      )}
    </div>
  )
}

function stageLabel(t: TaskView): string {
  if (t.current_stage) return t.current_stage
  if (t.status === 'pending') return '等待调度'
  if (t.status === 'unknown') return '结果未知'
  return '执行中'
}
