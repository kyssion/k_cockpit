import { useEffect, type ReactNode } from 'react'

import { Button } from './Button'

interface ModalProps {
  open: boolean
  title: string
  description?: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  /** 宽度档位：表单用 md / lg，带侧边步骤的向导用 xl。 */
  size?: 'md' | 'lg' | 'xl'
  /**
   * 内容区的额外类名。
   *
   * 默认内容区自带内边距与滚动；向导这类**自己管滚动与分栏**的内容需要
   * 覆盖掉它（`p-0` + 固定高度），否则会出现「弹窗内又套一层滚动条」。
   */
  bodyClassName?: string
  /**
   * 是否允许用 Esc 关闭。默认允许——键盘用户不该被弹窗困住。
   *
   * 设为 false 只用于**内容只出现一次**的场合（如刚生成的 API 凭证）：
   * 按一下 Esc 就再也拿不回来的东西，值得多一步确认。
   */
  dismissable?: boolean
}

const sizes = {
  md: 'max-w-[420px]',
  lg: 'max-w-[640px]',
  xl: 'max-w-[1040px]',
}

export function Modal({
  open,
  title,
  description,
  onClose,
  children,
  footer,
  size = 'md',
  bodyClassName,
  dismissable = true,
}: ModalProps) {
  // Esc 关闭：键盘用户不该被弹窗困住。
  useEffect(() => {
    if (!open || !dismissable) return
    function onKey(event: KeyboardEvent) {
      if (event.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open, onClose, dismissable])

  if (!open) return null

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      role="dialog"
      aria-modal="true"
      aria-label={title}
    >
      <div
        className={`w-full ${sizes[size]} rounded-card border border-line bg-raised shadow-3`}
        onClick={(e) => e.stopPropagation()}
      >
        <header className="border-b border-line px-5 py-4">
          <h2 className="text-md font-semibold text-ink">{title}</h2>
          {description && <p className="mt-1 text-sm text-ink-3">{description}</p>}
        </header>

        <div className={bodyClassName ?? 'max-h-[60vh] overflow-y-auto px-5 py-4'}>{children}</div>

        <footer className="flex justify-end gap-2 border-t border-line px-5 py-3">
          {footer ?? (
            <Button variant="secondary" size="sm" onClick={onClose}>
              关闭
            </Button>
          )}
        </footer>
      </div>
    </div>
  )
}
