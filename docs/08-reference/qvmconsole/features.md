# QVMConsole 功能与实现结论

> 状态：生效
> 最后更新：2026-09-13
> 结论基于提交：`52023d6`（`reference/QVMConsole`，分支 `main`）
> 关联：[`../README.md`](../README.md) · [`README.md`](README.md)（工程概览）· [`architecture.md`](architecture.md)（工程架构）· [`capabilities.md`](capabilities.md)（功能清单）· [`pitfalls.md`](pitfalls.md)（踩坑清单）

本文只记录 QVMConsole **有哪些功能、分别是怎么实现的**，作为我们实现类似能力时的参考。**不含** `k_cockpit` 自身的技术选型与方案（见 [`../README.md`](../README.md) §2）。文中路径均相对 `reference/QVMConsole/`。

> **功能面清单**（按页面、操作、配置维度、运行态约束组织）见 [`capabilities.md`](capabilities.md)；本文侧重"功能 → 实现机制 → 关键文件"。

> QVMConsole 是面向单机的 KVM/QEMU 虚拟机管理面板。它是我们的**功能参考**，其单机绑定等限制不构成我们的设计约束。

---

## 0. 并发控制模型（理解各功能的前提）

它没有全局 VM 互斥锁，而是分四层控制并发：

| 机制 | 作用 | 实现要点 |
|---|---|---|
| 任务队列 | 所有重操作（创建/克隆/删除/快照/迁移/重装/导入）都入内存队列，由 worker 串行化 | `server/taskqueue/queue.go` |
| 迁移锁 | 迁移中的 VM 禁止其它操作 | 查队列中是否存在该 VM 的迁移任务 `service/vm/migration/lock.go` |
| 快照锁 | 快照中的 VM 禁止电源操作 | 查队列 + 兜底查 `virsh domstate --reason` 的 `paused (saving)` `service/snapshot/snapshot_lock.go` |
| 来宾操作锁 | 同一 VM 的磁盘/改密等来宾操作串行 | `sync.Map[vmName]*sync.Mutex` `service/guest_agent/client.go` |

另有一个 `VMLock`（`model/vm_lock.go`）是**业务软锁**而非并发锁：锁定后禁止删除、禁止移动磁盘，关机仅提示。

---

## 1. 虚拟机生命周期

- 入口 `server/handler/vm.go OperateVm`，落点 `server/service/vm/lifecycle.go`。
- **开机做了大量前置修复**：修 `on_reboot=destroy`（直接 sed 改 `/etc/libvirt/qemu/<name>.xml`）、清理不完整的 `backingStore` XML（防 AppArmor 拦截链式盘）、校验 UEFI NVRAM 文件存在、迁移/配额/维护模式检查；**开启端口安全时先以 `--paused` 启动 → 装防火墙策略 → 再 resume**，避免恢复瞬间裸奔。
- 区分暂停类型：普通暂停走 `ResumeDomainRPC`；**QEMU internal-error 暂停**（解析 `info status`）则报错要求重置。
- **全项目双路径**：优先 go-libvirt RPC，失败降级 `virsh` 命令行。
- 删除 `service/clone/delete.go DeleteVMWithDisks`：先删全部快照 → 断电 → 收集/解绑网络（IP、端口转发）→ 收集磁盘（含按 `<name>.` 前缀**目录扫描补漏**）→ `undefine --nvram --snapshots-metadata` → 删盘（**保护模板目录**，用路径前缀判断）→ 清状态/运行时/VPC 绑定/定时任务。另有 `ForceDeleteVM` 处理僵尸 VM（失败时直接删 libvirt XML + autostart 软链 + 重启 libvirtd）。

---

## 2. VM 创建（亮点：XML 生成方式）

