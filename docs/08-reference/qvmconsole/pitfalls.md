# QVMConsole 踩坑与修复清单

> 状态：生效
> 最后更新：2026-09-13
> 结论基于提交：`52023d6`（`reference/QVMConsole`，分支 `main`）
> 关联：[`../README.md`](../README.md) · [`README.md`](README.md) · [`architecture.md`](architecture.md) · [`features.md`](features.md)

本文汇总 QVMConsole **已被修复过的问题**，用于我们实现同类能力时提前避坑。数据来源：

1. 提交历史中消息含 `fix` / `修复` / `hotfix` / `bug` 的提交 **88 条**（2026-06-02 ~ 2026-09，含提交正文匹配）；
2. `reference/QVMConsole/docs/` 下的专题修复与兼容性文档（57 篇中的相关部分）；
3. 2026-09-13 在真实宿主机（Ubuntu 24.04 + libvirt 10.0 + OVS 3.3.9）上的实测复现。

> **一个重要观察**：该仓库 3 个月、374 次提交里有 88 次是修复，且很多问题修过不止一次（见 §3 的 dnsmasq 案例）。**"长尾兼容性"才是这类系统的主要成本，而不是功能开发。**

---

## 1. 固件与引导（高频、连锁故障）

| 坑 | 根因与做法 | 证据 |
|---|---|---|
| UEFI NVRAM 创建失败 → 虚拟机启动失败 | OVMF 模板文件路径不存在，导致 NVRAM 无效，libvirt 启动时报 `operation failed`；**属连锁故障**（第 3 类错误其实源于第 2 类）。修法：创建前 `os.Stat` 检查 + 启动前再校验一次 NVRAM 存在 | `fix-report-2026-07-08.md`（4 类错误，其中 2、3 连锁） |
| 固件路径硬编码 | 不同发行版路径不同（`/usr/share/OVMF/...`、`ovmf` / `edk2-ovmf` / `qemu-efi-aarch64`），需按架构细分选择 | `fix(arch): 细化UEFI固件路径选择逻辑` |
| UEFI 克隆首启出现 `Boot Option Restoration` 倒计时 + 冷复位 | `shim/fallback.efi` 自动登记启动项导致；面板预置 `FB_NO_REBOOT=1`（需 `virt-fw-vars`，Debian 包名 `python3-virt-firmware`），NVRAM 用临时文件原子替换 | `uefi-clone-first-boot.md` |
| Windows + i440FX + UEFI 卡在 TianoCore 画面 | 强制改为 BIOS；**前端与后端都要做同样的兼容处理** | `vm-machine-type-selection.md` |
| 机型别名差异 | 提交 `i440fx` 后服务端要转成宿主机稳定别名 `pc`；ARM/RISC-V 固定 `virt` | `vm-machine-type-selection.md` |
| ARM 上默认开启 SPICE / QXL 导致创建失败 | 部分机器默认开 SPICE 会失败 → 改为高级选项显式开关 + 系统设置项；ARM64 不再注入不支持的 QXL 模型 | `fix: 修改部分机器因为默认开启SPICE导致创建虚拟机失败的问题`、`fix(service): 适配 ARM64 架构，避免修改不支持的 QXL 显卡模型` |

## 2. 属主与权限（跨发行版最容易翻车）

| 坑 | 根因与做法 | 证据 |
|---|---|---|
| `chown: invalid user: 'libvirt-qemu:kvm'` | 代码里**硬编码了 qemu 属主**，而很多发行版是 `qemu:qemu`。修法：统一走 `utils.ChownLibvirtQEMU()`（先 `libvirt-qemu:kvm`，失败回退 `qemu:qemu`），一次性替换 12 个文件的调用点 | `fix-report-2026-07-08.md` 错误 1 |
| 模板磁盘设了 `chattr +i` 不可变 → 改权限/元数据失败 | 操作前临时解除不可变标记，完事恢复；且**属主已正确时跳过 `chown`**，避免对不可变文件产生无效告警 | `fix(template): 修复meta.json不可变属性导致的权限修改失败`、`snapshot-external-restore.md` |
| AppArmor 拒绝 libvirt 访问自定义存储 | 面板要把**实际磁盘路径**写进 `virt-aa-helper` 与 `libvirt-qemu.d` 规则；已知存储根用目录级规则，其他绝对路径按实际目录生成最小范围规则；无扩展名的外部快照层也要覆盖 | `snapshot-external-restore.md` |

## 3. 网络（坑最多、最难根治）

### 3.1 OVS dnsmasq 反复被误杀（**修了三次仍未根治**）

