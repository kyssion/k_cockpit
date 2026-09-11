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
- **Go Web 基础框架**（Go 1.27）
  - Web 框架：CloudWeGo Hertz v0.10.6
  - ORM：GORM v1.31.2
  - 数据库：PostgreSQL（pgx v5）与 SQLite（纯 Go 驱动），通过 `DB_DRIVER` 切换
  - 配置：全部来自环境变量，启动时校验，摘要输出已脱敏
  - 示例接口：`GET /health`、`POST /api/v1/users`、`GET /api/v1/users`、`GET /api/v1/users/:id`
  - 测试：配置加载与 handler 行为，使用 SQLite 临时库，不依赖外部服务

<!--
## [0.1.0] - YYYY-MM-DD

### Added
- 首个版本：<!-- 简述 -->
-->
