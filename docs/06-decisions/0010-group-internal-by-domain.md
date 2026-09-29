# 0010. internal 按业务域分组（platform / compute / network / storage / ops）

- 状态：Accepted
- 日期：2026-09-28
- 决策人：kyssion
- 关联：[`../../AGENTS.md`](../../AGENTS.md) §4 · [`../02-architecture/ARCHITECTURE.md`](../02-architecture/ARCHITECTURE.md) §3 · [0007-mock-agent-first.md](0007-mock-agent-first.md)

---

## 背景

重排前 `internal/` 下有 **58 个包全部平铺**，且规模极不均匀（`vm` 44 个文件、`handler` 66 个、`agent` 49 个），存在多组易混淆的近似命名（`schedule` / `scheduler`、`network` / `networkbridge`、`audit` / `auditlog`）。真实 agent（M5）落地前，`agent/` 与节点侧还会新增大量代码——落进没有归属信号的平铺目录只会加剧。

四条设计约束：

1. **不引入技术分层**：项目既有设计是"域包端到端拥有逻辑"（service + executor + 审计同包，model 薄共享），本次整理不推翻它；
2. **嵌套不超过两级**：`internal/<域>/<包>` 即止，层级是给人找路的，不是越深越好；
3. **迁移必须是纯机械操作**：`git mv` + import 路径更新，零逻辑改动，每域一个 commit、全量测试护航；
4. **model 保持全局共享**：按域拆 model 必然出现跨域模型引用，import 环的风险大于收益。

## 决策

**1. 六个业务域 + 三个顶层包**

```
internal/
├── platform/   # 身份与权限 + 全部基础设施（api/auth/authz/risk/audit/
│               # auditlog/apikey/authkey/invite/useradmin/settings/reqlog/
│               # accesscontrol/passaudit/config/cryptoutil/database/
│               # logging/mailer/version）
├── compute/    # vm/vmtag/template/importer/passthrough/computequota
├── network/    # vswitch/bridge/firewall/hostfirewall/securitygroup/vpcacl/
│               # publicip/portsecurity/portmirror/capture
├── storage/    # pool/userstorage/quota
├── ops/        # task/realtime/cron/scheduler/alert/monitor/dashboard/
│               # search/diagnostics/platformcheck/hosttuning/maintenance/
│               # emergency/quotaenforce
├── node/       # 节点接入与投影（将来 agent 服务端通道也在此）
├── agent/      # 领域操作契约 + mock
├── model/      # 跨域共享表模型（不按域拆，理由见约束 4）
├── handler/    # 按同名六域分子包（platform/compute/network/storage/ops/node）
└── router/     # 路由注册与角色声明
```

**2. 顺带消歧三组命名**

| 原 | 新 | 理由 |
|---|---|---|
| `internal/network` | `network/vswitch`（包名 `vswitch`） | 它管的就是 VPC 交换机；且 Go 的 `switch` 是关键字不能做包名 |
| `internal/networkbridge` | `network/bridge`（包名 `bridge`） | 与域目录同名消歧 |
| `internal/schedule` | `ops/cron`（包名 `cron`） | 与 `ops/scheduler`（周期组件注册表）这对最易混的名字分开 |
| `internal/storage` | `storage/pool`（包名 `pool`） | 它管的是存储池；消掉与域目录同名的歧义 |

**3. handler 按域拆同名子包**，共享助手（路径/分页参数解析、下载头）上移 `platform/api` 导出——解析助手不属于任何一个域。`handler/node` 包与 `internal/node` 同名，router 里以别名 `nodehandler` 导入。

## 两个被否决的子项（执行中验证）

**auditlog 并入 audit —— 依赖环否决。** 尝试合并时出现 `audit(查询) → authz → auth → audit(写入)` 的环：auth 的初始化流程要写审计，而审计查询要按角色过滤。结论：**审计写侧必须比 auth 更底层**，读写保持两个包（`platform/audit` 写、`platform/auditlog` 读），这不是历史包袱而是依赖方向的体现。

**端口转发 / 静态地址从 vm 抽到 network 域 —— 深耦合否决。** 这些方法长在 `vm.Service` 上，依赖其内部管线（`enqueueNetChange`、带宽配额检查、执行器共享参数、`load` 的归属校验）。强行抽出需要先设计跨包接口，收益不抵风险。**与 vm 拆包（快照 / 迁移 / 控制台 / 网络写路径各自成包）同批处理**，时机是真实 agent 落地需要动这些文件之时——此前强行拆只是搬运耦合。

## 后果

- import 路径全量变更（约 200 个文件），`git blame` 首次归属需 `--follow` 追溯；
- 迁移目录随 `database` 包移动，`cmd/migrate` 的默认 `-dir` 同步为 `internal/platform/database/migrations`；
- 路由对账测试（`route_coverage_test.go`）适配域包前缀与子目录 glob；
- AGENTS.md §4 目录树与 ARCHITECTURE.md §3 模块表同步重写；
- 新增包一律进对应域目录；新域的设立需先更新本 ADR。
