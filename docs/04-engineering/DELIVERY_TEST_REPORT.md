# 交付测试执行报告（第 14 轮）

> 执行日期：2026-11-10
> 依据：[`DELIVERY_TEST_PLAN.md`](DELIVERY_TEST_PLAN.md)（TP-00 ~ TP-23、五道阶段门、§7.3 出口准则）
> 被测对象：当前工作区源码编译的控制面 `bin/server-r14` + 同仓库交叉编译的节点 agent
> 环境：单台 Linux/aarch64 KVM 节点 + agent 反向长连接；控制面为 macOS 开发形态、SQLite 库
> 脱敏：本报告不写真实 IP、账号、口令、证书内容；证据只给路径与结论

---

## 1. 执行摘要

本轮按计划实际跑通了工程门禁、真机 26 套件回归（多轮）、新增自动化用例、
前端 E2E 冒烟与四份机械对账，并在测试过程中直接修复发现的问题后重测。

| 门 | 结论 | 说明 |
|---|---|---|
| **G0 环境就绪** | **通过（经复位）** | 初始不满足：agent 断连、节点 libvirt 域/模板盘/存储池/OVS 桥全空、SSH 编排不可一键复跑。逐项复位后节点在线、能力 131 项、5 台基准域（2 运行 / 3 关机） |
| **G1 回归基线** | **通过** | TP-00 工程门禁全绿；26 套件首轮 6 个 rc=0，20 个失败**全部定界为环境前提**，无一条产品回归 |
| **G2 缺口补齐** | **部分通过** | 修复 1 个产品缺陷、2 类文档缺陷、1 个红线问题；新增 4 组自动化用例与 3 份入库测试资产。A 档缺口中 G-14/G-15/G-08/G-17 已关闭或显著收敛 |
| **G3 专项与发布** | **未通过** | PostgreSQL 生产形态、双机迁移、性能压测、发布制品与回滚演练**未执行**（环境/前置不具备），见 §7 |
| **G4 最终全量回归** | **通过** | 最终确认轮 26/26 套件 rc=0 / FAIL=0，另加新增用例 500+ 条断言 0 失败 |

**出口判定：当前不能签「可交付使用」**。§7.3 的 8 条准则中，第 4/5/6/7 条仍未满足：
PostgreSQL 生产形态全量回归、上线检查清单、发布演练记录、文档状态回填均缺证据。
这不是"测试没做完"，而是**本环境不具备这些能力的前置条件**（详见 §7）。
计划本身把正式发布列为 M5 的独立工作，本轮结论应回填为「工程与真机功能面已验证，发布面待补」。

---

## 2. 环境复位（G0，逐项可复跑）

初始状态经只读诊断确认：**两套环境并存**——节点上有一套 09:31 部署的独立控制面 + agent，
macOS 上的 r13 控制面 :18080 的 agent 在 17:30 断连（`context canceled`），
节点侧 libvirt 域定义、模板盘、存储池挂载、OVS 桥、基准 VM 全部不存在，
控制面却还保留 5 台 VM / 47 个模板的投影。

| # | 复位动作 | 验证 |
|---|---|---|
| 1 | 部署本机公钥到节点，让 26 套件里 `ssh -o BatchMode=yes` 免交互 | 一键复跑成功；此前 `run_final.sh` 卡在密码提示（G-13 的实证） |
| 2 | 用控制面 CA 给 node 1 的 `agent_id` 补签客户端证书，在节点用**独立 state-dir** 起第二个 agent（不动现有那套环境） | `[agent-hub] 节点 1 通道建立`；`/nodes` status=online、能力 131 项、`/nodes/1/stats` 返回真实数据 |
| 3 | 用当前工作区交叉编译 `linux/arm64` agent 并部署 | 通道与能力自报正常 |
| 4 | **用当前工作区重新编译控制面**并重启（此前 :18080 跑的是 11:13 的旧二进制，测的不是当前代码） | `bin/server-r14`；health 200、agent 自动重连 |
| 5 | 重建模板盘（2 块无 backing 的可引导 qcow2）、挂载 2 个存储池、建 `br-ovs` 及 2 个历史交换机桥 | 模板检查全过；存储池由 `unmounted` **自动回升 `ready`**（14.7 / 5.8 GiB，r13 P1 修复回归通过） |
| 6 | 按投影 UUID 重建 5 个 libvirt 域，2 台开机（OVS 接口需 `<virtualport type='openvswitch'/>`） | `virsh list --all` 5 个域；采集器把投影校正为 2 running / 3 stopped、`present=1`、概览 `missing=0` |
| 7 | 启动假 SMTP 汇聚端、重建 `/dev/nbd3` 8G 测试盘、清除历史告警 | 邮件批次、存储批次、对账批次由红转绿 |

