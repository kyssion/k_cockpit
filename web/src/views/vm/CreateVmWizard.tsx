import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router'

import { ApiError, NetworkError } from '@/api/client'
import { nodeApi } from '@/api/node'
import { templateApi, type TemplateView } from '@/api/template'
import { vmApi, type CreateFormField, type DataDiskInput } from '@/api/vm'
import { Button } from '@/components/common/Button'
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
  isoFileID: number
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
  const [isoFileID, setISOFileID] = useState(draft?.isoFileID ?? 0)
  const [switchID, setSwitchID] = useState(draft?.switchID ?? 0)
  const [groupIDs, setGroupIDs] = useState<number[]>(draft?.groupIDs ?? [])
  const [count, setCount] = useState(draft?.count ?? 1)
  // 数据盘不在矩阵里（它是一组结构且数量不定），因此单独存一份 state。
  const [dataDisks, setDataDisks] = useState<DataDiskInput[]>(draft?.dataDisks ?? [])
  const [templateID, setTemplateID] = useState(0)
  const [error, setError] = useState('')
  const [submitted, setSubmitted] = useState<number[] | null>(null)

  // 幂等键在**打开向导时**生成一次：它是「这一次提交」的身份。每次渲染
  // 重新生成等于没有幂等，而放在提交时生成则挡不住按钮的连点。
  const [clientToken] = useState(() => newToken())

  const nodes = useQuery({ queryKey: ['nodes'], queryFn: nodeApi.list, enabled: open })
  const form = useQuery({
    queryKey: ['vm-create-form', nodeID],
    queryFn: () => vmApi.createForm(nodeID),
    enabled: open && nodeID > 0,
  })
  const templates = useQuery({
    queryKey: ['templates', { node_id: nodeID, only_ready: true }],
    queryFn: () => templateApi.list({ node_id: nodeID, only_ready: true }),
    enabled: open && nodeID > 0 && mode === 'template',
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
      isoFileID, switchID, groupIDs, count, dataDisks,
      savedAt: Date.now(),
    }
    localStorage.setItem(DRAFT_KEY, JSON.stringify(draft))
  }, [open, submitted, name, nodeID, mode, values, isoFileID, switchID, groupIDs, count, dataDisks])

  const steps = useMemo(() => {
    const backend = (form.data?.groups ?? []).filter((g) => !g.planned)
    return [{ key: 'mode', label: '创建方式' }, ...backend, { key: 'confirm', label: '确认信息' }]
  }, [form.data])

  const create = useMutation({
    mutationFn: () =>
      vmApi.create({
        name: name.trim(),
        node_id: nodeID,
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

        iso_file_id: mode === 'iso' && isoFileID > 0 ? isoFileID : undefined,
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
  if (nodeID <= 0) missing.push('节点')
  if (mode === 'iso' && isoFileID <= 0) missing.push('安装镜像')
  if (mode === 'template' && templateID <= 0) missing.push('模板')
  const blocked = (form.data?.prerequisites ?? []).filter((p) => !p.ok)

  return (
    <Modal
      open={open}
      title={submitted ? '创建任务已提交' : '创建虚拟机'}
      description={
        submitted
          ? '任务正在执行，完成后虚拟机才会出现在列表中。'
          : '按步骤填写配置。创建是异步操作，提交后可在任务中心查看进度。'
      }
      onClose={handleClose}
    >
      {submitted ? (
        <div className="flex flex-col gap-3">
          <p className="text-base text-ink-2">
            共提交 {submitted.length} 个任务：{submitted.map((id) => `#${id}`).join('、')}
          </p>
          <div className="flex gap-2">
            <Link to="/task">
              <Button size="sm">查看任务</Button>
            </Link>
            <Button variant="secondary" size="sm" onClick={handleClose}>
              留在本页
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-4">
          <StepBar steps={steps} current={step} onJump={setStep} />

          {step === 0 && (
            <div className="flex flex-col gap-3">
              <ModeCard
                active={mode === 'iso'}
                title="ISO 安装"
                desc="挂上镜像从零安装系统。适用于需要自定义分区与安装选项的场景。"
                onClick={() => setMode('iso')}
              />
              <ModeCard
                active={mode === 'template'}
                title="模板克隆"
                desc="从已制备好的模板复制一台，开机即用。最快的方式。"
                onClick={() => setMode('template')}
              />
              <ModeCard
                disabled
                title="导入已有磁盘"
                desc="把已有的 qcow2 / vmdk 等磁盘作为系统盘导入，见「导入」页面。"
              />
              <ModeCard
                disabled
                title="导入 OVF / OVA 虚拟机包"
                desc="从其它虚拟化平台导出的整机包导入，尚未实现。"
              />

              <Prerequisites form={form.data} />
            </div>
          )}

          {step > 0 && steps[step]?.key !== 'confirm' && (
            <div className="flex flex-col gap-3">
              {step === 1 && (
                <>
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
                  <div className="flex flex-col gap-1.5">
                    <label htmlFor="wizard-node" className="text-sm font-medium text-ink-2">
                      节点
                    </label>
                    <select
                      id="wizard-node"
                      value={nodeID}
                      onChange={(e) => setNodeID(Number(e.target.value))}
                      className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
                    >
                      <option value={0}>请选择节点</option>
                      {(nodes.data ?? []).map((n) => (
                        <option key={n.id} value={n.id}>
                          {n.name}
                          {n.status !== 'online' ? '（离线）' : ''}
                        </option>
                      ))}
                    </select>
                  </div>
                  <Input
                    label="数量"
                    type="number"
                    value={String(count)}
                    onChange={(e) => setCount(Math.max(1, Number(e.target.value) || 1))}
                    hint="一次最多 5 台：每台都要写入完整镜像，同时进行会让宿主机存储持续占满。"
                  />
                </>
              )}

              {steps[step]?.key === 'disk' && mode === 'iso' && (
                <div className="flex flex-col gap-1.5">
                  <label htmlFor="wizard-iso" className="text-sm font-medium text-ink-2">
                    安装镜像
                  </label>
                  <select
                    id="wizard-iso"
                    value={isoFileID}
                    onChange={(e) => {
                      const id = Number(e.target.value)
                      setISOFileID(id)
                      const iso = (form.data?.iso_files ?? []).find((f) => f.id === id)
                      // 镜像带出系统类型与最小磁盘：识别结果给用户一个不用
                      // 查文档的起点，而不是必须照做的约束。
                      if (iso?.os_type) setValue('os_type', iso.os_type)
                      if (iso && iso.min_disk_gb > 0) {
                        setValue('disk_gb', Math.max(num(effective.disk_gb, 40), iso.min_disk_gb))
                      }
                    }}
                    className="h-9 rounded-control border border-line-strong bg-sunken px-3 text-base text-ink focus:outline-none focus-visible:border-brand"
                  >
                    <option value={0}>不挂载镜像（之后手动挂载）</option>
                    {(form.data?.iso_files ?? []).map((f) => (
                      <option key={f.id} value={f.id}>
                        {f.filename}
                        {f.min_disk_gb > 0 ? `（至少 ${f.min_disk_gb} GB）` : ''}
                      </option>
                    ))}
                  </select>
                  {(form.data?.iso_files ?? []).length === 0 && (
                    <p className="text-sm text-ink-3">
                      该节点上还没有镜像，可先在「我的存储」上传，或在创建后手动挂载。
                    </p>
                  )}
                </div>
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

              {fieldsOf(steps[step]?.key ?? '').map((f) => (
                <Field
                  key={f.key}
                  field={f}
                  value={effective[f.key]}
                  onChange={(v) => setValue(f.key, v)}
                />
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
                isoName={(form.data?.iso_files ?? []).find((f) => f.id === isoFileID)?.filename}
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

          {error && <p className="text-base text-danger">{error}</p>}

          <div className="flex items-center justify-between gap-2 border-t border-line pt-3">
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
      )}
    </Modal>
  )
}

/** 步骤条。步骤由后端下发，因此这里不硬编码「共 9 步」。 */
function StepBar({
  steps,
  current,
  onJump,
}: {
  steps: { key: string; label: string }[]
  current: number
  onJump: (i: number) => void
}) {
  return (
    <ol className="flex flex-wrap gap-1.5">
      {steps.map((s, i) => (
        <li key={s.key}>
          <button
            type="button"
            onClick={() => onJump(i)}
            className={
              'rounded-pill border px-2.5 py-1 text-xs ' +
              (i === current
                ? 'border-brand bg-brand/10 text-brand'
                : i < current
                  ? 'border-line-strong text-ink-2'
                  : 'border-line text-ink-3')
            }
          >
            {i + 1}. {s.label}
          </button>
        </li>
      ))}
    </ol>
  )
}

function ModeCard({
  title,
  desc,
  active,
  disabled,
  onClick,
}: {
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
        'rounded-card border px-4 py-3 text-left ' +
        (disabled
          ? 'cursor-not-allowed border-line bg-sunken opacity-60'
          : active
            ? 'border-brand bg-brand/5'
            : 'border-line-strong hover:border-brand/50')
      }
    >
      <p className="text-md font-medium text-ink">
        {title}
        {disabled && <span className="ml-2 text-xs text-ink-3">尚未提供</span>}
      </p>
      <p className="mt-0.5 text-base text-ink-3">{desc}</p>
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

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '创建失败，请稍后重试'
}
