# 参考项目

> 状态：生效
> 最后更新：2026-09-12
> 关联：[`GIT_WORKFLOW.md`](GIT_WORKFLOW.md) · [`../../AGENTS.md`](../../AGENTS.md)

本仓库通过 **git 子模块（submodule）** 关联两个外部参考仓库，作为实现 `k_cockpit` 各项能力与功能时的参考。

---

## 1. 参考仓库清单

| 目录 | 上游仓库 | 跟踪分支 | 用途 |
|---|---|---|---|
| `reference/QVMConsole/` | https://github.com/kyssion/QVMConsole | `main` | 能力 / 功能实现参考 |
| `reference/kite/` | https://github.com/kyssion/kite | `main` | 能力 / 功能实现参考 |

> `reference/` 下的内容**不参与** `k_cockpit` 自身的构建与测试，仅供查阅与参考；其代码与文档由各自上游仓库维护。

**只读约定**：参考仓库**只读，不得修改**。两个子模块均在 `.gitmodules` 中设置 `ignore = all`，父仓库会忽略其内部改动与提交指针变化，避免参考内容被误提交进 `k_cockpit`。需要调整参考内容时，应到对应上游仓库修改，本仓库只负责同步。

---

## 2. 同步机制

子模块默认「钉」在父仓库记录的某次提交上，**普通 `git pull` 只会回到该提交，不会前进到上游最新**。为用一条 `git pull` 同步两个仓库的最新内容，本仓库使用 `.githooks/post-merge` 钩子，在每次 `git pull` / `git merge` 成功后执行：

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

## 3. 说明与注意事项

- 子模块为**只读参考**：`ignore = all` 使 `git status` / `git add .` 不会带入子模块内部的改动，误改也不会被提交（但仍应避免修改）。
- `git pull` 通过钩子把子模块推进到上游最新，父仓库不记录子模块提交指针，因此 `git status` 保持干净。
- 若不需要自动同步，跳过 `core.hooksPath` 配置即可，改用上面的等价命令。
- 两个子模块跟踪各自的 `main` 分支（见 `.gitmodules` 的 `branch`）。按当前只读约定默认始终跟随上游最新；如需锁定版本，需另作调整（移除 `branch` 与 `ignore = all` 并提交指针）。

---

## 4. 变更记录

| 日期 | 变更内容 |
|---|---|
| 2026-09-12 | 创建文档：记录参考仓库清单与子模块同步机制 |
| 2026-09-12 | 明确参考仓库为只读（`ignore = all`） |