- 调度：`handler/vm_create.go CreateVm` 做同步校验（名称/磁盘/交换机/配额/VPC）后 **转异步任务**，不阻塞请求。
- 表单维度很宽（`CreateVmRequest` 约 40+ 字段）：vcpu/max_vcpu/ram/disk_format/disk_bus/osinfo/iso/nic_model/boot_type/boot_order/watchdog/video/spice/cpu_topology/cpu_limit/cpu_affinity/virt_type/arch/extra_nics/extra_disks/host_devices/pcie_root_ports/...
- **磁盘**：`qemu-img create -f qcow2`，稀疏不预分配。
- **XML 不手拼**：调用 **`virt-install --print-xml`** 生成后再取 `<domain>` —— 把机型、引导、CPU 拓扑等复杂参数交给 virt-install，避免手写 XML 出错。
- **引导类型**：`--boot uefi` / `uefi,firmware.feature0.name=secure-boot`，再后处理建 NVRAM；**Windows + i440FX 会被强制改为 BIOS**（否则卡固件画面）；ARM 把 SATA CDROM 改为 USB。
- **网络**：只用 OVS（非空 `SwitchID`），否则 `--network none`，不建裸桥。
- 普通创建**不做系统初始化**（只挂 ISO 装机）；初始化只发生在模板克隆链路（§3、§4）。
- 后处理注入：memballoon、PCIe root port 预留、RTC、GuestAgent、SMBIOS、视频模型、CPU 限速/亲和、嵌套虚拟化、KVM 隐藏、vendor_id、direct boot、SPICE 等。
- **失败全链路回滚**（undefine + 删盘）。

---

## 3. 模板系统（设计含量最高）

### 3.1 从运行中的 VM 制作模板（`service/template/prepare.go`）
- 前置：VM 必须**已关机**；先采集默认硬件配置（vCPU/RAM/盘大小/总线/网卡/视频/CPU 拓扑）。
- **磁盘三选一**：`qemu-img convert -c`（压缩，子模板用 `-B` 保持父 backing）/ `mv` 搬移 / `cp --sparse=always`。
- **模板族**：按系统盘 backing 反查父模板，写 `TemplateUID/ParentNodeID/RootNodeID`；子模板 `CloneVisible=false`。
- UEFI：拷 `.nvram.fd`；Linux：制作时一次性装 cloud-init/growpart。
- 落 `.meta.json`（含 MD5+SHA256+size），并 `chattr +i` 设为不可变；失败按阶段回滚。

### 3.2 `.meta.json`（`service/template/types.go TemplateMeta`）
`type`(linux/windows/fnos/openwrt/other)、`category`、`boot_type/boot_verified/nvram_path`、`cloud_init_mode`(nocloud/configdrive/fnos/none)、`template_user/post_boot_command/post_boot_blocking`、`default_config`、模板族 `template_uid/node_id/parent_node_id/root_node_id`、`clone_visible/disabled`、`md5/sha256/file_size` 等。

### 3.3 导入 / 导出（tar.gz + 分片上传）
- 导出 `service/template/transfer.go`：按 `scope=node|root` 收集**模板族子树** → 逐节点算哈希写 `manifest.json` → 非 qcow2 先转 qcow2 再重算哈希 → 打包 `tar -czf`。
- 包结构：`manifest.json` + `<nodeID>.qcow2` + `<nodeID>.meta.json`。
- 导入：`PreviewImportTemplate`（校验 manifest、判 create/update、节点冲突、发一次性 token）→ `ImportTemplate`（**先验原始哈希，不匹配即拒绝** → 转 qcow2 → **MD5+SHA256 双校验** → 保存 meta → `chattr +i`）。
- 上传 `handler/upload_chunk.go`：分片 + **秒传** + 断点续传 + 缺失分片清单，单分片上限 4MB。

### 3.4 多系统初始化（克隆时改主机名/IP/密码，全部免 SSH）
| 系统 | 实现方式 |
|---|---|
| Linux | `virt-customize --no-network` **离线**注入 NoCloud cloud-init（`meta-data`+`user-data`），清 machine-id/DHCP 租约/cloud-init 缓存，`--password` 离线改密，首启执行 growpart/LVM 扩容 |
| Windows | 生成 **OpenStack ConfigDrive ISO**（label `config-2`）挂 CD-ROM，cloudbase-init 读取；用 guest agent **轮询日志等初始化完成后自动弹出 ISO** |
| OpenWrt | ext4 布局用 `virt-customize --upload`；squashfs+overlay（iStoreOS）用 `guestfish` 写 overlay 分区（**仅文件注入，不用 run-command**，因 BusyBox 缺工具） |
| FnOS | `virt-customize` 建用户、chpasswd、置初始化标记 |
| 不初始化 | `DisableSystemInit` 或 `cloud_init_mode=none`；`other` 类型强制不初始化 |

