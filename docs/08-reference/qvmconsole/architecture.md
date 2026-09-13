# QVMConsole 工程架构

> 状态：生效
> 最后更新：2026-09-13
> 结论基于提交：`52023d6`（`reference/QVMConsole`，分支 `main`）
> 关联：[`../README.md`](../README.md) · [`README.md`](README.md)（概览）· [`features.md`](features.md)（功能结论）

本文记录 QVMConsole 的**架构层面**事实：启动装配、分层约定、中间件、并发与后台任务、数据模型、HTTP API 分组与前端结构。文中路径均相对 `reference/QVMConsole/`。

---

## 1. 启动流程与依赖装配

后端入口 `server/main.go`（约 1520 行），启动顺序：

```
main()
├─ 子命令分派：port-mirror-watchdog / system-compatibility-check / host-zram-apply
├─ ensureLargeTempDir()                # KVM_TMPDIR；/tmp 为 tmpfs 时重定向
├─ config.Init()                       # 环境变量 + .env 加载
├─ logger.InitWithConsoleConfig(...)   # 日志系统
├─ model.InitDB()                      # SQLite + AutoMigrate + 默认管理员
├─ config.GlobalConfig.LoadFromDB()    # 数据库设置覆盖环境变量默认值
├─ libvirt_rpc.InitLibvirtRPC()        # go-libvirt RPC，失败则 fatal
├─ service.BootstrapVMCacheFromHost()  # VM 缓存预热
├─ config.ValidateSecurity()           # 默认 JWT 密钥则拒绝启动
├─ initCloneDeps() / service.Deps{...} # 集中注入各子包依赖钩子
├─ registerTaskHandlers()              # 注册 30+ 任务类型处理器
├─ taskqueue.Start(3)                  # 启动 3 个 worker
├─ 各 Start* 后台协程（见 §4）
├─ router.Setup()
└─ r.Run(":" + config.GlobalConfig.Port)   # 默认 8080
```

**依赖装配方式**：包级函数 + hook 变量（避免循环引用），如 `server/service/hooks.go`、`server/service/clone/deps.go`、`server/service/host/deps.go`；由 `main.go` 中的 `initCloneDeps()` 与末尾的 `service.Deps{...}` 大结构体集中赋值。

---

## 2. 分层与调用约定

| 层 | 目录 | 约定 |
|---|---|---|
| 路由 | `server/router/router.go`（1 文件，约 662 行） | 只做分组、中间件挂载与 `handler.*` 绑定；行尾中文注释会被前端 API 文档生成器采集 |
| 处理器 | `server/handler/`（49 文件） | 解析/校验入参、鉴权、二次验证，调用 `service.*`，返回统一 `ApiResponse` |
| 业务 | `server/service/`（358 文件） | 领域实现；顶层 `*_wire.go` / `*_delegate.go` / `*_registry.go` 做依赖注入与转发（如 `service/vm_delegate.go`、`service/ovs_wire.go`） |
| 存储 | `server/model/`（26 文件） | GORM 模型 + 表级查询函数 + `db.go` 迁移 |
| libvirt | `server/service/libvirt_rpc/` | go-libvirt RPC 封装；`UseGoLibvirt=false` 时降级 `virsh` |
| 异步任务 | `server/taskqueue/`（1 文件，约 617 行） | 内存队列 + worker + SSE 广播 + 取消 |
| 中间件 | `server/middleware/`（11 文件） | Gin 中间件与安全工具 |
| 工具 | `server/utils/`（6 文件） | `ExecCommand`、文件/进程封装（含 unix/windows 构建标签） |
| 日志 | `server/logger/`（2 文件） | slog 风格 `logger.App`，lumberjack 轮转 + 每日轮转 |

---

## 3. 中间件清单

### 3.1 全局（`server/router/router.go`，按注册顺序）

