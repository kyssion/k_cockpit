/**
 * 测试环境的全局装配。
 *
 * 只做两件事：断言扩展（toBeInTheDocument 等）与全局还原。不做更多——
 * setup 里的全局魔法越少，单个用例失败时越容易定位是业务问题还是环境
 * 问题。
 */
import '@testing-library/jest-dom/vitest'

import { afterEach, vi } from 'vitest'
import { cleanup } from '@testing-library/react'

afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
})
