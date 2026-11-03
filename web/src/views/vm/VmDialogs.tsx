import type { PowerAction, VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'
import {
  POWER_ACTION_DANGEROUS,
  POWER_ACTION_LABEL,
  VM_STATUS_LABEL,
} from '@/utils/labels'

/**
 * 详情页的三个确认弹窗：电源操作、进入救援、业务软锁。
 *
 * 都是 fixed 覆盖层（Modal 非 portal），同一时刻最多打开一个，因此顺序
 * 不参与层叠判断。文案里的「会发生什么 / 保留什么」是这些弹窗的主体，
 * 不要为了简洁删掉。
 */
export function VmDialogs({
  vm,
  confirmAction,
  powerPending,
  onCloseConfirm,
  onConfirmPower,
  rescueOpen,
  rescuePending,
  onCloseRescue,
  onEnterRescue,
  lockOpen,
  lockPending,
  onCloseLock,
  lockReason,
  onLockReason,
  onLock,
}: {
  vm: VmView
  confirmAction: PowerAction | null
  powerPending: boolean
  onCloseConfirm: () => void
  onConfirmPower: () => void
  rescueOpen: boolean
  rescuePending: boolean
  onCloseRescue: () => void
  onEnterRescue: () => void
  lockOpen: boolean
  lockPending: boolean
  onCloseLock: () => void
  lockReason: string
  onLockReason: (v: string) => void
  onLock: () => void
}) {
  return (
    <>
      <Modal
        open={confirmAction !== null}
        title={confirmAction ? `确认${POWER_ACTION_LABEL[confirmAction]}` : ''}
        description={confirmAction ? POWER_ACTION_DANGEROUS[confirmAction] : undefined}
        onClose={() => onCloseConfirm()}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => onCloseConfirm()}>
              取消
            </Button>
            <Button
              variant="danger"
              size="sm"
              loading={powerPending}
              onClick={() => confirmAction && onConfirmPower()}
            >
              确认{confirmAction ? POWER_ACTION_LABEL[confirmAction] : ''}
            </Button>
          </>
        }
      >
        <p className="text-base text-ink-2">
          当前状态：{VM_STATUS_LABEL[vm.status]}。操作提交后可在「系统信息」中跟踪进度。
        </p>
      </Modal>

      <Modal
        open={rescueOpen}
        title={`让「${vm.name}」进入救援模式`}
        description="虚拟机会从救援镜像启动，盘型、网卡与引导顺序会被调整。磁盘内容原样保留，退出时按进入前的配置自动还原。"
        onClose={() => onCloseRescue()}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => onCloseRescue()}>
              取消
            </Button>
            <Button
              size="sm"
              loading={rescuePending}
              onClick={() => onEnterRescue()}
            >
              进入救援
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-2 text-base text-ink-2">
          <p>
            进入前会保存一份配置快照（引导顺序、机型、固件、网卡与显示设备），
            退出时按它还原。
          </p>
          <p className="text-ink-3">
            快照只覆盖救援过程会改动的字段——你在这期间通过编辑页改的其它配置
            不会被回滚，因为那属于「你的改动」，不属于「救援过程」。
          </p>
          <p>
            当前为
            <span className="text-ink">{VM_STATUS_LABEL[vm.status]}</span>
            ，进入救援需要保持关机状态。
          </p>
        </div>
      </Modal>

      <Modal
        open={lockOpen}
        title={`锁定「${vm.name}」`}
        description="锁定后无法删除该虚拟机，也不会被批量删除选中执行。随时可以解锁——解锁需要一次二次验证。"
        onClose={() => onCloseLock()}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => onCloseLock()}>
              取消
            </Button>
            <Button size="sm" loading={lockPending} onClick={() => onLock()}>
              锁定
            </Button>
          </>
        }
      >
        {/* 原因必填与否是一个取舍：要求必填能让「为什么锁着」总有答案，
            但也可能让人嫌麻烦干脆不加锁。因此它是可选的，但界面上明确
            说明「会被显示给其他人看」——写不写由用户判断。 */}
        <Input
          label="锁定原因（可选）"
          value={lockReason}
          maxLength={255}
          placeholder="例如：生产环境主库，禁止删除"
          onChange={(e) => onLockReason(e.target.value)}
          hint="会显示在虚拟机列表与详情页上，让其他人知道为什么不能删。"
        />
      </Modal>
    </>
  )
}
