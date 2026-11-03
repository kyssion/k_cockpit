import type { VmView } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { formatDateTime } from '@/utils/format'
import { VM_STATUS_LABEL } from '@/utils/labels'

/**
 * 详情页的状态告警区：链式克隆依赖、救援模式、重装备份、业务软锁。
 *
 * 四条告警的先后有讲究（各自的原因写在原处）：越"根本"的越靠前——
 * 救援改变的是「你现在看到的是什么系统」，比「能不能删」更要紧。
 */
export function VmAlerts({
  vm,
  onUnlinkTemplate,
  onPurgeBackup,
  purgePending,
}: {
  vm: VmView
  onUnlinkTemplate: () => void
  onPurgeBackup: () => void
  purgePending: boolean
}) {
  return (
    <>
      {/* 链式克隆的依赖必须显示出来：磁盘只是模板之上的一层覆盖，模板被删后
          数据就不可用了，而且不会立刻报错。用户看不到这条关系，就无法理解
          为什么「删掉一个模板」会让自己的机器出事。 */}
      {vm.clone_mode === 'linked' && (
        <div className="rounded-card border border-line bg-raised px-4 py-3">
          <p className="text-base text-ink-2">
            此虚拟机是模板
            <span className="text-ink"> #{vm.template_id}</span> 的链式克隆，
            磁盘以该模板为底层。
            <span className="font-medium text-ink">
              模板被删除后，这台机器的数据将不可用
            </span>
            ——且不会立刻报错，要等到下次开机或读到未缓存的数据块时才暴露。
          </p>
          {/* 给一条出路：只说清风险而不给出解决方式，用户能做的是
              「那就别删模板」——而模板管理迟早需要删。 */}
          <p className="mt-2 text-sm text-ink-3">
            如果这个模板需要下线，可以先把这台机器的磁盘合并成独立镜像，
            之后它就不再依赖模板了。
          </p>
          <Button
            size="sm"
            variant="secondary"
            className="mt-2"
            onClick={() => onUnlinkTemplate()}
          >
            解除模板依赖
          </Button>
        </div>
      )}

      {/* 救援提示放在锁定提示之前：救援改变的是「你现在看到的是什么系统」，
          比「能不能删」更根本。救援模式下盘型、网卡、引导顺序都被改过，
          把它当成日常状态会让人做出错误判断。 */}
      {vm.rescue_active && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base font-medium text-warning">此虚拟机处于救援模式</p>
          <p className="mt-1 text-base text-ink-2">
            它当前从救援镜像启动，盘型、网卡与引导顺序均已调整——
            <span className="font-medium text-ink">画面里看到的不是你的系统</span>。
            磁盘内容原样保留，退出救援时会按进入前的配置自动还原。
            {vm.rescue_since && (
              <span className="text-ink-3"> · 自 {formatDateTime(vm.rescue_since)}</span>
            )}
          </p>
        </div>
      )}

      {/* 备份提示。它是用户「回到原来的系统」的唯一退路，因此既要让他知道
          它存在，也要让他知道它挡着下一次重装。 */}
      {vm.has_reinstall_backup && (
        <div className="flex items-start justify-between gap-4 rounded-card border border-line bg-raised px-4 py-3">
          <p className="text-base text-ink-2">
            重装系统时留下的原系统盘备份仍在占用存储空间。
            <span className="text-ink-3">
              它是回到原系统的唯一退路；确认不再需要后可以清理，
              清理后即可再次重装。
            </span>
            {vm.reinstall_at && (
              <span className="text-ink-3"> · 重装于 {formatDateTime(vm.reinstall_at)}</span>
            )}
          </p>
          <Button
            variant="secondary"
            size="sm"
            disabled={vm.status !== 'stopped'}
            loading={purgePending}
            title={vm.status !== 'stopped' ? '清理备份需要先关机' : ''}
            onClick={() => onPurgeBackup()}
          >
            清理备份
          </Button>
        </div>
      )}

      {vm.locked && (
        <div className="rounded-card border border-warning/40 bg-warning/5 px-4 py-3">
          <p className="text-base font-medium text-warning">此虚拟机已锁定</p>
          <p className="mt-1 text-base text-ink-2">
            锁定期间无法删除，也不会被批量删除操作选中执行。
            {vm.lock_reason ? (
              <>
                锁定原因：<span className="text-ink">{vm.lock_reason}</span>
              </>
            ) : (
              <span className="text-ink-3">（未填写原因）</span>
            )}
            {vm.locked_at && (
              <span className="text-ink-3"> · {formatDateTime(vm.locked_at)}</span>
            )}
          </p>
        </div>
      )}

      {vm.available_actions.length === 0 && !vm.stale && (
        <p className="text-base text-ink-3">
          当前状态（{VM_STATUS_LABEL[vm.status]}）下没有可执行的电源操作。
        </p>
      )}
    </>
  )
}
