# k_cockpit

> KVM/QEMU 多宿主机虚拟化管理控制台：一个控制面纳管多台宿主机，完成虚拟机的生命周期、模板、网络、存储、用户与配额管理。

[![status](https://img.shields.io/badge/status-M5%20in%20progress-orange)]()
[![go](https://img.shields.io/badge/go-1.27-00ADD8)]()
[![license](https://img.shields.io/badge/license-TBD-lightgrey)]()

---

## 项目简介

`k_cockpit` 面向需要自建虚拟化平台、又不希望每台宿主机都暴露管理凭据的场景。它把「管理面」与「宿主机」彻底拆开：

- **控制面**（本仓库 `cmd/server`）只持有期望态、配额与任务编排，**不持有任何宿主机登录凭据**；
- **节点代理**（`cmd/agent`）部署在每台宿主机上，是唯一直接操作 libvirt / OVS / iptables / LVM 的角色，通过**反向** gRPC 长连接 + mTLS 接入，宿主机无需开放入站端口。

能力面对标开源项目 QVMConsole（见 `reference/`，仅作只读参考），不照搬其技术栈。

## 当前状态

| 阶段 | 状态 |
|---|---|
| M0 基础设施 / M1 技术选型与骨架 | 已完成 |
| M2 最小可用（底座 + 单机完整生命周期） | 已完成 |
| M3 功能完善（模板 / 网络 / 存储 / 配额 / 安全治理） | 已完成 |
| M4 稳定加固（P2 收尾 / 迁移闭环 / CI / 安全自查） | 控制面部分已完成 |
| M5 真实 agent 与正式发布 | **进行中**（`feat/agent` 分支：agent 二进制、85 项能力自报、P0–P5 执行器已真机验收） |

详见 [`docs/01-product/ROADMAP.md`](docs/01-product/ROADMAP.md) 与 [`CHANGELOG.md`](CHANGELOG.md)。

## 技术栈

| 层 | 选型 |
|---|---|
| 语言 / Web / ORM | Go 1.27 · CloudWeGo Hertz · GORM |
| 数据库 | PostgreSQL（生产）· SQLite（本地开发，纯 Go 驱动、无需 CGO） |
| 控制面 ↔ agent | gRPC 双向流 + Protocol Buffers，mTLS（控制面私有 CA 自举） |
| 前端 | React 19 · TypeScript · Vite · Tailwind CSS 4 · TanStack Query · Zustand · oxlint |
| 测试 | 标准库 testing · Vitest · Playwright |

选型理由见 [`docs/02-architecture/TECH_STACK.md`](docs/02-architecture/TECH_STACK.md)，决策历史见 [`docs/06-decisions/`](docs/06-decisions/README.md)。

---

## 快速开始

环境要求：**Go 1.27+**；前端另需 Node.js LTS + pnpm。默认使用 SQLite，**无需安装数据库**。

```bash
# 1. 准备环境变量（默认配置即为 SQLite）
cp .env.example .env

# 2. 安装依赖
go mod download

# 3. 首次建库（服务启动**不做**自动建表；SQLite 的建库入口是 e2e-init，
#    按模型建出全套表。SQL 迁移 cmd/migrate 是 PostgreSQL 专用语法）
go run ./cmd/e2e-init

# 4. 启动服务（默认监听 0.0.0.0:8080）
go run ./cmd/server
```

服务启动后会打印一次性管理员初始化令牌；首次访问通过 `/setup` 创建管理员（见 [`docs/04-engineering/DEVELOPMENT.md`](docs/04-engineering/DEVELOPMENT.md)）。

### 验证服务

```bash
curl http://127.0.0.1:8080/health
# {"status":"ok","database":"up"}
```

### 切换到 PostgreSQL

修改 `.env`：

```bash
DB_DRIVER=postgres
DB_HOST=localhost
DB_PORT=5432
DB_NAME=k_cockpit
DB_USER=postgres
DB_PASSWORD=your_password
DB_SSLMODE=disable
```

自行建库后执行显式迁移（`internal/platform/database/migrations/` 下的 SQL 是**表结构的唯一入口**）：

```bash
go run ./cmd/migrate -status   # 只看待应用清单，不改动任何东西
go run ./cmd/migrate           # 执行
```

### 节点 agent

```bash
go build -o bin/agent ./cmd/agent
bin/agent enroll -server <控制面地址>:8443 -token <一次性令牌>
bin/agent run
```

默认身份目录 `/var/lib/k_cockpit-agent` 需要 root 权限，非 root 调试时用 `-state-dir` 指定可写目录。

systemd 单元见 [`deploy/agent/k_cockpit-agent.service`](deploy/agent/k_cockpit-agent.service)。控制面启动后监听 `:8443`（mTLS），agent 主动拨入；不接 agent 时节点列表为空，所有节点操作会明确返回「节点不在线」。

### 前端

```bash
cd web && pnpm install
cd web && pnpm dev        # http://localhost:5173，自动代理 /api/v1 到 127.0.0.1:8080
```

生产构建（`pnpm build`）后把 `web/dist` 拷成控制面的 `public/`（或用 `STATIC_DIR` 指定目录），由控制面直接托管：SPA 深链回退到 `index.html`，未匹配的 `/api/*` 仍返回 JSON 404。

---

## 常用命令

```bash
# 后端
go build -o bin/server ./cmd/server
gofmt -l cmd internal        # 格式化检查
go vet ./...                 # 静态检查
go test ./...                # 单元测试

# 前端（在 web/ 下）
pnpm exec tsc -b             # 类型检查
pnpm lint                    # oxlint
pnpm test                    # Vitest
pnpm test:e2e                # Playwright 冒烟（自拉起独立控制面与 vite）
pnpm build                   # 产物在 web/dist
```

---

## 目录结构

```
k_cockpit/
├── cmd/
│   ├── server/           # 控制面入口（装配按域拆在 wire_*.go）
│   ├── agent/            # 节点代理入口（enroll / run）
│   └── migrate/          # PostgreSQL 迁移执行器
├── internal/             # 顶级目录 = 层，二级目录 = 业务域（ADR-0012）
│   ├── router/           # 路由注册与角色声明
│   ├── handler/          # HTTP 参数解析与响应
│   ├── service/          # 业务逻辑（compute / network / storage / ops / node / platform）
│   ├── repository/       # 数据库访问（分批迁入中）
│   ├── model/            # 表模型（跨域共享）
│   ├── platform/         # 基础设施（api / config / database / logging / authz / audit …）
│   ├── agent/            # 控制面 → 宿主机 的领域操作契约
│   ├── agentproto/       # gRPC 线上契约（agent.proto）
│   └── nodeagent/        # agent 侧执行器
├── web/                  # 前端（纯 SPA）
├── docs/                 # 全部项目文档（入口 docs/README.md）
├── reference/            # 只读外部参考项目（git 子模块）
└── AGENTS.md             # AI 协作规范（单一事实来源）
```

---

## 文档导航

| 文档 | 内容 |
|---|---|
| [AGENTS.md](AGENTS.md) | 项目规则、技术栈与红线（AI 与人共同必读） |
| [docs/README.md](docs/README.md) | 文档总索引与阅读路径 |
| [docs/01-product/PRD.md](docs/01-product/PRD.md) | 功能清单、编号与优先级 |
| [docs/02-architecture/ARCHITECTURE.md](docs/02-architecture/ARCHITECTURE.md) | 架构设计（控制面 / 节点代理） |
| [docs/03-api/API.md](docs/03-api/API.md) | 接口契约与清单 |
| [docs/04-engineering/DEVELOPMENT.md](docs/04-engineering/DEVELOPMENT.md) | 本地开发指南 |
| [docs/06-decisions/](docs/06-decisions/README.md) | 架构决策记录（ADR） |

## 参与贡献

请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。使用 AI 工具开发的，**必须**先阅读 [AGENTS.md](AGENTS.md)。

## 安全

发现安全问题请勿提交公开 Issue，参见 [SECURITY.md](SECURITY.md)。

## 许可

<!-- TODO: 确定开源协议，如 MIT / Apache-2.0 / 私有 -->