> 第 2 项是用 CA 直签证书恢复通道的**测试环境夹具**，绕过了一次性注册令牌流程；
> **不作为 `TC-04-1`（令牌用后即失效）的验收证据**，报告里单独标注。

**一条通过的回归证据**：在 agent 在线但节点 0 个域时，控制面把 5 台 VM 全部校正为
`present=0 / status=unknown`、概览 `missing=5`；域恢复后自动回到 `present=1 / missing=0`。
说明 r13 修的投影对账与软删除污染逻辑在当前代码上仍成立，没有出现「假成功」。

---

## 3. 逐套件结果

### 3.1 工程门禁（TP-00，R1）

| 项 | 结果 |
|---|---|
| `gofmt -l cmd internal` | 无输出 |
| `go vet ./...` | rc=0 |
| `go test ./...` | rc=0（含本轮新增 2 个测试文件） |
| `pnpm exec tsc -b` | rc=0 |
| `pnpm lint`（oxlint，187 文件 / 116 规则） | 0 warning / 0 error |
| `pnpm test`（Vitest） | 8 文件 / 34 用例全过 |
| `pnpm build` | rc=0（首次在受限执行环境下的 `prepareOutDir` 权限失败，非产品缺陷） |
| Playwright E2E 冒烟 | **5/5 通过**（首次因系统 HTTP 代理让 Playwright 误判 5179 被占用，清除代理变量后通过） |

### 3.2 真机 26 套件（R3，`~/.kc_test/r13/run_final.sh`）

| 轮次 | 条件 | rc=0 套件数 |
|---|---|---|
| 第 1 轮 | 初始环境（agent 离线、节点数据全空、SSH 编排卡住→修复后） | 6 / 26 |
| 第 2 轮 | agent 恢复、假 SMTP/模板盘/存储池复位（**旧二进制**） | 约 9 / 26 |
| 第 3 轮 | **当前代码编译的控制面** + 完整环境复位 | 19 / 26 |
| 第 4 轮（定向） | 重建 5 个基准域并开机后，重跑 7 个失败套件 | 4 个套件转绿 |
| 第 5 轮（G4 最终） | 再补 2 个 OVS 桥、清除历史告警后的全量回归 | 25 / 26（`r13_fault` 夹具冲突） |
| 第 6 轮（最终确认） | 修正故障注入夹具、修 flaky 后的全量回归 | **26 / 26 全绿**，见 §3.3 |

第 3 轮起，`s_network` 50/50、`s_platform` 65/65、`s_storage` 14/14、`t_read` 28/28、
`t_clone` 54/54、`r12_ops_b`、`r12_mail_b`、`r12_storage_b`、`r12_fuzz`、
`r13_rbac`、`r13_edge`、`r13_audit` 等均全绿。

**首轮 20 个失败套件的定界结论**：全部指向环境前提
（`节点不可达`、`virsh 0 个域`、`/var/lib/k_cockpit/templates 不存在`、
模板盘无 backing、假 SMTP 未启动、`/dev/nbd3` 容量 0），**没有一条产品回归**。
典型证据：`s_compute_core` 的「模板克隆创建」失败信息是
「该节点没有可用存储池，请先创建存储池」；`r13_reconcile` 的 `R2` 是
「节点域: []」；`r12_mail_b` 是「dial tcp 127.0.0.1:2525: connection refused」。

### 3.3 G4 最终回归结果（21:07:45 → 21:15:05）

**25 / 26 套件 rc=0 / FAIL=0**，是本项目有记录以来最接近全绿的一次真机回归：

