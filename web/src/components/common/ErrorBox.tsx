/**
 * 错误提示块。
 *
 * `role="alert"` 让读屏软件立即播报——错误信息只有颜色区分时，
 * 使用读屏的用户不会知道操作失败了。
 */
export function ErrorBox({ message }: { message: string }) {
  return (
    <p role="alert" className="rounded-card bg-danger/10 px-4 py-3 text-base text-danger">
      {message}
    </p>
  )
}
