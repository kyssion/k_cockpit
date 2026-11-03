import { useQuery } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { Link } from 'react-router'

import { editApi } from '@/api/edit'
import { vmApi, type PowerAction, type VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Meter } from '@/components/common/Meter'
import { describe } from '@/utils/describe'
import { formatMemory, relativeTime } from '@/utils/format'
import { POWER_ACTION_LABEL } from '@/utils/labels'

/**
 * VmHero 是详情页的 Hero 三卡（FRONTEND.md §5.3.3）。
 *
 * 抽成独立组件而不是写在主组件里：它有两个会轮询的查询，而主组件在详情
 * 加载完成前就 return 了——把钩子写在那之后是**条件调用**，React 不允许，
 * 写在之前又拿不到 vm（需要在拿到 vm 之后才能判断「是否在运行」）。
 * 挂载时机本身就是条件，这正是组件的用途。
 */
export function VmHero({ vm }: { vm: VmView }) {
  const running = vm.status === 'running'

  const stats = useQuery({
    queryKey: ['vm-stats', vm.id],
    queryFn: () => vmApi.stats(vm.id),
    // **只在运行时轮询**：停机的虚拟机没有指标可读，每 5 秒问一次
    // 只会得到一整屏「—」，还顺带把节点唤醒一遍。
    enabled: running,
    refetchInterval: running ? 5000 : false,
  })

  const form = useQuery({
    queryKey: ['vm-edit-form', vm.id],
    queryFn: () => editApi.form(vm.id),
    // 配置摘要几乎不变：页签切来切去不该让它反复请求。
    staleTime: 60_000,
  })

  // 控制台画面按固定间隔换帧。
  //
  // 20 秒是个折中：帧本身要等 hypervisor 出一帧（比指标慢得多），间隔太短
  // 会让请求堆在一起；太长则预览卡看起来像死图。用 state 计数而不是定时请求
  // 数据——画面交给 `<img>` 自己去取，浏览器能按需要复用连接。
  const [stamp, setStamp] = useState(() => Date.now())
  useEffect(() => {
    if (!vm.has_console) return
    const timer = setInterval(() => setStamp(Date.now()), 20_000)
    return () => clearInterval(timer)
  }, [vm.has_console])

  const values = form.data?.values ?? {}
  const config = (key: string) => {
    const v = values[key]
    return v == null || v === '' ? '—' : String(v)
  }

  return (
    <div className="grid gap-4 lg:grid-cols-3">
      {/* 卡一：状态与配置摘要 */}
      <HeroCard title="状态">
        <dl className="flex flex-col gap-2">
          <HeroRow label="运行时长">
            {running && stats.data ? formatUptime(stats.data.uptime_seconds) : '—'}
          </HeroRow>
          <HeroRow label="机器类型">{config('machine_type')}</HeroRow>
          <HeroRow label="固件">
            {config('firmware')}
            {values.secure_boot === true && <span className="text-ink-3"> · 安全启动</span>}
          </HeroRow>
          <HeroRow label="引导顺序">{config('boot_order')}</HeroRow>
          <HeroRow label="自动启动">
            {values.auto_start === true ? '开启' : values.auto_start === false ? '关闭' : '—'}
          </HeroRow>
        </dl>
      </HeroCard>

      {/* 卡二：资源用量。
          数据来自节点探测（agent.OpVMStats），不是控制面按配置推算——
          控制面看到的 vcpu / memory_mb 是**配置**而不是**用量**，把配置当
          用量显示，用户会看到一台空闲机器常年「内存占满」。 */}
      <HeroCard
        title="资源"
        action={
          running && stats.data ? (
            <span className="text-xs text-ink-3">{relativeTime(stats.data.at)}</span>
          ) : null
        }
      >
        {!running ? (
          // 停机时没有指标，但卡片不该因此空着：把创建时定下的规格摆出来，
          // 那是这张卡在停机状态下唯一诚实且有用的内容。
          <div className="flex flex-col gap-2">
            <p className="text-sm text-ink-3">虚拟机未运行，无实时指标。以下是设定的规格：</p>
            <dl className="flex flex-col gap-2">
              <HeroRow label="vCPU">{vm.vcpu} 核</HeroRow>
              <HeroRow label="内存">{formatMemory(vm.memory_mb)}</HeroRow>
              <HeroRow label="磁盘">{vm.disk_gb} GB</HeroRow>
            </dl>
          </div>
        ) : stats.isError ? (
          // 采集失败要说清楚是「没读到」而不是显示 0%——0% 看起来是
          // 「机器很闲」，而实际是「不知道」。
          <p className="text-sm text-warning">指标采集失败：{describe(stats.error)}</p>
        ) : (
          <div className="flex flex-col gap-3">
            <Meter
              label="CPU"
              detail={`${vm.vcpu} 核`}
              percent={stats.data?.cpu_percent ?? 0}
            />
            <Meter
              label="内存"
              detail={
                stats.data
                  ? `${formatMemory(stats.data.mem_used_mb)} / ${formatMemory(stats.data.mem_total_mb)}`
                  : '—'
              }
              percent={
                stats.data && stats.data.mem_total_mb > 0
                  ? (stats.data.mem_used_mb / stats.data.mem_total_mb) * 100
                  : 0
              }
            />
            <div className="grid grid-cols-2 gap-x-4 gap-y-1.5">
              <Rate label="网络 ↓" kbps={stats.data?.net_rx_kbps} />
              <Rate label="网络 ↑" kbps={stats.data?.net_tx_kbps} />
              <Rate label="磁盘读" kbps={stats.data?.disk_read_kbps} />
              <Rate label="磁盘写" kbps={stats.data?.disk_write_kbps} />
            </div>
          </div>
        )}
      </HeroCard>

      {/* 卡三：控制台预览。
          display=none 的虚拟机**不显示这张卡**：给一个必然黑屏的预览，
          比不给更糟——用户会以为虚拟机出问题了。 */}
      {vm.has_console && (
        <HeroCard
          title="控制台预览"
          action={
            <Link
              to={`/vm/${vm.id}/console`}
              className="text-xs text-brand hover:underline"
            >
              打开控制台 →
            </Link>
          }
        >
          <Link
            to={`/vm/${vm.id}/console`}
            className="block overflow-hidden rounded-control border border-line bg-console"
          >
            <img
              // key 跟着 stamp 变，强制浏览器重新拉图：URL 里已经带了
              // 时间戳，但 React 不会因为 src 变化就丢弃已解码的旧图，
              // 换 key 能确保加载态与错误态一起重置。
              key={stamp}
              src={vmApi.consoleFrameUrl(vm.id, stamp)}
              alt={`${vm.name} 的控制台预览`}
              className="aspect-video w-full object-cover"
              onError={(e) => {
                // 失败时隐藏图片而不是留一个破图图标：破图看起来像前端坏了，
                // 而实际原因在节点侧。
                e.currentTarget.style.display = 'none'
              }}
            />
          </Link>
          {!running && (
            <p className="mt-1.5 text-xs text-ink-3">虚拟机未运行，画面可能为空。</p>
          )}
        </HeroCard>
      )}
    </div>
  )
}

