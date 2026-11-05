# 真实 agent 真机验收清单

> 状态：生效（M5 验收待执行）
> 最后更新：2026-11-05
> 关联：[`../01-product/ROADMAP.md`](../01-product/ROADMAP.md) §5.2 · [`../07-specs/f-6-08-agent.md`](../07-specs/f-6-08-agent.md) · [`TESTING.md`](TESTING.md) · [`SECURITY_REVIEW.md`](SECURITY_REVIEW.md) §3 · [ADR-0013](../06-decisions/0013-remove-mock-transport.md)

本文件把「代码写了但没有证据证明在真机上成立」变成**可逐项执行、可判定、可回填**的清单。它不替代单元测试：单测证明控制面逻辑，真机验收证明节点侧真的发生了那件事。

## 0. 判定口径（每一条都适用）

- [ ] **任务终态**为成功，且任务阶段与错误信息不存在「成功但跳过」的降级路径；
- [ ] **节点侧事实**可观测：域 XML / `virsh` 输出 / 磁盘与包文件 / `iptables` 或 OVS 流表 / dnsmasq 租约 / 系统服务状态，至少一项与控制面期望态一致；
- [ ] **控制面投影**在下一轮心跳或采集后与节点事实一致（不是只写库成功）；
- [ ] 失败用例验证了**回滚或诚实失败**：不留孤儿域、半截文件、悬空规则、卡在 `running` 的任务。

> 只满足第 1 条的，按 `f-6-08-agent.md` §2.2 的「假成功」定义判为**验收不通过**。

## 1. 环境与纪律

- [ ] 一台 Linux + libvirt/KVM 宿主机（迁移与 relay 项需两台），agent 以 systemd 托管、按 `deploy/agent/` 与 `DEPLOYMENT.md` enroll；
- [ ] 控制面可连 PostgreSQL（或明确标注本轮用 SQLite 冒烟形态，不据此验收生产路径）；
- [ ] 工具链就绪：`virsh` / `qemu-img` / `genisoimage` 或 `xorriso` / `mkisofs` / `tcpdump` / OVS / dnsmasq / `virt-customize`（模板预处理项）；
- [ ] 来宾镜像内装 `qemu-guest-agent`（首次启动就绪探测、来宾自动化项）；
- [ ] 遵守 [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md)：只读诊断优先、变更最小且可回滚、变更后做业务侧验证；
- [ ] 记录中**不写真实 IP / 账号 / 口令 / 证书**（`AGENTS.md` §7 红线），证据用脱敏输出。

## 2. 通道与注册（F-6-01 / F-6-08）

- [ ] enroll 成功：一次性令牌用后即失效，mTLS 证书 CN ↔ `agent_id` 绑定；
- [ ] 协议版本：改造客户端发 `protocol_version=0/2` 时应被 `FailedPrecondition` 拒绝且不签发证书（`hub.protocolVersionSupported`）；
- [ ] 无客户端证书连接 `Channel` 被拒；心跳 10s、40s 离线阈值符合 `f-6-01` R-005；
- [ ] 断连自治：拔掉控制面后节点不中断在途操作，重连后阶段/结果补报、控制面投影对账；
- [ ] 能力自报：节点上报的能力清单与执行器注册表同源（当前 124 项），控制面「节点能力」页可见；
- [ ] **SPICE 探测**：在真机上确认 `virsh domcapabilities` 输出是否含 spice 字面量（探测为保守启发式，见 `internal/nodeagent/sys/spice.go`），并验证「节点上报 → 该节点 VM 的 `spice_supported` 同步 → 控制台列出 SPICE → `.vv` 连接文件可用」整条链路；libvirt 未编译 SPICE 时应明确拒绝而不是显示可用。

## 3. 2026-11-04 补齐的 37 项能力（单测通过、真机待验）

按 `f-6-08-agent.md` §2.1 分组，逐项按第 0 节四条判定；**重点看写路径**，只读探测项至少核对真实返回值不是常量。

| 组 | 操作 | 节点侧事实（证据） | 结果 |
|---|---|---|---|
| VM 6 项 | `vm.adopt` / `vm.xml.apply` / `vm.disk.change` / `vm.disk.resize` / `vm.disks.independent` / `vm.interface.change` | `virsh dumpxml` 的对应段落、`qemu-img info` 容量、网口所在桥 | [ ] |
| 模板 12 项 | prepare / delete / rebase / flatten / promote_child / promote_delete / preprocess / export / export.delete / export.fetch / import.preview / import | 磁盘链与 `tar.gz` 包（`manifest.json` + `disk.qcow2`，SHA256 一致）、分片 offset/length 可续传 | [ ] |
| 镜像 2 项 | `image.parse` / `image.import` | 落盘文件与格式探测结果 | [ ] |
| 网络 14 项 | VPC ACL、交换机迁移/重配、端口释放、计数重置、全局带宽、IPv6 策略、公网 IP 预览/重载/来宾状态/IPv6 检测、抓包取回/删除 | OVS 流表 / `iptables -S` / dnsmasq 配置 / 抓包分片文件 | [ ] |
| 存储 / 宿主 3 项 | `storage.partition.list` / `host.pci.bind` / `host.tuning.apply` | `lsblk`、IOMMU 绑定、sysctl 或调优项 | [ ] |

