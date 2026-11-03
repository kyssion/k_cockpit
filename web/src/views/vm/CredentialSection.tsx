import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { vmApi } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { describe } from '@/utils/describe'

/**
 * 登录凭据（G-30）。
 *
 * 明文**只读一次、显示一次**：每次刷新页面都要重新点「显示」才会再取——
 * 后端对每次读取都写审计，界面上「主动取一下」的动作与审计条目一一对应，
 * 而不是页面一打开密码就躺在那里。
 */
export function CredentialSection({ vmID }: { vmID: number }) {
  const [revealed, setRevealed] = useState(false)
  const [copied, setCopied] = useState<'user' | 'pass' | null>(null)

  // 凭据的读取是**用户动作驱动**而不是页面加载驱动：
  // staleTime 为 0 + 手动 refetch 都不如「enabled 由 revealed 控制」直白——
  // 没点「显示」就永远不发请求，审计里也就没有多余的读取记录。
  const cred = useQuery({
    queryKey: ['vm-initial-credential', vmID],
    queryFn: () => vmApi.initialCredential(vmID),
    enabled: revealed,
    staleTime: 0,
    gcTime: 0,
  })

  const copy = async (text: string, which: 'user' | 'pass') => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(which)
      setTimeout(() => setCopied(null), 2000)
    } catch {
      // 剪贴板不可用时用户仍可手动选中，不打断。
    }
  }

  return (
    <section className="rounded-card border border-line bg-surface p-4">
      <div className="flex items-baseline justify-between gap-2">
        <h3 className="text-sm text-ink-3">登录凭据</h3>
        <span className="text-xs text-ink-3">每次读取都会记录到审计</span>
      </div>

      {!revealed ? (
        <div className="mt-2.5 flex flex-wrap items-center gap-2">
          <Button variant="secondary" size="sm" onClick={() => setRevealed(true)}>
            显示登录凭据
          </Button>
          <span className="text-sm text-ink-3">
            创建时注入的初始密码；改过密码后请以来宾自动化里的重设结果为准。
          </span>
        </div>
      ) : cred.isLoading ? (
        <p className="mt-2.5 text-sm text-ink-3">读取中…</p>
      ) : cred.isError ? (
        <p className="mt-2.5 rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
          {describe(cred.error)}
        </p>
      ) : !cred.data?.has ? (
        <p className="mt-2.5 text-sm text-ink-3">
          这台机器创建时没有设置初始密码。可在下方「来宾自动化」里为它重设一个。
        </p>
      ) : (
        <dl className="mt-2.5 grid gap-2 sm:grid-cols-2">
          <div className="flex items-center justify-between gap-2 rounded-control border border-line px-3 py-2">
            <div className="min-w-0">
              <dt className="text-xs text-ink-3">用户名</dt>
              <dd className="truncate font-mono text-base text-ink">{cred.data.username}</dd>
            </div>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => copy(cred.data?.username ?? '', 'user')}
            >
              {copied === 'user' ? '已复制' : '复制'}
            </Button>
          </div>
          <div className="flex items-center justify-between gap-2 rounded-control border border-line px-3 py-2">
            <div className="min-w-0">
              <dt className="text-xs text-ink-3">密码</dt>
              <dd className="truncate font-mono text-base text-ink">{cred.data.password}</dd>
            </div>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => copy(cred.data?.password ?? '', 'pass')}
            >
              {copied === 'pass' ? '已复制' : '复制'}
            </Button>
          </div>
          {cred.data.created_at && (
            <p className="text-xs text-ink-3 sm:col-span-2">
              设置于 {cred.data.created_at}
            </p>
          )}
        </dl>
      )}
    </section>
  )
}
