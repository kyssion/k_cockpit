import { defineConfig } from '@playwright/test'

// 实机 UI 走查：复用已启动的 vite(5181) + 真实控制面(18080)，
// 不拉起独立服务，避免与正在跑的实机环境互相干扰。
export default defineConfig({
  testDir: './e2e-live',
  timeout: 60_000,
  retries: 0,
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL: 'http://localhost:5181',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
})
