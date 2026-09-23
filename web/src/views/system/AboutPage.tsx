/**
 * AboutPage 是关于项目（F-9-09）。
 *
 * 它只陈述**能核实的事实**：版本号来自构建信息（与 /version 同一接口），
 * 开源地址与许可证以仓库里的实际情况为准。
 *
 * **不写营销语**：一个"致力于提供业界领先的虚拟化管理体验"的句子既不
 * 可验证，也不帮助用户判断这个东西是什么。这里每一行都应当能被证实或
 * 被指出是错的。
 */
import { useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { Link } from 'react-router'

import { BETA_NOTICE, USER_AGREEMENT, type AgreementSection } from '@/utils/agreement'
import { versionApi } from '@/api/version'
import { Button } from '@/components/common/Button'
import { EmptyState, PageLoading } from '@/components/common/Feedback'

/** 仓库地址。留空时界面会显示"未配置"，而不是编一个链接出来。 */
const REPO_URL = ''


export function AboutPage() {
  const version = useQuery({ queryKey: ['version'], queryFn: versionApi.get })

  return (
    <div className="flex flex-col gap-4">
      <header>
        <h1 className="text-xl font-semibold text-ink">关于项目</h1>
        <p className="mt-1 text-base text-ink-3">
          K Cockpit —— 面向宿主机的虚拟机控制面板，控制面与节点代理（agent）分离部署。
        </p>
      </header>

      {version.isPending ? (
        <PageLoading />
      ) : version.isError || !version.data ? (
        <div className="rounded-card border border-dashed border-line-strong">
          <EmptyState
            title="读取版本信息失败"
            description="版本由构建时注入。以 go run 启动时没有构建信息，这是正常的。"
          />
        </div>
      ) : (
        <section className="rounded-card border border-line">
          <table className="w-full text-left text-base">
            <tbody>
              <Row label="面板版本" value={version.data.panel_version || '（无构建信息）'} />
              <Row label="Go 版本" value={version.data.go_version || '—'} />
              <Row label="平台" value={version.data.platform || '—'} />
              <Row label="构建时刻" value={version.data.build_time || '—'} />
              <Row
                label="提交"
                value={
                  version.data.revision
                    ? version.data.revision + (version.data.dirty ? '（有未提交改动）' : '')
                    : '—'
                }
              />
            </tbody>
          </table>
        </section>
      )}

      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="text-base font-medium text-ink">开源与许可证</h2>
        <dl className="mt-2 flex flex-col gap-1.5 text-base">
          <div className="flex gap-2">
            <dt className="w-20 shrink-0 text-ink-3">仓库</dt>
            <dd className="text-ink-2">
              {REPO_URL ? (
                <a href={REPO_URL} target="_blank" rel="noreferrer" className="text-brand hover:underline">
                  {REPO_URL}
                </a>
              ) : (
                '未配置（在 AboutPage 的 REPO_URL 中填入仓库地址后显示）'
              )}
            </dd>
          </div>
          <div className="flex gap-2">
            <dt className="w-20 shrink-0 text-ink-3">许可证</dt>
            <dd className="text-ink-2">
              仓库中未提供 LICENSE 文件。开源协议需要由项目所有者确定并放入仓库根目录——
              这一步不做，任何人都无法合法地二次分发。
            </dd>
          </div>
        </dl>
      </section>

      {/* 协议：法律文本必须与版本绑定且可审计，因此它是前端常量而不是可编辑
          的设置项——可编辑会让"当前生效的是哪一版"变成要额外回答的问题。 */}
      <AgreementSectionView
        title="用户协议"
        hint="使用本面板即表示你已阅读并同意以下条款。"
        sections={USER_AGREEMENT}
      />
      <AgreementSectionView
        title="公测协议"
        hint="当前版本处于公测阶段，以下几点请特别留意。"
        sections={BETA_NOTICE}
      />

      <section className="rounded-card border border-line bg-surface p-4">
        <h2 className="text-base font-medium text-ink">相关页面</h2>
        <ul className="mt-2 flex flex-col gap-1 text-base">
          <li>
            <Link to="/version" className="text-brand hover:underline">
              版本与依赖清单
            </Link>
            <span className="ml-2 text-ink-3">实际链接进二进制的依赖模块</span>
          </li>
          <li>
            <Link to="/api-docs" className="text-brand hover:underline">
              API 文档
            </Link>
            <span className="ml-2 text-ink-3">已注册的接口清单</span>
          </li>
        </ul>
      </section>
    </div>
  )
}

/**
 * AgreementSectionView 展示一份协议。
 *
 * 默认收起：它很长，展开会把页面推得很远。给一个"展开/收起"而不是直接渲染
 * 全文，是让它**可被找到**又不打扰正常浏览的唯一办法。
 */
function AgreementSectionView({
  title,
  hint,
  sections,
}: {
  title: string
  hint: string
  sections: AgreementSection[]
}) {
  const [open, setOpen] = useState(false)

  return (
    <section className="rounded-card border border-line bg-surface">
      <div className="flex items-center justify-between gap-3 px-4 py-2.5">
        <div>
          <h2 className="text-base font-medium text-ink">{title}</h2>
          <p className="mt-0.5 text-sm text-ink-3">{hint}</p>
        </div>
        <Button size="sm" variant="secondary" onClick={() => setOpen((v) => !v)}>
          {open ? '收起' : '展开'}
        </Button>
      </div>

      {open && (
        <div className="flex flex-col gap-3 border-t border-line px-4 py-3">
          {sections.map((sec) => (
            <div key={sec.title}>
              <h3 className="text-sm font-medium text-ink-2">{sec.title}</h3>
              {sec.paragraphs.map((p) => (
                <p key={p} className="mt-1 text-base leading-relaxed text-ink-3">
                  {p}
                </p>
              ))}
            </div>
          ))}
        </div>
      )}
    </section>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <tr className="border-b border-line last:border-0">
      <th className="w-32 px-4 py-2.5 text-left font-medium text-ink-3">{label}</th>
      <td className="kc-mono px-4 py-2.5 text-ink">{value}</td>
    </tr>
  )
}
