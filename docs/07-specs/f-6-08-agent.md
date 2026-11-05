# F-6-08 节点 agent（真实通道与领域执行）

- 状态：In Progress（P0~P5 执行器已全量注册；全量真机验收与发布见 ROADMAP §5.2）
- 负责人：—
- 关联：[f-6-01](f-6-01-node-onboarding.md)（接入与通道协议）· [ADR-0005](../06-decisions/0005-control-plane-node-agent-architecture.md) · [`internal/agentproto/agent.proto`](../../internal/agentproto/agent.proto)（线上契约唯一来源）

## 1. 已交付（P0，通道骨架）

**架构**：`cmd/agent`（enroll/run 两子命令）+ `internal/nodeagent`（agent 侧：TLS 身份、重连退避、执行器注册表）+ `internal/agent/hub`（控制面侧：连接注册表、PKI 自举、Execute 路由、心跳落库）。传输只有 gRPC over TLS（mTLS），协议形状见 proto；2026-11-04 起 mock 已删除。

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

**2026-10-26 三轮深检验收（真机，同一环境）**：安全组兜底+挂载合成验证；端口安全
**precheck 缺失致功能整体不可用**补实现后 apply→停用流表归零（fake internal 口实态）；
端口转发 add→remove 全清（remove 不对称残留修复）；双层防火墙带保护规则落链、
**回滚只清 KC-FW 致 KC-HFW 残留**修复后归零；公网 IP bind 假成功（executor 不发
vm_ip）改节点解析 + 关机 fail-fast + unbind 形状匹配删除。全部以节点实态
（iptables -S/-t nat -S、ovs-ofctl dump-flows、ovs-vsctl）对读验证，残留清零。


**2026-10-27 P5 第二批验收（真机双节点 rig）**：同机双 agent（kvm-host-2 以私有 mount namespace
运行：镜像目录私有、staging 共享，等效真实双节点+控制面中转已回放）。温迁移（save→staging→
幂等落位→UUID 守卫→define+restore→记录→源清理）与 offline 全链均一次通过；失败重试链路（暂存
重发/幂等落位/unknown 放行）实测；live 阻断与 guest 无 agent 诚实拒绝验证。迭代中真机抓出并修复：
staging 未建目录、restore 无 --persistent（瞬态域重启即丢）、restore 失败后重试的状态死锁、
takeover 放置非幂等、英文 locale 域不存在误判。已知同机伪影（真双节点不存在）：共享 libvirtd 使
cleanup 波及目标侧定义与 NVRAM。跨节点分片中转（relay 循环）待真实两机环境接入。

## 2. 分期路线（P0~P2 真机验收通过；P3~P5 执行器已全量注册并部署，深化项见各条注记）

| 期 | 内容 | 状态 | 深化项（后续迭代） |
|---|---|---|---|
| P1 只读探测 | 23 个探测操作全部真数据 | ✅ 真机对账通过 | host.hardware 内存条（需 SMBIOS）|
| P2 生命周期+控制台 | create/clone/delete、电源、快照、光驱、截帧、VNC 流 | ✅ 真机验收通过（aarch64 UEFI+ramfb 建→开→截帧→删；10-22 复验：多数据盘创建/删除、光驱挂载） | 槽位记账精化；克隆 E2E |
| P3 网络 | vpc.switch（OVS+dnsmasq+NAT）、桥/上行口（看门狗）、安全组（per-VM 链）、双层防火墙、端口转发 DNAT、公网 IP DNAT、端口安全（OVS 流表）、镜像、抓包（tcpdump）、收敛修复、ACL/交换机迁移/计数/带宽/IPv6 | ✅ 执行器真机部署（10-22 镜像全链路验收）；长尾 14 项 2026-11-04 接通、单测通过 | OVS 原生流表口径深化；交换机迁移真机验收 |
| P4 存储与模板 | 池格式化挂载/删除/配置/卸载/trim、分区、LVM 卷、文件提交与读取、9p 共享、模板制备/维护/导入导出/预处理、镜像导入 | ✅ 执行器部署（10-22 分片上传→文件落盘真机验证）；模板/镜像 14 项 2026-11-04 接通、单测通过 | 模板包大文件真机验收；virt-customize 环境验收 |
| P5 长尾 | 冷迁移、重装（备份→重建→失败还原）、救援（配置快照 + CDROM 启动顺序）、导出（qcow2/raw/OVA）、口令审计（读 shadow+crypt 比对）、SSH 开关（DenyUsers drop-in+踢会话）、配额处置（FORWARD 链）、平台修复 | ✅ 执行器部署（真机口令审计 36 账号验证；10-22 SSH 开关双向 reload 真机验收通过） | live 热迁移（架构边界阻断）、来宾自动化（需 guest-agent）、takeover 完整重建 |

### 2.1 mock 兜底项补齐（2026-11-04 完成）

