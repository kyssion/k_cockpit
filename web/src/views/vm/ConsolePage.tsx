import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useRef, useState, type FormEvent } from 'react'
import { Link, useParams } from 'react-router'

// noVNC 的 package.json 里 `exports` 直接指向 core/rfb.js，
// 因此从包名导入即可，不能再写子路径（写了会被 exports 字段拒绝）。
import RFB from '@novnc/novnc'

import { ApiError, NetworkError } from '@/api/client'
import { consoleApi, consoleSocketURL, type ConsoleConfig } from '@/api/console'
import { vmApi } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

/** 连接状态。`unsupported` 与 `failed` 必须分开——原因与处理方式都不同。 */
type ConnState = 'idle' | 'connecting' | 'connected' | 'failed' | 'unsupported'

/** 重连上限（R-009：需提示重试次数，且不得创建重复会话）。 */
const MAX_RETRIES = 3

export function ConsolePage() {
  const { id } = useParams<{ id: string }>()
  const vmID = Number(id)

  const [state, setState] = useState<ConnState>('idle')
  const [message, setMessage] = useState('')
  const [attempt, setAttempt] = useState(0)
  const [password, setPassword] = useState('')
  const [passwordAsked, setPasswordAsked] = useState(false)
  const [configOpen, setConfigOpen] = useState(false)
  // 要配置哪一种控制台。两种协议**共用同一套守卫**（二次验证、监听地址切换），
  // 因此这里只是一个"改哪几个字段"的选择，不是两条独立的路。
  const [protocol, setProtocol] = useState('vnc')

  const containerRef = useRef<HTMLDivElement>(null)
  const rfbRef = useRef<RFB | null>(null)

  const vm = useQuery({
    queryKey: ['vm', vmID],
    queryFn: () => vmApi.get(vmID),
    enabled: Number.isFinite(vmID),
  })

  const config = useQuery({
    queryKey: ['console', vmID, protocol],
    queryFn: () => consoleApi.get(vmID, protocol),
    enabled: Number.isFinite(vmID),
  })

  // 需要密码时先问用户：密码**只写不读**（R-005），服务端给不出明文，
  // 界面只能请用户自己输入。
  const needPassword = config.data?.has_password === true && !passwordAsked
  const canConnect =
    config.data?.available === true &&
    config.data.enabled &&
    config.data.stream_supported &&
    !needPassword

  useEffect(() => {
    if (!canConnect || !containerRef.current) return

    let rfb: RFB
    try {
      rfb = new RFB(containerRef.current, consoleSocketURL(vmID), {
        credentials: { password },
      })
    } catch (err) {
      // 构造失败是异常路径：RFB 只在参数非法时抛错（正常失败都走
      // disconnect 事件）。这里的 setState 无法改成派生值——它表达的
      // 是「外部系统初始化失败」，而那只有在这一刻才知道。
      // oxlint-disable-next-line react-hooks/set-state-in-effect
      setState('failed')
      setMessage(describe(err))
      return
    }

    rfb.scaleViewport = true
    rfb.background = '#0b0f14'
    rfbRef.current = rfb

    rfb.addEventListener('connect', () => {
      setState('connected')
      setMessage('')
    })

    rfb.addEventListener('disconnect', (event: Event) => {
      const detail = (event as CustomEvent<{ clean: boolean }>).detail
      setState('failed')
      // 主动断开（clean）不该被当作错误提示——用户自己点的关闭。
      setMessage(
        detail?.clean
          ? '控制台已断开'
          : '控制台连接中断。可能是节点不可达，或该虚拟机的控制台被关闭。',
      )
    })

    rfb.addEventListener('securityfailure', (event: Event) => {
      const detail = (event as CustomEvent<{ reason?: string }>).detail
      setState('failed')
      setMessage(detail?.reason ? `控制台认证失败：${detail.reason}` : '控制台认证失败')
    })

    return () => {
      // 释放旧连接（R-009：重连不得创建重复会话）。
      try {
        rfb.disconnect()
      } catch {
        // 已经断开时 disconnect 会抛错，忽略即可——这里的目标只是确保
        // 不再持有它。
      }
      rfbRef.current = null
    }
  }, [canConnect, password, vmID, attempt])

  function retry() {
    if (attempt >= MAX_RETRIES) return
    setAttempt((n) => n + 1)
  }

  if (vm.isPending || config.isPending) return <PageLoading />
  if (config.isError) {
    return (
      <div className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
        {describe(config.error)}
      </div>
    )
  }

  const cfg = config.data

  // 「连接中」是**派生**出来的：可连接但还没收到 connect 事件，就是连接中。
  // 把它写进 state 需要在 effect 里同步 setState，那会多一次渲染，
  // 而且「连接中」与「可连接」随时可能不同步。
  const displayState: ConnState = state === 'idle' && canConnect ? 'connecting' : state

  return (
    <div className="flex h-full flex-col gap-3">
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-3">
          <Link to={`/vm/${vmID}`} className="text-sm text-ink-3 hover:text-brand">
            ← 返回详情
          </Link>
          <span className="text-base font-medium text-ink">{vm.data?.name ?? `#${vmID}`}</span>
          <ConnBadge state={displayState} />
        </div>

        <Button variant="secondary" size="sm" onClick={() => setConfigOpen(true)}>
          控制台设置
        </Button>
      </div>

      {/* 不可用与不支持要**分开说**：前者是配置问题（去开启控制台），
          后者是能力问题（当前节点不支持），提示错了会让用户白折腾。 */}
      {!cfg.available && (
        <Notice tone="warning" title="该虚拟机没有控制台" text={cfg.unavailable_reason ?? ''} />
      )}

      {cfg.available && !cfg.enabled && (
        <Notice
          tone="idle"
          title="控制台尚未开启"
          text="在「控制台设置」中开启后即可查看画面。"
        />
      )}

      {cfg.available && cfg.enabled && !cfg.stream_supported && (
        <Notice
          tone="idle"
          title="当前 agent 不支持控制台通道"
          text="控制面已就绪，但该节点以模拟模式运行，控制台画面暂不可用。"
        />
      )}

      {needPassword && (
        <PasswordPrompt
          onSubmit={(value) => {
            setPassword(value)
            setPasswordAsked(true)
          }}
          onCancel={() => setPasswordAsked(true)}
        />
      )}

      {displayState === 'failed' && message && (
        <Notice
          tone="danger"
          title="连接失败"
          text={message}
          action={
            attempt < MAX_RETRIES ? (
              <Button variant="secondary" size="sm" onClick={retry}>
                重试（还剩 {MAX_RETRIES - attempt} 次）
              </Button>
            ) : (
              <span className="text-sm text-ink-3">
                已重试 {MAX_RETRIES} 次。请检查节点状态或控制台设置后再试。
              </span>
            )
          }
        />
      )}

      <div
        ref={containerRef}
        className="min-h-0 flex-1 overflow-hidden rounded-card border border-line bg-[#0b0f14]"
      />

      {displayState === 'connecting' && (
        <p className="text-center text-sm text-ink-3">正在连接控制台…</p>
      )}

      <ConsoleSettingsModal
        open={configOpen}
        vmID={vmID}
        config={cfg}
        protocol={protocol}
        onProtocolChange={setProtocol}
        onClose={() => setConfigOpen(false)}
      />
    </div>
  )
}

