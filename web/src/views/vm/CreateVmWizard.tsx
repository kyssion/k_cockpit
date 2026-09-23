import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { templateApi, type TemplateView } from '@/api/template'
import { passthroughApi, type PCIDevice } from '@/api/passthrough'
import { userStorageApi, type FileView } from '@/api/userstorage'
import { vmApi, type CreateFormField, type DataDiskInput } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { Icon, type IconName } from '@/components/common/Icon'
import { Input } from '@/components/common/Input'
import { Modal } from '@/components/common/Modal'

/**
 * 创建虚拟机向导（F-2-02）。
 *
 * 两条贯穿本组件的约定：
 *
 *  1. **规则来自后端**（R-002）：步骤、字段、取值、默认值、前置条件全部
 *     取自 `create-form`，这里不写第二份可选值。改矩阵即可改向导，不必
 *     动前端。
 *  2. **草稿只存配置，不存敏感信息**（R-008）。本版没有凭据字段（首次
 *     启动初始化属 F-2-17），因此草稿里全是普通配置。
 *
 * 向导是长表单，逐步填写比一屏塞满更好填；但步骤不是越多越好——这里按
 * 后端下发的分组走，后端没给的分组（如尚未实现的硬件直通）不占一步。
 */

/** 创建方式。导入的两条在面板里有独立入口，这里只做引导。 */
type CreateMode = 'iso' | 'template'

/** 草稿的结构。 */
interface Draft {
  name: string
  nodeID: number
  mode: CreateMode
  values: Record<string, unknown>
  /** 多 ISO（G-29）：首个为主安装盘。旧草稿只有 isoFileID 单值，读入时归一。 */
  isoFileIDs?: number[]
  /** @deprecated 旧草稿的单值字段，读取时并入 isoFileIDs。 */
  isoFileID?: number
  switchID: number
  groupIDs: number[]
  count: number
  dataDisks: DataDiskInput[]
  savedAt: number
}

const DRAFT_KEY = 'kc.vm.create.draft'
const DRAFT_TTL_MS = 24 * 60 * 60 * 1000

/**
 * readDraft 读取草稿；过期或无法解析时**顺手清掉**。
 *
 * 留着一份解析不了的草稿没有任何好处：它会在下一次打开时再失败一次，
 * 而用户既看不到它也不会被告知。
 */
function readDraft(): Draft | null {
  try {
    const raw = localStorage.getItem(DRAFT_KEY)
    if (!raw) return null
    const draft = JSON.parse(raw) as Draft
    if (!draft.savedAt || Date.now() - draft.savedAt > DRAFT_TTL_MS) {
      localStorage.removeItem(DRAFT_KEY)
      return null
    }
    return draft
  } catch {
    localStorage.removeItem(DRAFT_KEY)
    return null
  }
}

