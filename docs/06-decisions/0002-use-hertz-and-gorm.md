# 0002. 采用 Hertz 与 GORM 作为 Web 与 ORM 框架

- 状态：Accepted
- 日期：2026-09-11
- 决策人：<!-- TODO -->
- 关联：[`../02-architecture/TECH_STACK.md`](../02-architecture/TECH_STACK.md)

---

## 背景

项目需要选定 Go 的 Web 框架与数据库访问方案。约束条件：

- 需要同时支持 **PostgreSQL 与 SQLite** 两种数据库
- 对 HTTP 吞吐有要求，但不追求极致性能
- 项目处于起步阶段，需要快速搭建可运行骨架
- 团队需要充足的文档与社区支持
- 需遵守项目规范：**不过度设计、不过度封装**，不自研通用能力

---

## 决策

- Web 框架采用 **CloudWeGo Hertz** v0.10.6
- ORM 采用 **GORM** v1.31.2
- 数据库驱动：PostgreSQL 用 `gorm.io/driver/postgres`，SQLite 用 `github.com/glebarez/sqlite`（见 [ADR-0003](0003-pure-go-sqlite-driver.md)）

---

## 理由

### 选择 Hertz

- 基于自研 netpoll 网络库，性能显著优于标准库路由方案
- API 直观：`h.GET(path, handler)`，handler 签名为 `func(context.Context, *app.RequestContext)`
- 内置常用能力，无需自行实现：参数绑定（`c.Bind`）、路由分组、优雅退出、日志（hlog）
- 兼容 `net/http`，必要时可切换网络库，降低锁定风险
- 中文文档完善

### 选择 GORM

- Go 生态中使用最广的 ORM，社区资源充足
- 一套代码支持多种数据库，PostgreSQL 与 SQLite 切换成本极低
- 提供 `AutoMigrate`，适合起步阶段快速迭代表结构
- 保留原生 SQL 逃生舱，复杂查询不会被 ORM 卡住

---

## 备选方案

### Web 框架：Gin

- 优势：生态最大，中间件最丰富，团队熟悉度可能更高
- 劣势：性能弱于 Hertz；路由分组、绑定等能力相当
- 否决原因：本项目对性能有明确偏好，而两者使用复杂度接近，Hertz 综合更优

### Web 框架：标准库 net/http

- 优势：零依赖，最稳定，无锁定风险
- 劣势：路由、参数绑定、分组、优雅退出均需自行实现
- 否决原因：这些属于通用能力，自行实现违背「不过度设计 / 不重复造轮子」的原则

### ORM：sqlc 或 ent

- 优势：性能更好，类型安全更强
- 劣势：sqlc 需要额外的代码生成流程；ent 的 schema 定义方式学习曲线较陡
- 否决原因：起步阶段优先选择上手成本低、迭代速度快的方案；本项目尚无极致性能诉求

### ORM：直接使用 database/sql

- 优势：最直接、无抽象泄漏、性能最优
- 劣势：需要手写大量扫描与映射代码，重复度高，易出错
- 否决原因：开发效率收益远大于性能损失，且 GORM 支持降级使用原生 SQL

---

## 影响

### 正面

- 框架能力开箱即用，骨架搭建速度快
- 双数据库支持使本地开发无需依赖外部数据库服务
- 中文社区成熟，问题易检索

### 负面 / 代价

- 引入较多传递依赖（Hertz 依赖 netpoll；GORM 依赖各数据库驱动）
- ORM 存在抽象泄漏风险，复杂查询仍需关注实际生成的 SQL
- Hertz 相对 net/http 属于较年轻的项目，长期维护性需持续观察

### 需要的配套调整

- 关闭 GORM 默认事务，减少单条写入的一次 BEGIN/COMMIT 往返（已在 `internal/database` 实现）
- 统一使用单数表名，避免 `users` 与 `user` 命名并存
- 关键选型变更必须新建 ADR，不得直接修改本文

---

## 参考

- [Hertz 官方文档](https://www.cloudwego.io/zh/docs/hertz/)
- [GORM 官方文档](https://gorm.io/zh_CN/docs/)
