# QVMConsole 详细功能清单（模块 · 接口 · 任务 · 高风险操作）

> 状态：生效
> 最后更新：2026-09-14
> 结论基于提交：`52023d6`（`reference/QVMConsole`，分支 `main`）
> 关联：[`../README.md`](../README.md) · [`README.md`](README.md)（工程概览）· [`architecture.md`](architecture.md)（工程架构）· [`capabilities.md`](capabilities.md)（功能清单）· [`features.md`](features.md)（实现机制）· [`pitfalls.md`](pitfalls.md)（踩坑清单）

本文把 QVMConsole 的功能**按模块逐项展开**到"接口 / 任务 / 高风险操作"三个可核对的维度，用于实现同类能力时对照"这个模块到底要做哪些事"。

**与 [`capabilities.md`](capabilities.md) 的分工**：`capabilities.md` 从**用户可见能力**（页面、操作、字段、约束）描述；本文从**系统交付面**（HTTP 接口、异步任务、需二次验证的操作）描述。同一功能在两处的描述角度不同，不重复。

---

## 0. 数据来源与统计口径

| 维度 | 数量 | 来源 |
|---|---|---|
| HTTP 接口 | **342** | `web/src/views/api-docs/generated/endpoints.json`（由 `generate-api-endpoints.mjs` 解析 `router.go` 与 handler 自动生成，生成时间 2026-09-04） |
| 异步任务类型 | **54** | `server/model/task.go` 的 `TaskType*` 常量 |
| 高风险操作（需交互式二次验证） | **82** | `endpoints.json` 的 `highRisk` 字段（值为操作标识符） |
| 前端任务类型中文文案 | **27 / 54** | `web/src/stores/task.ts` `TASK_TYPE_TEXT`（**缺 27 个，界面回退显示原始 key**） |

**接口标记说明**（本文中缀在路径后的方括号）：

| 标记 | 含义 | 来源字段 |
|---|---|---|
| `A` | 需要管理员 | `admin` |
| `E` | 弹性云专属（轻量云不可用） | `elasticOnly` |
| `V` | 需校验 VM 归属（`VMAccessMiddleware`） | `vmAccess` |
| `!` | **高风险**：需交互式二次验证（428 → 验证 → 重试原请求） | `highRisk` |

> 注意：`!` 标记对应"JWT 会话下需要二次验证"；**API Key 调用不触发交互式验证**（在 handler 里显式放行），这是它在设计上的取舍。

---

## 1. 模块功能矩阵

### 1.1 `/vm` —— 虚拟机（92 个接口，全系统最大模块）

**列表与实时**：`GET /vm/list`、`GET /vm/sse`〔V〕、`GET /vm/os-variants`

**创建与导入**：
`POST /vm/create`〔EV!〕、`POST /vm/clone`〔EV〕、`POST /vm/batch-clone`〔EV〕、`POST /vm/import-disk`〔AEV!〕、`POST /vm/import-appliance`〔AEV!〕、`POST /vm/import-appliance/inspect`〔AEV〕

**电源与生命周期**：`POST /vm/:name/operate`〔V〕（start/shutdown/reboot/destroy/reset）、`POST /vm/:name/force-delete`〔AEV!〕、`DELETE /vm/:name`〔EV!〕

**详情与监控**：`GET /vm/:name`〔V〕、`GET /vm/:name/stats`〔V〕、`GET /vm/:name/stats/history`〔V〕、`GET /vm/:name/ip`〔V〕、`GET /vm/:name/pcie-info`〔V〕、`GET /vm/:name/qcow2-disks`〔V〕、`GET /vm/:name/sse`〔V〕

**编辑与配置**：`PUT /vm/:name`〔EV〕（备注/分组/标签等）、`GET|PUT /vm/:name/xml`〔AEV〕（`PUT` 为高风险）、`GET|POST|DELETE /vm/:name/passthrough`〔AEV〕

