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
    proxy: {
      '/api': { target: 'http://127.0.0.1:8080' },
      '/health': { target: 'http://127.0.0.1:8080' },
    },
  },
  build: {
    // 生产产物由控制面托管（ARCHITECTURE §7）。
    outDir: 'dist',
    // 体积预算见 FRONTEND §6.7：首屏 JS ≤250KB。分包由路由懒加载驱动，
    // 不在此处手工 vendor 切分——那会让改动收益难以归因。
    sourcemap: false,
  },
})
