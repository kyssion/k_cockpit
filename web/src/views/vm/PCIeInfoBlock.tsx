import { useQuery } from '@tanstack/react-query'

import { vmApi } from '@/api/vm'
import { StatusBadge } from '@/components/common/StatusBadge'

/**
 * PCIe 根端口余量。
 *
 * 它回答的是"还能再热插几块设备"。热插一块盘或网卡需要一个空闲的 PCIe
 * 根端口，槽位用完的表现是"挂载命令成功、设备却没出现"——而那个报错里
 * 不会提到槽位，因此值不值得单独占一块地方：那是排查时唯一能想到原因的
 * 地方。
 *
 * 「不支持热插拔」与「槽位用完」必须分开显示：前者要换机型，后者只是扩容。
 */
export function PCIeInfoBlock({ vmID }: { vmID: number }) {
  const info = useQuery({
    queryKey: ['vm', vmID, 'pcie'],
    queryFn: () => vmApi.pcieInfo(vmID),
    // 按需加载（切到该页才取）：它是一次向节点的请求。
    staleTime: 30000,
  })

  if (!info.data) return null
  const d = info.data

  return (
    <div className="flex flex-col gap-1.5 rounded-card border border-line px-4 py-3">
      <div className="flex items-baseline justify-between">
        <h3 className="text-base font-medium text-ink-2">PCIe 热插槽</h3>
        <StatusBadge tone={d.hotplug_supported ? (d.free > 0 ? 'success' : 'warning') : 'idle'}>
          {d.hotplug_supported ? `可用 ${d.free}` : '不支持热插拔'}
        </StatusBadge>
      </div>
      {d.unavailable ? (
        <p className="text-sm text-ink-3">{d.unavailable}</p>
      ) : (
        <p className="kc-nums text-sm text-ink-3">
          共 {d.total} 个，已用 {d.used} 个，剩 {d.free} 个 · 机型 {d.machine_type}
          {d.reason && <span className="block">{d.reason}</span>}
        </p>
      )}
    </div>
  )
}
