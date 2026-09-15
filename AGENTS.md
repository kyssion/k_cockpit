# AGENTS.md

> 本文件是 **AI 编码代理的单一事实来源（Single Source of Truth）**。
> 采用工具无关的 `AGENTS.md` 开放约定，任何具备文件读取能力的 AI 编码工具都可直接使用，**不绑定任何具体产品或平台**。
>
> 维护约定：项目规则变更时**只改本文件**。不要在仓库内创建各工具专属的规则副本或配置文件，避免规则分叉与工具锁定。工具如何接入见 [`docs/05-ai/AI_DEVELOPMENT.md`](docs/05-ai/AI_DEVELOPMENT.md)。

---

## 1. 项目概览

| 项目 | 内容 |
|---|---|
| 名称 | `k_cockpit` |
| 一句话描述 | <!-- TODO: 一句话说明这个项目做什么 --> |
| 当前阶段 | 初始化（Pre-alpha） |
| 主要负责人 | <!-- TODO: 填写 owner + 联系方式 --> |
| 需求文档 | [`docs/01-product/PRD.md`](docs/01-product/PRD.md) |
| 技术方案 | [`docs/02-architecture/ARCHITECTURE.md`](docs/02-architecture/ARCHITECTURE.md) |

---

## 2. 技术栈

> 变更技术栈必须同步更新本表格、[`docs/02-architecture/TECH_STACK.md`](docs/02-architecture/TECH_STACK.md)，并为重大选型新建 ADR。

| 层 | 选型 | 版本 | 备注 |
|---|---|---|---|
| 架构形态 | 控制面 + 节点代理（agent） | — | 每台宿主机只部署轻量 agent；控制面**不持有**宿主机登录凭据（[ADR-0005](docs/06-decisions/0005-control-plane-node-agent-architecture.md)） |
| 节点代理 | 与控制面同仓库构建的独立二进制 | — | systemd 托管；节点侧**唯一**直接操作 libvirt/KVM 的角色 |
| 控制面 ↔ agent 协议 | 领域操作 + 任务指令 / 进度流的 RPC 长连接 | **待定** | agent **反向**长连接，宿主机无需开放入站端口；不转发 libvirt 原始 RPC |
| 节点接入鉴权 | 一次性注册令牌 + mTLS 客户端证书 | — | 控制面不持有宿主机登录凭据 |
| 语言 | Go | 1.27 | 版本以 `go.mod` 为准 |
| Web 框架 | CloudWeGo Hertz | v0.10.6 | 基于 netpoll，兼容 net/http |
| ORM | GORM | v1.31.2 | 已关闭默认事务，使用单数表名 |
| 数据库 | PostgreSQL / SQLite | — | 由 `DB_DRIVER` 切换 |
| PostgreSQL 驱动 | gorm.io/driver/postgres | v1.6.2 | 底层为 pgx v5 |
| SQLite 驱动 | github.com/glebarez/sqlite | v1.11.0 | 纯 Go 实现，**无需 CGO** |
| 配置 | 环境变量 + godotenv | v1.5.1 | godotenv 仅用于本地加载 `.env` |
| 测试 | 标准库 testing + Hertz `ut` | — | 用 SQLite 临时文件，不依赖外部服务 |
| 前端框架 | React | 19 | 纯 SPA；构建产物由控制面静态托管 |
| 前端语言 | TypeScript（strict） | — | 完整选型见 [ADR-0006](docs/06-decisions/0006-frontend-tech-stack.md) |
| 前端构建 / 样式 / UI | Vite · Tailwind CSS 4 · shadcn/ui | — | 设计令牌 `--kc-*` 三层桥接，业务代码禁魔法值 |
| 前端路由 / 状态 | React Router v7（库模式）· TanStack Query · Zustand | — | 服务端状态归 Query，UI 状态归 Zustand |
| 前端包管理 / 测试 | pnpm · Vitest + Playwright | — | 锁文件 `web/pnpm-lock.yaml`；E2E 覆盖核心链路 |

**AI 注意**：
- 版本号一律以 `go.mod` 为准，不要凭记忆填写。
- 前端版本号一律以 `web/pnpm-lock.yaml` 为准；前端工程结构与约定见 [`docs/02-architecture/FRONTEND.md`](docs/02-architecture/FRONTEND.md) §2。
- 新增第三方依赖必须先说明理由并获确认。
- 关键选型的历史理由见 [`docs/06-decisions/`](docs/06-decisions/README.md)，**不得违背既有 ADR**。

---

## 3. 常用命令

> AI 执行任何构建/测试前，以本表为准。

```bash
# 安装依赖
go mod download

# 本地开发（默认走 SQLite，无需预先准备数据库）
cp .env.example .env
go run ./cmd/server

# 构建
go build -o bin/server ./cmd/server

# 格式化检查（列出未格式化的文件）
gofmt -l cmd internal

# 静态检查
go vet ./...

# 单元测试
go test ./...

# 带覆盖率测试
go test -cover ./...

# 单个包测试
go test ./internal/handler/...
```