| 中间件 | 文件 | 作用 |
|---|---|---|
| `RequestLoggerMiddleware` | `middleware/request_logger.go` | 记录请求信息与脱敏响应体（受 `request_detail_log_enabled` 控制） |
| `SafeRecoveryMiddleware` | `middleware/request_guard.go` | panic 安全恢复 |
| `PublicAccessMiddleware` | `middleware/public_access.go` | 路由匹配前统一限制公网请求 |
| `CORSMiddleware` | `middleware/cors.go` | 跨域，支持白名单 |
| `SecurityHeadersMiddleware` | `middleware/security_headers.go` | 安全响应头 |
| `CredentialGuardMiddleware` | `middleware/credential_guard.go` | 认证前校验凭据格式与入口唯一性 |
| `RequestFilterMiddleware` | `middleware/request_filter.go` | 请求过滤开关 |
| `RequestGuardMiddleware` | `middleware/request_guard.go` | 请求体大小 / 路径守卫（含上传白名单） |
| `RateLimitMiddleware` | `middleware/ratelimit.go` | 全局 IP 限频（公开/认证两档，默认 20 / 0 次每分钟） |

### 3.2 路由级（`server/middleware/auth.go`）

| 中间件 | 作用 |
|---|---|
| `AuthMiddleware` | 仅允许正式 access token |
| `TokenTypeMiddleware` | 允许指定类型 token（如 `login`） |
| `JWTTokenTypeMiddleware` | 仅 JWT，不接受 API Key |
| `AdminMiddleware` | 管理员权限 |
| `ForcePasswordChangeMiddleware` | 强制改密时仅放行改密/登出接口 |
| `PublicBootstrapSettingsMiddleware` | 公网安全初始化令牌仅限 SMTP 初始化接口 |
| `ElasticCloudOnlyMiddleware` | 禁止轻量云用户使用弹性云自助能力 |
| `VMAccessMiddleware` | 非 admin 校验 VM（`:name`）归属 |

### 3.3 工具型（非 Gin 中间件）

- `middleware/fingerprint.go` `GenerateSessionFingerprint`：IP + User-Agent 指纹。
- `middleware/redact.go` `redactResponseJSON`：响应体脱敏。

---

## 4. 并发与后台任务

### 4.1 任务队列（`server/taskqueue/queue.go`）

- 内存 map 存储 + `sync.RWMutex`；任务 ID 由 `atomic.AddUint64` 自增。
- 队列通道 `taskChan = make(chan uint, 100)`；**worker 数量 = 3**（`taskqueue.Start(3)`）。
- 任务函数签名 `TaskFunc(ctx, task, progress)`，`CancelTask` 经 `context.CancelFunc` 取消（等待中直接标记、运行中触发 cancel）。
- SSE 广播：`RegisterSSEClient` / `broadcastEvent`，客户端缓冲满则丢弃事件。
- 自动清理：每小时删除 24 小时前的已结束任务。
- 任务参数输出前脱敏（`redactTaskParams`）。
- 任务类型常量 40+ 种（`server/model/task.go`），处理器在 `main.go` 注册（clone / linked_clone / batch / reinstall / prepare / template_export / template_import / delete_template / create / delete / snapshot / import / export / import_appliance / rescue 等）。

### 4.2 启动时拉起的后台协程 / 定时器

