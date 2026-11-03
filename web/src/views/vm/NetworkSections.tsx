import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { publicIPExtraApi, type GuestIPView } from '@/api/publicip'
import { vmApi } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
import { StatusBadge } from '@/components/common/StatusBadge'

const NEIGHBOR_STATE_LABEL: Record<string, string> = {
  reach: '可达',
  stale: '过期',
  delay: '探测中',
  probe: '探测中',
  failed: '不可达',
  permanent: '固定',
}

/**
 * NeighborSection 邻居表（ARP / NDP）。
 *
 * 它回答"这台机器现在能看见谁"。排查"虚拟机之间不通"时，先看邻居表能不能
 * 学到对端 MAC，比直接抓包快一个数量级：邻居表里没有对端说明问题在二层
 * （交换机、VLAN、端口隔离），有对端则要看三层（安全组、防火墙）。
 *
 * MAC 由控制面回填成机器名：让用户拿 MAC 去比对没人名有用。
 */
export function NeighborSection({ vmID }: { vmID: number }) {
  const list = useQuery({
    queryKey: ['vm', vmID, 'neighbors'],
    queryFn: () => vmApi.neighbors(vmID),
    staleTime: 15000,
  })
  const items = list.data?.items ?? []

  return (
    <section className="rounded-card border border-line">
      <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
        <h2 className="text-sm font-medium text-ink-2">邻居表（网络诊断）</h2>
        <Button size="sm" variant="secondary" onClick={() => void list.refetch()}>
          刷新
        </Button>
      </div>

      {list.data?.unavailable ? (
        <p className="px-4 py-3 text-base text-ink-3">{list.data.unavailable}</p>
      ) : items.length === 0 ? (
        <p className="px-4 py-3 text-base text-ink-3">还没有学到任何邻居。</p>
      ) : (
        <table className="w-full border-collapse text-base">
          <thead>
            <tr className="border-b border-line text-xs text-ink-3">
              <th className="px-4 py-2 text-left font-normal">地址</th>
              <th className="px-4 py-2 text-left font-normal">MAC</th>
              <th className="px-4 py-2 text-left font-normal">虚拟机</th>
              <th className="px-4 py-2 text-left font-normal">网口 / 网桥</th>
              <th className="px-4 py-2 text-left font-normal">状态</th>
            </tr>
          </thead>
          <tbody>
            {items.map((n, i) => (
              <tr key={`${n.ip}-${i}`} className="border-t border-line">
                <td className="kc-mono px-4 py-2.5 text-ink">{n.ip}</td>
                <td className="kc-mono px-4 py-2.5 text-ink-2">{n.mac || '—'}</td>
                <td className="px-4 py-2.5 text-ink-2">
                  {n.is_self ? <span className="text-ink-3">（本机）</span> : n.vm_name || '—'}
                </td>
                <td className="px-4 py-2.5 text-ink-2">
                  {[n.interface, n.bridge].filter(Boolean).join(' / ') || '—'}
                </td>
                <td className="px-4 py-2.5">
                  {/* failed 是最值得单独标出来的一种：它意味着"有记录但
                      解析不到 MAC、二层不通"。 */}
                  <StatusBadge tone={n.state === 'failed' ? 'danger' : n.state === 'reach' ? 'success' : 'idle'}>
                    {NEIGHBOR_STATE_LABEL[n.state ?? ''] ?? n.state ?? '—'}
                  </StatusBadge>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}


/**
 * GuestIPSection 来宾里实际配置的公网地址。
 *
 * 它回答的是"绑定成功之后为什么还是不通"：**绑定成功 ≠ 来宾里配好了**——
 * 控制面只保证规则下发，来宾里还要有人（或 cloud-init）把地址配上。没有这一
 * 项，这类问题只能靠进虚拟机自己看。
 *
 * 数据来自节点（`public_ip.guest_status`），因为"配没配、通不通"只有它知道。
 */
export function GuestIPSection({ vmID }: { vmID: number }) {
  const [open, setOpen] = useState(false)

  const list = useQuery({
    queryKey: ['guest-ips', vmID],
    queryFn: () => publicIPExtraApi.guestStatus(vmID),
    enabled: open,
  })

  const items = list.data?.items ?? []

  return (
    <section className="rounded-card border border-line">
      <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
        <div>
          <h2 className="text-sm font-medium text-ink-2">来宾地址状态</h2>
          <p className="mt-0.5 text-xs text-ink-3">
            绑定成功不等于来宾里配好了——这里看的是虚拟机里实际有没有这个地址。
          </p>
        </div>
        <Button size="sm" variant="secondary" onClick={() => setOpen((v) => !v)}>
          {open ? '收起' : '查看'}
        </Button>
      </div>

      {open && (
        <div className="px-4 py-3">
          {list.isPending ? (
            <PageLoading />
          ) : items.length === 0 ? (
            <p className="text-base text-ink-3">没有检测到公网地址。</p>
          ) : (
            <table className="w-full border-collapse text-base">
              <thead>
                <tr className="text-left text-xs text-ink-3">
                  <th className="px-2 py-1.5 font-normal">地址</th>
                  <th className="px-2 py-1.5 font-normal">已配置</th>
                  <th className="px-2 py-1.5 font-normal">可达</th>
                  <th className="px-2 py-1.5 font-normal">说明</th>
                </tr>
              </thead>
              <tbody>
                {items.map((it: GuestIPView) => (
                  <tr key={it.address} className="border-t border-line transition-colors hover:bg-sunken/70">
                    <td className="kc-mono px-2 py-1.5 text-ink">{it.address}</td>
                    <td className="px-2 py-1.5">
                      <StatusBadge tone={it.configured ? 'success' : 'warning'}>
                        {it.configured ? '是' : '否'}
                      </StatusBadge>
                    </td>
                    <td className="px-2 py-1.5">
                      <StatusBadge tone={it.reachable ? 'success' : 'idle'}>
                        {it.reachable ? '是' : '否'}
                      </StatusBadge>
                    </td>
                    {/* 未配置时 detail 是最有价值的一句：它说明卡在哪一步。 */}
                    <td className="px-2 py-1.5 text-ink-3">{it.detail || '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </section>
  )
}
