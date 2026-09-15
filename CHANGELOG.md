# Changelog

本项目所有值得注意的变更都记录在此文件。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本 SemVer](https://semver.org/lang/zh-CN/)。

分类说明：
- `Added` 新增功能
- `Changed` 行为变更
- `Deprecated` 即将废弃
- `Removed` 已移除
- `Fixed` 缺陷修复
- `Security` 安全相关

---

## [Unreleased]

### Added
- 项目基础设施：`.gitignore`、`.editorconfig`、`.gitattributes`、`.env.example`
- AI 协作规范：`AGENTS.md`（遵循开放标准，不绑定任何具体工具）
- 文档体系：`docs/` 七个分区
- 协作规范：`CONTRIBUTING.md`、`SECURITY.md`、`CHANGELOG.md`
- **产品与架构文档**：功能需求（`docs/01-product/PRD.md`，11 个能力域 93 项功能与优先级）、能力地图（`docs/01-product/CAPABILITY_MAP.md`）、数据模型（`docs/02-architecture/DATA_MODEL.md`，43 张表）
- **数据库表结构**：`internal/database/migrations/0001_init_schema.sql`（43 张表 / 81 个显式索引），已建库并在 `schema_migration` 登记
- **Go Web 基础框架**（Go 1.27）
  - Web 框架：CloudWeGo Hertz v0.10.6
  - ORM：GORM v1.31.2
  - 数据库：PostgreSQL（pgx v5）与 SQLite（纯 Go 驱动），通过 `DB_DRIVER` 切换
  - 配置：全部来自环境变量，启动时校验，摘要输出已脱敏
  - 健康检查接口：`GET /health`
  - 测试：配置加载与 handler 行为，使用 SQLite 临时库，不依赖外部服务

### Removed
- 脚手架阶段的示例模型（`model.User`）与示例接口（`/api/v1/users` 系列）：与正式表结构不兼容，保留会让自动迁移给正式表加错列
- 启动时的自动迁移与 `DB_AUTO_MIGRATE` 配置项：表结构统一由 `internal/database/migrations/` 下的 SQL 迁移管理，服务启动不建表

<!--
## [0.1.0] - YYYY-MM-DD

### Added
- 首个版本：<!-- 简述 -->
-->