| 启动函数 | 周期 | 作用 | 文件 |
|---|---|---|---|
| `service.StartStatsCollector` | 10s 采集 / 60s 落库 | 宿主机 + VM 资源采集与持久化；内部再启动流量配额检查 | `service/host/stats_collector.go` |
| `StartTrafficQuotaChecker`（由上面触发） | 60s | 用户流量配额检查（含凌晨重置） | `service/traffic/quota.go` |
| `service.StartSchedulerEventCleanup` | 1h | 清理过期调度事件（`scheduler_event_retention_hours`） | `service/scheduler/center.go` |
| `service.StartVMScheduleRunner` | 30s | 执行到期的 VM 定时任务 | `service/vm/schedule.go` |
| `service.StartJWTSecretRotator` | `KVM_JWT_SECRET_ROTATE_HOURS`（默认 24h，0=禁用） | JWT 密钥轮换 | `service/security/jwt_secret.go` |
| `service.StartExpiredUploadSessionCleanup` | 30min | 清理过期分片上传会话 | `service/upload/chunkupload.go` |
| `service.StartPasswordBreachScheduler` | 每天 00:00 | 泄露密码定时检测 | `service/password_breach.go` |
| `service.StartStorageTrimScheduler` | 每天 02:00 | 用户存储回收（fstrim + `fallocate --dig-holes`） | `service/storage_trim_scheduler.go` |
| `service.StartUserSessionCleanup` | 1h | 清理过期/已撤销会话 | `service/user_session.go` |
| `service.StartPublicIPv6PrefixMonitor` | `KVM_PUBLIC_IPV6_SYNC_INTERVAL_SECONDS`（默认 60s，最小 10s） | 公网 IPv6 前缀与来宾配置对齐 | `service/public_ip/monitor.go` |
| `service.StartPortSecurityReconciler` | 默认 60s（最小 10s）+ OVSDB 事件 | OVS 端口安全策略协调 | `service/network/portsecurity/service.go` |

**启动时一次性同步（"启动自愈"）**：`SyncSSHDenyConfig`、`EnsureAllActiveUsersDefaultSecurityGroup`、`EnsureSystemBaseNetwork`、`EnsureAllNetworkBridgesRuntime`、`RestorePortForwardRules`、`EnsureAllVPCSwitchRuntime`、`RestorePortMirror`、`RestorePublicIPRules`。

**包内隐式定时器**：日志每日轮转（`logger/rotation.go`）、HIBP 缓存每小时清理（`service/security/breached_password.go`）、登录限流条目每 5 分钟清理（`service/security/login_limiter.go`）。

---

## 5. 数据模型（`server/model/`）

**建表方式**：GORM `AutoMigrate` + 手写增量迁移函数；**未发现 SQL 脚本或外部迁移工具**（全仓库无 `*.sql`）。

**DSN**：`<DBPath>?_busy_timeout=5000&_journal_mode=WAL&_txlock=immediate`，默认 `./data/kvm_console.db`；默认管理员在 `initDefaultAdmin()` 创建。

### 5.1 AutoMigrate 的表

