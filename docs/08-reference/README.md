# 参考项目（Reference Projects）

> 状态：生效
> 最后更新：2026-09-13
> 关联：[`../../AGENTS.md`](../../AGENTS.md) · [`../04-engineering/GIT_WORKFLOW.md`](../04-engineering/GIT_WORKFLOW.md) · [`qvmconsole/`](qvmconsole/) · [`kite/`](kite/)

本分区专门收录**外部参考项目**（`reference/` 下的 git 子模块）的全部资料：使用原则、只读与同步机制，以及每个项目的**功能与实现结论**和**工程结构速查**（技术栈、目录、数据模型、API、部署）。

**边界**：本分区只描述"参考项目**有什么、怎么做的**"；`k_cockpit` 自己的技术选型与方案写在 [`../02-architecture/`](../02-architecture/) 与 [`../07-specs/`](../07-specs/)，两者不混写。

> 本分区由原 `docs/04-engineering/REFERENCE_PROJECTS.md`、`REFERENCE_QVMCONSOLE.md`、`REFERENCE_KITE.md` 迁移而来（2026-09-13），旧位置仅保留跳转存根。

---

## 1. 目录结构

```
08-reference/
├── README.md            # 本文件：参考项目总览、只读与同步机制
├── qvmconsole/          # QVMConsole（KVM/QEMU 虚拟机管理平台）
│   ├── README.md        #   概览：定位、技术栈、目录结构、构建部署、配置、规模
│   ├── architecture.md  #   工程架构：分层、中间件、并发与后台任务、数据模型、API、前端
│   ├── features.md      #   功能与实现机制结论
│   └── pitfalls.md      #   踩坑与修复清单：已踩过的坑 → 根因 → 做法 → 证据
└── kite/                # kite（多集群 Kubernetes 工作空间）
    └── README.md        #   功能与实现机制结论
```

---

## 2. 使用原则（大方向）

> 这是方向性总原则，所有涉及参考项目的工作都以它为准。

1. **参考而非照搬**：参考项目只提供"**有哪些功能、怎么实现的**"这一层信息；`k_cockpit` 的技术栈、架构与代码组织**由我们自己决定**，不因为参考项目用了什么就跟着用什么。
2. **看功能与实现，不看技术栈**：我们借鉴的是**功能设计、实现机制与踩坑经验**（例如某个兼容性问题怎么解、某个状态怎么同步），**不是**它的框架、库、目录结构或代码风格。
3. **架构问题由我们自研**：参考项目自身的限制（如 QVMConsole 的单机绑定）**不是我们的设计约束**；多机、多集群等架构由 `k_cockpit` 自行设计。
4. **只读**：参考仓库**不得修改**（见 §5）。要改内容请到对应上游仓库改。
5. **结论沉淀**：每次阅读参考项目得到的结论，应更新到对应项目的结论文档（见 §4），而不是只停留在对话里。
6. **明确边界**：本分区只描述"参考项目有什么、怎么做"；`k_cockpit` 自己的方案写在 `02-architecture/` 与 `07-specs/`，两者不混写。

---

## 3. 参考仓库清单

| 目录 | 上游仓库 | 跟踪分支 | 用途 | 资料入口 | 结论基于提交 |
|---|---|---|---|---|---|
| `reference/QVMConsole/` | https://github.com/kyssion/QVMConsole | `main` | 虚拟机管理能力参考 | [`qvmconsole/README.md`](qvmconsole/README.md)（工程速查）· [`qvmconsole/features.md`](qvmconsole/features.md)（功能结论） | `52023d6` |
| `reference/kite/` | https://github.com/kyssion/kite | `main` | Kubernetes 看板能力参考 | [`kite/README.md`](kite/README.md) | `546820c` |

> **"结论基于提交"** 记录结论文档撰写（或最近复核）时参考仓库所处的提交，用于判断结论是否已过期（见 §6）。

`reference/` 下的内容**不参与** `k_cockpit` 自身的构建与测试，仅供查阅与参考；其代码与文档由各自上游仓库维护。

**只读约定**：参考仓库**只读，不得修改任何内容**（含代码、文档、配置与子模块提交指针，见 [`../../AGENTS.md`](../../AGENTS.md) 第 7 节红线）。两个子模块均在 `.gitmodules` 中设置 `ignore = all`，父仓库会忽略其内部改动与提交指针变化，避免参考内容被误提交进 `k_cockpit`。需要调整参考内容时，应到对应上游仓库修改，本仓库只负责同步。

---

## 4. 各项目的资料组织

每个参考项目一个目录，按"**概览 → 架构 → 功能结论**"三层组织，职责如下：

| 文件 | 记录内容 |
|---|---|
| `<项目>/README.md` | 定位与整体印象、技术栈与版本、顶层/二级目录结构、构建与部署方式、配置项、代码规模、测试与工程实践 |
| `<项目>/architecture.md` | 分层与模块划分、启动装配流程、中间件、并发与后台任务、数据模型、HTTP API 分组、前端结构 |
| `<项目>/features.md` | 逐功能记录"**功能 → 实现机制 → 关键文件**"，含踩坑与兼容性处理 |
| `<项目>/pitfalls.md` | 汇总**已被修复过的问题与兼容性坑**："现象 → 根因 → 做法 → 证据（提交/文档）"，用于后续实现提前避坑 |