**磁盘与设备**：`GET /vm/:name/disks`〔V〕、`POST /vm/:name/disk`〔EV〕、`POST /vm/:name/disk/attach`〔EV〕、`POST /vm/:name/disk/import`〔AEV〕、`POST /vm/:name/disk/:dev/resize`〔EV〕、`DELETE /vm/:name/disk/:dev`〔EV!〕、`PUT /vm/:name/disk/:dev/bus`〔EV〕、`GET|PUT /vm/:name/disk/:dev/iops`、`GET /vm/:name/disk/:dev/guest-status`〔V〕、`POST /vm/:name/disk/:dev/guest-grow`〔EV〕、`POST /vm/:name/disk/:dev/guest-mount`〔EV〕、`GET /vm/:name/disk-migration/options`〔AV〕、`POST /vm/:name/disk/:dev/migrate`〔AV!〕、光驱/软盘：`POST|DELETE /vm/:name/cdrom`、`PUT /vm/:name/cdrom/:dev/bus`、`POST /vm/:name/cdrom/eject`、`POST|DELETE /vm/:name/floppy`、`POST /vm/:name/floppy/eject`

**网络**：`GET|PUT /vm/:name/vpc`〔V〕、`PUT /vm/:name/security-group`〔V〕、`GET|POST /vm/:name/interfaces`〔V〕、`PUT|DELETE /vm/:name/interfaces/:order`〔V〕、`GET /vm/:name/network/status`〔V〕、`GET /vm/:name/network/diagnostics`〔AV〕、`POST /vm/:name/network/capture`〔AV!〕

**共享目录**：`GET /vm/:name/shares`〔EV〕、`POST /vm/:name/share`〔EV〕、`DELETE /vm/:name/share/:tag`〔EV〕

**快照**：`GET|POST /vm/:name/snapshots`、`POST /vm/:name/snapshot/:snap/revert`〔V〕、`DELETE /vm/:name/snapshot/:snap`〔V!〕、`DELETE /vm/:name/snapshots`〔V!〕

**控制台**：
VNC：`GET /vm/:name/vnc/status`〔V〕、`POST /vm/:name/vnc/enable|disable`〔V〕、`POST /vm/:name/vnc/passwd`〔V〕、`POST /vm/:name/vnc/expose`〔V〕、`GET /vm/:name/vnc/ws`〔V〕（WebSocket 代理）
SPICE：`GET /vm/:name/spice/status|info`〔V〕、`POST /vm/:name/spice/enable|disable|expose|passwd`〔均高风险〕、`GET /vm/:name/spice/vv`〔V〕

**运维操作**：`POST /vm/:name/password/reset`〔V!〕、`POST /vm/:name/reinstall`〔EV!〕、`POST /vm/:name/rescue`〔V〕、`POST /vm/:name/lock`〔EV〕、`GET /vm/:name/lock`〔V〕、`POST /vm/:name/unlock`〔EV!〕、`POST /vm/:name/make-independent`〔AEV!〕

**迁移**：`GET /nodes/:id/migration-options`〔A〕（见 §1.14）、`POST /vm/:name/migration/preview`〔AV〕、`POST /vm/:name/migrate`〔AV!〕、`POST /migration/adopt-vm`〔A〕（目标节点接管）

**定时任务**：`GET|POST /vm/:name/schedules`〔EV，POST 高风险〕、`PUT|DELETE /vm/:name/schedules/:id`〔EV，PUT 高风险〕

**关联任务类型**：`create`、`clone`、`linked_clone`、`batch`、`delete`、`snapshot`、`reinstall`、`rescue`、`reset_vm_password`、`vm_disk_resize`、`vm_disk_provision`、`vm_disk_guest_mount`、`disk_transfer`、`vm_disk_migrate`、`vm_migrate`、`import`、`import_disk`、`import_disk_attach`、`import_appliance`、`export`、`make_vm_independent`、`network_capture`、`vm_schedule_action`、`power`