---

## 4. 克隆

- 同入口 `handler/clone.go CloneVm`（单）/ `BatchCloneVm`（批），`clone_mode=linked|full`。
- **完整克隆**：`qemu-img convert -O qcow2`（脱离模板依赖）；**链式克隆**（默认）：`qemu-img create -B <模板盘>`（backing chain，省空间但依赖模板盘）。
- **身份改写**由初始化模式决定（§3.4）；主网卡 MAC 用 `52:54:00:*` 写入 netplan，额外网口用 `99-qvm-hotplug.network` 做 DHCP 兜底。
- 磁盘大小 `ResolveCloneDiskSizeGB`：不小于模板虚拟大小、**只增不减**。
- 纯链式克隆 `linked_clone.go`：不做来宾初始化，走 `DefineDomainXMLRPC`。
- 克隆取消：清 VM + 盘（`cleanupLinkedCloneArtifacts`）。

---

## 5. 快照（兼容性问题处理值得借鉴）

- 核心 `service/snapshot/core.go`，全部经任务执行（action：create/revert/delete/delete_all）。
- **策略**：运行中+含内存 → 内部快照（可选先 suspend→建→resume）；运行中不含内存 → **外部快照**（`--disk-only`）；关机 → 内部快照。
- **UEFI NVRAM 问题**：NVRAM 为 raw 格式时 libvirt **不支持建内部内存快照**。解决：关机时把它 `qemu-img convert` 转成 qcow2 并改 XML `format='qcow2'`；运行中则提示（可选自动关机转换再开机）。
- **共享目录问题**：挂 9p/VirtFS 时 libvirt 禁止含内存内部快照，直接拦截并提示。
- **外部快照恢复**不能用 `snapshot-revert`：改为读快照层 → **新建可写 overlay 作为新活动盘**（不 commit，避免污染早期快照）→ 改 XML 盘路径 → 修权限 → `snapshot-current` 同步指针。
- **AppArmor**：展开 `qemu-img info --backing-chain` 全链路径，写入 `virt-aa-helper` 规则并 reload（支持自定义挂载点）。

---

## 6. 重装

`service/clone/reinstall.go`：需**严格二次验证**，且同一 VM 只允许一个进行中重装。
流程：校验引导族兼容（BIOS↔BIOS / UEFI↔UEFI）→ 清空全部快照 → 断电 → **原系统盘 rename 成 backup** → `qemu-img create -b 模板` 建新系统盘 → 按模板类型离线初始化 → 改 XML（注入 ConfigDrive）→ 启动；全程 `defer` 保护，失败还原原 XML 与 backup 盘。

---

## 7. 迁移（跨节点）

编排 `service/vm/migration/execute.go`；预检 `preview.go`（目标同名 VM/存储不足/backing 校验/公网 IP/网络匹配）。

- **冷迁移**：逐盘 `test ! -e` → **rsync 复制 overlay** → `chown libvirt-qemu` → 目标 `virsh define`。
- **热迁移**：目标端**预建空 overlay + NVRAM**，再本机 `virsh migrate --live --persistent --copy-storage-inc --migrateuri tcp://... --disks-uri tcp://... qemu+ssh://...`（增量拷盘 + 内存热迁）。
- **停机策略/评估**：冷迁移要求源先关机；热迁移先做**线路测速 + 脏页速率评估**（`virsh domdirtyrate-calc`），脏页速率过高直接拒绝或自动 **CPU 限流**（`virsh schedinfo`，defer 还原）。
- **回滚**：热迁移失败 `defer` 清理目标端预建盘/NVRAM。
- **接管**：迁移完调目标面板 `POST /api/migration/adopt-vm`（确保 NVRAM、建/更新用户、绑 VPC、同步凭据、重建端口转发）。

---

## 8. 磁盘与设备