| 类别 | 套件 | 结果 |
|---|---|---|
| 既有主套件 14 | `s_compute_core` 58/58、`s_compute_ext` 77/77、`s_compute_more` 28/28、`s_storage` 14/14、`s_network` 50/50、`s_platform` 65/65、`s_console_ws` 12/12、`t_read` 28/28、`t_clone` 54/54、`t_final` 29/29、`t_honesty` 16/16、`t_mirror_e2e`、`t_zombie_disk`、`t_concurrency` | 全部 rc=0 / FAIL=0（有明细的 11 个合计 **431 条断言全过**） |
| r12 七批次 | `r12_control_a`、`r12_network_b`、`r12_ops_b`、`r12_netres_b`、`r12_mail_b`、`r12_storage_b`、`r12_fuzz` | 全部 rc=0 |
| r13 五批次 | `r13_reconcile` 49/49、`r13_rbac`、`r13_edge`、`r13_audit`、`r13_fault` | 前四个 rc=0；`r13_fault` rc=1（见下） |

**`r13_fault` 的唯一失败是测试夹具与用例假设冲突，不是产品缺陷**：
本轮为恢复通道额外起了 `kc-agent-r14`，而该套件只停 `k_cockpit-agent.service`，
于是「节点应收敛为离线」的断言拿不到离线状态。修正夹具（同时停/起两个 agent，
并把 r14 从 transient unit 改成正式 systemd unit 以便可靠恢复）后单跑
**19/19 全过**（`F15 agent 已拉起`、`F16 节点恢复在线`、`F17 能力自报恢复`、
`F18 网络探测恢复`、`F19 无悬挂任务` 均通过）。

**最终确认回归（21:25:28 → 21:31:57）：26 / 26 套件全部 `rc=0` / `FAIL=0`。**

在 G4 之后又跑了两轮，把最后两处不稳定性按 `TESTING.md` §9「修 flaky 而不是放宽判定」处理：

| 轮次 | 结果 | 处理 |
|---|---|---|
| 确认回归（21:17） | 25/26：`r13_fault` 已转绿；`s_compute_more` 偶发 1 条（创建任务成功后立即按名字查列表查不到） | 单跑 `s_compute_more` 28/28 全过 → 判定为**异步可见性竞态**；在 R3 脚本里加 6 次 ×1s 可见性等待，**断言「必须登记」不放宽**，留 `.bak-r14` 备份 |
| **最终回归（21:25）** | **26/26 全绿** | 既有主套件 14 个（有明细的 11 个合计 431 条断言）、r12 七批次、r13 五批次（含 `r13_fault` 19/19、`r13_reconcile` 49/49）全部 rc=0 |

本轮新增用例另计：守卫矩阵 4 例、search 6 例、日志端点 19 例、E2E 冒烟 5 例、
Vitest 34 例、`go test ./...` 全包、机械对账 rc=0，**合计新增/复跑断言 500+ 条，0 失败**。

> R3 夹具的 3 处修正（同时停/起两个 agent、r14 改正式 systemd unit、可见性等待）
> 都在 `~/.kc_test/` 外部资产里，未入库，均留 `.bak-r14` 备份；
> 不改产品代码、不放宽断言口径。

> 该夹具修正位于 `~/.kc_test/r13/t_r13_fault.py`（R3 外部资产，未入库，已留
> `.bak-r14` 备份），不改变产品代码，也未放宽任何断言口径。

---

## 4. 发现的问题与修复（可直接复验）

### 4.1 产品缺陷（1 个，已修复 + 补单测 + 重测）

**D-1（P2）跨资源检索的 LIKE 通配符未转义** — `internal/service/ops/search/service.go`

- 现象：用户输入直接拼进 `LIKE '%q%'`，搜 `%` 会命中全部资源；`auditlog` 已有
  `escapeLike` + `ESCAPE '\'`，此模块漏了。
- 根因：SQL 的 `%` / `_` 未转义，且 SQLite 没有默认 LIKE 转义符，只转义不写
  `ESCAPE` 会在测试库上静默失效。
- 修复：按 `auditlog` 同口径转义 `%` / `_` / `\`，三条查询全部加 `ESCAPE '\'`。
- 复验：新增 `internal/service/ops/search/service_test.go` 6 例全过，含
  「搜 `%-` 只按字面命中 `100%-cpu`」「租户看不到他人 VM」「模板可见性」「每类上限 5」。

### 4.2 文档缺陷（2 类，已修订）

**D-2（G-08）`API.md` 公开接口注记与登记不完整**