### 1.2 `/network` —— 网络与端口转发（39）

`GET /network/bridges`、`POST|DELETE /network/bridges`〔A!〕、`GET /network/host/interfaces`〔A〕、`GET|PUT /network/interfaces/:name/config`〔A，PUT 高风险〕、`GET /network/client-ip`（供"使用当前访问 IP"）

端口转发：`GET /network/port-forward/list`、`POST /network/port-forward/add`、`PUT|DELETE /network/port-forward/:id`、`POST /network/port-forward/batch-delete`〔!〕、`POST /network/port-forward/save`、`GET|POST /network/port-forward/ip-mapping`〔E〕、`DELETE /network/port-forward/ip-mapping/:id`〔E!〕

静态 IP：`GET /network/static-ip/list`、`POST /network/static-ip/bind`、`POST /network/static-ip/unbind`〔E〕

公网 IP：`GET|POST /network/public-ips`〔A〕、`PUT|DELETE /network/public-ips/:id`〔A，DELETE 高风险〕、`POST /network/public-ips/:id/preview|bind|unbind|migrate`〔A，除 preview 外均高风险〕、`POST|DELETE /network/public-ips/batch` 与 `batch/bind|batch/unbind`〔A!〕、`POST /network/public-ips/apply`〔A!〕、`GET /network/public-ips/ipv6-prefixes` 与 `POST .../import`〔A〕

抓包：`GET /network/captures/:task_id`、`.../download`、`DELETE /network/captures/:task_id`〔A〕
宿主机防火墙快捷：`GET /network/ufw/status`、`POST /network/ufw/rule`〔A〕

**关联任务**：`public_ip_apply`、`network_capture`、`port_mirror`

### 1.3 `/vpc` —— VPC 交换机与安全组（17）

交换机：`GET /vpc/switches`、`POST /vpc/switches`〔E!〕、`PUT|DELETE /vpc/switches/:id`〔E，DELETE 高风险〕、`POST /vpc/switches/:id/reconfigure`〔E!〕、`POST /vpc/switches/:id/traffic/reset`〔E〕、`GET /vpc/switches/:id/vms`、`GET /vpc/quota`〔E〕

安全组：`GET /vpc/security-groups`、`POST /vpc/security-groups`〔E〕、`PUT|DELETE /vpc/security-groups/:id`〔E〕、`POST /vpc/security-groups/:id/rules`、`PUT|DELETE /vpc/security-groups/rules/:id`

ACL：`GET /vpc/acl/preview`、`POST /vpc/acl/apply`〔!〕

**关联任务**：`vpc_switch_reconfigure`

### 1.4 `/ovs` —— OVS 基础设施与端口安全（16）

`GET /ovs/status`〔A〕、`POST /ovs/repair`〔A!〕、`GET /ovs/ports`〔A〕、`GET /ovs/leases`〔A〕

端口安全：`POST /ovs/port-security/preflight|enable|disable|reconcile`〔A〕、`GET /ovs/port-security/status`〔A〕、`POST /ovs/port-security/ports/:port/isolate|release`〔A〕

端口镜像：`GET /ovs/port-mirror/status|options`〔A〕、`POST /ovs/port-mirror/enable|disable`〔A!〕

**关联任务**：`ovs_repair`、`port_security`、`port_mirror`

### 1.5 `/firewall` —— 双层防火墙（21）

KVM 网络防火墙：`GET /firewall/status`〔A〕、`GET|PUT /firewall/policy`〔A〕、`POST /firewall/preview`〔A〕、`POST /firewall/apply|disable|rollback`〔A!〕、`PUT /firewall/port-forward`、GeoIP：`POST /firewall/geoip/import|update`〔A〕

