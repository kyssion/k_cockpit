/**
 * 冒烟 E2E：登录与导航这条「面板从零到能用」的最短链路。
 *
 * 选这四条的理由：它们坏掉时**用户连备用入口都没有**（登录页打不开、
 * 登不进、进来后哪里都去不了），而其余页面的问题至少还能在登录之后
 * 被人发现。更深的链路（创建 / 电源 / 迁移）依赖真实 agent，按
 * TESTING.md 的口径挂在 M5 验收。
 */
import { expect, test } from '@playwright/test'

import { E2E_ADMIN } from './admin'

/** 登录并越过首次的安全初始化引导（可跳过，F-1-08），落到工作台。 */
async function loginAsAdmin(page: import('@playwright/test').Page) {
  await page.getByLabel('用户名').fill(E2E_ADMIN.username)
  await page.getByLabel('密码').fill(E2E_ADMIN.password)
  await page.getByRole('button', { name: '登录' }).click()

  // 首次登录会先落在安全初始化引导页（可跳过）；同一次冒烟里后续登录
  // 则直接进工作台——两条都是真实路径，各自都该走得通。
  const heading = page.getByRole('heading', { level: 1 })
  try {
    await expect(heading).toContainText(E2E_ADMIN.username, { timeout: 8_000 })
  } catch {
    const skip = page.getByRole('button', { name: '跳过，直接进入' })
    await skip.click()
    // 工作台标题是问候语（“下午好，xxx”），用户名出现即认为进入应用。
    await expect(heading).toContainText(E2E_ADMIN.username, { timeout: 15_000 })
  }
}

test.beforeEach(async ({ page }) => {
  await page.goto('/login')
})

test('登录页渲染：表单与提交按钮可见', async ({ page }) => {
  await expect(page.getByLabel('用户名')).toBeVisible()
  await expect(page.getByLabel('密码')).toBeVisible()
  await expect(page.getByRole('button', { name: '登录' })).toBeEnabled()
})

test('错误密码被拒绝，且错误可见', async ({ page }) => {
  await page.getByLabel('用户名').fill(E2E_ADMIN.username)
  await page.getByLabel('密码').fill('wrong-password-123')
  await page.getByRole('button', { name: '登录' }).click()

  // 后端拒绝而不是表单前端校验挡下——这是在测服务端链路。
  await expect(page.getByRole('alert')).toBeVisible({ timeout: 10_000 })
  // 仍然停在登录页。
  await expect(page).toHaveURL(/\/login/)
})

test('管理员登录（含首次引导的跳过）并进入工作台', async ({ page }) => {
  await loginAsAdmin(page)
})

test('登录后可导航到虚拟机列表', async ({ page }) => {
  await loginAsAdmin(page)

  await page.getByRole('link', { name: '虚拟机' }).first().click()
  await expect(page.getByRole('heading', { level: 1, name: '虚拟机' })).toBeVisible()
})

test('创建虚拟机向导可打开（真实浏览器的挂载守卫）', async ({ page }) => {
  await loginAsAdmin(page)

  await page.getByRole('link', { name: '虚拟机' }).first().click()
  await page.getByRole('heading', { level: 1, name: '虚拟机' }).waitFor()
  // Modal 的挂载同步曾在真实浏览器失效而 jsdom 单测全绿——这条路径由
  // 真实浏览器守卫：点击后对话框（role=dialog）必须出现。
  await page.getByRole('button', { name: '创建虚拟机' }).click()
  await expect(page.getByRole('dialog')).toBeVisible()
  await expect(page.getByText('选择创建方式')).toBeVisible()
})
