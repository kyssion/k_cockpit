# 规格：虚拟机详情页（VmDetailPage）拆分重构

- 状态：Ready
- 负责人：<!-- TODO -->
- 创建日期：2026-11-03
- 关联需求：F-2-03（详情页）；**行为口径以 [`f-2-01-vm-list.md`](f-2-01-vm-list.md) 为准，本规格只调整代码组织**
- 关联 ADR：无（不改技术栈与架构；组件风格遵循 [ADR-0006](../06-decisions/0006-frontend-tech-stack.md) 与 [`FRONTEND.md`](../02-architecture/FRONTEND.md) §4.7）

> 本规格是**重构规格**，不新增功能、不改变任何用户可见行为。规格中的「验收标准」以「与拆分前等价」为核心。

---

## 1. 背景与目标

### 1.1 背景

`web/src/views/vm/VmDetailPage.tsx` 当前 **3987 行 / 约 146 KB**，是全仓最大的单文件：

| 事实 | 数值 |
|---|---|
| 顶层定义 | 46 个（1 个 type + 1 个常量表 + 44 个函数） |
| 承载内容 | 主页面壳 + 7 个待拆页签 + 11 个弹窗 + 9 个私有 UI + 8 个工具函数 |
| 单块超长 JSX | 3 处：主组件 return 435 行、`NetworkTab` return 395 行、`SnapshotTab` return 253 行 |
| 导出符号 | 仅 `VmDetailPage`（`router/index.tsx` 直接 import） |

三个具体问题：

1. **不可并行修改**：任意两个改动落在同一文件，合并冲突面覆盖全部 46 个定义。
2. **阅读成本**：修改一个页签需要跨越数千行上下文；AI 代理必须整文件读取才能定位。
3. **已有先例但未执行到底**：`ExportTab.tsx`、`DiskTab.tsx`、`ShareTab.tsx`、`XMLTab.tsx`、`CDROMTab.tsx`、`GuestActionsSection.tsx`、`MigrateSection.tsx`、`VMMetricsPanel` 已从本文件拆出，文件头注释原文即为「那个文件已经 2600 行……按一个页签一个文件拆开，后续再加页签也有地方放」。剩余 7 个页签未拆。

### 1.2 目标（可衡量）

| 编号 | 目标 |
|---|---|
| G-01 | `VmDetailPage.tsx` 降到 **≤ 400 行**（仅保留页面壳：路由参数、主查询、电源类 mutation、页签分发） |
| G-02 | 拆分后**任一文件 ≤ 600 行** |
| G-03 | 用户可见行为**零变更**：页签数量、DOM 结构、文案、接口调用序列、Query key 字符串全部不变 |
| G-04 | 每个批次独立可构建、可回滚（不要求一次性原子完成） |

### 1.3 不做什么（范围外）

- 不改视觉、交互、文案、加载态与错误态。
- 不改接口路径、请求参数、Query key 字符串、缓存策略与轮询间隔。
- 不引入 `context`、状态管理库、新依赖，不做组件 API 重设计。
- 不拆已经独立的 `DiskTab` / `ShareTab` / `XMLTab` / `CDROMTab` / `ExportTab` / `GuestActionsSection` / `MigrateSection` / `VMMetricsPanel`。
- **不顺手修以下已发现缺陷**（会污染「零行为变更」的验证）：`ConsoleTab` 硬编码截图 URL（2106 行）、`NicModal`/`BindIPModal`/`PortForwardModal` 的未使用 `vmID` 类型字段。两者单独立项。
- 不做 `VmDetailPage` 的功能增强或页签合并。

---

## 2. 拆分产物

命名遵循 `web/src/views/` 现有惯例：**扁平目录**（不建子目录，全仓 60 个页面文件平铺）、`XxxTab.tsx` / `XxxSection.tsx` / `XxxModal.tsx` / `XxxPage.tsx`、**单命名导出、无 default**。