控制面 `internal/service` 实际下发 120 个 OpKind，`NewRegistry` 原注册 86 项；差额 37 项在 mock 下走 `Execute` 默认分支返回 `Success=true`，真实 agent 下会返回「未实现」。已按 VM → 模板 → 镜像 → 网络 → 存储/宿主分批补齐，注册表当前 **124 项，控制面引用缺口 0**（含 2026-11-04 新增的 `vm.console.update`）。

**已接通（单测通过，真机待验）**：
- VM 6 项：`vm.adopt`、`vm.xml.apply`、`vm.disk.change`、`vm.disk.resize`、`vm.disks.independent`、`vm.interface.change`
- 模板 12 项：prepare / delete / rebase / flatten / promote_child / promote_delete / preprocess / export / export.delete / export.fetch / import.preview / import
- 镜像 2 项：`image.parse`、`image.import`
- 网络 14 项：VPC ACL、交换机迁移/重配、端口释放、计数重置、全局带宽、IPv6 策略、公网 IP 预览/重载/来宾状态/IPv6 检测、抓包取回/删除
- 存储/宿主 3 项：`storage.partition.list`、`host.pci.bind`、`host.tuning.apply`

**已修的契约缺口**：
- 模板派生链维护新增 `parent_disk_path / child_disk_paths`（节点只认磁盘路径，不认数据库 ID）。
- 模板包格式固定为 `tar.gz`（`manifest.json` + `disk.qcow2`，磁盘 SHA256 + 包 SHA256）；`template.export.fetch` 改为 offset/length 分片，控制面 `ExportFile` 改为流式转发。
- `vm.interface.change` 增补 `bridge_name`（控制面由 `switch_id` 解析）。
- `public_ip.reload` 增补 `bindings` 列表（节点不知道控制面的绑定关系）。

**2026-11-04 同时删除**：`agent.MockClient`、`AGENT_TRANSPORT`、`internal/devdata`、模拟节点、`/api/v1/dev/agent-register`、前端模拟标记（见 [ADR-0013](../06-decisions/0013-remove-mock-transport.md)）。测试改为每包自写 stub。

### 2.2 第一轮复审后的补强（2026-11-04）

第一轮“注册即接通”之后，逐项对照真实命令路径，又修了以下问题：

- **长耗时命令超时**：`vm.create` 完整克隆、`vm.reinstall`、`vm.export`、`vm.migrate` 的 `qemu-img convert` / `cp` 原先仍用 10s 探测超时；已改用 `RunTimeout`。
- **OVA 导出**：控制面允许 `ova`，但 `qemu-img` 没有 ova 输出格式。现改为 VMDK（streamOptimized）+ OVF + SHA1 清单打 tar，`export.fetch` 的 MIME 为 `application/ovf`。
- **假成功**：
  - `vm.floppy.apply` 从“返回成功但什么都没做”改为真实 attach/detach，控制面补 `rel_path`（创建时从 `storage_file` 解析）。
  - `vm.staticip.change` 改为写 dnsmasq `dhcp-host`；控制面补 `mac / bridge_name / dhcp_start / dhcp_end`。
  - `platform.repair` 改为按控制面下发的完整 `items`（端口安全 / 公网 IP / 端口镜像）重新下发，不再固定返回 `repaired: 0`。
  - `vm.nvram.repair` 改为备份并用固件模板重置 NVRAM。
- **公网 IP 模式**：`nat_1to1` 走 DNAT；`routed` 添加路由与 proxy ARP；`bridged` 不做宿主侧 NAT/路由，并要求来宾自行配置地址。
- **交换机迁移**：控制面补 `old_uplink_if`，节点在接入新上行后摘掉旧上行，并校验目标口不属于其它 OVS 桥。
- **任务恢复**：启动时把遗留 `running` 转为 `unknown`，宽限期后落 `failed` 并释放资源锁；新增 `recoverStale / sweepUnknown`。
- **运行态对账**：指标采集器每轮先读 `OpNodeDomains`，把 VM 的 `status / present / last_synced_at` 校正到真实域列表。
- **抓包下载**：`capture.fetch` 改为 offset/length 分片，控制面与 handler 流式转发，不再有 32 MiB 上限。
- **端口安全 PPS**：`port_security.apply` 写 OVS meter，不再忽略 `pps_limit`。
- **救援模式**：进入救援时把 CDROM 放到 boot 顺序第一位，退出时校验还原结果。

**仍未完成 / 有意的边界**：
- live 热迁移仍被 ADR-0005 的节点间互信边界阻断（offline/warm 可用）。
- `vm.migrate.assess` 的**带宽**仍按源节点默认路由网卡协商速率估算；跨节点真实测速需要双端协同。热迁移评估的脏页已改为触发 `domdirtyrate-calc` 后采样。
- `template.preprocess` 的 `install_agent / inject_ssh_key` 依赖节点 `virt-customize` 与来宾网络，缺工具链时返回 `Unavailable`。
- 模板/镜像/救援等新路径只做了单测与命令边界验证，**仍需 Linux + libvirt 真机验收**。
- `repository` 分层按域迁入中（2026-11-05 已迁入 `vmtag`，其余域待续）；单实例队列/实时总线、agent root 运行仍属架构级待办。控制面已加会话级单实例锁防双开，不等于支持多实例。

