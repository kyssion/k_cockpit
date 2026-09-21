/**
 * NetworkToolsSection 网络的三个运维动作（F-4-05）。
 *
 * 它们都是低频但必要的动作：不常点，一旦需要就没有别的路可走。因此放在
 * 一页的底部，且**每个都说明它到底做什么**——这三个名字都不够自解释，
 * 尤其是 IPv6「保护」：它是"只信任列出的前缀"，而不是"禁用 IPv6"。
 */
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { netMaintainApi } from '@/api/network'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'

export function NetworkToolsSection({
  nodeID,
  onError,
  onNotice,
}: {
  nodeID: number
  onError: (msg: string) => void
  onNotice: (msg: string) => void
}) {
  const queryClient = useQueryClient()
  const [ipv6Open, setIpv6Open] = useState(false)
  const [releaseOpen, setReleaseOpen] = useState(false)

  const resetCounters = useMutation({
    mutationFn: () => netMaintainApi.resetCounters(nodeID),
    onSuccess: (r) => onNotice(r.message || '计数器已归零'),
    onError: (err) => onError(describe(err)),
  })

  const refreshTasks = () =>
    void queryClient.invalidateQueries({ queryKey: ['tasks'] })

  return (
    <section className="flex flex-col gap-3">
      <h2 className="text-sm font-medium text-ink-2">运维动作</h2>

      <div className="flex flex-wrap gap-2">
        <Button size="sm" variant="secondary" loading={resetCounters.isPending} onClick={() => resetCounters.mutate()}>
          重置流量计数
        </Button>
        <Button size="sm" variant="secondary" onClick={() => setIpv6Open(true)}>
          IPv6 保护策略
        </Button>
        <Button size="sm" variant="secondary" onClick={() => setReleaseOpen(true)}>
          释放端口
        </Button>
      </div>

      <p className="text-sm text-ink-3">
        计数是累计值：换过环境或迁移之后，旧基数会让"这个月用了多少"完全失真。
        重置让它从零开始，而不是靠人去记一个差值。
      </p>

      <IPv6Modal
        nodeID={nodeID}
        open={ipv6Open}
        onClose={() => setIpv6Open(false)}
        onDone={(m) => {
          setIpv6Open(false)
          onNotice(m)
        }}
        onError={onError}
      />

      <ReleaseModal
        nodeID={nodeID}
        open={releaseOpen}
        onClose={() => setReleaseOpen(false)}
        onDone={(m) => {
          setReleaseOpen(false)
          refreshTasks()
          onNotice(m)
        }}
        onError={onError}
      />
    </section>
  )
}

/** IPv6 保护策略。 */
function IPv6Modal({
  nodeID,
  open,
  onClose,
  onDone,
  onError,
}: {
  nodeID: number
  open: boolean
  onClose: () => void
  onDone: (msg: string) => void
  onError: (msg: string) => void
}) {
  const [protect, setProtect] = useState(false)
  const [prefixes, setPrefixes] = useState('')

  const apply = useMutation({
    mutationFn: () =>
      netMaintainApi.applyIPv6Policy({
        node_id: nodeID,
        protect,
        trusted_prefixes: prefixes
          .split(/[,，\n]/)
          .map((s) => s.trim())
          .filter(Boolean),
      }),
    onSuccess: (r) => onDone(r.message || 'IPv6 策略已下发'),
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title="IPv6 保护策略"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button size="sm" loading={apply.isPending} onClick={() => apply.mutate()}>
            下发
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {/* "保护"的语义必须写清：它不是禁用 IPv6，而是只信任列出的前缀。
            混同这两者会让用户以为开启保护等于断网。 */}
        <p className="text-sm text-ink-3">
          「保护」不是禁用 IPv6，而是**只放行可信前缀**，其余 IPv6 转发被限制。
        </p>
        <label className="flex cursor-pointer items-center gap-2 text-sm text-ink-2">
          <input type="checkbox" checked={protect} onChange={(e) => setProtect(e.target.checked)} />
          启用 IPv6 保护
        </label>
        <Input
          label="可信前缀"
          value={prefixes}
          onChange={(e) => setPrefixes(e.target.value)}
          placeholder="例如 2001:db8::/32，多个用逗号分隔"
        />
        {apply.isError && <p className="text-sm text-danger">{describe(apply.error)}</p>}
      </div>
    </Modal>
  )
}

/** 释放端口。 */
function ReleaseModal({
  nodeID,
  open,
  onClose,
  onDone,
  onError,
}: {
  nodeID: number
  open: boolean
  onClose: () => void
  onDone: (msg: string) => void
  onError: (msg: string) => void
}) {
  const [portRef, setPortRef] = useState('')

  const release = useMutation({
    mutationFn: () => netMaintainApi.releasePort({ node_id: nodeID, port_ref: portRef.trim() }),
    onSuccess: () => onDone('已提交端口释放'),
    onError: (err) => onError(describe(err)),
  })

  return (
    <Modal
      open={open}
      title="释放端口"
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            loading={release.isPending}
            disabled={portRef.trim() === ''}
            onClick={() => release.mutate()}
          >
            释放
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-2">
        <Input
          label="端口引用"
          value={portRef}
          onChange={(e) => setPortRef(e.target.value)}
          placeholder="网桥名或虚拟机网口名"
        />
        {/* 与"删除网卡"的区别必须说清：一个动的是记录，一个动的是节点资源。 */}
        <p className="text-sm text-ink-3">
          这是**节点资源**层面的回收，与「删除网卡」（控制面记录）不同：只删记录
          会留下一个谁也不用却一直占着的端口，它最终会以"端口不够用"的形式暴露。
        </p>
        {release.isError && <p className="text-sm text-danger">{describe(release.error)}</p>}
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  return error instanceof Error ? error.message : '操作失败'
}
