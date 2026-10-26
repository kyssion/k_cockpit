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

**2026-10-22 修复验收（真机，同一环境）**：四条链路经控制面 API 真实走通——
①端口镜像 enable→confirm→disable→重建 enable：OVS 恰一行、不产生重复行、disable 不误杀同桥兄弟镜像
（过程中抓出并修复 ovs-vsctl 事务语义：create 无 --may-exist、同事务引用需 --id、引用列值需命名 UUID、
摘挂即删行见 [TROUBLESHOOTING §7](../04-engineering/TROUBLESHOOTING.md)）；
②ISO 挂载 → AppArmor 抽象层**按目录**新增放行（旧 images 条目不受影响）→ 开机 running 且日志零拒绝
（顺带抓出 agent 单元 `ProtectSystem=full` 静默吞 /etc 写入，单元已加最小 ReadWritePaths 豁免）；
③VM 创建携带两块数据盘：vda/vdb/vdc 全部真实落盘、属主归一 libvirt-qemu:kvm；
④SSH 开关禁用→drop-in 落盘且 `sshd -T` 立即生效；解禁→文件移除且 reload 后立即放开（此前解禁不 reload 的缺陷修复验证）。

**2026-10-26 二轮深检验收（真机，同一环境）**：静态深挖 + 六条扩展链路——
快照 create→restore（UEFI 外部快照 revert 真机证伪预判：**可用且语义正确**，API 的 kind 字段口径遗留）；
镜像看门狗 60s 窗口内重启 agent→到期自动撤销→控制面新 reconcile 循环一分钟对账（/run 持久化补臂）；
防火墙 apply/rollback 用 `iptables -S` 实态对读，抓出**词表/键位错位族**（accept→DROP、deny 不收紧、
hostfirewall 丢规则、DNAT 缺链名）并全部修复复验；VPC 交换机全生命周期（dnsmasq 前台不写 pid→agent 自落、
杀前核验 comm、delete 补 spec.cidr 拆 MASQUERADE）四项清零验证；配置变更 vcpu/内存扩容（setmaxmem 先行）、
导出落盘。安全组 FORWARD 空转修复（无 IP 兜底不挂）+ 挂载语法真机验证。词表错位族全表见
[TROUBLESHOOTING §7.4](../04-engineering/TROUBLESHOOTING.md)。

## 2. 分期路线（P0~P2 真机验收通过；P3~P5 执行器已全量注册并部署，深化项见各条注记）

| 期 | 内容 | 状态 | 深化项（后续迭代） |
|---|---|---|---|
| P1 只读探测 | 23 个探测操作全部真数据 | ✅ 真机对账通过 | host.hardware 内存条（需 SMBIOS）|
| P2 生命周期+控制台 | create/clone/delete、电源、快照、光驱、截帧、VNC 流 | ✅ 真机验收通过（aarch64 UEFI+ramfb 建→开→截帧→删；10-22 复验：多数据盘创建/删除、光驱挂载） | 槽位记账精化；克隆 E2E |
| P3 网络 | vpc.switch（OVS+dnsmasq+NAT）、桥/上行口（看门狗）、安全组（per-VM 链）、双层防火墙、端口转发 DNAT、公网 IP DNAT、端口安全（OVS 流表）、镜像、抓包（tcpdump）、收敛修复 | ✅ 执行器真机部署（85 项能力自报；10-22 镜像全链路真机验收通过） | OVS 原生流表口径深化；交换机迁移 |
| P4 存储与模板 | 池格式化挂载/删除/配置/卸载/trim、分区、LVM 卷、文件提交与读取、9p 共享 | ✅ 执行器部署（10-22 分片上传→文件落盘真机验证） | 模板制备/导入导出 tar 通道 |
| P5 长尾 | 冷迁移、重装（备份→重建→失败还原）、救援（配置快照）、导出、口令审计（读 shadow+crypt 比对）、SSH 开关（DenyUsers drop-in+踢会话）、配额处置（FORWARD 链）、平台修复 | ✅ 执行器部署（真机口令审计 36 账号验证；10-22 SSH 开关双向 reload 真机验收通过） | 热迁移/脏页采样、来宾自动化（需 guest-agent）、takeover 完整重建 |

## 3. 实现约束（继承既有决策）

- agent 是**唯一直接操作 libvirt/KVM 与宿主系统**的角色（ADR-0005）；libvirt 客户端用纯 Go 实现（无 CGO，交叉编译 linux/arm64），`virsh` 命令降级。
- 危险操作自带**节点侧**保险（uplink/端口镜像看门狗）；`firewall.rollback` 不允许失败；`host_firewall` 先写入后收紧。
- 幂等收敛类操作下发期望状态而非增量；`vm.xml.apply` 校验用 libvirt 校验器；源侧迁移数据在目标确认前不删。
- 阶段 Key 沿用 mock 的 stagePlan 词汇（前端时间线已按此渲染）；`Total=0` 表示总数未知。
- 真实 IP/密码/证书不入仓库（红线）；测试机操作遵循 TROUBLESHOOTING 只读优先与最小变更原则。
