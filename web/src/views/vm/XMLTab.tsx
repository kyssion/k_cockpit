/**
 * XMLTab 查看与编辑虚拟机的 libvirt 定义。
 *
 * **查看是排查时的唯一真相**：面板上的配置是控制面的意图，而这份 XML 是节点
 * 上实际在跑的东西。因此界面上要把两件事说清楚：
 *
 *   1. **看的是哪一份定义**（运行中 / 持久）。热插拔一块盘之后两份会不同：
 *      运行中的有它、持久定义没有，重启之后那块盘就消失了。
 *   2. **哪些字段被脱敏了**。不说的话，用户看到 `passwd='[已脱敏]'` 会以为
 *      面板把它读错了。
 *
 * **编辑是这一页里危险的部分**：它绕过控制面建立的**全部**校验（配额、地址
 * 唯一性、前置条件）。因此流程刻意做成三步：改 → 看到 diff 与校验结果 →
 * 确认保存。**用户看的必须是他将要提交的那一份**，而不是一个"大概的意思"。
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { vmApi, type XMLPrecheck } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'
import { Modal } from '@/components/common/Modal'
import { StatusBadge } from '@/components/common/StatusBadge'

export function XMLTab({ vmID }: { vmID: number }) {
  const queryClient = useQueryClient()
  const [live, setLive] = useState(false)
  const [copied, setCopied] = useState(false)
  const [editing, setEditing] = useState<string | null>(null)
  const [precheck, setPrecheck] = useState<XMLPrecheck | null>(null)
  const [error, setError] = useState('')

  const xml = useQuery({
    queryKey: ['vm-xml', vmID, live],
    queryFn: () => vmApi.xml(vmID, live),
  })

  const copy = async () => {
    if (!xml.data) return
    try {
      await navigator.clipboard.writeText(xml.data.xml)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // 剪贴板在非 HTTPS 或未授权时会失败。失败就算了——用户还能手动选中。
    }
  }

  const check = useMutation({
    mutationFn: (text: string) => vmApi.xmlPrecheck(vmID, text),
    onSuccess: (r) => {
      setPrecheck(r)
      setError('')
    },
    onError: (e) => setError(describe(e)),
  })

  const save = useMutation({
    mutationFn: (text: string) => vmApi.xmlUpdate(vmID, text),
    onSuccess: () => {
      setEditing(null)
      setPrecheck(null)
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['vm-xml'] })
      void queryClient.invalidateQueries({ queryKey: ['vm'] })
      void queryClient.invalidateQueries({ queryKey: ['audit'] })
    },
    onError: (e) => setError(describe(e)),
  })

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-medium text-ink">虚拟机定义</h2>
          <p className="mt-1 text-sm text-ink-3">
            节点上实际在跑的 libvirt 定义。面板配置与它不一致时（手工改过、
            或某次下发部分失败），这一份才是真相。
          </p>
        </div>
        <div className="flex items-end gap-2">
          <div className="flex gap-1">
            <button
              onClick={() => setLive(false)}
              className={`rounded-control px-2.5 py-1 text-sm ${
                !live ? 'bg-primary/10 text-primary' : 'text-ink-3 hover:bg-sunken'
              }`}
            >
              持久定义
            </button>
            <button
              onClick={() => setLive(true)}
              className={`rounded-control px-2.5 py-1 text-sm ${
                live ? 'bg-primary/10 text-primary' : 'text-ink-3 hover:bg-sunken'
              }`}
            >
              运行中
            </button>
          </div>
          <Button size="sm" variant="secondary" onClick={copy} disabled={!xml.data}>
            {copied ? '已复制' : '复制'}
          </Button>
          {/* 编辑的是**持久定义**——运行中的那份重启后就没了，拿它当编辑
              基准会让用户改的东西在下次重启时消失。 */}
          <Button
            size="sm"
            disabled={!xml.data || live}
            title={live ? '运行中的定义不能作为编辑基准——重启之后它就没了' : undefined}
            onClick={() => setEditing(xml.data?.xml ?? '')}
          >
            编辑
          </Button>
        </div>
      </header>

      <p className="rounded-control bg-sunken px-3 py-2 text-sm text-ink-3">
        {live
          ? '当前是运行中的定义——热插拔的设备会出现在这里，但重启后不一定还在。'
          : '当前是持久定义——重启之后生效的就是这一份。'}
      </p>

      {(xml.data?.redacted?.length ?? 0) > 0 && (
        <p className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning">
          定义里的 {xml.data?.redacted?.join('、')} 已被替换为 [已脱敏]——那是控制台
          凭据，界面上一贯只写不读，这里也不例外。它不是面板读错了。
        </p>
      )}

      {xml.isPending ? (
        <PageLoading />
      ) : xml.isError ? (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {describe(xml.error)}
        </p>
      ) : (
        <pre className="kc-mono max-h-[600px] overflow-auto rounded-card border border-line bg-sunken px-3 py-2.5 text-xs text-ink-2">
          {xml.data?.xml}
        </pre>
      )}

      {error && (
        <p className="rounded-control border border-danger/30 bg-danger/10 px-3 py-2 text-base text-danger">
          {error}
        </p>
      )}

      {/* 第一步：编辑。 */}
      <Modal
        open={editing !== null && precheck === null}
        title="编辑定义"
        description="改动会先经过节点校验并给出逐行差异，确认之后才会应用。"
        onClose={() => setEditing(null)}
        footer={
          <>
            <Button variant="secondary" size="sm" onClick={() => setEditing(null)}>
              取消
            </Button>
            <Button
              size="sm"
              loading={check.isPending}
              disabled={editing === null || editing.trim() === ''}
              onClick={() => editing !== null && check.mutate(editing)}
            >
              校验并查看差异
            </Button>
          </>
        }
      >
        <textarea
          value={editing ?? ''}
          onChange={(e) => setEditing(e.target.value)}
          rows={20}
          spellCheck={false}
          className="kc-mono w-full rounded-control border border-line-strong bg-sunken px-2.5 py-2 text-xs text-ink"
        />
        <p className="mt-2 text-xs text-warning">
          这里能改的东西**没有边界**：配额、地址唯一性、端口安全的前置条件都可以
          被绕开。因此保存需要二次验证。
        </p>
      </Modal>

      {/* 第二步：看差异与校验结果，然后决定。 */}
      <Modal
        open={precheck !== null}
        title="确认改动"
        description="下面是这次改动的逐行差异与节点给的校验结果。"
        onClose={() => setPrecheck(null)}
        footer={
          <>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                setPrecheck(null)
                // 回到编辑状态，让用户接着改。
              }}
            >
              返回修改
            </Button>
            <Button
              size="sm"
              variant={precheck?.valid ? 'primary' : 'danger'}
              disabled={!precheck?.valid || precheck?.diff?.identical}
              loading={save.isPending}
              onClick={() => editing !== null && save.mutate(editing)}
            >
              {precheck?.diff?.identical ? '没有改动' : '确认保存'}
            </Button>
          </>
        }
      >
        {precheck && (
          <div className="flex flex-col gap-3">
            {!precheck.valid && (
              <div className="rounded-control border border-danger/40 bg-danger/5 px-3 py-2">
                <p className="text-sm text-danger">这份定义没有通过节点校验，不能保存：</p>
                <ul className="mt-1 flex flex-col gap-0.5">
                  {precheck.errors?.map((e) => (
                    <li key={e} className="kc-mono text-xs text-danger">
                      {e}
                    </li>
                  ))}
                </ul>
              </div>
            )}

            {precheck.valid && (precheck.warnings?.length ?? 0) > 0 && (
              <ul className="flex flex-col gap-1">
                {precheck.warnings?.map((w) => (
                  <li
                    key={w}
                    className="rounded-control border border-warning/40 bg-warning/5 px-3 py-2 text-sm text-warning"
                  >
                    {w}
                  </li>
                ))}
              </ul>
            )}

            <div className="flex items-center gap-3 text-sm">
              <StatusBadge tone={precheck.diff.identical ? 'idle' : 'warning'}>
                {`+${precheck.diff.added} / -${precheck.diff.removed}`}
              </StatusBadge>
              {precheck.diff.identical && (
                <span className="text-ink-3">
                  没有检测到改动——保存会触发一次完整的重新定义，因此没有变化时
                  不下发。
                </span>
              )}
            </div>

            <pre className="max-h-96 overflow-auto rounded-control bg-sunken px-3 py-2 text-xs">
              {precheck.diff.lines.map((l, i) => (
                <div
                  key={i}
                  className={
                    l.kind === 'add'
                      ? 'text-success'
                      : l.kind === 'del'
                        ? 'text-danger'
                        : 'text-ink-3'
                  }
                >
                  <span className="mr-2 select-none opacity-50">
                    {l.kind === 'add' ? '+' : l.kind === 'del' ? '-' : ' '}
                  </span>
                  <span className="kc-mono">{l.text}</span>
                </div>
              ))}
            </pre>
          </div>
        )}
      </Modal>
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '操作失败，请稍后重试'
}
