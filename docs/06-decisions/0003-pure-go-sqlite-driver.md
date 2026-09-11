# 0003. SQLite 采用纯 Go 驱动而非 CGO 驱动

- 状态：Accepted
- 日期：2026-09-11
- 决策人：<!-- TODO -->
- 关联：[`../02-architecture/TECH_STACK.md`](../02-architecture/TECH_STACK.md) · [ADR-0002](0002-use-hertz-and-gorm.md)

---

## 背景

项目需要同时支持 PostgreSQL 与 SQLite。SQLite 用于**本地开发**与**轻量部署**场景。

GORM 官方推荐的 SQLite 驱动是 `gorm.io/driver/sqlite`，其底层为 `mattn/go-sqlite3`，**必须启用 CGO**，依赖本地 C 编译器。这会带来一系列约束：

- 构建环境必须安装 gcc / clang 与完整 C 工具链
- 交叉编译（例如构建 Linux 容器镜像）需要额外配置交叉编译工具链
- 编译速度明显变慢
- 静态链接与最小化容器镜像（如 alpine / scratch）更困难

---

## 决策

SQLite 使用 **`github.com/glebarez/sqlite`** v1.11.0。

它是基于 `modernc.org/sqlite` 的纯 Go 实现，**不依赖 CGO**，并实现了 GORM 的 Dialector 接口。

---

## 理由

- **构建更简单**：`CGO_ENABLED=0` 即可编译，无需任何 C 工具链
- **交叉编译无障碍**：可直接产出 Linux / ARM 等目标平台二进制，便于容器化与边缘部署
- **编译更快**：省去 C 编译环节
- **部署更轻**：静态链接的单二进制，符合 Go 的核心部署优势
- **用法一致**：`sqlite.Open(dsn)` 与官方驱动完全相同，替换成本极低

---

## 备选方案

### 方案 A：gorm.io/driver/sqlite（CGO）

- 优势：GORM 官方推荐，社区最广，性能略高于纯 Go 实现
- 劣势：需要 CGO 与 C 工具链；交叉编译复杂；编译慢；镜像构建受限
- 否决原因：本项目的 SQLite 主要用于开发与小规模场景，性能差距不构成瓶颈，而构建与部署便利性的收益明显更大

### 方案 B：直接使用 modernc.org/sqlite

- 优势：少一层封装
- 劣势：需要自行实现 GORM 的 Dialector
- 否决原因：属于重复造轮子，而 `glebarez/sqlite` 已经完成且维护正常

---

## 影响

### 正面

- 构建与 CI 无需安装 C 工具链
- 可轻松交叉编译各平台二进制
- 容器镜像可基于 alpine / scratch 构建，体积更小

### 负面 / 代价

- 纯 Go 实现的性能略低于 CGO 版本（本项目场景下可忽略）
- 依赖树变大（引入 `modernc.org/libc`、`modernc.org/memory` 等）
- `glebarez/sqlite` 由社区维护而非 GORM 官方，需关注其维护状态与升级

### 需要的配套调整

- `internal/database` 中按驱动区分连接池策略：SQLite 固定为**单连接**，从根源规避 `database is locked`
- CI 中统一以 `CGO_ENABLED=0` 验证构建，确保不引入隐式 CGO 依赖
- 若后续性能测试表明纯 Go 实现成为瓶颈，需新建 ADR 评估替换

---

## 参考

- [glebarez/sqlite 项目主页](https://github.com/glebarez/sqlite)
- [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)
- [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3)