宿主机防火墙（UFW）：`GET /firewall/host/status`、`POST /firewall/host/enable|disable`〔A!〕、`POST /firewall/host/enable/preview`〔A〕、`GET|POST /firewall/host/rules`〔A，POST 高风险〕、`PUT|DELETE /firewall/host/rules/:id`〔A!〕、`POST /firewall/host/rules/vnc-default`〔A!〕

连接管理：`GET /firewall/host/connections/preview`〔A〕、`POST /firewall/host/connections/close`〔A!〕

**关联任务**：`apply_firewall`、`disable_firewall`、`rollback_firewall`、`update_firewall_geoip`、`enable_host_firewall`、`disable_host_firewall`

### 1.6 `/template` —— 模板（19）

`GET /template/list`〔E〕、`POST /template/prepare`〔E!〕（制作模板）、`PUT /template/:name/meta|publish`〔E〕、`GET /template/:name/vms`〔E〕、`GET /template/:name/delete-preview`〔E〕、`DELETE /template/:name`〔E!〕、`GET /template/:name/prepare-linux/check` 与 `POST /template/:name/prepare-linux`〔AE〕、导出：`POST /template/:name/export`〔E〕、`DELETE /template/:name/export`〔E〕、`GET /template/download/:filename`〔E〕、导入与上传：`POST /template/import/preview|import|import/confirm`〔E〕、`POST /template/upload/init|chunk|complete`〔E〕、`DELETE /template/upload`〔E〕

**关联任务**：`prepare`、`delete_template`、`template_export`、`template_import`、`template_linux_prepare`

### 1.7 `/storage-pool` + `/self/storage` —— 存储

宿主机存储池（12）：`GET /storage-pool/list`〔AE〕、`GET /storage-pool/:id`、`PUT /storage-pool/:id/config`〔AE〕、`POST /storage-pool/:id/default`〔AE〕、`POST /storage-pool/:id/format-mount|create-partition|delete-partitions`〔AE!〕、`POST /storage-pool/create-volume|delete-volume`〔AE!〕、`GET /storage-pool/pv-targets`〔AE〕、`GET /storage-pool/vm-targets|all-isos`〔E〕

我的存储（9，均〔E〕）：`GET /self/storage/info`、`POST /self/storage/init`、`GET /self/storage/files/:category`、`GET /self/storage/isos`、`GET /self/storage/download/:category/:filename`、`DELETE /self/storage/file/:category/:filename`〔!〕、分片上传 `POST /self/storage/upload/init|chunk|complete`、`GET /self/storage/upload/pending|status`、`DELETE /self/storage/upload`、挂载 `GET /self/storage/mounts`、`POST /self/storage/mount`、`DELETE /self/storage/mount/:vmName/:tag`

**关联任务**：`storage_format`、`storage_create_partition`、`storage_delete_partitions`、`storage_create_lvm_volume`、`storage_delete_lvm_volume`、`storage_trim`

### 1.8 `/host` —— 宿主机（19）

`GET /host/stats`、`GET /host/stats/history`、`GET /host/stats/sse`、`GET /host/disks`、`GET /host/cpus`、`GET /host/cpu/hardware`〔A〕、`GET /host/memory/modules`〔A〕

KSM/zRAM/嵌套：`GET|PUT /host/ksm`〔A，PUT 高风险〕、`GET|PUT /host/zram`〔A，PUT 高风险〕、`GET|PUT /host/kvm-intel-unrestricted-guest`〔A，PUT 高风险〕

硬件直通：`GET /host/passthrough`、`POST /host/passthrough/bind|unbind`〔A〕、`GET /host/hardware-passthrough/status`〔A〕、`POST /host/hardware-passthrough/enable-iommu|load-vfio`〔A!〕

### 1.9 `/user` —— 用户与配额（16，均管理员）

