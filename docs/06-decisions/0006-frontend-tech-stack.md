# 0006. 前端技术栈选型（AI 驱动）

- 状态：Accepted
- 日期：2026-09-15
- 决策人：<!-- TODO -->
- 关联：[`../02-architecture/FRONTEND.md`](../02-architecture/FRONTEND.md)（§2 工程约定 · §10 U-1/U-6）· [`../02-architecture/TECH_STACK.md`](../02-architecture/TECH_STACK.md) · [`../02-architecture/ARCHITECTURE.md`](../02-architecture/ARCHITECTURE.md) §7 · [`../05-ai/AI_DEVELOPMENT.md`](../05-ai/AI_DEVELOPMENT.md) · [`../04-engineering/TESTING.md`](../04-engineering/TESTING.md)

---

## 背景

控制面前端为**纯 SPA**：静态产物由 Go 控制面单二进制托管（[`ARCHITECTURE.md`](../02-architecture/ARCHITECTURE.md) §7），**无 SSR 需求**。产品级设计已在 [`FRONTEND.md`](../02-architecture/FRONTEND.md) 定稿（信息架构、设计系统、17 个页面、跨页交互），其 §10 U-1 明确：**技术栈待 ADR，定稿前不得开始前端实现**。

前端以 **AI 驱动开发**为主（[`AI_DEVELOPMENT.md`](../05-ai/AI_DEVELOPMENT.md)）。选型除满足功能需求外，还须满足两个 AI 特有约束：

- **语料密度**：所选库要是生态主流，AI 生成的是正确用法而不是幻觉 API；
- **唯一标准写法**：同类逻辑（状态、表单）有既定的库与范式可模仿，避免 AI 在各页面自由发挥、形态不一。

需求约束（均来自 `FRONTEND.md`）：

| 约束 | 出处 |
|---|---|
| 设计令牌驱动，业务代码禁止颜色/间距魔法值 | §2.1、§4.2 |
| 深色默认 + 浅色对等、密度开关（36/44） | §1.3、§4.8 |
| 高密度数据界面：500+ 行表格、虚拟滚动、双视图 | §5.3.1、§6.7 |
| 多通道实时（列表 2s / 详情 3s / 任务 / 宿主指标），单页最多 2 条 SSE | §6.1 |
| 重型长表单：9 步创建向导、草稿、分步校验、字段联动 | §5.3.2、§6.4 |
| 性能预算：首屏 JS ≤250KB（gzip），重依赖全部懒加载 | §6.7 |

---

## 决策

**1. 形态**：纯 SPA（CSR）。Vite 构建为静态产物，由控制面 Go 服务托管；不引入 SSR / SSG。

**2. 运行时与工程栈**（主版本为准，见第 8 条）：

| 类别 | 选型 | 说明 |
|---|---|---|
| 构建 | Vite | 按路由分包，满足性能预算 |
| 框架 | React 19 | |
| 语言 | TypeScript（strict） | 契约类型来自 [`API.md`](../03-api/API.md) |
| 样式 | Tailwind CSS 4 | CSS-first `@theme`，令牌 → utility 映射 |
| UI 基础 | shadcn/ui（Radix 底座） | 源码复制模式；只用基础组件，业务组件自研（§4.7） |
| 路由 | React Router v7（**库模式**） | 见第 3 条 |
| 服务端状态 | TanStack Query | 缓存 / 失效 / 重试；SSE 推送写入缓存 |
| 客户端状态 | Zustand | FRONTEND §4.9 五块全局 UI 状态 |
| 表格 | TanStack Table + TanStack Virtual | 排序 / 筛选 / 固定列 / 多选 / 虚拟滚动 |
| 表单 | React Hook Form + Zod | shadcn Form 的实现基础；向导与编辑表单 |
| 图表 | ECharts（按需引入 + 自研轻封装） | 见第 5 条 |
| 国际化 | i18next | 键值化自第一天开始（U-3） |
| 远程控制台 | noVNC | VNC 控制台（§5.3.4） |
| 终端 | xterm.js | 终端容器（§4.7，按需） |
| Mock | MSW | 契约先行；AI 独立于后端开发与 E2E |
| 组件测试 | Vitest + React Testing Library | 与 Vite 同源 |
| E2E 测试 | Playwright | 覆盖核心链路（`TESTING.md` 分层） |
| 包管理 | pnpm | lockfile 提交，依赖可复现 |
| 代码质量 | ESLint（flat config）+ Prettier | AI 输出一致性门禁 |

