/**
 * 侧栏用的极简图标集。
 *
 * 为什么自己写而不是引一个图标库：这里只需要十几个 **16px 描边** 的图形，
 * 为一个菜单引入一整个图标包（动辄几百 KB、上千个导出）不划算。图形都是
 * 几何形状，用 `currentColor` 描边即可跟随文字颜色，不需要额外处理暗色主题。
 *
 * 每个图标都配 `title`：菜单收窄或文字被截断时它仍能被认出来，也是无障碍
 * 上的最低要求。
 */
export type IconName =
  | 'dashboard'
  | 'vm'
  | 'template'
  | 'trash'
  | 'task'
  | 'alert'
  | 'network'
  | 'ip'
  | 'firewall'
  | 'shield'
  | 'storage'
  | 'volume'
  | 'node'
  | 'user'
  | 'audit'
  | 'setting'
  | 'security'
  | 'docs'
  | 'info'
  | 'logs'
  | 'key'
  | 'chip'
  | 'mirror'
  | 'capture'
  | 'tuning'
  | 'diagnostics'
  | 'quota'

// 每个图形都用同一套坐标与描边参数，视觉上才像一套。
const PATHS: Record<IconName, string> = {
  dashboard: 'M2 2h5v5H2zM9 2h5v3H9zM9 7h5v7H9zM2 9h5v5H2z',
  vm: 'M3 3h10v8H3zM6 14h4M8 11v3',
  template: 'M8 2l6 3-6 3-6-3zM2 9l6 3 6-3M2 12l6 3 6-3',
  trash: 'M3 4h10M6 4V2h4v2M4 4l1 10h6l1-10',
  task: 'M3 3h10v10H3zM5.5 7.5l1.5 1.5 3-3',
  alert: 'M8 2l6 11H2zM8 6v3M8 11.5v.5',
  network: 'M8 2v3M3 10v3M13 10v3M8 5v5M3.5 10h9M8 10v0',
  ip: 'M8 2a6 6 0 100 12A6 6 0 008 2zM2 8h12M8 2c2 2 2 10 0 12M8 2C6 4 6 12 8 14',
  firewall: 'M2 4h12v3H2zM4 7v3M8 7v3M12 7v3M2 10h12v3H2z',
  shield: 'M8 2l5 2v5c0 3-2.5 4.5-5 5-2.5-.5-5-2-5-5V4z',
  storage: 'M3 4c0-1 2.2-2 5-2s5 1 5 2-2.2 2-5 2-5-1-5-2zM3 4v8c0 1 2.2 2 5 2s5-1 5-2V4',
  volume: 'M4 3h8v10H4zM6 6h4M6 9h4',
  node: 'M3 3h10v4H3zM3 9h10v4H3zM5.5 5h.5M5.5 11h.5',
  user: 'M8 3a2.5 2.5 0 100 5 2.5 2.5 0 000-5zM3 14c0-2.8 2.2-4 5-4s5 1.2 5 4',
  audit: 'M4 2h8v12H4zM6 5h4M6 8h4M6 11h2',
  setting: 'M8 2l1.5 2.5L12 5l-1 2.5L12 10l-2.5-.5L8 12l-1.5-2.5L4 10l1-2.5L4 5l2.5-.5z',
  security: 'M4 6V4h8v2M6 6v4a2 2 0 004 0V6M8 12v2',
  docs: 'M3 3h7l3 3v7H3zM6 8h4M6 11h4',
  info: 'M8 14A6 6 0 108 2a6 6 0 000 12zM8 7v4M8 5v.5',
  logs: 'M3 3h10v10H3zM5 6h6M5 9h6M5 12h3',
  key: 'M9 7a2 2 0 100 4 2 2 0 000-4zM9 9l-6 6M4 12l1 1M6 10l1 1',
  chip: 'M5 5h6v6H5zM3 7h2M3 9h2M11 7h2M11 9h2M7 3v2M9 3v2M7 11v2M9 11v2',
  mirror: 'M8 2v12M3 5l5 3 5-3M3 11l5-3 5 3',
  capture: 'M3 8a5 5 0 1010 0 5 5 0 00-10 0zM8 6v4M6 8h4',
  tuning: 'M2 5h6M11 5h3M2 11h3M8 11h6M11 3v4M5 9v4',
  diagnostics: 'M3 3h10v10H3zM5.5 8l1.5-2 1.5 3 1.5-2',
  quota: 'M2 12h12M4 12V8M8 12V5M12 12V9',
}

export function Icon({ name, className }: { name: IconName; className?: string }) {
  return (
    <svg
      viewBox="0 0 16 16"
      aria-hidden="true"
      className={className ?? 'h-4 w-4 shrink-0'}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.4"
      strokeLinecap="round"
      strokeLinejoin="round"
    >
      <path d={PATHS[name]} />
    </svg>
  )
}