function ConnBadge({ state }: { state: ConnState }) {
  switch (state) {
    case 'connected':
      return <StatusBadge tone="success">已连接</StatusBadge>
    case 'connecting':
      return <StatusBadge tone="info">连接中</StatusBadge>
    case 'failed':
      return <StatusBadge tone="danger">已断开</StatusBadge>
    default:
      return null
  }
}

function Notice({
  tone,
  title,
  text,
  action,
}: {
  tone: 'warning' | 'danger' | 'idle'
  title: string
  text: string
  action?: React.ReactNode
}) {
  const styles: Record<string, string> = {
    warning: 'border-warning/40 bg-warning/10',
    danger: 'border-danger/30 bg-danger/10',
    idle: 'border-line bg-raised',
  }

  return (
    <div className={`flex items-start justify-between gap-4 rounded-card border px-4 py-3 ${styles[tone]}`}>
      <div className="flex flex-col gap-0.5">
        <span className="text-base font-medium text-ink">{title}</span>
        {text && <span className="text-base text-ink-2">{text}</span>}
      </div>
      {action}
    </div>
  )
}

function PasswordPrompt({
  onSubmit,
  onCancel,
}: {
  onSubmit: (value: string) => void
  onCancel: () => void
}) {
  const [value, setValue] = useState('')

  return (
    <Modal
      open
      title="输入控制台密码"
      description="该虚拟机的控制台已设置密码。密码不会被服务端回传，因此需要你手动输入。"
      onClose={onCancel}
    >
      <form
        onSubmit={(e: FormEvent) => {
          e.preventDefault()
          onSubmit(value)
        }}
        className="flex flex-col gap-4"
      >
        <Input
          label="密码"
          type="password"
          value={value}
          onChange={(e) => setValue(e.target.value)}
          autoFocus
          autoComplete="off"
        />
        <div className="flex justify-end gap-2">
          <Button variant="secondary" size="sm" type="button" onClick={onCancel}>
            跳过
          </Button>
          <Button size="sm" type="submit">
            连接
          </Button>
        </div>
      </form>
    </Modal>
  )
}