- [ ] 大文件与长耗时：模板导出/导入、`qemu-img convert` 超过 10s 不被探测超时误杀（`sys.RunTimeout`）；
- [ ] OVA 导出产物为 VMDK + OVF + SHA1 清单，`export.fetch` 的 MIME 为 `application/ovf`；
- [ ] `vm.nvram.repair`、`vm.floppy.apply`、`vm.staticip.change`、`platform.repair` 不再是「成功但没做事」。

## 4. 首次启动初始化（F-2-17，2026-11-05 新增）

- [ ] NoCloud：`meta-data` + `user-data` 打成卷标 `cidata` 的 ISO，挂为第二光驱（`sdb`），安装 ISO 仍是 `sda`；
- [ ] ConfigDrive：`meta_data.json` / `user_data`，卷标 `config-2`；
- [ ] 主机名、初始密码、静态地址在来宾内真的生效；`(MAC, IP)` dnsmasq 静态租约由节点确认后才记 `applied_at`；
- [ ] `vm.init.ready` 经 qemu-guest-agent 检查 `boot-finished`；NoCloud 就绪后自动弹出安装介质；
- [ ] 缺工具链 / 挂载失败时**连带回滚域与磁盘**，不留孤儿域；无 qemu-guest-agent 的 Linux 与 Windows 明确不自动弹出（界面需提示手工弹出）。

## 5. 迁移（需双机）

- [ ] offline：源停机 → XML/盘/NVRAM → staging → 目标接管 → 记录 → 源清理，一次通过；
- [ ] warm：`virsh save` + `restore --xml --persistent`，恢复后内存现场与定义都在；
- [ ] 失败重试：暂存重发、幂等落位、unknown 放行、状态不死锁；
- [ ] live 模式被**明确阻断**且文案可执行（ADR-0005 边界，不判失败）；
- [ ] 跨节点分片中转（relay）需**真实两台物理/虚拟宿主机**；同机双 agent 的等效回放不算通过，需记录真实双机结论。

## 6. 降级、安全与 E2E（ROADMAP §5.2）

- [ ] 按 [`../01-product/CAPABILITY_MAP.md`](../01-product/CAPABILITY_MAP.md) §5 逐项：节点缺能力时「拦截 / 只读 / 提示」与界面标记一致，不出现假成功；
- [ ] 安全自查 §3 三项：节点通道安全、libvirt / 命令注入面、节点侧日志脱敏（方法见 `SECURITY_REVIEW.md` §3）；
- [ ] agent 侧日志（libvirt / cmd 分类）与遥测上报（**M5.1 未完成**，先验收通道再验收内容）；
- [ ] Playwright 核心链路：登录 / 创建 / 电源 / 控制台 / 迁移，在**接真实 agent 的独立环境**跑通；不接 agent 的环境不写假用例；
- [ ] 性能压测：创建 / 电源 / 迁移在 10 / 50 并发下的吞吐与 P95、SSE 100 并发订阅、5000 VM 列表（`TESTING.md` 口径）；
- [ ] 外部可观测性接入需先新建 ADR，接入后验证指标来自真实节点而非常量。

## 7. 结果回填

每批验收完成后，同步更新（**不得只在本文件打勾**）：

- [ ] `f-6-08-agent.md` §2 的分期状态与「深化项」；
- [ ] `ROADMAP.md` §5.1 / §5.2 的勾选项；
- [ ] `SECURITY_REVIEW.md` §3 的结论列；
- [ ] 各规格 §7 的验收标准（见 `../07-specs/README.md` 的验收口径）与 `CHANGELOG.md`。

## 8. 明确不在本清单内（需先有决策或环境）

live 热迁移、跨节点 relay、agent 自动升级、证书续期 / 吊销与 GeoIP 依赖的候选方案与前置条件登记在 [ADR-0014（Proposed）](../06-decisions/0014-node-channel-and-agent-lifecycle.md)，**决策前不实现**。SPICE 节点探测链路已落地（真机判定待验）；`repository` 分层按域分批迁入（每域一个 commit）。

已在本清单外并行落地、无需新决策的两项：**PostgreSQL 单实例锁**（`cmd/server` 启动时取会话级 advisory lock，验收项为「同库启动第二个实例应明确失败」）与 **tag 触发的发布制品流水线**（`release.yml` 只出多平台制品，不部署；部署目标与回滚方案仍待确定）。这些项**不因本清单勾选而视为完成**。