**写作约束**：以上文档只记录参考项目的功能与实现机制，**不得**写入 `k_cockpit` 自己的技术选型或方案。文档中引用的路径均相对各参考仓库根目录。

---

## 5. 同步机制

子模块默认「钉」在父仓库记录的某次提交上，**普通 `git pull` 只会回到该提交，不会前进到上游最新**。为用一条 `git pull` 同步参考仓库的最新内容，本仓库使用 `.githooks/post-merge` 钩子，在每次 `git pull` / `git merge` 成功后执行：

```sh
git submodule update --init --remote --recursive
```

`--remote` 会拉取 `.gitmodules` 中 `branch` 指定分支（`main`）的最新提交，从而让 `git pull` 完成参考仓库的同步。

### 首次克隆后（一次性）

`core.hooksPath` 属于本地配置，无法随仓库提交，**每次新克隆后需执行一次**：

```bash
git clone --recurse-submodules <repo-url>
cd k_cockpit
git config core.hooksPath .githooks
```

若克隆时未带上子模块内容，先补一次：`git submodule update --init --recursive`。

### 手动同步（与钩子等价）

```bash
git submodule update --init --remote --recursive
```

---

## 6. 维护约定：参考项目更新后同步结论

参考项目会持续演进，**其实现可能变化，结论随之过期**。因此约定：

- **约定**：当 `reference/` 下的参考项目更新后，必须**复核并更新**对应项目的资料（§4），同时更新 §3 表格中的**"结论基于提交"**与文档头部的"最后更新"日期。
- **同步粒度**：至少复核"与本次变化文件相关的功能"；若变化范围大，则整体重新复核该项目的资料。
- **即使结论未变**，也应更新"结论基于提交"与复核日期，明确"已复核、无变化"。

### 触发方式：手动触发

本项目**不为参考项目同步设置定时任务**。同步由人明确发起，触发后按下节步骤自动执行完整复核流程。

- 触发语示例：对 AI 说「**同步参考项目结论**」或「检查 `reference/` 是否有更新并同步结论文档」。

### 执行步骤（触发后执行）

1. **同步子模块**到上游最新，并查看当前提交：

   ```bash
   git submodule update --init --remote --recursive
   git submodule status
   ```

2. **比对基线**：将上一步的提交与 §3「结论基于提交」比对。
   - 一致 → 无需处理，结束。
   - 不一致 → 继续。
3. **定位变化范围**：

   ```bash
   git -C reference/QVMConsole log --oneline <baseline>..HEAD
   git -C reference/QVMConsole diff --stat <baseline>..HEAD
   git -C reference/kite log --oneline <baseline>..HEAD
   git -C reference/kite diff --stat <baseline>..HEAD
   ```

4. **复核并更新资料**：阅读变化涉及的代码/文档，更新对应项目目录下受影响的条目；变化大则整体复核。
5. **回写基线**：更新 §3 的「结论基于提交」、资料文档头部的「结论基于提交」与「最后更新」，并在文档的「变更记录」中加一行。
6. **重复**：对每个有更新的参考仓库执行第 3–5 步。

---

## 7. 说明与注意事项

- 子模块为**只读参考**：`ignore = all` 使 `git status` / `git add .` 不会带入子模块内部的改动，误改也不会被提交（但仍应避免修改）。
- `git pull` 通过钩子把子模块推进到上游最新，父仓库不记录子模块提交指针，因此 `git status` 保持干净。
- 若不需要自动同步，跳过 `core.hooksPath` 配置即可，改用上面的等价命令。
- 两个子模块跟踪各自的 `main` 分支（见 `.gitmodules` 的 `branch`）。按当前只读约定默认始终跟随上游最新；如需锁定版本，需另作调整（移除 `branch` 与 `ignore = all` 并提交指针）。
- **"只读"是父仓库视角的只读**：它保证 `k_cockpit` 不会记录/提交子模块改动，但**不能**阻止有人进入子模块直接向上游仓库提交。要强约束需在上游仓库设置分支保护。
- 旧的 `docs/04-engineering/REFERENCE_*.md` 已迁移至本分区，旧文件保留为跳转存根，**请以本分区内容为准**。

---

## 8. 变更记录

| 日期 | 变更内容 |
|---|---|
| 2026-09-12 | 创建文档：记录参考仓库清单与子模块同步机制 |
| 2026-09-12 | 明确参考仓库为只读（`ignore = all`） |
| 2026-09-12 | 补充"使用原则（大方向）"、"结论文档"与"参考项目更新后同步结论"的维护约定 |
| 2026-09-12 | 明确同步为"手动触发"，并补充触发方式与执行步骤（SOP） |
| 2026-09-13 | 由 `docs/04-engineering/REFERENCE_PROJECTS.md` 迁移至 `docs/08-reference/README.md`，补充本分区目录结构、资料组织与 QVMConsole 工程速查入口 |