export function CreateVmWizard({ open, onClose }: { open: boolean; onClose: () => void }) {
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  // 草稿在**挂载时**读一次，而不是放进 effect：effect 里的 setState 会再
  // 触发一轮渲染，而它做的事其实只是初始化——初始化就该在初始化时做。
  const [draft] = useState(readDraft)

  const [step, setStep] = useState(0)
  const [name, setName] = useState(draft?.name ?? '')
  const [nodeID, setNodeID] = useState(draft?.nodeID ?? 0)
  const [mode, setMode] = useState<CreateMode>(draft?.mode ?? 'iso')
  // values 只保存**用户改过的项**；渲染与提交用的是它与后端默认值的合并
  // 结果（effective）。这样后端调整默认值时，已经填过一半的表单不会被
  // 覆盖，而没动过的项会跟着更新。
  const [values, setValues] = useState<Record<string, unknown>>(draft?.values ?? {})
  // 多 ISO（G-29）：首个是主安装盘，其余作为额外光驱。旧草稿的单值并入数组。
  const [isoFileIDs, setISOFileIDs] = useState<number[]>(() => {
    const ids = draft?.isoFileIDs ?? (draft?.isoFileID ? [draft.isoFileID] : [])
    return ids
  })
  const [switchID, setSwitchID] = useState(draft?.switchID ?? 0)
  const [groupIDs, setGroupIDs] = useState<number[]>(draft?.groupIDs ?? [])
  const [count, setCount] = useState(draft?.count ?? 1)
  // 数据盘不在矩阵里（它是一组结构且数量不定），因此单独存一份 state。
  // 这三个也不在矩阵里：网口数量是一个"重复次数"，直通与软盘是从别的接口
  // 选出来的对象，矩阵表达不了。
  const [nicCount, setNicCount] = useState(1)
  const [pciAddresses, setPciAddresses] = useState<string[]>([])
  const [floppyFileID, setFloppyFileID] = useState(0)
  const [dataDisks, setDataDisks] = useState<DataDiskInput[]>(draft?.dataDisks ?? [])
  const [templateID, setTemplateID] = useState(0)
  const [error, setError] = useState('')
  const [submitted, setSubmitted] = useState<number[] | null>(null)

  // 幂等键在**打开向导时**生成一次：它是「这一次提交」的身份。每次渲染
  // 重新生成等于没有幂等，而放在提交时生成则挡不住按钮的连点。
  const [clientToken] = useState(() => newToken())

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list, enabled: open })

  // 目标节点：草稿里存过就用它，否则落到第一个在线节点。
  //
  // 必须是**派生值**而不是「effect 里 setState」：字段矩阵、镜像、模板、
  // 网络、配额全都要按节点向后端取，节点为空时步骤只剩「创建方式 + 确认
  // 信息」两屏、一个输入框都没有——用户看得见「还需要填写：虚拟机名、
  // 节点、安装镜像」，却无处可填。
  const activeNodeID =
    nodeID > 0
      ? nodeID
      : (nodes.data?.find((n) => n.status === 'online')?.id ?? nodes.data?.[0]?.id ?? 0)

  const form = useQuery({
    queryKey: ['vm-create-form', activeNodeID],
    queryFn: () => vmApi.createForm(activeNodeID),
    enabled: open && activeNodeID > 0,
  })
  const templates = useQuery({
    queryKey: ['templates', { node_id: activeNodeID, only_ready: true }],
    queryFn: () => templateApi.list({ node_id: activeNodeID, only_ready: true }),
    enabled: open && activeNodeID > 0 && mode === 'template',
  })

  // 合并后的取值：后端默认值打底，用户改动覆盖其上。
  const effective = useMemo(
    () => ({ ...(form.data?.values ?? {}), ...values }),
    [form.data, values],
  )
  // 主网口默认落到系统网络，省掉用户必选的一步。
  const effectiveSwitch =
    switchID > 0 ? switchID : (form.data?.switches.find((s) => s.is_system)?.id ?? 0)

  // 存草稿。字段逐个列出而不是把整个 state 序列化：后者迟早会把某个不该
  // 落盘的东西一起写进去，而这种泄漏不会有任何提示。
  useEffect(() => {
    if (!open || submitted) return
    const draft: Draft = {
      name, nodeID, mode,
      // 初始密码**不进草稿**（R-008）：localStorage 里的凭据是一个不出门
      // 却能长期存在的泄漏面，而它的价值只是"少打一次字"。
      values: withoutSensitive(values),
      isoFileIDs, switchID, groupIDs, count, dataDisks,
      savedAt: Date.now(),
    }
    localStorage.setItem(DRAFT_KEY, JSON.stringify(draft))
  }, [open, submitted, name, nodeID, mode, values, isoFileIDs, switchID, groupIDs, count, dataDisks])

  const steps = useMemo(() => {
    const backend = (form.data?.groups ?? []).filter((g) => !g.planned)
    return [{ key: 'mode', label: '创建方式' }, ...backend, { key: 'confirm', label: '确认信息' }]
  }, [form.data])

  const create = useMutation({
    mutationFn: () =>
      vmApi.create({
        name: name.trim(),
        node_id: activeNodeID,
        vcpu: num(effective.vcpu, 2),
        memory_mb: num(effective.memory_mb, 2048),
        disk_gb: num(effective.disk_gb, 40),
        remark: str(effective.remark),
        group_name: str(effective.group_name),

        template_id: mode === 'template' && templateID > 0 ? templateID : undefined,
        clone_mode: mode === 'template' && templateID > 0 ? 'full' : undefined,

        disk_format: str(effective.disk_format),
        disk_bus: str(effective.disk_bus),
        nic_model: str(effective.nic_model),
        os_type: str(effective.os_type),
        os_variant: str(effective.os_variant),
        hostname: str(effective.hostname),
        initial_password: str(effective.initial_password),
        init_mode: str(effective.init_mode),
        static_ip: str(effective.static_ip),
        data_disks: dataDisks.length > 0 ? dataDisks : undefined,
        nic_count: nicCount > 1 ? nicCount : undefined,
        pci_addresses: pciAddresses.length > 0 ? pciAddresses : undefined,
        floppy_file_id: floppyFileID > 0 ? floppyFileID : undefined,
        machine_type: str(effective.machine_type),
        firmware: str(effective.firmware),
        secure_boot: bool(effective.secure_boot),
        boot_order: str(effective.boot_order),
        auto_start: bool(effective.auto_start),
        watchdog: str(effective.watchdog),
        cpu_type: str(effective.cpu_type),
        cpu_limit_percent: num(effective.cpu_limit_percent, 0),
        apic: bool(effective.apic),
        pae: bool(effective.pae),
        freeze_on_start: bool(effective.freeze_on_start),
        disk_iops_total: num(effective.disk_iops_total, 0),
        disk_iops_read: num(effective.disk_iops_read, 0),
        disk_iops_write: num(effective.disk_iops_write, 0),

        video_model: str(effective.video_model),
        rtc_mode: str(effective.rtc_mode),
        arch: str(effective.arch),
        cpu_hotplug: bool(effective.cpu_hotplug),
        memory_hotplug: bool(effective.memory_hotplug),

        // 多 ISO 优先（首个为主安装盘）；单值字段保留给旧客户端兼容。
        iso_file_ids: mode === 'iso' && isoFileIDs.length > 0 ? isoFileIDs : undefined,
        iso_file_id: mode === 'iso' && isoFileIDs.length === 1 ? isoFileIDs[0] : undefined,
        switch_id: effectiveSwitch > 0 ? effectiveSwitch : undefined,
        security_group_ids: groupIDs.length > 0 ? groupIDs : undefined,

        count,
        client_token: clientToken,
        batch_key: count > 1 ? clientToken : undefined,
      }),
    onSuccess: (result) => {
      setSubmitted(result.task_ids ?? [result.task_id])
      setError('')
      localStorage.removeItem(DRAFT_KEY)
      void queryClient.invalidateQueries({ queryKey: ['vms'] })
      void queryClient.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (err) => setError(describe(err)),
  })

  function setValue(key: string, value: unknown) {
    setValues((prev) => ({ ...prev, [key]: value }))
  }

  /**
   * 带联动规则的写入（G-29）。与后端 validateCreateCombinations 是**同一套
   * 规则的前端副本**：后端那份保证非法组合进不了系统，这份让用户根本不必
   * 手工去改联动项——选了 Windows 再手选回 BIOS，是无意义的来回。
   *
   * 联动只做"自动调整"不做提示：调整的每一项都是矩阵里看得见的字段，
   * 用户改完立刻能看到它们变了。
   */
  function setValueLinked(key: string, value: unknown) {
    const patch: Record<string, unknown> = { [key]: value }
    const v = String(value)
    const cur = (k: string) => String(effective[k] ?? '')
    if (key === 'os_type') {
      if (v === 'windows') {
        // Windows 惯例：UEFI 引导、SATA 磁盘（安装器不带 VirtIO 驱动）、
        // e1000 网卡、硬件钟记本地时间。
        if (cur('firmware') !== 'uefi') patch.firmware = 'uefi'
        if (cur('disk_bus') === 'virtio' || cur('disk_bus') === 'ide') patch.disk_bus = 'sata'
        if (cur('nic_model') === 'virtio') patch.nic_model = 'e1000'
        if (cur('rtc_mode') === 'utc') patch.rtc_mode = 'localtime'
        // i440fx + UEFI 会卡在固件画面，连同修正到 q35。
        if (cur('machine_type') === 'i440fx') patch.machine_type = 'q35'
      }
    } else if (key === 'arch') {
      if (v === 'aarch64') {
        // ARM 是硬约束：virt 机型 + UEFI + ramfb，三者缺一引导不了。
        patch.machine_type = 'virt'
        patch.firmware = 'uefi'
        patch.video_model = 'ramfb'
      } else if (v === 'x86_64') {
        // 从 ARM 切回 x86 时，ARM 专属的选择一并回落到 x86 合法值。
        if (cur('machine_type') === 'virt') patch.machine_type = 'q35'
        if (cur('video_model') === 'ramfb') patch.video_model = 'virtio'
      }
    } else if (key === 'machine_type') {
      if (v === 'i440fx' && cur('firmware') === 'uefi' && cur('os_type') === 'windows') {
        patch.firmware = 'bios'
      }
    } else if (key === 'firmware') {
      if (v === 'bios' && bool(effective.secure_boot)) {
        // BIOS 没有 Secure Boot 概念，留着这个开关等于配置与实际不符。
        patch.secure_boot = false
      }
      if (v === 'uefi' && cur('machine_type') === 'i440fx' && cur('os_type') === 'windows') {
        patch.machine_type = 'q35'
      }
    }
    setValues((prev) => ({ ...prev, ...patch }))
  }

  function pickTemplate(id: number, list: TemplateView[] | undefined) {
    setTemplateID(id)
    const tpl = (list ?? []).find((t) => t.id === id)
    if (!tpl) return
    // 模板带出规格：它取自制备时的源虚拟机，是最合理的起点。**不是**强制，
    // 用户仍可改（磁盘另有下限，由服务端抬升）。
    if (tpl.default_cpu > 0) setValue('vcpu', tpl.default_cpu)
    if (tpl.default_memory_mb > 0) setValue('memory_mb', tpl.default_memory_mb)
    if (tpl.min_disk_gb > 0) setValue('disk_gb', tpl.min_disk_gb)
  }

  function handleClose() {
    if (!submitted) localStorage.removeItem(DRAFT_KEY)
    setStep(0)
    setSubmitted(null)
    setError('')
    onClose()
  }

  const fieldsOf = (group: string) => (form.data?.fields ?? []).filter((f) => f.group === group)
  // 数据盘的格式与驱动**复用矩阵的可选值**：它们与系统盘是同一套候选，
  // 抄一份之后新增格式时数据盘那边会静默拒绝它。
  const optionsOf = (key: string) =>
    (form.data?.fields ?? []).find((f) => f.key === key)?.options ?? []

  const missing: string[] = []
  if (!name.trim()) missing.push('虚拟机名')
  if (activeNodeID <= 0) missing.push('节点')
  if (mode === 'iso' && isoFileIDs.length === 0) missing.push('安装镜像')
  if (mode === 'template' && templateID <= 0) missing.push('模板')
  const blocked = (form.data?.prerequisites ?? []).filter((p) => !p.ok)

  // 动作栏交给 Modal 的 footer：内容区自己滚动时，按钮不会跟着滚出视野。
  const footer = submitted ? (
    <div className="flex justify-end gap-2">
      <Link to="/task">
        <Button size="sm">查看任务</Button>
      </Link>
      <Button variant="secondary" size="sm" onClick={handleClose}>
        留在本页
      </Button>
    </div>
  ) : (
    <div className="flex items-center justify-between gap-2">
      <Button variant="secondary" size="sm" onClick={handleClose}>
        取消
      </Button>
      <div className="flex gap-2">
        <Button
          variant="secondary"
          size="sm"
          onClick={() => setStep((s) => Math.max(0, s - 1))}
          disabled={step === 0}
        >
          上一步
        </Button>
        {steps[step]?.key === 'confirm' ? (
          <Button
            size="sm"
            loading={create.isPending}
            disabled={missing.length > 0 || blocked.length > 0}
            onClick={() => create.mutate()}
          >
            提交创建
          </Button>
        ) : (
          <Button size="sm" onClick={() => setStep((s) => Math.min(steps.length - 1, s + 1))}>
            下一步
          </Button>
        )}
      </div>
    </div>
  )

  return (
    <Modal
      open={open}
      title={submitted ? '创建任务已提交' : '创建虚拟机'}
      description={submitted ? undefined : '创建是异步操作，提交后可在任务中心查看进度。'}
      onClose={handleClose}
      size="xl"
      bodyClassName="p-0"
      footer={footer}
    >
      {submitted ? (
        <div className="px-6 py-8">
          <p className="text-base text-ink-2">
            共提交 {submitted.length} 个任务：{submitted.map((id) => `#${id}`).join('、')}
          </p>
        </div>
      ) : (
        <div className="flex min-h-[520px] flex-col md:flex-row">
          <StepRail steps={steps} current={step} onJump={setStep} />
          <div className="min-h-0 flex-1 overflow-y-auto px-6 py-5 md:max-h-[58vh]">
            <StepHeader stepKey={steps[step]?.key} fallback={steps[step]?.label ?? ''} />

          {step === 0 && (
            <div className="flex flex-col gap-5">
              {/* 节点放在第一步：它决定后面各步能填什么（存储池、网络、镜像、
                  架构都由节点决定），而字段矩阵本身也要按节点向后端要。 */}
              <div className="flex flex-col gap-1.5">
                <label htmlFor="wizard-node" className="text-sm font-medium text-ink-2">
                  目标节点
                </label>
                <select
                  id="wizard-node"
                  value={activeNodeID}
                  onChange={(e) => setNodeID(Number(e.target.value))}
                  className="h-10 rounded-control border border-line-strong bg-raised px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
                >
                  <option value={0}>请选择节点</option>
                  {(nodes.data ?? []).map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                      {n.status !== 'online' ? '（离线）' : ''}
                    </option>
                  ))}
                </select>
                <p className="text-sm text-ink-3">
                  {nodes.isPending
                    ? '正在读取节点…'
                    : (nodes.data ?? []).length === 0
                      ? '还没有接入任何节点。虚拟机的磁盘与网络都在宿主机上，需要先接入一台节点。'
                      : '虚拟机建在哪台宿主机上。切换节点会重新加载该节点的可用配置。'}
                </p>
              </div>

              <div className="flex flex-col gap-2">
                <p className="text-sm font-medium text-ink-2">创建方式</p>
                <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-4">
                  <ModeCard
                    icon="storage"
                    active={mode === 'iso'}
                    title="ISO 安装"
                    desc="挂上镜像从零安装系统。适用于需要自定义分区与安装选项的场景。"
                    onClick={() => setMode('iso')}
                  />
                  <ModeCard
                    icon="template"
                    active={mode === 'template'}
                    title="模板克隆"
                    desc="从已制备好的模板复制一台，开机即用。最快的方式。"
                    onClick={() => setMode('template')}
                  />
                  {/* G-34：两条导入路径已接通（文件真实上传 + 转换产出模板）。
                      它们不在向导内完成——导入是一个独立的长任务流程，
                      引导到导入页而不是把整个流程塞进弹窗。 */}
                  <ModeCard
                    icon="volume"
                    title="导入已有磁盘"
                    desc="把已有的 qcow2 / vmdk 等磁盘上传并转换为模板。"
                    onClick={() => {
                      handleClose()
                      navigate('/import')
                    }}
                  />
                  <ModeCard
                    icon="docs"
                    title="导入 OVF / OVA 虚拟机包"
                    desc="从其它虚拟化平台导出的整机包导入，配置从包内解析。"
                    onClick={() => {
                      handleClose()
                      navigate('/import')
                    }}
                  />
                </div>
              </div>

              <Prerequisites form={form.data} />
            </div>
          )}

          {step > 0 && steps[step]?.key !== 'confirm' && (
            <div className="flex flex-col gap-4">
              {/* 名称与数量不在后端下发的矩阵里（它们是「这一次创建」的属性，
                  不是虚拟机的配置项），但仍归在「基础信息」这一步。 */}
              {steps[step]?.key === 'basic' && (
                <Section title="虚拟机名称">
                  <Input
                    label="名称"
                    value={name}
                    onChange={(e) => setName(e.target.value)}
                    placeholder="web-01"
                    hint={
                      count > 1
                        ? `批量创建 ${count} 台，实际名称为 ${name || 'web-01'}-1 … ${name || 'web-01'}-${count}`
                        : '1-63 位字母、数字或连字符，同一节点内不可重名'
                    }
                  />
                  <Input
                    label="数量"
                    type="number"
                    value={String(count)}
                    onChange={(e) => setCount(Math.max(1, Number(e.target.value) || 1))}
                    hint="一次最多 5 台：每台都要写入完整镜像，同时进行会让宿主机存储持续占满。"
                  />
                </Section>
              )}

              {steps[step]?.key === 'disk' && mode === 'iso' && (
                <div className="flex flex-col gap-1.5">
                  <label htmlFor="wizard-iso" className="text-sm font-medium text-ink-2">
                    安装镜像（可多选，首个为主安装盘）
                  </label>
                  {/* 多 ISO（G-29）：勾选列表而不是单选下拉——驱动盘与安装盘
                      分开存放是常见做法，多选让两步并成一步。首个勾选为主
                      安装盘，据此自动补全系统类型与最小磁盘。 */}
                  <div className="flex flex-col gap-1 rounded-control border border-line px-3 py-2">
                    {(form.data?.iso_files ?? []).map((f) => {
                      const idx = isoFileIDs.indexOf(f.id)
                      const on = idx >= 0
                      return (
                        <label
                          key={f.id}
                          className="flex cursor-pointer items-center gap-2 text-base text-ink"
                        >
                          <input
                            type="checkbox"
                            checked={on}
                            onChange={() => {
                              setISOFileIDs((prev) => {
                                if (on) return prev.filter((id) => id !== f.id)
                                return [...prev, f.id]
                              })
                              // 主安装盘（勾选后的第一个）带出系统类型与最小
                              // 磁盘：识别结果给用户一个不用查文档的起点。
                              const next = on
                                ? isoFileIDs.filter((id) => id !== f.id)
                                : [...isoFileIDs, f.id]
                              const primary = next[0]
                              const iso = (form.data?.iso_files ?? []).find((x) => x.id === primary)
                              if (iso?.os_type) setValueLinked('os_type', iso.os_type)
                              if (iso && iso.min_disk_gb > 0) {
                                setValue('disk_gb', Math.max(num(effective.disk_gb, 40), iso.min_disk_gb))
                              }
                            }}
                          />
                          <span>
                            {f.filename}
                            {idx === 0 && <span className="ml-1 text-sm text-brand">（主安装盘）</span>}
                            {f.min_disk_gb > 0 ? `· 至少 ${f.min_disk_gb} GB` : ''}
                          </span>
                        </label>
                      )
                    })}
                    {(form.data?.iso_files ?? []).length === 0 && (
                      <p className="text-sm text-ink-3">
                        该节点上还没有镜像，可先在「我的存储」上传，或在创建后手动挂载。
                      </p>
                    )}
                  </div>
                </div>
              )}

              {steps[step]?.key === 'disk' && (
                <FloppyPicker
                  nodeID={activeNodeID}
                  value={floppyFileID}
                  onChange={setFloppyFileID}
                />
              )}

              {steps[step]?.key === 'disk' && (
                <DataDiskEditor
                  items={dataDisks}
                  onChange={setDataDisks}
                  formatOptions={optionsOf('disk_format')}
                  busOptions={optionsOf('disk_bus')}
                />
              )}

              {mode === 'template' && steps[step]?.key === 'disk' && (
                <div className="flex flex-col gap-1.5">
                  <label htmlFor="wizard-tpl" className="text-sm font-medium text-ink-2">
                    模板
                  </label>
                  <select
                    id="wizard-tpl"
                    value={templateID}
                    onChange={(e) => pickTemplate(Number(e.target.value), templates.data)}
                    className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
                  >
                    <option value={0}>请选择模板</option>
                    {(templates.data ?? []).map((t) => (
                      <option key={t.id} value={t.id}>
                        {t.name}
                      </option>
                    ))}
                  </select>
                </div>
              )}

              {steps[step]?.key === 'network' && (
                <div className="flex flex-col gap-1 rounded-control border border-line px-3 py-2.5">
                  <label className="flex flex-col gap-1">
                    <span className="text-sm text-ink-2">网口数量</span>
                    <input
                      type="number"
                      min={1}
                      max={8}
                      value={nicCount}
                      onChange={(e) => setNicCount(Math.max(1, Math.min(8, Number(e.target.value) || 1)))}
                      className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
                    />
                  </label>
                  {/* 说明写清"含主网口"：只写"网口数量"会让人怀疑还要不要另建
                      一块主网卡。 */}
                  <span className="text-xs text-ink-3">
                    含主网口。多网卡机器（内网 + 公网）在这里一次建好，省得事后一块块补。
                  </span>
                </div>
              )}

              {steps[step]?.key === 'network' && (
                <div className="flex flex-col gap-3">
                  <div className="flex flex-col gap-1.5">
                    <label htmlFor="wizard-switch" className="text-sm font-medium text-ink-2">
                      主网口接入的网络
                    </label>
                    <select
                      id="wizard-switch"
                      value={effectiveSwitch}
                      onChange={(e) => setSwitchID(Number(e.target.value))}
                      className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
                    >
                      <option value={0}>不指定（使用节点默认网络）</option>
                      {(form.data?.switches ?? []).map((s) => (
                        <option key={s.id} value={s.id}>
                          {s.name}
                          {s.is_system ? '（系统）' : ''}
                        </option>
                      ))}
                    </select>
                  </div>
                  <div className="flex flex-col gap-1.5">
                    <span className="text-sm font-medium text-ink-2">安全组（可多选）</span>
                    {(form.data?.security_groups ?? []).length === 0 && (
                      <p className="text-sm text-ink-3">该节点上还没有安全组。</p>
                    )}
                    <div className="flex flex-wrap gap-2">
                      {(form.data?.security_groups ?? []).map((g) => {
                        const on = groupIDs.includes(g.id)
                        return (
                          <button
                            key={g.id}
                            type="button"
                            onClick={() =>
                              setGroupIDs((prev) =>
                                on ? prev.filter((x) => x !== g.id) : [...prev, g.id],
                              )
                            }
                            className={
                              'rounded-pill border px-3 py-1 text-sm ' +
                              (on
                                ? 'border-brand bg-brand/10 text-brand'
                                : 'border-line-strong text-ink-2 hover:border-brand/50')
                            }
                          >
                            {g.name}
                            {g.is_default ? '（默认）' : ''}
                          </button>
                        )
                      })}
                    </div>
                  </div>
                </div>
              )}

              {steps[step]?.key === 'advanced' && (
                <PassthroughPicker
                  nodeID={activeNodeID}
                  selected={pciAddresses}
                  onChange={setPciAddresses}
                />
              )}

              {sectionsOf(steps[step]?.key ?? '', fieldsOf(steps[step]?.key ?? '')).map((sec) => (
                <Section key={sec.title} title={sec.title}>
                  {sec.fields.map((f) => (
                    <Field
                      key={f.key}
                      field={f}
                      value={effective[f.key]}
                      onChange={(v) => setValueLinked(f.key, v)}
                    />
                  ))}
                </Section>
              ))}
            </div>
          )}

          {steps[step]?.key === 'confirm' && (
            <div className="flex flex-col gap-3">
              <Summary
                name={name}
                count={count}
                mode={mode}
                values={effective}
                dataDisks={dataDisks}
                switchName={(form.data?.switches ?? []).find((s) => s.id === effectiveSwitch)?.name}
                isoName={isoFileIDs
                  .map((id, i) => {
                    const f = (form.data?.iso_files ?? []).find((x) => x.id === id)
                    if (!f) return null
                    return i === 0 ? `${f.filename}（主安装盘）` : f.filename
                  })
                  .filter(Boolean)
                  .join('、') || undefined}
                groups={(form.data?.security_groups ?? [])
                  .filter((g) => groupIDs.includes(g.id))
                  .map((g) => g.name)}
              />
              {blocked.length > 0 && (
                <div className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
                  {blocked.map((p) => (
                    <p key={p.key}>{p.message || p.label + '未满足'}</p>
                  ))}
                </div>
              )}
              {missing.length > 0 && (
                <p className="text-sm text-warning">还需要填写：{missing.join('、')}</p>
              )}
            </div>
          )}

          {error && <p className="mt-3 text-base text-danger">{error}</p>}
          </div>
        </div>
      )}
    </Modal>
  )
}