| 产物文件 | 来源（当前行） | 内容 | 预估行数 |
|---|---|---|---|
| `VmDetailPage.tsx`（保留） | 1–102、133–715 | 页面壳：`useParams`、`detail`/`tasks`/`nodes` 查询、`power`/`setLock`/`rescue`/`reinstall`/`purgeBackup` mutation、页签分发、3 个内联确认弹窗 | ≈ 330 |
| `VmTabs.ts` | 103–131、1042–1066 | `TabKey`、`TABS`、`TabBar` | ≈ 50 |
| `VmHero.tsx` | 827–1066 | `VmHero`、`HeroCard`、`HeroRow`、`Rate`、`formatRate`、`formatUptime` | ≈ 240 |
| `SystemTab.tsx` | 1069–1158、3433–3445 | `SystemTab`、`Field` | ≈ 100 |
| `CredentialSection.tsx` | 3899–3987 | 登录凭据区块 | ≈ 90 |
| `PCIeInfoBlock.tsx` | 3497–3526 | PCIe 槽位区块 | ≈ 30 |
| `ResizeDiskModal.tsx` | 3698–3807 | 磁盘扩容（由 `SystemTab` 打开） | ≈ 110 |
| `NetworkTab.tsx` | 1161–1655 | 网卡 / 静态 IP / 端口转发三卡 | ≈ 500 |
| `NetworkModals.tsx` | 1655–2046 | `NicModal`、`BindIPModal`、`PortForwardModal`、`FillMyIP`、`AppliedBadge` | ≈ 390 |
| `NetworkSections.tsx` | 3537–3680 | `NeighborSection`、`GuestIPSection`、`NEIGHBOR_STATE_LABEL` | ≈ 130 |
| `SnapshotTab.tsx` | 2480–2950 | `SnapshotTab`、两个 `xxxDisabledReason`、`CreateSnapshotModal` | ≈ 450 |
| `ScheduleTab.tsx` | 2116–2477 | `ScheduleTab`、`LastResultCell`、`describeSchedule`、`CreateScheduleModal` | ≈ 350 |
| `EditTab.tsx` | 2953–3252 | `EditTab`、`FieldControl`、`buildDraft`、`computeChanges`、`toStr` | ≈ 300 |
| `EventTab.tsx` | 3453–3485 | `EventTab` | ≈ 45 |
| `ConsoleTab.tsx` | 2049–2113 | `ConsoleTab` | ≈ 65 |
| `ReinstallModal.tsx` | 724–817 | 重装系统 | ≈ 95 |
| `DeleteVmModal.tsx` | 3285–3431 | 删除（含磁盘处置与强删） | ≈ 150 |
| `MakeIndependentModal.tsx` | 3816–3890 | 解除模板依赖 | ≈ 75 |
| `utils/describe.ts` | 3682–3685 | `describe`（**30 处调用 / 12 个组件**，必须最先下沉） | ≈ 8 |
| `components/common/ErrorBox.tsx` | 3258–3264 | 错误提示块（4 个页签共用） | ≈ 12 |
| `utils/format.ts`（追加） | 3442–3445 | `formatMemory`（与既有 `formatBytes` 同族） | +6 |
| `views/vm/vmKeys.ts` | 新增 | 跨文件共享的 query key 常量（见 D-004） | ≈ 20 |

> 预估行数为估算，验收以 G-01 / G-02 的**实际行数**为准。

---

## 3. 拆分规则（必须遵守）

| 编号 | 规则 | 理由 |
|---|---|---|
| R-001 | **Query key 字符串一字不改**：`['vm', vmID]`、`['vms']`、`['tasks']`、`['vm-edit-form', vmID]`、`['vm-interfaces', vmID]`、`['vm-static-ips', vmID]`、`['vm-snapshots', vmID]`、`['vm-schedules', vmID]`、`['guest-ips', vmID]`、`['vm', vmID, 'neighbors']` 等 | 全部跨组件协作都经由 Query 缓存完成（无 props 传值、无 context）。**key 不变即行为不变** |
| R-002 | 每个页签组件**自持** `useQueryClient` 与本地 `refresh()`，只失效自身 key + `['tasks']` | 与 `DiskTab.tsx` / `ShareTab.tsx` / `ExportTab.tsx` 既有写法一致 |
| R-003 | 页签 props 保持现状：自取数据用 `{ vmID: number }`；需读 vm 字段用 `{ vm: VmView }`；需要固件用 `{ vmID, firmware }` | 与 `CDROMTab`（`{ vmID, nodeID }`）、`ExportTab`（`{ vm }`）一致 |
| R-004 | `VmHero` **必须是独立组件**，不得内联回壳 | 主组件在 `isPending` / `isError` 处提前 return（268–275 行），其后若有 hook 属条件调用。此约束已在 819–826 行注释中记载 |
| R-005 | `NicModal` / `BindIPModal` / `PortForwardModal` 由父组件用 `key` 强制重挂载的行为**保留在调用方** | 1669–1671 行注释明示这是刻意选择（拒绝用 ref 初始化） |
| R-006 | 先完成 §4 批次 0（共享件下沉），再动任何页签 | `describe` 被 12 个组件使用，不下沉会导致每拆一个文件复制一份实现 |
| R-007 | 每拆一个文件**单独提交**，提交信息写清「纯移动，无逻辑改动」 | 便于 review 与逐批回滚 |
| R-008 | `router/index.tsx` 的 import 路径与导出名 `VmDetailPage` **不得变化** | 路由入口稳定性 |
| R-009 | 搬迁时保持 `NEIGHBOR_STATE_LABEL` 与其使用者的**同文件、且声明在前** | 当前它声明在 `NeighborSection` 之后（3696 行 vs 3537 行），靠渲染期求值才没触发 TDZ |

