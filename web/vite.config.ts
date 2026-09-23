import { fileURLToPath, URL } from 'node:url'

import tailwindcss from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
import { defineConfig } from 'vite'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  server: {
    port: 5173,
    // 开发期把 /api 与 /health 代理到控制面：既避免跨域，也让会话 Cookie
    // 在浏览器看来是同源（SameSite=Strict 才不会被拒绝）。
    //
    // 目标地址可用 KC_API_TARGET 覆盖：控制面默认端口 8080 常与其它本地
    // 服务冲突，写死的话一冲突就只能改文件。
    proxy: (() => {
      const target = process.env.KC_API_TARGET || 'http://127.0.0.1:8080'
      return {
        // 只代理 /api/v1，**不能写成 '/api'**：前缀匹配会把前端自己的页面
        // 路由（/api-keys、/api-docs）也转发给后端，于是直接打开或刷新那两个
        // 页面拿到的是后端的 404，而不是 index.html。
        '^/api/v1': { target },
        '/health': { target },
      }
    })(),
  },
  build: {
    // 生产产物由控制面托管（ARCHITECTURE §7）。
    outDir: 'dist',
    // 体积预算见 FRONTEND §6.7：首屏 JS ≤250KB。分包由路由懒加载驱动，
    // 不在此处手工 vendor 切分——那会让改动收益难以归因。
    sourcemap: false,
  },
})