function HeroCard({
  title,
  action,
  children,
}: {
  title: string
  action?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between gap-2">
        <h2 className="text-sm text-ink-3">{title}</h2>
        {action}
      </div>
      <div className="mt-2.5">{children}</div>
    </section>
  )
}

function HeroRow({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3 text-base">
      <dt className="shrink-0 text-ink-3">{label}</dt>
      <dd className="truncate text-right text-ink">{children}</dd>
    </div>
  )
}

/** Rate 显示一项速率；无数据时显示「—」而不是 0。 */
function Rate({ label, kbps }: { label: string; kbps?: number }) {
  return (
    <div className="flex items-baseline justify-between gap-2 text-base">
      <span className="shrink-0 text-ink-3">{label}</span>
      <span className="kc-nums truncate text-right text-ink">{formatRate(kbps)}</span>
    </div>
  )
}

/** formatRate 把 Kbps 换算成可读的速率。 */
function formatRate(kbps?: number): string {
  if (kbps == null) return '—'
  if (kbps < 1000) return `${kbps.toFixed(0)} Kbps`
  return `${(kbps / 1000).toFixed(1)} Mbps`
}

/** formatUptime 把秒换算成「3 天 4 小时」。 */
function formatUptime(seconds: number): string {
  if (seconds <= 0) return '—'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days} 天 ${hours} 小时`
  if (hours > 0) return `${hours} 小时 ${minutes} 分`
  return `${minutes} 分`
}
export function PowerButton({
  action,
  loading,
  onTrigger,
}: {
  action: PowerAction
  loading: boolean
  onTrigger: (action: PowerAction) => void
}) {
  // 开机是恢复性操作，用主按钮；关机类用次级按钮；强制断电是危险动作，
  // 与其它操作在视觉上区分开。
  const variant = action === 'start' ? 'primary' : action === 'poweroff' ? 'danger' : 'secondary'
  return (
    <Button variant={variant} size="sm" loading={loading} onClick={() => onTrigger(action)}>
      {POWER_ACTION_LABEL[action]}
    </Button>
  )
}
