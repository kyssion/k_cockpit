/**
 * XMLTab 查看虚拟机的 libvirt 定义（F-2-03 的一部分）。
 *
 * **只读**。它是排查「面板显示的和实际跑的不是一回事」时唯一的真相——因此
 * 界面上要把两件事说清楚：
 *
 *   1. **看的是哪一份定义**（运行中 / 持久）。热插拔一块盘之后两份会不同：
 *      运行中的有它、持久定义没有，重启之后那块盘就消失了。拿运行时的定义
 *      去判断「重启后还在不在」会出错。
 *   2. **哪些字段被脱敏了**。不说的话，用户看到 `passwd='[已脱敏]'` 会以为
 *      面板把它读错了——而真相是它本来就在那里、只是没给他看。
 */
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'

import { ApiError, NetworkError } from '@/api/client'
import { vmApi } from '@/api/vm'
import { Button } from '@/components/common/Button'
import { PageLoading } from '@/components/common/Feedback'

export function XMLTab({ vmID }: { vmID: number }) {
  const [live, setLive] = useState(false)
  const [copied, setCopied] = useState(false)

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
        </div>
      </header>

      {/* 两份定义会不同，而用户需要知道自己在看哪一份。 */}
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
    </div>
  )
}

function describe(error: unknown): string {
  if (error instanceof ApiError || error instanceof NetworkError) return error.message
  return '读取失败，请稍后重试'
}