- 注记写「公开接口共 13 个」，代码 `publicAPIRoutes` 是 **16 条**；
- 差额是邀请注册 `GET /invites/preview`、`POST /invites/accept` 两条公开接口完全漏登；
- 进一步机械对账发现另有 **18 条已注册端点完全没有契约登记**（请求日志、存储分区、
  VPC ACL、邀请管理、口令审计五组）。
- 修复：注记改为 16 条并写明逐条公开理由；补登 API-393 ~ API-412；新增
  `TestPublicAPIRoutesMatchRouter` 守一致性。复核：公开登记行数 16，
  `adminAPIRoutes` 160 与代码零差异。

**D-3（计划自身口径偏差 5 条）** — 已回填到 `DELIVERY_TEST_PLAN.md` §10.1：
G-18 的 trim 回归其实**后端已有** `TestUpdateTrimsWhitespace`；
G-15 应精确为「单元层已有完整单测，缺 HTTP 端点级」；
零测试子包实测 5 个（非 6 个）；等。

### 4.3 安全红线（1 个，已修复）

**D-4（G-17）** `web/e2e-live/live.spec.ts` 把测试管理员口令写死在源码。
改为 `E2E_ADMIN_USER` / `E2E_ADMIN_PASSWORD` 环境变量注入，缺失时明确报错退出；
`tsc -b` / oxlint / Vitest 复验通过。

### 4.4 测试基础设施问题（3 个，已修复）

| 问题 | 修复 |
|---|---|
| `run_final.sh` 与 26 套件里的 `ssh -o BatchMode=yes` 卡在密码提示（G-13 实证） | 部署本机公钥到节点，编排一键可复跑；未改动 26 个脚本 |
| :18080 跑的是 11:13 旧二进制，测的不是当前代码 | 用当前工作区重编译 `bin/server-r14` 并等价重启 |
| R2 走查/E2E 冒烟受系统 HTTP 代理干扰，误判端口占用 | 运行时清除 `*_PROXY` 并设置 `NO_PROXY` |

### 4.5 定界为「非产品缺陷」的现象

- `s_platform` 的「令牌可无 Cookie 下载」422：下载用例所选文件在节点上不存在（数据前提）。
- `r12_network_b` 的 M7「修复后管理网桥仍在」：`br-ovs` 当时尚未重建。
- `t_final` 的日志窗口 503：均为节点离线/控制台无 VM 期间的历史环境性 503。
- `r13_fault` 的 F5/F8/F9：该套件停的是 `k_cockpit-agent` 单元，而本轮为恢复通道额外起了
  `kc-agent-r14`，节点始终在线 —— **测试夹具与用例假设冲突**，不是产品的离线收敛失效。
- `s_compute_more` 的抓包下载 422：测试脚本把 `virsh domiflist` 的占位符 `-` 当接口名传入，
  节点侧 tcpdump 未生成文件，控制面如实返回「抓包文件不存在」（**诚实失败，非假成功**）。
  建议后续在节点侧补一条「接口名为 `-` / 空时明确拒绝」的边界校验，列为 P3 加固项。

---

## 5. 新增测试资产（随代码交付）

| 资产 | 内容 | 结果 |
|---|---|---|
| `internal/handler/platform/guard_matrix_test.go` | 340 条域路由逐条归入 public/admin/user 三档；方法分布；元数据与源码双向对账；adminOnly 必须同时挂 requireAuth | 4 用例全过 |
| `internal/service/ops/search/service_test.go` | 见 §4.1 | 6 用例全过 |
| `tests/delivery/check_log_endpoints.py` | 6 个日志端点的真实 HTTP 功能验证（统计/过滤/脱敏导出/级别审计/截断清理/SSE 首帧），补 G-15 的 HTTP 级证据 | **19/19 通过** |
| `tests/delivery/reconcile.py` | §6.8 四份机械对账（路由/能力/矩阵/证据），输出 orphan 清单 | rc=0 |
| `tests/delivery/README.md` | 前置、用法、口径说明 | — |

归档脚本已去掉本机绝对路径，用 `KC_BASE` / `KC_COOKIE` / `KC_ROOT` / `KC_RECONCILE_OUT`
环境变量注入，在干净检出上可独立复跑（本次已验证）。

---

## 6. 覆盖矩阵与机械对账（§6.8）