前端（`web/`，**尚未创建**；技术栈见 [ADR-0006](docs/06-decisions/0006-frontend-tech-stack.md)，脚本名以落地后的 `web/package.json` 为准）：

```bash
cd web
pnpm install      # 安装依赖
pnpm dev          # 本地开发（/api 代理到控制面 8080）
pnpm build        # 构建产物（由控制面托管）
pnpm typecheck    # 类型检查
pnpm lint         # Lint
pnpm test         # 单元与组件测试
pnpm test:e2e     # E2E（Playwright）
```

**本地运行提示**：默认配置使用 SQLite，数据文件写入 `data/`。表结构由 [`internal/database/migrations/`](internal/database/migrations/) 下的 SQL 迁移管理（**服务启动不做自动迁移**），执行方式见 [`docs/02-architecture/DATA_MODEL.md`](docs/02-architecture/DATA_MODEL.md) 第 6 节。

---

## 4. 目录结构约定

```
k_cockpit/
├── cmd/server/            # 程序入口（main 包）
├── internal/              # 私有代码，外部模块不可导入
│   ├── config/            # 配置加载与校验
│   ├── database/          # 数据库连接、驱动切换与 SQL 迁移（migrations/）
│   ├── handler/           # HTTP 接口实现（测试同目录）
│   └── router/            # 路由注册
├── web/                   # 前端工程（纯 SPA，规划中；结构与约定见 docs/02-architecture/FRONTEND.md §2.2）
├── reference/             # 只读的外部参考项目（git 子模块，见 docs/08-reference/README.md）
├── docs/                  # 所有项目文档（见 docs/README.md）
│   ├── 01-product/        # 产品：需求、路线图
│   ├── 02-architecture/   # 架构：设计、技术栈、数据模型
│   ├── 03-api/            # 接口契约
│   ├── 04-engineering/    # 工程规范：编码、测试、Git 流程
│   ├── 05-ai/             # AI 协作规范与提示词
│   ├── 06-decisions/      # ADR 架构决策记录（只增不改）
│   ├── 07-specs/          # 功能规格（一功能一文件）
│   └── 08-reference/      # 外部参考项目资料：功能与实现结论、工程结构
└── AGENTS.md              # 本文件
```

**规则**：
- 文档一律放 `docs/` 下，**不要在根目录散落新的 `.md` 文件**（根目录仅保留 `README.md`、`AGENTS.md`、`CONTRIBUTING.md`、`CHANGELOG.md`、`SECURITY.md`）。
- 前端代码一律放 `web/`；前端工程结构、组件边界与状态职责见 [`docs/02-architecture/FRONTEND.md`](docs/02-architecture/FRONTEND.md) §2。
- 新增架构决策 → 在 `docs/06-decisions/` 新建 ADR，**不要修改历史 ADR**。
- **不要在仓库内创建绑定特定 AI 工具或平台的配置文件**（各产品专属的规则目录等）。AI 规范统一以本文件为准；工具接入方式见 `docs/05-ai/AI_DEVELOPMENT.md`。
- 新增文档后，需在 `docs/README.md` 的对应索引中登记。
- `reference/` 存放只读的外部参考项目（git 子模块），**仅作功能与实现参考**：借鉴其功能设计与实现机制，**不照搬其技术栈**，其架构限制不构成我们的设计约束。使用原则、各项目的功能/实现结论与工程结构资料、同步维护约定统一放 [`docs/08-reference/`](docs/08-reference/README.md)。

---

## 5. 编码规范（AI 必须遵守）