- `2026-07-08` 首次记录：`failed to create listening socket for 192.168.122.1: Address already in use`，`Restart=on-failure` 导致频繁重启、端口释放不及时。
- `2026-07-10` 提交"**精确杀死 libvirt dnsmasq，避免误杀 OVS dnsmasq**"，同一天还有"修复 dnsmasq systemd 服务崩溃循环"。
- **2026-09-13 实测仍然复现**：`DisableLibvirtDefaultNetworkIfNeeded()` 用 `OvsGatewayIP()`（= `192.168.122.1`）去 `ss -tlnp | grep '<ip>:53'` 再 `kill`，而 OVS 的 dnsmasq 监听的正是这个地址 → **自己杀自己**；短时间多次"杀掉→拉起"撞上 systemd `DefaultStartLimitBurst=5/10s` → `start-limit-hit` → 服务 failed → 安装时的兼容性实机测试在"绑定基础 OVS 网络"阶段失败（详见 `README.md` §4.4 的已知问题）。

**给我们的结论**：任何"按 IP/端口找进程再 kill"的逻辑都必须做**归属校验**（PID 文件、systemd unit、进程名 + 参数指纹），绝不能只凭监听地址判断"这是别人的进程"。

### 3.2 端口转发与 NAT

| 坑 | 做法 | 证据 |
|---|---|---|
| 宿主机本地访问端口转发不生效 | `nat PREROUTING` **和** `nat OUTPUT` 都要写规则 | `fix(network): 增加 OUTPUT 链 DNAT 规则支持并完善端口转发错误回滚` |
| 端口转发被安全组挡掉 | 转发时要**补一条安全组放行规则**，否则转发链路失效 | `fix(network): 补充端口转发后安全组规则避免转发失效` |
| 规则写入失败后残留 | 写规则要有**回滚** | 同上 |

### 3.3 网卡与 IP

| 坑 | 做法 | 证据 |
|---|---|---|
| 静态 IP 设置后不生效 | 改 dnsmasq `dhcp-hostsfile` 后，运行中的 VM 需 **detach/attach 网卡**强制刷新 DHCP | `features.md` §12 |
| 热插拔网口后新网口 DOWN / unmanaged | 写入 `/etc/systemd/network/99-qvm-hotplug.network`（匹配 `en*` + DHCP，metric 200），主网口 netplan 规则优先级更高不受影响；Guest Agent 可用时即时生效 | `linux-hotplug-network.md` |
| 热插网口 VLAN 不一致 | 同步 `ovs-vsctl set Port <vnet> tag`，并写入持久化 XML `<vlan>` | `linux-hotplug-network.md` |
| 桥接模式 VM 拿不到 IP | 补桥接模式下的 IP 获取兜底；物理口 IP 配置要持久化 + 启动恢复 | `fix(libvirt): 补充桥接模式下VM IP获取兜底逻辑`、`feat(network): 支持网桥直接模式下物理接口IP配置的持久化与恢复` |
| OVS 网桥物理口 DHCP 管理混乱 | 单独处理物理上行口的 DHCP 归属 | `fix(network): 优化 OVS 网桥的物理接口 DHCP 管理` |

### 3.4 能力探测与外部影响

| 坑 | 做法 | 证据 |
|---|---|---|
| 端口安全依赖 OVS 细节能力 | 依赖 OpenFlow13、OpenFlow14 bundle、packet meter、`ingress_policing_kpkts_*` 字段与 OVSDB schema；**必须先预检**，能力不足就拒绝开启；bundle 不可用时降级为"先隔离、再顺序更新" | `port-security.md` |
| 公网 IPv6 前缀变化留下陈旧地址 | 前缀变化先更新绑定，再协调端口策略，**删除旧前缀源地址**防伪造 | `port-security.md` |
| **`nmap` 全网扫描阻塞启动** | 宿主防火墙初始化时的 nmap 扫描把启动卡住 | `fix(firewall): 修复启动被 nmap 全网扫描阻塞` |
| OVS 子网与宿主机已有网段冲突 | 安装时检测冲突并生成候选子网 | `install-network-subnet.md`、`install.sh` 子网冲突检测函数 |

## 4. 模板与克隆（业务复杂度最高）