`GET /user/list`〔A〕、`POST /user`〔A!〕、`DELETE /user/:username`〔A!〕、`PUT /user/:username/account|quota|ssh|vms`〔A〕、`PUT /user/:username/status`〔A!〕（封禁/解封）、`POST /user/:username/resend-invite`〔A〕、`POST /user/:username/traffic/reset`〔A〕、轻量云：`POST|DELETE /user/:username/lightweight-registrations`〔A〕、`PUT /user/:username/lightweight-vm-quota`〔A〕、`DELETE /user/:username/lightweight-vm/:vmName`〔A〕、`POST /user/:username/lightweight-vm/:vmName/delete`〔A!〕

**关联任务**：`deleteuser`、`disable_user`、`lightweight_vm_provision`、`runtime_quota_shutdown`、`lightweight_runtime_quota_shutdown`

### 1.10 `/auth` —— 认证与账户安全（27）

登录链路：`POST /auth/login`、`POST /auth/login/email/send`、`POST /auth/login/verify`、`POST /auth/logout`、`POST /auth/session/activity`

安全初始化：`POST /auth/skip-bootstrap`、`POST /auth/email/code/send`、`POST /auth/email/bind`、`POST /auth/2fa/setup|enable|disable|recovery/regen`

账户：`GET /auth/info`、`PUT /auth/password`〔!〕、`PUT /auth/username`〔!〕、`POST /auth/check-password`、API 凭证：`GET|POST /auth/api-key`〔POST 高风险〕、`DELETE /auth/api-key`〔!〕

找回密码：`POST /auth/password/forgot`、`.../send-code`、`.../verify-code`、`.../select-account`、`POST /auth/password/reset`

高风险验证：`POST /auth/high-risk/verify`
邀请：`GET /auth/invite`、`POST /auth/invite/complete`

### 1.11 `/self` —— 用户自助（29）

配额与列表：`GET /self/quota`、`GET /self/vms`、`GET /self/vms/sse`

自助创建：`POST /self/vm/create`〔E!〕、`POST /self/vm/clone`〔E〕、`POST /self/vm/import`〔E〕、`POST /self/vm/import-appliance`〔E!〕、`.../inspect`〔E〕、`DELETE /self/vm/:name`〔E!〕、导出：`GET /self/vm/:name/export-options`、`POST /self/vm/export`〔E〕、`GET /self/vm/:name/qcow2-disks`

轻量云：`GET /self/lightweight-registrations`、`POST /self/lightweight-registrations/:id/confirm`〔!〕
我的存储：见 §1.7

### 1.12 `/settings` —— 系统设置与诊断（14，均管理员）

`GET|PUT /settings`、`PUT /settings/public-access`、`PUT /settings/cpu-affinity-presets`、`POST /settings/smtp/test`、`POST /settings/storage/trim`、`GET /settings/user-storage-iso-path`、`POST /settings/jwt-secret/rotate`〔!〕

日志：`GET /settings/log/status|read`、`POST /settings/log/export|log/delete`
诊断导出：`GET /settings/diagnostics/categories`、`POST /settings/diagnostics/export`

### 1.13 `/task` 与 `/scheduler` —— 任务与调度

`GET /task/list`、`GET /task/:id`、`POST /task/:id/cancel`、`DELETE /task/clear`〔!〕、`GET /task/sse`
`GET /scheduler/list`〔A〕、`GET /scheduler/events`〔A〕、`GET /scheduler/events/sse`〔A〕

### 1.14 `/nodes` —— 迁移目标节点（6，均管理员）

`GET /nodes`、`POST /nodes`、`PUT|DELETE /nodes/:id`、`POST /nodes/:id/probe`、`GET /nodes/:id/migration-options`

### 1.15 `/security`、`/public` 与零散接口

`POST /security/password-breach/scan`〔A!〕、`GET /security/password-breach/status`〔A〕
`GET /public/settings`、`GET /public/version`（免登录）
`GET /system-info`、`GET /cpu-affinity-presets`、`POST /migration/adopt-vm`〔A〕