- **不过度设计（YAGNI）**：只为当前明确的需求设计。不预留假想的扩展点、不为「以后可能需要」增加配置项与抽象层、不实现用不到的功能。删掉一段代码若功能不受影响，它就是多余的。
- **不过度封装**：不为一次性逻辑创建抽象。中间层、包装器、管理器、设计模式，**必须能说清它解决的具体问题**，说不清就是过度封装。直接清晰的代码优于「灵活」的间接层。
- **性能与资源占用**：避免重复计算、循环内查库（N+1）、全量加载、无界缓存、大对象无谓拷贝。**不做未经测量的优化**，但也不写出明显低效的实现；性能敏感路径需给出验证数据。
- **可读性优先**：任何抽象与技巧都以不损害可读性为前提。判断标准是「不了解背景的人能否在几分钟内读懂」。可读性优先于「优雅」与「聪明」。
- **最小改动原则**：只改与任务相关的代码，不顺手重构无关文件、不批量改格式。
- **不留下半成品**：提交前确保代码能编译/运行，不提交注释掉的死代码。
- **禁止臆造 API**：不存在的函数、库、配置项一律不准编造，不确定就查证或询问。
- **错误处理**：不使用吞异常的写法（如空 `catch`），必须有明确处理或向上抛出。
- **命名**：Go 风格（导出 `PascalCase`、非导出 `camelCase`、缩写保持大小写一致如 `ID` / `URL`），详见 [`docs/04-engineering/CODING_STANDARDS.md`](docs/04-engineering/CODING_STANDARDS.md) 第 11 节。
- **注释**：解释「为什么」，不复述「做了什么」。公共 API 必须有文档注释。
- **依赖**：新增第三方依赖**必须先说明理由并获确认**，不得擅自引入。
- **系统资源操作**：禁止硬编码系统账号、属主、设备路径与服务名，必须「探测 + 回退」（例如 QEMU 属主在不同发行版是 `libvirt-qemu:kvm` 或 `qemu:qemu`）。
- **杀进程 / 删资源**：执行 `kill`、`pkill`、批量删除前必须校验归属（PID 文件、systemd unit、进程名 + 参数指纹），**禁止仅凭端口/IP 占用**判断「这是别人的进程」。
- **自愈逻辑**：启动期与周期性的 `Restore*` / `Reconcile*` / `Ensure*` 必须**幂等**，且单步失败只降级告警、不阻断主流程。
- **外部状态先检查**：不可变标记（`chattr +i`）、链式依赖、外部快照链等，必须先探测再操作，并给出可执行的修复提示。
- **库表变更**：索引冲突、数据去重、枚举回填等必须写**显式迁移**，不能只依赖 ORM 的自动迁移。
- **前端代码**：组件只消费设计令牌（禁止颜色/间距魔法值）；组件边界以 [`docs/02-architecture/FRONTEND.md`](docs/02-architecture/FRONTEND.md) §4.7 清单与 `web/src/components/` 为准；完整工程约定见该文档 §2。

详细规范见 [`docs/04-engineering/CODING_STANDARDS.md`](docs/04-engineering/CODING_STANDARDS.md)；排障与运行环境操作见 [`docs/04-engineering/TROUBLESHOOTING.md`](docs/04-engineering/TROUBLESHOOTING.md)。

---

## 6. AI 工作流程

每次任务的推荐流程：

1. **理解**：先读 `AGENTS.md` → 相关 `docs/` 文档 → 现有代码，再动手。
2. **规划**：多步骤任务先列出步骤清单（TODO），再逐项执行。
3. **实现**：遵循最小改动原则，写代码的同时补测试。
4. **自检**：运行 lint / 测试，修复自己引入的问题。
5. **说明**：汇报时说明「改了什么、为什么、怎么验证」，不要只贴大段代码。

**任务粒度**：一次任务聚焦一个功能点。若需求过大，先拆解并在 `docs/07-specs/` 写规格，再实现。

**上下文边界**：不要读取或修改 `node_modules`、`dist`、`build`、`.git` 等目录；不要提交任何密钥、令牌或真实用户数据。

**运行环境操作**：涉及宿主机 / 远端环境的排障与变更（服务、网络、防火墙、存储、数据库），必须遵循 [`docs/04-engineering/TROUBLESHOOTING.md`](docs/04-engineering/TROUBLESHOOTING.md)：只读诊断优先、变更最小且可回滚、变更后做业务侧真实验证并说明遗留影响。

---

## 7. 禁止事项（红线）

- 禁止提交任何形式的**密钥、令牌、证书、真实凭据**（含测试用的真实账号）。
- 禁止修改 `main` 分支保护设置、禁止绕过 CI 校验合并。
- 禁止在未经确认的情况下执行**破坏性操作**（`rm -rf`、强制推送、数据库删除、`DROP` 语句）。
- 禁止引入 GPL/AGPL 等**传染性协议**依赖而不说明。
- 禁止把真实用户数据、生产数据写入测试用例或提交到仓库。
- 禁止在无说明的情况下大幅重写他人已有代码。
- **禁止修改 `reference/` 下参考项目的任何内容**（代码、文档、配置、子模块提交指针等）：参考项目仅作只读查阅，需要调整内容时到对应上游仓库修改后同步，不得在 `k_cockpit` 内改动。
- **禁止在未完成只读诊断、未确认回滚方式的情况下修改运行环境**（宿主机服务、网络规则、防火墙、数据库结构）。
- **禁止把真实 IP、账号、密码、密钥写入排障记录、文档或提交**。

---

## 8. 文档维护责任

AI 在完成以下改动时，**有义务同步更新对应文档**：

| 改动类型 | 需同步更新 |
|---|---|
| 新增/变更需求 | `docs/01-product/PRD.md` |
| 架构或模块划分变化 | `docs/02-architecture/ARCHITECTURE.md` |
| 接口变更 | `docs/03-api/API.md` |
| 重大技术选型 | 新建 `docs/06-decisions/ADR-xxxx-*.md` |
| 影响用户的变更 | `CHANGELOG.md` |
| 项目规则变化 | **本文件 `AGENTS.md`** |
| 参考项目（`reference/`）更新 | `docs/08-reference/README.md` 及各参考项目资料（`docs/08-reference/<项目>/`） |
| 排障与运行环境经验 | `docs/04-engineering/TROUBLESHOOTING.md` |
