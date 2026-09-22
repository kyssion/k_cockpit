import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  CAPABILITY_STATE_LABEL,
  CAPABILITY_STATE_TONE,
  netMaintainApi,
  networkApi,
  SWITCH_MODE_LABEL,
  type Capability,
  type SwitchView,
} from '@/api/network'
import { nodeApi } from '@/api/node'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function NetworkPage() {
  const queryClient = useQueryClient()
  const [nodeID, setNodeID] = useState(0)
  const [formOpen, setFormOpen] = useState(false)
  const [editTarget, setEditTarget] = useState<SwitchView | null>(null)
  const [deleteTarget, setDeleteTarget] = useState<SwitchView | null>(null)
  // 迁移要选目标物理网卡与 VLAN，因此是一个弹窗而不是一个按钮直发。
  const [migrateTarget, setMigrateTarget] = useState<SwitchView | null>(null)
  const [migrateUplink, setMigrateUplink] = useState('')
  const [migrateVlan, setMigrateVlan] = useState('')
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list })

  const effectiveNodeID = nodeID || nodes.data?.[0]?.id || 0

  const remove = useMutation({
    mutationFn: (sw: SwitchView) => networkApi.deleteSwitch(sw.id),
    onSuccess: () => {
      setDeleteTarget(null)
      setError('')
      setNotice('已提交删除，正在下发到节点')
      // 与创建同理：记录由执行器在节点成功后处理，不能立刻刷新列表。
      setTimeout(() => {
        void queryClient.invalidateQueries({ queryKey: ['networks', effectiveNodeID] })
      }, 2000)
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => {
      setDeleteTarget(null)
      setNotice('')
      // 占用检查是同步的，因此这里能立刻看到「还有 N 块网卡接着」。
      setError(describe(err))
    },
  })

  const status = useQuery({
    queryKey: ['network-status', effectiveNodeID],
    queryFn: () => networkApi.status(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  const reconfigure = useMutation({
    mutationFn: (id: number) => netMaintainApi.reconfigureSwitch(id),
    onSuccess: () => {
      setError('')
      setNotice('已提交重配置')
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const migrate = useMutation({
    mutationFn: (vars: { id: number; uplink_if: string; vlan_id?: number }) =>
      netMaintainApi.migrateSwitch(vars.id, {
        uplink_if: vars.uplink_if,
        vlan_id: vars.vlan_id,
        acknowledge: true,
      }),
    onSuccess: () => {
      setMigrateTarget(null)
      setError('')
      setNotice('已提交迁移')
      void queryClient.invalidateQueries({ queryKey: ['networks', effectiveNodeID] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => {
      setMigrateTarget(null)
      setError(describe(err))
    },
  })

  const networks = useQuery({
    queryKey: ['networks', effectiveNodeID],
    queryFn: () => networkApi.networks(effectiveNodeID),
    enabled: effectiveNodeID > 0,
  })

  if (nodes.isPending) return <PageLoading />

  return (
    <div className="flex max-w-[880px] flex-col gap-5">
      <header className="flex items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">网络中心</h1>
          <p className="mt-1 text-base text-ink-3">
            节点的网络后端与能力状态。缺少依赖不会影响面板本身，只会让对应的网络功能不可用。
          </p>
        </div>

        <select
          value={effectiveNodeID}
          onChange={(e) => setNodeID(Number(e.target.value))}
          className="h-8 shrink-0 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
        >
          {(nodes.data ?? []).map((n) => (
            <option key={n.id} value={n.id}>
              {n.name}
            </option>
          ))}
        </select>
      </header>

      {effectiveNodeID === 0 && (
        <p className="rounded-card border border-dashed border-line-strong px-4 py-6 text-center text-base text-ink-3">
          还没有节点。网络能力建立在节点上，请先接入节点。
        </p>
      )}

      {status.isPending && effectiveNodeID > 0 && <PageLoading />}

      {status.isError && (
        <p className="rounded-card border border-danger/30 bg-danger/10 px-4 py-3 text-base text-danger">
          {describe(status.error)}
        </p>
      )}

      {status.data && (
        <>
          <section className="rounded-card border border-line">
            <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
              <h2 className="text-sm font-medium text-ink-2">后端模式</h2>
              {/* 探测失败时不能宣称「降级」——我们并不知道它缺什么，
                  断言降级会诱导用户去做无谓的修复。 */}
              {status.data.probe_failed ? (
                <StatusBadge tone="idle">无法确认</StatusBadge>
              ) : status.data.degraded ? (
                <StatusBadge tone="warning">功能受限</StatusBadge>
              ) : (
                <StatusBadge tone="success">正常</StatusBadge>
              )}
            </div>

            <div className="flex flex-col gap-2 px-4 py-3.5 text-base">
              <div className="flex gap-3">
                <span className="w-24 text-ink-3">当前模式</span>
                <span className="text-ink">{status.data.mode_label}</span>
              </div>
              {status.data.probe_failed && (
                <p className="rounded-control bg-warning/10 px-3 py-2 text-warning">
                  无法探测节点网络能力：{status.data.probe_message}
                  <span className="mt-1 block text-ink-2">
                    这不代表缺少依赖——只说明这次没能确认。节点恢复后可重新查看。
                  </span>
                </p>
              )}
            </div>
          </section>

          <section className="flex flex-col gap-2">
            <h2 className="text-sm font-medium text-ink-2">能力清单</h2>
            <div className="flex flex-col gap-2">
              {status.data.capabilities.map((cap) => (
                <CapabilityCard key={cap.key} capability={cap} />
              ))}
            </div>
          </section>
        </>
      )}

      {networks.data && (
        <section className="flex flex-col gap-2">
          <div className="flex items-baseline justify-between gap-3">
            <h2 className="text-sm font-medium text-ink-2">虚拟交换机</h2>
            <Button
              size="sm"
              variant="secondary"
              disabled={effectiveNodeID === 0}
              onClick={() => {
                setEditTarget(null)
                setFormOpen(true)
              }}
            >
              新建交换机
            </Button>
          </div>

          {notice && (
            <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">
              {notice}
            </p>
          )}
          {error && (
            <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
              {error}
            </p>
          )}

          <div className="overflow-x-auto rounded-card border border-line">
            <table className="w-full border-collapse text-base">
              <thead>
                <tr className="bg-sunken text-left text-xs text-ink-2">
                  <th className="px-4 py-2.5 font-medium">名称</th>
                  <th className="px-4 py-2.5 font-medium">网桥</th>
                  <th className="px-4 py-2.5 font-medium">模式</th>
                  <th className="px-4 py-2.5 font-medium">网段</th>
                  <th className="px-4 py-2.5 font-medium">操作</th>
                </tr>
              </thead>
              <tbody>
                {networks.data.map((nw) => (
                  <tr key={nw.id} className="border-t border-line">
                    <td className="px-4 py-2.5">
                      <span className="text-ink">{nw.name}</span>
                      {nw.is_system && <span className="ml-2 text-xs text-brand">系统</span>}
                      {nw.vlan_id != null && (
                        <span className="ml-2 text-xs text-ink-3">VLAN {nw.vlan_id}</span>
                      )}
                    </td>
                    <td className="kc-mono px-4 py-2.5 text-ink-2">{nw.bridge_name}</td>
                    <td className="px-4 py-2.5 text-ink-2">{modeLabel(nw.mode)}</td>
                    <td className="kc-mono px-4 py-2.5 text-ink-2">{nw.cidr || '—'}</td>
                    <td className="px-4 py-2.5">
                      {/* 系统基础网络不给操作按钮，并说明原因：它是未指定
                          网络时的默认落点，改网段或删掉会让该节点上所有虚拟
                          机立刻失去网络。给一个点了必然被拒的按钮没有意义。 */}
                      {nw.is_system ? (
                        <span className="text-xs text-ink-3">未指定网络时的默认落点</span>
                      ) : (
                        <span className="flex gap-2">
                          <button
                            className="text-sm text-brand hover:underline"
                            onClick={() => {
                              setEditTarget(nw)
                              setFormOpen(true)
                            }}
                          >
                            编辑
                          </button>
                          {/* 迁移：换物理网卡（可同时换 VLAN）。它搬的是**现有
                              端口**，与"只改配置"的编辑不是一回事。 */}
                          <button
                            className="text-sm text-ink-2 hover:underline"
                            onClick={() => setMigrateTarget(nw)}
                          >
                            迁移
                          </button>
                          {/* 重配置：按现有记录重新下发。对应"配置是对的、
                              节点上状态漂了"这一场景，因此不需要参数。 */}
                          <button
                            className="text-sm text-ink-2 hover:underline"
                            disabled={reconfigure.isPending}
                            onClick={() => reconfigure.mutate(nw.id)}
                          >
                            重配置
                          </button>
                          <button
                            className="text-sm text-danger hover:underline"
                            onClick={() => setDeleteTarget(nw)}
                          >
                            删除
                          </button>
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      <SwitchFormModal
        key={editTarget?.id ?? 'new'}
        open={formOpen}
        nodeID={effectiveNodeID}
        target={editTarget}
        onClose={() => setFormOpen(false)}
        onDone={(msg) => {
          setFormOpen(false)
          setError('')
          setNotice(msg)
          // 记录由执行器在节点成功后写入，因此要刷新列表；但**不能立刻**，
          // 那时任务还没跑完——2 秒足以覆盖 mock 的模拟耗时，真实环境里
          // 用户可以自己再刷新。
          setTimeout(() => {
            void queryClient.invalidateQueries({ queryKey: ['networks', effectiveNodeID] })
          }, 2000)
          void queryClient.invalidateQueries({ queryKey: ['tasks'] })
        }}
        onError={(msg) => {
          setFormOpen(false)
          setNotice('')
          setError(msg)
        }}
      />
      <Modal
        open={migrateTarget !== null}
        title={`迁移「${migrateTarget?.name ?? ''}」`}
        description="把这台交换机的端口搬到另一块物理网卡上。迁移期间该网络的网口会短暂中断。"
        onClose={() => setMigrateTarget(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setMigrateTarget(null)}>
              取消
            </Button>
            <Button
              size="sm"
              loading={migrate.isPending}
              disabled={migrateUplink.trim() === ''}
              onClick={() =>
                migrateTarget &&
                migrate.mutate({
                  id: migrateTarget.id,
                  uplink_if: migrateUplink.trim(),
                  vlan_id: migrateVlan.trim() === '' ? undefined : Number(migrateVlan),
                })
              }
            >
              提交迁移
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-3">
          <Input
            label="目标物理网卡"
            value={migrateUplink}
            onChange={(e) => setMigrateUplink(e.target.value)}
            placeholder="例如 eth1"
          />
          <Input
            label="目标 VLAN（可留空表示不变）"
            value={migrateVlan}
            onChange={(e) => setMigrateVlan(e.target.value)}
            placeholder="1-4094"
          />
          {migrate.isError && <p className="text-sm text-danger">{describe(migrate.error)}</p>}
        </div>
      </Modal>

      <Modal
        open={deleteTarget != null}
        title={`删除交换机「${deleteTarget?.name ?? ''}」`}
        description="删除会移除宿主机上的网桥。仍有网卡接在上面时会被拒绝，并告诉你还有几块。"
        onClose={() => setDeleteTarget(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeleteTarget(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={remove.isPending}
              onClick={() => deleteTarget && remove.mutate(deleteTarget)}
            >
              确认删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          删除后无法恢复。接在该网络上的虚拟机会失去网络连通，因此请先确认它们
          已经改接到其它网络。
        </p>
      </Modal>
    </div>
  )
}

/**
 * SwitchFormModal 是交换机的新建 / 编辑表单。
 *
 * 新建与编辑共用一个表单：两者的字段完全一致（后端也把「让网桥变成这样」
 * 当作同一件事），拆成两个组件只会让校验规则抄一份、然后慢慢分叉。
 *
 * `name` 作为 key 由调用方传入，切换目标时整个表单重新挂载——输入框与
 * 错误提示随之重置，不必在 effect 里同步 setState（那更容易把上一次的
 * 值带进这一次）。
 */
function SwitchFormModal({
  open,
  nodeID,
  target,
  onClose,
  onDone,
  onError,
}: {
  open: boolean
  nodeID: number
  target: SwitchView | null
  onClose: () => void
  onDone: (message: string) => void
  onError: (message: string) => void
}) {
  const editing = target != null
  const [name, setName] = useState(target?.name ?? '')
  const [mode, setMode] = useState(target?.mode ?? 'nat')
  const [vlan, setVlan] = useState(target?.vlan_id != null ? String(target.vlan_id) : '')
  const [cidr, setCidr] = useState(target?.cidr ?? '')
  const [gateway, setGateway] = useState(target?.gateway_ip ?? '')
  const [dhcpStart, setDhcpStart] = useState(target?.dhcp_start ?? '')
  const [dhcpEnd, setDhcpEnd] = useState(target?.dhcp_end ?? '')
  const [uplink, setUplink] = useState(target?.uplink_if ?? '')
  // 带宽上限（G-38）：0 / 空 = 不限。约束的是整个交换机的合计吞吐。
  const [bwIn, setBwIn] = useState(
    target?.bandwidth_in_mbps ? String(target.bandwidth_in_mbps) : '',
  )
  const [bwOut, setBwOut] = useState(
    target?.bandwidth_out_mbps ? String(target.bandwidth_out_mbps) : '',
  )

  const save = useMutation({
    mutationFn: () => {
      const input = {
        name: name.trim(),
        mode,
        vlan_id: vlan.trim() === '' ? undefined : Number(vlan),
        cidr: cidr.trim(),
        gateway_ip: gateway.trim(),
        dhcp_start: dhcpStart.trim(),
        dhcp_end: dhcpEnd.trim(),
        uplink_if: uplink.trim(),
        bandwidth_in_mbps: bwIn.trim() === '' ? 0 : Number(bwIn),
        bandwidth_out_mbps: bwOut.trim() === '' ? 0 : Number(bwOut),
      }
      return editing
        ? networkApi.updateSwitch(target.id, input)
        : networkApi.createSwitch(nodeID, input)
    },
    onSuccess: () =>
      onDone(editing ? '已提交修改，正在下发到节点' : '已提交创建，正在下发到节点'),
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title={editing ? `编辑「${target?.name}」` : '新建虚拟交换机'}
      description="变更会下发到节点创建或调整网桥，因此是异步的——提交后可在任务中心跟踪进度。"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={save.isPending} onClick={() => save.mutate()}>
            {editing ? '保存' : '创建'}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        <Input
          label="名称"
          value={name}
          maxLength={64}
          placeholder="例如：prod-net"
          onChange={(e) => setName(e.target.value)}
          hint="在节点内唯一。网桥名由系统按名称生成——Linux 的接口名上限是 15 个字符，用名称直接做网桥名会被内核静默截断。"
        />

        <div className="flex flex-col gap-1">
          <label className="text-sm text-ink-2">网络模式</label>
          <select
            value={mode}
            onChange={(e) => setMode(e.target.value)}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink focus:outline-none focus-visible:border-brand"
          >
            {Object.entries(SWITCH_MODE_LABEL).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </select>
        </div>

        <Input
          label="VLAN ID（可选）"
          value={vlan}
          placeholder="1-4094，留空表示不划分"
          onChange={(e) => setVlan(e.target.value.replace(/[^\d]/g, ''))}
          hint="在节点内唯一。0 与 4095 是保留值，配上去不会报错但行为未定义，因此不接受。"
        />

        <Input
          label="网段"
          value={cidr}
          placeholder="192.168.10.0/24"
          onChange={(e) => setCidr(e.target.value)}
          hint="必须填写。至少 /30——/31 与 /32 没有可用主机地址，网关与虚拟机都放不下。"
        />
        <Input
          label="网关地址（可选）"
          value={gateway}
          placeholder="192.168.10.1"
          onChange={(e) => setGateway(e.target.value)}
          hint="必须落在上面配置的网段内，否则 dnsmasq 照样会起来但不发地址。"
        />

        <div className="grid grid-cols-2 gap-3">
          <Input
            label="DHCP 起始"
            value={dhcpStart}
            placeholder="192.168.10.100"
            onChange={(e) => setDhcpStart(e.target.value)}
          />
          <Input
            label="DHCP 结束"
            value={dhcpEnd}
            placeholder="192.168.10.200"
            onChange={(e) => setDhcpEnd(e.target.value)}
          />
        </div>

        <Input
          label={mode === 'physical' ? '上行物理网卡' : '上行网卡（可选）'}
          value={uplink}
          placeholder="例如：eth0"
          onChange={(e) => setUplink(e.target.value)}
          hint={
            mode === 'physical'
              ? '桥接到这块物理网卡。选错会让该节点的管理地址短暂不可达，请确认口位。'
              : '出网走这块网卡；留空表示由系统选择。'
          }
        />

        {/* 带宽上限（G-38）：整个交换机的合计吞吐，与单网卡限速是两层。 */}
        <div className="grid grid-cols-2 gap-3">
          <Input
            label="入方向带宽上限（Mbps）"
            type="number"
            value={bwIn}
            placeholder="不限"
            onChange={(e) => setBwIn(e.target.value)}
          />
          <Input
            label="出方向带宽上限（Mbps）"
            type="number"
            value={bwOut}
            placeholder="不限"
            onChange={(e) => setBwOut(e.target.value)}
          />
        </div>
        <p className="text-xs text-ink-3">
          约束整个交换机的合计吞吐（0 表示不限），与单台虚拟机的网卡限速是两层限制。
        </p>
      </div>
    </Modal>
  )
}

/**
 * 能力卡片。
 *
 * 缺失时给三样东西：缺什么、影响什么、怎么修（f-4-01 R-011）。
 * 只说「不可用」会让用户只能靠猜——而能打开这个页面的人，本来就是要
 * 执行修复命令的人。
 */
function CapabilityCard({ capability }: { capability: Capability }) {
  const missing = capability.state === 'unavailable'

  return (
    <div className="rounded-card border border-line px-4 py-3">
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <span className="text-base font-medium text-ink">{capability.label}</span>
          {!capability.required && <span className="text-xs text-ink-3">可选</span>}
        </div>
        <StatusBadge tone={CAPABILITY_STATE_TONE[capability.state]}>
          {CAPABILITY_STATE_LABEL[capability.state]}
        </StatusBadge>
      </div>

      {missing && (
        <div className="mt-2 flex flex-col gap-2 text-base">
          <p className="text-ink-2">{capability.reason}</p>

          {capability.affected_features && capability.affected_features.length > 0 && (
            <p className="text-ink-3">
              受影响的功能：
              <span className="text-ink-2">{capability.affected_features.join('、')}</span>
            </p>
          )}

          {capability.fix && (
            <div className="flex flex-col gap-1">
              <span className="text-xs text-ink-3">修复方式</span>
              <code className="kc-mono select-all rounded-control bg-sunken px-3 py-2 text-sm text-ink">
                {capability.fix}
              </code>
            </div>
          )}
        </div>
      )}

    </div>
  )
}

function modeLabel(mode: string): string {
  switch (mode) {
    case 'nat':
      return 'NAT 出网'
    case 'physical':
      return '桥接物理网卡'
    case 'empty':
      return '隔离'
    default:
      return mode
  }
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}