/**
 * E2E 全局初始化：等控制面就绪后，用日志里的一次性令牌创建管理员。
 *
 * 走的是**真实的初始化链路**（ADR-0008：令牌只打印到服务端日志，
 * POST /setup/admin 创建首个管理员）——不直接往库里塞用户。冒烟的
 * 价值恰恰在于「面板从零到能用的第一条路」没断，绕过它等于只测了
 * 登录表单本身。
 */
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { E2E_ADMIN } from './admin'

const API = process.env.KC_E2E_API ?? 'http://127.0.0.1:8099'
const LOG = path.resolve(fileURLToPath(new URL('.', import.meta.url)), '../../build/e2e/server.log')

// 令牌在提示文案之后的第一段长十六进制串。不按整行匹配：日志前缀
// （时间戳 + 级别）会插在文案与令牌之间，逐行解析反而脆。
const TOKEN_RE = /一次性初始化令牌[\s\S]*?([0-9a-f]{32,})/

async function waitForToken(timeoutMs = 30_000): Promise<string> {
  const deadline = Date.now() + timeoutMs
  while (Date.now() < deadline) {
    try {
      const log = fs.readFileSync(LOG, 'utf8')
      const m = log.match(TOKEN_RE)
      if (m) return m[1]
    } catch {
      // 日志还没写出来：webServer 只保证 /health 可达，令牌可能稍后打印。
    }
    await new Promise((r) => setTimeout(r, 300))
  }
  throw new Error(`在 ${LOG} 里等不到初始化令牌（控制面可能没起来）`)
}

export default async function globalSetup() {
  const token = await waitForToken()
  const res = await fetch(`${API}/api/v1/setup/admin`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ...E2E_ADMIN, token }),
  })
  const body = await res.text()
  // 库是每次冒烟重建的，「已初始化」只在开发期复用服务时出现。
  if (!res.ok && !body.includes('已初始化')) {
    throw new Error(`创建管理员失败 (${res.status}): ${body}`)
  }
}