**3. 路由模式**：React Router v7 采用**库模式（Library Mode）**，不使用 Framework Mode。数据获取统一由 TanStack Query 承载，不用路由 `loader`，避免双轨。

**4. 主题与令牌**：`data-theme` 属性 + CSS 变量三层桥接——设计令牌 `--kc-*`（唯一事实来源，§4.2）→ Tailwind `@theme` 映射 → shadcn 语义变量引用 `--kc-*`。**不允许三套变量并存**。

**5. 图表**：全站图表唯一路径为 ECharts（按需引入 + 自研轻封装 hook）；**不启用 shadcn Chart（Recharts）**，避免两套图表实现并存。

**6. 状态职责划分**：TanStack Query 承担**全部服务端状态**（含 SSE 数据写入缓存）；Zustand 只承载**纯客户端 UI 状态**（§4.9 五块）；**域内私有状态不进全局 store**。

**7. AI 约束**：ESLint 禁止令牌外的色值 / 间距魔法值；`tsc --noEmit` + Vitest + Playwright 构成 AI 自检回路（`AI_DEVELOPMENT.md` §6 质量门禁）。

**8. 版本策略**：上表以**主版本**为准；具体小版本由 `web/pnpm-lock.yaml` 锁定，以落地时最新稳定版为准（与 Go 侧"版本以 `go.mod` 为准"同理）。

**随行引入的小件**（逐项理由）：

| 依赖 | 用途 |
|---|---|
| lucide-react | 图标（shadcn 默认，线性 1.5px 符合 §4.5） |
| cmdk | ⌘K 命令面板（shadcn Command 的实现基础，§5.1） |
| dnd-kit | 拖拽（创建向导引导顺序、任务栏高度，§5.3.2） |
| dayjs | 相对 / 绝对时间格式化（§4.9） |

**延后项**：SPICE 客户端（F-2-09 为 P2，届时再评估 spice-html5 或替代方案）；代码编辑器 Monaco / CodeMirror（P2，§4.7 代码编辑器容器）。

---

## 理由

- **契合令牌驱动与观感目标**：Tailwind `@theme` + shadcn 源码复制模式，组件与样式均可被直接读写修改（非黑盒），能按 §4 设计系统深度定制，达到"观感不弱于参考项目"（§1.3）。
- **a11y 基线达标**：Radix 底座提供键盘操作、焦点管理、ARIA 语义，满足 §7 的无障碍要求。
- **性能预算可实现**：Vite 路由分包 + 重依赖二次懒加载满足 §6.7；ECharts Canvas 渲染 + 增量更新满足"实时 4 图 + 大数据点降采样"（§4.7、§6.7）。
- **AI 驱动适配性**：所选均为 React 生态语料最密的主流方案；且库提供"唯一标准写法"——store 是单例形态、表单是 RHF 范式，AI 照现有代码模仿即可，输出形态一致，边界情况（异步校验、脏状态、重渲染）由库兜底。
- **部署形态匹配**：纯 SPA 静态产物与"单二进制 + 前端静态资源"（§7）无缝；开发期前端 dev server 代理 `/api` 到控制面。

**关于"减少依赖让 AI 更自由"的反向论证**（被评估过，见方案 C）：AI 驱动下最大的风险不是"写不出"，而是"每次写得不一样"。没有库约束时，状态管理会在 Context / useReducer / 页面内状态之间漂移；表单逻辑等于让 AI 每页重新发明一个自制 RHF。**库 = 约定 = 输出可预测**，净效果是减少不一致与边界遗漏。

---

## 备选方案

### 方案 A：Vue 3 + Naive UI / Element Plus

- 优势：国内运维面板常见，高密度表格组件开箱即用；团队可能更熟。
- 劣势：组件库为黑盒，令牌化定制深度受限；AI 语料与组件生态不及 React + shadcn 线。
- **否决原因**：§2.1「组件只消费设计令牌」与 §1.3 的观感目标需要源码级控制能力。

### 方案 B：Ant Design

- 优势：企业级密集数据组件最全（ProTable 等），中文文档完善。
- 劣势：深度定制需覆盖其整套设计 token，观感容易停留在"标准后台"，与"不弱于参考项目"有差距；AI 生成时易混淆 v4 / v5 API。
- **否决原因**：观感差异化与令牌体系一致性；且组件粒度偏大，叠加 §4.7 业务组件自研后收益有限。

