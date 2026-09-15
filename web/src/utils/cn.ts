import { clsx, type ClassValue } from 'clsx'
import { twMerge } from 'tailwind-merge'

/**
 * 合并类名。
 *
 * twMerge 负责消解 Tailwind 冲突（后写的覆盖先写的），使调用方可以用
 * `className` 覆盖组件内置样式——否则传入的类与内置类谁生效取决于
 * CSS 生成顺序，无法预测。
 */
export function cn(...inputs: ClassValue[]): string {
  return twMerge(clsx(inputs))
}
