import { defineConfig } from '@playwright/test'

/**
 * Playwright E2E 配置（冒烟档，ADR-0006 / TESTING.md）。
 *
 * 冒烟 E2E 一次性拉起独立的控制面（临时 SQLite）与 vite 开发服务器，不要求
 * 本机有可用的 libvirt agent。它证明的是「登录与导航这条最重要的链路端到端
 * 没断」——创建 / 电源 / 迁移的核心链路覆盖依赖真实 agent，仍按口径挂在 M5
 * 验收。
 *
 * 端口刻意避开开发期常占用的 8080 / 5173：冒烟与本机开发并行不互踩。
 */
const API_PORT = 8099
const WEB_PORT = 5179
const RUN_DIR = 'build/e2e' // 相对仓库根

export default defineConfig({
  testDir: 'e2e',
  timeout: 30_000,
  retries: process.env.CI ? 1 : 0,
  reporter: [['list']],
  use: {
    baseURL: `http://localhost:${WEB_PORT}`,
    trace: 'retain-on-failure',
  },
  globalSetup: './e2e/global-setup.ts',

  webServer: [
    {
      // 控制面：干净库 → 迁移 → 启动。输出重定向到文件，globalSetup 从
      // 中解析一次性初始化令牌（ADR-0008：令牌只打印到服务端日志）。
      command: [
        `rm -rf ${RUN_DIR} && mkdir -p ${RUN_DIR}`,
        // 冒烟栈用 SQLite（cmd/migrate 是 PG 专用的）；e2e-init 按模型
        // 建全套表，结构一致性由 database 的对齐测试兜底。
        `go run ./cmd/e2e-init`,
        `go run ./cmd/server > ${RUN_DIR}/server.log 2>&1`,
      ].join(' && '),
      cwd: '../',
      env: {
        APP_ENV: 'development',
        APP_PORT: String(API_PORT),
        // agent 网关也要避开开发期占用的 8443：控制面在端口被占时直接
        // 退出（log.Fatalf），冒烟会以「Process from config.webServer was
        // not able to start」这种毫无指向性的错误失败。18443 与 API_PORT
        // 一样，只在冒烟期间使用。
        AGENT_LISTEN_ADDR: ':18445',
        DB_DRIVER: 'sqlite',
        DB_PATH: `${RUN_DIR}/e2e.db`,
        // 日志落在冒烟目录里，不污染开发期的 data/logs。
        LOG_DIR: 'build/e2e/logs',
      },
      url: `http://127.0.0.1:${API_PORT}/health`,
      reuseExistingServer: false,
      timeout: 120_000,
    },
    {
      // 前端：vite 开发服务器，代理目标指向冒烟控制面。
      command: `pnpm exec vite --port ${WEB_PORT} --strictPort`,
      env: { KC_API_TARGET: `http://127.0.0.1:${API_PORT}` },
      url: `http://localhost:${WEB_PORT}`,
      reuseExistingServer: false,
      timeout: 60_000,
    },
  ],
})