### 方案 C：不引入状态 / 表单库，由 AI 手写实现

- 优势：直接依赖更少。
- 劣势：五块跨页面状态用 Context 会在 SSE 2~3s 高频推送下引起大面积重渲染（撞 §6.7 性能预算），且需手写 selector、订阅与偏好持久化（§6.6）；表单要自研受控组件 + 校验 + 联动 + 草稿（§5.3.2、§6.4），边界情况必然遗漏且每页遗漏不一致。
- **否决原因**：见"理由"末段——AI 驱动下"有库约束"反而提高一致性；此处否决的是"无约束的自由发挥"，不是"少写代码"。

### 方案 D：React Router v7 Framework Mode（含 SSR）

- 优势：官方主推形态，文件路由 + loader 开箱即用。
- 劣势：纯内网控制台无 SSR 需求；`loader` 与 TanStack Query 职责重叠形成双轨；与 Go 静态托管、离线部署形态不匹配。
- **否决原因**：SPA 场景下库模式足够，Framework Mode 只增加复杂度。

### 方案 E：图表使用 Recharts（shadcn Chart 默认）

- 优势：与 shadcn 生态顺滑、声明式 React 写法。
- 劣势：SVG 渲染，在工作台"实时 4 图 + 按设备筛选 + 时间范围切换"（§5.2）场景下有性能风险；降采样后仍需高频重渲。
- **否决原因**：监控场景优先 Canvas 方案；图表需求（图例、悬浮读数、IOPS↔吞吐切换）ECharts 开箱即用。

---

## 影响

### 正面

- 设计令牌、a11y、性能预算、实时通道四类硬需求均有明确对应方案，可直接进入实现。
- AI 生成有标准范式可循（store 单例、RHF 表单、Query 键约定），一致性可被 lint 与类型检查强制。
- 全部选型为主流开源方案，许可均为 MIT 系，无传染性协议风险。

### 负面 / 代价

- 运行时直接依赖约 14 个，供应链面积增大；由 `pnpm-lock.yaml` 锁定并经依赖准入流程评估（`TECH_STACK.md` §5）。
- shadcn/ui 默认观感偏轻亮，需按 §4 做令牌覆盖与密度适配，属额外工作量。
- 新增独立前端工具链（Node / pnpm），CI 需增加前端流水线（lint / 类型检查 / 单测 / E2E）。
- ECharts 需自研约 30 行的轻封装 hook（不引入 `echarts-for-react` 包装层，控制依赖与可读性）。

### 需要的配套调整

| 文档 / 代码 | 调整 |
|---|---|
| `docs/06-decisions/README.md` | ✅ 索引登记本 ADR |
| `docs/02-architecture/FRONTEND.md` | ✅ §2 改为已定技术栈 + 工程约定与目录结构；页首状态、§10 U-1 / U-6、§11 变更记录同步 |
| `docs/02-architecture/TECH_STACK.md` | ✅ 增加前端选型行；§3.1 补图表与状态管理取舍 |
| `docs/02-architecture/ARCHITECTURE.md` | ✅ §8 已知限制移除"前端技术栈未定" |
| `AGENTS.md` | ✅ §2 技术栈表、§3 常用命令（前端）、§4 目录结构（`web/`） |
| `README.md` | ✅ 技术栈表、目录结构、快速开始（前端） |
| `docs/04-engineering/DEVELOPMENT.md` | ✅ 环境要求（Node / pnpm）与前端命令 |
| `docs/04-engineering/TESTING.md` | ✅ 前端测试分层与范围 |
| `CHANGELOG.md` | ✅ Unreleased 记录本决策 |
| `web/` 工程创建 | ⬜ 待执行（本 ADR 已 Accepted；目录结构见 `FRONTEND.md` §2.2） |

---

## 参考

- 前端产品级设计：[`../02-architecture/FRONTEND.md`](../02-architecture/FRONTEND.md)
- 部署形态（单二进制 + 静态资源）：[`../02-architecture/ARCHITECTURE.md`](../02-architecture/ARCHITECTURE.md) §7
- AI 驱动规范与质量门禁：[`../05-ai/AI_DEVELOPMENT.md`](../05-ai/AI_DEVELOPMENT.md)
- shadcn/ui 对 Tailwind v4 与 React 19 的支持状态：<https://ui.shadcn.com/docs/tailwind-v4>
- React Router v7 使用模式（Library / Data / Framework）：<https://reactrouter.com/start/modes>
