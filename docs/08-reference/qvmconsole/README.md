# QVMConsole 概览（工程结构与运行方式）

> 状态：生效
> 最后更新：2026-09-13
> 结论基于提交：`52023d6`（`reference/QVMConsole`，分支 `main`）
> 关联：[`../README.md`](../README.md) · [`architecture.md`](architecture.md) · [`features.md`](features.md)

本文记录 QVMConsole 的**工程结构层面**事实：定位、技术栈与版本、目录组织、构建/部署/运行方式、配置项与代码规模。文中路径均相对 `reference/QVMConsole/`。

**功能与实现机制**见 [`features.md`](features.md)；**分层、数据模型与 API** 见 [`architecture.md`](architecture.md)。

---

## 1. 速览

- **定位**（`README.md`）：面向小型企业 / 个人私有云的开源 **KVM/QEMU 虚拟机管理平台**，双入口（Web 控制台 + RESTful API）。
- **核心能力**：虚拟机生命周期、模板与克隆、VPC/OVS 网络、存储池、防火墙与带宽、快照、异步任务队列 + SSE。
- **形态**：单体 Go 后端 + React 前端，**单机部署**（同一台 KVM 宿主机上运行），SQLite 存储，systemd 托管。
- **规模量级**：后端约 457 个 Go 文件（约 7 万行级），前端约 320 个文件（约 3 万行级）。
- **构建特点**：前端产物以**静态目录 `web-dist/`** 由后端托管（**无 `//go:embed`**）；CGO 必需（`mattn/go-sqlite3`）。

---

## 2. 顶层目录结构

| 目录/文件 | 职责 |
|---|---|
| `server/` | Go 后端（module `kvm_console`） |
| `web/` | React + TypeScript 前端（Vite） |
| `docs/` | 约 70 篇功能/设计/排查文档 |
| `scripts/` | 系统级脚本（如 `check-system-compatibility.sh`、`port-mirror.sh`、`prepare-vfio-primary-gpu.sh`） |
| `security/` | 安全缓解工具（如 `security/cve-2026-53359/`） |
| `.github/workflows/` | CI：`build.yml`（发行包构建）、`opencode.yml`（AI 评论机器人） |
| `install.sh` | 安装/更新/卸载 + 依赖安装 + systemd 单元生成（约 90 KB） |
| `build.sh` | 本地打包（前端 + 双变体后端二进制） |
| `start-dev.sh` | 开发一键启动（air + vite） |
| `qvmc-manage.sh` | 服务器侧账户/安全管理脚本 |
| `AGENTS.md` / `DEPENDENCIES.md` / `README.md` | 开发规约、依赖说明、项目说明 |

**后端 `server/`**：`main.go`（约 1520 行，全仓库最大 Go 文件）、`compatibility_command.go`、`.air.toml`，以及 9 个子包：`config/ handler/ logger/ middleware/ model/ router/ service/ taskqueue/ utils/`。

**前端 `web/`**：`src/`、`public/`、`scripts/generate-api-endpoints.mjs`、`index.html`、`package.json`、`vite.config.ts`、`tsconfig*.json`、`.oxlintrc.json`。

**`server/service/` 子包（约 30 个）**：`appliance arch bandwidth clone compatibility diagnostics firewall guest_agent guest_automation host ip_resolver libvirt_rpc lightweight network ovs public_ip rescue scheduler security share snapshot spice storage template traffic upload user vm vm_xml vnc`；其中 `network/` 含 `vpc/ portsecurity/ portmirror/ diagnostics/ bridge/`，`vm/` 含 `vmimport/ migration/`，`storage/` 含 `quota/ pool/ disk/`。

**`web/src/` 二级目录**：`api/ components/ config/ features/ hooks/ layout/ router/ stores/ types/ utils/ views/`。

---

## 3. 技术栈与版本

> 版本以 `server/go.mod` 与 `web/package.json` 为准（`README.md` 中列的前端版本与该文件不完全一致）。

### 3.1 后端（`server/go.mod`）

- **module**：`kvm_console`；**Go**：`1.26.0`
- 直接依赖：

| 依赖 | 版本 | 用途 |
|---|---|---|
| `github.com/gin-gonic/gin` | v1.12.0 | Web 框架 |
| `gorm.io/gorm` | v1.31.2 | ORM |
| `gorm.io/driver/sqlite` + `mattn/go-sqlite3` | v1.6.0 / v1.14.50 | SQLite（**CGO**） |
| `github.com/digitalocean/go-libvirt` | v0.0.0-20260814190004 | libvirt RPC 客户端 |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | JWT |
| `golang.org/x/crypto` | v0.56.0 | bcrypt / AES 等 |
| `github.com/pquerna/otp` | v1.5.0 | TOTP |
| `github.com/gorilla/websocket` | v1.5.3 | VNC / WebSocket 代理 |
| `gopkg.in/natefinch/lumberjack.v2` | v2.2.1 | 日志轮转 |
| `golang.org/x/sys` | v0.47.0 | 系统调用 |

- **SSE**：无第三方 SSE 库，用 `gin-contrib/sse`（indirect）+ 自研内存事件广播（`server/taskqueue/queue.go`）。
- **任务队列**：无第三方库，纯 Go 内存队列（`server/taskqueue/queue.go`）。

### 3.2 前端（`web/package.json`）

| 类别 | 选型 | 版本 |
|---|---|---|
| 框架 | React + react-dom | `^19.2.8` |
| 语言 | TypeScript | `^7.0.2` |
| 构建 | Vite（+ `@vitejs/plugin-react`） | `^8.2.2` |
| 路由 | react-router（`createBrowserRouter`） | `8.3.1` |
| 状态管理 | zustand | `^5.0.15` |
| UI 库 | Semi UI（`@douyinfe/semi-ui` + icons/illustrations） | `^2.103.0` |
| 图表 | echarts | `^6.1.0` |
| HTTP | axios | `^1.20.0` |
| 远程控制台 | `@novnc/novnc`、`@xterm/xterm` + `@xterm/addon-fit` | `^1.7.0` / `^6.0.0` |
| 其他 | nprogress、qrcode、spark-md5、sass-embedded | — |
| Lint | oxlint（`web/.oxlintrc.json`） | `^1.81.0` |