- **热插拔** `service/storage/disk/create.go`：q35 机型**手工分配空闲 pcie-root-port 的 `<address>`**；**PCIe 槽耗尽自动降级 virtio-scsi**。
- **扩容**：`virsh blockresize` + `qemu-img resize`；支持 `AutoGrowPartition`（进来宾内 growpart/LVM 扩容）。
- **IOPS 限制**：构造 `libvirt.TypedParam`（`total_iops_sec` 与 `read/write_iops_sec` **互斥**），`SetBlkIOParametersRPC`。
- **光驱/软盘/ISO**：热插拔 CD-ROM、多 ISO 挂载、弹出。
- **PCI 直通** `service/vm/passthrough.go`：加载 vfio → 校验 → 绑定 `vfio-pci` → 注入 hostdev；修改直通需先关机。
- **救援模式** `service/rescue/rescue.go`：断电 → 记录原配置 → 盘改 SATA、网卡改 e1000e、挂救援 ISO、引导改 `cdrom,hd` → 开机；退出时逆操作还原。

---

## 9. VNC / SPICE

- **VNC 获取连接** `service/vnc/vnc.go GetVncConnInfo`：先解析 XML 的 `socket=`（**Unix Socket 优先**），否则 `virsh qemu-monitor-command --hmp "info vnc"` 取 TCP 端口。
- **双向代理** `handler/vnc.go VncWebSocket`：升级 WS（subprotocol `binary`）→ `net.Dial` 到 VNC → 两个 goroutine 双向转发，`context.WithCancel` 任一端断开即取消；公网会话 15s 校验、键鼠操作按分钟节流续期，失效发 `CloseMessage(4001)`。
- **SPICE**：**不走面板代理**，客户端直连 QEMU SPICE 端口；面板提供状态/开关/改密/暴露，并下发 `.vv` 连接文件。

---

## 10. 列表/详情缓存与实时（SSE）

- `VMCache` 表（`model/vm_cache.go`）存列表投影；**同步机制 = 启动时全量一次 + 管理员访问列表时按 8s 冷却异步刷新**（防抖 + 单飞），**非定时、非事件驱动**；对账用 `OnConflict(UpdateAll)` upsert，未出现者标 `present=false`。
- 列表读缓存 `ListCachedVMs WHERE present=true`，不强行查 libvirt。
- **SSE**：列表 `/api/vm/sse` **2s**、详情 `/api/vm/:name/sse` **3s**；libvirt 不可用推空数组；15s 校验会话。
- **避免卡顿的关键**：CPU 等采样由**后台采集器每 10s 更新内存缓存、每 60s 落库**，列表/详情只读缓存；详情页按"当前可见标签"才拉附属数据。

---

## 11. 批量任务

- 批量克隆 `service/clone/batch.go`：`context` 可取消 + **信号量并发**（默认 10）+ `sync.WaitGroup`；命名 `<prefix>-<序号>`；单台失败不阻断，取消则全局停止；配额按 `×Count` 计算；禁止复用同一组物理直通设备。

---

## 12. 网络虚拟化