/**
 * 步骤栏。桌面端竖排在左（配置项多的时候，横排胶囊会挤成两行且看不出进度），
 * 窄屏回落成顶部横向。
 *
 * 步骤本身由后端下发，因此这里只做呈现：不硬编码「共 9 步」。
 */
function StepRail({
  steps,
  current,
  onJump,
}: {
  steps: { key: string; label: string }[]
  current: number
  onJump: (i: number) => void
}) {
  return (
    <nav className="border-b border-line bg-sunken/60 px-3 py-3 md:w-[196px] md:shrink-0 md:border-b-0 md:border-r md:px-3 md:py-5">
      <ol className="flex gap-1.5 overflow-x-auto md:flex-col md:gap-0.5 md:overflow-visible">
        {steps.map((s, i) => {
          const done = i < current
          const active = i === current
          return (
            <li key={s.key} className="relative md:flex-1">
              <button
                type="button"
                onClick={() => onJump(i)}
                className={
                  'flex w-full items-center gap-2.5 whitespace-nowrap rounded-control px-2.5 py-2 text-left text-sm transition-colors ' +
                  (active
                    ? 'bg-brand/10 font-medium text-brand'
                    : done
                      ? 'text-ink-2 hover:bg-line/40'
                      : 'text-ink-3 hover:bg-line/40')
                }
              >
                <span
                  className={
                    'flex h-5 w-5 shrink-0 items-center justify-center rounded-full border text-[11px] ' +
                    (active
                      ? 'border-brand bg-brand text-white'
                      : done
                        ? 'border-brand/40 bg-brand/15 text-brand'
                        : 'border-line-strong text-ink-3')
                  }
                >
                  {done ? '✓' : i + 1}
                </span>
                <span className="md:truncate">{s.label}</span>
              </button>
            </li>
          )
        })}
      </ol>
    </nav>
  )
}