| 视角 | 结果 |
|---|---|
| 功能 | PRD 94 个 `F-x-xx` ↔ 计划 §6.1 矩阵 **94/94**，缺漏 `[]` |
| 端点 | 340 条域路由 + `/health` + `/api-endpoints` = **342**；`GET /api-endpoints` 清单 341 条（自身不在快照内） |
| 守卫 | public 15（域内）+ `/health` = 16；admin 160；user 165；`publicAPIRoutes` / `adminAPIRoutes` 与代码**零差异** |
| 能力 | `agent.go` 常量 / nodeagent 注册表 / 节点实际上报 / 控制面投影 **均 131 项，零差异** |
| 前端 | 46 条路由 path；`live.spec` 受检路径 38 条；E2E 冒烟 5/5 |
| 证据 | R1 Go 测试文件 229（本轮 +2）、Vitest 8 文件 / 34 例、R2 走查 1、R3 编排 26 套件 |

**orphan 判定**：机械对账初报 58 条「有路由无文档」，排除 `API.md` 的方括号写法、
合并行与缩写路径等格式差异后，实际为 **19 条**；其中 `GET /ovs/ports` 属合并行误报，
最终 18 条已补登（API-395 ~ 412）。逐条判定结论：**文档滞后，非实现缺漏**。
补登后归档脚本 `tests/delivery/reconcile.py` 的粗口径仍会报 40 条（从 58 降下来），
这些是参数命名差异（`:cdromID` vs `:id`）、方括号后缀与合并行缩写造成的假阳性，
不是新的实现缺漏；每次重跑都应做这一步逐条判定，不能把粗口径数字当缺陷数。

---

## 7. 残留风险与豁免（仍未满足的出口准则）

### 7.1 阻断交付（A 档，本环境不具备前置条件）

| # | 项 | 缺失前置 |
|---|---|---|
| A-1 | **PostgreSQL 生产形态全量回归**（PG 专用迁移、单实例锁、主链路重跑） | 无 PG 实例；本轮全程 SQLite 开发形态 |
| A-2 | **发布制品与回滚演练**（`v*` tag → 多平台 tarball + SHA256SUMS、`pg_dump` 备份、回滚） | 发布目标与回滚方案仍待确定（ADR-0014）；不应在无决策时擅自打 tag |
| A-3 | **双节点迁移专项**（offline / warm / relay / 目标接管） | 只有 1 台 KVM 节点；同机双 agent 等效回放按计划不算通过 |
| A-4 | **性能压测**（10/50 并发任务吞吐 P95、SSE 100 并发、5000 VM 列表） | `TESTING.md` 明确要求真实 agent 落地后才立项；本轮未造规模数据 |
| A-5 | **上线检查清单 11 项 + 部署文档定稿 + 版本号定稿** | 属 M5.3 正式发布工作，需 owner 决策 |
| A-6 | `live` 热迁移、跨节点 relay | ADR-0005 / ADR-0014 明确诚实阻断，属 B 档边界，不计入缺陷 |

### 7.2 已关闭 / 显著收敛的 A 档

| 缺口 | 本轮结论 |
|---|---|
| G-08 文档注记与漏登 | **已关闭**（16 条注记 + 20 条补登 + 守护测试） |
| G-13 测试资产未随代码交付 | **部分关闭**：自包含资产已入库并可干净复跑；26 套件编排与历史报告仍在 `~/.kc_test/`，未入库 |
| G-14 Go 侧路由级用例 0 | **已关闭**（守卫矩阵进 CI；`search` 子包补 6 例）；`authkey/mailer/passaudit/reqlog` 仍零测试，列为后续 |
| G-15 日志端点无功能证据 | **已关闭**（HTTP 级 19/19；单元层原本就有完整单测） |
| G-16 前端产物同步 | **未单独验证**（`STATIC_DIR` 托管与 E2E 冒烟间接覆盖），仍建议在发布演练中确认 |
| G-17 硬编码口令 | **已关闭** |
| G-18 trim 回归 | **校正为不存在缺口**（后端已有单测） |
| G-19/G-20 菜单/页内 UX 落差 | 保留 B 档（后端已拦截，非安全缺口）；建议补 `roles` / 按角色隐藏按钮 |
| L-09 节点重启后 DNAT 不持久、`platform.repair` 未覆盖 `port_forward` | **未修，按计划升为 A 档**：本轮确认有「节点无孤儿 DNAT」对账护栏，但控制面记录仍可能显示 `applied=true` 而没有提示。建议在交付前把 `port_forward` 纳入 repair 类别，或启用节点规则持久化；否则按 §7.3 属阻断项 |

