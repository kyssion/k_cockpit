# k_cockpit

> <!-- TODO: 用一句话说明项目定位与目标用户 -->

[![status](https://img.shields.io/badge/status-pre--alpha-orange)]()
[![go](https://img.shields.io/badge/go-1.27-00ADD8)]()
[![license](https://img.shields.io/badge/license-TBD-lightgrey)]()

---

## 项目简介

<!-- TODO: 3-5 句话说明：解决什么问题、为谁解决、核心价值是什么 -->

## 当前状态

基础框架已可运行：HTTP 服务、双数据库支持、健康检查与示例接口均已就绪。

- [x] 版本控制与 `.gitignore`
- [x] AI 协作规范（`AGENTS.md`）
- [x] 文档体系（`docs/`）
- [x] 技术栈选型（Go 1.27 + Hertz + GORM；前端见 [ADR-0006](docs/06-decisions/0006-frontend-tech-stack.md)）
- [x] Go Web 基础框架（健康检查接口）
- [x] 表结构设计与建库（43 张表，见 `docs/02-architecture/DATA_MODEL.md`）
- [ ] 业务功能开发

详见 [`docs/01-product/ROADMAP.md`](docs/01-product/ROADMAP.md)。

---

## 技术栈

| 层 | 选型 | 版本 |
|---|---|---|
| 语言 | Go | 1.27 |
| Web 框架 | CloudWeGo Hertz | v0.10.6 |
| ORM | GORM | v1.31.2 |
| 数据库 | PostgreSQL / SQLite | — |
| SQLite 驱动 | glebarez/sqlite（纯 Go，无需 CGO） | v1.11.0 |

前端（`web/`，规划中）：Vite · React 19 · TypeScript · Tailwind CSS 4 · shadcn/ui · TanStack Query / Zustand · Vitest + Playwright · pnpm（见 [ADR-0006](docs/06-decisions/0006-frontend-tech-stack.md)）。

选型理由见 [`docs/02-architecture/TECH_STACK.md`](docs/02-architecture/TECH_STACK.md)。

---

## 快速开始

环境要求：**Go 1.27 或更高版本**。默认使用 SQLite，**无需额外安装数据库**。

```bash
# 1. 克隆仓库
git clone <repo-url>
cd k_cockpit

# 2. 准备环境变量（默认配置即为 SQLite，可直接运行）
cp .env.example .env

# 3. 安装依赖
go mod download

# 4. 启动服务（默认监听 0.0.0.0:8080）
go run ./cmd/server
```

> 表结构由 `internal/database/migrations/` 下的 SQL 迁移管理，**服务启动不做自动建表**；执行方式见 [`docs/02-architecture/DATA_MODEL.md`](docs/02-architecture/DATA_MODEL.md) 第 6 节。

### 验证服务

```bash
curl http://127.0.0.1:8080/health
# {"database":"up","status":"ok"}
```

> 业务接口尚未实现。第一个业务功能落地后，先在 [`docs/03-api/API.md`](docs/03-api/API.md) 登记接口，再在此补充调用示例。

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

### 常用命令

```bash
go build -o bin/server ./cmd/server   # 构建
gofmt -l .                            # 格式化检查
go vet ./...                          # 静态检查
go test ./...                         # 测试
go test -cover ./...                  # 测试 + 覆盖率
```

### 前端（规划中）

前端工程位于 `web/`，技术栈已由 [ADR-0006](docs/06-decisions/0006-frontend-tech-stack.md) 确认（**尚未创建**）。环境要求：Node.js LTS + pnpm；目录结构与命令见 [`docs/02-architecture/FRONTEND.md`](docs/02-architecture/FRONTEND.md) §2 与 [`docs/04-engineering/DEVELOPMENT.md`](docs/04-engineering/DEVELOPMENT.md)。

---

## 目录结构

```
k_cockpit/
├── cmd/server/            # 程序入口
├── internal/
│   ├── config/            # 环境变量配置加载与校验
│   ├── database/          # 数据库连接（PostgreSQL / SQLite 双支持）
│   │   └── migrations/    # SQL 迁移：表结构的唯一入口
│   ├── model/             # GORM 数据模型（随功能实现逐步补齐）
│   ├── handler/           # HTTP 接口实现
│   └── router/            # 路由注册
├── web/                   # 前端工程（纯 SPA，规划中，见 docs/02-architecture/FRONTEND.md §2.2）
├── docs/                  # 项目文档（入口见 docs/README.md）
├── AGENTS.md              # AI 协作规范（单一事实来源，工具无关）
├── CONTRIBUTING.md        # 贡献指南
├── CHANGELOG.md           # 变更日志
├── SECURITY.md            # 安全策略
└── .env.example           # 环境变量模板
```

---

## 文档导航

| 文档 | 内容 |
|---|---|
| [docs/README.md](docs/README.md) | 文档总索引 |
| [AGENTS.md](AGENTS.md) | AI 协作规范（必读） |
| [docs/02-architecture/TECH_STACK.md](docs/02-architecture/TECH_STACK.md) | 技术选型与理由 |
| [docs/02-architecture/ARCHITECTURE.md](docs/02-architecture/ARCHITECTURE.md) | 架构设计 |
| [docs/03-api/API.md](docs/03-api/API.md) | 接口契约 |
| [docs/04-engineering/DEVELOPMENT.md](docs/04-engineering/DEVELOPMENT.md) | 本地开发指南 |
| [docs/06-decisions/](docs/06-decisions/README.md) | 架构决策记录 |
| [CONTRIBUTING.md](CONTRIBUTING.md) | 贡献指南 |

---

## 参与贡献

请阅读 [CONTRIBUTING.md](CONTRIBUTING.md)。使用 AI 工具开发的，**必须**先阅读 [AGENTS.md](AGENTS.md)。

## 安全

发现安全问题请勿提交公开 Issue，参见 [SECURITY.md](SECURITY.md)。

## 许可

<!-- TODO: 确定开源协议，如 MIT / Apache-2.0 / 私有 -->