| 功能 | 实现要点 | 关键文件 |
|---|---|---|
| VPC 逻辑交换机 | 每个交换机在 OVS 建 `type=internal` 网关端口（带 `tag=VLANID`）+ **独立 dnsmasq** 提供 DHCP + `MASQUERADE` 出网；启动时全量恢复运行态并清孤儿端口 | `service/network/vpc/switch.go`、`switch_runtime.go` |
| 安全组/ACL | 编译成 **nftables** 表（非 OVS 流表）：出站编译为 `reject`、入站为 `accept`、最后统一 `reject`；规则顺序强制"拒绝先于放行、放行先于 established"；`nft -c` 校验后原子重载 | `service/network/vpc/acl.go` |
| 端口转发 NAT | iptables：**`nat PREROUTING` 与 `nat OUTPUT` 都写**（保证宿主机本地访问也生效）；目标 IP 实时解析（静态绑定/DHCP 租约/邻居表）；规则持久化文件 + 启动恢复 | `service/network/port_forward*.go` |
| 静态 IP | 写各交换机 dnsmasq 的 dhcp-hostsfile 按 MAC 固定；运行中 VM 通过 detach/attach 网卡强制刷新 DHCP 生效 | `service/network/static_ip.go` |
| 公网 IP | `PublicIP` 资源池 + `PublicIPBinding`；下发时生成**带清理段的 bash `rules.sh`** 统一执行（NAT / classic_route / classic_bridge 三模式），应用前备份旧脚本 | `service/public_ip/*.go` |
| 双层防火墙 | VM 层策略存 JSON → 编译 nftables 独立表（支持 dry-run/回滚）；宿主层用 `ufw` 并自动探测 SSH/面板端口做保护 | `service/firewall/*.go` |
| 带宽限速 | 三套并存：libvirt `domiftune` + 运行态 **tc/IFB**；OVS 环境用 **OpenFlow meter（上行）+ QoS/Queue（下行）**；用户级"配额均分到其 VM"的再分配 | `service/bandwidth/*.go` |
| 流量统计 | **不引入独立计数器**，从 `VmStatsRecord` 的网卡累计字节取**相邻正增量求和**（负值视为计数器归零），按月窗口汇总（带 `Offset` 基线支持重置），超阈值进"惩罚速率" | `service/traffic/quota.go`、`service/network/vpc/traffic.go` |
| 抓包/诊断 | `tcpdump` 双进程（写 pcap + 输出摘要行），协程监控文件大小上限，支持超时/取消/下载 | `service/network/diagnostics/*.go` |

---

## 13. 存储管理

- **识别**：`lsblk -J` + `findmnt` + `df` 拼设备树，过滤 loop/rom/ram，注入 LVM 层级与 VM 磁盘占用，再与管理员配置表 `HostStoragePool` 合并。
- **格式化/挂载**：严格卸载（正常→`fuser` 杀占用→按设备卸载，**禁止 lazy umount**）→ `wipefs -a` → `mkfs.<fstype>` → `blkid` 取 UUID → 写 `/etc/fstab`(nofail) → `mount` → 建 `vm-disks` 目录并配 libvirt/AppArmor 权限。
- **分区/LVM**：分区增删、PV/VG/LV 管理，均为异步任务。
- **用户存储配额**：**回环镜像文件 + ext4 project quota**（按 UID 分配 project ID）在内核层强制写入限制；配 fstrim + `fallocate --dig-holes` 定时回收。

---

## 14. 认证与会话（借鉴价值高）

- **多阶段登录**：不同阶段发不同 `token_type`——`bootstrap`（30min 受限）/ `login`（15min，用于换 2FA）/ `access`；流程为 密码校验 → （需要时）2FA → 换 access token。
- **JWT 指纹绑定**：`SHA256(IP 前 3 段 + User-Agent)` 截断 base64，不符即 401"登录环境变化"（可通过设置关闭）。
- **服务端会话 `UserSession`**：session_id + 过期 + 最后活动时间；**公网请求要求空闲 <30min** 且只由前端上报的真实活动续期；改密/改用户名后按 `iat` 立即失效旧 token。
- **TOTP**：`pquerna/otp`（SHA1/6 位/30s）；绑定后生成 **10 个恢复码**（剔除易混字符），恢复码只存 SHA-256，明文只返回一次，比对用常量时间。
- **API Key**：`kvm_id_` + `kvm_sk_` 两段；明文只返回一次；库中只存 `SHA256(SecuritySecret + ":" + key)`。
- **高风险二次验证（428）**：敏感操作检查 `X-High-Risk-Token`；未通过时返回 **428** + 验证方式（totp / email / totp_email）→ 前端弹窗验证 → 带 token **重试原请求**（单飞锁 + 防死循环标志）。签发的高风险 token 有效期 5 分钟、绑定 operation。
- **密码泄露检查**：**HIBP k-匿名**（只发 SHA-1 前 5 位，后缀本地比对，30min 缓存）；账户只存"密码 SHA-1 的 HMAC 指纹"；支持每日批量扫描（同前缀合并请求），命中后按角色分级处置。
- **登录限流**：IP（5 次失败/5min）+ 用户名（10 次/15min）双维度内存计数器；另有全局滑动窗口限频中间件。
- **邮件动作令牌**：随机 32 字节明文 + SHA-256 入库（只存哈希）；同用户同用途旧令牌先作废；找回密码为"验证码 → 账号列表 → 选择 → 重置"多阶段。