### 7.3 P3 加固与文档待办

- 节点侧抓包执行器：接口名为 `-` / 空时应明确拒绝（当前为诚实失败，但错误信息较晚）。
- `authkey` / `mailer` / `passaudit` / `reqlog` 四个 service 子包补最小单测。
- 37 项「单测通过、真机待验」逐项对照表：本轮真机套件已覆盖大部分写路径，
  但**未产出逐项对照表**，仍需在发布前补齐。
- `AGENT_ACCEPTANCE.md` / `ROADMAP.md` §5.2/5.3 / `SECURITY_REVIEW.md` §3 的勾选项
  仍为未勾选（39 处）。按计划要求「只按真实证据勾选、不得批量打勾」，本轮只在
  本报告中给出证据，未擅自回填。

---

## 8. 出口判定与建议

**结论：本轮测试流程在可执行范围内已完成，但不能签「可交付使用」。**

理由：§7.3 的 8 条出口准则里，G3 专项与发布门未过，PG 生产形态、双机迁移、
性能压测、发布演练四类证据缺失。这些不是"漏测"，而是计划自己列出的、
需要额外环境或决策才能立项的工作。把它们写成「已通过」会重犯本计划一直避免的
「假成功」。

建议的下一步顺序：

1. **TP-22**：准备 PostgreSQL 实例，跑 PG 专用迁移 + 单实例锁 + 主链路全量回归；
2. **TP-21**：准备第二台 KVM 节点，跑 offline / warm 与真实 relay；
3. **TP-20**：按 `TESTING.md` 口径立项压测，出容量结论；
4. **TP-22-6a / G-16**：确认 `web/dist → public/` 进入发布制品，做一次发布与回滚演练；
5. **TP-23**：按本轮证据回填 `AGENT_ACCEPTANCE.md` / `ROADMAP.md` / 各规格 §7，
   补齐 37 项逐项对照表，再逐条勾验 §7.3 出口准则。

---

## 9. 证据索引

| 证据 | 路径 |
|---|---|
| 各轮 26 套件汇总 | `/tmp/kc_exec/run_final_r14*.log`、`~/.kc_test/r13/final/summary.txt` |
| 定向重跑 | `/tmp/kc_exec/recheck/`、`/tmp/kc_exec/recheck/summary2.txt` |
| 日志端点 / 机械对账（入库版） | `tests/delivery/`；本次输出 `/tmp/kc_exec/t_r14_log.out`、`/tmp/kc_exec/reconcile_r14.out` |
| 工程门禁 | `/tmp/kc_exec/tp00_go.log`、`tp00_web.log`、`gate_final.log`、`web_final.log` |
| E2E 冒烟 | `/tmp/kc_exec/e2e_smoke3.log` |
| 控制面日志 | `data/lab2/server-r14.log`（不入库） |
| 节点事实 | 节点侧 `virsh list --all`、`ovs-vsctl list-br`、`findmnt`、`qemu-img info`（脱敏输出见各套件日志） |

---

## 10. 第 15 轮（r21）：U-12 全量收口

> 执行日期：2026-11-10
> 被测对象：`v0.1.0-r21`（Linux/arm64 交叉编译，`/opt/kcockpit/releases/v0.1.0-r21`，systemd `k-cockpit`）
> 环境：**生产形态**——真实 KVM 节点（aarch64 / QEMU 8.2.2 / OVS 3.3.9）+ nginx TLS 终结 + PostgreSQL 16 + 真实 agent（mTLS `:8443`，禁用 `SECURITY_DEV_BYPASS_CODE`）
> 范围：仅 U-12（45 条 SKIP 端点 + 四个零测试子包），不重复第 14 轮的 26 套件

### 10.1 结果总览

