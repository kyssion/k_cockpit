# 本地开发指南

> 状态：草稿（技术栈确定后补全）
> 最后更新：<!-- TODO: YYYY-MM-DD -->

---

## 1. 环境要求

| 依赖 | 版本要求 | 校验命令 |
|---|---|---|
| Go | 1.27 或更高 | `go version` |
| PostgreSQL | 14+（**可选**，仅在使用 postgres 驱动时需要） | `psql --version` |
| C 编译器 | **不需要** | — |
| Node.js | LTS（仅前端 `web/`，**规划中**） | `node --version` |
| pnpm | 随 Node（仅前端 `web/`，**规划中**） | `pnpm --version` |

> 项目使用纯 Go 的 SQLite 驱动，**无需 CGO 与 C 工具链**，`CGO_ENABLED=0` 即可正常构建（见 [ADR-0003](../06-decisions/0003-pure-go-sqlite-driver.md)）。
> Go 版本在 `go.mod` 中声明，依赖版本在 `go.sum` 中锁定。

---

## 2. 首次搭建

```bash
# 1. 克隆仓库
git clone <repo-url>
cd k_cockpit

# 2. 准备环境变量（默认即为 SQLite，无需额外安装数据库）
cp .env.example .env

# 3. 安装依赖
go mod download

# 4. 启动服务（默认监听 0.0.0.0:8080）
go run ./cmd/server
```

**验证搭建成功**：

```bash
curl http://127.0.0.1:8080/health
# 期望返回：{"database":"up","status":"ok"}
```

数据文件默认位于 `data/k_cockpit.db`，该目录已被 `.gitignore` 忽略。

### 使用 PostgreSQL

在 `.env` 中设置：

```bash
DB_DRIVER=postgres
DB_HOST=localhost
DB_PORT=5432
DB_NAME=k_cockpit
DB_USER=postgres
DB_PASSWORD=your_password
DB_SSLMODE=disable
```

需先自行创建数据库 `k_cockpit`，再执行 `internal/database/migrations/` 下的 SQL 迁移建表（见 [`../02-architecture/DATA_MODEL.md`](../02-architecture/DATA_MODEL.md) 第 6 节）。**服务启动不做自动建表**。

---

## 3. 常用命令

> 与 `AGENTS.md` 第 3 节保持一致，两处需同步更新。

| 用途 | 命令 |
|---|---|
| 安装依赖 | `go mod download` |
| 整理依赖 | `go mod tidy` |
| 启动开发服务 | `go run ./cmd/server` |
| 构建 | `go build -o bin/server ./cmd/server` |
| 格式化检查 | `gofmt -l .` |
| 格式化修复 | `gofmt -w .` |
| 静态检查 | `go vet ./...` |
| 单元测试 | `go test ./...` |
| 单包测试 | `go test ./internal/handler/...` |
| 覆盖率 | `go test -cover ./...` |
| 数据库迁移 | 执行 `internal/database/migrations/` 下的 SQL 迁移，并登记到 `schema_migration`（见 [`../02-architecture/DATA_MODEL.md`](../02-architecture/DATA_MODEL.md) §6） |
| 前端依赖 / 开发 / 构建（`web/`，**尚未创建**，技术栈见 [ADR-0006](../06-decisions/0006-frontend-tech-stack.md)） | `cd web && pnpm install` / `pnpm dev`（`/api` 代理到 8080） / `pnpm build` |
| 前端类型检查 / Lint | `cd web && pnpm typecheck` / `pnpm lint` |
| 前端测试 | `cd web && pnpm test`（Vitest）/ `pnpm test:e2e`（Playwright） |

---

## 4. 环境变量

- 仓库只保留 `.env.example`，真实值放本地 `.env`（已被忽略）。
- 新增变量必须同时在 `.env.example` 登记并注明用途。
- **严禁**把真实密钥、生产配置写入 `.env.example` 或任何提交到仓库的文件。
- 变量命名：全大写 + 下划线；同类变量使用统一前缀。

详见仓库根目录 `.env.example`。

---

## 5. 分支与提交

见 [`GIT_WORKFLOW.md`](GIT_WORKFLOW.md)。

---

## 6. 常见问题排查

> 排障原则、远端操作纪律、systemd 检查清单与案例复盘见 [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md)。本节只保留**本地开发环境**的现象速查。

| 现象 | 可能原因 | 解决方式 |
|---|---|---|
| <!-- TODO --> | | |

### 本地环境重置

```bash
# TODO: 清理依赖与缓存、重建数据库的标准步骤
```

---

## 7. 调试与日志

- 日志级别通过环境变量控制，本地默认 `debug`。
- 生产环境**禁止** `debug` 级别。
- 排查问题时使用 `request_id` 串联链路。
- 禁止在代码中长期保留调试输出，提交前清理。

---

## 8. IDE / 编辑器配置

项目通过 `.editorconfig` 与 `.gitattributes` 统一格式，**不依赖特定编辑器**：

- 编辑器需安装 EditorConfig 插件（多数编辑器已内置）。
- 统一换行为 LF，字符集 UTF-8。
- 缩进规则由 `.editorconfig` 定义，不要手动覆盖。
- 个人编辑器配置目录（如各 IDE 的本地配置）已在 `.gitignore` 中忽略，不会被提交。

---

## 9. 变更记录

| 日期 | 变更内容 |
|---|---|
| | 创建文档 |
