/**
 * ApiDocsPage 列出面板开放的全部 HTTP 接口（F-9-07）。
 *
 * 三条刻意的取舍：
 *
 *  1. **清单来自路由表**，不在前端写死。手写清单在第一次加接口时就会漏，
 *     而漏了不会有任何提示。
 *  2. **不标注角色要求**。它无法从路由表里可靠地取出来，猜出来的版本比
 *     没有更糟——写错一条，就有人照着它设计调用。页面明确指向
 *     docs/03-api/API.md。
 *  3. **可复制的是 curl 而不是"试试看"按钮**。这个页面没有请求体编辑器：
 *     一半以上的接口是写操作，给一个随手就能点的执行按钮，等于把生产
 *     数据交给一次误点。
 */
import { useQuery } from '@tanstack/react-query'
import { useMemo, useState } from 'react'

import { apiDocsApi, type ApiEndpoint } from '@/api/apidocs'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Input } from '@/components/common/Input'

/** 按路径第一段分组：与 docs/03-api/API.md 的模块划分一致。 */
function groupOf(path: string): string {
  const seg = path.replace(/^\/api\/v\d+/, '').split('/').filter(Boolean)[0] ?? ''
  return seg || 'other'
}

export function ApiDocsPage() {
  const [keyword, setKeyword] = useState('')
  const [copied, setCopied] = useState('')

  const list = useQuery({ queryKey: ['api-endpoints'], queryFn: apiDocsApi.list })

  const groups = useMemo(() => {
    const items = list.data?.endpoints ?? []
    const kw = keyword.trim().toLowerCase()
    const buckets = new Map<string, ApiEndpoint[]>()
    for (const e of items) {
      if (kw && !e.path.toLowerCase().includes(kw) && !e.method.toLowerCase().includes(kw)) {
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
        <h1 className="text-lg font-semibold text-ink">API 文档</h1>
        <p className="mt-1 text-base text-ink-3">
          共 {list.data?.endpoints.length ?? 0} 个接口，清单由服务端从已注册的路由生成。
          参数的含义、鉴权要求与错误码以{' '}
          <span className="text-ink-2">docs/03-api/API.md</span> 为准。
        </p>
      </header>

      <div className="max-w-sm">
        <Input
          label="过滤"
          value={keyword}
          onChange={(e) => setKeyword(e.target.value)}
          placeholder="按路径或方法过滤，如 vms / POST"
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
              /{name}
              <span className="ml-2 text-xs text-ink-3">{items.length} 个</span>
            </h2>
          </header>
          <ul className="divide-y divide-line">
            {items.map((e) => {
              const key = `${e.method} ${e.path}`
              const curl = `curl -X ${e.method} '${e.path}' \\\n  -H 'Authorization: Bearer kc_...'`
              return (
                <li key={key} className="flex flex-wrap items-center gap-2 px-4 py-2">
                  <span className="kc-mono w-14 shrink-0 text-xs text-brand">{e.method}</span>
                  <code className="kc-mono min-w-0 flex-1 truncate text-base text-ink-2" title={e.path}>
                    {e.path}
                  </code>
                  <button
                    type="button"
                    onClick={() => copy(curl, key)}
                    className="text-sm text-ink-3 hover:text-brand"
                  >
                    {copied === key ? '已复制' : '复制 curl'}
                  </button>
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