---

## 4. 编译与安装

### 4.1 编译前置条件

| 项 | 要求 | 依据 |
|---|---|---|
| Go | 版本以 `server/go.mod` 为准（当前 1.26.0） | `server/go.mod`、`build.sh` 环境检查 |
| CGO + C 编译器 | **必需**，`mattn/go-sqlite3` 是 CGO 库，`CGO_ENABLED=0` 会编译成空壳并在运行时报错；需 `gcc` / `build-essential` | `start-dev.sh` `ensure_cgo()` |
| Node.js + npm | 推荐 v20+，用于前端构建 | `build.sh` |
| Zig | **仅构建兼容版需要**（用于锁定 GLIBC 目标） | `build.sh`、`docs/build-compatibility.md` |
| 交叉编译器 | CGO 交叉编译时需 `gcc-x86-64-linux-gnu` / `gcc-aarch64-linux-gnu` | `build.sh` |

### 4.2 开发运行（源码直跑）

`start-dev.sh`（一键启动脚本）：

1. `ensure_cgo()`：`go env -w CGO_ENABLED=1` **持久化**启用 CGO；未装 gcc 时在 Linux 上自动安装 `build-essential`（RPM 系为 `gcc-c++ make`）。
2. 安装 `air`（固定 `v1.61.7`）与前端依赖（`web/node_modules` 不存在时 `npm install`）。
3. 同时启动：后端 `KVM_DEVELOPMENT_MODE=true air`（配置见 `server/.air.toml`：`go build -o ./tmp/kvm-console .`）与前端 `npx vite --host 0.0.0.0`。
4. 端口：后端 8080，前端 5173（`web/vite.config.ts` 把 `/api` 代理到 `localhost:8080`）；`Ctrl+C` 时 trap 清理两个子进程。

### 4.3 打包编译（`build.sh`）

```bash
bash build.sh                          # 全部变体，版本号 dev
bash build.sh -v 1.0.0                 # 指定版本
bash build.sh --variant compat         # 仅 zig 兼容版
bash build.sh --variant native         # 仅宿主机原生版
bash build.sh --compat-glibc 2.17      # 自定义兼容版 GLIBC 上限
bash build.sh --target-arch arm64      # 交叉编译 ARM64
bash build.sh --skip-frontend|--skip-backend
```

| 参数 | 说明 |
|---|---|
| `-v/--version` | 版本号（自动去 `v` 前缀，构建时统一加 `v`；缺省 `dev`） |
| `--target-arch` | `amd64` / `arm64`，默认取宿主机架构；与宿主机不同则进入交叉编译分支 |
| `--variant` | `compat`（zig 兼容版）/ `native`（宿主机原生版），默认两个都构建 |
| `--compat-glibc` | 兼容版 GLIBC 上限，默认 amd64 `2.2.5`、arm64 `2.17` |
| `--skip-frontend` / `--skip-backend` | 跳过对应构建（跳过前端时要求 `web/dist` 已存在） |

**执行流程**：

1. 清理并重建 `release/` 目录。
2. **前端**：`npm ci`（若报锁文件与平台元数据不同步则回退 `npm install`）→ `npm run build`，产物 `web/dist`。
3. **后端（双变体）**：
   - **兼容版**：`CC="zig cc -target x86_64-linux-gnu.${GLIBC}"`（arm64 为 `aarch64-linux-gnu.*`），`CGO_CFLAGS="-O2 -mno-avx2 -mno-fma -mno-avx"`（amd64，避免新 GCC 生成 FMA3 指令在 Ivy Bridge 等旧 CPU 上 SIGILL）；先 `go clean -cache` 防止复用原生版缓存；构建后用 `readelf --version-info` 校验实际最高 GLIBC 依赖不超过上限，超限即构建失败。
   - **原生版**：`unset CC CXX` 改用系统编译器；若同时构建兼容版则输出名为 `kvm-console-native`。
   - 共同参数：`CGO_ENABLED=1 GOOS=linux GOARCH=… go build -ldflags="-s -w -X main.Version=… -X kvm_console/handler.Version=… -X kvm_console/handler.BuildTime=…"`。
4. **打包**：生成 `release/kvm-console-linux-{amd64|arm64}/` 并压缩为同名 `tar.gz`，内含 `kvm-console`（兼容版）、`kvm-console-native`（原生版）、`web-dist/`、`install.sh`、`check-system-compatibility.sh`、`bundled/`。
5. **捆绑 RPM**：从 EPEL 预取 `arp-scan`，从 AlmaLinux 8 AppStream 预取 `libguestfs-tools-c` / `libguestfs-tools`（noarch），供 Kylin / openEuler 等缺包环境兜底；下载失败仅警告。

**未发现 Makefile**；**未发现 `//go:embed`**（前端产物作为 `web-dist/` 静态目录，由 `server/router/router.go` 的静态服务 + SPA 回退托管）。

### 4.4 安装（`install.sh`）

在解压后的发行包根目录执行（**需要 root**）：

```bash
sudo ./install.sh
```

**前置强校验（任一不满足直接退出）**：

| 校验 | 要求 |
|---|---|
| `check_root` | 必须 root（或 sudo） |
| `check_os` | 存在 `/etc/os-release`，自动探测包管理器（apt / dnf / yum） |
| `check_arch` | 仅 `x86_64` / `aarch64` |
| `check_locale` | **语言环境必须是 `en_US.UTF-8`、`C.UTF-8` 或 `POSIX.UTF-8`**，否则提示用 `localectl set-locale LANG=en_US.UTF-8` 后退出（大量功能依赖英文命令输出解析） |
| `check_kvm_hardware` | x86 需 CPU 出现 `vmx`/`svm` 标记；arm64 需 `/dev/kvm` 存在 |
| `ensure_kvm_runtime` | 加载 `kvm`/`kvm_intel`/`kvm_amd` 模块并确认 `/dev/kvm` 可用 |

