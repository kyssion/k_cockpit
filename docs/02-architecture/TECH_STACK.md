# 技术选型

> 状态：生效（后端 / 前端 / agent 协议均已定稿；选型以 `AGENTS.md` 与 `go.mod`、`web/pnpm-lock.yaml` 为准）
> 最后更新：2026-11-05

> **重要**：未在本表登记的选型，AI 代理**不得臆造**框架、库与版本号。
> 每项关键选型都应在 [`../06-decisions/`](../06-decisions/README.md) 中记录一条 ADR。

---

## 1. 选型原则

- **匹配问题**：选择能解决本项目实际问题的技术，而非最新或最流行的。
- **成熟稳定**：优先选择生态成熟、文档完善、社区活跃的方案。
- **团队可控**：团队能维护、能排查问题；避免引入无人能懂的「黑盒」。
- **可替换性**：核心能力通过抽象层隔离，降低替换成本。
- **协议合规**：确认开源协议允许商业使用，避免传染性协议。
- **长期成本**：评估学习成本、运维成本与升级路径。

---

## 2. 选型清单

| 类别 | 选型 | 版本 | 理由 | ADR |
|---|---|---|---|---|
| 编程语言 | Go | 1.27 | 静态编译、并发模型简洁、部署无运行时依赖 | |
| Web 框架 | CloudWeGo Hertz | v0.10.6 | 高性能（netpoll）、API 简洁、与 `net/http` 兼容 | [0002](../06-decisions/0002-use-hertz-and-gorm.md) |
| ORM | GORM | v1.31.2 | 生态成熟、支持多数据库 | [0002](../06-decisions/0002-use-hertz-and-gorm.md) |
| 数据库 | PostgreSQL | 14+ | 主力生产库，事务与类型系统完善 | |
| 数据库 | SQLite | 3.x | 本地开发与测试，零运维；生产不使用 | [0003](../06-decisions/0003-pure-go-sqlite-driver.md) |
| PostgreSQL 驱动 | gorm.io/driver/postgres | v1.6.2 | GORM 官方驱动，底层 pgx v5 | |
| SQLite 驱动 | github.com/glebarez/sqlite | v1.11.0 | 纯 Go 实现，无需 CGO，可交叉编译 | [0003](../06-decisions/0003-pure-go-sqlite-driver.md) |
| 配置管理 | 环境变量 + godotenv | v1.5.1 | 十二要素应用；godotenv 只负责本地加载 `.env` | |
| 日志 | 标准库 `log` + Hertz `hlog` + GORM logger | — | 不引入重型日志库 | |
| 测试 | 标准库 `testing` + Hertz `ut` | — | 不额外引入断言库，减少依赖 | |
| 格式化 / 静态检查 | gofmt / go vet | 随 Go | 官方标准 | |
| 构建工具 | go build | 随 Go | 单文件产出 | |
| 包管理 | Go Modules | 随 Go | 官方方案 | |
| 部署方式 | 单二进制 + systemd | — | agent 单元见 `deploy/agent/k_cockpit-agent.service`；容器编排尚未启用 | |
| CI/CD | GitHub Actions | — | 后端 gofmt / vet / test + 前端 typecheck / lint / test / build + E2E 冒烟 | |
| 缓存 | 未使用 | — | 当前无缓存依赖，需要时再选型 | |
| 消息队列 | 未使用 | — | 任务队列基于库表实现，无外部 MQ | |
| 接口风格 | REST（JSON）+ SSE / WebSocket | — | 详见 `../03-api/API.md` | |
| 架构形态 | 控制面 + 节点代理（agent） | — | 控制面保持平台无关；宿主侧操作与能力探测全部下沉到节点 | [0005](../06-decisions/0005-control-plane-node-agent-architecture.md) |
| 节点代理 | 与控制面同仓库构建的独立二进制（systemd 托管） | — | 节点侧只需一个进程，无前端、无独立数据库 | [0005](../06-decisions/0005-control-plane-node-agent-architecture.md) |
| 控制面 ↔ agent 协议 | gRPC 双向流 + Protocol Buffers | ProtocolVersion 1 | agent 反向长连接，宿主机不开入站端口；线上契约唯一来源是 `internal/agentproto/agent.proto`，兼容承诺 N-1 | [0005](../06-decisions/0005-control-plane-node-agent-architecture.md) · [0013](../06-decisions/0013-remove-mock-transport.md) |
| 节点接入鉴权 | 一次性注册令牌 + mTLS 客户端证书 | — | 控制面**不持有**宿主机登录凭据 | [0005](../06-decisions/0005-control-plane-node-agent-architecture.md) |
| 前端框架 / 语言 | React 19 + TypeScript（strict） | — | 纯 SPA；版本以 `web/pnpm-lock.yaml` 为准 | [0006](../06-decisions/0006-frontend-tech-stack.md) |
| 前端构建 / 样式 / UI | Vite · Tailwind CSS 4（CSS-first）· 自研组件 | — | 组件在 `web/src/components/common/`；不使用 shadcn/ui / Radix；设计令牌 `--kc-*` 三层桥接 | [0006](../06-decisions/0006-frontend-tech-stack.md) |
| 前端路由 / 状态 | React Router v8（库模式）· TanStack Query · Zustand | — | 服务端状态归 Query，UI 状态归 Zustand | [0006](../06-decisions/0006-frontend-tech-stack.md) |
| 前端表格 / 表单 / 图表 | 自研轻量实现 | — | ADR-0006 曾评估 TanStack Table / RHF + Zod / ECharts，**均未引入**；需求出现时按 §5 重新评估 | [0006](../06-decisions/0006-frontend-tech-stack.md) |
| 前端国际化 / 控制台 | 自研键值表（通用词，渐进迁移）· noVNC | — | `i18next` 未引入；业务页面文案仍为中文，见 `FRONTEND.md` | [0006](../06-decisions/0006-frontend-tech-stack.md) |
| 前端测试 / 包管理 / lint | Vitest + React Testing Library · Playwright · pnpm · oxlint | — | 组件级 + E2E 分层；E2E 目前为登录/导航冒烟 | [0006](../06-decisions/0006-frontend-tech-stack.md) |

