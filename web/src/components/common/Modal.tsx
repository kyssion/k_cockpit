import { useEffect, type ReactNode } from 'react'

import { Button } from './Button'

interface ModalProps {
  open: boolean
  title: string
  description?: string
  onClose: () => void
  children: ReactNode
  footer?: ReactNode
  /** 宽度档位：表单用 md，展示命令或大段文本用 lg。 */
  size?: 'md' | 'lg'
  /**
   * 是否允许用 Esc 关闭。默认允许——键盘用户不该被弹窗困住。
   *
   * 设为 false 只用于**内容只出现一次**的场合（如刚生成的 API 凭证）：
   * 按一下 Esc 就再也拿不回来的东西，值得多一步确认。
   */
  dismissable?: boolean
}

const sizes = { md: 'max-w-[420px]', lg: 'max-w-[640px]' }

export function Modal({
  open,
  title,
  description,
  onClose,
  children,
  footer,
  size = 'md',
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

        <div className="max-h-[60vh] overflow-y-auto px-5 py-4">{children}</div>

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