**模式选择**：检测到 `/opt/kvm-console/kvm-console` 或已存在 unit 文件时提供菜单——`1 更新`（默认）/ `2 卸载` / `3 修复配置文件`；未检测到则直接进入首次安装。

**安装 / 更新主流程**（`run_install_or_update()` 顺序）：

```
check_kvm_hardware → check_and_install_deps → configure_qemu_for_rpm → configure_libvirt_nonroot
→ ensure_kvm_runtime → setup_quota → configure_port → configure_public_access → get_release
→ install_files → write_env → ensure_directories → ensure_apparmor_storage_access
→ ensure_sysctl_network → setup_ovs_foundation → 首次安装兼容性测试 → 更新兼容性测试
→ setup_sshd_foundation → setup_service → start_service → show_info
```

**交互点**：
- **端口**：提示输入网页端口（默认/现有值，校验 1–65535）。
- **公网访问**：仅首次安装询问，默认关闭（关闭时所有非局域网请求返回 403）；开启后首次登录**必须完成 SMTP、邮箱与 2FA 安全初始化，且现有管理员 API Key 会被撤销**。
- **兼容性实机测试**：首次安装推荐执行、更新时默认不执行（见 §4.5）。

**依赖安装**：`APT_DEPS` 约 33 个包（`qemu-utils`、libvirt 组件、`openvswitch-switch`、`dnsmasq-base`、`virtinst`、`libguestfs-tools`、`nftables`、`ufw`、`tcpdump`、`nmap`、`arp-scan`、`dmidecode` 等）+ 架构特有包（x86：`qemu-system-x86`/`ovmf`；arm64：`qemu-system-arm`/`qemu-efi-aarch64`）；RPM 系按映射表换名（如 `libvirt-daemon-system`→`libvirt`、`virtinst`→`virt-install`）；发行包 `bundled/` 中的 RPM 作为补充源；可选补装 `polkit`、`kvm_stat`、`virt-fw-vars`（失败仅警告）。

**发行包获取顺序**（`get_release()`）：
1. 脚本同级目录已含 `kvm-console` + `web-dist`（本地发行目录）→ 直接使用；
2. 当前工作目录存在 `kvm-console-linux-<arch>.tar.gz` → 询问后解压；
3. 都没有 → 从官方下载源按架构下载（变量 `DOWNLOAD_URL_AMD64` / `DOWNLOAD_URL_ARM64`，优先 `wget`，回退 `curl -fL`）。

**文件部署与二进制自动选择**（`install_files()`）：更新时先停服务并删除旧 `web-dist`；安装目录 `mkdir -p /opt/kvm-console/data`；拷贝 `kvm-console` 为 `/opt/kvm-console/kvm-console`。若发行包含 `kvm-console-native`，读取宿主机 GLIBC 版本——**GLIBC ≥ 2.34 且 CPU 支持 `avx2`** 时把原 `kvm-console` 改名为 `kvm-console-compat`、将原生版提升为主程序；否则保留 zig 兼容版为主程序（`kvm-console-native` 留作手动切换）。兼容性测试脚本安装到 `/opt/kvm-console/scripts/`（权限 `700`）。

**关键路径**：安装目录 `/opt/kvm-console`、环境文件 `/opt/kvm-console/.env`（`chmod 600`）、数据 `/opt/kvm-console/data`、`/etc/kvm-console/ovs`、`/var/lib/kvm-console/ovs`、`/etc/kvm-portforward`、`/etc/libvirt/vm-access`、`/etc/kvm-console/firewall`、`/etc/kvm-console/vpc`、用户存储镜像 `/var/lib/kvm-user-storage.img`。

**systemd 单元**（脚本生成）：

1. `kvm-console.service`：

```ini
[Unit]
Description=QVMConsole 虚拟机管理平台
After=network-online.target libvirtd.service openvswitch-switch.service
Wants=network-online.target libvirtd.service openvswitch-switch.service

[Service]
Type=simple
WorkingDirectory=/opt/kvm-console
EnvironmentFile=/opt/kvm-console/.env
ExecStart=/opt/kvm-console/kvm-console
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
StandardOutput=journal
StandardError=journal
SyslogIdentifier=kvm-console

[Install]
WantedBy=multi-user.target
```

2. `kvm-console-ovs-dnsmasq.service`：`Type=forking`，运行 `dnsmasq --conf-file=/etc/kvm-console/ovs/dnsmasq.conf`。
3. 辅助脚本 `prepare-bridge.sh`。

`setup_service()` 后 `start_service()` 执行 `systemctl restart` 并 `is-active` 校验（失败提示 `journalctl -u kvm-console -f`）。

**完成信息**：访问地址 `http://<宿主IP>:<端口>`、安装目录、配置文件路径、**默认账号 `admin / admin123`**，以及 `systemctl status|restart`、`journalctl` 命令。

**卸载**（`uninstall_app()`）：需输入 `UNINSTALL` 确认；停止并禁用服务、删除 unit 文件；可选停用 OVS DHCP 辅助服务；可选删除整个安装目录（否则只删二进制与 `web-dist`，保留 `data/` 与 `.env`）。**明确不删除已有虚拟机磁盘、模板、libvirt 定义与用户存储镜像。**

**修复配置**（`repair_config()`）：确认后把 `.env` 重置为默认值并重启服务（会覆盖已有自定义配置）。

> **已知问题（上游缺陷，2026-09-13 实测复现）**：`EnsureOVSNetworkReady()` 会先调用 `DisableLibvirtDefaultNetworkIfNeeded()`（`server/service/ovs/network.go`），后者按 `OvsGatewayIP()` 去 `ss -tlnp | grep '<gateway>:53'` 并 `kill` 占用者；但 OVS 的 dnsmasq 监听的正是网关地址 `<subnet>.1:53`，于是**把自己杀掉**。该函数在 196 行与 234 行两处无条件执行，而 OVS dnsmasq 单元是 `Type=forking` + `PIDFile` 且 `Restart=on-failure`；短时间多次"杀掉→拉起"会撞上 systemd 默认的 `DefaultStartLimitBurst=5 / 10s`，触发 `start-limit-hit`，服务进入 failed，导致兼容性实机测试在"绑定基础 OVS 网络"阶段失败。修改 `KVM_SUBNET_PREFIX` 无法规避（该函数用的就是 OVS 自身网关 IP）。缓解办法是给该单元加 systemd drop-in：`StartLimitIntervalSec=0` + `Restart=always` + `RestartSec=2`，让被误杀后自动恢复。