function ConsoleSettingsModal({
  open,
  vmID,
  config,
  protocol,
  onProtocolChange,
  onClose,
}: {
  open: boolean
  vmID: number
  config: ConsoleConfig
  protocol: string
  onProtocolChange: (p: string) => void
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')

  const update = useMutation({
    // **协议在这里统一注入**，而不是让每个调用点各自带上。
    // 逐个去改是漏改的来源，而漏改的表现是"我切到 SPICE 上点了开关，
    // 结果改的还是 VNC"——那不会报错，只会让人以为开关坏了。
    mutationFn: (input: Parameters<typeof consoleApi.update>[1]) =>
      consoleApi.update(vmID, { ...input, protocol }),
    onSuccess: (_cfg, input) => {
      setError('')
      setPassword('')
      setNotice(describeChange(input))
      void queryClient.invalidateQueries({ queryKey: ['console', vmID] })
    },
    onError: (err) => {
      setNotice('')
      setError(describe(err))
    },
  })

  function handleClose() {
    setPassword('')
    setError('')
    setNotice('')
    onClose()
  }

  return (
    <Modal open={open} title="控制台设置" onClose={handleClose}>
      <div className="flex flex-col gap-4">
        {/* 协议选择：两种协议**共用同一套守卫**，因此这里只是选"改哪几个
            字段"，不是两条独立的路。只列出**可用**的协议——SPICE 是 libvirt
            编译期可选项，列出一个点了打不开的选项会让用户去反复检查
            "是不是我哪里配错了"。 */}
        {(config.protocols?.length ?? 0) > 1 && (
          <div className="flex flex-col gap-1.5">
            <span className="text-base text-ink">协议</span>
            <div className="flex gap-2">
              {(config.protocols ?? []).map((p) => (
                <button
                  key={p}
                  onClick={() => onProtocolChange(p)}
                  className={`rounded-control px-3 py-1 text-sm ${
                    protocol === p
                      ? 'bg-primary/10 text-primary'
                      : 'text-ink-3 hover:bg-sunken hover:text-ink-2'
                  }`}
                >
                  {p.toUpperCase()}
                </button>
              ))}
            </div>
            {protocol === 'spice' && (
              <p className="rounded-control border border-line bg-sunken px-3 py-2 text-xs text-ink-3">
                SPICE 的价值在于外部客户端（声音、USB 重定向、多显示器）——
                这里的配置是给 virt-viewer 这类客户端用的。面板内置的查看器是
                VNC 的，SPICE 的画面请用外部客户端连接上面的地址与端口。
              </p>
            )}
          </div>
        )}

        <div className="flex items-center justify-between gap-4 rounded-control border border-line px-3 py-2.5">
          <div className="flex flex-col">
            <span className="text-base text-ink">开启控制台</span>
            {config.port ? (
              <span className="kc-mono text-xs text-ink-3">
                端口 {config.port}，监听 {config.bind}
              </span>
            ) : null}
          </div>
          <Button
            variant={config.enabled ? 'secondary' : 'primary'}
            size="sm"
            loading={update.isPending}
            onClick={() => update.mutate({ enabled: !config.enabled })}
          >
            {config.enabled ? '关闭' : '开启'}
          </Button>
        </div>

        <div className="flex flex-col gap-2 rounded-control border border-line px-3 py-2.5">
          <span className="text-base text-ink">
            控制台密码
            {config.has_password && <span className="ml-2 text-xs text-success">已设置</span>}
          </span>
          <span className="text-sm text-ink-3">
            {protocol === 'vnc'
              ? '密码最长 8 位（VNC 协议限制）。设置后无法查看，只能重新设置。'
              : '设置后无法查看，只能重新设置。'}
          </span>
          <div className="flex gap-2">
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="输入新密码"
              autoComplete="off"
              className="h-8 flex-1 rounded-control border border-line-strong bg-sunken px-2.5 text-base text-ink placeholder:text-ink-3 focus:outline-none focus-visible:border-brand"
            />
            <Button
              variant="secondary"
              size="sm"
              loading={update.isPending}
              disabled={password.length === 0}
              onClick={() => update.mutate({ password })}
            >
              设置
            </Button>
          </div>
        </div>

        {/* 对外暴露：会向网络开放宿主机端口，走二次验证（R-004）。 */}
        <div className="flex flex-col gap-2 rounded-control border border-warning/40 bg-warning/10 px-3 py-2.5">
          <div className="flex items-center justify-between gap-4">
            <span className="text-base text-ink">对外暴露</span>
            <StatusBadge tone={config.exposed ? 'warning' : 'idle'}>
              {config.exposed ? '已暴露' : '未暴露'}
            </StatusBadge>
          </div>
          <span className="text-sm text-ink-2">
            开启后宿主机端口将向网络开放，任何人只要能访问该端口就可以接入这台虚拟机，
            <span className="text-warning">绕过面板的权限控制</span>。需要完成二次验证。
          </span>
          <div className="flex justify-end">
            <Button
              variant={config.exposed ? 'secondary' : 'danger'}
              size="sm"
              loading={update.isPending}
              onClick={() => update.mutate({ exposed: !config.exposed })}
            >
              {config.exposed ? '关闭暴露' : '开启暴露'}
            </Button>
          </div>
        </div>

        {notice && <p className="rounded-control bg-success/10 px-3 py-2 text-sm text-success">{notice}</p>}
        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}

        <div className="flex justify-end">
          <Button variant="secondary" size="sm" onClick={handleClose}>
            关闭
          </Button>
        </div>
      </div>
    </Modal>
  )
}

function describeChange(input: Parameters<typeof consoleApi.update>[1]): string {
  if (input.exposed !== undefined) {
    return input.exposed
      ? '已开启对外暴露。宿主机端口现在向网络开放，建议在不需要时及时关闭。'
      : '已关闭对外暴露，端口收回 127.0.0.1。'
  }
  if (input.password !== undefined) return '密码已更新。'
  if (input.enabled !== undefined) return input.enabled ? '控制台已开启。' : '控制台已关闭。'
  return '设置已保存。'
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