---

## 2. 异步任务类型全表（54）

> 所有重操作都入任务队列，前端"任务中心"可按类型筛选。**标注 `✗文案` 的 27 个类型没有中文文案**，界面会直接显示原始 key（`taskTypeText` 的 fallback），这是可复用实现里应避免的细节。

| 域 | 任务类型 | 前端文案 |
|---|---|---|
| **虚拟机生命周期** | `create` | 普通创建 |
| | `clone` | 链式克隆 |
| | `linked_clone` | ✗文案（原始 key） |
| | `batch` | 批量克隆 |
| | `delete` | 删除虚拟机 |
| | `power` | 电源操作 |
| | `reinstall` | 重装系统 |
| | `rescue` | ✗文案 |
| | `reset_vm_password` | ✗文案 |
| | `export` | 导出虚拟机 |
| | `import` | 导入虚拟机 |
| | `import_appliance` | 导入虚拟机包 |
| | `import_disk` | ✗文案 |
| | `import_disk_attach` | ✗文案 |
| | `make_vm_independent` | ✗文案 |
| **磁盘与存储** | `vm_disk_resize` / `vm_disk_provision` / `vm_disk_guest_mount` | ✗文案 |
| | `disk_transfer` / `vm_disk_migrate` | ✗文案 |
| | `storage_format` | 格式化存储 |
| | `storage_create_partition` | 创建分区 |
| | `storage_delete_partitions` | 删除分区 |
| | `storage_create_lvm_volume` / `storage_delete_lvm_volume` | ✗文案 |
| | `storage_trim` | ✗文案 |
| **快照** | `snapshot` | 快照操作 |
| **模板** | `prepare` | 制作模板 |
| | `delete_template` | 删除模板 |
| | `template_export` / `template_import` | 导出模板 / 导入模板 |
| | `template_linux_prepare` | Linux 模板预处理 |
| **网络** | `ovs_repair` | OVS 修复 |
| | `port_security` | ✗文案 |
| | `port_mirror` | 端口镜像 |
| | `network_capture` | 网络抓包 |
| | `vpc_switch_reconfigure` | ✗文案 |
| | `public_ip_apply` | ✗文案 |
| **防火墙** | `apply_firewall` / `disable_firewall` / `rollback_firewall` | ✗文案 |
| | `update_firewall_geoip` | ✗文案 |
| | `enable_host_firewall` / `disable_host_firewall` | ✗文案 |
| **迁移** | `vm_migrate` / `vm_disk_migrate` | 迁移虚拟机 / 迁移硬盘 |
| **用户与配额** | `deleteuser` / `disable_user` | ✗文案 |
| | `runtime_quota_shutdown` / `lightweight_runtime_quota_shutdown` | ✗文案 / 轻量云时长关机 |
| | `lightweight_vm_provision` | 轻量云开通 |
| **维护** | `enter_maintenance_mode` / `exit_maintenance_mode` | ✗文案 |
| **调度** | `vm_schedule_action` | 虚拟机定时任务 |
| **安全** | `password_breach_scan` / `password_breach_notify` | 泄露密码扫描 / 泄露密码通知 |

---

## 3. 高风险操作全表（82，需交互式二次验证）

按模块分组，列出 `operation` 标识（`X-High-Risk-Token` 绑定的操作名）。

**认证与账户（4）**：`rotate_api_key`、`revoke_api_key`、`change_password`、`change_username`

**用户与配额（6）**：`create_user`、`delete_user`、`update_user_account`、`change_user_status`、`delete_vm`（轻量云 VM）、`create_vm`（轻量云开通）

**虚拟机创建/删除（7）**：`create_vm`（`/vm/create`、`/vm/import-disk`、`/vm/import-appliance`）、`delete_vm`（`DELETE /vm/:name`）、`force_delete_vm`