### 4.5 首次安装系统兼容性实机测试

- 触发：首次安装时提示「是否运行系统兼容性测试？首次安装强烈推荐 [Y/n]」；更新时单独提示「是否使用本次更新的新版本代码重新运行系统兼容性测试？[y/N]」（默认不执行）。
- 脚本获取：先找当前工作目录的 `check-system-compatibility.sh`（校验非空、可读、`bash -n`），失败则用 `install.sh` 顶部 `COMPATIBILITY_CHECK_URL` 下载（curl 优先、wget 回退），校验后 `700` 权限原子替换。
- 实机测试内容：调用后端 CLI `kvm-console system-compatibility-check --vcpu 1 --ram-gb 1 --disk-gb 1 --report-dir /opt/kvm-console/logs/compatibility`，用与面板 ISO 创建一致的业务链路真实创建 1 vCPU / 1GB 内存 / 1GB QCOW2 空盘虚拟机（x86 q35+BIOS，arm64 virt+UEFI，VirtIO 盘与网卡），启动后校验 libvirt RPC、OVS 网桥/DHCP/NAT/转发、端口安全 meter 与流表、域状态 `running`、XML 内 `virtualport type='openvswitch'`、`vnet` 端口 `ofport` 有效，结束后清理本次唯一命名的全部临时资源。
- 结果处理：失败时打印阶段汇总（阶段状态 `passed`/`failed`/`skipped`）与报告目录，并询问「兼容性测试未通过，是否仍继续安装面板？[y/N]」——`y` 继续安装但完成页显示兼容性警告；回车则撤回本次复制的后端/前端/脚本文件（保留依赖、网络地基、配置、数据库与报告）。
- 报告：`/opt/kvm-console/logs/compatibility/`（目录 `700`、文件 `600`），含 `*-report.json`、持久化/运行态 XML、诊断日志与终端输出日志。
- 安装后可手动重跑：`sudo /opt/kvm-console/scripts/check-system-compatibility.sh [--vcpu … --ram-gb … --disk-gb … --report-dir …]`。

### 4.6 系统依赖

- **APT（Debian/Ubuntu）**：`qemu-utils`、`libvirt-daemon-system`、`libvirt-daemon-driver-qemu`、`libvirt-clients`、`openvswitch-switch`、`dnsmasq-base`、`virtinst`、`libguestfs-tools`、`ntfs-3g`、`genisoimage`、`sshpass`、`cloud-image-utils`、`lvm2`、`cloud-guest-utils`、`quota`、`e2fsprogs`、`util-linux`、`nftables`、`iproute2`、`iptables`、`tcpdump`、`ufw`、`nmap`、`arp-scan`、`conntrack`、`openssh-*`、`parted`、`dmidecode` 等。
- **架构特有**：x86 需 `qemu-system-x86` + `ovmf`；arm64 需 `qemu-system-arm` + `qemu-efi-aarch64`。
- **RPM 映射**：`libvirt`、`libvirt-client`、`openvswitch`、`dnsmasq`、`virt-install`、`qemu-kvm`（回退 `qemu`）、`edk2-ovmf`/`edk2-aarch64`、`firewalld` 等。
- **工具层**：`virsh`、`virt-install`、`qemu-img`、`ovs-vsctl`、`ovs-ofctl`、`ovsdb-client`、`tc`、`ip`、`iptables`、`ip6tables`、`dnsmasq`、`dmidecode`、`virt-fw-vars`。
- **来宾侧**（模板/初始化链路需要）：`qemu-guest-agent`、`cloud-guest-utils`、`e2fsprogs`、`xfsprogs`、`btrfs-progs`、`lvm2`、`gdisk`、`parted`。

#### 发行版支持与 RPM 系差异

`install.sh` **明确支持** RPM 系，不是"碰巧能跑"：

- `detect_pkg_manager()` 显式识别 `kylin|neokylin|openEuler|centos|rhel|anolis|rocky|alma|fedora`，并通过 `ID_LIKE` 匹配 `rhel`/`fedora`/`kylin`/`openeuler`；最终回退按 `dnf` → `yum` 探测。
- `RPM_PKG_MAP` 把约 33 个 Debian 包名映射为 RPM 名（`libvirt-daemon-system`→`libvirt`、`virtinst`→`virt-install`、`ufw`→`firewalld`、`qemu-utils`→`qemu-img` 等）；**无映射的包直接跳过**。
- `RPM_PKG_SOFT` 软性包 + `install_bundled_packages()` 三阶段（本地 `bundled/` RPM → dnf 重试 → `dnf provides` 兜底），专为 Kylin / openEuler 缺包场景设计。
- `configure_qemu_for_rpm()`：向 `/etc/libvirt/qemu.conf` 写入 `user/group = "root"` 并重启 libvirtd；`configure_libvirt_nonroot()`：非 root 运行时把用户加入 `libvirt` 组并设置 `LIBVIRT_DEFAULT_URI`。
- QEMU 包回退：`qemu-kvm` 不可用时改用 `qemu`。

**但 RPM 系是"支持但不平等"，有若干真实差异**：