| 坑 | 做法 | 证据 |
|---|---|---|
| 克隆阶段联网装依赖不可靠 | **依赖全部前置到模板预处理阶段**（cloud-init、growpart），克隆阶段只跑一次 `virt-customize --no-network` | `linux-template-offline-compat.md` |
| 历史模板没有离线依赖 | 提供"离线预处理"任务：先检查链式克隆依赖 → 有则返回 `409` 并列出关联 VM → 管理员需先把链式 VM "转为独立虚拟机" → 再解除不可变标记、检查依赖、重算 MD5/SHA256、写回状态 | 同上 |
| 模板包迁移丢状态 | 导出包携带每个节点的 `linux_init_status`；`ready` 的节点导入时直接继承，不重复跑 guestfs | 同上 |
| Netplan 固定 MAC 导致克隆后没网 | 克隆时同时改 libvirt XML 与来宾 Netplan 的主网卡 MAC；不带主网口的克隆改为匹配 `en*` 以便后续加网口 | 同上 |
| 密码/用户处理冲突 | root 锁定、目标用户创建或重命名、避免"密码被 cloud-init 覆盖"（显式 `lock_passwd: false`）、避免重复设置密码 | `fix(clone): 修复用户密码重复设置及cloud-init解锁问题` 等 4 条 |
| RPM 系缺包（如 `btrfs-progs`） | 视为**可选**：缺失只影响 btrfs 扩容，不阻断预处理 | `linux-template-offline-compat.md` |
| 导入源不做校验 | 导入磁盘链路不校验"源里是否有可识别 OS"：把 ISO 当磁盘导入会先 `convert` 成功、再在 `virt-customize` 阶段报 `no operating systems were found`（2026-09-13 实测） | 实测 + `README.md` §4.4 |

## 5. 快照

| 坑 | 做法 | 证据 |
|---|---|---|
| 外部快照不能用 `snapshot-revert` 恢复 | 读快照层 → 新建可写 overlay 作为新活动盘（不 commit，避免污染早期快照）→ 改 XML → 修权限 → 同步 `snapshot-current` | `features.md` §5、`snapshot-external-restore.md` |
| UEFI NVRAM 为 raw 时不能建含内存内部快照 | 关机时把 NVRAM 转成 qcow2 并改 XML `format='qcow2'`；运行中则提示（可自动关机转换再开机） | `features.md` §5 |
| 挂 9p/VirtFS 时禁止含内存内部快照 | 直接拦截并提示 | 同上 |
| 权限修正产生大量无效告警 | 先比较当前 UID/GID 与实际可用 qemu 账号，一致则跳过 | `snapshot-external-restore.md` |

## 6. 数据模型与库表迁移（自己挖的坑）

| 坑 | 做法 | 证据 |
|---|---|---|
| `vpc_switches.cidr` 唯一索引 + 重复数据导致迁移失败 | AutoMigrate 之前先做前置修复（删重复、删旧唯一索引），再迁移列/索引 | `fix(model): 修复 VPCSwitch CIDR 列迁移及映射问题`、`fix(db): 修复 vpc_switches.cidr 索引重复和迁移问题`、`preFixVPCSwitchCIDRIndex()` |
| 网口 `interface_order` 有间隙/重复 | 增加迁移 + 归一端点 | `migrateVPCBindingInterfaceOrder(+Normalize)` |
| 枚举与配额字段追加 | 每个字段都写了手写迁移（`cloud_type`、`address_family`、`max_port_forwards`、`max_snapshots`、`max_runtime_hours` 等） | `server/model/db.go` |

> **结论**：GORM `AutoMigrate` 处理不了"索引冲突、数据去重、枚举回填"，这些必须显式写迁移函数。这个教训我们 `k_cockpit` 直接适用。

## 7. 安全

| 坑 | 做法 | 证据 |
|---|---|---|
| Shell 命令注入 | 统一封装 shell 参数转义函数 | `feat(security): add unified shell argument escaping to prevent command injection` |
| **高风险验证死循环** | 邮箱验证码通过后写入信任时间，但旧逻辑先处理 `bootstrap_skipped` 分支 → 反复要求验证。修法：**优先检查有效信任时间**，再走兼容逻辑 | `high-risk-email-verification-loop-fix.md` |
| 超管被误改/误删 | 加强内置超管与自身操作保护 | `fix(user): 加强内置超级管理员和自身操作保护` |
| 大请求体路径匹配 | 修正大请求体的路径匹配逻辑 | `fix(server): 修正大请求体路径匹配逻辑` |
| 请求日志泄露 | 响应体与任务参数脱敏（password/token/secret → `******`） | `architecture.md` §4.1 |
| 已知 CVE | 提供 CVE-2026-53359 的自动缓解与恢复脚本 | `security/cve-2026-53359/` |

## 8. 构建、安装与部署

