import type { TaskView } from '@/api/task'
import type { VmView } from '@/api/vm'
import { VMMetricsPanel } from '@/views/monitor/MetricsPanel'
import { ExportTab } from '@/views/vm/ExportTab'
import { GuestActionsSection } from '@/views/vm/GuestActionsSection'
import { MigrateSection } from '@/views/vm/MigrateSection'

import { CDROMTab } from './CDROMTab'
import { ConsoleTab } from './ConsoleTab'
import { CredentialSection } from './CredentialSection'
import { DiskTab } from './DiskTab'
import { EditTab } from './EditTab'
import { EventTab } from './EventTab'
import { NetworkTab } from './NetworkTab'
import { PCIeInfoBlock } from './PCIeInfoBlock'
import { ScheduleTab } from './ScheduleTab'
import { ShareTab } from './ShareTab'
import { SnapshotTab } from './SnapshotTab'
import { SystemTab } from './SystemTab'
import type { TabKey } from './VmTabs'
import { XMLTab } from './XMLTab'

/**
 * 详情页的页签内容与页级提示条。
 *
 * **惰性挂载**：条件渲染而非 CSS 隐藏，未选中的页签不会发任何请求。
 * 六个页签一次性拉全部数据会让打开页面产生十几个并发请求，而用户通常
 * 只看其中一个。
 */
export function VmTabsPanel({
  vm,
  tab,
  tasks,
  nodeName,
  notice,
  error,
  onSaved,
}: {
  vm: VmView
  tab: TabKey
  tasks: TaskView[]
  nodeName?: string
  notice: string
  error: string
  onSaved: () => void
}) {
  return (
    <>
      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && (
        <p role="alert" className="rounded-control bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}
      {tab === 'system' && (
        <div className="flex flex-col gap-4">
          <SystemTab
            vm={vm}
            nodeName={nodeName}
            tasks={tasks}
          />
          {/* 来宾自动化（f-2-10）挂在「系统信息」下而不是另开一个页签：
              它是对这台机器的运维动作，与「这台机器是什么样」属于同一处
              上下文，而页签已经七个了。 */}
          {/* 登录凭据（G-30）挂在「系统信息」下：它回答的是"这台机器怎么登"，
              与配置摘要属于同一处上下文。 */}
          <CredentialSection vmID={vm.id} />
          <GuestActionsSection vm={vm} />
          {/* 迁移挂在「系统信息」下：它改变的是这台机器**在哪**，
              与「这台机器是什么样」属于同一处上下文。 */}
          {/* PCIe 槽位：热插拔设备需要空闲根端口，用完的表现是"挂载成功
              但设备不出现"，而报错里不会提到槽位。 */}
          <PCIeInfoBlock vmID={vm.id} />
          <MigrateSection vm={vm} />
        </div>
      )}
      {tab === 'network' && <NetworkTab vmID={vm.id} />}
      {tab === 'console' && <ConsoleTab vmID={vm.id} />}

      {/* 固件类型一并传进去：**只有 UEFI 机器才有启动项可修**，而"能不能
          修"这个判断不该由快照页自己再查一次虚拟机。 */}
      {tab === 'snapshot' && <SnapshotTab vmID={vm.id} firmware={vm.firmware ?? ''} />}
      {/* 监控画的是**这台机器实际发生了什么**，与「系统信息」里的配置
          是两回事——配置说它应该有多少内存，监控说它实际用了多少。 */}
      {tab === 'monitor' && (
        <VMMetricsPanel
          vmID={vm.id}
          description="指标由采样器按固定间隔采集。图上断开的地方表示那段时间没采到数据，而不是指标为 0。"
        />
      )}
      {/* 光驱：可以多个。**弹出与移除是两件事**，界面上分开呈现。 */}
      {/* 磁盘：列表来自节点实时探测，换总线要关机、系统盘不可卸载。 */}
      {tab === 'disks' && <DiskTab vmID={vm.id} />}
      {tab === 'cdrom' && <CDROMTab vmID={vm.id} nodeID={vm.node_id} />}
      {tab === 'share' && <ShareTab vmID={vm.id} />}
      {/* 事件：把审计与任务两条流水合成一条，回答"这台机器发生过什么"。 */}
      {tab === 'event' && <EventTab vmID={vm.id} />}
      {tab === 'schedule' && <ScheduleTab vmID={vm.id} />}
      {tab === 'export' && <ExportTab vm={vm} />}
      {/* 只读的 libvirt 定义：排查「面板显示的和实际跑的不是一回事」时
          它是唯一的真相。 */}
      {tab === 'xml' && <XMLTab vmID={vm.id} />}
      {tab === 'edit' && <EditTab vmID={vm.id} onSaved={onSaved} />}
    </>
  )
}