| 组 | 脚本 | 断言 | 结果 |
|---|---|---|---|
| 认证与多阶段登录 | `~/.kc_test/r21/t_auth_matrix.py` | 60 | **60/60** |
| 流式 / 导出 / 控制台 | `~/.kc_test/r21/t_skip_streams.py` | 32 | **32/32** |
| 全局写入类端点 | `~/.kc_test/r21/t_skip_global.py` | 35 | **35/35** |
| 合计 | | **127** | **127/127 通过** |

`go test ./internal/...` 60 个包全部 `ok`（exit 0），`gofmt -l cmd internal` 无输出。

### 10.2 本轮修复的缺陷（6 条，全部为真实缺陷）

**1）`agent/hub` 连接注销被吞掉（P1 回归，上一轮引入）**
`TestGatewayEndToEnd` 报「节点下线（连接注销）超时」。`git worktree` 对照 HEAD 证明是工作区引入：上一轮为「新连接顶替旧连接」加的读协程里，`recvCh <- err` 与 `<-stream.Context().Done()` 在 `select` 中竞争，客户端断开时约有一半概率选中 Done 分支，**断开事件被丢弃**，`detach` 永不执行——节点在控制面上永久「在线」，指令被投进没人读的 outbox 直到超时。真实表现是：一次带同一证书的探测连接就能把在线节点顶成不可达。
修法：改用 `close(recvDone)` 传递结束信号（`close` 不会被 `select` 丢弃），错误存 `var recvErr error`（close 建立 happens-before，无需加锁），并抽出 `dispatchInbound` / `drainInbound` 保证已到达消息不丢。验证：全包通过、`-count=5`、`-race` 均绿。

**2）口令审计在开关关闭时记「检查失败」（每次启动一条红事件）**
`passaudit` 的注释写着「开关关闭不算故障」，但 `tick()` 把 `RunNow` 的 422 校验错误当成真失败，于是每次启动都在调度事件页留一条「口令检查失败：定时口令检查未开启」。
修法：新增 `Service.Enabled()`，`tick()` 开头早退；`TestLoopStaysSilentWhenDisabled` 作为回归锁。

**3）软删除的用户名永久占名（实机 U-12 抓出）**
认证矩阵第二遍运行时 `POST /users` 返回 409「用户名已被占用」，而 `/users` 列表里查不到该用户。根因：`uniq_user_username` 是**无条件**唯一索引，而用户是软删除（要保留审计里的 `operator_id` 引用）。与上一轮修的 `uniq_node_name` 完全同型。
修法：迁移 `0058_user_unique_partial.sql` 改为 `WHERE deleted_at IS NULL` 的部分唯一索引，模型 tag 加条件、`testschema.go` 登记、实机 `DROP/CREATE INDEX` 重建（`\d user` 已确认带 `WHERE`）。`TestRecreateUserAfterDelete` 锁定「删掉的名字可以重新注册」。

**4）密码下限两套标准（安全降级路径）**
`POST /auth/login/password` 按下限 12 位拒绝，而 `PUT /auth/password` 只要求 8 位且仅挡「与用户名完全相同」。于是可以先用 12 位设密码，再在账号设置里降回 8 位——**下限被从最弱的那条路径绕过**。
修法：新建 `internal/platform/policy`，`MinPasswordLength = 12` 单点定义，五个入口（登录改密、账号设置、管理员建号、应急口令、邀请注册）全部引用；账号设置同时改为「不得**包含**用户名」并按 `[]rune` 计长。

**5）停机时打开控制台返回 503（本轮实机抓出）**
VM 关机后 `GET /vms/:id/console/ws` 返回 `503 无法建立控制台通道`。503 的语义是「服务端暂时不可用，稍后重试」，而重试一台关着的机器永远不会成功，界面还会把它显示成一次服务故障。
修法：`OpenConsole` 在**流打开失败之后**再参考状态投影——停机则返回 `409 虚拟机未运行，无法打开控制台`，其余仍为 503。顺序是刻意的：先让 agent 试，失败后才解释；反过来先按投影拒绝，会让一个恰好陈旧的投影挡住本可成功的连接（`vm.Status` 的权威在虚拟化层）。新增 3 条单测锁住这个边界，其中 `TestOpenConsoleDoesNotTrustStaleStatus` 专门验证「状态写 stopped 但流能开 → 必须成功」。