/** 每一步的标题与一句说明。后端只下发步骤名，这里补齐「这一步在配什么」。 */
const STEP_META: Record<string, { title: string; desc: string }> = {
  mode: { title: '选择创建方式', desc: '先定节点，再选一条最适合当前需求的途径。' },
  basic: { title: '基础信息', desc: '名称与数量——它们决定这批机器在列表里怎么被识别。' },
  hardware: { title: '硬件规格', desc: 'CPU、内存与虚拟化引擎参数。默认值可直接用。' },
  disk: { title: '存储介质', desc: '系统盘容量、格式、总线与 IO 限制。' },
  network: { title: '网络设置', desc: '网卡型号、接入的网络与安全组。' },
  boot: { title: '系统配置', desc: '操作系统、引导方式与首次开机的初始化。' },
  advanced: { title: '高级选项', desc: 'CPU 特性、设备型号与电源行为。不确定就保持默认。' },
  passthru: { title: '硬件直通', desc: '把宿主机的 PCI 设备整块交给这台虚拟机。' },
  confirm: { title: '确认信息', desc: '核对一遍再提交。创建是异步任务，提交后可在任务中心看进度。' },
}

function StepHeader({ stepKey, fallback }: { stepKey?: string; fallback: string }) {
  const meta = stepKey ? STEP_META[stepKey] : undefined
  return (
    <header className="mb-4">
      <h3 className="text-md font-semibold text-ink">{meta?.title ?? fallback}</h3>
      {meta?.desc && <p className="mt-1 text-sm text-ink-3">{meta.desc}</p>}
    </header>
  )
}

