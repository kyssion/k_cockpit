/**
 * ApiDocsPage 列出面板开放的全部 HTTP 接口（F-9-07）。
 *
 * 刻意的取舍：
 *
 *  1. **清单与元数据都来自服务端**，不在前端写死。手写清单在第一次加接口
 *     时就会漏，而漏了不会有任何提示。
 *  2. **认证标注是按注册处的数据表判定的**，个别新增路由忘同步时的表现是
 *     "标错"而不是"缺失"——因此页面仍指向 docs/03-api/API.md 作为最终权威。
 *  3. **可复制的是 curl 而不是"试试看"按钮**。这个页面没有请求体编辑器：
 *     一半以上的接口是写操作，给一个随手就能点的执行按钮，等于把生产
 *     数据交给一次误点。
 */
import { useQuery } from '@tanstack/react-query'
import { useMemo, useState } from 'react'

import { AUTH_LABEL, apiDocsApi, type ApiEndpoint } from '@/api/apidocs'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'

/** 按路径第一段分组：与 docs/03-api/API.md 的模块划分一致。 */
function groupOf(path: string): string {
  const seg = path.replace(/^\/api\/v\d+/, '').split('/').filter(Boolean)[0] ?? ''
  return seg || 'other'
}

function authClass(auth: string): string {
  if (auth === 'public') return 'border-success/30 bg-success/10 text-success'
  if (auth === 'admin') return 'border-danger/30 bg-danger/10 text-danger'
  return 'border-line-strong bg-sunken text-ink-3'
}

export function ApiDocsPage() {
  const [keyword, setKeyword] = useState('')
  const [copied, setCopied] = useState('')
  const [expanded, setExpanded] = useState('')

  const list = useQuery({ queryKey: ['api-endpoints'], queryFn: apiDocsApi.list })

  const groups = useMemo(() => {
    const items = list.data?.endpoints ?? []
    const kw = keyword.trim().toLowerCase()
    const buckets = new Map<string, ApiEndpoint[]>()
    for (const e of items) {
      if (
        kw &&
        !e.path.toLowerCase().includes(kw) &&
        !e.method.toLowerCase().includes(kw) &&
        !e.summary.toLowerCase().includes(kw)
      ) {
        continue
      }
      const key = groupOf(e.path)
      const bucket = buckets.get(key)
      if (bucket) bucket.push(e)
      else buckets.set(key, [e])
    }
    return [...buckets.entries()].sort((a, b) => a[0].localeCompare(b[0]))
  }, [list.data, keyword])

  function copy(text: string, key: string) {
    void navigator.clipboard?.writeText(text)
    setCopied(key)
    window.setTimeout(() => setCopied(''), 1500)
  }

  return (
    <div className="flex flex-col gap-4">
      <header>
        <h1 className="text-xl font-semibold text-ink">API 文档</h1>
        <p className="mt-1 text-base text-ink-3">
          共 {list.data?.endpoints.length ?? 0} 个接口，清单由服务端从已注册的路由生成；
          摘要与认证方式由服务端补齐。请求体的字段含义与错误码以{' '}
          <span className="text-ink-2">docs/03-api/API.md</span> 为准。
        </p>
      </header>

      <div className="max-w-sm">
        <Input
          label="过滤"
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          placeholder="按路径、方法或摘要过滤，如 vms / POST"
        />
      </div>

      {list.isPending && <PageLoading />}

      {list.isError && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          读取接口清单失败，请稍后重试。
        </p>
      )}

      {list.data && groups.length === 0 && (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState title="没有匹配的接口" description="换个关键词试试。" />
        </div>
      )}

      {groups.map(([name, items]) => (
        <section key={name} className="rounded-card border border-line">
          <header className="border-b border-line bg-sunken px-4 py-2">
            <h2 className="text-base font-medium text-ink">
              {items[0]?.module || name}
              <span className="kc-mono ml-2 text-xs text-ink-3">/{name}</span>
              <span className="ml-2 text-xs text-ink-3">{items.length} 个</span>
            </h2>
          </header>
          <ul className="divide-y divide-line">
            {items.map((e) => {
              const key = `${e.method} ${e.path}`
              const open = expanded === key
              return (
                <li key={key} className="px-4 py-2">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="kc-mono w-14 shrink-0 text-xs text-brand">{e.method}</span>
                    <button
                      type="button"
                      className="kc-mono min-w-0 flex-1 truncate text-left text-base text-ink-2 hover:text-brand"
                      title={e.path}
                      onClick={() => setExpanded(open ? '' : key)}
                    >
                      {e.path}
                    </button>
                    <span className="text-sm text-ink-3">{e.summary}</span>
                    <span
                      className={`rounded-full border px-2 py-0.5 text-xs ${authClass(e.auth)}`}
                      title="认证方式"
                    >
                      {AUTH_LABEL[e.auth] ?? e.auth}
                    </span>
                    <button
                      type="button"
                      onClick={() => copy(e.curl, key)}
                      className="text-sm text-ink-3 hover:text-brand"
                    >
                      {copied === key ? '已复制' : '复制 curl'}
                    </button>
                  </div>

                  {open && (
                    <div className="mt-2 flex flex-col gap-2 border-t border-line pt-2">
                      {(e.params?.length ?? 0) > 0 && (
                        <p className="text-sm text-ink-3">
                          参数：
                          {e.params!.map((p) => (
                            <code key={p.name} className="kc-mono mx-1 text-ink-2">
                              {p.in === 'path' ? ':' : `${p.in} `}
                              {p.name}
                            </code>
                          ))}
                        </p>
                      )}
                      <pre className="kc-mono overflow-auto rounded-control bg-sunken px-3 py-2 text-xs text-ink-2">
                        {e.curl}
                      </pre>
                    </div>
                  )}
                </li>
              )
            })}
          </ul>
        </section>
      ))}

      <p className="text-sm text-ink-3">
        会话凭据在 HttpOnly Cookie 中；用 API 凭证调用时改用{' '}
        <code className="kc-mono">Authorization: Bearer kc_...</code>。写操作可能触发
        二次验证（428），请求层会自动完成验证并重放。
      </p>
    </div>
  )
}
