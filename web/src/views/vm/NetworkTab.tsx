import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { netApi, type PortForward } from '@/api/net'
import { NIC_MODEL_LABEL, vmApi, type StaticIP, type TaskRef, type VMInterface } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { ErrorBox } from '@/components/common/ErrorBox'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { describe } from '@/utils/describe'
import { formatDateTime } from '@/utils/format'

import { BindIPModal, NicModal, PortForwardModal } from './NetworkModals'
import { GuestIPSection, NeighborSection } from './NetworkSections'

/** NetworkTab 管理虚拟机的网络（F-2-03「网络管理」）。 */
export function NetworkTab({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [editNic, setEditNic] = useState<VMInterface | null>(null)
  const [deleteNic, setDeleteNic] = useState<VMInterface | null>(null)
  const [addingNic, setAddingNic] = useState(false)
  const [bindingIP, setBindingIP] = useState(false)
  const [deleteIP, setDeleteIP] = useState<StaticIP | null>(null)
  const [addingPF, setAddingPF] = useState(false)
  const [deletePF, setDeletePF] = useState<PortForward | null>(null)
  // 端口转发的多选。与公网地址那页同一套做法——一张列表上逐条删十条
  // 是纯粹的体力活，而它恰恰是「换个端口就重配一遍」时最常做的事。
  const [pfSelected, setPFSelected] = useState<Set<number>>(new Set())
  const [pfFailed, setPFFailed] = useState<
    { pf_id: number; reason: string }[] | null
  >(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  // **批量删除的 mutation 必须放在提前 return 之前。**
  // 放到「取到数据之后」看起来很自然（它确实只在那时才有意义），但那样
  // 就违反了 Hook 的调用规则——组件在 loading 与 ready 两次渲染中走了不同
  // 数量的 Hook，React 会直接抛错。lint 抓住了这一处；它不会在编译期暴露，
  // 只有运行时才炸。
  const pfBatchRemove = useMutation({
    mutationFn: (ids: number[]) =>
      netApi.batchRemovePortForwards(ids.map((id) => ({ vm_id: vmID, pf_id: id }))),
    onSuccess: (r) => {
      setError('')
      // 服务的失败项是 `{ref, reason}`，这里摊平成 pf_id —— 界面只需要
      // 用它来对上号，vm_id 在当前页面里是同一个值。
      setPFFailed(
        (r.failed ?? []).map((f) => ({ pf_id: f.ref.pf_id, reason: f.reason })),
      )
      setPFSelected(new Set())
      void queryClient.invalidateQueries({ queryKey: ['port-forwards'] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const interfaces = useQuery({
    queryKey: ['vm-interfaces', vmID],
    queryFn: () => vmApi.interfaces(vmID),
  })
  const staticIPs = useQuery({
    queryKey: ['vm-static-ips', vmID],
    queryFn: () => vmApi.staticIPs(vmID),
  })
  const forwards = useQuery({
    queryKey: ['vm-port-forwards', vmID],
    queryFn: () => netApi.listPortForwards(vmID),
  })

  function refresh() {
    void queryClient.invalidateQueries({ queryKey: ['vm-interfaces', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['vm-static-ips', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['vm-port-forwards', vmID] })
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })
  }

  // 所有写操作都是「提交任务」这一个形状，用一个 hook 收口：
  // 各自写一遍 onSuccess / onError 只会得到五份几乎相同的代码。
  const submit = useMutation({
    mutationFn: (fn: () => Promise<TaskRef>) => fn(),
    onSuccess: (r, _fn, ctx) => {
      void ctx
      setNotice(`已提交，任务 #${r.task_id} 正在执行`)
      setError('')
      closeAll()
      refresh()
    },
    onError: (err) => {
      setError(describe(err))
      closeAll()
    },
  })

  function closeAll() {
    setAddingNic(false)
    setEditNic(null)
    setDeleteNic(null)
    setBindingIP(false)
    setDeleteIP(null)
    setAddingPF(false)
    setDeletePF(null)
  }

  if (interfaces.isPending || staticIPs.isPending || forwards.isPending) return <PageLoading />
  if (interfaces.isError) return <ErrorBox message={describe(interfaces.error)} />
  if (staticIPs.isError) return <ErrorBox message={describe(staticIPs.error)} />
  if (forwards.isError) return <ErrorBox message={describe(forwards.error)} />

  const nics = interfaces.data.items
  const ips = staticIPs.data.items
  const pfs = forwards.data.items

  return (
    <div className="flex flex-col gap-4">
      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && <ErrorBox message={error} />}

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">网卡</h2>
          <Button size="sm" onClick={() => setAddingNic(true)}>
            新增网卡
          </Button>
        </div>

        {nics.length === 0 ? (
          <EmptyState title="没有网卡" description="该虚拟机尚未配置网络接口。" />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">序号</th>
                <th className="px-4 py-2 text-left font-normal">型号</th>
                <th className="px-4 py-2 text-left font-normal">MAC</th>
                <th className="px-4 py-2 text-left font-normal">接入网络</th>
                <th className="px-4 py-2 text-left font-normal">限速</th>
                <th className="px-4 py-2 text-left font-normal">下发状态</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {nics.map((n) => (
                <tr key={n.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="px-4 py-2.5 text-ink">
                    {n.order}
                    {n.is_primary && <span className="ml-1.5 text-xs text-ink-3">主网卡</span>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">{NIC_MODEL_LABEL[n.model] ?? n.model}</td>
                  <td className="kc-mono px-4 py-2.5 text-sm text-ink-2">{n.mac || '—'}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {n.switch_name ?? <span className="text-ink-3">节点默认网络</span>}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {n.rate_limit_mbps > 0 ? `${n.rate_limit_mbps} Mbps` : '不限速'}
                  </td>
                  <td className="px-4 py-2.5">
                    <AppliedBadge applied={n.applied} at={n.last_applied_at} />
                  </td>
                  <td className="px-4 py-2.5 text-right whitespace-nowrap">
                    <button
                      className="text-sm text-brand hover:underline"
                      onClick={() => setEditNic(n)}
                    >
                      编辑
                    </button>
                    <button
                      className="ml-3 text-sm text-danger hover:underline disabled:text-ink-3 disabled:no-underline"
                      disabled={n.is_primary}
                      // 主网卡不可删：重装系统依赖它保持网络可达。
                      // 禁用而不是隐藏，附上原因让用户知道为什么。
                      title={n.is_primary ? '主网卡不能删除：重装系统等操作依赖它保持网络可达' : ''}
                      onClick={() => setDeleteNic(n)}
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="rounded-card border border-line">
        <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">静态地址</h2>
          <Button size="sm" onClick={() => setBindingIP(true)}>
            绑定地址
          </Button>
        </div>

        {ips.length === 0 ? (
          <EmptyState
            title="没有分配静态地址"
            description="虚拟机的 IP 由 DHCP 分配时，这里为空；实际拿到的地址显示在「系统信息」的 IP 一栏。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="px-4 py-2 text-left font-normal">地址</th>
                <th className="px-4 py-2 text-left font-normal">网卡</th>
                <th className="px-4 py-2 text-left font-normal">MAC</th>
                <th className="px-4 py-2 text-left font-normal">来源</th>
                <th className="px-4 py-2 text-left font-normal">下发状态</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {ips.map((s) => (
                <tr key={s.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="kc-mono px-4 py-2.5 text-ink">{s.ip}</td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {s.interface_order !== undefined ? `#${s.interface_order}` : '—'}
                  </td>
                  <td className="kc-mono px-4 py-2.5 text-sm text-ink-2">{s.mac || '—'}</td>
                  {/* 区分来源：排查时一个查 DHCP 服务，另一个要进系统看配置文件。 */}
                  <td className="px-4 py-2.5 text-ink-2">
                    {s.is_dhcp_reservation ? 'DHCP 静态租约' : '手工配置'}
                  </td>
                  <td className="px-4 py-2.5">
                    <AppliedBadge applied={s.applied} at={s.applied_at} />
                  </td>
                  <td className="px-4 py-2.5 text-right">
                    <button
                      className="text-sm text-danger hover:underline"
                      onClick={() => setDeleteIP(s)}
                    >
                      解绑
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="rounded-card border border-line">
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-line px-4 py-2.5">
          <h2 className="text-sm font-medium text-ink-2">端口转发</h2>
          <span className="flex items-center gap-2">
            {/* 选中之后才出现：常驻会让这一行平时多两个用不上的按钮。 */}
            {pfSelected.size > 0 && (
              <>
                <span className="text-xs text-ink-3">已选 {pfSelected.size} 条</span>
                <Button
                  size="sm"
                  variant="secondary"
                  loading={pfBatchRemove.isPending}
                  onClick={() => pfBatchRemove.mutate(Array.from(pfSelected))}
                >
                  批量删除
                </Button>
                <button
                  className="text-xs text-ink-3 hover:underline"
                  onClick={() => setPFSelected(new Set())}
                >
                  取消选择
                </button>
              </>
            )}
            <Button size="sm" onClick={() => setAddingPF(true)}>
              新增转发
            </Button>
          </span>
        </div>

        {/* 批量删除可能**部分成功**，因此逐条列出失败原因，而不是一句
            「批量删除失败」——那样用户不知道该处理哪几条。 */}
        {/* 联动提示（G-51）：转发目标依赖虚拟机当前地址，而静态绑定与转发是
            两条独立链路。有转发却没绑静态地址时提醒，而不是替用户自动绑——
            "绑定哪个地址"在多网卡时不是一个可以替人决定的问题。 */}
        {pfs.length > 0 && ips.length === 0 && (
          <p className="border-b border-line bg-warning/5 px-4 py-2 text-sm text-warning">
            该虚拟机还没有绑定静态地址：转发目标依赖当前 IP，DHCP 续租后地址一变转发就会失效。
            建议在上方「静态地址」里绑定当前地址。
          </p>
        )}

        {(pfFailed?.length ?? 0) > 0 && (
          <div className="border-b border-line bg-danger/5 px-4 py-2">
            <p className="text-xs text-danger">以下条目删除失败：</p>
            <ul className="mt-0.5">
              {pfFailed?.map((f) => (
                <li key={f.pf_id} className="text-xs text-danger">
                  #{f.pf_id}：{f.reason}
                </li>
              ))}
            </ul>
          </div>
        )}

        {pfs.length === 0 ? (
          <EmptyState
            title="没有端口转发"
            description="端口转发把宿主机上的一个端口指向虚拟机的服务，让外部可以访问。"
          />
        ) : (
          <table className="w-full border-collapse text-base">
            <thead>
              <tr className="border-b border-line text-xs text-ink-3">
                <th className="w-10 px-4 py-2">
                  <input
                    type="checkbox"
                    checked={pfSelected.size === pfs.length && pfs.length > 0}
                    onChange={(e) =>
                      setPFSelected(
                        e.target.checked ? new Set(pfs.map((x) => x.id)) : new Set(),
                      )
                    }
                  />
                </th>
                <th className="px-4 py-2 text-left font-normal">宿主机端口</th>
                <th className="px-4 py-2 text-left font-normal">目标</th>
                <th className="px-4 py-2 text-left font-normal">允许来源</th>
                <th className="px-4 py-2 text-left font-normal">下发状态</th>
                <th className="px-4 py-2 text-right font-normal">操作</th>
              </tr>
            </thead>
            <tbody>
              {pfs.map((p) => (
                <tr key={p.id} className="border-t border-line transition-colors hover:bg-sunken/70">
                  <td className="px-4 py-2.5">
                    <input
                      type="checkbox"
                      checked={pfSelected.has(p.id)}
                      onChange={() =>
                        setPFSelected((prev) => {
                          const next = new Set(prev)
                          if (next.has(p.id)) next.delete(p.id)
                          else next.add(p.id)
                          return next
                        })
                      }
                    />
                  </td>
                  <td className="kc-mono px-4 py-2.5 text-ink">
                    {p.protocol}/{p.host_port}
                  </td>
                  <td className="px-4 py-2.5 text-ink-2">
                    {p.target_ip ? `${p.target_ip}:${p.target_port}` : `:${p.target_port}`}
                  </td>
                  <td className="px-4 py-2.5">
                    {p.allowed_ips ? (
                      <span className="kc-mono text-sm text-ink-2">{p.allowed_ips}</span>
                    ) : (
                      // 空来源意味着**任何地址都能访问**。用 warning 色而不是
                      // 灰色「—」：这是一个需要用户注意的状态，不是「没填」。
                      <span className="text-sm text-warning">不限制（任何来源均可访问）</span>
                    )}
                  </td>
                  <td className="px-4 py-2.5">
                    <AppliedBadge applied={p.applied} at={p.last_applied_at} />
                  </td>
                  <td className="px-4 py-2.5 text-right">
                    <button
                      className="text-sm text-danger hover:underline"
                      onClick={() => setDeletePF(p)}
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <p className="rounded-card border border-line bg-raised px-4 py-3 text-sm text-ink-3">
        静态地址与「系统信息」里的 IP 可能不一致：前者是我们**期望**的分配，
        后者是从节点**探测到**的实际地址。不一致时以实际地址为准。
        <br />
        端口转发会把虚拟机的服务暴露到外部网络，请确认「允许来源」符合预期。
      </p>

      {/* 邻居表：排查"虚拟机之间不通"时，先看它比抓包快一个数量级。 */}
      <GuestIPSection vmID={vmID} />

      <NeighborSection vmID={vmID} />

      <NicModal
        // key 让「新增」与「编辑某一块」在使用不同状态时重新挂载：
        // 表单的初始值取自 props，切换目标就该拿到新的初始值。
        key={editNic ? `nic-${editNic.id}` : 'nic-new'}
        open={addingNic || editNic !== null}
        vmID={vmID}
        nic={editNic}
        submitting={submit.isPending}
        onClose={() => {
          setAddingNic(false)
          setEditNic(null)
        }}
        onSubmit={(input) =>
          submit.mutate(() =>
            editNic
              ? netApi.updateInterface(vmID, editNic.id, input)
              : netApi.addInterface(vmID, input),
          )
        }
      />

      <Modal
        open={deleteNic !== null}
        title={`删除网卡 #${deleteNic?.order ?? ''}`}
        description="删除后该网卡的配置与地址都会失效。"
        onClose={() => setDeleteNic(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeleteNic(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={submit.isPending}
              onClick={() => deleteNic && submit.mutate(() => netApi.removeInterface(vmID, deleteNic.id))}
            >
              确认删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          删除的是虚拟机的第 {deleteNic?.order} 块网卡（MAC {deleteNic?.mac ?? '—'}）。
          来宾系统里依赖它的网络配置将失效。
        </p>
      </Modal>

      <BindIPModal
        // 每次打开都重新挂载：关闭再打开不该带着上一次填了一半的地址。
        key={bindingIP ? 'bind-open' : 'bind-closed'}
        open={bindingIP}
        vmID={vmID}
        nics={nics}
        submitting={submit.isPending}
        onClose={() => setBindingIP(false)}
        onSubmit={(input) => submit.mutate(() => netApi.bindStaticIP(vmID, input))}
      />

      <Modal
        open={deleteIP !== null}
        title={`解绑地址 ${deleteIP?.ip ?? ''}`}
        description="解绑后虚拟机将回到由 DHCP 分配地址。"
        onClose={() => setDeleteIP(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeleteIP(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={submit.isPending}
              onClick={() => deleteIP && submit.mutate(() => netApi.unbindStaticIP(vmID, deleteIP.id))}
            >
              确认解绑
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          这只解除控制面的分配关系，不会删除来宾系统里已经写好的网络配置。
        </p>
      </Modal>

      <PortForwardModal
        key={addingPF ? 'pf-open' : 'pf-closed'}
        open={addingPF}
        vmID={vmID}
        usedPorts={pfs.map((p) => `${p.protocol}/${p.host_port}`)}
        submitting={submit.isPending}
        onClose={() => setAddingPF(false)}
        onSubmit={(input) => submit.mutate(() => netApi.addPortForward(vmID, input))}
      />

      <Modal
        open={deletePF !== null}
        title={`删除转发 ${deletePF?.protocol}/${deletePF?.host_port ?? ''}`}
        description="删除后该端口将不再指向这台虚拟机。"
        onClose={() => setDeletePF(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setDeletePF(null)}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={submit.isPending}
              onClick={() => deletePF && submit.mutate(() => netApi.removePortForward(vmID, deletePF.id))}
            >
              确认删除
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          删除的是宿主机上的转发规则，虚拟机的服务本身不受影响。
        </p>
      </Modal>
    </div>
  )
}
/** AppliedBadge 标注配置是否已下发到节点。 */
function AppliedBadge({ applied, at }: { applied: boolean; at?: string }) {
  if (applied) {
    return (
      <span className="text-sm text-success" title={at ? formatDateTime(at) : undefined}>
        已生效
      </span>
    )
  }
  // 尚未下发**不是失败**：任务可能还在队列里。用 idle 色而不是红色——
  // 标红会让人去排查一个可能马上就会完成的操作。
  return (
    <span className="text-sm text-ink-3" title="配置已保存，尚未下发到节点">
      尚未生效
    </span>
  )
}
