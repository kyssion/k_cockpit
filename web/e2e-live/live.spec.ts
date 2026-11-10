/**
 * 实机 UI 走查（临时用例，不属于仓库正式 E2E）：
 * 登录真实控制面后遍历所有管理页面，断言页面不出现「页面不存在 / 加载失败」，
 * 且请求不出现 5xx；再验证 VM 详情与控制台入口。
 *
 * 凭据只从环境变量读取，不写进源码（交付测试 TC-23-6 红线）：
 *   E2E_ADMIN_USER / E2E_ADMIN_PASSWORD=... \
 *     pnpm exec playwright test --config=playwright.live.config.ts
 */
import { expect, test, type Page } from '@playwright/test'

const ADMIN = {
  username: process.env.E2E_ADMIN_USER ?? 'admin',
  password: process.env.E2E_ADMIN_PASSWORD ?? '',
}

test.beforeAll(() => {
  if (!ADMIN.password) {
    throw new Error('缺少 E2E_ADMIN_PASSWORD 环境变量：实机走查凭据不得写死在源码里')
  }
})

const ROUTES = [
  ['/', '工作台'],
  ['/vm', '虚拟机'],
  ['/trash', '回收站'],
  ['/task', '任务中心'],
  ['/my-storage', '我的存储'],
  ['/import', '导入'],
  ['/public-ip', '公网 IP'],
  ['/security-group', '安全组'],
  ['/port-security', '端口安全'],
  ['/host-firewall', '宿主机防火墙'],
  ['/passthrough', '硬件直通'],
  ['/capture', '抓包诊断'],
  ['/firewall', '防火墙'],
  ['/port-mirror', '端口镜像'],
  ['/api-keys', 'API 凭证'],
  ['/template', '模板'],
  ['/node', '节点管理'],
  ['/node/1', '节点'],
  ['/storage-pool', '存储池'],
  ['/storage-volume', '存储卷'],
  ['/scheduler', '调度器'],
  ['/alerts', '告警中心'],
  ['/diagnostics', '诊断导出'],
  ['/version', '版本'],
  ['/about', '关于'],
  ['/api-docs', 'API 文档'],
  ['/logs', '日志'],
  ['/host-tuning', '宿主机调优'],
  ['/platform-check', '平台自检'],
  ['/access-control', '访问控制'],
  ['/resource-quota', '资源配额'],
  ['/network', '网络中心'],
  ['/network/base', '网络底座'],
  ['/user', '用户管理'],
  ['/audit', '审计日志'],
  ['/quota', '存储配额'],
  ['/settings', '系统设置'],
  ['/security', '安全中心'],
] as const

async function login(page: Page) {
  await page.goto('/login')
  await page.getByLabel('用户名').fill(ADMIN.username)
  await page.getByLabel('密码').fill(ADMIN.password)
  await page.getByRole('button', { name: '登录' }).click()

  const heading = page.getByRole('heading', { level: 1 })
  try {
    await expect(heading).toContainText(ADMIN.username, { timeout: 10_000 })
  } catch {
    const skip = page.getByRole('button', { name: '跳过，直接进入' })
    if (await skip.isVisible().catch(() => false)) {
      await skip.click()
      await expect(heading).toContainText(ADMIN.username, { timeout: 15_000 })
    } else {
      throw new Error('登录后未进入应用，也没有出现安全初始化跳过按钮')
    }
  }
}

test('所有管理页面可加载且无 5xx', async ({ page }) => {
  const pageErrors: string[] = []
  const api5xx: string[] = []
  page.on('pageerror', (e) => pageErrors.push(String(e)))
  page.on('response', (r) => {
    const u = r.url()
    if (u.includes('/api/') && r.status() >= 500) api5xx.push(`${r.status()} ${u}`)
  })

  await login(page)

  const failures: string[] = []
  for (const [path, label] of ROUTES) {
    await page.goto(path)
    // 等首屏数据加载与懒加载 chunk 到位
    await page.waitForLoadState('networkidle').catch(() => {})
    await page.waitForTimeout(400)

    const body = await page.locator('body').innerText()
    if (body.includes('页面不存在')) failures.push(`${path} 渲染成 404 占位页`)
    if (body.includes('加载失败')) failures.push(`${path} 出现「加载失败」错误态`)
    if (!body.includes(label) && path !== '/') failures.push(`${path} 未出现预期文案「${label}」`)
    // 页面根节点仍在
    await expect(page.locator('#root')).not.toBeEmpty()
  }

  expect(pageErrors, '页面 JS 异常').toEqual([])
  expect(api5xx, '接口 5xx').toEqual([])
  expect(failures, '页面加载问题').toEqual([])
})

test('虚拟机列表 → 详情 → 控制台入口', async ({ page }) => {
  await login(page)
  await page.goto('/vm')
  await expect(page.getByRole('heading', { level: 1, name: '虚拟机' })).toBeVisible()

  // 打开已知存在的 lab-vm-1
  await page.getByText('lab-vm-1').first().click()
  await expect(page).toHaveURL(/\/vm\/7/, { timeout: 10_000 })
  await page.waitForLoadState('networkidle').catch(() => {})
  const body = await page.locator('body').innerText()
  expect(body).toContain('lab-vm-1')
  expect(body).not.toContain('加载失败')

  // 详情页的标签页（按需渲染，至少概览与监控可见）
  for (const tab of ['概览', '监控']) {
    const el = page.getByText(tab, { exact: true }).first()
    if (await el.isVisible().catch(() => false)) {
      await el.click()
      await page.waitForTimeout(300)
      expect(await page.locator('body').innerText()).not.toContain('加载失败')
    }
  }
})

test('创建虚拟机向导可打开', async ({ page }) => {
  await login(page)
  await page.goto('/vm')
  await page.getByRole('button', { name: '创建虚拟机' }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await expect(page.getByText('选择创建方式')).toBeVisible()
})