| 结构体 | 表名 | 用途 | 关键字段 / 索引 |
|---|---|---|---|
| `User` | `users` | 用户、配额与安全状态 | `Username` unique、`Role`、`CloudType`、`DedicatedVPCSwitchID`、TOTP 系列、`MaxCPU/MaxMemory/MaxDisk/MaxVM/MaxStorage/MaxRuntimeHours/MaxPortForwards/MaxSnapshots/MaxBandwidth*/MaxTraffic*/MaxPublicIPs`；软删除 |
| `UserAPIKey` | `user_api_keys` | 外部 API 凭证 | `UserID` unique |
| `UserSession` | `user_sessions` | 服务端会话状态 | `SessionID` unique、`UserID` |
| `VmStatsRecord` | `vm_stats_records` | VM 历史资源记录 | `VMName` index、CPU/内存/网络/磁盘 |
| `PortForwardIP` | `port_forward_ips` | 端口转发手动 IP 映射 | `VMName` index |
| `HostStatsRecord` | `host_stats_records` | 宿主机历史资源 | 设备级数据以 JSON 字段内嵌（非独立表） |
| `UserTrafficDaily` | `user_traffic_daily*` | 用户日流量 | `Username`、`Date`、上下行、offset、限速标记 |
| `SystemSetting` | `system_settings` | 键值持久化配置（覆盖环境变量默认值） | `Key` 主键、`Value` |
| `VMCredential` | `vm_credentials` | VM 登录凭据（加密） | `VMName` unique、`PasswordEnc` |
| `VMCache` | `vm_caches` | 列表接口用的 VM 缓存投影 | `Name` unique、`OwnerUsername`、`Present`、`LastSyncedAt` |
| `AuthActionToken` | `auth_action_tokens` | 邮件链接类令牌 | `UserID`、`Purpose`、`TokenHash` unique |
| `SecurityChallenge` | `security_challenges` | 邮箱验证码 / 短时挑战 | `UserID`、`Purpose`、`CodeHash` |
| `SchedulerEvent` | `scheduler_events` | 定时/调度事件记录 | `SchedulerKey/Name/Group`、`VMName`、`Status` |
| `VMSchedule` | `vm_schedules` | VM 定时任务 | `VMName`、`Action`、`NextRunAt`、`LastTaskID` |
| `NetworkBridge` | `network_bridges` | 面板管理的宿主机网桥 | `Name` unique、`Mode`(nat/bridge)、`UplinkIF`、双栈地址 |
| `HostStoragePool` | `host_storage_pools*` | 宿主机硬盘面板配置 | `DeviceID` 主键、`MountPath`（运行时信息仍走命令） |
| `HostNode` | `host_nodes` | 迁移目标节点连接信息 | `APIBaseURL`、`SSHHost/Port/User`、`SSHKeyAuth`、`CapabilitiesJSON` |
| `LightweightVMQuota` | `lightweight_vm_quotas` | 轻量云单 VM 配额 | `VMName` unique、流量/带宽/端口/快照/运行时长 |
| `LightweightVMTrafficMonthly` | `lightweight_vm_traffic_monthlies` | 轻量云月流量 | 唯一索引 `(vm_name, month)` |
| `LightweightVMRegistration` | `lightweight_vm_registrations` | 轻量云待开通服务器 | `VMName` unique、`Template`、`SwitchID`、`TaskID`、`Status` |
| `VPCSwitch` | `vpc_switches` | 用户逻辑交换机 | `Username`、`BridgeName`、`BridgeMode`、`VLANID` unique、`CIDR`、`GatewayIP`/`DHCPStart`/`DHCPEnd` |
| `VPCSecurityGroup` | `vpc_security_groups` | 安全组 | `Username`、`VMName`、`IsDefault` |
| `VPCSecurityGroupRule` | `vpc_security_group_rules` | 安全组规则 | `SecurityGroupID` index、`Direction`、`AddressFamily`、`Protocol`、`TargetType/Value` |
| `VPCVMBinding` | `vpc_vm_bindings` | VM-交换机-安全组绑定（多网口） | 唯一索引 `(vm_name, interface_order)` |
| `VPCSwitchTrafficMonthly` | `vpc_switch_traffic_monthlies` | 交换机月流量 | 唯一索引 `(switch_id, month)` |
| `PublicIP` | `public_ips` | 公网/浮动 IP 资源 | `IP` unique、`CIDR`、`SupportedModes`、`AutoIPv6` |
| `PublicIPBinding` | `public_ip_bindings` | 公网 IP 绑定 | `PublicIPID` unique、`VMName`、`Mode`、`RuntimeStatus` |
| `VMLock` | `vm_locks` | VM 业务锁 | `VMName` unique、`Locked`、`LockedBy` |
| `UploadSession` | `upload_sessions` | 分片上传会话（秒传/断点续传） | `FilePath` 主键、`ReceivedBmp`、`Status`、`ExpiresAt` |

### 5.2 不入库的模型

- `Task`：纯内存（`server/model/task.go`）。
- `HostNetDeviceStat` / `HostDiskDeviceStat`：JSON 内嵌于宿主记录。
- `SchedulerEventFilter`：查询 DTO。

### 5.3 手写迁移

`server/model/db.go` 中除 AutoMigrate 外还有一批手写迁移与前置修复：`migrateUserCloudType`、`migratePublicIPCIDRColumn`（删除遗留 `c_id_r` 列）、`migrateUserPortForwardFeature/Quota`、`migrateUserSnapshotQuota`、`migrateLightweightSnapshotQuota`、`migrateLightweightRuntimeQuota`、`migrateVPCBindingInterfaceOrder(+Normalize)`、`migrateVPCSwitchCIDRColumn`、`migrateVPCSwitchTopologyFields`、`migrateVPCSecurityGroupRuleAddressFamily`，以及前置修复 `preFixVPCSwitchCIDRIndex()`。DDL 的索引名走白名单 `allowedIndexNames`。