---

## 15. 任务中心与 SSE

- **任务队列**：纯内存（不持久化）；`taskChan` 缓冲 100 + 3 个 worker；`context.WithCancel` 支持取消（等待中直接标记、运行中触发 cancel）；每小时清理 24h 前已终态任务；**查询按用户隔离**（非 admin 只看自己的）；返回前对参数**脱敏**（password/token/secret → `******`）。
- **任务类型**：约 40+ 种（创建/克隆/模板/快照/导入导出/磁盘/防火墙/OVS/公网 IP/存储/迁移/定时任务/密码扫描…）。
- **SSE 通道**（共 4 类）：VM 列表/详情（2s/3s）、任务进度 `/api/task/sse`（事件驱动）、宿主监控 `/api/host/stats/sse`（5s）、调度事件。
- **鉴权**：Token 放 **query 参数 `?token=`**（因 `EventSource` 无法自定义请求头）。
- **心跳/过期**：各通道靠固定 ticker 顺带做心跳，每轮校验会话有效性，失效推 `session_expired` 并关闭连接；前端统一 5s 重连。

---

## 16. 监控与统计

- **VM 指标**：经 go-libvirt RPC——两次 `DomainGetCPUStats` 采样算 CPU%、`DomainGetMemoryStats`、解析 XML 取网卡后 `DomainGetInterfaceStats`、首个非 cdrom 盘 `DomainGetBlockStats`；RPC 不可用降级 `virsh`。
- **宿主指标**：读 `/proc/stat`、`/proc/meminfo`（优先 `MemAvailable`）、挂载统计、`iostat`、`/proc/net/dev`、`/proc/diskstats`（排除 lo/virbr/vnet）、KSM 读 `/sys/kernel/mm/ksm/*`。
- **频率**：后台**每 10s 采集写内存缓存、每 60s 持久化**到 `VmStatsRecord` / `HostStatsRecord`（宿主记录含设备级 JSON）；历史按时间范围查询。

---

## 17. 用户 / 配额 / 多租户

- **两种云类型**：`elastic`（弹性云，自助网络/VPC/存储）vs `lightweight`（轻量云，按 VM 配额、菜单受限）。
- **配额校验**：创建/编辑/开机分别用 `CheckQuota` / `CheckQuotaForEdit`（按增量）/ `CheckQuotaForStart`（叠加运行中资源）；存储配额在内核层由 project quota 强制。
- **运行时长配额**：按"上次观测时间差 × 运行中 VM 数"累计，超限提交关机任务。
- **VM 归属**：用**文件**维护（`VMAccessDir/<username>` 每行一个 VM），并据此重新生成 **polkit** 规则，让 libvirt 侧也按归属授权；中间件鉴权同样读该文件。
- **邀请注册**：管理员创建 pending 用户 → 发邀请令牌邮件 → 校验令牌设密码激活（公网下管理员另有 bootstrap 安全初始化）。
- **流量/时长重置**：流量按"月"窗口，通过 `Offset` 基线重置而非清历史；运行时长不周期清零。

---

## 18. 定时器与后台任务（启动即拉起）

统计采集（10s/60s + 流量检查）、调度事件清理（每小时）、VM 定时任务（30s 扫描）、**JWT 密钥轮换**、上传会话清理、每日密码泄露扫描、每日存储回收（fstrim + dig-holes）、会话清理、IPv6 前缀监测、端口安全 reconcile。

启动时还做一系列 **restore**（`RestorePortForwardRules`、`EnsureAllVPCSwitchRuntime`、`RestorePortMirror`、`RestorePublicIPRules`…），让系统层残留状态与 DB、OVS、iptables/dnsmasq 对齐（**启动自愈**）。

> 各后台任务的启动函数、周期与文件位置见 [`architecture.md`](architecture.md) §4.2。

---

## 19. 系统设置

