import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { App } from './App'
import './index.css'

const container = document.getElementById('root')
if (!container) {
  // 挂载点缺失说明 index.html 与模板不一致，属构建配置问题；
  // 静默失败会让页面一片空白且没有任何线索。
  throw new Error('未找到挂载点 #root，请检查 index.html')
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
)
