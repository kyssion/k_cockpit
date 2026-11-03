import { useState } from 'react'
import { Link } from 'react-router'

import { Button } from '@/components/common/Button'
import { Modal } from '@/components/common/Modal'

/** 控制台入口与状态说明。 */
export function ConsoleTab({ vmID }: { vmID: number }) {
  // 截图（API-032）：手动抓一张全幅画面。stamp 用于强制 <img> 重取——
  // 服务端 no-store，但同 URL 的 img 元素不会自动重新请求。
  const [shotOpen, setShotOpen] = useState(false)
  const [stamp, setStamp] = useState(0)

  return (
    <section className="rounded-card border border-line">
      <h2 className="border-b border-line px-4 py-2.5 text-sm font-medium text-ink-2">
        VNC 控制台
      </h2>
      <div className="flex flex-col gap-3 px-4 py-3.5">
        <p className="text-base text-ink-2">
          控制台流量经面板代理，不直连宿主机端口。
        </p>
        <div className="flex gap-2">
          <Link to={`/vm/${vmID}/console`}>
            <Button size="sm">打开控制台</Button>
          </Link>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => {
              setStamp(Date.now())
              setShotOpen(true)
            }}
          >
            截取画面
          </Button>
        </div>
        <p className="text-sm text-ink-3">
          控制台的开关、密码与「对外暴露」在控制台页面的「控制台设置」中配置；
          这里提供进入控制台的入口。
        </p>
      </div>

      <Modal
        open={shotOpen}
        title="控制台截图"
        description="截图是抓取那一刻的画面；要持续操作请打开控制台。"
        onClose={() => setShotOpen(false)}
        footer={
          <>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setStamp(Date.now())}
            >
              重新截取
            </Button>
            <Button size="sm" onClick={() => setShotOpen(false)}>
              关闭
            </Button>
          </>
        }
      >
        <img
          src={`/api/v1/vms/${vmID}/console/screenshot?t=${stamp}`}
          alt="控制台画面截图"
          className="w-full rounded-control border border-line bg-console"
        />
      </Modal>
    </section>
  )
}