---

## 6. HTTP API

统一前缀 `/api`，注册文件 `server/router/router.go`。**前端构建期**由 `web/scripts/generate-api-endpoints.mjs` 解析 `router.go` 与 `server/handler/*.go` 生成 `web/src/views/api-docs/generated/endpoints.json`（当前 342 条）。公开端点仅 `/api/public/*` 与 `/api/auth/*` 免登录。

| 模块 | 前缀 | 代表端点 | 注册位置 |
|---|---|---|---|
| 公开信息 | `/api/public` | `GET /public/settings`、`GET /public/version` | `router.go` |
| 认证 | `/api/auth` | `POST /auth/login`、`POST /auth/invite/complete`、`POST /auth/2fa/enable`、`POST /auth/high-risk/verify`、`POST /auth/logout` | `router.go` |
| 系统设置（管理员） | `/api/settings` | `GET/PUT /settings`、`POST /settings/smtp/test`、`GET /settings/log/read`、`POST /settings/diagnostics/export`、`POST /settings/storage/trim` | `router.go` |
| 安全 | `/api/security` | `GET /security/password-breach/status`、`POST /security/password-breach/scan` | `router.go` |
| 虚拟机 | `/api/vm` | `GET /vm/list`、`GET /vm/sse`、`POST /vm/create`、`POST /vm/clone`、`POST /vm/:name/operate`、`GET /vm/:name/vnc/ws`、快照/磁盘/CDROM/软盘/救援/直通/迁移/多网口 | `router.go` |
| 模板 | `/api/template` | `GET /template/list`、`POST /template/prepare`、`POST /template/upload/init\|chunk\|complete`、`POST /template/import` | `router.go` |
| 网络 | `/api/network` | 静态 IP、端口转发、UFW、宿主机网桥、接口 IP/DNS、公网 IP 批量操作、抓包下载 | `router.go` |
| VPC | `/api/vpc` | `/vpc/switches`、`/vpc/security-groups(+rules)`、`/vpc/acl/preview\|apply` | `router.go` |
| 防火墙（管理员） | `/api/firewall` | `/firewall/status`、`/firewall/apply`、`/firewall/host/rules`、`/firewall/geoip/import` | `router.go` |
| OVS 诊断（管理员） | `/api/ovs` | `/ovs/status`、`/ovs/repair`、`/ovs/port-security/*`、`/ovs/port-mirror/*` | `router.go` |
| 存储池 | `/api/storage-pool` | `/storage-pool/list`、`/storage-pool/:id/format-mount`、`/storage-pool/create-volume` | `router.go` |
| 节点 / 迁移（管理员） | `/api/nodes`、`/api/migration` | `/nodes`、`/nodes/:id/probe`、`POST /migration/adopt-vm` | `router.go` |
| 用户（管理员） | `/api/user` | `/user/list`、`/user/:username/quota`、`/user/:username/lightweight-registrations` | `router.go` |
| 用户自助 | `/api/self` | `/self/quota`、`/self/vms(+sse)`、`/self/vm/create\|clone\|import\|export`、`/self/storage/*` | `router.go` |
| 宿主机监控 | `/api/host` | `/host/stats(+sse)`、`/host/cpus`、`/host/ksm`、`/host/zram`、`/host/hardware-passthrough/*` | `router.go` |
| 任务队列 | `/api/task` | `/task/list`、`/task/sse`、`/task/:id`、`/task/:id/cancel`、`DELETE /task/clear` | `router.go` |
| 调度事件（管理员） | `/api/scheduler` | `/scheduler/list`、`/scheduler/events(+sse)` | `router.go` |
| 其他 | — | `GET /api/cpu-affinity-presets`、`GET /api/system-info`、`PUT /api/settings/public-access` | `router.go` |

**SSE 端点汇总**：`/api/vm/sse`、`/api/vm/:name/sse`、`/api/self/vms/sse`、`/api/host/stats/sse`、`/api/task/sse`、`/api/scheduler/events/sse`。