- **持久化与优先级**：所有设置以 key-value 存 `SystemSetting` 表；启动时先读环境变量再 `LoadFromDB` 覆盖（**环境变量 > 数据库 > 默认值**）；保存时同步写 `.env` 保证重启一致。
- **在线修改的副作用**：带宽变更触发异步重分配、端口安全变更触发 reconcile、维护模式变更提交进入/退出任务（失败回滚设置）等。
- **JWT 密钥轮换**：生成 36 字节随机密钥 → 写回 `.env`（0600）→ 即时替换运行时密钥；轮换后旧 token 全部失效；支持手动（需高风险验证）与定时。

---

## 20. 前端实现要点（机制层面）

- **请求层**：Axios 实例统一注入 `Bearer`、统一错误 Toast；**401 自动登出**；**428 用独立不拦截的 `rawClient` 调高风险验证，成功后注入 `X-High-Risk-Token` 自动重试原请求**（单飞锁 + 防死循环）。
- **SSE 消费**：各 Hook 自建连/解析/重连（5s）；统一处理 `session_expired`；详情页在前端用相邻累计字节算瞬时速率，并做"失焦停推、聚焦立即重连"。
- **菜单**：`ADMIN_NAV` / `USER_NAV` 两套，按角色渲染；路由守卫对轻量云做白名单限制。
- **大表单**：核心逻辑集中在单个 `useVmForm` Hook，管理全部联动（OS/ISO/模板/架构/机型/引导互推）；向导与编辑表单复用同一份逻辑，配 `sections/`、`defaults.ts`、`validators.ts`、`payload.ts`。
- **任务栏 TaskBar**：底部常驻抽屉，显示活动任务数/进度条/SSE 连接状态，可展开、可拖拽（高度持久化到 localStorage）。

---

## 附：关键文件速查

| 功能 | 关键文件（相对 `reference/QVMConsole/`） |
|---|---|
| 生命周期 | `server/service/vm/lifecycle.go`、`server/handler/vm.go` |
| VM 创建 | `server/service/vm/create.go`、`server/handler/vm_create.go` |
| 模板制作/元数据 | `server/service/template/prepare.go`、`types.go` |
| 模板导入导出/分片上传 | `server/service/template/transfer.go`、`server/handler/upload_chunk.go` |
| 克隆/批量克隆 | `server/service/clone/core.go`、`linked_clone.go`、`batch.go` |
| 多系统初始化 | `server/service/clone/linux_cloudinit.go`、`windows_configdrive.go`、`openwrt_init.go`、`fnos_init.go` |
| 快照 | `server/service/snapshot/core.go`、`external.go`、`nvram.go`、`disk_access.go` |
| 重装 | `server/service/clone/reinstall.go` |
| 迁移 | `server/service/vm/migration/{execute,assess,preview,adopt}.go` |
| 磁盘/设备 | `server/service/storage/disk/{create,crud,iops,cdrom}.go`、`server/service/vm/passthrough.go`、`server/service/rescue/rescue.go` |
| VNC / SPICE | `server/handler/vnc.go`、`server/service/vnc/vnc.go`、`server/handler/spice.go` |
| 缓存 / SSE | `server/service/vm/cache.go`、`server/service/host/stats_collector.go`、`server/handler/vm_sse.go` |
| 网络 | `server/service/network/**`、`server/service/public_ip/**`、`server/service/firewall/**`、`server/service/bandwidth/**` |
| 存储 | `server/service/storage/**` |
| 认证/安全 | `server/handler/auth.go`、`server/middleware/auth.go`、`server/service/security/**` |
| 任务中心 | `server/taskqueue/queue.go`、`server/handler/task.go`、`server/model/task.go` |
| 设置 | `server/config/config.go`、`server/handler/settings.go` |
| 前端 | `web/src/api/client.ts`、`web/src/stores/task.ts`、`web/src/features/vm-form/` |

---

## 变更记录

| 日期 | 变更内容 |
|---|---|
| 2026-09-12 | 创建文档：基于提交 `52023d6` 整理功能与实现结论 |
| 2026-09-13 | 由 `docs/04-engineering/REFERENCE_QVMCONSOLE.md` 迁移至 `docs/08-reference/qvmconsole/features.md`，拆分出工程概览（`README.md`）与工程架构（`architecture.md`） |
| 2026-09-14 | 头部关联补充 [`capabilities.md`](capabilities.md)（功能清单），并明确本文侧重实现机制 |
