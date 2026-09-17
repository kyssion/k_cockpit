/**
 * GuestActionsSection 是「系统信息」页签底部的来宾自动化区块（F-2-10）。
 *
 * 做成独立组件而不是并进 VmDetailPage：那一块自带一次查询、一组按状态
 * 变化的按钮与两个弹框，继续往 2600 行的文件里堆只会让每次改动都要在
 * 几千行里定位。
 *
 * 核心的界面判断只有一条：**可用动作由后端下发**，界面不自行判断。
 * 可用性与「是否装了 Guest Agent」「是否运行中」都相关，前端各判一遍迟早
 * 会与后端不一致——那会出现「按钮可点、点下去被拒」这类最让人烦躁的交互。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import {
  GUEST_ACTION_HINT,
  vmApi,
  type GuestAction,
  type VmView,
} from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'

const ALL_ACTIONS: GuestAction[] = [
  'password_online',
  'password_offline',
  'disk_attach',
  'expand_disk',
]

export function GuestActionsSection({ vm }: { vm: VmView }) {
  const queryClient = useQueryClient()
  const [openAction, setOpenAction] = useState<GuestAction | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const caps = useQuery({
    queryKey: ['vm-guest-actions', vm.id],
    queryFn: () => vmApi.guestActions(vm.id),
  })

  const available = new Set(caps.data?.available_actions ?? [])

  const run = useMutation({
    mutationFn: (input: {
      action: GuestAction
      username?: string
      password?: string
      disk_id?: string
      disk_gb?: number
    }) => vmApi.runGuestAction(vm.id, input),
    onSuccess: (result) => {
      setOpenAction(null)
      setError('')
      setNotice(`已提交，任务 #${result.task_id} 正在执行`)
      void queryClient.invalidateQueries({ queryKey: ['vm-guest-actions', vm.id] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => {
      setOpenAction(null)
      setNotice('')
      setError(describe(err))
    },
  })

  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between gap-2">
        <h3 className="text-sm text-ink-3">来宾自动化</h3>
        {/* agent 状态单独显示：它是这些动作的前提，而「没装」与「装了但没跑」
            对用户来说是同一件事——都得进去把它启起来。 */}
        {caps.data && (
          <span className={caps.data.guest_agent_ready ? 'text-xs text-success' : 'text-xs text-warning'}>
            {caps.data.guest_agent_ready ? 'Guest Agent 已就绪' : '未检测到 Guest Agent'}
          </span>
        )}
      </div>

      {notice && (
        <p className="mt-2.5 rounded-control bg-success/10 px-3 py-2 text-base text-success">
          {notice}
        </p>
      )}
      {error && (
        <p className="mt-2.5 rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      <div className="mt-2.5 flex flex-wrap gap-2">
        {ALL_ACTIONS.map((action) => {
          const hint = GUEST_ACTION_HINT[action]
          const enabled = available.has(action)
          return (
            <Button
              key={action}
              variant="secondary"
              size="sm"
              disabled={!enabled}
              // 禁用时说明**是哪一条不满足**，而不是笼统的「不可用」——
              // 两种情况用户要做的事完全不同（装 agent vs 关机）。
              title={
                enabled
                  ? ''
                  : hint.requiresRunning
                    ? '需要虚拟机运行中，且来宾里已启动 Guest Agent'
                    : '需要虚拟机处于关机状态'
              }
              onClick={() => setOpenAction(action)}
            >
              {hint.label}
            </Button>
          )
        })}
      </div>

      <p className="mt-2 text-xs text-ink-3">
        在线类操作经 Guest Agent 在来宾内执行；离线类操作绕开来宾、由宿主机
        直接挂载磁盘完成——后者用在系统起不来的时候，但改完首次开机可能需要
        做一次上下文修复。
      </p>

      <ActionModal
        key={openAction ?? 'none'}
        action={openAction}
        pending={run.isPending}
        onClose={() => setOpenAction(null)}
        onConfirm={(input) => run.mutate(input)}
      />
    </section>
  )
}

/** ActionModal 按动作渲染对应的输入项。 */
function ActionModal({
  action,
  pending,
  onClose,
  onConfirm,
}: {
  action: GuestAction | null
  pending: boolean
  onClose: () => void
  onConfirm: (input: {
    action: GuestAction
    username?: string
    password?: string
    disk_id?: string
    disk_gb?: number
  }) => void
}) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [diskID, setDiskID] = useState('')
  const [diskGB, setDiskGB] = useState('')

  if (!action) return null
  const hint = GUEST_ACTION_HINT[action]

  const isPassword = action === 'password_online' || action === 'password_offline'

  return (
    <Modal
      open
      title={hint.label}
      description={hint.detail}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" size="sm" onClick={onClose}>
            取消
          </Button>
          <Button
            size="sm"
            loading={pending}
            disabled={
              isPassword
                ? username.trim() === '' || password.length < 8
                : action === 'disk_attach'
                  ? diskID.trim() === ''
                  : Number(diskGB) <= 0
            }
            onClick={() =>
              onConfirm({
                action,
                username: isPassword ? username.trim() : undefined,
                password: isPassword ? password : undefined,
                disk_id: action === 'disk_attach' ? diskID.trim() : undefined,
                disk_gb: action === 'expand_disk' ? Number(diskGB) : undefined,
              })
            }
          >
            执行
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {isPassword && (
          <>
            <Input
              label="用户名"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              placeholder="例如：ubuntu"
              hint="用户名与密码不允许含空格、冒号或换行——它们会被拼进交给来宾执行的命令，含这些字符会让命令被拆开。"
            />
            <Input
              label="新密码"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="至少 8 位"
              hint="只在本面板的约束下校验长度；来宾系统自己的密码策略仍然生效，那里拒绝时会给出更具体的原因。"
            />
          </>
        )}

        {action === 'disk_attach' && (
          <Input
            label="磁盘标识"
            value={diskID}
            onChange={(e) => setDiskID(e.target.value)}
            placeholder="例如：ata-mock-data1"
            hint="从节点详情页的块设备列表里取。分区与格式化**不可逆**，请先确认目标盘上没有需要的数据。"
          />
        )}

        {action === 'expand_disk' && (
          <Input
            label="扩容后大小（GB）"
            type="number"
            min={1}
            value={diskGB}
            onChange={(e) => setDiskGB(e.target.value)}
            hint="只会变大，不能缩小——缩小文件系统需要先删数据，那属于重装。"
          />
        )}

        {isPassword && (
          <p className="rounded-control border border-line bg-raised px-3 py-2 text-sm text-ink-3">
            密码不会写入审计流水，任务执行完成后也会从任务参数里清除。
          </p>
        )}
      </div>
    </Modal>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