/** 一步之内的内容分区。同一件事的字段放在一张卡片里，避免一长条平铺。 */
function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="rounded-card border border-line bg-raised px-4 py-3.5">
      <h4 className="mb-3 text-sm font-medium text-ink-2">{title}</h4>
      <div className="flex flex-col gap-3.5">{children}</div>
    </section>
  )
}

/**
 * 一步之内的二级分区。
 *
 * 后端只下发到「步骤」这一层（9 步），但一步里有十几个字段时必须再分层，
 * 否则用户面对的是没有结构的一长列。这里按字段 key 归组，**未命中的字段
 * 落进「其他」**——后端将来加了字段，它照样会出现，只是没有专属标题。
 */
const FIELD_SECTIONS: Record<string, { title: string; keys: string[] }[]> = {
  basic: [{ title: '分组与备注', keys: ['group_name', 'remark'] }],
  hardware: [
    {
      title: 'CPU 与内存',
      keys: ['vcpu', 'memory_mb', 'cpu_sockets', 'cpu_cores', 'cpu_threads', 'cpu_hotplug', 'memory_hotplug'],
    },
    { title: '架构', keys: ['arch'] },
  ],
  disk: [
    { title: '系统盘', keys: ['disk_gb', 'disk_format', 'disk_bus'] },
    { title: 'IOPS 限制', keys: ['disk_iops_total', 'disk_iops_read', 'disk_iops_write'] },
    { title: '吞吐限制', keys: ['disk_bytes_total', 'disk_bytes_read', 'disk_bytes_write'] },
  ],
  network: [{ title: '网卡与地址', keys: ['nic_model', 'static_ip'] }],
  boot: [
    {
      title: '操作系统与引导',
      keys: ['os_type', 'os_variant', 'machine_type', 'firmware', 'secure_boot', 'boot_order'],
    },
    { title: '首次启动', keys: ['hostname', 'initial_password', 'init_mode', 'auto_start', 'watchdog'] },
  ],
  advanced: [
    {
      title: 'CPU 特性',
      keys: ['cpu_type', 'hide_kvm', 'nested_virt', 'cpu_affinity', 'cpu_limit_percent', 'apic', 'pae'],
    },
    { title: '设备与电源', keys: ['video_model', 'rtc_mode', 'freeze_on_start'] },
  ],
}

