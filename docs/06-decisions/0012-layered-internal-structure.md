# 0012. internal 按层组织：router / handler / service / repository / model

- 状态：Accepted
- 日期：2026-10-06
- 关联：[`0010-group-internal-by-domain.md`](0010-group-internal-by-domain.md)（本 ADR **取代**其顶级目录的组织方式，域分组保留为其二级结构）· [`0007-mock-agent-first.md`](0007-mock-agent-first.md)（agent 契约边界不变）
- 决策人：kyssion

---

## 背景

ADR-0010 把 `internal/` 按业务域分组（platform / compute / network / storage / ops / node + agent / model / handler / router），解决了当时 58 个包平铺与近似命名混淆的问题。运行一个月后暴露出它的一处代价：**分层不可见**。

- 服务层与基础设施混居：`internal/platform` 同时装着 config / logging 这类基础设施与 auth / settings / invite 这类业务服务，`internal/{compute,network,…}` 五个域目录本身是服务层却没有任何命名体现。
- 只实现了前端与 server（agent 二进制是 M5 的事），读者打开 `internal/` 看到十个平级目录，无法一眼判断「改一个接口要动哪几层、哪一层的东西」。
- 数据访问与业务逻辑同包同文件：service 直接持有 `*gorm.DB`，「数据库层」事实上不存在。

## 决策

### 1. 顶级目录 = 层，二级目录 = 域

```
internal/
├── router/          # 路由层：路由注册与角色声明（routes_<域>.go 与 handler 域子包同名对应）
├── handler/         # 接口层：HTTP 参数解析与响应（按域分子包，维持 ADR-0010 的划分）
├── service/         # 服务层：业务逻辑（compute / network / storage / ops / node / platform）
├── repository/      # 数据库访问层：SQL/GORM 查询与写入的唯一居所（与 service 域同名对应）
├── model/           # 数据库层：表模型（跨域共享，维持 ADR-0010 不拆的决策）
├── platform/        # 基础设施：api / audit(写侧) / authz / config / cryptoutil / database / logging / version
├── agent/           # agent 契约 + mock（未来 agent 二进制的边界，不变）
└── devdata/         # 开发期演示数据预置
```

与 ADR-0010 的关系：

- **取代**的是顶级组织方式（「不引入技术分层」那条约束就此作废）；
- **保留**的是域划分本身——域沉为 `service/` 与 `repository/` 的二级目录，handler 域子包与 `routes_*.go` 的对应关系不变；ADR-0010 的其余约束（嵌套不超过两级、迁移必须纯机械、model 跨域共享不拆、audit 写侧比 auth 更底层）全部继续有效。

### 2. 设立 repository 层，各域分批迁入

repository 层**新建即立规范、不一次性重写**：约 6 万行服务代码、上千处 GORM 调用点，一次迁移无法被现有测试护航。路线：

- **范式样本**：`repository/node`（最小域）先行，确立形状——Repo 为具体类型（不预抽接口），方法按业务动词命名、承载服务需要的查询与写入；返回原始数据与错误（`gorm.ErrRecordNotFound` 原样上抛），不翻译 `api.Error`；不含业务判断（唯一索引冲突原样返回、软删除记录照查）。
- **分批迁移**：其余域按范式逐个迁入，每域一个 commit、全量测试护航，compute（94 文件）放最后。未迁入的域维持现状（service 直接查库），但**新代码一律走 repo**。
- **目标态**：service 不执行任何 GORM 查询；引用 gorm 哨兵错误做翻译（错误分类）允许。规范全文见 CODING_STANDARDS 第 13 节。

### 3. 平台域的拆分边界

`internal/platform` 保留的 8 个包按「有没有 HTTP 之外的语义」划分：

- **留下的**：api（响应/中间件助手）、audit（写侧记录器，ADR-0010 论证过必须比 auth 更底层）、authz（Viewer 判定 + 角色中间件）、config、cryptoutil、database、logging、version。
- **迁到 service/platform 的 12 个**：auth、settings、invite、useradmin、apikey、risk、auditlog、reqlog、passaudit、authkey、accesscontrol、mailer——它们是有领域语义、被 handler 调用的业务服务。audit 的**读侧**（auditlog，查询服务）随之迁走，读写两侧继续分包防依赖环。

## 后果

- **正面**：层一眼可辨（目录名即层名）；服务层完整收敛在 `service/` 下；数据访问有了唯一居所与可对账的边界；路由层按域可读（routes_*.go），且顺带修复了一个存量回归——`NewAPIDocs` 在构造时快照 `h.Routes()`，此前构造于 v1 路由注册之前，`/api-endpoints` 一直只列出 `/health`。
- **代价**：迁移期间 repository 只覆盖 node 域，其余域「service 直接查库」的存量与规范并存——用「新代码一律走 repo」的硬规则约束分叉不再扩大；import 路径普遍变长一级（`internal/service/...`）。
- **迁移成本**：机械目录迁移一次完成（git mv + 全仓 import 重写，约 320 个文件，零逻辑改动）；repository 分批进行，无截止日期压力。

## 与被取代决策的对照

| ADR-0010 的约束 | 本 ADR 之后 |
|---|---|
| 不引入技术分层 | **作废**：层为顶级、域为二级 |
| 嵌套不超过两级 | 继续有效（`internal/service/<域>/<包>`） |
| 迁移必须纯机械、每域一 commit | 继续有效（本次同样遵守） |
| model 跨域共享不拆 | 继续有效 |
| audit 写侧比 auth 更底层 | 继续有效（audit 留 platform，auth 迁 service） |