**6）邀请建号硬编码 12 位（第 4 条的同源漏网处）**
`useradmin.CreateFromInvite` 里写的是 `len(password) < 12` 与一段手写文案，而同一个文件的 `Create` 已改用 `policy.MinPasswordLength`。改下限时会漏改这一处，且按**字节**计长对中文密码偏严。已统一。

### 10.3 覆盖率变化（U-12 后半）

| 包 | 之前 | 之后 | 用例要点 |
|---|---|---|---|
| `service/platform/authkey` | 0% | **85.5%** | 轮换后「在用密钥恰好 1 把」、旧记录保留但停用、密钥 64 位 hex、换 `SESSION_SECRET` 后必须报错而不是用空密钥签名、循环在关闭/到期/失败三态下的事件文案 |
| `service/platform/mailer` | 0% | **87.5%** | 进程内假 SMTP：头注入（`\r\nBcc:`）必须拒绝、RFC2047 主题与发件人解码、AUTH 失败、RCPT 被拒、**STARTTLS 不被支持时必须失败**（不得静默降级明文） |
| `service/platform/passaudit` | 0% | **90.4%** | 先清后标、跳过不存在的用户名、只通知命中者本人、审计 `hits` 与 `NodeID`、开关关闭静默（回归用例） |
| `service/platform/reqlog` | 0% | **90.0%** | 开关即时生效（无需重启）、`limit` 夹到 100、`Clear(0)` 清空 vs `Clear(30)` 只删旧的、库不可用时 List 报错而 Record 不报错（旁路） |

### 10.4 45 条 SKIP 端点：测试自身假设错误（**不是**产品缺陷）

这些先被当成缺陷、定界后改的是测试，产品一行未动。记录在此以免下一轮重复怀疑：

| 现象 | 真因 |
|---|---|
| SSE 端点一律 401，而同一 Cookie 的 `/tasks` 是 200 | **会话指纹 R-007 把 IP + User-Agent 一起绑进会话**。测试客户端自造 UA 即被判指纹不符。这不是 Cookie 问题，也不是缺陷——浏览器同源请求天然一致，非浏览器场景有 `action_token` 通道 |
| SSE 用例用 `kc2.get('/tasks/stream')` 会永久挂住 | 拿到 200 之后它就是一条不会结束的流，服务端每 20 秒心跳，socket 超时被不断重置。改为一读到一个完整事件就返回 |
| `POST /vms/:id/exports` 返回体里没有 `id` | 导出是异步的：受理返回 `{task_id, status}`，导出记录在任务成功后落库。模板制备、删除导出同理 |
| 控制台 WS 停机时返回「控制台尚未开启」 | 控制台出厂是关闭的，必须先 `PATCH /vms/:id/console {"enabled":true}`。否则「停机拒绝」会因为错误的原因通过 |
| 管理员新建的账号立刻登录 401 | 管理员建号默认 `status=pending` + `force_password_change`，必须先 `PUT /users/:id/status` 激活；邀请注册不走这条路（密码本人设，即刻可登录） |
| 改密后断言「当前会话仍有效」失败 | R-010：安全信息变更让该用户**全部**会话（含当前）立即失效 |

### 10.5 残留

- `authkey` 轮换在**多实例**部署下的密钥分发（单实例无此路径，无法在本环境制造）；
- 公开 15 条端点的字段级契约仍由功能套件覆盖，U-12 只断言「未登录可达 / 未登录不可达」；
- U-05 / U-07 / U-13 / U-14 / U-26 等仍按计划未执行，状态不变（见 `DELIVERY_TEST_PLAN.md` §11）。

### 10.6 证据索引（r21）

| 证据 | 路径 |
|---|---|
| 认证矩阵结果 | `~/.kc_test/r21/auth-matrix.json`（60 条） |
| 流式/导出/控制台结果 | `~/.kc_test/r21/stream-matrix.json`（32 条） |
| 全局写入结果 | `~/.kc_test/r21/global-matrix.json`（35 条） |
| 脚本 | `~/.kc_test/r21/{t_auth_matrix,t_skip_streams,t_skip_global}.py` |
| 单测与覆盖率 | `go test ./internal/...`（60 包 ok）；四个子包 `-cover` 见 §10.3 |
| 部署制品 | `/opt/kcockpit/releases/v0.1.0-r21`（`current` 已指向） |
| 服务端日志 | `journalctl -u k-cockpit`（控制台升级/释放有 `[console]` 行） |