function sectionsOf(group: string, fields: CreateFormField[]): { title: string; fields: CreateFormField[] }[] {
  const defs = FIELD_SECTIONS[group]
  if (!defs) return fields.length > 0 ? [{ title: '配置项', fields }] : []
  const used = new Set<string>()
  const out = defs
    .map((d) => {
      const fs = fields.filter((f) => d.keys.includes(f.key))
      fs.forEach((f) => used.add(f.key))
      return { title: d.title, fields: fs }
    })
    .filter((s) => s.fields.length > 0)
  const rest = fields.filter((f) => !used.has(f.key))
  if (rest.length > 0) out.push({ title: '其他', fields: rest })
  return out
}

function ModeCard({
  icon,
  title,
  desc,
  active,
  disabled,
  onClick,
}: {
  icon: IconName
  title: string
  desc: string
  active?: boolean
  disabled?: boolean
  onClick?: () => void
}) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      className={
        'group flex flex-col items-start gap-2 rounded-card border px-4 py-4 text-left transition-all ' +
        (disabled
          ? 'cursor-not-allowed border-line bg-sunken opacity-60'
          : active
            ? 'border-brand bg-brand/5 shadow-2'
            : 'border-line-strong bg-raised hover:-translate-y-0.5 hover:border-brand/60 hover:shadow-2')
      }
    >
      <span
        className={
          'flex h-8 w-8 items-center justify-center rounded-control ' +
          (active ? 'bg-brand/15 text-brand' : 'bg-sunken text-ink-3 group-hover:text-brand')
        }
      >
        <Icon name={icon} className="h-4 w-4" />
      </span>
      <span className="text-base font-medium text-ink">
        {title}
        {disabled && <span className="ml-2 text-xs text-ink-3">尚未提供</span>}
      </span>
      <span className="text-sm leading-relaxed text-ink-3">{desc}</span>
    </button>
  )
}