| 差异 | 说明 |
|---|---|
| **SELinux 未处理** | 全项目仅来宾侧使用 `virt-customize --selinux-relabel`，宿主机侧没有任何 `setsebool` / `semanage` / 重打标签逻辑；而 AppArmor 处理（`ensure_apparmor_storage_access()`）在 RHEL 系会因 `/sys/module/apparmor` 不存在而整体跳过。RHEL 系默认 enforcing 下，libvirt 访问 `/var/lib/kvm-storage`、模板目录、外部快照层很可能被拒绝——**这是最大的坑** |
| **宿主层防火墙只支持 ufw** | `service/firewall/host.go` 全部调用 `ufw`；包映射虽把 `ufw`→`firewalld`，但程序不会调用 firewalld，宿主防火墙功能在 RPM 系实际不可用 |
| **部分包依赖 EPEL** | `libguestfs-tools`、`arp-scan`、`ntfs-3g` 等在 RHEL 系常需 EPEL；脚本不自动添加 EPEL 源，靠发行包内预取的 `bundled/` RPM 兜底 |
| **locale 要求一致** | 仍要求 `en_US.UTF-8` / `C.UTF-8` / `POSIX.UTF-8`，RHEL 最小化安装（`zh_CN.UTF-8` 或 `C`）会直接拒绝 |
| **官方推荐仍是 Debian 系** | `README.md` 写"操作系统: Debian/Ubuntu（推荐 Debian 12+）"；脚本中多处 `[ "$PKG_MGR" = "apt" ] && return 0` 也说明 RPM 路径属补充实现 |

**实操判断**：openEuler / 麒麟是脚本的重点适配对象（非 root libvirt 与 `qemu.conf` 修复合专门为它们写）；RHEL 9 / Rocky 9 / Alma 9（glibc 2.34，会切原生版二进制）相对顺利；CentOS 7 等过旧版本不建议。无论如何都要先解决 SELinux（置 permissive 或自行补策略），并接受"宿主防火墙不可用"。

### 4.7 安装落点与安装期网络访问

**安装落点：全部写入系统目录，不装到 `install.sh` 所在目录。**

| 路径 | 内容 |
|---|---|
| `/opt/kvm-console/` | 主目录：`kvm-console`（主程序）、`kvm-console-native` / `kvm-console-compat`、`web-dist/`、`.env`（600）、`data/`（SQLite 库）、`scripts/check-system-compatibility.sh`（700）、`logs/compatibility/`（700） |
| `/opt/kvm-console/firmware/` | 仅 aarch64：`AAVMF_CODE_legacy.fd` / `AAVMF_VARS_legacy.fd`（旧版 UEFI 兼容固件） |
| `/etc/systemd/system/` | `kvm-console.service`、`kvm-console-ovs-dnsmasq.service` |
| `/etc/kvm-console/{ovs,vpc,firewall}` | OVS dnsmasq 配置与运行态、VPC 配置、防火墙配置（含 `backups/`） |
| `/etc/kvm-portforward`、`/etc/libvirt/vm-access` | 端口转发持久化目录、VM 归属文件 |
| `/var/lib/kvm-user-storage.img` → 挂载 `/var/lib/kvm-user-storage` | 用户存储回环镜像（ext4 project quota）；镜像所在磁盘安装时可交互选择，默认 `/var/lib` |
| `/var/lib/libvirt/images/{templates,_imports,_exports}`、`/var/lib/libvirt/images`、`/var/lib/libvirt/images/ISO` | 模板、克隆盘、ISO 目录（chown `libvirt-qemu:kvm`，权限 775） |
| `/etc/fstab` | 追加 `<img> /var/lib/kvm-user-storage ext4 loop,prjquota 0 0` |
| `/etc/projects`、`/etc/projid` | project quota 映射 |
| `/etc/sysctl.d/99-kvm-console-network.conf` | `net.ipv4.ip_forward=1` |
| `/etc/apparmor.d/local/usr.lib.libvirt.virt-aa-helper`、`/etc/apparmor.d/abstractions/libvirt-qemu.d/kvm-console-storage` | AppArmor 存储访问块（以 `# BEGIN/END kvm_console managed storage access` 标记包裹，重复执行不叠加） |
| `/etc/ssh/sshd_config.d/` | 创建目录，供 SSH 拒绝策略使用 |
| `/usr/local/bin/*` | 仅 RPM 系：从 `bundled/` 提取的 `virt-*` 工具（`rpm2cpio`/`bsdtar` 解包后拷贝） |
| 系统组 `vmoperator` | 创建（已存在则跳过） |

**运行目录（`$PWD`，脚本内为 `INSTALL_LAUNCH_DIR`）只被用于两件事**：
1. 查找 / 解压本地发行包（`kvm-console-linux-<arch>.tar.gz` 或已解压的同级目录）；
2. 兼容性测试脚本缺失时，把它**下载到当前目录**（`check-system-compatibility.sh`，权限 700）。

**安装期会发起的网络访问**：

| 场景 | 目标 | 失败行为 |
|---|---|---|
| 本地无可用发行包 | 官方下载源 `download.xiaozhuhouses.asia`（按架构两个链接），`wget` 优先、`curl -fL` 回退 | 报错退出 |
| 首次安装选择执行兼容性测试、且当前目录无脚本 | `COMPATIBILITY_CHECK_URL`（**当前脚本已填入地址**，与 `docs/install-system-compatibility-check.md` 中"地址预留为空"的描述不一致） | 记录"测试未执行"，并询问是否继续安装 |
| aarch64 且缺兼容固件 | `http://ports.ubuntu.com/...` 或 `http://mirrors.aliyun.com/ubuntu-ports/...` 的 `qemu-efi-aarch64_2024.02-2_all.deb` | 仅警告并跳过 |
| 系统依赖 | apt / dnf / yum **系统软件源**（约 33 个包 + 架构特有包）；RPM 系另有 `dnf provides` 兜底与软性包重试 | 必需包缺失则报错退出 |
| RPM 系缺 `rpm2cpio`/`bsdtar` | 系统源安装 `rpm-build` | 静默忽略（后续提取可能失败） |

> `bundled/` 内的 RPM 是**本地文件**，不产生下载；它们由 `build.sh` 在打包阶段预先取回。

### 4.8 不使用 install.sh 的最小安装（依赖分层）

`install.sh` **不是运行必需**——它等于"发行包部署 + 系统地基准备"。后端自身启动的硬条件只有 4 条：

| 硬条件 | 依据 |
|---|---|
| 可执行文件同级目录（或工作目录）存在 `web-dist/` | `server/router/router.go` `setupStaticFileServing()`（找不到仅跳过前端托管） |
| 可写的 SQLite 路径（其父目录由程序自动 `MkdirAll`） | `server/model/db.go` `InitDB()` |
| **可连的 libvirtd** | `server/main.go`：`InitLibvirtRPC()` 失败即 `log.Fatal`，面板无法启动 |
| **非默认 `KVM_JWT_SECRET`** | `server/config/config.go` `ValidateSecurity()`：默认密钥直接 `os.Exit(1)`（开发模式仅警告） |

