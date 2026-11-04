# 0013. 移除 mock 传输与演示数据

- 状态：Accepted
- 日期：2026-11-04
- 决策人：kyssion
- 关联：[`0007-mock-agent-first.md`](0007-mock-agent-first.md)（被本 ADR 取代）· [`0011-mock-power-state.md`](0011-mock-power-state.md)（被本 ADR 取代）· [`0005-control-plane-node-agent-architecture.md`](0005-control-plane-node-agent-architecture.md)（架构不变）

---

## 背景

ADR-0007 在 agent 尚未实现时引入 mock 传输，让控制面的业务逻辑、任务队列、实时通道与前端可以先落地。这个决定在当时是正确的：它把「控制面」与「宿主机」两条线解耦，使控制面可以在没有 Linux 环境的情况下被完整开发与测试。

2026-11 盘点时发现两个事实：

1. **agent 能力已基本补齐**：控制面实际下发 120 个 OpKind，agent 注册表最初只有 86 项；差额 37 项在 mock 下走 `Execute` 的默认分支，返回 `Success=true` 与空数据——控制面看起来「功能正常」，真实节点上却返回「未实现」。
2. **mock 已经进入生产可选路径**：`AGENT_TRANSPORT` 默认 `mock`，`Config.Validate` 不禁止生产环境使用；同时还有模拟节点预置、演示数据、`/api/v1/dev/agent-register` 与前端「模拟」标记。这些逻辑让「未实现」被静默掩盖，也让部署者可能在一套假数据上完成验收。

## 决策

1. **删除 mock 传输**：`agent.MockClient`、`mock_frame.go`、`AGENT_TRANSPORT` 配置与 `internal/devdata/`、`node.EnsureSimulated`、模拟注册路由全部移除；控制面只保留 gRPC 反向长连接（`agent/hub`）。
2. **先补能力，再删 mock**：37 个缺失的 agent 执行器按 VM → 模板 → 镜像 → 网络 → 存储/宿主分批接通真实 libvirt / OVS / iptables / ip / tc 操作；补齐完成后再删除生产 mock。
3. **测试改用每包自写 stub**：测试不再共享一个全局 mock 实现；每个测试包在 `stub_test.go` 中实现该包真正依赖的最小返回值，避免「mock 行为」成为隐式契约。
4. **E2E 不再要求 mock**：Playwright 冒烟栈只起独立控制面与 vite，不接 agent；节点相关链路依赖真机验收。

## 理由

- **假成功比失败更危险**：mock 对未知 OpKind 返回成功，会让「未实现」在演示与验收中不可见；真实节点返回明确失败，反而能推动补齐。
- **生产路径必须唯一**：保留一条「接口直接返回假数据」的分支，等于给部署者一个静默降级的选项；删除它，配置错误会在启动/首次操作时立即暴露。
- **测试替身应当局部化**：全局 mock 会随着业务增长变成一个 2000 行的隐式规范；每个包只声明自己需要的返回值，测试与实现的耦合面更小。

## 备选方案

### 方案 A：保留 mock 作为开发模式，仅禁止生产启用
- 优势：开发期无需 Linux 节点。
- 劣势：仍需维护两套实现；37 个未实现操作会继续被掩盖；E2E 继续测假成功。
- 否决原因：与「先补能力」的目标冲突。

### 方案 B：保留全局测试替身（agenttest 包）
- 优势：151 处调用点机械替换，工作量小。
- 劣势：替身会再次演化为隐式契约；新功能容易只改替身、不改真实执行器。
- 否决原因：测试应显式声明依赖，而不是共享一份「万能成功」。

## 影响

### 正面

- 生产代码与依赖图中不再出现 mock；`AGENT_TRANSPORT` 配置错误不再可能。
- 37 项能力接真后，控制面下发与 agent 注册表一一对应（123 项，缺口 0）。
- 测试失败会指向具体依赖，而不是「mock 返回了这个值」。

### 负面 / 代价

- **开发与 E2E 需要真实 agent 才能看到节点数据**；冒烟只覆盖登录与导航。
- 41 个测试文件、151 处调用点需要改为包内 stub；后续新增依赖要同步维护 stub。
- 新增的宿主侧执行器只做了单测与命令边界验证，**真机验收仍需在 Linux 宿主机上补**（尤其模板导入导出、VPC ACL、PCI 绑定、热迁移）。

### 需要的配套调整

- 文档：`README.md`、`AGENTS.md`、`DEPLOYMENT.md`、`TESTING.md`、`API.md`、`f-6-08-agent.md` 已同步移除 mock 口径。
- 部署：`DEPLOYMENT.md` 增加 agent 安装 / enroll / systemd 章节。
- 后续：模板 `tar.gz` 包格式、派生链维护路径参数、`public_ip.reload` 绑定列表等契约缺口已在 `f-6-08-agent.md` §2.1 记录。

## 参考

- [`0007-mock-agent-first.md`](0007-mock-agent-first.md)
- [`0011-mock-power-state.md`](0011-mock-power-state.md)
- [`../07-specs/f-6-08-agent.md`](../07-specs/f-6-08-agent.md) §2.1