**虚拟机运维（11）**：`reinstall_vm`、`reset_vm_password`、`edit_vm_xml`、`unlock_vm`、`migrate_vm`、`migrate_vm_disk`、`make_vm_independent`、`network_capture`、`create_vm_schedule_delete`、`update_vm_schedule_delete`、`delete_snapshot`

**磁盘与设备（1）**：`delete_disk_file`

**控制台（6）**：`enable_spice`、`disable_spice`、`expose_spice`、`change_spice_password`（VNC 的 enable/disable/passwd/expose **未**标记高风险，仅 SPICE 暴露类为高风险）

**网络（19）**：`create_network_bridge`、`delete_network_bridge`、`set_interface_config`、`delete_port_forward`（单条与批量同名）、`delete_port_forward_ip`、`delete_public_ip`（单条、批量）、`bind_public_ip`（单条、批量）、`unbind_public_ip`（单条、批量）、`migrate_public_ip`、`apply_public_ip`、`disable_port_mirror`、`enable_port_mirror`、`repair_ovs_network`

**VPC（4）**：`create_vpc_switch_physical_uplink`、`delete_vpc_switch_physical_uplink`、`vpc_switch_reconfigure`、`apply_vpc_acl`

**防火墙（12）**：`apply_firewall`、`disable_firewall`、`rollback_firewall`、`enable_host_firewall`、`disable_host_firewall`、`create_host_firewall_rule`、`update_host_firewall_rule`、`delete_host_firewall_rule`、`add_host_firewall_vnc_default`、`close_host_firewall_connections`

**宿主机（5）**：`update_host_ksm`、`update_host_zram`、`update_kvm_unrestricted_guest`、`enable_host_iommu`、`load_vfio_pci`

**存储（5）**：`format_storage_pool`、`create_storage_partition`、`delete_storage_partitions`、`create_storage_volume`、`delete_storage_volume`

**模板（2）**：`delete_template`、`move_vm_disk_to_template`（制作模板的"移动"模式）

**我的存储（1）**：`delete_user_storage_file`

**系统（4）**：`rotate_jwt_secret`、`run_password_breach_scan`、`clear_finished_tasks`、`delete_port_forward`

> **可借鉴的取值口径**：高风险操作按"**会造成不可逆结果或影响可达性**"划定——删除类、暴露类（SPICE 对外）、网络拆分（桥接/接口配置）、宿主内核参数、密钥轮换、格式化。**查询类、开箱即用的创建类（普通 VM 创建除外）不列入**，避免二次验证被滥用后用户产生"验证疲劳"。

---

## 4. 从接口标记反推的权限模型

| 维度 | 表现 |
|---|---|
| **管理员专属** | `/host`、`/firewall`、`/settings`、`/security`、`/scheduler`、`/nodes`、`/storage-pool`、`/user` 几乎全部标记 `A`；`/ovs` 全部为 `A` |
| **弹性云专属（`E`）** | `/template`、`/vpc`、`/self/storage`、`/storage-pool/vm-targets` 等——轻量云用户不可见 |
| **VM 归属校验（`V`）** | `/vm/:name/**` 绝大多数带 `V`，即"只能操作自己名下的 VM"；对应 `VMAccessMiddleware` |
| **双入口** | 用户侧 `/self/**` 与管理侧 `/vm/**` 是**两套并行接口**（用户自助创建/删除/导出走 `/self`，管理员走 `/vm`），权限与参数范围不同 |
| **免登录** | 仅 `/public/settings`、`/public/version`、`/auth/*`（登录与找回密码链路） |

---

## 5. 变更记录

| 日期 | 变更内容 |
|---|---|
| 2026-09-14 | 创建文档：基于 `endpoints.json`（342 接口）、`server/model/task.go`（54 任务类型）、`endpoints.json` 的 `highRisk` 字段（82 高风险操作），按模块展开成功能矩阵；补充"任务类型缺 27 个中文文案"与权限模型反推 |
