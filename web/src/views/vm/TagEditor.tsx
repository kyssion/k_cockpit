/**
 * TagEditor 编辑一台虚拟机的标签（F-2-16）。
 *
 * 交互刻意简单：一个输入框，逗号分隔，保存时**整体替换**。
 *
 * 不从服务端拉全部标签做自动补全，是因为标签会越用越多——一个几百个候选的
 * 下拉框对"给这台机器打两个标"这件事没有帮助，反而让人在列表里找。这里只
 * 在下方列出该用户已有的标签作为**可点的快捷项**，点一下就加进来。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { vmTagApi } from '@/api/vmtag'
import { Button } from '@/components/common/Button'

/**
 * TagEditor 自己取标签，不从 props 拿。
 *
 * VM 视图里没有标签字段——为它加一个字段会让**每一次**虚拟机查询都多出
 * 一次关联查询，而标签只在详情与编辑时才看得到。让这一小块自己按需取，
 * 代价只落在真正用到它的地方。
 */
export function TagEditor({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const [error, setError] = useState('')

  const own = useQuery({
    queryKey: ['vm-tags', vmID],
    queryFn: () => vmTagApi.list(vmID),
  })
  const tags = own.data?.tags ?? []

  const known = useQuery({
    queryKey: ['tags'],
    queryFn: vmTagApi.all,
    enabled: editing,
  })

  const save = useMutation({
    mutationFn: (next: string[]) => vmTagApi.set(vmID, next),
    onSuccess: () => {
      setError('')
      setEditing(false)
      void queryClient.invalidateQueries({ queryKey: ['vms'] })
      void queryClient.invalidateQueries({ queryKey: ['vm', vmID] })
      void queryClient.invalidateQueries({ queryKey: ['vm-tags', vmID] })
      void queryClient.invalidateQueries({ queryKey: ['tags'] })
    },
    onError: (err) => setError(describe(err)),
  })

  const commit = (raw: string) => {
    const next = raw
      .split(/[,，]/)
      .map((s) => s.trim())
      .filter(Boolean)
    save.mutate(next)
  }

  if (own.isPending) return null

  if (!editing) {
    return (
      <div className="flex flex-wrap items-center gap-2">
        {tags.length === 0 ? (
          <span className="text-sm text-ink-3">还没有标签</span>
        ) : (
          tags.map((t) => (
            <span
              key={t}
              className="rounded-pill bg-brand/10 px-2 py-0.5 text-sm text-brand"
            >
              {t}
            </span>
          ))
        )}
        <button
          className="text-sm text-brand hover:underline"
          onClick={() => {
            setDraft(tags.join(', '))
            setEditing(true)
          }}
        >
          编辑
        </button>
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && commit(draft)}
          placeholder="多个标签用逗号分隔，例如：生产, 数据库"
          className="h-8 flex-1 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
        />
        <Button size="sm" loading={save.isPending} onClick={() => commit(draft)}>
          保存
        </Button>
        <Button variant="secondary" size="sm" onClick={() => setEditing(false)}>
          取消
        </Button>
      </div>

      {error && <p className="text-sm text-danger">{error}</p>}

      <p className="text-xs text-ink-3">
        标签不能含逗号或换行，最长 32 字。保存是
        <span className="text-ink-2">整体替换</span>——不在这个输入框里的标签会被移除。
      </p>

      {/* 已有的标签作为快捷项：点一下就加进来，省去重复输入。 */}
      {(known.data?.items ?? []).length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-ink-3">点一下添加：</span>
          {(known.data?.items ?? []).map((t) => (
            <button
              key={t.tag}
              className="rounded-pill border border-line px-2 py-0.5 text-xs text-ink-2 hover:border-brand hover:text-brand"
              onClick={() => {
                const cur = draft
                  .split(/[,，]/)
                  .map((s) => s.trim())
                  .filter(Boolean)
                if (!cur.includes(t.tag)) {
                  setDraft([...cur, t.tag].join(', '))
                }
              }}
            >
              {t.tag}
              <span className="ml-1 text-ink-3">({t.count})</span>
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
