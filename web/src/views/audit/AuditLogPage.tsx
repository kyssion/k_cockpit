/**
 * AuditLogPage 展示审计流水（F-1-12）。
 *
 * 在此之前审计只有写入没有读取——记录一直在写，但没有任何人能查。因此这个
 * 页面本身就是那个缺口。
 *
 * 三处与普通列表页不同的处理：
 *
 *   - **结果被截断时明确说出来**。普通列表少几条用户不会在意，而审计少了
 *     几条会让「这段时间没发生过这件事」这个结论变成错的；
 *   - **来源单列一栏**。用 API 凭证执行的操作不触发二次验证，因此「这条记录
 *     是怎么来的」是判断风险时的第一手信息；
 *   - **前后状态并排显示**，而不是折叠起来——审计的价值就在于看清"改成了
 *     什么"。
 */
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { auditApi, actionLabel, SOURCE_LABEL, type AuditEntry, type AuditFilter } from '@/api/audit'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'
import { relativeTime } from '@/utils/format'

export function AuditLogPage() {
  const [draft, setDraft] = useState({ keyword: '', action: '', resourceType: '', success: '' })
  const [filter, setFilter] = useState<AuditFilter>({ page: 1, page_size: 50 })
  const [detail, setDetail] = useState<AuditEntry | null>(null)

  const facets = useQuery({ queryKey: ['audit-facets'], queryFn: auditApi.facets })
  const page = useQuery({
    queryKey: ['audit', filter],
    queryFn: () => auditApi.list(filter),
  })

  const apply = () =>
    setFilter({
      page: 1,
      page_size: filter.page_size,
      keyword: draft.keyword.trim() || undefined,
      action: draft.action || undefined,
      resource_type: draft.resourceType || undefined,
      success: draft.success === '' ? undefined : draft.success === 'true',
    })

  const reset = () => {
    setDraft({ keyword: '', action: '', resourceType: '', success: '' })
    setFilter({ page: 1, page_size: 50 })
  }

  const data = page.data

  return (
    <div className="flex flex-col gap-5">
      <header>
        <h1 className="text-lg font-semibold text-ink">审计日志</h1>
        <p className="mt-1 text-base text-ink-3">
          记录不可修改也<span className="text-ink-2">不可删除</span>——这正是审计的意义。
          普通账号只能看到自己的操作记录。
        </p>
      </header>

      <section className="flex flex-wrap items-end gap-3 rounded-card border border-line bg-surface p-4">
        <div className="flex min-w-[200px] flex-1 flex-col gap-1">
          <label className="text-xs text-ink-3">关键字（匹配资源名与失败原因）</label>
          <input
            value={draft.keyword}
            onChange={(e) => setDraft({ ...draft, keyword: e.target.value })}
            onKeyDown={(e) => e.key === 'Enter' && apply()}
            placeholder="如 worker-01"
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          />
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-xs text-ink-3">动作</label>
          <select
            value={draft.action}
            onChange={(e) => setDraft({ ...draft, action: e.target.value })}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value="">全部</option>
            {(facets.data?.actions ?? []).map((a) => (
              <option key={a} value={a}>
                {actionLabel(a)}
              </option>
            ))}
          </select>
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-xs text-ink-3">资源类型</label>
          <select
            value={draft.resourceType}
            onChange={(e) => setDraft({ ...draft, resourceType: e.target.value })}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value="">全部</option>
            {(facets.data?.resource_types ?? []).map((t) => (
              <option key={t} value={t}>
                {t}
              </option>
            ))}
          </select>
        </div>

        <div className="flex flex-col gap-1">
          <label className="text-xs text-ink-3">结果</label>
          <select
            value={draft.success}
            onChange={(e) => setDraft({ ...draft, success: e.target.value })}
            className="h-8 rounded-control border border-line-strong bg-sunken px-2 text-base text-ink"
          >
            <option value="">全部</option>
            <option value="true">成功</option>
            <option value="false">失败</option>
          </select>
        </div>

        <div className="flex gap-2">
          <Button size="sm" onClick={apply}>
            查询
          </Button>
          <Button variant="secondary" size="sm" onClick={reset}>
            重置
          </Button>
        </div>
      </section>

      {/* 截断必须明说：审计少几条会让「没发生过」这个结论变成错的。 */}
      {data?.truncated && (
        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-base text-warning">
          {data.note}
        </p>
      )}

      {page.isPending ? (
        <PageLoading />
      ) : (data?.items ?? []).length === 0 ? (
        <EmptyState
          title="没有匹配的记录"
          description="调整筛选条件，或确认是否已经把时间范围设得太窄。"
        />
      ) : (
        <>
          <div className="overflow-x-auto rounded-card border border-line">
            <table className="w-full border-collapse text-base">
              <thead>
                <tr className="bg-sunken text-left text-xs text-ink-2">
                  <th className="px-4 py-2.5 font-medium">时间</th>
                  <th className="px-4 py-2.5 font-medium">操作者</th>
                  <th className="px-4 py-2.5 font-medium">来源</th>
                  <th className="px-4 py-2.5 font-medium">动作</th>
                  <th className="px-4 py-2.5 font-medium">对象</th>
                  <th className="px-4 py-2.5 font-medium">结果</th>
                </tr>
              </thead>
              <tbody>
                {(data?.items ?? []).map((e) => (
                  <tr
                    key={e.id}
                    className="cursor-pointer border-t border-line hover:bg-sunken"
                    onClick={() => setDetail(e)}
                  >
                    <td className="px-4 py-2.5 text-ink-3">{relativeTime(e.at)}</td>
                    <td className="px-4 py-2.5 text-ink-2">
                      {e.operator_name || <span className="text-ink-3">系统</span>}
                    </td>
                    <td className="px-4 py-2.5 text-xs text-ink-3">
                      {SOURCE_LABEL[e.source] ?? e.source}
                    </td>
                    <td className="px-4 py-2.5">
                      <span className="text-ink">{actionLabel(e.action)}</span>
                      {/* 未登记的动作把原名也显示出来，便于排查。 */}
                      {actionLabel(e.action) !== e.action && (
                        <span className="kc-mono block text-xs text-ink-3">{e.action}</span>
                      )}
                    </td>
                    <td className="px-4 py-2.5 text-ink-2">
                      {e.resource_name || <span className="text-ink-3">{e.resource_type}</span>}
                    </td>
                    <td className="px-4 py-2.5">
                      {e.success ? (
                        <StatusBadge tone="success">成功</StatusBadge>
                      ) : (
                        <StatusBadge tone="danger">失败</StatusBadge>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          <Pager
            page={data?.page ?? 1}
            pageSize={data?.page_size ?? 50}
            total={data?.total ?? 0}
            onChange={(p) => setFilter({ ...filter, page: p })}
          />
        </>
      )}

      <Modal
        open={detail !== null}
        title={detail ? actionLabel(detail.action) : ''}
        description={detail ? `${detail.at} · ${detail.client_ip || '来源未知'}` : ''}
        onClose={() => setDetail(null)}
        size="lg"
        footer={
          <Button size="sm" onClick={() => setDetail(null)}>
            关闭
          </Button>
        }
      >
        {detail && (
          <div className="flex flex-col gap-3">
            <dl className="grid gap-2 text-sm sm:grid-cols-2">
              <Row label="操作者">
                {detail.operator_name || '系统'}
                {detail.operator_id ? ` (#${detail.operator_id})` : ''}
              </Row>
              <Row label="来源">{SOURCE_LABEL[detail.source] ?? detail.source}</Row>
              <Row label="对象">
                {detail.resource_type}
                {detail.resource_name ? ` · ${detail.resource_name}` : ''}
              </Row>
              <Row label="结果">{detail.success ? '成功' : '失败'}</Row>
            </dl>

            {detail.error && (
              <Block title="失败原因" tone="text-danger">
                {detail.error}
              </Block>
            )}
            {detail.params && <Block title="操作参数">{detail.params}</Block>}
            {/* 前后状态并排而不是折叠：审计的价值就在于看清「改成了什么」。 */}
            {detail.before_state && <Block title="变更前">{detail.before_state}</Block>}
            {detail.after_state && <Block title="变更后">{detail.after_state}</Block>}
          </div>
        )}
      </Modal>
    </div>
  )
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-0.5">
      <dt className="text-ink-3">{label}</dt>
      <dd className="text-ink">{children}</dd>
    </div>
  )
}

function Block({
  title,
  tone,
  children,
}: {
  title: string
  tone?: string
  children: React.ReactNode
}) {
  return (
    <div>
      <p className={`text-sm ${tone ?? 'text-ink-3'}`}>{title}</p>
      <pre className="kc-mono mt-0.5 overflow-x-auto rounded-control border border-line bg-sunken px-3 py-2 text-sm text-ink-2">
        {children}
      </pre>
    </div>
  )
}

function Pager({
  page,
  pageSize,
  total,
  onChange,
}: {
  page: number
  pageSize: number
  total: number
  onChange: (p: number) => void
}) {
  const pages = Math.max(1, Math.ceil(total / pageSize))
  return (
    <div className="flex items-center justify-between gap-3">
      <span className="text-sm text-ink-3">
        共 <span className="kc-nums">{total}</span> 条 · 第 {page}/{pages} 页
      </span>
      <div className="flex gap-2">
        <Button
          variant="secondary"
          size="sm"
          disabled={page <= 1}
          onClick={() => onChange(page - 1)}
        >
          上一页
        </Button>
        <Button
          variant="secondary"
          size="sm"
          disabled={page >= pages}
          onClick={() => onChange(page + 1)}
        >
          下一页
        </Button>
      </div>
    </div>
  )
}
