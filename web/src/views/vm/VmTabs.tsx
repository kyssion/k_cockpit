/** 详情页的页签。取值与 FRONTEND.md §5.3.3 的表格一一对应。 */
export type TabKey =
  | 'system'
  | 'snapshot'
  | 'network'
  | 'disks'
  | 'monitor'
  | 'cdrom'
  | 'share'
  | 'event'
  | 'schedule'
  | 'export'
  | 'console'
  | 'xml'
  | 'edit'

const TABS: { key: TabKey; label: string }[] = [
  { key: 'system', label: '系统信息' },
  { key: 'snapshot', label: '快照管理' },
  { key: 'network', label: '网络管理' },
  { key: 'disks', label: '磁盘' },
  { key: 'monitor', label: '监控' },
  { key: 'cdrom', label: '光驱' },
  { key: 'share', label: '目录共享' },
  { key: 'schedule', label: '定时任务' },
  { key: 'export', label: '导出' },
  { key: 'console', label: '控制台' },
  { key: 'xml', label: '定义' },
  { key: 'edit', label: '编辑' },
]
/** TabBar 是详情页的页签条。 */
export function TabBar({ value, onChange }: { value: TabKey; onChange: (k: TabKey) => void }) {
  return (
    <div role="tablist" className="flex flex-wrap gap-1 border-b border-line">
      {TABS.map((t) => {
        const active = t.key === value
        return (
          <button
            key={t.key}
            role="tab"
            aria-selected={active}
            onClick={() => onChange(t.key)}
            className={[
              'rounded-t-control px-3.5 py-2 text-base transition-colors',
              active
                ? 'border-b-2 border-brand font-medium text-brand'
                : 'border-b-2 border-transparent text-ink-3 hover:text-ink',
            ].join(' ')}
          >
            {t.label}
          </button>
        )
      })}
    </div>
  )
}