## 3. 实现约束（继承既有决策）

- agent 是**唯一直接操作 libvirt/KVM 与宿主系统**的角色（ADR-0005）；节点侧通过 `virsh` 命令操作 libvirt（无 CGO 依赖，可交叉编译 linux/arm64），命令输出由 `internal/nodeagent/sys/` 的解析器还原为结构体，真机脱敏 fixture 见 `sys/testdata/`。
- 危险操作自带**节点侧**保险（uplink/端口镜像看门狗）；`firewall.rollback` 不允许失败；`host_firewall` 先写入后收紧。
- 幂等收敛类操作下发期望状态而非增量；`vm.xml.apply` 校验用 libvirt 校验器；源侧迁移数据在目标确认前不删。
- 阶段 Key 沿用既有阶段词汇（前端时间线已按此渲染）；`Total=0` 表示总数未知。
- 真实 IP/密码/证书不入仓库（红线）；测试机操作遵循 TROUBLESHOOTING 只读优先与最小变更原则。

## 4. P5 第二批设计（迁移三模式 / takeover 完整重建 / 来宾自动化）

### 4.1 迁移三模式（诚实的能力边界）

| 模式 | 实现 | 停机窗口 | 说明 |
|---|---|---|---|
| offline（冷迁移） | 源：停机 → dump XML → 盘/NVRAM 复制到 staging → undefine 源；目标：takeover 从 staging 重建 | 全程 | 本批交付 |
| warm（温迁移） | 源：`virsh save` 落内存态文件 + 同 offline；目标：`virsh restore --xml` 恢复**运行现场** | ≈ 传输时长（但不重启、内存现场保留） | 本批交付；libvirt ≥6.0 的 `restore --xml` 使 XML 重映射与内存恢复可同时成立 |
| live（热迁移） | **诚实阻断** | — | libvirt 对等连接（qemu+ssh/tcp）要求节点间互信凭据，与 ADR-0005「控制面不持有宿主机登录凭据」冲突；待节点间通道定型（专用传输证书或受限 ssh 指纹登记）后接入 |

**源侧数据在目标确认前不删**的边界细化：磁盘文件在 cleanup 阶段（记录落库成功之后）才删；
域**定义**在源侧 staging 完成后即 undefine——同名双活比「暂时无定义」危险（有人可能把源侧那份
开机，而控制面已指向目标），且定义可随时由已返回的 XML 重建，恢复路径无损。

### 4.2 传输与 staging

- staging 根：`/var/lib/k_cockpit/migrate/<vm>/`，条目命名沿用源盘 basename（+ `<vm>.memstate` / NVRAM basename），manifest 随 `MigrateResult` 回传（`xml`/`files`/`memory` 三个数据键）。
- 跨节点中转：控制面拉取 staging 分片 → 回放到目标 staging（复用导出/分片上传通道）。**本批不实现 relay 循环**——单节点测试环境无法验证；同机双 agent（`--state-dir` 区分身份，共享文件系统使中转退化为 no-op）已可端到端验证全部编排与两侧执行器。跨节点 relay 随两节点环境补齐。
- cleanup（`vm.migrate` 带 `action=cleanup`）：源删原盘 + staging，best-effort——失败只留日志（源残留可手工清，绝不因清理失败判迁移失败）。

### 4.3 takeover 完整重建（目标侧）

staging 文件落位到 images 目录（属主/AppArmor 归一）→ XML 磁盘路径重映射 → NVRAM 放回
`/var/lib/libvirt/qemu/nvram/` → 有内存态则 `virsh restore --xml`（恢复后即运行），否则
`virsh define`。时序：源成功之后、控制面改记录之前（失败时记录仍指源，重试安全）。

### 4.4 来宾自动化（vm.guest）

- 通道：`virsh qemu-agent-command`（guest-ping 探测；未装 agent 如实报「请在虚拟机内安装 qemu-guest-agent」）。
- `password_online`：`guest-set-user-password`（base64 明文）。
- `expand_disk`：宿主侧 `qemu-img resize`（运行中叠加 `virsh blockresize`），来宾侧 `guest-exec` 跑 `growpart` + `resize2fs`/`xfs_growfs`。
- `disk_attach`：宿主侧建盘 + `attach-device --config`，来宾侧 `guest-exec` 分区/格式化/挂载。
- `password_offline`：依赖 libguestfs（virt-customize）；未安装时**如实报不可用**，不伪造成功。
- guest-exec 辅助：`guest-exec` + 轮询 `guest-exec-status`（500ms×60 上限），输出 base64 解码后回传。
