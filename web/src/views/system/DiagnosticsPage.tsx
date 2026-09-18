/**
 * DiagnosticsPage 导出诊断包（F-9-03）。
 *
 * 页面上要回答的一个问题：**用户凭什么敢直接把这个包发出去。**
 *
 * 答案必须写在最显眼处——敏感项（密钥、密码、令牌）在生成时就被打码，
 * 包里不含明文。这句话不说，用户会自己去翻一遍再决定，而翻不完的时候
 * 他会选择不发——那这个功能就没用了。
 *
 * 另一件要说清的是**分类里含什么**：日志分类里有操作者与来源 IP，
 * 导出前他需要知道。
 */
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { diagnosticsApi, type DiagnosticCategory } from '@/api/diagnostics'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'

export function DiagnosticsPage() {
  const [selected, setSelected] = useState<string[]>([])
  const [seeded, setSeeded] = useState(false)

  const categories = useQuery({
    queryKey: ['diagnostics-categories'],
    queryFn: diagnosticsApi.categories,
  })

  // 默认全选：不选等于"我全都想要"，而逐个勾选是额外负担。
  if (!seeded && categories.data) {
    setSeeded(true)
    setSelected(categories.data.items.map((c) => c.key))
  }

  if (categories.isPending) return <PageLoading />
  const items: DiagnosticCategory[] = categories.data?.items ?? []

  const toggle = (key: string) =>
    setSelected((prev) => (prev.includes(key) ? prev.filter((k) => k !== key) : [...prev, key]))

  const download = () => {
    // 交给浏览器：同源会带上会话 Cookie，并自己处理大文件落盘。
    window.location.href = diagnosticsApi.exportUrl(selected)
  }

  return (
    <div className="flex flex-col gap-5">
      <header>
        <h1 className="text-lg font-semibold text-ink">诊断导出</h1>
        <p className="mt-1 text-base text-ink-3">
          按分类导出一个排障包（zip），供离线分析或发给支持。
        </p>
      </header>

      <div className="rounded-card border border-success/30 bg-success/5 px-4 py-3">
        <p className="text-base text-ink">
          <span className="font-medium">包可以直接外发。</span>
          敏感配置项（密钥、密码、令牌）在生成时就被打码，只显示「已设置」而不含明文
          ——不需要您自己先删一遍，也建议不要依赖「发出前检查」这种做法。
        </p>
      </div>

      <section className="flex flex-col gap-3">
        <h2 className="text-base font-medium text-ink-2">选择分类</h2>
        {items.map((c) => (
          <label
            key={c.key}
            className="flex cursor-pointer gap-2.5 rounded-card border border-line bg-surface px-4 py-3"
          >
            <input
              type="checkbox"
              checked={selected.includes(c.key)}
              onChange={() => toggle(c.key)}
              className="mt-1"
            />
            <span className="flex flex-col gap-0.5">
              <span className="flex items-baseline gap-2">
                <span className="text-base text-ink">{c.label}</span>
                {/* 含敏感内容的分类要标出来，用户有权在导出前知道。 */}
                {c.sensitive && (
                  <span className="rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning">
                    含敏感内容
                  </span>
                )}
              </span>
              <span className="text-sm text-ink-3">{c.description}</span>
            </span>
          </label>
        ))}
      </section>

      <div className="flex items-center gap-3">
        <Button disabled={selected.length === 0} onClick={download}>
          导出诊断包
        </Button>
        <span className="text-sm text-ink-3">
          {selected.length === 0
            ? '请至少选择一个分类'
            : `将导出 ${selected.length} 个分类`}
        </span>
      </div>

      <section className="rounded-card border border-line bg-surface px-4 py-3">
        <h2 className="text-base font-medium text-ink-2">包里有什么</h2>
        <ul className="mt-1.5 flex flex-col gap-1 text-sm text-ink-3">
          <li>
            <span className="kc-mono text-ink-2">MANIFEST.json</span>
            ：生成时刻、包含的分类，以及哪些内容被截断。
            截断会被写明——不说的话，离线分析的人会把「只看到最后 2000 条日志」
            当成「这段时间只有这些」。
          </li>
          <li>
            运行时状态只含计数与状态，不含具体内容：需要细节时请到任务中心，
            那里有权限控制。
          </li>
          <li>每一次导出都会记审计（操作者、时刻、来源 IP、选了哪些分类）。</li>
        </ul>
      </section>
    </div>
  )
}