---

## 4. 分批实施

每一批独立提交，批内每步都可单独回滚。

### 批次 0：共享件下沉（前置，无结构变化）

| 步骤 | 动作 | 验证 |
|---|---|---|
| 0.1 | `describe` → `web/src/utils/describe.ts`，替换 30 处调用 | `tsc -b` |
| 0.2 | `ErrorBox` → `web/src/components/common/ErrorBox.tsx`，替换 4 处 | `tsc -b` |
| 0.3 | `formatMemory` → `web/src/utils/format.ts`（与 `formatBytes` 同文件） | 新增单测（空值 / 0 / 常规值） |
| 0.4 | **推迟**：`vmKeys.ts` 在批次 2.2 首次出现跨文件 key 时按需建立（只收纳被两个及以上文件使用的 key）。拆分前所有 key 都在同一文件内，此刻建文件属空抽象 | 建立时 `tsc -b` + AC-04 grep 对账 |

批次 0 结束时**不改变任何组件边界**，`VmDetailPage.tsx` 仅减少被搬走的定义。

**进度（2026-11-03）**：0.1 `describe` → `utils/describe.ts`、0.2 `ErrorBox` → `components/common/ErrorBox.tsx`、0.3 `formatMemory` → `utils/format.ts` **已完成**；`VmDetailPage.tsx` 3987 → 3971 行，新增 6 个单测（前端单测 17 → 23）。0.4 按上述说明推迟。

### 批次 1：零耦合页签（只有单一消费方）

按体量从小到大：`EventTab` → `ConsoleTab` → `SystemTab`（含 `Field`、`CredentialSection`、`PCIeInfoBlock`、`ResizeDiskModal`）→ `ScheduleTab`（含 `CreateScheduleModal`、`LastResultCell`、`describeSchedule`）→ `SnapshotTab`（含 `CreateSnapshotModal` 与两个 reason 函数）。

每步验证：`tsc -b` + `pnpm lint` + 对应页签手工冒烟。

**进度（2026-11-03）**：5 个页签全部完成，一页签一提交（1.1 `EventTab` / 1.2 `ConsoleTab` / 1.3 `SystemTab` 组 / 1.4 `ScheduleTab` 组 / 1.5 `SnapshotTab` 组）。`VmDetailPage.tsx` 3971 → **2651 行**；新文件 46 / 72 / 117+45+130+103 / 384 / 494 行，均 ≤ 600；每步 queryKey 集合（全目录 47 项）diff 为空。

### 批次 2：中耦合页签

| 步骤 | 动作 | 需保留的耦合 |
|---|---|---|
| 2.1 | `EditTab`（含 `FieldControl`、`buildDraft`、`computeChanges`、`toStr`） | `onSaved={refresh}` 由壳传入，语义不变 |
| 2.2 | `NetworkTab`（三卡） | 壳的 `refresh()` 仍须失效 `['vm-interfaces']`、`['vm-static-ips']`（编辑保存后网卡要重取） |
| 2.3 | `NetworkModals` / `NetworkSections` | `key` 重挂载留在 `NetworkTab` 调用侧 |

### 批次 3：壳瘦身

| 步骤 | 动作 |
|---|---|
| 3.1 | `VmHero` + `HeroCard`/`HeroRow`/`Rate`/`formatRate`/`formatUptime` → `VmHero.tsx` |
| 3.2 | `TabKey` / `TABS` / `TabBar` → `VmTabs.ts` |
| 3.3 | 三个内联确认弹窗（电源确认 601–625、救援 648–684、锁定 685–712）→ 独立组件 |
| 3.4 | 壳最终 ≤ 400 行 |

---

## 5. 边界与风险

| 编号 | 风险 | 处理 |
|---|---|---|
| K-01 | `['vm-edit-form', vmID]` 被 `VmHero`（60s `staleTime`）与 `EditTab`（默认）**共用但配置不同**；`refresh()` 也不失效该 key | 拆分后**两侧配置原样保留**（D-001）。这是现状，不在本次修复 |
| K-02 | 壳的 `refresh()` 失效 `NetworkTab` 私有的两个 key | key 下沉到 `vmKeys.ts`，两边引用同一常量（D-002） |
| K-03 | `EditTab` 保存后 `['vm', vmID]` 失效会重渲 `SnapshotTab`（`firmware` prop 变化决定「修复 UEFI 启动项」按钮是否出现） | 壳继续持有 `detail` 查询并把 `vm` 下发给 `SnapshotTab`（props 不变） |
| K-04 | 提前 return 与 hook 顺序 | `VmHero` 保持独立组件（R-004） |
| K-05 | 拆错导致某个页签首次挂载即发请求（原先惰性挂载） | 页签分发仍是条件渲染（`tab === 'x' && <XTab/>`），不得改成 CSS 隐藏 |
| K-06 | 拆分过程中 key 改名导致缓存互不命中（表现为「保存后页面不刷新」，无报错） | R-001 + AC-04 的 grep 对账 |