---

## 3. 候选项对比

### 3.1 前端技术栈

候选对比与取舍理由（Vue 3 + Naive UI、Ant Design、不引入状态/表单库自研、React Router Framework Mode、Recharts 等方案的否决原因）见 [ADR-0006](../06-decisions/0006-frontend-tech-stack.md)「备选方案」节，本文不重复。

### 3.2 其它类别

暂无其它待选类别；新增类别按 §5 流程评估并记 ADR。

---

## 4. 版本管理策略

- **语言/运行时版本**：确定后由版本锁定文件（如语言原生的版本声明文件）统一管理，避免「我这能跑」问题。
- **依赖版本**：使用语义化版本，锁定文件提交到仓库以保证依赖可复现。
- **升级节奏**：每季度评估一次依赖升级。

---

## 5. 第三方依赖准入

引入新依赖前必须评估：

- [ ] 是否标准库或已有依赖可满足？
- [ ] 最近一次发布是否在合理时间内（活跃度）？
- [ ] 是否有未修复的高危 CVE？
- [ ] 协议是否允许本项目使用？
- [ ] 是否会显著增大体积或启动开销？
- [ ] 是否有多个维护者（单点依赖风险）？

**流程**：在 PR 描述中说明评估结论，由 Reviewer 确认；项目规则要求新增依赖先说明理由并获确认。

---

## 6. 变更记录

| 日期 | 变更内容 | 关联 ADR |
|---|---|---|
| | 创建文档 | |
| 2026-09-15 | 新增「架构形态」「节点代理」「控制面 ↔ agent 协议」「节点接入鉴权」四行（当时协议实现与版本标注为待定） | [0005](../06-decisions/0005-control-plane-node-agent-architecture.md) |
| 2026-09-15 | 新增前端技术栈六行；§3.1 补前端候选对比指向 | [0006](../06-decisions/0006-frontend-tech-stack.md) |
| 2026-11-05 | 按实际落地修订：协议定稿为 gRPC + Protobuf（ProtocolVersion 1）、部署/CI/缓存/MQ 去 TODO、前端未引入的选型显式标注、清理占位章节 | [0013](../06-decisions/0013-remove-mock-transport.md) |