**功能依赖分层**（缺失只影响对应功能，不影响面板启动）：

| 功能 | 需要的系统包 / 命令 |
|---|---|
| 面板启动 + VM 电源/详情/监控 | libvirt、qemu、`virsh`（`KVM_USE_GO_LIBVIRT=false` 时的降级路径） |
| 创建 / 克隆 / 快照 / 磁盘 | `qemu-img`、`virt-install`（创建时用 `--print-xml`） |
| 模板初始化（Linux/Windows/OpenWrt/FnOS） | `virt-customize`、`guestfish`（libguestfs-tools）、`genisoimage`、`virt-fw-vars` |
| VPC 网络 / 静态 IP / 端口转发 | Open vSwitch、`dnsmasq`、`iptables`、`iproute2`、`tc` |
| 安全组 / 防火墙 | `nftables`、`ufw` |
| 抓包诊断 | `tcpdump` |
| 用户存储配额 | `quota`（project quota）+ loop 挂载 |
| 端口镜像 / ARP 扫描 | `tc` + `systemd-run`、`arp-scan`（缺失时回退 `nmap`） |
| 宿主机硬件详情 | `dmidecode` |

**最小手动安装步骤（示例）**：

```bash
# 1) 取产物：用官方 tar.gz，或自行 bash build.sh
tar -xzf kvm-console-linux-amd64.tar.gz && cd kvm-console-linux-amd64

# 2) 放到独立目录（不一定是 /opt）
install -d /srv/qvmconsole && cp kvm-console /srv/qvmconsole/ && cp -r web-dist /srv/qvmconsole/

# 3) 最小系统依赖（Debian/Ubuntu）
apt-get install -y libvirt-daemon-system libvirt-clients qemu-utils

# 4) 最小 .env
cat >/srv/qvmconsole/.env <<EOF
KVM_PORT=8080
KVM_DB_PATH=/srv/qvmconsole/data/kvm-console.db
KVM_JWT_SECRET=$(openssl rand -base64 48)
EOF
chmod 600 /srv/qvmconsole/.env

# 5) 运行（先前台验证）
cd /srv/qvmconsole && ./kvm-console
# 稳定后再自建 systemd unit（参考 §4.4 的 kvm-console.service，改 WorkingDirectory / EnvironmentFile）
```

首次启动自动建库并创建默认管理员 `admin / admin123`（应立刻改密）。全程不需要预建 `/etc/kvm-console/*`、`/var/lib/...` 等目录——程序在用到时自行创建。

**与 `install.sh` 系统改动的对照**——以下改动都只在"对应功能或标准化部署"时才必要：

| install.sh 的改动 | 真正需要它的场景 |
|---|---|
| `/etc/fstab` 追加 loop+prjquota、`/etc/projects`、`/etc/projid` | 使用用户存储配额 |
| `/etc/sysctl.d/99-kvm-console-network.conf`（`ip_forward=1`） | 使用 VPC / NAT 出网 |
| AppArmor 两处规则块 | 存储/模板目录落在 AppArmor 默认放行范围外 |
| iptables INPUT 放通 53/67 | 使用 VPC 的 dnsmasq DHCP/DNS |
| `/etc/ssh/sshd_config.d` 与 SSH 拒绝策略 | 使用"用户 SSH 访问控制" |
| `/usr/local/bin` 下提取 `virt-*` | RPM 系发行版源缺 libguestfs 工具 |
| `/opt/kvm-console`、systemd 单元、`vmoperator` 组 | 仅为标准化部署与开机自启 |

> 注意：即使不跑 `install.sh`，**程序运行期**仍会执行一批系统同步（`SyncSSHDenyConfig`、`EnsureSystemBaseNetwork`、`RestorePortForwardRules`、`EnsureAllVPCSwitchRuntime`、`RestorePortMirror`、`RestorePublicIPRules`），`main.go` 中对失败以 warn 级降级，但会实际读写系统网络与 SSH 配置。

### 4.9 关键目录的用途（配置态 / 数据态）

**`/etc/kvm-console/` —— 面板的配置与运行态（文本文件，可直接查看/修改）**

| 子路径 | 用途 |
|---|---|
| `ovs/` | 基础 OVS 网桥地基：`dnsmasq.conf`（DHCP/DNS 配置）、`dhcp-hosts`（静态 IP 绑定）、`prepare-bridge.sh`（建桥脚本）；由 `kvm-console-ovs-dnsmasq.service` 直接引用 |
| `vpc/` | 每个用户 VPC 交换机的运行态：`dnsmasq-<id>.conf` / `.pid`、`dhcp-hosts-<id>`、`leases-<id>`，以及安全组编译产物 `acl.nft` |
| `bridges/*.sh` | 面板管理的宿主网桥脚本，由 `kvm-console-bridges.service` 开机逐个执行 |
| `firewall/` | `policy.json`（策略源）+ `rules.nft`（nftables 编译产物）+ `backups/` |
| `port-mirror/` | 端口镜像配置 |
| `public-ip/` | 公网 IP 下发脚本 `rules.sh`（含清理段）+ `backups/` |
| `zram.env` / `ksm.env` | 宿主机 zRAM / KSM 调优参数，被对应 systemd 单元的 `EnvironmentFile` 读取 |
| `vnc-ports/<vm>` | 记录 VM 的 VNC 端口（删除 VM 时据此清理） |

**`/var/lib/` —— 实际数据（磁盘、租约、抓包）**

| 子路径 | 用途 |
|---|---|
| `kvm-console/ovs/dnsmasq.leases` | 基础网桥的 DHCP 租约 |
| `kvm-console/captures/` | 抓包 pcap 与摘要输出（`KVM_NETWORK_CAPTURE_DIR`） |
| `kvm-storage/<deviceID>/` | 宿主机存储池挂载根（管理员挂载硬盘后的挂载点），也是快照校验与 AppArmor 白名单的根 |
| `kvm-user-storage.img` → 挂载 `kvm-user-storage/` | 用户存储回环镜像（ext4 project quota 按 UID 限配额），在 `/etc/fstab` 中注册为 loop 挂载 |
| `libvirt/images/templates`（+ `_imports` / `_exports`） | 模板库与模板导入导出中转目录 |
| `libvirt/images`、`libvirt/images/ISO` | 克隆盘目录与全局 ISO 库 |