---

## 6. 验收标准

- [ ] **AC-01**：`wc -l web/src/views/vm/VmDetailPage.tsx` ≤ 400。
- [ ] **AC-02**：`web/src/views/vm/` 下所有拆分产物单文件 ≤ 600 行。
- [ ] **AC-03**：`router/index.tsx` 未改动；`VmDetailPage` 导出名与文件路径不变。
- [ ] **AC-04**：拆分前后对 `web/src/views/vm/` 全目录 grep `queryKey` 得到的**字面量集合完全一致**（顺序不计，集合相等）。
- [ ] **AC-05**：`pnpm exec tsc -b`、`pnpm lint`、`pnpm test`、`pnpm build` 全部通过；测试数与拆分前一致或更多。
- [ ] **AC-06**：`pnpm test:e2e`（5 条冒烟）通过。
- [ ] **AC-07**：人工回归 13 个页签（system / network / console / snapshot / monitor / disks / cdrom / share / event / schedule / export / xml / edit）与全部弹窗，DOM 与文案与拆分前一致。
- [ ] **AC-08**：任一批次回滚后其余批次仍可独立编译。

---

## 7. 测试要点

| 类型 | 用例 |
|---|---|
| 单元（新增） | `utils/describe`：`Error` 实例、字符串、`{ message }` 对象、`null`/`undefined`、非错误对象（不得抛错，不得显示 `[object Object]`） |
| 单元（新增） | `utils/format.formatMemory`：0、小于 1 GB、整 GB、非整数 GB、`undefined` |
| 回归 | 现有 17 个前端单测保持通过；不因拆分调整断言 |
| E2E | 5 条冒烟通过；创建向导与登录链路不受影响 |
| 手工 | 每个页签首次进入（惰性挂载）、页签切换、12 个弹窗开关与提交后的刷新 |

---

## 8. 待决问题

| 编号 | 问题 | 影响 | 状态 |
|---|---|---|---|
| Q-001 | 是否需要为拆分后的每个页签补组件测试（当前全仓仅 3 个前端单测文件） | 影响 AC-05 的「测试数」口径；补测试会显著扩大本次工作量 | 待确认（默认：**不补**，仅补 §7 的两个 util 单测） |

> 状态为 `Ready` 的前提是 Q-001 采用默认口径（不补页面级测试）。若要求补页面测试，规格降为 `Draft` 并重估批次。

---

## 9. 关键决策记录

| 编号 | 决策 | 理由 |
|---|---|---|
| D-001 | `['vm-edit-form']` 的 60s / 默认两种 `staleTime` **原样保留**，不统一 | 统一属于行为变更，需单独评估；重构批次只搬运不改语义 |
| D-002 | 建立 `web/src/views/vm/vmKeys.ts`，只收纳**跨文件**使用的 key | 解决「壳与页签各自写字符串」的漂移；单文件私有 key 继续内联，避免过早抽象（AGENTS.md：不过度封装） |
| D-003 | 不建 `views/vm/` 子目录 | 全仓 `views/` 为扁平结构（60 个文件），新增子目录会让同类文件两套位置 |
| D-004 | `describe` 放 `utils/` 而非 `views/vm/` | 它是纯错误格式化，与虚拟机域无关，其他域后续可直接复用 |
| D-005 | 本次不修已发现的两个缺陷（硬编码截图 URL、未使用 `vmID` 字段） | 保持「零行为变更」的可验证性；单独提交便于回滚 |
| D-006 | 以 600 行作为单文件上限 | 当前同类文件体量（`VmDetailPage` 3987 / `CreateVmWizard` 约 1300 / `TemplatePage` 约 1300）说明 600 是显著改善且不碎片化；阈值更低会切出大量 50 行文件 |

---

## 10. 变更记录

| 日期 | 版本 | 变更内容 | 变更人 |
|---|---|---|---|
| 2026-11-03 | v0.1 | 初稿：基于对 `VmDetailPage.tsx` 的逐定义测绘（46 个顶层定义、13 个页签、5 条 Query 耦合、3 处超长 JSX）确定产物清单、9 条规则、4 个批次与 8 条验收标准 | AI |
| 2026-11-03 | v0.2 | 批次 0.1–0.3 落地（`describe` / `ErrorBox` / `formatMemory` 下沉 + 6 个单测）；0.4 修正为**推迟**（拆分前不存在跨文件 key，提前建 `vmKeys.ts` 属空抽象） | AI |
| 2026-11-03 | v0.3 | 批次 1 完成（5 个页签、5 个提交）：详情页 3971 → 2651 行，最大新文件 494 行；AC-04 每步对账通过 | AI |
