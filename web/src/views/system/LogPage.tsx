/**
 * LogPage 提供服务端日志的查看、级别调整、导出与清理（F-9-02）。
 *
 * 页面上有三处要在用户动手**之前**说清的代价：
 *
 *  调到 DEBUG  日志量会显著上升（磁盘消耗同步加快），而且它记录的东西更多、
 *              也就更需要脱敏规则覆盖得全。
 *  清理        之后就**无法回溯**了——出问题时最想要的那段历史正好是刚被
 *              清掉的那段。
 *  导出        导出的内容是**脱敏后**的（脱敏发生在写入时），因此可以直接
 *              外发，不必自己先检查一遍。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { formatBytes, logApi, LOG_LEVELS, type LogEntry } from '@/api/logging'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function LogPage() {
  const queryClient = useQueryClient()
  const [level, setLevel] = useState('')
  const [keyword, setKeyword] = useState('')
  const [confirmPurge, setConfirmPurge] = useState(false)
  const [error, setError] = useState('')

  const status = useQuery({ queryKey: ['log-status'], queryFn: logApi.status, refetchInterval: 15000 })
  const lines = useQuery({
    queryKey: ['log-read', level, keyword],
    queryFn: () => logApi.read({ limit: 300, level, keyword }),
    // 跟在看日志的节奏上：日志会持续产生，手动刷新是把系统该做的事推给用户。
    refetchInterval: 5000,
  })

  const setLevelMut = useMutation({
    mutationFn: (v: string) => logApi.setLevel(v),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['log-status'] })
    },
    onError: (e) => setError(describe(e)),
  })

  const purge = useMutation({
    mutationFn: () => logApi.purge(),
    onSuccess: (r) => {
      setConfirmPurge(false)
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['log-status'] })
      void queryClient.invalidateQueries({ queryKey: ['log-read'] })
      alert(`已释放 ${formatBytes(r.freed_bytes)}`)
    },
    onError: (e) => {
      setConfirmPurge(false)
      setError(describe(e))
    },
  })

  if (status.isPending) return <PageLoading />
  const st = status.data
  const items = lines.data?.items ?? []

  return (
    <div className="flex flex-col gap-5">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-lg font-semibold text-ink">日志</h1>
          <p className="mt-1 text-base text-ink-3">
            服务端运行日志。
            <span className="text-ink-2">敏感字段在写入时就被脱敏</span>
            ——磁盘上的那份本身就不含明文，因此导出的内容可以直接外发。
          </p>
        </div>
        <div className="flex items-end gap-2">
          <div className="flex flex-col gap-1">
            <label className="text-xs text-ink-3">级别</label>
            <select
              value={level || st?.level?.toLowerCase() || 'info'}
              onChange={(e) => {
                setLevel(e.target.value)
                setLevelMut.mutate(e.target.value)
              }}
              className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
            >
              {LOG_LEVELS.map((l) => (
                <option key={l.value} value={l.value}>
                  {l.label}
                </option>
              ))}
            </select>
          </div>
          <Button size="sm" variant="secondary" onClick={() => (window.location.href = logApi.exportUrl())}>
            导出
          </Button>
          <Button size="sm" variant="danger" onClick={() => setConfirmPurge(true)}>
            清理
          </Button>
        </div>
      </header>

      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {/* 状态卡：直接回答"最近有没有错误"——那是打开这一页时最想问的第一个问题。 */}
      <section className="grid gap-3 sm:grid-cols-4">
        <Stat label="当前级别" value={(st?.level ?? '—').toUpperCase()} />
        <Stat
          label="最近错误"
          value={String(st?.lines?.ERROR ?? 0)}
          tone={(st?.lines?.ERROR ?? 0) > 0 ? 'danger' : undefined}
        />
        <Stat label="最近警告" value={String(st?.lines?.WARN ?? 0)} />
        <Stat
          label="文件大小"
          value={st?.file_path ? formatBytes(st.file_size) : '未落盘'}
          hint={st?.file_path ? undefined : '当前只写内存与标准错误，不写文件'}
        />
      </section>

      {st && (st.rotated?.length ?? 0) > 0 && (
        <p className="text-sm text-ink-3">
          另有 {st.rotated?.length ?? 0} 个轮转文件（保留最近 {st.keep_files} 个）。
        </p>
      )}

      <section className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center gap-2">
          <input
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
            placeholder="按关键字过滤…"
            className="h-8 w-64 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          />
          <select
            value={level}
            onChange={(e) => setLevel(e.target.value)}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value="">全部级别</option>
            {LOG_LEVELS.map((l) => (
              <option key={l.value} value={l.value}>
                仅 {l.label}
              </option>
            ))}
          </select>
          {/* 在线查看只读内存：它要即时响应，而扫一遍几百 MB 的文件会让
              页面卡上几秒。更早的内容用导出。 */}
          <span className="text-xs text-ink-3">
            显示内存中最近 {st?.ring_lines ?? 0} 行中的一部分；更早的内容请导出。
          </span>
        </div>

        {lines.isPending ? (
          <PageLoading />
        ) : items.length === 0 ? (
          <EmptyState
            title="没有匹配的日志"
            description="当前级别下没有输出，或过滤条件太严。把级别调到 debug 可以看到更多。"
          />
        ) : (
          <div className="max-h-[560px] overflow-auto rounded-card border border-line bg-sunken">
            <table className="w-full text-left text-xs">
              <tbody>
                {items.map((e, i) => (
                  <LogRow key={`${e.at}-${i}`} entry={e} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <Modal
        open={confirmPurge}
        title="清理日志"
        description="会删除全部轮转文件，并清空当前日志文件。"
        onClose={() => setConfirmPurge(false)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setConfirmPurge(false)}>
              取消
            </Button>
            <Button variant="danger" size="sm" loading={purge.isPending} onClick={() => purge.mutate()}>
              确认清理
            </Button>
          </>
        }
      >
        <p className="text-sm text-ink-3">
          清理之后就无法回溯了——出问题时最想要的那段历史，往往正是刚被清掉的
          那一段。如果只是想腾点空间，先导出再清理。
        </p>
        <p className="mt-2 text-sm text-ink-3">
          当前文件本身不会被删除（只截断）：删掉它会让正在运行的服务失去写入位置，
          表现为「清理之后日志再也不出现了」——而那时恰恰最需要日志。
        </p>
      </Modal>
    </div>
  )
}

function LogRow({ entry }: { entry: LogEntry }) {
  const tone =
    entry.level === 'ERROR' ? 'danger' : entry.level === 'WARN' ? 'warning' : 'success'
  return (
    <tr className="border-b border-line/50 last:border-0">
      <td className="whitespace-nowrap px-3 py-1 align-top text-ink-3">
        {entry.at.slice(11, 23)}
      </td>
      <td className="px-1 py-1 align-top">
        <StatusBadge tone={tone}>{entry.level}</StatusBadge>
      </td>
      <td className="kc-mono whitespace-pre-wrap break-all px-3 py-1 align-top text-ink-2">
        {entry.line}
      </td>
    </tr>
  )
}

function Stat({
  label,
  value,
  tone,
  hint,
}: {
  label: string
  value: string
  tone?: 'danger'
  hint?: string
}) {
  return (
    <div className="rounded-card border border-line bg-surface px-3 py-2.5">
      <div className="text-xs text-ink-3">{label}</div>
      <div className={`kc-nums text-base ${tone === 'danger' ? 'text-danger' : 'text-ink'}`}>
        {value}
      </div>
      {hint && <div className="mt-0.5 text-xs text-ink-3">{hint}</div>}
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