**其他同类落点**：

- `/etc/kvm-portforward/rules.sh`（+ `backups/`）：iptables 端口转发规则的持久化脚本，启动时由 `RestorePortForwardRules` 重放。
- `/etc/libvirt/vm-access/<username>`：每行一个 VM 名的纯文本归属文件，中间件鉴权与 polkit 规则都读它。
- `/etc/modprobe.d/kvm-intel-unrestricted-guest.conf`、`/etc/ssh/sshd_config.d/kvm-console-deny.conf`、`/etc/systemd/system/kvm-console-{bridges,zram,ksm,public-ipv4,public-ipv6}.service`、`/etc/fstab`、`/etc/projects`、`/etc/projid`。

**设计含义**：这些目录就是面板的"系统侧状态库"。因为它是**单机直管**架构，OVS / dnsmasq / iptables / 网桥的配置必须落盘，进程重启后才能靠 `Restore*` / `Reconcile*` 自愈（见 §4.8 末尾的提醒）。

### 4.10 CI

- `.github/workflows/build.yml`：**仅 `workflow_dispatch` 手动触发**，参数含版本号、平台、变体、产物存储（GitHub 或阿里云 OSS）、是否创建 Release；步骤为 Node 20 + Go（读 `server/go.mod`）+ zig 0.14.0 + `bash build.sh`；jobs：`build`、`build-arm64`、`release`。
- **CI 不运行任何测试或 lint**。

---

## 5. 配置项与环境变量

- **加载顺序**：环境变量 > `.env` 文件（`KVM_ENV_FILE` 指定路径，默认 `./.env`）；`config.Init()` 之后由数据库 `system_settings` 表覆盖（`LoadFromDB`）。
- **写入顺序**：可持久化键（`PersistableKeys` 白名单，约 90 项）保存时同步写回 `.env`，保证重启一致。
- 校验：`ValidateSecurity()` 在非开发模式下遇到默认 `KVM_JWT_SECRET` 会**拒绝启动**。

### 5.1 主要变量（默认值）

| 分组 | 变量 | 默认值 |
|---|---|---|
| 服务 | `KVM_PORT` | `8080` |
| 数据库 | `KVM_DB_PATH` | `./data/kvm_console.db` |
| 密钥 | `KVM_JWT_SECRET` / `KVM_VM_CREDENTIAL_SECRET` / `KVM_SECURITY_SECRET` | 前者默认值会被拒启动；后两者为空时自动生成并写回 `.env` |
| 会话 | `KVM_JWT_EXPIRE_HOURS` / `KVM_JWT_SECRET_ROTATE_HOURS` | `24` / `24`（0=禁用轮换） |
| 模板/磁盘 | `KVM_TEMPLATE_DIR` / `KVM_CLONE_DIR` / `KVM_ISO_DIR` | `/var/lib/libvirt/images/templates` / `/var/lib/libvirt/images` / `/var/lib/libvirt/images/ISO` |
| 网络 | `KVM_NETWORK_BACKEND` / `KVM_OVS_BRIDGE` / `KVM_OVS_UPLINK` / `KVM_OVS_DHCP_START|END` / `KVM_SUBNET_PREFIX` | `ovs` / `br-ovs` / 空（自动检测） / 空 / `192.168.122` |
| 网络 | `KVM_DEFAULT_NETWORK` / `KVM_EXTERNAL_NIC` / `KVM_HOST_IP` | `default` / 空（自动检测） / 空（自动检测） |
| 端口转发 | `KVM_PORTFORWARD_DIR` / `KVM_AUTO_PORT_START` / `KVM_AUTO_PORT_END` | `/etc/kvm-portforward` / `10000` / `20000` |
| 权限 | `KVM_VM_ACCESS_DIR` | `/etc/libvirt/vm-access` |
| 管理员 | `KVM_ADMIN_USER` / `KVM_ADMIN_PASS` | `admin` / `admin123`（首次建库创建） |
| 主程序 | `KVM_USE_GO_LIBVIRT` | `true`（关闭则降级 `virsh`） |
| 主程序 | `KVM_DEVELOPMENT_MODE` / `KVM_PUBLIC_ACCESS_ENABLED` | `false` / `false` |
| 主程序 | `KVM_SERVICE_UNIT_NAME` | `kvm-console.service` |
| 维护模式 | `KVM_MAINTENANCE_MODE` / `KVM_MAINTENANCE_SERVICE_UNITS` / `KVM_MAINTENANCE_VM_SHUTDOWN_TIMEOUT_SECONDS` | `false` / 5 个 libvirt 相关 unit / `40` |
| 带宽 | `KVM_MAX_BURST_INBOUND` / `KVM_MAX_BURST_OUTBOUND` | `0`（不限） |
| 邮件 | `KVM_SMTP_HOST` / `PORT` / `USERNAME` / `PASSWORD_ENC` / `FROM_NAME` / `FROM_ADDRESS` / `SECURITY` / `TIMEOUT_SECONDS` | 空 / `587` / 空 / 空 / `QVMConsole` / 空 / `starttls` / `15` |
| 站点 | `KVM_PUBLIC_BASE_URL` / `KVM_SITE_TITLE` | 空 / `QVMConsole` |
| 任务/调度 | `KVM_SCHEDULER_EVENT_RETENTION_HOURS` / `KVM_BATCH_CLONE_MAX_CONCURRENCY` | `168` / `10` |
| VPC | `KVM_VPC_SUBNET_PREFIX` / `VLAN_START` / `VLAN_END` / `DNS` / `ACL_TABLE` | `10.200` / `100` / `4094` / `223.5.5.5,223.6.6.6` / `kvm_console_vpc_acl` |
| 端口安全 | `KVM_PORT_SECURITY_ENABLED` + 6 个阈值 + `RECONCILE_INTERVAL_SECONDS` | `false` / 50 / 40 / 200 / 400 / 1000 / 2000 / `60` |
| 抓包 | `KVM_NETWORK_CAPTURE_DIR` / `DEFAULT_SECONDS` / `MAX_SECONDS` / `MAX_MB` / `MAX_PACKETS` | `/var/lib/kvm-console/captures` / `30` / `120` / `64` / `5000` |
| 限流 | `KVM_RATE_LIMIT_PUBLIC` / `KVM_RATE_LIMIT_AUTH` | `20` / `0`（0=不限） |
| 请求 | `KVM_REQUEST_FILTER_ENABLED` / `KVM_API_MAX_BODY_SIZE_MB` / `KVM_ERROR_DETAIL_IN_RESPONSE` / `KVM_REQUEST_DETAIL_LOG_ENABLED` / `KVM_REQUEST_LOG_MAX_BODY_BYTES` | `true` / `2` / `false` / `true` / `8192` |
| 日志 | `KVM_LOG_DIR` / `LEVEL` / `MAX_DAYS` / `COMPRESS` / `CONSOLE` / `CONSOLE_TYPES` / `MAX_SIZE_MB` | `./log` / `info` / `7` / `true` / `true` / `app,cmd,libvirt` / `100` |
| 安全开关 | `KVM_SESSION_FINGERPRINT_ENABLED` / `KVM_PASSWORD_BREACH_CHECK_ENABLED` / `KVM_SCHEDULED_PASSWORD_BREACH_CHECK_ENABLED` / `KVM_SCHEDULED_STORAGE_TRIM_ENABLED` | `true` ×4 |
| 其他 | `KVM_RESCUE_ISO` / `KVM_SPICE_ENABLED_BY_DEFAULT` / `KVM_HARDWARE_PASSTHROUGH_ENABLED` / `KVM_SECURITY_GROUP_DEFAULT_ALLOW_ALL` | 空 / `false` / `false` / `false` |
| 其他 | `KVM_CORS_ALLOWED_ORIGINS` / `KVM_TRUSTED_PROXIES` | 空 / 空 |
| 其他 | `KVM_TMPDIR` / `KVM_ENV_FILE` / `KVM_PUBLIC_IPV6_SYNC_INTERVAL_SECONDS` / `KVM_NETWORK_WAIT_ONLINE_DISABLED` | 未设置时自动选择 / `./.env` / `60` / `false` |

