import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  NIC_MODEL_OPTIONS,
  type AddPortForwardInput,
  type BindStaticIPInput,
  type InterfaceInput,
} from '@/api/net'
import { platformCheckApi } from '@/api/platformcheck'
import { type NICModel, type VMInterface } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'

/** NicModal 新增或编辑网卡。 */
export function NicModal({
  open,
  nic,
  submitting,
  onClose,
  onSubmit,
}: {
  open: boolean
  vmID: number
  nic: VMInterface | null
  submitting: boolean
  onClose: () => void
  onSubmit: (input: InterfaceInput) => void
}) {
  // 初始值直接取自 props，切换目标时由调用方通过 `key` 触发重新挂载，
  // 状态自然重置。在渲染期间用 ref 记录「是否已初始化」是反模式：
  // ref 的读写在渲染中是不被允许的，而且它改变不了这一次渲染的结果。
  //
  // 编辑已有网卡时**不允许改序号与 MAC**：序号是来宾里的设备顺序（改了
  // 会让 eth0/eth1 对调），MAC 可能已被来宾按它配置过网络。
  const [model, setModel] = useState<NICModel>(nic?.model ?? 'virtio')
  const [rateLimit, setRateLimit] = useState(String(nic?.rate_limit_mbps ?? 0))
  const [allowed, setAllowed] = useState(nic?.allowed_addresses ?? '')
  const [error, setError] = useState('')

  const limit = Number(rateLimit)
  const ready = Number.isFinite(limit) && limit >= 0

  return (
    <Modal
      open={open}
      title={nic ? `编辑网卡 #${nic.order}` : '新增网卡'}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={!ready}
            loading={submitting}
            onClick={() => {
              if (!ready) {
                setError('限速需要是一个不小于 0 的数字')
                return
              }
              onSubmit({
                model,
                rate_limit_mbps: limit,
                allowed_addresses: allowed,
              })
            }}
          >
            保存
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <label className="flex flex-col gap-1">
          <span className="text-base text-ink">型号</span>
          <select
            className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            value={model}
            onChange={(e) => setModel(e.target.value as NICModel)}
          >
            {NIC_MODEL_OPTIONS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
          <span className="text-sm text-ink-3">
            装完系统发现没网，原因通常就是这里选了一个来宾没有驱动的型号。
          </span>
        </label>

        <Input
          label="限速上限（Mbps）"
          type="number"
          min={0}
          value={rateLimit}
          onChange={(e) => setRateLimit(e.target.value)}
          hint="0 表示不限速。"
        />

        <Input
          label="允许的源地址（可选）"
          value={allowed}
          placeholder="留空表示不限制"
          onChange={(e) => setAllowed(e.target.value)}
          hint="逗号分隔多个地址，用于防止 IP 欺骗。"
        />

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </Modal>
  )
}

/** BindIPModal 绑定静态地址。 */
export function BindIPModal({
  open,
  nics,
  submitting,
  onClose,
  onSubmit,
}: {
  open: boolean
  vmID: number
  nics: VMInterface[]
  submitting: boolean
  onClose: () => void
  onSubmit: (input: BindStaticIPInput) => void
}) {
  // 初始值取自当前网卡列表；每次打开由调用方通过 `key` 重新挂载。
  const [ip, setIP] = useState('')
  const [order, setOrder] = useState(nics[0] ? String(nics[0].order) : '')
  const [dhcp, setDHCP] = useState(true)

  return (
    <Modal
      open={open}
      title="绑定静态地址"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={ip.trim() === ''}
            loading={submitting}
            onClick={() =>
              onSubmit({
                ip: ip.trim(),
                interface_order: order === '' ? undefined : Number(order),
                is_dhcp_reservation: dhcp,
              })
            }
          >
            绑定
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <Input
          label="地址"
          value={ip}
          placeholder="例如 10.0.0.20"
          onChange={(e) => setIP(e.target.value)}
          hint="同一节点上不能有两台虚拟机使用同一个地址。"
        />

        <label className="flex flex-col gap-1">
          <span className="text-base text-ink">绑定到网卡</span>
          <select
            className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
            value={order}
            onChange={(e) => setOrder(e.target.value)}
          >
            <option value="">不指定</option>
            {nics.map((n) => (
              <option key={n.id} value={n.order}>
                #{n.order}
                {n.is_primary ? '（主网卡）' : ''}
              </option>
            ))}
          </select>
        </label>

        <label className="flex cursor-pointer items-start gap-2.5">
          <input
            type="checkbox"
            className="mt-0.5"
            checked={dhcp}
            onChange={(e) => setDHCP(e.target.checked)}
          />
          <span>
            <span className="block text-base text-ink">通过 DHCP 静态租约下发</span>
            <span className="block text-sm text-ink-3">
              关闭时只记录分配关系，需要你在来宾系统里手工配置地址。
            </span>
          </span>
        </label>
      </div>
    </Modal>
  )
}

/** PortForwardModal 新增端口转发。 */
export function PortForwardModal({
  open,
  usedPorts,
  submitting,
  onClose,
  onSubmit,
}: {
  open: boolean
  vmID: number
  usedPorts: string[]
  submitting: boolean
  onClose: () => void
  onSubmit: (input: AddPortForwardInput) => void
}) {
  // 每次打开由调用方通过 `key` 重新挂载，状态自然是初始值。
  const [protocol, setProtocol] = useState<'tcp' | 'udp'>('tcp')
  const [hostPort, setHostPort] = useState('')
  const [targetIP, setTargetIP] = useState('')
  const [targetPort, setTargetPort] = useState('')
  const [allowed, setAllowed] = useState('')
  const [error, setError] = useState('')

  const h = hostPort.trim() === '' ? 0 : Number(hostPort)
  const t = Number(targetPort)
  // 宿主机端口留空 = 自动分配（G-54）；目标端口必须由用户给出——
  // 转发指向哪里是用户的意图，没有可以替人决定的默认值。
  const hostOK = h === 0 || (Number.isInteger(h) && h >= 1 && h <= 65535)
  const ready =
    hostOK && Number.isInteger(t) && t >= 1 && t <= 65535

  return (
    <Modal
      open={open}
      title="新增端口转发"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            disabled={!ready}
            loading={submitting}
            onClick={() => {
              if (!ready) {
                setError('端口需要是 1 到 65535 之间的整数')
                return
              }
              onSubmit({
                protocol,
                host_port: h,
                target_ip: targetIP.trim() || undefined,
                target_port: t,
                allowed_ips: allowed.trim() || undefined,
              })
            }}
          >
            创建
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3.5">
        <div className="flex gap-3">
          <label className="flex flex-1 flex-col gap-1">
            <span className="text-base text-ink">协议</span>
            <select
              className="rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink"
              value={protocol}
              onChange={(e) => setProtocol(e.target.value as 'tcp' | 'udp')}
            >
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
            </select>
          </label>
          <Input
            label="宿主机端口（留空自动分配）"
            type="number"
            min={1}
            max={65535}
            value={hostPort}
            onChange={(e) => setHostPort(e.target.value)}
            hint="留空时从系统设置的范围里取未占用的端口；范围可在系统设置 → 网络中调整。"
          />
        </div>

        {usedPorts.length > 0 && (
          <p className="text-sm text-ink-3">
            这台虚拟机已占用：<span className="kc-mono">{usedPorts.join('、')}</span>。
            端口在节点内独占，与其它虚拟机冲突的会被拒绝。
          </p>
        )}

        <Input
          label="目标地址（可选）"
          value={targetIP}
          placeholder="留空则按虚拟机的实际地址"
          onChange={(e) => setTargetIP(e.target.value)}
        />

        <Input
          label="目标端口"
          type="number"
          min={1}
          max={65535}
          value={targetPort}
          onChange={(e) => setTargetPort(e.target.value)}
        />

        <div className="flex flex-col gap-1">
          <span className="text-base text-ink">允许的来源（可选）</span>
          <div className="flex gap-2">
            <input
              value={allowed}
              placeholder="留空表示不限制"
              onChange={(e) => setAllowed(e.target.value)}
              className="h-9 flex-1 rounded-control border border-line-strong bg-sunken px-2.5 text-base text-ink placeholder:text-ink-3 focus:outline-none focus-visible:border-brand"
            />
            <FillMyIP onFill={(ip) => setAllowed((cur) => (cur.trim() === '' ? ip : cur + ',' + ip))} />
          </div>
          <span className="text-sm text-ink-3">
            留空意味着任何地址都能访问这个端口，请确认这是预期的。多个来源用逗号分隔。
          </span>
        </div>

        {error && (
          <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}
      </div>
    </Modal>
  )
}

/**
 * FillMyIP 把「面板看到的我的地址」填进来源白名单。
 *
 * 为什么不直接读 X-Forwarded-For 或让用户自己查：白名单比对的口径是
 * **面板看到的地址**，自己查到的出口地址很可能不一样（差一层 NAT 就是
 * 两个世界）。接口返回的 note 里若提示了反向代理，会一并展示——那种
 * 情况下两个地址都该让用户看见再选。
 */
function FillMyIP({ onFill }: { onFill: (ip: string) => void }) {
  const [note, setNote] = useState('')
  const [error, setError] = useState('')

  const fetchIP = useMutation({
    mutationFn: () => platformCheckApi.clientIP(),
    onSuccess: (data) => {
      setError('')
      onFill(data.client_ip)
      setNote(data.note ?? '')
    },
    onError: (err) => {
      setNote('')
      setError(err instanceof ApiError || err instanceof NetworkError ? err.message : '获取地址失败')
    },
  })

  return (
    <div className="flex flex-col gap-1">
      <Button
        variant="secondary"
        size="sm"
        loading={fetchIP.isPending}
        onClick={() => fetchIP.mutate()}
      >
        填入我的 IP
      </Button>
      {note && <span className="max-w-72 text-xs text-warning">{note}</span>}
      {error && <span className="max-w-72 text-xs text-danger">{error}</span>}
    </div>
  )
}
