/**
 * VersionPage 展示版本与依赖清单（F-9-05）。
 *
 * 这一页的内容**全部来自二进制的构建信息**，一个字段都没有手写——手写的
 * 清单在第一次 `go mod tidy` 之后就漂移了，而它恰恰是排查「这个版本里用的
 * 是哪个库」时要看的东西。一份漂移的清单比没有更糟：它给出一个看起来很
 * 具体的错误答案，而人不会去质疑它。
 *
 * 因此页面上还有一句要说明的：依赖清单反映的是**这个二进制里实际链了什么**，
 * 而不是 go.mod 里写了什么——后者含有大量根本不会被链接的间接依赖。
 */
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { versionApi } from '@/api/version'
import { PageLoading } from '@/components/common/Feedback'

export function VersionPage() {
  const [showAll, setShowAll] = useState(false)
  const info = useQuery({ queryKey: ['version'], queryFn: versionApi.get })

  if (info.isPending) return <PageLoading />
  const v = info.data
  if (!v) return null

  const deps = showAll ? v.dependencies : v.dependencies.slice(0, 12)

  return (
    <div className="flex flex-col gap-5">
      <header>
        <h1 className="text-xl font-semibold text-ink">版本与关于</h1>
        <p className="mt-1 text-base text-ink-3">
          面板版本、构建信息与依赖清单。遇到问题时，这里的内容通常是第一个要看的。
        </p>
      </header>

      {!v.build_available && (
        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-base text-warning">
          这份运行没有可用的构建信息（例如直接用 go run 启动）。版本号与依赖清单
          只在构建产物里才有——留空是准确的，而不是这里坏了。
        </p>
      )}

      <section className="rounded-card border border-line bg-surface p-4">
        <dl className="grid gap-x-6 gap-y-2 sm:grid-cols-2">
          <Row label="面板版本">
            {v.panel_version || <span className="text-ink-3">未注入</span>}
          </Row>
          <Row label="Go 版本">{v.go_version}</Row>
          <Row label="平台">{v.platform}</Row>
          <Row label="构建时刻">
            {v.build_time ? formatTime(v.build_time) : <span className="text-ink-3">未知</span>}
          </Row>
          {/* 提交号与「有无未提交改动」是排障时最有用的一对：它们回答
              「跑的是哪个提交、那个提交是不是仓库里能看到的那份」。 */}
          <Row label="提交">
            {v.revision ? (
              <span className="flex items-baseline gap-2">
                <span className="kc-mono">{v.revision.slice(0, 12)}</span>
                {v.dirty && (
                  <span className="rounded-pill bg-warning/10 px-1.5 py-0.5 text-xs text-warning">
                    有未提交改动
                  </span>
                )}
              </span>
            ) : (
              <span className="text-ink-3">未知</span>
            )}
          </Row>
        </dl>
        {v.dirty && (
          <p className="mt-2 text-sm text-warning">
            这个构建包含未提交的改动——它无法用提交号复现。如果是在排查「本地是对的」
            这类问题，这一点往往就是关键。
          </p>
        )}
      </section>

      {/* 被替换的模块单独列出：一个 replace 意味着**实际跑的不是官方版本**，
          而这正是「本地能跑、线上出问题」最常见的原因之一。 */}
      {(v.replacements ?? []).length > 0 && (
        <section className="rounded-card border border-warning/40 bg-warning/5 p-4">
          <h2 className="text-base font-medium text-ink">被替换的模块</h2>
          <p className="mt-1 text-sm text-ink-3">
            这些模块实际运行的**不是官方版本**——go.mod 里的 replace 指令会覆盖它。
            排查「为什么只有这个环境出问题」时先看这里。
          </p>
          <ul className="mt-2 flex flex-col gap-1">
            {(v.replacements ?? []).map((d) => (
              <li key={d.path} className="text-sm">
                <span className="kc-mono text-ink-2">{d.path}</span>
                <span className="ml-2 text-ink-3">→ {d.version}</span>
              </li>
            ))}
          </ul>
        </section>
      )}

      <section className="flex flex-col gap-3">
        <header className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 className="text-base font-medium text-ink-2">
            依赖清单（{v.dependencies.length}）
          </h2>
          {v.dependencies.length > 12 && (
            <button className="text-sm text-primary hover:underline" onClick={() => setShowAll((s) => !s)}>
              {showAll ? '收起' : '展开全部'}
            </button>
          )}
        </header>
        <p className="-mt-2 text-sm text-ink-3">
          这里列的是**这个二进制里实际链了什么**，而不是 go.mod 里写了什么——后者
          含有大量根本不会被链接的间接依赖。
        </p>
        {v.dependencies.length === 0 ? (
          <p className="text-sm text-ink-3">没有第三方依赖。</p>
        ) : (
          <div className="overflow-hidden rounded-card border border-line">
            <table className="w-full text-left text-sm">
              <thead className="bg-sunken text-ink-3">
                <tr>
                  <th className="px-3 py-2 font-normal">模块</th>
                  <th className="px-3 py-2 font-normal">版本</th>
                </tr>
              </thead>
              <tbody>
                {deps.map((d) => (
                  <tr key={d.path} className="border-t border-line transition-colors hover:bg-sunken/70">
                    <td className="kc-mono px-3 py-1.5 text-ink-2">{d.path}</td>
                    <td className="px-3 py-1.5 text-ink-3">{d.version}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex gap-2 text-sm">
      <span className="w-20 shrink-0 text-ink-3">{label}</span>
      <span className="text-ink">{children}</span>
    </div>
  )
}

function formatTime(s: string): string {
  const t = Date.parse(s)
  if (!Number.isFinite(t)) return s
  return new Date(t).toLocaleString()
}