| 坑 | 做法 | 证据 |
|---|---|---|
| 交叉编译时 `CGO_ENABLED` 未启用 | 二进制运行报错；构建脚本必须显式 `CGO_ENABLED=1`（`mattn/go-sqlite3` 是 CGO 库） | `fix(build): 修复交叉编译时CGO_ENABLED未启用问题` |
| 兼容版/原生版切换错误 | 按 GLIBC 与 AVX2 选二进制；构建后用 `readelf` 校验 GLIBC 上限 | `fix(build): 修复兼容版构建和原生版切换逻辑`、`docs/build-compatibility.md` |
| **OVS 配置失败中断整个安装** | 把 OVS 地基包装成子函数，**任何失败只警告不中断安装** | `fix(install): OVS配置改为子函数包装，任何失败不中断安装` |
| 磁盘检测与损坏镜像 | 优化磁盘检测逻辑，增加损坏镜像处理 | `fix(install.sh): 优化磁盘检测逻辑并增加损坏镜像处理能力` |
| 用户存储目录处理 | 镜像可放在选定挂载点，但配额挂载点固定；已存在镜像从 `/etc/fstab` 或 loop 挂载自动识别复用，不重复询问、不迁移数据 | `fix(install): 修复安装脚本处理用户存储目录的问题`、`install-storage-disk-selection.md` |
| 安装脚本查找命令/版本提取 | 修正查找与版本解析 | `fix(install): 修复安装脚本中查找命令和版本提取的问题` |
| `npm ci` 锁文件不同步 | 检测到 `EUSAGE`/锁文件不同步时**回退 `npm install`**；修复 peer 依赖声明 | `fix(build): 增强前端依赖安装过程的错误处理和锁文件修复` |
| 操作系统兼容性 | 安装前强校验 root、locale（必须英文 UTF-8）、KVM 硬件标记、`/dev/kvm`；并自动探测包管理器 | `install.sh`、`install-system-compatibility-check.md` |

## 9. 前端

| 坑 | 做法 | 证据 |
|---|---|---|
| 弹窗层级混乱 | 统一加 `append-to-body`，并写了一份**弹窗关闭动画约定文档** | `refactor: 统一添加弹窗append-to-body属性并修复弹窗层级问题`、`docs/modal` |
| 上传进度卡在 0% | 改用**抽样哈希**（不做全文件哈希） | `上传改用抽样哈希并修复进度数字卡0%` |
| 磁盘扩容表单 | 校验"新容量不得小于当前值" | `fix(vmform): validate new disk size cannot be smaller than current size` |
| 运行中 VM 的 CPU/内存下限 | 修正最小值限制；CPU 核心数上限动态计算 | `fix(vmform): 修正运行中虚拟机CPU和内存最小值限制`、`fix(vm): 动态设置 CPU 核心数最大值限制` |
| 路由体积 | 抽离页面懒加载 | `refactor(router): 抽取页面懒加载至独立文件` |
| 依赖安全更新 | react-router 安全更新、axios 升级 | `react-router-security-update.md` |

---

## 10. 对 `k_cockpit` 的直接启示

> 本节结论已固化为项目规则：**编码红线**见 [`../../../AGENTS.md`](../../../AGENTS.md) 第 5 节，**排障与运行环境纪律**（含 OVS dnsmasq 误杀案例复盘）见 [`../../04-engineering/TROUBLESHOOTING.md`](../../04-engineering/TROUBLESHOOTING.md)。此处保留推导依据，便于追溯。

1. **禁止硬编码系统账号**：qemu/libvirt 属主一律"探测 + 回退"，封装成单一工具函数，禁止各处手写（它因此修了 12 个文件）。
2. **"按端口/IP 找进程再 kill"必须带归属校验**：用 PID 文件 / systemd unit / 进程名+参数指纹，绝不凭监听地址判断归属（它修了三次仍误杀）。
3. **启动自愈（restore/reconcile）要幂等且可失败**：每个 restore 步骤失败只降级告警，不阻断主服务；否则一个子系统异常会拖垮整个面板（它把 OVS 配置包成子函数就是为此）。
4. **不可变/链式依赖要先检查再动手**：`chattr +i`、链式克隆依赖、外部快照链——操作前必须先探测，并给出可执行的中文修复指引。
5. **库表变更一律显式迁移**：索引冲突、数据去重、枚举回填都写迁移函数，别指望 `AutoMigrate`。
6. **前端全局约定早点定**：弹窗层级、懒加载、请求拦截（401/428）、SSE 重连、任务栏——它为此专门写了约定文档并做过一次全量重构。
7. **把"环境问题"和"代码问题"分开**：它的兼容性实机测试脚本（真起一台 1vCPU/1GB VM 验证 libvirt + OVS + DHCP + NAT）是这次排障 3 秒定位失败阶段的根本原因，值得我们在第 1 天就做。
8. **对外部输入的语义校验要前移**：导入磁盘时不校验"源是否含可用操作系统"，导致 ISO 被当成系统盘一路走到 `virt-customize` 才报错——我们的导入功能应在入口就给明确提示。

---

## 变更记录

| 日期 | 变更内容 |
|---|---|
| 2026-09-13 | 创建文档：基于 88 条修复提交、专题修复文档与 2026-09-13 实测，按 9 个领域归纳已踩过的坑与结论 |
| 2026-09-13 | 第 10 节结论已固化为项目规则，权威来源改指 `AGENTS.md` 第 5 节与 `docs/04-engineering/TROUBLESHOOTING.md` |
