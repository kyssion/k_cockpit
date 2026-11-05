import { useMutation, useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import {
  editApi,
  type EditField,
  type EditForm,
  type EditGroupInfo,
} from '@/api/edit'
import type { VmStatus } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { ErrorBox } from '@/components/common/ErrorBox'
import { PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'
import { describe } from '@/utils/describe'
import { VM_STATUS_LABEL } from '@/utils/labels'

/** EditTab 编辑虚拟机配置（F-2-05）。 */
export function EditTab({ vmID, onSaved }: { vmID: number; onSaved: () => void }) {
  const [group, setGroup] = useState('basic')
  const [draft, setDraft] = useState<Record<string, string> | null>(null)
  const [notice, setNotice] = useState('')
  const [error, setError] = useState('')

  const form = useQuery({
    queryKey: ['vm-edit-form', vmID],
    queryFn: () => editApi.form(vmID),
  })

  // 表单在**首次渲染时**从接口数据派生一次，之后由本地状态接管（f-2-01 R-012）。
  //
  // 用「渲染时派生」而不是 useEffect + setState：后者表达不出「只取第一次」
  // 这个意图——后台每次刷新都会重新触发 effect，把用户正在输入的内容冲回
  // 原值，而用户看到的是「我打的字自己消失了」，最难排查的一类问题。
  const currentDraft = draft ?? (form.data ? buildDraft(form.data.values) : null)

  const diff = form.data && currentDraft ? computeChanges(form.data, currentDraft) : null
  const dirty = diff !== null && diff.keys.length > 0

  const save = useMutation({
    mutationFn: async () => {
      if (!diff) return
      // 两类修改走不同接口：元数据同步生效，其余配置入队执行。
      // 顺序上先元数据后配置——元数据几乎不会失败，先把它落下来，
      // 万一配置提交失败，用户至少不用重填备注。
      if (diff.hasMetadata) {
        await editApi.updateMetadata(vmID, diff.metadata)
      }
      if (diff.hasConfig) {
        return editApi.updateConfig(vmID, diff.config)
      }
      return undefined
    },
    onSuccess: (result) => {
      setError('')
      setDraft(null) // 重新冻结一次，拿到刚保存后的值
      void form.refetch()
      onSaved()
      setNotice(result ? `已提交配置变更，任务 #${result.task_id} 正在执行` : '已保存')
    },
    onError: (err) => setError(describe(err)),
  })

  if (form.isPending || currentDraft === null) return <PageLoading />
  if (form.isError) return <ErrorBox message={describe(form.error)} />

  const data = form.data
  // 兜底：后端总会下发 groups，但一个空列表不该让整页崩掉。
  const active: EditGroupInfo =
    data.groups.find((g) => g.key === group) ??
    data.groups[0] ?? { key: 'basic', label: '基础配置', planned: false }
  const fields = data.fields.filter((f) => f.group === active.key)
  // 需要关机才能改的项，在当前运行态下不可提交。
  const blockedByStatus = !data.editable_now

  return (
    <div className="flex flex-col gap-4">
      {notice && (
        <p className="rounded-control bg-success/10 px-3 py-2 text-base text-success">{notice}</p>
      )}
      {error && <ErrorBox message={error} />}

      {/* 子选项卡由后端下发：新增一个只需要改后端矩阵一处，界面自动跟上。 */}
      <div className="flex flex-wrap gap-1 border-b border-line">
        {data.groups.map((g) => (
          <button
            key={g.key}
            onClick={() => setGroup(g.key)}
            className={[
              'rounded-t-control px-3.5 py-2 text-base transition-colors',
              g.key === active.key
                ? 'border-b-2 border-brand font-medium text-brand'
                : 'border-b-2 border-transparent text-ink-3 hover:text-ink',
            ].join(' ')}
          >
            {g.label}
          </button>
        ))}
      </div>

      {active.planned ? (
        <section className="rounded-card border border-line bg-raised px-4 py-5">
          <p className="text-base font-medium text-ink">{active.label}</p>
          <p className="mt-1.5 text-base text-ink-2">{active.note}</p>
          <p className="mt-3 text-sm text-ink-3">对应需求 F-2-05 / F-2-06。</p>
        </section>
      ) : (
        <section className="rounded-card border border-line">
          <div className="flex items-center justify-between border-b border-line px-4 py-2.5">
            <h2 className="text-sm font-medium text-ink-2">{active.label}</h2>
            <div className="flex items-center gap-3">
              {dirty && <span className="text-sm text-warning">有未保存的修改</span>}
              <Button
                size="sm"
                disabled={!dirty}
                loading={save.isPending}
                onClick={() => save.mutate()}
              >
                保存
              </Button>
            </div>
          </div>

          <div className="flex flex-col gap-4 px-4 py-3.5">
            {fields.length === 0 && (
              <p className="text-base text-ink-3">这个选项卡暂时没有可编辑的项。</p>
            )}
            {fields.map((f) => {
              // 运行态下，需要关机的项禁用输入——但**仍然显示**：
              // 直接隐藏会让用户在关机之后再进来才发现多出几项。
              // 例外（G-33）：热添加开关打开的机器，vCPU / 内存运行中可增。
              const hotOK = data.hot_addition?.[f.key] === true
              const blocked =
                (f.requires_shutdown && blockedByStatus && !hotOK) ||
                f.read_only ||
                f.not_applied
              const statusLabel =
                VM_STATUS_LABEL[data.current_status as VmStatus] ?? data.current_status

              // 生效方式与提示合成一句：用户读的是「这一项改了会怎样」，
              // 拆成徽标 + 提示两处反而要来回对照。
              const mode = f.read_only
                ? '由节点上报，不可修改'
                : f.not_applied
                  ? '当前版本不支持在编辑页修改'
                  : f.requires_shutdown
                    ? '需关机后修改'
                    : '即时生效，不影响运行'
              const hint = f.read_only
                ? mode
                : f.not_applied
                  ? (f.not_applied_reason ?? mode)
                  : f.requires_shutdown && hotOK
                    ? `当前为${statusLabel}，已开启热添加：可增加，不能减少`
                    : f.requires_shutdown && blockedByStatus
                      ? `当前为${statusLabel}，需关机后才能修改`
                      : [mode, f.hint].filter(Boolean).join(' · ')

              return (
                <FieldControl
                  key={f.key}
                  field={f}
                  value={currentDraft[f.key] ?? ''}
                  disabled={blocked || save.isPending}
                  hint={hint}
                  onChange={(v) => setDraft({ ...currentDraft, [f.key]: v })}
                />
              )
            })}
          </div>
        </section>
      )}

      <p className="rounded-card border border-line bg-raised px-4 py-3 text-sm text-ink-3">
        「即时生效」的项只记录在控制面，虚拟机运行中也能改；「需关机」的项要下发到
        节点，关闭电源后才能提交。
        <br />
        保存时**只提交改动过的字段**：等值提交会让一次「改内存」顺带触发一次没有
        理由的重启。
      </p>
    </div>
  )
}

/**
 * FieldControl 按矩阵声明的类型渲染控件。
 *
 * 分支集中在这一处，而不是散在每个字段的渲染里：新增一种控件类型时只需要
 * 在这里加一个 case，其余代码（校验、差异、提交）都不用动。
 */
function FieldControl({
  field,
  value,
  disabled,
  hint,
  onChange,
}: {
  field: EditField
  value: string
  disabled: boolean
  hint: string
  onChange: (v: string) => void
}) {
  if (field.kind === 'boolean') {
    return (
      <label className="flex cursor-pointer items-start gap-2.5">
        <input
          type="checkbox"
          className="mt-0.5"
          checked={value === 'true'}
          disabled={disabled}
          onChange={(e) => onChange(e.target.checked ? 'true' : 'false')}
        />
        <span>
          <span className="block text-base text-ink">{field.label}</span>
          <span className={disabled ? 'block text-sm text-warning' : 'block text-sm text-ink-3'}>
            {hint}
          </span>
        </span>
      </label>
    )
  }

  if (field.kind === 'select') {
    return (
      <label className="flex flex-col gap-1">
        <span className="text-base text-ink">{field.label}</span>
        <select
          className={[
            'rounded-control border border-line-strong bg-surface px-2.5 py-2 text-base text-ink',
            disabled ? 'cursor-not-allowed opacity-60' : '',
          ].join(' ')}
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        >
          {/* 枚举值来自后端下发的 options，前端不硬编码——
              否则后端新增一个取值时，界面会显示成空白选项。 */}
          {(field.options ?? []).map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <span className={disabled ? 'text-sm text-warning' : 'text-sm text-ink-3'}>{hint}</span>
      </label>
    )
  }

  return (
    <Input
      label={field.label}
      type={field.kind === 'number' ? 'number' : 'text'}
      value={value}
      min={field.min}
      max={field.max}
      disabled={disabled}
      hint={hint}
      onChange={(e) => onChange(e.target.value)}
    />
  )
}

/** buildDraft 把当前值转成表单可编辑的字符串。 */
function buildDraft(values: Record<string, unknown>): Record<string, string> {
  const out: Record<string, string> = {}
  for (const [k, v] of Object.entries(values)) {
    out[k] = v === null || v === undefined ? '' : String(v)
  }
  return out
}

/**
 * computeChanges 算出**真正变化**的字段，并按「是否需要下发」分成两组。
 *
 * 只提交差异而不是整表：整表提交会让一次「改内存」顺带把没动过的 CPU 一起
 * 上报，在需要重启的项上就会触发一次没有理由的重启。
 */
function computeChanges(form: EditForm, draft: Record<string, string>) {
  // 元数据走专门的接口：它同步生效、不入队，与需要下发的配置不是一回事。
  const metadata: { remark?: string; group_name?: string } = {}
  const config: Record<string, unknown> = {}
  const keys: string[] = []
  let hasMetadata = false
  let hasConfig = false

  for (const f of form.fields) {
    // 节点侧未实现的项不参与差异计算：即使输入框被绕过改了值，
    // 也不该把它提交上去换回一个 422。
    if (f.read_only || f.not_applied) continue

    const before = toStr(form.values[f.key])
    const after = draft[f.key] ?? ''
    if (before === after) continue

    keys.push(f.key)

    if (!f.requires_node) {
      // 目前只有两项纯元数据，它们有各自的接口。
      if (f.key === 'remark') metadata.remark = after
      else if (f.key === 'group_name') metadata.group_name = after
      else continue
      hasMetadata = true
      continue
    }

    switch (f.kind) {
      case 'number': {
        const n = Number(after)
        if (!Number.isFinite(n)) continue
        config[f.key] = n
        break
      }
      case 'boolean':
        config[f.key] = after === 'true'
        break
      default:
        config[f.key] = after
    }
    hasConfig = true
  }

  return { metadata, config, hasMetadata, hasConfig, keys }
}

function toStr(v: unknown): string {
  return v === null || v === undefined ? '' : String(v)
}