---

## 7. 前端结构（`web/src/`）

### 7.1 目录组织

| 目录 | 内容 |
|---|---|
| `api/`（21 文件） | 按模块封装的 HTTP 调用（`vm.ts` 最大）；`client.ts` 为 axios 实例 |
| `components/`（6 文件） | `business/`（`HighRiskChallengeModal`、`TaskDetailSheet`、`TaskMessage`）、`common/` |
| `config/`（3 文件） | `constants.ts`（API 基址、storage key）、`nav.tsx`（角色化导航）、`site.ts` |
| `features/vm-form/`（53 文件） | VM 创建向导/编辑表单：`CreateVmWizard`、`EditVmForm`、`payload.ts`、`sections/`、`dialogs/`、`useVmForm.ts` |
| `hooks/`（8 文件） | SSE 与通用 hook：`useVmListSSE`、`useVmDetailSSE`、`useHostStatsSSE`、`useTheme` 等 |
| `layout/` | `index.tsx` + `components/{Sidebar,TopBar,TaskBar,PageTabsBar,SponsorWidget}` |
| `router/` | `index.tsx`（路由表）、`pages.tsx`（`React.lazy` 页面声明）、`guards.tsx`（`RequireAuth`） |
| `stores/`（6 文件） | Zustand：`user`、`task`、`vm`、`app`、`highRisk`、`pageTabs` |
| `types/`、`utils/`（11 文件） | `api.ts`、`novnc.d.ts`；`vnc.ts`、`chunkUploader.ts`、`format.ts` |
| `views/`（235 文件） | 每模块 `index.tsx` + `components/` + `dialogs/` + css；`vm/detail/` 为子页面 |

### 7.2 路由表

`web/src/router/index.tsx` 用 `createBrowserRouter`：主框架 `/`（`RequireAuth` + `Layout`）下 17 个子路由（`dashboard vm vm/detail/:id template network public-ip firewall storage-pool my-storage user nodes scheduler task settings security api-docs about`），另有 `/login`、`/invite`、`/reset-password`、`/vm/:id/vnc-window` 独立路由；页面经 `React.lazy` 懒加载。

### 7.3 状态管理与请求层

- **状态**：Zustand store（见上表），`stores/task.ts` 承载全局任务进度与通知。
- **请求层**：`api/client.ts` 的 axios 实例，`baseURL = API_BASE_URL`（默认 `/api`）；请求拦截注入 `Bearer` token + NProgress；响应拦截统一错误 Toast（Semi）、**401 自动登出**、**428 高风险二次验证**自动弹窗 → `POST /auth/high-risk/verify` → 带 `X-High-Risk-Token` 重试原请求。

### 7.4 SSE 消费

原生 `EventSource`，token 通过 **query 参数**传递（`EventSource` 无法自定义请求头）：

- 任务：`api/task.ts` → `new EventSource('/api/task/sse?token=...')`，事件 `connected` / `task_progress` / `session_expired`；由 `stores/task.ts` 在登录后启动，5s 自动重连。
- VM 列表/详情、宿主监控：`hooks/useVmListSSE.ts`、`useVmDetailSSE.ts`、`useHostStatsSSE.ts` 同模式。

### 7.5 前端产物托管

`server/router/router.go` 的 `setupStaticFileServing()` 查找可执行文件同级或工作目录下的 `web-dist/`，注册 `/assets`、`/favicon.svg`、`/icons.svg` 静态路由，并用 `NoRoute` 对非 `/api` 路径做 SPA 回退到 `index.html`（含路径穿越与 null byte 校验）。构建时 `build.sh` 将 `web/dist` 复制为 `web-dist`，`install.sh` 部署到 `${INSTALL_DIR}/web-dist`。

---

## 8. 变更记录

| 日期 | 变更内容 |
|---|---|
| 2026-09-13 | 创建文档：基于提交 `52023d6` 整理启动装配、分层、中间件、并发与后台任务、数据模型、API 与前端结构 |
