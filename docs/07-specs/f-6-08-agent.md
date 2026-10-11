# F-6-08 节点 agent（真实通道与领域执行）

- 状态：生效中（P0~P2 已交付 + P3~P5 执行器全量接入；剩余为深化项）
- 关联：[f-6-01](f-6-01-node.md)（接入与通道协议）· [ADR-0005](../06-decisions/0005-control-plane-node-agent-architecture.md) · [ADR-0007](../06-decisions/0007-mock-agent-first.md) · [`internal/agentproto/agent.proto`](../../internal/agentproto/agent.proto)（线上契约唯一来源）

## 1. 已交付（P0，通道骨架）

**架构**：`cmd/agent`（enroll/run 两子命令）+ `internal/nodeagent`（agent 侧：TLS 身份、重连退避、执行器注册表）+ `internal/agent/hub`（控制面侧：连接注册表、PKI 自举、Execute 路由、心跳落库）。传输 gRPC over TLS（mTLS），协议形状见 proto；`AGENT_TRANSPORT=grpc` 启用，mock 保留（E2E 冒烟仍跑 mock）。

**安全模型**：控制面启动时自举私有 CA（`agent-pki/`）；agent 一次性令牌经 `Register` 换客户端证书（CN=agent_id）；此后连接出示证书、并校验服务端链是否由该 CA 签发。无证书仅能调 Register；Channel 按 CN 解析节点。令牌校验/审计复用 node 域 `Register` 语义（经 Registrar 接口延迟解析）。

**通道语义**（与 mock 同构，业务零改动）：
- 心跳 10s（首帧即发）；离线由控制面按 40s 阈值推导，agent 不上报状态字符串；
- Execute：下行 `ExecuteRequest`，上行 `StageUpdate`（透传到任务时间线）与 `ExecuteResult`；`error=未送达` 与 `Success=false=执行失败` 严格二分；
- 重连：1s×1.6 上限 60s ±20% 抖动；稳定会话（≥30s）后失败计数归零；
- 能力自报 = 执行器注册表（同源）；未注册操作返回明确的「未实现」Result。

**P0 真机验收记录**（Ubuntu 24.04 arm64，libvirt 10 + OVS 3.3.9）：
enroll → 面板 online（版本/协议号正确）；`node.stats` 经真实通道返回「未实现」；杀进程 45s 内 offline；重启/`systemctl restart` 后自动恢复 online；心跳时间戳以 10s 周期推进。systemd 单元见 [`deploy/agent/`](../../deploy/agent/k_cockpit-agent.service)。

## 2. 分期路线（P0~P2 真机验收通过；P3~P5 执行器已全量注册并部署，深化项见各条注记）

| 期 | 内容 | 状态 | 深化项（后续迭代） |
|---|---|---|---|
| P1 只读探测 | 23 个探测操作全部真数据 | ✅ 真机对账通过 | host.hardware 内存条（需 SMBIOS）|
| P2 生命周期+控制台 | create/clone/delete、电源、快照、光驱、截帧、VNC 流 | ✅ 真机验收通过（aarch64 UEFI+ramfb 建→开→截帧→删） | 槽位记账精化；克隆 E2E |
| P3 网络 | vpc.switch（OVS+dnsmasq+NAT）、桥/上行口（看门狗）、安全组（per-VM 链）、双层防火墙、端口转发 DNAT、公网 IP DNAT、端口安全（OVS 流表）、镜像、抓包（tcpdump）、收敛修复 | ✅ 执行器真机部署（85 项能力自报） | OVS 原生流表口径深化；交换机迁移 |
| P4 存储与模板 | 池格式化挂载/删除/配置/卸载/trim、分区、LVM 卷、文件提交与读取、9p 共享 | ✅ 执行器部署 | 模板制备/导入导出 tar 通道 |
| P5 长尾 | 冷迁移、重装（备份→重建→失败还原）、救援（配置快照）、导出、口令审计（读 shadow+crypt 比对）、SSH 开关（DenyUsers drop-in+踢会话）、配额处置（FORWARD 链）、平台修复 | ✅ 执行器部署（真机口令审计 36 账号验证） | 热迁移/脏页采样、来宾自动化（需 guest-agent）、takeover 完整重建 |

## 3. 实现约束（继承既有决策）

- agent 是**唯一直接操作 libvirt/KVM 与宿主系统**的角色（ADR-0005）；libvirt 客户端用纯 Go 实现（无 CGO，交叉编译 linux/arm64），`virsh` 命令降级。
- 危险操作自带**节点侧**保险（uplink/端口镜像看门狗）；`firewall.rollback` 不允许失败；`host_firewall` 先写入后收紧。
- 幂等收敛类操作下发期望状态而非增量；`vm.xml.apply` 校验用 libvirt 校验器；源侧迁移数据在目标确认前不删。
- 阶段 Key 沿用 mock 的 stagePlan 词汇（前端时间线已按此渲染）；`Total=0` 表示总数未知。
- 真实 IP/密码/证书不入仓库（红线）；测试机操作遵循 TROUBLESHOOTING 只读优先与最小变更原则。