> 更细的键名与读取位置见 `server/config/config.go`。

---

## 6. 代码规模

| 范围 | 规模 |
|---|---|
| 后端 | 约 457 个 `.go` 文件，约 7 万行级；`service/` 358 文件（占约 78%），`handler/` 49、`model/` 26、`middleware/` 11、`utils/` 6、`logger/` 2，`main.go` 约 1520 行 |
| 前端 | 约 320 个文件，约 3 万行级；`views/` 235（占约 63%）、`features/vm-form/` 53、`api/` 21 |
| 体量最大的模块 | `server/service/`、`web/src/views/`、`web/src/features/vm-form/`、`server/handler/` |

**未发现**：Makefile、`*_test.go`、`*.sql`、`//go:embed`、Swagger/OpenAPI 文件、golangci-lint 配置。

---

## 7. 测试与工程实践

- **无测试代码**：全仓库未发现后端 `*_test.go` 与前端测试文件；`AGENTS.md` 明确"本项目没有测试代码，不需要写测试代码"。
- **后端 lint**：无 golangci-lint / staticcheck 配置。
- **前端 lint**：`oxlint`（`web/.oxlintrc.json`，`react/rules-of-hooks: error`）；TypeScript 严格选项（`noUnusedLocals`、`noUnusedParameters`、`noFallthroughCasesInSwitch`、`verbatimModuleSyntax`、target `es2023`）。
- **代码规约**：根 `AGENTS.md` 约 34 条，含"耗时操作全部使用任务队列""不使用数据库同步 VM 状态""敏感操作二次验证""新接口必须兼容 API Key"等。
- **API 文档自动化**：`web/scripts/generate-api-endpoints.mjs` 在 `predev` / `prebuild` 钩子解析 `server/router/router.go` 与 `server/handler/*.go`（含高风险验证标记），生成 `web/src/views/api-docs/generated/endpoints.json`（当前 342 条端点）。

---

## 8. 变更记录

| 日期 | 变更内容 |
|---|---|
| 2026-09-13 | 创建文档：基于提交 `52023d6` 整理工程结构、技术栈、构建部署、配置与规模 |
| 2026-09-13 | 细化"编译与安装"：补全部编译前置条件与手动命令、`build.sh` 双变体细节、`install.sh` 前置校验/模式选择/主流程/交互点/发行包获取与二进制自动选择、systemd 单元全文、首次安装兼容性实机测试 |
| 2026-09-13 | 补充"安装落点与安装期网络访问"：逐项列出系统目录写入点与 5 类安装期下载场景 |
| 2026-09-13 | 补充"不使用 install.sh 的最小安装"：区分 4 条启动硬条件与按功能分层的系统依赖，并对照 install.sh 的系统改动说明何时才真正需要 |
| 2026-09-13 | 补充"关键目录的用途"：逐条说明 `/etc/kvm-console/*`、`/var/lib/*` 及其他同类落点分别存什么、由谁使用 |
| 2026-09-13 | 补充"发行版支持与 RPM 系差异"：记录 RPM 系适配机制（包名映射、软性包、bundled RPM、qemu.conf 修复）与 SELinux/ufw/EPEL 等实际差异 |
| 2026-09-13 | 记录"已知问题"：`EnsureOVSNetworkReady()` 按 OVS 网关 IP 误杀自身 dnsmasq，多次重启触发 systemd `start-limit-hit`，导致兼容性实机测试在 OVS 绑定阶段失败；含缓解方案（systemd drop-in） |
| 2026-09-13 | 新增配套文档 [`pitfalls.md`](pitfalls.md)：汇总 88 条修复提交与专题修复文档得出的踩坑清单 |