/** 前置条件。不满足时给出可执行的修复入口，而不是一句「不可用」。 */
function Prerequisites({ form }: { form?: { prerequisites: { key: string; label: string; ok: boolean; message?: string; link?: string }[] } }) {
  const items = form?.prerequisites ?? []
  if (items.length === 0) return null
  return (
    <div className="flex flex-col gap-1 rounded-control border border-line bg-sunken px-3 py-2">
      {items.map((p) => (
        <p key={p.key} className="text-sm">
          <span className={p.ok ? 'text-success' : 'text-danger'}>{p.ok ? '✓' : '✕'}</span>
          <span className="ml-1.5 text-ink-2">{p.label}</span>
          {!p.ok && p.message && (
            <>
              <span className="ml-1.5 text-ink-3">— {p.message}</span>
              {p.link && (
                <Link to={p.link} className="ml-1.5 text-brand hover:underline">
                  去处理
                </Link>
              )}
            </>
          )}
        </p>
      ))}
    </div>
  )
}

/** 单个配置项。控件类型与可选值都来自后端下发的矩阵。 */
function Field({
  field,
  value,
  onChange,
}: {
  field: CreateFormField
  value: unknown
  onChange: (v: unknown) => void
}) {
  if (field.kind === 'boolean') {
    return (
      <label className="flex items-start gap-2">
        <input
          type="checkbox"
          className="mt-1"
          checked={value === true}
          onChange={(e) => onChange(e.target.checked)}
        />
        <span>
          <span className="text-base text-ink-2">{field.label}</span>
          {field.hint && <span className="block text-sm text-ink-3">{field.hint}</span>}
        </span>
      </label>
    )
  }

  if (field.kind === 'select') {
    return (
      <div className="flex flex-col gap-1.5">
        <label className="text-sm font-medium text-ink-2" htmlFor={`f-${field.key}`}>
          {field.label}
        </label>
        <select
          id={`f-${field.key}`}
          value={String(value ?? '')}
          onChange={(e) => onChange(e.target.value)}
          className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
        >
          {(field.options ?? []).map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        {field.hint && <p className="text-sm text-ink-3">{field.hint}</p>}
      </div>
    )
  }

  return (
    <Input
      label={field.label}
      type={field.kind === 'number' ? 'number' : 'text'}
      value={String(value ?? '')}
      onChange={(e) =>
        onChange(field.kind === 'number' ? Number(e.target.value) || 0 : e.target.value)
      }
      hint={field.hint}
    />
  )
}

function Summary({
  name,
  count,
  mode,
  values,
  switchName,
  isoName,
  groups,
  dataDisks,
}: {
  name: string
  count: number
  mode: CreateMode
  values: Record<string, unknown>
  switchName?: string
  isoName?: string
  groups: string[]
  dataDisks: DataDiskInput[]
}) {
  const rows: [string, string][] = [
    ['名称', count > 1 ? `${name}-1 … ${name}-${count}（${count} 台）` : name],
    ['创建方式', mode === 'iso' ? 'ISO 安装' : '模板克隆'],
    ['规格', `${values.vcpu ?? 2} 核 · ${values.memory_mb ?? 2048} MB · ${values.disk_gb ?? 40} GB`],
    ['磁盘', `${values.disk_format ?? 'qcow2'} · ${values.disk_bus ?? 'virtio'}`],
    ['系统', `${values.os_type ?? 'linux'} · ${values.machine_type ?? 'q35'} · ${values.firmware ?? 'bios'}`],
    ['网络', `${switchName ?? '节点默认网络'} · ${values.nic_model ?? 'virtio'}`],
  ]
  if (mode === 'iso') rows.push(['安装镜像', isoName ?? '未选择'])
  if (groups.length > 0) rows.push(['安全组', groups.join('、')])
  // 数据盘与"第一次开机"的配置一并列出：它们是最容易在提交后才被发现
  // 填错了的几项（尤其是静态地址——填错表现为开机后没有网络）。
  if (dataDisks.length > 0) {
    rows.push([
      '数据盘',
      dataDisks.map((d) => `${d.size_gb} GB${d.bus ? ` · ${d.bus}` : ''}`).join('、'),
    ])
  }
  if (values.hostname) rows.push(['主机名', String(values.hostname)])
  if (values.static_ip) rows.push(['静态地址', String(values.static_ip)])
  if (values.init_mode && values.init_mode !== 'none') {
    rows.push(['首次启动初始化', String(values.init_mode)])
  }

  return (
    <div className="rounded-card border border-line">
      <table className="w-full border-collapse text-base">
        <tbody>
          {rows.map(([k, v]) => (
            <tr key={k} className="border-t border-line first:border-t-0">
              <th className="w-28 px-4 py-2 text-left font-medium text-ink-3">{k}</th>
              <td className="px-4 py-2 text-ink">{v}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function num(v: unknown, fallback: number): number {
  const n = Number(v)
  return Number.isFinite(n) ? n : fallback
}

function str(v: unknown): string | undefined {
  const s = String(v ?? '').trim()
  return s === '' ? undefined : s
}

function bool(v: unknown): boolean | undefined {
  return typeof v === 'boolean' ? v : undefined
}

/** newToken 生成幂等键。 */
function newToken(): string {
  if (typeof crypto !== 'undefined' && 'randomUUID' in crypto) return crypto.randomUUID()
  return `t-${Date.now()}-${Math.random().toString(36).slice(2)}`
}

/**
 * withoutSensitive 去掉草稿里不该落盘的值。
 *
 * 凭据不进 localStorage：它是一个不出门却能长期存在的泄漏面——用户关掉
 * 浏览器之后它还在，而任何能读到本地存储的脚本都能拿到。
 */
function withoutSensitive(values: Record<string, unknown>): Record<string, unknown> {
  const next = { ...values }
  delete next.initial_password
  return next
}

/**
 * DataDiskEditor 编辑"除系统盘之外还要建几块盘"。
 *
 * 它不在矩阵里（数据盘是一组结构且数量不定），但**格式与驱动的可选值复用
 * 矩阵**：与系统盘是同一套候选，抄一份之后新增格式时数据盘那边会静默拒绝它。
 */
function DataDiskEditor({
  items,
  onChange,
  formatOptions,
  busOptions,
}: {
  items: DataDiskInput[]
  onChange: (items: DataDiskInput[]) => void
  formatOptions: { value: string; label: string }[]
  busOptions: { value: string; label: string }[]
  }) {
  return (
    <div className="flex flex-col gap-2 rounded-control border border-line px-3 py-2.5">
      <div className="flex items-center justify-between gap-2">
        <span className="text-base text-ink">数据盘（可选）</span>
        <Button
          variant="secondary"
          size="sm"
          disabled={items.length >= 4}
          title={items.length >= 4 ? '一次最多附带 4 块' : undefined}
          onClick={() => onChange([...items, { size_gb: 20 }])}
        >
          + 添加
        </Button>
      </div>

      {items.length === 0 ? (
        <p className="text-sm text-ink-3">不附加数据盘。建好之后可在详情页的「磁盘」里逐块挂载。</p>
      ) : (
        <ul className="flex flex-col gap-2">
          {items.map((d, i) => (
            <li key={i} className="flex flex-wrap items-end gap-2">
              <Input
                label={`第 ${i + 1} 块大小（GB）`}
                type="number"
                value={String(d.size_gb)}
                onChange={(e) =>
                  onChange(
                    items.map((x, j) =>
                      j === i ? { ...x, size_gb: Math.max(1, Number(e.target.value) || 1) } : x,
                    ),
                  )
                }
              />
              <div className="flex flex-col gap-1.5">
                <label className="text-sm font-medium text-ink-2">格式</label>
                <select
                  value={d.format ?? ''}
                  onChange={(e) =>
                    onChange(items.map((x, j) => (j === i ? { ...x, format: e.target.value || undefined } : x)))
                  }
                  className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
                >
                  <option value="">跟随系统盘</option>
                  {formatOptions.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
              </div>
              <div className="flex flex-col gap-1.5">
                <label className="text-sm font-medium text-ink-2">驱动</label>
                <select
                  value={d.bus ?? ''}
                  onChange={(e) =>
                    onChange(items.map((x, j) => (j === i ? { ...x, bus: e.target.value || undefined } : x)))
                  }
                  className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
                >
                  <option value="">跟随系统盘</option>
                  {busOptions.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
              </div>
              <Button
                variant="ghost"
                size="sm"
                onClick={() => onChange(items.filter((_, j) => j !== i))}
              >
                移除
              </Button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

/**
 * PassthroughPicker 选创建时要一并直通的 PCI 设备。
 *
 * 只列出**可直通**的设备，并且把同组其它设备展示出来：IOMMU 分组里的设备
 * 只能一起直通，用户选了一块卡就必须知道另一块也被带上了——否则他会在另一台
 * 机器上看到"这块卡怎么也挂不上"。
 */
function PassthroughPicker({
  nodeID,
  selected,
  onChange,
}: {
  nodeID: number
  selected: string[]
  onChange: (addrs: string[]) => void
}) {
  const list = useQuery({
    queryKey: ['passthrough', nodeID],
    queryFn: () => passthroughApi.overview(nodeID),
    enabled: nodeID > 0,
  })

  const devices = (list.data?.devices ?? []).filter((d) => d.CanPassthrough)

  return (
    <div className="flex flex-col gap-1.5 rounded-control border border-line px-3 py-2.5">
      <span className="text-sm text-ink-2">直通设备（可选）</span>
      {devices.length === 0 ? (
        <span className="text-xs text-ink-3">该节点没有可直通的设备。</span>
      ) : (
        <ul className="flex flex-col gap-1">
          {devices.map((d: PCIDevice) => (
            <li key={d.Address} className="flex items-start gap-2">
              <input
                type="checkbox"
                className="mt-1"
                // 已被别的机器占用的不给勾：勾了也会在启动时失败，而那时两台
                // 机器都已经跑起来了。
                disabled={!!d.attached_to_vm_id}
                checked={selected.includes(d.Address)}
                onChange={(e) =>
                  onChange(
                    e.target.checked
                      ? [...selected, d.Address]
                      : selected.filter((a) => a !== d.Address),
                  )
                }
              />
              <span className="min-w-0">
                <span className="kc-mono text-sm text-ink">{d.Address}</span>
                <span className="ml-2 text-sm text-ink-2">{d.Description || d.VendorDevice || '未识别设备'}</span>
                {d.attached_to_vm_id ? (
                  <span className="ml-2 text-xs text-warning">已被 {d.attached_to_vm_name || '其它虚拟机'} 挂载</span>
                ) : null}
                {d.group_peers.length > 0 && (
                  <span className="block text-xs text-ink-3">
                    同组设备（会一起直通）：{d.group_peers.join('、')}
                  </span>
                )}
              </span>
            </li>
          ))}
        </ul>
      )}
      <span className="text-xs text-ink-3">
        直通设备不支持热插拔；创建时机器是关着的，正是挂载它们的时机。
      </span>
    </div>
  )
}

/**
 * FloppyPicker 选软盘镜像。
 *
 * 软盘是**独立于光驱**的设备，因此它不复用光驱那一步的选择，而是单独一项。
 * 候选来自「我的存储」而不是让用户填路径——路径是节点内部细节，让他去猜
 * 等于没给入口。
 */
function FloppyPicker({
  nodeID,
  value,
  onChange,
}: {
  nodeID: number
  value: number
  onChange: (id: number) => void
}) {
  const list = useQuery({
    queryKey: ['storage-files', nodeID],
    queryFn: () => userStorageApi.listFiles(nodeID),
    enabled: nodeID > 0,
  })

  const files = list.data?.items ?? []

  return (
    <div className="flex flex-col gap-1 rounded-control border border-line px-3 py-2.5">
      <label className="flex flex-col gap-1">
        <span className="text-sm text-ink-2">软盘镜像（可选）</span>
        <select
          value={value}
          onChange={(e) => onChange(Number(e.target.value) || 0)}
          className="h-9 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
        >
          <option value={0}>不挂载</option>
          {files.map((f: FileView) => (
            <option key={f.id} value={f.id}>
              {f.filename}
            </option>
          ))}
        </select>
      </label>
      <span className="text-xs text-ink-3">
        老系统的驱动盘或安装流程有时仍走软盘。它与光驱是两个不同的设备。
      </span>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '创建失败，请稍后重试'
}
