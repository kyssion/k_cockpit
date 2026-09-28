import path from 'node:path'
import { fileURLToPath } from 'node:url'

import react from '@vitejs/plugin-react'
import { defineConfig } from 'vitest/config'

const dirname = path.dirname(fileURLToPath(import.meta.url))

/**
 * Vitest 配置（ADR-0006：Vitest 承担单元与组件测试）。
 *
 * alias 与 tsconfig 的 `@/` 保持一致——测试里的 import 路径与业务代码
 * 同一套，避免出现"业务用 @、测试用相对路径"的分叉。
 *
 * environment 用 jsdom：要测的组件（表单、弹窗、请求层的 428 重放）都
 * 依赖 DOM 与 fetch 存根，node 环境跑不起来。
 */
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { '@': path.resolve(dirname, 'src') },
  },
  test: {
    environment: 'jsdom',
    globals: false,
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['./src/test/setup.ts'],
  },
})
