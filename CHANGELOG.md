# Changelog

本项目所有值得注意的变更都记录在此文件。

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本 SemVer](https://semver.org/lang/zh-CN/)。

分类说明：
- `Added` 新增功能
- `Changed` 行为变更
- `Deprecated` 即将废弃
- `Removed` 已移除
- `Fixed` 缺陷修复
- `Security` 安全相关

---

## [Unreleased]

### Added
- **U-12 全量收口（2026-11-10，r21）**：按当前代码重生成 SKIP 清单后逐条补测 45 条端点，实机 **127/127 断言通过**（认证 60 / 流式·导出·控制台 32 / 全局写入 35）；`authkey`、`mailer`、`passaudit`、`reqlog` 四个零测试子包覆盖率 0% → 85.5% / 87.5% / 90.4% / 90.0%。另有 **U-16 节点通道 9/9 + 心跳 8/8**（协议版本 0/2 拒绝、证书 CN == agent_id、令牌一次性、无证书/陌生 CN/非本 CA 均被拒、心跳 10s 与离线 40s）与 **U-26 诊断导出 21/21**（secret 项只回 `is_set`、哨兵口令全包扫描不可见、租户 403）。`DELIVERY_TEST_PLAN.md` 的 TP-19 升为已测试、U-12/U-16/U-26 标记收口，`DELIVERY_TEST_REPORT.md` 新增 §10。
- **交付测试计划与执行报告（2026-11-10）**：新增 [`DELIVERY_TEST_PLAN.md`](docs/04-engineering/DELIVERY_TEST_PLAN.md)（能力基线、TP-00 ~ TP-23 共 24 套件、六视角追溯矩阵、五道阶段门与出口准则）与执行报告；`docs/README.md` 索引同步登记。
- **交付测试计划的执行状态标记（2026-11-10）**：`DELIVERY_TEST_PLAN.md` 新增 §5.0 逐套件状态总表（13 已测试 / 7 部分验证 / 4 未测试）与 §11 未测试项清单（U-01 ~ U-31，A/B/C 三档）；§7.1 补五道门实绩与出口准则逐条状态。明确区分「跑过」与「测完」，避免把 26/26 全绿误读为可交付。
- **路由守卫矩阵的 CI 用例（G-14）**：新增 `internal/handler/platform/guard_matrix_test.go`，从 `routes_*.go` 逐条解析 340 条域路由，校验总数与 GET/POST/PUT/PATCH/DELETE 方法分布、public 15 / admin 160 / user 165 三档互斥且覆盖全集、`publicAPIRoutes` 与源码逐条一致、adminOnly 必须同时挂 requireAuth。新增路由漏标权限时 CI 直接失败。
- **跨资源检索的单测（G-14 / TC-19-7）**：新增 `internal/service/ops/search/service_test.go`（6 例），覆盖空/单字符关键字、LIKE 通配符按字面匹配、租户 VM 归属隔离、模板可见性、节点仅管理员可见、每类 5 条上限。
- **接口契约补登（G-08）**：`docs/03-api/API.md` 补登邀请注册 2 条公开接口（API-393/394）与机械对账发现的 18 条漏登端点（API-395 ~ 412），公开接口注记由「13 个」修订为 **16 个**并写明逐条理由与一致性守护测试。

### Fixed
- **跨资源检索的 LIKE 通配符未转义**：`search.Service` 此前把用户输入直接拼进 `LIKE '%q%'`，搜 `%` 会命中全部资源（`auditlog` 已有转义，此处漏了）。现按同一口径转义 `%` / `_` / `\`，并给三条查询全部加上 `ESCAPE '\'` 子句——SQLite 没有默认转义符，不写 ESCAPE 时该修复会在测试库上静默失效。
- **停机时打开控制台返回 503（U-12 实机抓出）**：`GET /vms/:id/console/ws` 在虚拟机关机时返回 `503 无法建立控制台通道`——503 的语义是「稍后重试」，而重试一台关着的机器永远不会成功，界面还会显示成一次服务故障。现改为在**流打开失败之后**再参考状态投影：停机返回 `409 虚拟机未运行，无法打开控制台`，其余仍为 503。顺序刻意如此——先让 agent 试，失败后才解释，避免陈旧的投影挡住本可成功的连接。
- **软删除的用户名永久占名**：`uniq_user_username` 是无条件唯一索引，而用户是软删除（要保留审计里的 `operator_id` 引用），删掉的用户名再也注册不回来，且报错信息是「已被占用」而列表里查不到人。现改为 `WHERE deleted_at IS NULL` 的部分唯一索引（迁移 `0058`），与上一轮 `uniq_node_name` 的修法同型。
- **口令审计在开关关闭时记「检查失败」**：`passaudit` 的注释写着「开关关闭不算故障」，但 `tick()` 把 `RunNow` 的 422 校验错误当成真失败，每次启动都在调度事件页留一条红色「口令检查失败：定时口令检查未开启」。新增 `Enabled()` 早退，并留回归用例。
- **`agent/hub` 连接注销被吞掉（P1）**：为「新连接顶替旧连接」加的读协程里，`recvCh <- err` 与 `<-stream.Context().Done()` 在 `select` 中竞争，客户端断开时约有一半概率选中 Done 分支，**断开事件被丢弃**，节点在控制面上永久「在线」，指令被投进没人读的 outbox 直到超时。改用 `close(recvDone)` 传结束信号，并保证已到达消息不丢。

### Security
- **密码下限两套标准（可降级的绕过路径）**：`POST /auth/login/password` 要求 12 位，而 `PUT /auth/password` 只要求 8 位且仅挡「与用户名完全相同」——先用 12 位设好，再在账号设置里降回 8 位即可。现新建 `internal/platform/policy`（`MinPasswordLength = 12`）单点定义，登录改密、账号设置、管理员建号、应急口令、邀请注册五个入口全部引用；账号设置同时改为「不得**包含**用户名」并按 `[]rune` 计长。
- **实机走查 spec 的硬编码凭据**：`web/e2e-live/live.spec.ts` 不再把测试管理员口令写死在源码里，改为 `E2E_ADMIN_USER` / `E2E_ADMIN_PASSWORD` 环境变量注入，缺少口令时明确报错退出。

### Added
- **第三批收口（2026-11-05）**：`vmtag` 域迁入 `internal/repository/vmtag`（service 不再持有 `*gorm.DB`，权限判定留服务层、事务留 repo）；删除 `settings/spec.go` 中 11 个从未接线的设置项及随之变空的会话分组，并同步替换测试样本；打通 SPICE 能力链路（节点侧保守探测 `virsh domcapabilities` → 心跳能力标记 → 控制面按节点同步 `vm.spice_supported`），覆盖存量 VM 与能力消失的回退，真机判定列入验收清单。
- **第二批收口（2026-11-05）**：新增 PostgreSQL 会话级单实例锁（`instancelock.go`，SQLite 开发形态明确降级并记录约束）；新增 tag 触发的 `release.yml`（linux amd64/arm64 制品 + `SHA256SUMS`，**只出制品不部署**）；新增 [ADR-0014（Proposed）](docs/06-decisions/0014-node-channel-and-agent-lifecycle.md) 登记 live 热迁移 / relay / 证书续期 / agent 自动升级 / GeoIP 的候选方案与前置条件；GeoIP 未实现能力改为诚实阻断：端口转发 `allowed_regions` 从对外契约移除并拒绝旧字段、防火墙三个写入 `geoip_regions` 的入口明确拒绝（节点侧无消费方）、前端移除无效的地区输入。
- **本地开发建库入口（2026-11-05）**：新增 `go run ./cmd/dev-init`，按 GORM 模型幂等建出本地 SQLite 全套表。此前 `AGENTS.md` 的「`cp .env.example .env && go run ./cmd/server`」会得到一个没有任何表的库（服务启动不做自动迁移，唯一建表入口 `cmd/e2e-init` 是 E2E 专用且拒绝已存在库）；`AGENTS.md`、`DATA_MODEL.md`、`DEVELOPMENT.md` 已同步。
- **注册阶段协议版本校验（2026-11-05）**：`RegisterRequest.protocol_version` 此前 agent 上报、控制面不校验，proto 却承诺 N-1 兼容。现在签发证书前按 `hub.protocolVersionSupported` 拒绝不兼容版本（当前仅接受 1），补单测。
- **注册名与宿主机名不再空转（2026-11-05）**：`k_cockpit-agent enroll -name NAME` 现在会把 `requested_name` 送到控制面用于节点命名并回显（传输层做格式校验，写失败不阻断注册）；心跳里的 `hostname` 落到 `node.hostname`（迁移 `0051`）并在节点视图带出，空值不覆盖旧值。
- **历史数据定时清理（2026-11-05）**：`scheduler.RetentionLoop` 每小时清理终态任务及其阶段（30 天）、审计日志（1 年）与过期抓包记录（7 天或按 `expires_at`），使用已定义但一直未登记的 `KeyTaskCleanup` 记录调度事件；抓包文件仍由节点侧清理。`DATA_MODEL.md` §7 按实现修订。
- **真实 agent 真机验收清单（2026-11-05）**：新增 `docs/04-engineering/AGENT_ACCEPTANCE.md`，把「单测通过但真机待验」的 37 项能力、F-2-17、双机迁移、降级矩阵、安全复查与 E2E 核心链路变成有判定标准的可勾选清单；不在无 agent 环境写假 E2E 用例。
- **首次启动初始化介质（F-2-17，2026-11-05）**：创建向导的「初始化方式 / 主机名 /
  初始密码」此前只落库，来宾第一次开机不会发生任何事。现在节点按 `nocloud` /
  `configdrive` 生成 `meta-data` + `user-data`（或 ConfigDrive v2 的
  `meta_data.json` / `user_data`），用 `genisoimage` / `mkisofs` / `xorriso`
  打成卷标 `cidata` / `config-2` 的 ISO，挂成第二块光驱（`sdb`，安装 ISO 仍是
  `sda`）；缺工具链或挂载失败会连带回滚域与磁盘，不留孤儿域。
- **创建期静态地址生效（2026-11-05）**：节点创建结果回传 libvirt 分配的主网卡
  MAC；控制面写回网卡记录，并按 `(MAC, IP)` 登记 dnsmasq 静态租约，节点确认后
  才记 `applied_at`。此前这个字段只做了合法性校验，从未落库或下发。
- **cloud-init 就绪探测（2026-11-05）**：`vm.init.ready` 从恒 false 的占位改为
  经 qemu-guest-agent 检查 `/var/lib/cloud/instance/boot-finished`（并关闭文件
  句柄）。自动弹出安装介质只对 NoCloud 登记；ConfigDrive（Windows）没有可靠的
  完成信号，明确不自动弹出，避免弹早了摘掉还在安装的 WinPE 介质。
- **控制台配置真实下发（2026-11-04）**：`PATCH /vms/:id/console` 此前只改数据库——
  「对外暴露」不会让 qemu 监听 `0.0.0.0`，「改密」也不会写进域定义。新增
  `vm.console.update`：按协议修改 `<graphics>` 的 `listen / port / passwd` 并
  `virsh define`；控制面改为**先下发、成功后再落库**（节点拒绝时不留"看起来
  已生效"的记录），运行中的域明确提示需重启，SPICE 在节点未上报支持时直接拒绝。
  补 XML 定点编辑与下发链路的服务层/执行器测试。
- **编辑矩阵 12 项接通节点**：`boot_order`、`watchdog`、`video_model`、`rtc_mode`、
  `apic`、`pae`、`hide_kvm`、`memory_hugepages`、CPU 拓扑（sockets/cores/threads）、
  `cpu_affinity`、`cpu_limit_percent` 此前只写库不生效，现经域 XML 定点修改后
  `define` 落盘；CPU 配额与绑核写入 `<cputune>`，并修正为需关机后生效。
- **前端业务页面改为路由级懒加载（2026-11-04）**：此前 40 个页面全部静态导入，
  首屏包包含创建向导、模板、监控图表等重依赖，与 `vite.config.ts` 的首屏预算
  注释直接矛盾。现在除登录/初始化等入口外一律 `lazy()`，Suspense 边界放在布局
  内容区（切页不闪侧栏）；构建产物已按页面拆分为独立 chunk。
- **控制面静态托管前端产物**：`STATIC_DIR`（默认 `public`）指向 `web/dist` 的拷贝，由控制面直接
  服务前端——此前 `AGENTS.md` 与 `DEPLOYMENT.md` 均声明「由控制面静态托管」，但代码里没有任何
  静态服务，产物实际无处可放。行为约定：未匹配的 GET/HEAD 回退 `index.html`（SPA 深链刷新）；
  未匹配的 `/api/*` 始终返回 **JSON 404**（不回退 HTML，否则前端把 HTML 当 JSON 解析，报错位置
  与真实原因无关）；`assets/` 下按内容哈希长缓存（`immutable`）、`index.html` 每次回源；目录缺失
  只记日志、不阻断启动（开发期前端跑在 vite dev server 上）。路径经 `path.Clean` + `filepath.Rel`
  包含性校验，无越界面。
- **agent 补全 37 项 mock 兜底能力**：盘点发现控制面下发 120 个 OpKind、agent 注册表只有
  86 项，差额 37 项在 mock 下走默认分支返回 `Success=true`，真实节点上会返回「未实现」。
  已按域全部接通：VM 6 项（adopt/xml.apply/disk.change/disk.resize/disks.independent/
  interface.change）、模板 12 项（prepare/delete/rebase/flatten/promote_child/promote_delete/
  preprocess/export/export.delete/export.fetch/import.preview/import）、镜像 2 项
  （image.parse/import）、网络 14 项（ACL/交换机维护/计数/带宽/IPv6/公网 IP/抓包）、
  存储分区 1 项、宿主 PCI 绑定与调优 2 项。注册表 123 项、控制面引用缺口 0。
  配套契约修正：模板派生链下发 `parent_disk_path / child_disk_paths`；模板包固定为
  `manifest.json + disk.qcow2`，`template.export.fetch` 改为 offset/length 分片、控制面流式转发；
  网卡下发 `bridge_name`；`public_ip.reload` 下发绑定列表；新增 `sys.RunTimeout` 供整盘转换使用。
  单测覆盖 XML 定点编辑、路径越界、运行态拒绝、包往返与成功路径；**真机验收待补**。

### Removed
- **移除 mock 传输与演示数据**（[ADR-0013](docs/06-decisions/0013-remove-mock-transport.md)）：
  删除 `agent.MockClient` / `mock_frame.go` / `AGENT_TRANSPORT` 配置分支 / `internal/devdata` /
  `node.EnsureSimulated` / `POST /api/v1/dev/agent-register` / 前端「模拟」标记与 E2E mock 环境；
  控制面只保留 gRPC 反向长连接。测试改为每个包自写最小 stub，不再共享全局 mock。

### Fixed
- **第 13 轮实机深度回归（2026-11-10，存量对账 / 权限 / 故障注入 / 并发边界）**：
  换四个新维度审「控制面记下来的状态可不可信」，发现并修复 4 个产品缺陷（1 个 P1），
  新增 5 个可重复深度批次（`~/.kc_test/r13/`）；最终在 `bin/server-r13c` + agent r13
  上跑完 14 个既有套件 + 7 个第 12 轮批次 + 5 个新批次，共 26 个套件全部 rc=0、
  0 失败。逐项证据见 `data/lab2/test-report-r13.md`。
  - **P1 存储池把根分区容量当成池容量**：节点重启后 `/dev/nbd*` 设备与挂载消失，
    只剩 fstab 条目和空目录；节点侧 `storagePoolStat` 直接 `statfs` 该目录（返回根文件系统
    容量），控制面又无条件刷新 `last_reported_at`，形成「ready + 新鲜 + 还有 77G」三重假象。
    现 `agent.PoolStat` 带 `Mounted`，节点按 `/proc/self/mountinfo` 精确判定挂载点；
    控制面只写回真实挂载的容量与时间戳，明确未挂载时降级为 `unmounted`，
    设备回来后自动回升。真机容量从假的 124 GiB 变成真实的 14.7 / 5.8 GiB。
  - **P2 告警中心越权**：API-024i 标注管理员专属，三条路由却只挂 `requireAuth`，
    租户可读全平台告警并用 `ack-all` 全部确认。现补 `adminOnly` + 服务层 `IsAdmin` 校验，
    同步 `adminAPIRoutes` 文档元数据。
  - **P2 软删除记录污染概览/告警/采集器**：`model.VM.DeletedAt` 是普通列，GORM 不自动
    过滤；回收站记录被 `syncRuntime` 刷成 `present=false` 后，`dashboard.missing` 与
    告警把它当成「在虚拟化层已不存在」。五处统一补 `deleted_at IS NULL`。
  - **P2 VM 投影新鲜度误报**：`syncRuntime` 实际每 300 秒对账一次（`runtimeTick%5`），
    而 `StaleThreshold` 只有 60 秒，导致面板长期误标「数据可能陈旧」、列表与详情结论相反。
    现恢复每轮对账，默认阈值改 120 秒（采集周期的 2 倍）。
- **第 12 轮实机全功能回归（2026-11-09，控制面 + 真实 KVM 节点）**：以 341 条端点和
  6 个新深度批次覆盖此前从未测过的写路径，发现并修复 15 个产品缺陷（3 个 P1），
  最终全量回归 0 失败。逐项证据见 `data/lab2/test-report-r12.md`。
  - **P1 交换机迁移假成功**：目标网卡不存在时 OVS `add-port` 会造出 internal 端口并报成功，
    任务全绿、面板记下一块不存在的网卡。现先校验网卡真实存在，失败即诚实报错。
  - **P1 存储卷创建从未成功**：`vgcreate` 前缺 `pvcreate`，且卷组创建失败被吞掉，
    `lvcreate` 必然报 `Volume group not found`；删除也不幂等，失败记录占着设备形成死锁。
    现按 PV→VG→LV 顺序建立、失败如实返回、删除按 `lvdisplay` 判定幂等并释放 VG/PV。
  - **P1 agent 单元缺 LVM 元数据写权限**：`ProtectSystem=full` 下 `/etc/lvm` 只读，
    `lvcreate` 在 systemd 服务里返回 status 5（普通 shell 正常）。`ReadWritePaths` 增 `-/etc/lvm`。
  - **P2 假选项 / 假成功**：`backend=ovs` 的网桥实际建成 Linux 桥；OVS 修复对未知 `kinds`
    建空任务并报成功；任务清理对畸形/负数 `keep_days` 静默按 7 天删历史任务；
    已撤销邀请可被 `resend` 复活；计算配额给不存在的用户/节点落库产生孤儿记录。
  - **P2 凭据有效期溢出**：节点注册令牌与邀请的 `ttl_hours` 无上限（99999 小时得到 2038 年
    过期的令牌，更大值让 `time.Duration` 乘法溢出并静默回落默认值）。现分别限 7 天 / 30 天。
  - **P3 契约与可观察性**：不存在节点的未纳管域列表改 404（原 503 把参数错误伪装成服务故障）；
    批量删除端口转发返回 `task_ids`；端口安全 Apply 的 `policy` 改用与列表一致的 snake_case 视图；
    存储池配置备注明确解引用写入。
  - 新增单测覆盖上述分支；`gofmt`/`vet`/`go test ./...`/`tsc`/oxlint/Vitest 34/`pnpm build` 全绿。
- **第 11 轮「假选项」收口（2026-11-09，控制面 + 真实 KVM 节点）**：本轮只处理一类问题——**配置看起来
  生效、实际不生效**：
  - **端口镜像的 `vlan_preserve` 是假选项（P2）**：界面写着「保留 VLAN 标签 —— 默认剥掉」，库里也接受
    `false`，但节点侧从未读取该字段，OVS `Mirror` 原样复制帧——标签一个都没少。真机建 VLAN 子接口抓包
    确认镜像帧带 `802.1Q vlan 100`。现按实现对齐契约：不传归一化为 `true`，显式 `false` 返回 400（文案
    说明该去掉字段或设为 true）；下发参数固定 `true`；存量 `false` 行编辑保存后自愈。前端移除复选框，
    改为静态说明「镜像帧原样复制，保留原始 VLAN 标签」，列表徽标一并去掉。`API.md` 的 API-181 同步。
  - **设置项保存不 trim（P3）**：`notification.smtp_from_name` 曾以 `"K Cockpit     "`（5 个尾随空格）
    入库并原样回显，下次保存继续带上。现在「校验、写入、应用钩子、审计」统一使用 trim 后的值（只裁
    首尾）；只有空白的敏感项仍按「留空保持原值」处理，不会被写成空串。存量值已按新规则修正。
  - **PATCH 定时任务静默丢弃字段（P3）**：`{enabled, time_of_day}` 曾返回 200 但只改开关，用户以为执行
    时间改了。现在显式接住 `action / snapshot_name / include_memory / schedule_type / weekdays /
    time_of_day / date / cron / name`，出现任一即 400，并说明「PATCH 仅支持启停，修改请删除后重建」
    （当前没有整条更新接口，文案不指向不存在的 PUT）。
  - **两处删除无二次确认（P3）**：删除用户、删除安全组此前点一下即发请求。现在都过确认弹窗，文案说清
    真实后果：账号软删除、用户名留在审计里、名下有虚拟机时不可删；安全组连同规则一起删除、不进回收站、
    仍被网口占用时不可删，规则已加载时给出实际条数。
  - **控制面无法优雅退出，重启后节点「在线但指令送不到」（P1，本轮实测发现）**：`Hub.Stop()` 直接调
    `grpc.Server.GracefulStop()`，而 agent 的 `Channel` 是长连接双向流、永不自然结束——进程收到 SIGTERM
    后挂死（实测 22 分钟），继续持有节点连接并写心跳，新实例的连接表为空，所有任务报「节点不在线」。
    现在改为「有限等待（3s）+ 强制停止」，超时日志带连接数；断连后 `Channel` 的 Recv 退出、在途操作以
    「结果未知」收场（与崩溃语义一致）。真机：旧版 SIGTERM 后 6s 仍存活，新版 3351ms 退出，重启后节点
    5 秒内自动重连；新增假 stopper 单测（反向改回直接 GracefulStop 即超时失败）。
  - **重启后底部任务栏卡在「执行中」（P2，补丁）**：托盘只认 SSE 事件、没有轮询兜底；而控制面重启时
    在途任务被收敛成 `unknown/failed`，那些事件发出时没有任何订阅者。现在每次 `connected`（含断线重连）
    都失效重取任务，并把单数键 `['task', id]` 一并纳入（此前详情抽屉只能靠 10s 兜底轮询）；实时连接断开
    期间托盘退化为 10s 低频轮询，连接正常时仍完全由事件驱动。
- **第 10 轮实机增量回归（2026-11-09，控制面 + 真实 KVM 节点，450 项断言全过）**：收口第 9 轮遗留的
  2 个已知问题与 2 项加固建议：
  - **端口镜像实际不复制流量（P2）**：`target_switches` 被下发但节点从未读取，OVS `Mirror` 无
    `output_port`/`sink`，多源只取第一个、方向被忽略。现在全部源端口进入同一 Mirror；每个目标交换机
    一条镜像行（跨桥用 veth 注入目标交换机、同桥用 internal 端口避免成环）；`ingress/egress/both` 正确
    映射 select 列；启用前按前缀清理旧拓扑、失败回滚；停用/看门狗到期/agent 重启统一清理 Mirror 行、
    端口与 veth。真机验证：目标侧 veth 用 tcpdump 实际抓到镜像帧，停用后无 `kc-mir-*`/`kcm*` 残留。
  - **僵尸机删除后系统盘残留（P2）**：域定义被外部 `virsh undefine` 后，节点从 XML 读不到磁盘，任务
    成功但 `images/<name>.qcow2` 留下。新建时落盘磁盘清单（`/var/lib/k_cockpit/vmmeta/<name>.json`），
    删除时优先按清单清理；无清单的旧机器按精确文件名清理系统盘、数据盘只提示不删（避免误删同名
    前缀的另一台机器），清单路径必须位于镜像目录内。`keep` 保留磁盘与清单。
  - **非法输入 403 缺少 request_id（P3）**：`RequestID` 提前到 `InputFilter` 之前，403 响应现在同时带
    `X-Request-Id` 与 `body.request_id`，可与日志关联。
  - **生产环境关闭 Secure Cookie（P3，加固）**：`APP_ENV=production` + `SESSION_SECURE_COOKIE=false`
    现在启动即拒绝，避免「HTTPS 下登录后立刻 401」与明文链路令牌截获。
- **第 9 轮实机增量回归（2026-11-09，控制面 + 真实 KVM 节点，1370 项断言全过）**：本轮覆盖全新安
  装、崩溃恢复、节点断连、并发、安全边界、应急 CLI、防火墙真实应用、UI 表单、定时任务与分页健壮
  性，修复 3 个问题（均带单测与真机复验）：
  - **宿主防火墙「默认拒绝」未落地（P1，安全假象）**：预览明确显示 `iptables -P INPUT DENY`，但节点
    实现从不修改 `INPUT` 策略、`KC-HFW` 链尾也没有兜底 DROP——在没有 ufw 的主机上未匹配流量全部
    放行。现在启用且默认动作为 deny 时，在链尾追加 `-j DROP`（排在全部放行规则之后、链每次 apply
    重建因此幂等），不再覆盖宿主机自身的默认策略；预览改为真实的链级命令，并新增「默认拒绝会拦掉
    宿主机给虚拟机提供的 DHCP/DNS」的可执行警告。真机端到端：未放行端口被拒、SSH 仍通、回滚恢复。
  - **崩溃重启后导出/导入影子记录永久 pending（P2）**：进程被强杀时执行器不会跑，`template_export` /
    `vm_export` / `image_import` 会永远显示「进行中」，用户既拿不到产物也无法重试。三个服务在构造期
    做一次启动收敛：`pending/running` → `failed`，错误文案明确「控制面重启，导出中断，请重试」/
    「导入中断，请重新导入」；`success/failed` 不动。单实例锁保证启动瞬间不存在其它实例的在途任务。
    kill -9 实测：任务转 `unknown`、记录转 `failed`、新导出可成功/下载/删除。
  - **安全组删除后详情面板残留（P3，前端）**：选中组被删除后列表已刷新，但 `selectedGroup` 仍指向
    已删对象，规则面板继续显示旧组并让人点出 404。改为渲染期从最新列表反查选中组（`activeGroup`），
    删除后面板自动收起；A5 复跑 `staleCount=0`。
  - 另发现两项需产品决策的已知问题（未修，详见 `data/lab2/test-report-r9.md` §3）：端口镜像的
    `target_switches` 被下发但节点从未读取（OVS Mirror 无 `output_port`/`sink`，实际不复制流量）；
    域定义被外部删除的僵尸机，删除成功但系统盘文件残留（缺少磁盘清单，按命名约定清理有误删风险）。
- **第 8 轮实机全功能回归（2026-11-08，控制面 + 真实 KVM 节点）**：本轮在真实 agent 环境跑完
  计算/存储/网络/模板/平台五域，共修复 20 个缺陷，均带单测与真机复验：
  - **控制台 WS 断开打崩整个控制面（P0）**：`pipeConsole` 的转发协程无 recover，浏览器关标签页/
    刷新导致 hijack 连接关闭后 hertz-contrib/websocket 在 `Read` 解引用 nil，进程直接退出。
    两个转发协程各加 recover（先兜 panic 再发 done，保证恰好一次），会话改为 sync.Once 释放并
    绑定连接生命周期；10 次「101 后 RST」不再崩溃。
  - **交互控制台 101 后零字节**：agent `handleOpenStream` 回拨 `StreamData` 的 RPC ctx 派生自
    3 秒拨号超时 ctx，接收循环 `dialCancel()` 立刻取消 RPC → 桥关闭 → 会话 1ms 结束。改为拨号
    上下文只给 `dialVNC`，RPC 用会话级 ctx；真机验证首帧 `RFB 003.008`、会话上限 3 与第 4 条 409。
  - **网络写操作下发域名为空**：`enqueueNetChange` 未把 `vm_id/vm_name` 放进任务参数，导致新增/
    删除网卡、静态地址等节点侧 `virsh` 拿到空域名而必然失败。入队前补齐身份字段。
  - **软删 VPC 交换机仍出现在创建向导**：`VpcSwitch.DeletedAt` 是自定义 `*time.Time`，查询未显式
    过滤 `deleted_at IS NULL`；创建向导与安全组交换机校验均已补。
  - **9p 目录共享假成功**：节点 `vm.share.mount` 只校验路径、从不改域定义，控制面却记 active。
    现在真正插入/移除 `<filesystem>` 并 `define`，运行中域如实提示需重启；卸载按 tag 精确删块。
  - **删除存储文件不删节点字节**：`DeleteFile` 只删库记录，节点文件永久成为孤儿。新增
    `storage.file.delete`（复用当前根→历史根回退、幂等、越界拒绝），控制面先删字节后删记录。
  - **重装系统忽略 `disk_gb`**：模板分支只 `qemu-img convert`，实际容量仍是模板大小而记录按请求
    容量计费。convert 后调用 `growDiskTo`（只扩不缩），失败还原 `.pre-reinstall` 备份。
  - **模板预处理注入 SSH 公钥必失败**：`virt-customize --ssh-inject` 需要 `root:file:<路径>`
    选择器；同时受理阶段校验公钥类型与 base64。
  - **模板 `visibility` 无枚举校验**：任意字符串可落库；改为枚举 + 小写归一化，非法值 422。
  - **模板派生第一代与根模板 version 重号**：`nextVersion` 只统计 `family_id`，根模板
    `family_id=NULL` 被漏掉；统计口径改为 `id = ? OR family_id = ?`。
  - **光驱换总线先改库、节点必然失败**：控制面受理时即写 `vm_cdrom.bus`，节点 `case "bus"` 直接
    拒绝 → 库与域分叉。现在下发 `from_bus` 并在节点真正 detach+attach，成功后才回写记录；
    aarch64/virt 机型选 `ide` 会在受理/节点双侧预拒绝（IDE 控制器不存在，放行会产出开不了机的域）。
  - **SPICE 能力未投影到新建/纳管 VM**：`vm.spice_supported` 只读不写，新建机器永远只能选 VNC。
    创建/纳管按节点 capabilities 初始化；心跳已有的存量同步补上，真机验证 `protocols=["vnc","spice"]`
    与 `.vv` 连接文件。
  - **接受邀请创建的账号无法登录**：`CreateFromInvite` 复用管理员建号默认值（pending + 强制改密），
    受邀人自设密码后仍 401。邀请路径改为 active + 免强制改密，管理员建号语义不变。
  - **快照删除下发显示名而非虚拟化层名字**：节点 `--snapshotname` 拿到空/错误值，宽泛的
    「not find → 成功」还会造成「库删了、节点快照还在」的假成功。改为下发 `snapshot_name`
    （域内真实名）+ 空名拒绝 + 缺失时查 `snapshot-list` 复核后再判幂等。
  - **快照创建失败留下 ready 幽灵记录**：节点明确失败时物理删行；节点不可达保留 error 态。
  - **VM 彻底删除时安全组关联清理报 `no such column: vm_id`**：改为先取该 VM 的网卡 ID 再按
    `interface_id IN (...)` 删除。
  - **控制台会话被提前释放**：`upgraded` 标志在 Upgrade 回调内才置位，而 defer 在 Upgrade 返回时
    已执行 → 每次成功升级都立刻 CloseConsole，`active_sessions` 恒 0、会话上限失效。改为
    sync.Once + 连接结束释放 + 15s 回调未调度兜底。
  - 配套回归：模板/镜像 373 用例 + 计算 163 + 存储 14 + 网络 50 + 平台 65 + 控制台 WS 12 +
    前端全页面走查 3，均在新构建上复跑；已知遗留见 `data/lab2/test-report-r8.md`。
- **自动化探测循环（2026-11-08 晚，独立实例上跑出）**：
  - **`POST /vpc-acl/apply` 缺 `node_id` 返回 503**：参数一路走到 `agent.Execute(NodeID=0)`，
    再被回落成「节点不支持 ACL 预览」——把一次参数错误伪装成节点故障，调用方会去查节点健康。
    现在与 List/Preview 一致返回 400「必须指定 node_id」（接口层 + 服务层各一道）。
  - **安全组名称没有长度上限**：列定义是 `size:64`，服务层只校验非空。SQLite 不检查 VARCHAR
    长度，2 MB 的名字会被原样存进去；PostgreSQL 会直接报 `value too long` 变成 500。同一份输入
    在两种驱动下行为不同，只能由服务层收口，现在与交换机/模板/用户一样限 64 字。
  - **未知路由返回 text/plain `Not Found`**：API-only 部署（没有前端产物、静态托管不启用）时
    没有 NoRoute 兜底，客户端按 JSON 解析直接报解析错误，也拿不到错误码与 request_id。
    现在统一返回 JSON 404；同时开启 `WithHandleMethodNotAllowed`，路径存在而方法不对时返回
    **405 `METHOD_NOT_ALLOWED`**（该错误码已登记到 API.md §3.3），不再混成 404。
  - **固定临时文件名导致并发覆盖与偶发失败**：光驱 XML 用 `/tmp/kc-cdrom-<vm>-<dev>.xml`，
    5 秒定时删除。并发/连续操作会互相覆盖，而同名文件被定时器删掉后再用就拿到空描述——
    表现是「单跑必过、整包偶发失败」。创建域、恢复域的 `kc-define-*/kc-restore-*` 同属一类。
    现在统一走 `writeTempXML`（`os.CreateTemp` 唯一文件 + 调用方清理）。
- **第五轮：7 项 P2（2026-11-08，实机抓出并逐项复验）**：
  - **创建向导的架构默认值按节点走**：此前硬编码 `x86_64 / q35 / bios`，在 aarch64 宿主上
    「什么都不改直接提交」会被组合校验拒成 400，每次创建都要手动改三项。节点现在通过心跳
    capabilities 自报 `host.arch.<arch>`（复用 `console.spice` 那条通道，不动 proto），
    向导据此下发 `aarch64 / virt / UEFI / ramfb`；`POST /vms` 也按节点架构补齐空缺字段，
    API 调用方不传架构同样能建成功。
  - **内置 API 文档摘要不再漏出路径原文**：341 条接口里曾有 147 条是「查询host列表」
    「创建imports」「对节点执行「磁盘」」这类条目。摘要生成补齐了模块 / 子资源 / 动作三张
    词表，改按「动作 + 宾语」组织（`POST /host/passthrough/bind` → 绑定直通设备），
    并新增回归测试 `TestAPIDocsSummariesUseChinese` 扫描全量路由防止再退化。
  - **建池后立即回填容量**：此前要等下一轮刷新（默认 2.5 分钟），期间列表显示 0，用户以为
    池没建成功。抽出 `RefreshNodeCapacity` 在建池任务结束时调用一次。
  - **`setup/admin` 不再下发中间态 Cookie**：新管理员的自动登录必然停在安全初始化阶段，
    返回的是五分钟的 login 级令牌——写进 Cookie 会得到一条所有业务接口都拒绝的会话，还留下
    孤儿记录。现在只在拿到 access 级令牌时写 Cookie，否则把 `stage` + `login_token` 放进
    响应体，与 `/auth/login` 一致。
  - **任务类响应形状统一**：13 处接口直接返回任务模型（`{"task":{"ID":29,...}}`，大驼峰），
    其余返回 `{"task_id":..,"status":..}`。新增 `api.WithTask` 收敛为标准形状；扩盘接口的
    `DiskResizeResult` 同步改为 `task_id`/`status`（它内嵌任务模型，是 map 之外的漏网一处）。
  - **小于 1 GB 的磁盘容量不再显示 0**：节点新增 `CapacityBytes` 精确上报，控制面按两位
    小数折算（153 MB → `0.14 GB`），并同时给出 `capacity_bytes`。
  - **历史存储根里的文件可以挂载**：存储根切换后，旧文件仍能列出、能下载，但挂盘走的是
    当前根拼路径，必然 `stat` 失败。挂盘与下载改用同一套回退寻址，并按文件实际所在根放行
    AppArmor。
- **用户存储根改为跟随默认存储池（2026-11-08，实机抓出）**：`userdataRoot` 此前是写死的
  `/var/lib/k_cockpit/userdata`，落在系统盘——节点建了 15G 数据池也用不上，616MB 的云镜像
  直接写进根分区，而"系统盘被镜像写满"会拖垮整台宿主机。现在控制面按默认池算出期望根
  （`<mount_path>/userdata`），通过新增的 `storage.root.set` 下发；agent 只接受落在自己管理的
  池挂载点之下的路径（控制面被攻破也不能把用户存储指到 `/etc`），生效后落盘到
  `<state>/storage-root` 并在启动时读回。建池/改配置/卸载与容量刷新循环都会收敛它。
  切换前上传的文件仍留在历史根里，读/秒传来源走**回退寻址**，不会变成"列得出、下不来"。
- **默认交换机不再伪造 `kbr0` 记录（2026-11-08，实机抓出）**：系统基础网络此前只往库里插一条
  `bridge_name=kbr0`、没有网段也没有 DHCP 范围的记录，而宿主上从来不存在那座桥——面板显示
  网络可用，实际一台虚拟机都接不上去。现在改为与自建交换机**同一条开通路径**（节点纳管后
  受理 `vpc.switch.change`，宿主建好 OVS 桥 + 网关 + DHCP 后才写记录），桥名由节点与名字
  推导，网段固定 `10.77.0.0/24`。另加网关占用检查：同一地址已配在别的网桥上时明确失败，
  而不是静默产生"同网段双网桥"这种只在偶发不通时才暴露的配置。
- **回收站恢复真正可用（2026-11-08，实机抓出）**：移入回收站会删除域定义，恢复接口因此只能
  回一句「域定义已不存在」——回收站事实上只有"彻底删除"一条出路。现在节点在
  `disk_action=keep` 时先把域 XML 留存到 `/var/lib/k_cockpit/trash/`，恢复时由新增的
  `vm.restore` 按它 `virsh define` 重建（关机态），彻底删除时一并清理。恢复后的机器可直接开机。
- **第 1–2 轮 E2E 缺链修复（2026-11-06）**：`page_size` 上限钳制为 100（此前大值直接进 SQL LIMIT，分页不真收敛）；XML 写路径补校验；`my-storage` 参数校验收口；审计日志 `/api/v1/audit` 补分页（此前无分页、自钳 200 条，超过就静默截断）；并发扩缩容后 `disk_gb` 投影与节点 qcow2 实际大小对账（采集器此前只增不减，resize 并发后两侧长期分叉）。
- **网络：计数重置命令非法、上行口被吞、dnsmasq 抢占 53、软删记录占住唯一索引（2026-11-06）**：
  ① `network.counter.reset` 用 `ovs-vsctl --all clear Interface statistics`，而 `clear` **没有 `--all` 选项**——这个功能从上线起每次都返回 422；改为先取接口名再逐个 `clear`，并改用 `ovs-vsctl` 真正支持的 `--no-headings --format=csv`。
  ② `vpc.switch.change` 把 `uplink_if` 读出来却从未使用，交换机建好却没有上行口（任务还报 success）；现在 `ovs-vsctl --may-exist add-port` + `ip link set up`，失败时**回滚刚建的桥**，删除时显式 `del-port`。另加一道闸：拒绝把承载默认路由的网卡当上行口（接进 OVS 会当场切断管理连接且摘口不恢复 IP）。
  ③ 每张交换机的 dnsmasq 配置没关 DNS，第二台实例必然抢占 `127.0.0.1:53` 启动即退，而旧代码只看 `Start()` 是否报错就判定成功（假成功）；现在 conf 加 `port=0`，并在启动后用 3×100ms 窗口确认进程真的存活，失败则清理 pidfile 并报错。
  ④ `vpc_switch` 的唯一索引不排除软删行，同名重建必然撞约束且失败后**在宿主机留下悬空桥**；索引改为 `WHERE deleted_at IS NULL` 的部分唯一索引，冲突时按节点回报的「这座桥是否本任务创建」回滚，不误删他人在用的桥。
- **模板：导出摘要缺失、关键字搜索 500、拉平只换父盘、软删同名重导必失败（2026-11-06）**：
  ① 导出包的 `sha256` 节点侧算好了、控制面直接丢掉，用户无法在导入前核对完整性；补 `template_export.sha256` 列并在导出视图透出（历史记录为空串）。
  ② `GET /templates?keyword=` 用 `ILIKE`（PostgreSQL 专有），真机是 SQLite → 一直 500；改为 `LOWER(name) LIKE ?`。
  ③ `template.flatten` 把同名 parent 原样传下去，等价于 rebase——派生链一点没变只是换了个父，「拉平」从未生效；改为走 `qemu-img convert` 到临时文件、校验无 backing 后原子替换。
  ④ 模板是软删除，而无条件唯一索引让它永久占名：预览（GORM 自动过滤软删行）说「可以导入」、落库却必然冲突。索引改为部分唯一索引，预览与导入执行器共用同一个同名判定（显式写 `deleted_at IS NULL`），文案也只有一处。
- **存储与磁盘：裸二进制分片被拦、秒传只写记录不落盘、设备名被静默改名、卸载不存在设备报成功、负数容量被受理、OVF 容量单位丢失（2026-11-06）**：
  ① 上传守卫的 Content-Type 白名单缺 `application/octet-stream`，分片 PUT 一律 403（只有"客户端恰好不声明"才通得过）；补上。
  ② 秒传命中时只 `Create` 一条指向新路径的记录，磁盘上什么都没有，接口却回 `instant=true`；现在先在节点侧硬链接（跨设备回退为 `io.Copy` + `fsync`），**节点确认后才写记录**，摘要不一致即失败。
  ③ 挂载磁盘时无条件按总线生成设备名，请求的 `dev` 从未被读（请求 `vdb` 实际得到 `sdb`）；现在采纳请求值，前缀与总线不匹配或名字被占用都返回明确错误；槽位耗尽导致总线降级时，在受理阶段就解释降级并给出替代名。
  ④ 卸载一个从未挂载过的共享返回 success，用户无法分辨「已经卸掉」与「本来就没有」；改为查域定义确认该 tag 存在，否则 404。**磁盘卸载是同一条路径上的第二个洞**：请求一个不存在的设备名时校验循环整圈没匹配到，任务照常入队、节点什么都没做、接口回 200 success；同样改为 404，并保留"系统盘不可卸载/不可热插拔"两条既有守卫与"存在的盘仍能正常卸载"的正向路径。
  ⑤ ZRAM 容量的两个判断都是"大于某值才算错"，负数两边都不满足 → 入队后节点又跳过设置，任务成功而容量没变；补负数下界。
  ⑥ OVF 的 `capacity` 没有按 `capacityAllocationUnits` 换算：`capacity="8" + "byte * 2^30"`（= 8 GiB）被当成 8 **字节**、向上取整成 1 GB；改按单位换算（byte/2^10…2^40 与词形写法，空格与大小写不敏感）。
- **控制台：graphics 带子元素时被写坏、口令长度校验分裂（2026-11-06）**：
  ① 改控制台配置按"第一个 `/>`"定位 `<graphics>`，而 libvirt 的 graphics 带 `<listen/>` 子元素——`passwd` 被插到内层标签上，产出的 XML 标签错配，**改口令 100% 失败**。改为区分开标签与整段元素、只改开标签、子元素原样保留，并同步 `<listen>` 子元素地址（否则"对外暴露"显示成功而实际监听不变）。
  ② VNC 口令上限 8 字节，但校验只写在保存密码那条分支，已启用时超长口令被原样下发给 libvirt，回来是 422 + 一句英文原文；现在在入口统一校验（**按字节**计：8 个中文 = 24 字节必须拒），两条分支共用同一句中文文案，SPICE 豁免。
- **来宾代理：域定义里从来没有 guest agent 通道（2026-11-06）**：创建、纳管、改 XML 三条产生域定义的路径都没有 `<channel type='unix'>`，来宾内改密 / 读 IP / 就绪探测 / 自动弹介质全部不可达（`virsh qemu-agent-command` 直接报 `guest agent is not configured`）。现在三条路径统一补齐 virtio-serial 控制器与通道（通道只写 `source mode='bind'`，socket 路径交给 libvirt 按域分配，不绑发行版路径）；改 XML 时**校验与落盘用同一份补齐后的定义**。既有域不会被静默重定义，需重新纳管或编辑 XML 保存才会获得通道另外**"agent 到底连上没有"这个信号以前根本没有人写**（`vm.guest_agent` 恒为 false，于是 `init.ready` 永远翻不过来——真机上 agent 在 t=40s 就连通了，面板 240s 里 20 次采样全是 `agent_unavailable`）；现在每轮指标采集（60s）对运行中的域探一次 `guest-ping` 并写进投影，采不到指标时不写（不拿"没采到"冒充"不可用"）。
- **接口：`GET /vms/:id` 说不清"来宾到底能不能用"（2026-11-06）**：此前不返回 `init`，客户端拿到空对象，分不清「正在初始化」与「永远不会就绪（来宾里没有 agent）」。现在返回 `init{ready,state,reason}`，`state` 取 `ready / starting / agent_unavailable / stopped / unknown`；`agent_unavailable` 的说明**同时包含"没启动"与"没安装"两种可能**（控制面投影分不出这两者，写死一种就是误导）。
- **告警：唯一索引只有 `resource_id`，评估器永久冲突重试（2026-11-06）**：`alert` 的唯一索引在模型里只声明了 `resource_id`（迁移是正确三列，两处分叉），于是"同一 id、不同种类"的告警插不进去，评估器查不到就 Create、撞约束就 continue、下一秒重试——真机 126 次/60s 的 `UNIQUE constraint failed`，把 SQLite 单写者占死（连应急改密都拿不到写锁）。索引改回三列的部分唯一口径，写入改为 `OnConflict` upsert（保持"仅当原为 cleared 才重置 first_at/确认信息"的语义），并给"开启中告警量偏大"加一行日志。
- **平台自检不再产生恒定假偏差（2026-11-05）**：节点侧 `platform.check` 此前除端口安全外一律回 `Present=false` +「随 P3 接入」，公网 IP 绑定与端口镜像会被永远报成配置丢失。现在按镜像名精确核对 OVS Mirror 行；无法按 Target 定位的项标记 `Unverifiable`，控制面单独计入 `unverified`、不计入 `drifts` 且生成不了修复项（`API.md` API-344 同步）。补节点侧与控制面两侧单元测试。
- **发布包 agent 版本号注入（2026-11-05）**：`release.yml` 此前只注入控制面版本变量，tag 产物里 agent 心跳与 `agent version` 恒显示 `dev`；补 `-X k_cockpit/internal/nodeagent.buildVersion`（本地以 `v9.9.9-verify` 构建验证通过），并修正 CI 中已过期的「mock 栈」job 名。
- **优雅停机补齐（2026-11-05）**：后台循环与任务队列改用同一个可取消 ctx，`stopBackground` 先取消并等待全部循环退出，再逐个调用自带 `Stop` 的组件，最后 `Queue.Stop()` 等在途任务跑完；此前 `Queue.Stop()` 从未被调用、`scheduler.Stop()` 存在却被注释误判为「没有 Stop」、6/10 个循环无人收尾。
- **CI 覆盖率门禁与权限契约对账（2026-11-05）**：后端 CI 改用 `go test -coverprofile` 并对整体行覆盖设 25% 防下降下限（实测 28.5%，不引入新依赖）；新增 `TestAdminAPIRoutesMatchRouter` 静态比对接口文档的 admin 清单与路由 `adminOnly` 声明，并修正 `GET /network/client-ip` 被误标为管理员、移除已删除的 `dev` 模块标签。
- **文档与配置事实校准（2026-11-05）**：13 份规格状态回填为 `Implemented` / `In Progress` 并新增状态定义与验收勾选口径（164 条 AC 仍按 M5 证据回填，不批量打勾）；8 份实质完整的草稿文档改为生效；`TECH_STACK.md` 的协议「待定」改为 gRPC + Protobuf 并标注未落地的前端选型；`API.md` 两项「规划中」改为「已废弃（由现有接口承接）」；`SECURITY_REVIEW.md` §3 改写为 M5 复查方法与待办状态；修正 `.env.example` 的 `SMTP_USER` 错名、删除 19 个无消费方的死变量并补登记实际读取的变量；`ROADMAP` / `CAPABILITY_MAP` / `README` / `AGENTS.md` / `DEVELOPMENT.md` / `CONTRIBUTING.md` 的过期表述与 TODO 一并回填。
- **引导与初始化口径（2026-11-05）**：创建向导现在把 `init_mode` 写进 `vm` 投影，
  并下发与凭据表同源的 `initial_username`；NoCloud 引导项 `disk` 统一映射为
  libvirt 的 `hd`（此前原样写进域定义会被拒绝）。
- **handler 层测试与迁移编号告警（2026-11-05）**：补 `handler/ops` 的 SSE 视角
  过滤 / 写帧测试与 `handler/network` 的请求映射测试；`cmd/migrate` 启动时对
  重复数字前缀告警（现存 `0018` / `0019` 各两份，历史文件不能改名以免校验和
  失配），并加单测锁住判定规则。
- **编辑页不再"假成功"（2026-11-04）**：节点侧未实现的键（机型、固件、架构、
  热插拔槽位、嵌套虚拟化等）此前会写库并返回成功。现在控制面在受理阶段明确
  拒绝并列出键名，编辑表单下发 `not_applied` 由前端禁用并说明原因；备注/分组
  不再允许从配置接口提交（走元数据接口）。节点侧同时保留兜底校验。
- **引导顺序 disk→hd 映射（2026-11-04）**：界面口径 `disk,cdrom,network` 与域 XML
  的 `hd / cdrom / network / fd` 不是一套取值，旧实现把 `disk` 原样写进定义
  （libvirt 会拒绝）。创建与编辑共用同一处映射。
- **显示设备改为「无」时同步控制台投影（2026-11-04）**：`video_model=none` 现在
  同时把 `display_device` 置为 `none`，详情页不再显示打不开的控制台入口。
- **磁盘限速五项在节点侧从未下发（2026-11-04）**：`vm.config.update` 此前只处理
  `disk_iops_total`，`disk_iops_read/write` 与 `disk_bytes_*` 三档吞吐限值**只写库并返回成功**——
  界面显示"已应用"，磁盘上没有任何变化。现统一翻译为一次 `virsh blkdeviotune` 调用下发
  （字节类字段按 MB/s → bytes/s 换算），运行中的域带 `--live` 与界面「即时生效」的口径对齐，
  关机态只写 `--config`；未提交的项不补 0，避免清掉用户原有设置。补运行态 / 关机态 /
  参数翻译三组单测。
- **agent 第一轮复审补强（2026-11-04）**：
  - 完整克隆 / 重装 / 导出 / 冷迁移的 `qemu-img convert` 与 `cp` 改用 `RunTimeout`，不再被 10s 探测超时截断。
  - OVA 导出改为 VMDK + OVF + SHA1 清单打 tar（`qemu-img` 无 ova 输出格式，旧实现必然失败）。
  - `vm.floppy.apply` / `vm.staticip.change` / `platform.repair` / `vm.nvram.repair` 从假成功改为真实节点动作；控制面补 `rel_path`、`mac`、`bridge_name`、`dhcp_start/end` 与完整修复项。
  - 公网 IP 按 `nat_1to1 / routed / bridged` 分流；交换机迁移补旧上行摘除与目标口冲突校验。
  - 启动恢复遗留 `running → unknown → failed`，指标采集每轮用 `OpNodeDomains` 校正 VM 投影。
  - `capture.fetch` 改为 offset/length 分片流式下载；端口安全补 OVS meter；救援模式修正 boot 顺序。
- **第五轮深检（VNC 控制台全链 + agent 会话层，真机 WS E2E 验证）**：
  - **VNC 对默认配置的域直接不可用**：`virsh vncdisplay` 的裸显示号输出 `:0` 被旧解析
    （`LastIndex > 0` 条件跳过剥离）送进 `Atoi` 必炸——剥离逻辑重写（兼容 `host:N`/`:N`/
    IPv6 字面量）
  - **VNC 空闲会话泄漏**：控制面断开后对侧协程卡在 `conn.Read`（VM 空闲无数据），
    `<-done` 永远等不到、`conn.Close` 排在其后——每次会话泄漏两协程加一 fd。Recv 退出
    立即关 conn
  - **流登记竞态**：StreamData 是独立 RPC，与主通道 StreamOpened(ok) 无顺序保证——旧代码
    收到 ok 后才登记 bridge，RPC 先到即夭折。bridge 提前登记、失败路径统一关桥
  - **io 契约两处**：Write 循环后返回被切空的 `len(p)`（恒 0，违反 Writer 契约，io.Copy
    报错）→ 返回原始长度；Read 丢弃超长 chunk 剩余字节（字节流中段丢失破坏 RFB）→ 剩余缓冲
  - **agent 会话 outbox 阻塞泄漏**：阶段上报/结果回包用阻塞发送，断线窗口（sendPump 已退、
    outbox 满）把执行 goroutine 永久卡死（channel 属已结束会话无人消费）——阶段非阻塞
    （满即弃）、结果 select sendDone 快速放弃；VNC dial 限 3s（防接收循环被卡）
  - 真机验证：原生 WS 客户端全链打通（101 升级 → RFB 3.8 横幅下行 → 客户端版本上行 →
    安全类型列表回执），双向字节流完整

### Fixed
- **第四轮深检（路径安全 + 导出/抓包链路，真机验证）**：
  - **导出删除从未真正删文件（真机实证）**：agent 读 `op.Target`（控制面只发 `file_path`
    参数，恒为空）——`os.Remove("")` 失败被吞，产物在节点上永久残留。改读参数并限定导出目录；
    真机验证删除后文件归零
  - **导出下载必然炸于 ~4MB（真机修复+验证）**：整文件塞一条 gRPC 消息被默认 4MB 收包上限
    拦下，且控制面整段载入内存。agent 侧 `vm.export.fetch` 增加 offset/length 分片（含
    total_size）；控制面 `ExportFile` 改 `io.Reader` 分片流式（8MB/片，内存占用恒为单片），
    handler `SetBodyStream` 直出；gRPC 两端显式 32MB 消息上限。真机下载哈希与节点源一致
  - **用户存储 relPath 逃逸**：`storage.file.commit/read` 的 `../` 可逃出存储根（读写宿主
    任意路径）——目录内包含性校验
  - **抓包网口名拼接**：iface 直接拼进落盘路径（`../../tmp/evil` 可写出 captures 之外）——
    agent 拒绝非安全字符；控制面同规则前置校验（真机实证：放行会得到一条永远 running 的
    记录）。合法网口抓包 E2E 通过（tcpdump 落盘、状态 ready）
  - **分区操作设备守卫**：`device_id` 非 `/dev/` 前缀直接拒绝（裸字符串进 parted 是任意
    路径操作面）
  - 分片 reader 无 total 时的死循环守卫（旧版 agent/mock 兼容：首片即 EOF）

### Added
- **P5 第二批：迁移三模式 + takeover 完整重建 + 来宾自动化（真机双节点端到端验收）**：
  - **迁移三模式**：offline（停机搬运）与 warm（`virsh save` 落内存现场 → 搬运 → 目标侧
    define + `virsh restore --xml` 恢复**运行现场不重启**，停机窗口≈传输时长）落地；live
    **诚实阻断**——需要节点间 libvirt 互信（qemu+ssh/tcp），与「控制面不持有宿主机凭据」
    （ADR-0005）冲突，待节点间通道定型后接入。控制面运行态自动选 warm（预检文案同步温迁移语义）
  - **takeover 完整重建**：staging 文件幂等落位（暂存缺失但目标已在位=已落位）、XML 路径
    重映射、NVRAM 放回、同名域按 **UUID 比对**处理（相同=源侧遗留可移除，不同=目标真有
    同名机器拒绝覆盖）、温迁移 define-then-restore（libvirt 10 的 restore 无 --persistent
    选项——先 define 再 restore 才能既在跑又持久；瞬态域宿主机重启即丢，真机实证）
  - **重试闭环**：源侧 staging 落 domain.xml + manifest.json，域已移除的重试走快速路径重发
    清单（温迁移内存态保留不降级）；takeover 幂等放置；控制面 unknown 状态 + 存在失败迁移
    记录时放行重试
  - **源侧数据在目标确认前不删**的边界细化：域定义保留到 cleanup（记录落库成功之后）才移除
    ——提前 undefine 会让重试的状态探测落空且失败窗口内两侧皆无定义；cleanup best-effort，
    失败只留日志绝不把成功迁移判成失败
  - **来宾自动化**：password_online（guest-set-user-password）、expand_disk（qemu-img/blockresize
    + guest-exec growpart/resize2fs/xfs_growfs）、disk_attach（建盘+attach+来宾分区）、
    password_offline 依赖 libguestfs（未装如实报）；guest-exec 带 500ms×60 轮询收结果
  - 真机验收（同机双 agent，第二个 agent 跑私有 mount namespace：镜像目录私有、staging 共享，
    精确模拟真实双节点+控制面中转已回放）：**温迁移全链成功**（save→staging→落位→UUID 守卫→
    define+restore→记录落库→源清理，迁移单 Moved 清单完整、盘只在目标、源/暂存清零）、
    **offline 全链成功**、**失败→重试链路实测**、live 阻断与 guest 无 agent 诚实拒绝验证。
    已知同机伪影（真双节点不存在）：共享 libvirtd 使 cleanup 的 undefine/nvram 清理波及目标侧
  - 修复连带缺陷：vmStatus 对英文 locale「failed to get domain」不识别（域不存在被当成探测失败）

### Fixed
- **第三轮收口（2026-10-26 补）**：
  - **快照 kind 记录错位**：受理时的 kind 是预测值，UEFI 域被 libvirt 强制走外部快照（--disk-only），关机 UEFI 建的快照记录 internal、实际是外部 overlay（真机实证）。agent 改为如实回报实际执行的种类，控制面按结果回写。真机复验：kind=external 且与 overlay 文件一致
  - **删除虚拟机残留 backing 链（真机实证）**：快照/恢复后域指向 overlay，原盘不在域 XML 里，删除报成功但 <vm>.qcow2 留在宿主机。沿 backing 链补删——**先探链再删**（首版先删后探，qemu-img 对已删文件读不到 backing）；只收文件名以本机名开头的文件，linked clone 的模板盘绝不顺链误删。真机「建→快照→删」零残留复验

### Fixed
- **第三轮深检（2026-10-26，同环境；安全组/端口安全/端口转发/公网 IP/双层防火墙回滚五链路真机验收）**：
  - **端口安全整体不可用 + 停用不删流**：控制面 Apply 依赖 agent 的 `port_security.precheck`，agent 从未实现——功能经 API 完全不可用。补实现（OVS/ofctl/端口挂载三项能力探测 + 将写入流预览 + 既有流覆盖提示）；apply 改「先清该口流再按需写入」的期望状态语义（停用=纯清理，此前只加不删，停用后防伪造/隔离流全残留）；隔离与防伪造同优先级双流的歧义改为互斥
  - **端口转发 add/remove 不对称（真机实证）**：remove 只摘 DNAT，add 写入的两条 FORWARD 放行永久残留——对称删除。真机 add→remove 全清复验
  - **紧急回滚只清一层（真机实证）**：宿主机防火墙回滚复用节点防火墙的 rollback（只清 KC-FW），KC-HFW 六条规则与 INPUT 跳板全部残留——回滚清两层并恢复 ACCEPT 策略。真机复验归零
  - **公网 IP 绑定假成功（真机实证）**：控制面 executor 只发 vm_name 从不发 vm_ip，agent 按空 vm_ip 拼 DNAT、iptables 报错被吞——bind 报成功规则 0 条。改为节点侧解析（domifaddr→该机自己的 vnet 邻居），解析不到（关机/无地址）如实报错；unbind 时 VM 可能已关机，按字段级形状匹配从 iptables-save 删除（iptables-save 会把地址规范化成 ip/32，子串匹配会漏）
  - **vmIP 邻居兜底串号**：返回第一个 vnet 邻居的 IP，多虚拟机宿主机上把别的机器的 IP 当这台的（安全组与公网 DNAT 都会绑错对象）——限定该机自己的 vnet 口
  - 抓包任务报「抓包完成」+0 字节（实际进行中）——改为「进行中，约 N 秒后可取回」；vswitch create 的重复 -C 冗余清理

### Fixed
- **第二轮深检 + 真机扩展验证（2026-10-26，同一测试环境；静态深挖 + 快照/看门狗/防火墙/交换机/配置/导出六条链路真机复验）**：
  - **安全组规则空转（高危）**：`KC-SG-*` 链只建不挂，FORWARD 里没有任何跳板——规则全部就位但零流量经过。挂载幂等接入 FORWARD（-d/-s 各一条）；解析不到 VM IP 时**不挂**（兜底网段跳板会把全宿主机转发流量引进带 DROP 链尾的链）。真机验证兜底行为与挂载语法/幂等
  - **防火墙词表/键位错位（高危，真机抓出）**：agent 按 `allow/port/source` 读取而控制面发 `accept/port_start/source_cidr`——「放行 443」落成 `-p tcp -j DROP`（无端口无来源，真机 iptables -S 实证）；`default_action=deny` 只认 `drop` 静默不收紧。按真实下发形状对齐，真机复验规则精确落成
  - **hostfirewall 同族**：`KC-HFW` 完全不读 rules（自定义与保护规则静默丢弃）、whitelist 逗号字符串按 `[]any` 断言恒失败。补齐渲染与两种白名单形态
  - **DNAT 全族加不上（真机同族排查）**：端口转发与公网 IP 的 nat 规则缺 PREROUTING 链名且 `-t nat` 位置错误（`-C` 把 "-t" 当链名）——新增 `iptablesNatAdd/Del`（`-t nat` 打头 + 链显式），真机验证
  - **dnsmasq 生命周期两缺口（真机实证）**：前台模式（--no-daemon）**不写 pid 文件**，stop 路径一直空转成孤儿——改由 agent 落盘 PID；杀进程前核验 `/proc/<pid>/comm`（PID 复用防护）。交换机删除不拆 MASQUERADE（delete 指令补 spec.cidr）。真机全生命周期复验：pid 文件在→删→进程停+NAT 清+桥删
  - **口令审计假阳性**：用户名叫 root/admin 的账号无论真实密码如何都被报「口令与用户名相同」（`cand == username` 未做 crypt 校验直接记命中）——改为真实比哈希
  - **镜像看门狗双缺口**：节点定时器在内存、agent 重启即丢（镜像复制的是全量流量，残留是隐私问题）——到期时刻落 /run、启动时补臂（已过期立即补撤销）；控制面 `portmirror/bridge.Reconcile` 写好却无任何调用方——新增 `service/network/reconcile` 周期循环接入。真机验证：60s 窗口内重启 agent → 到期自动撤销 → 控制面一分钟内对账
  - **路径包含性**：vmShareMount 前缀绕过（`userdata-evil` 同前缀目录通过 `HasPrefix`）；CDROM ISO 的 rel_path 带 `../` 可逃出存储根——均改为「目录内」判定
  - **/tmp 固定文件名碰撞**：`kc-redefine.xml` 与 `kc-cdrom-<dev>.xml` 不带域名，并发操作互相覆盖——均带上目标名；agent 侧补 username 字符集守卫（防校验上线前的老账号把 drop-in 写到目录外）
  - **内存扩容静默失败（真机抓出）**：`virsh setmem` 不能超过最大内存——扩容先 `setmaxmem` 抬天花板。真机 512MB→2048MB 复验生效
  - 真机另证伪一项预判：UEFI 外部快照的 `snapshot-revert` 在 libvirt 10 上**可用且语义正确**（丢弃快照后 overlay、在快照点新建 overlay）；遗留小口径：快照 API 的 `kind` 恒显示 internal（实际外部机制）

### Fixed
- **Review 修复批次 + 真机复验（2026-10-22，Ubuntu 24.04 arm64 真机全链路验收，详见 [f-6-08](docs/07-specs/f-6-08-agent.md) 与 [TROUBLESHOOTING §7](docs/04-engineering/TROUBLESHOOTING.md)）**：
  - **端口镜像 enable 恒失败/重复建行**：`ensureMirror` 的「先查后建」条件写成了恒真，且 create 带了 ovs-vsctl 不支持的 `--may-exist`。真机进一步抓出三种错误事务形状（create 无 `--id` 同事务不可引用、引用列值不能写端口名、命名 UUID 不能作 record）——按 OVS 3.3 实测语义重写：先查后建、引用列经 `get Port` 换命名 UUID、撤销走摘挂（弱引用 GC 顺带删行，挂载中的 `destroy` 静默无效）。真机验收：enable/confirm/disable/重建/stale 行重 enable 全通过，不产生重复行、不误杀同桥兄弟镜像
  - **AppArmor 放行只对首个目录生效**：幂等判定是「文件里出现过 kc.images 标记」而非「该目录已放行」——首个放行目录会吞掉后续所有目录（ISO 的 userdata、快照链的其它池目录永远写不进）。改为按目录标记幂等；真机验证旧条目保留、新目录写入、开机零拒绝
  - **agent systemd 单元静默吞掉 /etc 写入（部署层）**：`ProtectSystem=full` 使 /etc 只读，AppArmor 放行条目、SSH drop-in、fstab 写入全部被只读层挡掉且被调用侧吞错。单元按真实写点加最小 `ReadWritePaths` 豁免（`-/etc/apparmor.d/abstractions -/etc/ssh/sshd_config.d /etc/fstab`）——**已部署的节点需要同步更新单元文件并 `daemon-reload`**
  - **SSH 开关解禁不 reload**：删除 DenyUsers drop-in 后不重载 sshd，内存里的拒绝规则依旧生效（「已允许却登不上」）。改为双向 reload，服务名按 ssh→sshd 探测回退（不再硬编码 Debian 系名）。真机验证 `sshd -T` 立即生效
  - **SSH 开关多节点误报**：任一节点不可达会把已成功节点遮蔽成「整批不可达」。改为独立计数，部分成功如实带出
  - **模拟节点预置从未生效**：`EnsureSimulated` 的入参 `names` 被同名局部变量遮蔽成「现有节点名」，一台都预置不出来（repository 重构引入，`TestEnsureSimulated` 长期失败）。修复后测试转绿
  - **vmCreate 数据盘失败回滚不清已建盘**：只删系统盘，先建成功的数据盘成孤儿文件。回滚清全部已建盘
  - **firewallApply 无协议时生成非法规则**：无 `-p` 却带 `--dport`，iptables 报错被静默吞掉、规则缺失。无协议时忽略端口
  - **删除两处死代码**：模板默认值的服务端填充块（入口强校验后不可达，DefaultCPU/DefaultMemoryMB 实际由前端向导预填）；弱口令审计里恒假的候选数守卫
  - **重装/救援如实性**：重装 define 失败文案补「域定义已移除、原盘已还原」的状态说明；救援 docstring 与实现对齐（不改写引导顺序）

### Fixed
- **全栈真机深测发现的六个缺陷修复（分支 `feat/agent`，测试环境 Ubuntu 24.04 arm64 + libvirt 10 + OVS 3.3.9 + QVMConsole 共存机）**：
  - **force-delete 永远残留域（高危）**：`virsh undefine` 没有 `--force` 选项（命令必然失败且被 force 分支吞掉），任务报 success 但域留在虚拟化层、磁盘被删——同名重建被「已存在同名域」拒绝且无从下手。真机两次复现。修复：改用 `--nvram --managed-save`，带快照的关机域追加 `--snapshots-metadata`（libvirt 拒绝删除带快照的关机域——僵尸域的常见形态），undefine 失败**如实报错**，末尾真实复核域已不存在
  - **创建规格无上限校验（高危）**：`validateCreateConfig` 刻意跳过规格三件套（模板会抬高下限），但上限无人校验——`disk_gb=999999` 一路直通，真机落成 977 TiB 稀疏盘。修复：模板推导后按编辑矩阵 Max 校验（盘 2000GB / vCPU 64 / 内存 256GB）
  - **失败创建的幂等键永久占据名字（中）**：队列对重复幂等键无条件返回既有任务（含已失败的），同名重试拿到 200+旧失败任务，名字被挡到 24h 保留清理后。修复：既有任务已是终态时释放其键、入队新任务（附回归测试）
  - **关机态快照命令非法（中）**：`--quiesce=off` 不是合法 virsh 语法（quiesce 是无值开关），关机域快照 100% 失败。修复：关机或不含内存统一走 `--disk-only` 外部快照
  - **外部快照链 qemu 打不开盘（中）**：两层根因——恢复新建的 overlay 是 root:600（libvirt 不经自己启动路径不改属主）；libvirt 动态 AppArmor profile 不为 /var/lib/libvirt 之外的镜像路径生成放行（QVMConsole pitfalls 同款）。修复：新增 `qemuowner.go`——qemu 属主探测+回退（libvirt 运行目录 → getent）归一属主与 0644；镜像目录幂等写入 `abstractions/libvirt-qemu`（每个域 profile 都 include 它）。修复后快照→恢复→再开机全链路通过
  - **快照恢复后投影不刷新**（观察项，未改）：外部快照恢复的成功路径把 VM 状态记为 unknown——语义保守（回滚后状态本不可信），但界面显示可优化
- passaudit 的 NodeID 写死 0（mock 时代遗留）导致真实通道下永远「节点不可达」——改为取第一个已接入节点（Pluck 修正 Scan 单列问题）

### Added
- **真实 agent P3~P5：全部领域执行器接入（分支 `feat/agent`，能力自报 85 项）**：
  - **P3 网络**：VPC 交换机（OVS 桥 + internal 网关 + 独立 dnsmasq + NAT，create/update/delete/reconfigure/release_port）、Linux 基础桥、上行口入桥（**节点侧看门狗**先布防再操作，confirm 解除）、收敛修复、安全组（per-VM iptables 链）、双层防火墙（KC-FW/KC-HFW 链，先写入后收紧；rollback 全通恢复）、端口转发 DNAT、公网地址 1:1 DNAT、端口安全（OVS 防伪造流表）、端口镜像（OVS mirror + 看门狗）、抓包（tcpdump 落盘）
  - **P4 存储**：池格式化挂载（wipefs→mkfs→mount→fstab nofail）、删除/配置/卸载（数据保留）、trim（fstrim）、分区增删（parted）、LVM 卷（vgcreate/lvcreate）、用户存储文件提交（SHA-256 校验）与读取、9p 共享目录（realpath 越界拒绝）
  - **P5 长尾**：冷迁移（源侧停机+磁盘清单回传）、重装（备份→模板重建→失败自动还原备份）、救援（配置快照保存/还原）、导出（qemu-img convert）、口令审计（读 /etc/shadow + crypt(3) 比对弱口令，真机 36 账号验证）、SSH 访问开关（sshd_config.d drop-in + 踢在线会话）、配额处置（FORWARD 链按用户 IP DROP）
  - **控制面修复**：passaudit 的 NodeID 写死 0（mock 时代遗留）导致真实通道下永远「节点不可达」——改为取第一个已接入节点（Pluck 修正 Scan 单列问题）
  - 每批执行器注册即能力自报（85 项），hub 解码表同步登记全部键；真机 agent（Ubuntu 24.04 arm64）已升级部署并通过口令审计/OVS 状态/节点在线三链路验收

### Added
- **真实 agent P0：通道骨架上线（分支 `feat/agent`，[f-6-08](docs/07-specs/f-6-08-agent.md)）**——控制面与节点的通信从 mock 假执行走向真执行的第一步，按 f-6-01 §5.2 定稿实现（gRPC over mTLS 反向长连接 + protobuf，[`internal/agentproto`](internal/agentproto/agent.proto) 为线上契约）：
  - **控制面网关**（`internal/agent/hub`）：私有 CA 与服务端证书自举；一次性令牌注册并签发客户端证书（注册业务复用 node 域，经 `Registrar` 接口延迟解析避免装配循环）；连接注册表把 `agent.Client.Execute` 路由到对应节点（阶段透传任务时间线）；心跳落库，Snapshot 从 DB 心跳推导。`AGENT_TRANSPORT=grpc` 启用（`:8443`，PKI 落 `agent-pki/`），mock 分支原样保留
  - **agent 二进制**（`cmd/agent` + `internal/nodeagent`）：`enroll` 换证书（enroll TLS 信任锚=令牌本身，成功后材料原子落盘 `/var/lib/k_cockpit-agent`）；`run` 常驻（首帧心跳 + 10s 周期；断线指数退避 1s×1.6 上限 60s±20%，稳定会话 30s 归零）；执行器注册表与能力自报同源；未实现操作返回明确失败的 Result（诚实的能力边界）
  - **测试**：hub↔nodeagent 真实 TLS 全链路集成测试（enroll→上线→Execute 阶段/结果→断线未送达语义 + 无证书 Channel 拒绝）+ 退避/解码单测；mock 栈 E2E 5/5 不受影响
  - **真机验收（Ubuntu 24.04 arm64 + libvirt 10 + OVS 3.3.9）**：面板注册令牌 → 真机 enroll → 节点 online（版本/协议号正确、心跳 10s 周期推进）；杀进程 45s 内 offline；`systemctl restart` 自动恢复 online；生产形态 root systemd 服务部署通过（[`deploy/agent/`](deploy/agent/k_cockpit-agent.service)）
  - AGENTS.md 协议行从「待定」落定为 gRPC/mTLS

### Changed
- **internal 按层重组（[ADR-0012](docs/06-decisions/0012-layered-internal-structure.md)，取代 ADR-0010 的顶级组织方式）**：顶级目录 = 层（`router` 路由层 / `handler` 接口层 / `service` 服务层 / `repository` 数据库访问层 / `model` 数据库层 / `platform` 基础设施 / `agent` 契约），业务域沉为 service 与 repository 的二级目录。纯机械迁移（git mv + 全仓 import 重写，约 320 文件，零逻辑改动）：五个域目录与 platform 的 12 个业务服务包迁入 `service/`，platform 瘦身为纯基础设施（api / audit 写侧 / authz / config / cryptoutil / database / logging / version）
  - **路由层按域拆分**：router.go（1112 行）拆为入口 + `routes_{platform,node,compute,network,storage,ops}.go`，342 条路由逐条迁移；coverage 对账测试改为扫描整个 router 包
  - **设立 repository 数据库访问层**：`repository/node` 为范式样本（服务不再执行任何 GORM 查询，gorm 仅用于哨兵错误翻译），其余域按 CODING_STANDARDS 第 13 节规范分批迁入；**新代码一律走 repo**
  - **cmd/server 装配按域拆分**：app struct 按域分组（platform / node / storage / network / compute / ops / bg），装配代码拆到 `wire_*.go`，main.go 只剩生命周期与 routerDeps 纯映射
- **修复 `/api-endpoints` 空清单的存量回归**：`NewAPIDocs` 构造时快照 `h.Routes()`，而它一直被构造于 v1 路由注册之前——接口文档页只能看到 `/health`。现移到全部注册完成之后构造
- **修复 DEPLOYMENT.md 的 ldflags 老路径**：`internal/version.*`（ADR-0010 之前的位置）修正为 `internal/platform/version.*`，照旧文档构建会得到无版本号的二进制
- **界面视觉升级：全新配色与主题风格**（设计令牌全表见 [`FRONTEND.md`](docs/02-architecture/FRONTEND.md) §4.2）：
  - **品牌色换为靛蓝一族**（浅 `#4F46E5` / 深 `#8B93FA`），语义色整体换到对应现代色阶；暗色主题底色更深一层、描边改用白色低透明度（随下层表面自适应）；圆角控件 6→8 / 卡片与弹层 10→12；阴影改为双层柔和结构
  - **新增令牌**：`--kc-on-brand` / `--kc-on-danger`（实底按钮上的文字色）——暗色主题下亮色实底配白字达不到 AA（旧版品牌色 `#3B9EFF` + 白字仅 2.6:1），亮底深字是现代面板的通行做法；`--kc-bg-console`（VNC/SPICE 视口恒暗，不随主题切换），消掉控制台页面残留的硬编码色值
  - **修两个存量视觉缺陷**：时序图表引用了不存在的 `--color-primary` 变量，品牌色从未生效、一直走硬编码回退蓝；输入框聚焦出现「外圈 + 边框」双层——`base.css` 的 `:focus-visible` 是未分层规则、永远压过 utilities 层，Input 组件声明的 `focus:outline-none` 从未生效（移入 `@layer base` 修正）
  - **悬停可见性**：表格行 / 菜单项 / 次要按钮的悬停底色从 `bg-raised`（浅色下与卡片同为白色，悬停不可见）改为 `bg-sunken`（两套主题下都成立），共 21 处
  - **动效系统落地**（FRONTEND §4.6 的规格首次实现）：动效令牌三档时长 + 单一缓动；按钮按压下潜、输入框边框平滑聚焦、页签/分段器/命令面板行色彩过渡、侧栏分组平滑折叠（`grid-template-rows` 过渡）、页面切换内容上浮入场、常驻任务栏滑入；弹窗与命令面板补齐进出场动画（退出播完 240ms 再卸载，规格里的硬性要求）；主题切换瞬间全局平滑变色（临时过渡类，平时零开销）。全部动效经 `prefers-reduced-motion` 全局兜底自动关闭
  - **对标 QVMConsole 实测后的四项手感补齐**（实测结论见会话记录；吸收手法而非样式）：①hero CTA 变体——品牌渐变底 + hover 光晕增强，用于页面级主操作（创建虚拟机 / 接入节点 / 创建模板，每个视图至多一处），新增 `--kc-brand-grad-*` 与 `--kc-shadow-glow` 令牌；②运行中状态点 2s 呼吸正式启用——语义收敛到 `utils/labels.ts` 的 `*_STATUS_PULSE`（只给运行中/执行中/在线，终态绝不呼吸），覆盖 VM/任务/节点全部 9 处徽章；③表格行悬停补 `transition-colors`（8 处，此前是瞬变）；④键盘焦点环加品牌色微光晕
  - **登录页品牌面板**（宽屏左侧）：自撰的品牌标题（渐变点缀）+ 四个能力点 + 两张悬浮示例卡（往复漂浮、绿点呼吸）+ 令牌派生的背景光斑——装饰性元素按 §4.1 的边界只落在非工作区；窄屏不显示，多阶段登录流程不受影响；浅色底色微调向冷紫蓝 `#F4F6FB`、阴影再柔半档，浅色主题更透气
  - 全部令牌值经 WCAG AA 对比度脚本核对通过（两套主题 × 正文 / 链接 / 徽章淡底 / 实底按钮文字）

### Added
- **对标 QVMConsole 的能力差距收口**（控制面 + 前端，agent 侧按既有 mock 模式模拟；差距核对结论：脏页分级处置、每核 CPU/内存条硬件详情、端口转发来源白名单本体在核对中发现**已实现**，未重复开发）：
  - **存量虚拟机纳管（API-390 / API-391）**：节点详情页「虚拟机」页签新增「存量虚拟机（未纳管）」区块（仅管理员）——扫描节点上面板之外的 libvirt 域，一键纳管：只补记录（规格取自域定义、UUID 由节点回传作对账键），不改动域本身。新 agent 操作 `node.domains` / `vm.adopt`，mock 预置两台演示域（`legacy-ubuntu-1804` / `bare-win2022`）；mock **不过滤已纳管域**（真实节点返回全部域，过滤是控制面按库做的，mock 里过滤会让重复纳管的 409 路径永远走不到）
  - **强制删除兜底（API-389）**：删除弹窗新增管理员可见的「强制删除（兜底）」选项——跳过状态探测（僵尸机正是查不出状态才删不掉），节点直接删域定义并重启 libvirt。新 risk 动作 `vm.force_delete`（二次验证）；锁与在途任务仍然拦；mock 下任务时间线呈现与常规删除不同的四步（删定义 → 清残留 → 重启 libvirt → 复核）
  - **控制台截图（API-032，原 503 stub）**：`vm.console.frame` 增加 `mode=screenshot` 档（全幅），详情页「控制台」页签新增「截取画面」；mock 生成 1280×800 与预览缩略图可区分
  - **控制台发送登录密码**：控制台工具条新增「发送登录密码」——取初始凭据（服务端每次读取记审计）逐字符发到远端，登录来宾系统不再需要切窗口抄密码；工具条文本发送逻辑收敛为共用函数
  - **端口转发白名单快速填充**：新增转发表单「填入我的 IP」（复用 `/network/client-ip`，反代场景展示两者的选择提示）；该接口从仅管理员放开为登录可用（返回的是请求者自己的地址，无信息面扩大）
- **开发环境自动预置整套演示数据**（`internal/devdata`，mock 运输层 + development 双重闸门）：3 台模拟节点（`dev-node-1/2/3`，走真实签发令牌 → 注册链路，与真实 agent 同一段逻辑）+ 每节点的默认存储池、系统基础网络、3 个演示镜像（Ubuntu 24.04 / Debian 12 / Win11，带识别出的系统信息与最小磁盘）+ 1 个可直接克隆的就绪模板。此前 fresh 环境里创建向导的前置条件链三处断裂（无存储池 / 无网络 / 无镜像），「没有地方放入镜像」就是这么来的。全部幂等，生产永不进入，失败只降级告警
- **mock 电源生命周期最小状态**（[ADR-0011](docs/06-decisions/0011-mock-power-state.md)，修订 ADR-0007 的实现口径）：mock 维护一张电源状态表（create/克隆 → stopped，start → running，shutdown/poweroff → stopped），`vm.status` 探测读表。此前探测恒返 running——刚创建（投影「已关机」）的虚拟机点开机会被「已是运行中」拒绝，「先关机再开机」永远走不通。创建/克隆执行器现在消费结果里的状态（新建虚拟机正确显示「已关机」而不是「状态未知」）；服务重启后用数据库投影给 mock 注水，跨重启无缝衔接。仍然不做：耗时、失败注入、离线等行为模拟
- **PRD 逐域核对补齐的六个控制面缺口**（页面 + 服务端功能面收口的最后一批）：
  - **API 响应 `no-store`（F-10-04）**：`/api/*` 与 `/health` 一律不缓存——接口返回的是此刻的状态，被中间层缓存后用户看到过去时；静态资源（SPA 产物）不受影响
  - **超长请求行拦截（F-10-03）**：路径 >1KB / 查询串 >2KB 在进业务之前拒绝（取值给正常请求留了十倍余量）
  - **配额超限处置第三档「关机」（F-8-06）**：限速 / 断网之外新增逐台优雅关机（只关超额用户在该节点上的运行中机器）；首次进入处置态触发一次（边沿判定），恢复仍走周期重置或人工重置
  - **超限与弱口令的邮件通知（F-8-06 / F-10-05）**：配额首次处置通知用户本人（维度 / 用量 / 上限 / 处置方式 / 恢复途径）；口令检查命中通知命中者。只发**已验证**邮箱、SMTP 未配置只记日志——通知是附属品，不反噬处置与检查本身
  - **Windows 初始化就绪后自动弹出安装介质（F-2-17）**：新列 `vm.media_auto_eject`（迁移 0050，创建时按 configdrive + 安装 ISO 置位）；弹出循环每分钟扫"欠一次弹出"的运行中机器，新 agent 操作 `vm.init.ready`（真实实现读来宾日志的 cloudbase-init 完成标记，mock 恒就绪）判定就绪后走既有光驱弹出路径（先清标记再入队，入队失败下一轮幂等重试）
  - **工作台非负载告警（F-8-03）**：状态横幅新增运维自检项——未配置 SMTP（找回密码与邀请邮件发不出）、未配置站点对外地址（邀请链接站外打不开）；仅管理员可见

### Fixed
- **清理两处过时的「尚未建模」注释**：编辑表单与创建向导的「硬件直通」子页说明仍称 PCI 直通未建模，而直通域（`host/passthrough` + `vms/:id/passthrough`）早已完整实现；「网口」说明同样滞后（网口增删改已在「网络管理」页签提供）。两处改为指向真实入口
- **`storage_pool` 默认池唯一索引的模型漂移**：迁移 0001 建的是 `(node_id) WHERE is_default`（每节点至多一个默认池，R-006），模型 gorm 标签却是 `(is_default) WHERE is_default`——全局唯一。生产库（走 SQL 迁移）无恙，但所有 AutoMigrate 建的库（测试 / E2E / 预览）里**第二台节点的默认池永远建不出来**，报一个看不懂的全局唯一冲突。标签补上 node_id 并注明 is_default 只出现在 WHERE 子句的语义；预置流程对唯一冲突幂等容忍
- **弹窗永远打不开的回归**：Modal / CommandPalette 的挂载同步用了「渲染期调整 state」模式，在真实浏览器（React 19 + StrictMode）下 `open` 翻 true 后对话框永不出现、也无任何报错——「创建虚拟机」等所有弹窗入口点了没反应。jsdom 单测从未覆盖「关闭→打开」翻转路径所以全绿漏检（真实用户首发即命中）。改回 effect 驱动的挂载同步（对 oxlint 的 `set-state-in-effect` 优化提示做文件级豁免并注明原因）；补两层守卫防再犯：单测新增「关闭→打开」翻转用例，E2E 冒烟新增「创建虚拟机向导可打开」真实浏览器用例
- **工作台自检提示从未真正展示（F-8-03）**：`Summary` 先把管理员的非负载自检提示（未配置 SMTP / 站点对外地址）放入 `Alerts`，末尾又被 `out.Alerts = buildAlerts(...)` 整体赋值覆盖——自检横幅一次也没出现过。改为追加（自检提示在前、负载告警在后），租户视角不受影响
- **请求过滤中间件的链穿透隐患**：Hertz 的 `Next` 是循环实现，middleware 里只 `return` 不阻断后续 handler（会覆盖状态码）——三处拦截分支补 `c.Abort()`（与 auth 中间件同一写法）。此前"扫描器拦截返回 403"其实依赖未命中路由的 404 兜底，命中路由的拦截会被业务 handler 覆盖成 200
- **模型对齐检查的盲区**：`model_alignment_test` / `index_alignment_test` 各自维护的模型子清单缺了 15 个后来加的模型（Alert、ComputeQuota、UserInvite、VMTag 等），从未被"模型 vs SQL 迁移"核对。清单收敛到 `database.AllModels()` 单一来源后全量核对：列级无漂移；索引级抓到一处真实缺口——**`user_invite` 的令牌唯一索引只在迁移里、模型未声明**，AutoMigrate 建的测试库因此没有这条约束（补 `uniqueIndex` tag 对齐）。另加一道源码扫描测试，断言 model 包每个 `TableName()` 都登记进 `AllModels()`——新模型忘了登记会在 CI 先红，而不是等冒烟栈运行期报 no such table

### Added
- **Playwright E2E 冒烟**（ADR-0006 规划落地）：`pnpm --dir web test:e2e` 一条命令自拉起独立控制面（临时 SQLite + mock + `cmd/e2e-init` 按模型建库——SQL 迁移是 PG 专用，冒烟栈不走）与 vite，覆盖「面板从零到能用」的最短链路：真实令牌初始化（ADR-0008 流程）、错误密码拒绝、管理员登录（含首次安全引导的跳过）、虚拟机列表导航。CI 新增 e2e job；创建 / 电源 / 迁移的全量覆盖仍按口径随 M5
- `database.AllModels()` / `BuildTestSchema()`：全套表模型的显式清单（按 `TableName()` 枚举 61 个），仅供冒烟与测试建库——生产建库入口仍是 SQL 迁移

### Changed
- **internal 按业务域分组**（[ADR-0010](docs/06-decisions/0010-group-internal-by-domain.md)）：58 个平铺包归入 `platform / compute / network / storage / ops` 五个域目录 + `node / agent / model` 顶层包；handler 按同名六域拆分子包；消歧改名 `network→vswitch`、`networkbridge→bridge`、`storage→pool`、`schedule→cron`。纯目录与命名迁移（`git mv` + import 更新，约 200 个文件），零逻辑改动，每域一个 commit、全量测试护航
- 迁移中验证并否决两个子项：**auditlog 并入 audit** 被 `audit→authz→auth→audit` 依赖环否决（审计写侧必须比 auth 更底层，读写两包保留）；**端口转发/静态地址从 vm 抽到 network 域** 因深耦合否决（与 vm 拆包同批、随真实 agent 落地处理）

### Fixed
- **API 文档页认证误标**：mock 专用的 `POST /dev/agent-register`（仅 `AGENT_TRANSPORT=mock` 时注册）未在公开路由清单里，文档页把它标成"需登录"；顺带为它补了说明性摘要
- **迁移审计条目补记方式**（live / offline）——两条路的失败模式完全不同，排查"那次走哪条路"不必再回查任务参数
- 迁移目标清单补服务层测试（容量聚合 / 维护与冲突的不可用原因 / 建议目标唯一 / 租户归属 404）

### Changed
- 文档债清理：`AGENTS.md` 删除过期的"前端尚未创建"重复块并补 `pnpm test`（目录树补 `internal/maintenance`）；`DEMO_PLAN` G-35 与 Demo-3 的阈值口径与实现对齐（≥80% 限流、≥100% 阻断，原脚本误写 0.50）；CHANGELOG 0.1.0 内五处"未做 / 未覆盖"的过期表述补注后续批次的落地条目

## [0.1.0] - 2026-09-28

首个里程碑版本：控制面功能面完整（对标 QVMConsole 四轮差距全部收口），
**节点代理仍为 mock**（[ADR-0007](docs/06-decisions/0007-mock-agent-first.md)）——
可演示、可联调前端，不能真实管理宿主机。部署与升级见
[`docs/04-engineering/DEPLOYMENT.md`](docs/04-engineering/DEPLOYMENT.md)。

### Added
- **跨节点迁移补全（F-2-15 / F-6-03 / F-6-04）**
  - **热迁移**：运行中的虚拟机不再被"先关机"拦下——方式由实时状态决定（运行中→热迁移、已关机→停机迁移），迁移记录落 `mode` 列（迁移 0049）。预检给出**量化收敛结论**：脏页速率 ÷ 实测带宽的比值，≥100% 直接阻断（追不上）、≥80% 自动开启 CPU 限流（auto-converge）、评估不出则如实说明；受理时**复核一次**（负载在预检与提交之间会漂）
  - **目标侧接管**：新增 agent 操作 `vm.migrate.takeover`——数据到位只是迁移的一半，固件变量（NVRAM）、网络绑定这些目标侧本地的补齐由**目标 agent** 执行，时序在"源侧成功之后、控制面改记录之前"，接管失败时源侧保留、可重试
  - **迁移目标清单**：`GET /vms/:id/migrate-targets` 按节点聚合容量（来自最近一次周期采样而非实时探测）、节点内资源冲突与可否作为目标，并标出**余量最大的建议目标**（只是建议——放哪儿是人的决定）。前端迁移弹窗换用该接口
- **磁盘热插拔的槽位降级（F-2-06 收尾）**：q35 机型 PCIe 根端口耗尽时（此前表现为"挂载成功但设备不出现"），挂载自动改走 virtio-scsi 控制器（一块控制器只占一个槽位）；机型不支持热插拔（i440FX）不降级——那是换机型的问题
- **持续集成与前端测试**（工程欠账收口）
  - GitHub Actions 工作流（`.github/workflows/ci.yml`）：后端 gofmt / vet / test 与前端 typecheck / lint / test / build 两job 并行，命令与 AGENTS.md §3 完全一致
  - Vitest 测试基建落地（此前前端测试为 0）：请求层核心链路（统一解包、401 登出、**428 二次验证后自动重放**）、通用件（Modal 的 Esc 与不可关闭语义）、展示层工具共 16 个用例
  - **顺带修了一个测试抓到的真缺陷**：428 验证通过后重放仍被 428 拒绝时，"验证状态已失效"的友好文案是死代码（重放在 catch 块内执行，异常直接穿透）——用户刚验证过却再次看到"需验证"，会以为验证坏了而反复重试
- **部署与发版文档**：`docs/04-engineering/DEPLOYMENT.md`——控制面构建、systemd 安装、迁移与升级流程、回滚口径（二进制可退、改写结构的迁移依赖备份）、上线检查清单
- **外部身份源定论（F-1-13）**：[ADR-0009](docs/06-decisions/0009-defer-external-identity.md) **本期不做**——自建自用形态下本地账号 + TOTP + 邮箱找回已闭环，外部 IdP 的收益不抵攻击面，且开发环境无真实 IdP 可验证；多组织共享或强制身份集中管理时重新评估

- **第四轮差距收尾（G-42 ~ G-54，能力闭环与运营期旋钮）**：对标 QVMConsole 实测后登记的十三个条目全部落地（进度与核对结论见 [`docs/01-product/DEMO_PLAN.md`](docs/01-product/DEMO_PLAN.md) §7/§8）
  - **邀请链接站点地址（G-49，缺陷修复）**：新增设置项 `basic.site_url`（环境变量 `SITE_URL`），`main.go` 里返回空串的装配接上设置读取——此前管理员复制到的 `/invite/<token>` 是相对路径，发给站外用户打不开。未配置时链接仍为相对路径（不猜域名），前端在邀请面板给出明确提示
  - **输入侧防护与全局安全响应头（G-42）**：新增 `internal/api` 的请求过滤中间件（路径穿越、空字节与控制字符、扫描器探测路径、异常 Content-Type；开关 `security.request_filter_enabled` 默认开）与安全响应头中间件（nosniff / X-Frame-Options: DENY / CSP frame-ancestors / Referrer-Policy）。此前 nosniff 只在四个下载类接口逐个手设，X-Frame-Options / CSP / Referrer-Policy 全仓缺失
  - **日志归档与保留策略（G-43）**：轮转归档改为 gzip 压缩（`kc.log.N.gz`，导出与清理兼容历史未压缩归档），新增「日志最大备份数」设置项（`logging.keep_files`，改完即时生效，缩小上限在下次轮转收敛）。libvirt / cmd 日志分类**随真实 agent 落地**——agent 未实现前没有输出可分类
  - **站点级维护模式（G-46）**：多节点语义——逐节点接管节点维护（可选先为运行中的虚拟机逐台入队优雅关机）并逐节点汇总结果；「阻止开机」复用既有的节点维护拦截（`ensureNodeUsable`），不加第二处判断；退出只解除**本次接管**的节点，管理员手工设置的维护不受牵连。状态行落表 `site_maintenance`（迁移 `0048`）；进入动作受二次验证保护
  - **速率型全局带宽总限（G-44）**：设置项 `network.global_bandwidth_mbps` / `network.global_burst_mbps`（全部虚拟机共享的节点出向总限，与累计型配额、交换机级限速三层分工）+ `POST /networks/global-bandwidth/apply` 逐节点下发；agent 新增 `network.global_bandwidth.apply` 操作与 mock
  - **默认磁盘 IOPS（G-45）**：设置项 `vm.default_disk_iops`——创建向导预填、受理时三项 IOPS 全空的请求兜底；显式填 0（明确不限速）不覆盖
  - **救援系统 ISO 可选（G-47）**：设置项 `vm.rescue_iso`（相对 ISO 存放目录的宿主机路径，校验不可逃逸），随救援进入任务下发；留空维持节点内置默认镜像
  - **调度事件保留期（G-48）**：设置项 `scheduler.event_keep_hours`（默认 168 小时）+ 每小时一轮的保留清理（注册为内置调度器「调度事件保留清理」，删了才记事件）
  - **API 文档元数据（G-50）**：接口清单在路由表实时取出的基础上补齐摘要（CRUD 按方法与路径形状生成、关键接口人工校准）、模块名、认证方式（公开与管理员路由为从注册处机械提取的显式数据表）、路径参数与可复制 curl（路径参数替换示例值）。文档页支持按摘要过滤、行展开查看 curl
  - **端口转发与静态地址联动提示（G-51）**：详情页网络 Tab 在「有转发但未绑静态地址」时警示 DHCP 变化会让转发失效；不做自动绑定——多网卡时"绑哪个地址"不能替用户决定
  - **存储空间自动回收（G-52）**：设置项 `storage.auto_trim_enabled` + 每天一轮对全部在线节点执行 trim（注册为内置调度器，结果含失败节点记入调度事件）；存储池页展示自动回收状态与最近一次执行结果
  - **一次性动作令牌闭环（G-53）**：认证中间件支持 `?action_token=` 查询参数（一次性消费、用途限定 download、校验在单条 UPDATE 内完成）——此前后端能签发能校验但没有任何消费方；API 凭证页新增签发入口（明文仅显示一次）
  - **端口自动分配（G-54）**：设置项 `network.port_range_start/end`（默认 10000-20000），端口转发宿主机端口**留空即自动取未占用端口**（节点内按协议独占、唯一索引兜底并发）。其余候选设置项（OVS 网桥/出口网卡/网段前缀/DHCP 范围/IPv6 检测周期）按"先找到消费代码再认它是能力"的硬规则登记为刻意不做
- 项目基础设施：`.gitignore`、`.editorconfig`、`.gitattributes`、`.env.example`
- AI 协作规范：`AGENTS.md`（遵循开放标准，不绑定任何具体工具）
- 文档体系：`docs/` 七个分区
- 协作规范：`CONTRIBUTING.md`、`SECURITY.md`、`CHANGELOG.md`
- **架构决策：控制面 / 节点代理（agent）架构**（[ADR-0005](docs/06-decisions/0005-control-plane-node-agent-architecture.md)）——每台宿主机只部署轻量 agent，控制面**不持有宿主机登录凭据**，能力探测下沉到节点；据此填充 `docs/02-architecture/ARCHITECTURE.md`，并同步 PRD、能力地图、数据模型（迁移 `0002_node_agent_fields`）、前端设计与技术选型
- **架构决策：前端技术栈**（[ADR-0006](docs/06-decisions/0006-frontend-tech-stack.md)）——Vite + React 19 + TypeScript（strict）+ Tailwind CSS 4 + shadcn/ui + React Router v7（库模式）+ TanStack Query / Zustand / TanStack Table / React Hook Form + Zod / ECharts / i18next / noVNC，工程配套 pnpm + Vitest + Playwright + MSW；前端工程规划于 `web/`（目录结构与约定见 `docs/02-architecture/FRONTEND.md` §2），**尚未创建**
- **产品与架构文档**：功能需求（`docs/01-product/PRD.md`，11 个能力域 94 项功能与优先级）、能力地图（`docs/01-product/CAPABILITY_MAP.md`）、数据模型（`docs/02-architecture/DATA_MODEL.md`，43 张表）、前端设计（`docs/02-architecture/FRONTEND.md`，设计系统与 17 个页面）
- **数据库表结构**：`internal/database/migrations/0001_init_schema.sql`（43 张表 / 81 个显式索引）建库并在 `schema_migration` 登记；后续迁移 `0002_node_agent_fields`（节点接入改为 agent）、`0003_task_agent_fields`（任务幂等键、下发与上报时间）均已执行
- **Go Web 基础框架**（Go 1.27）
  - Web 框架：CloudWeGo Hertz v0.10.6
  - ORM：GORM v1.31.2
  - 数据库：PostgreSQL（pgx v5）与 SQLite（纯 Go 驱动），通过 `DB_DRIVER` 切换
  - 配置：全部来自环境变量，启动时校验，摘要输出已脱敏
  - 健康检查接口：`GET /health`
  - 测试：配置加载与 handler 行为，使用 SQLite 临时库，不依赖外部服务

- **工作台（F-8-03 管理员 / F-8-04 租户）**：首页由占位页改为真实概览——状态横幅（正常 / 告警，告警逐条列出并直达对应页面）、统计卡（虚拟机、节点、进行中任务、近 24 小时失败任务）、宿主机资源三条计量条（CPU / 内存 / 存储池，标注采样时刻与采样节点数）、已分配给虚拟机的资源（运行中 / 全部）、最近虚拟机 5 行
  - 新增聚合接口 `GET /api/v1/dashboard/summary`（API-233）：一次返回整屏，避免「宿主机资源」退化成逐节点实时探测
  - 采样补 `host_stats_record.cpu_cores`（迁移 `0025_host_stats_cpu_cores`）：核数此前只存在于按需探测的结果里，没有它就无法把「已分配的 vCPU」换算成占比
  - 未覆盖（依赖 agent 侧能力，待后续）：FRONTEND §5.2 的「当前用量 / 理论最大量」双进度条、KSM / zRAM 区段、CPU 与内存的硬件详情（dmidecode）、宿主监控按物理网卡与硬盘分设备筛选（四项均已在此后的「工作台对齐」与「监控增强」批次落地，走 agent 按需操作 + mock）
- **邮件与登录链路（F-1-08）**：账号从此可以自助闭环——此前忘记密码只能上服务器跑应急脚本
  - 新增 `internal/mailer`（仅标准库）：SMTP 发信，支持 STARTTLS / 隐式 TLS / 明文，配置来自系统设置因此**保存即生效**（"测试邮件"按钮是验证配置是否正确的唯一手段，重启才能生效会让那个按钮看起来一直是坏的）
  - 系统设置新增「通知」分组（SMTP 主机 / 端口 / 加密 / 用户名 / 密码 / 发件邮箱 / 发件人名称 / 超时），密码为敏感项：不回传明文、不入审计，**留空表示保持原值**（界面拿不到明文，整表回传时那一栏必然为空，按普通字段处理会让"改端口"顺手清空密码）
  - 登录改为**多阶段**：`ok` / `login_verify` / `force_password_change` / `bootstrap_security`。中间态令牌五分钟、单次、不下发 Cookie；业务接口额外断言只接受访问级会话，否则二次验证只是装饰
  - 管理员首次登录进入安全初始化引导（绑邮箱 + 启用两步验证，**可跳过**——自建自用的面板里"没有邮箱"是真实情况）
  - 找回密码：邮箱验证码 → 一次性重置票据 → 新密码。邮箱不存在同样返回成功（否则就是"谁注册了本面板"的查询入口）；只给**已验证**邮箱发码
  - 新增迁移 `0030_email_verification`：验证码存 bcrypt 哈希、带尝试次数上限与重发节流
  - 安全中心新增邮箱绑定区块；系统设置「通知」组下新增测试发信入口
- **配额维度补齐（F-1-09）**：此前"配额"这个词只覆盖了一半资源，用户照着配额规划，撞上的却是另一套规则
  - 计算配额新增三个**数量型维度**：快照、端口转发、公网 IP（迁移 `0031_compute_quota_extra_dims`）。按**用户 × 节点**汇总统计——换一台机器建快照不会绕过去；未配置时快照退回原有的"每台机器 10 个"默认值
  - 落到真实校验点：创建快照、添加端口转发、绑定公网 IP 各自校验（后两者此前**完全没有上限**）
  - 创建向导的前置条件新增"计算资源配额"试算：让用户在第一步就看到额度不够，而不是填完八个步骤再在最后一步被拒
  - 周期型配额新增**重置用量**（`POST /resource-quotas/:id/reset-usage`）：清的是本周期的累计而不只是状态——只清状态的话，下一次评估（五分钟内）会立刻重新判定为超限
  - 未做：**带宽配额**。（后续批次已补齐：用户级 `max_bandwidth_mbps`，见「用户与配额对齐」条目）
- **磁盘管理补深度（F-2-06）**
  - 新增**吞吐限制**（MB/s，总量 / 读 / 写，迁移 `0032_vm_disk_throughput`）：此前只有 IOPS，而一次连续的大文件拷贝不会被任何 IOPS 规则拦住——那恰恰是最容易把共享存储打满的操作。IOPS 与吞吐可并存，每组内部的「总量」与「读写分离」互斥
  - 限速挪进磁盘 Tab：用户想限一台机器的磁盘 IO 时，第一反应是来这里找，而不是去「编辑」页
  - 新增**磁盘迁移**（换存储池）：只接受同节点且就绪的池；运行中需显式确认热迁移；没有第二个池时不给入口
  - 删除虚拟机新增第三种磁盘处理方式 **transfer**
- **任务实时通道与常驻任务栏（F-7-02）**
  - 新增 `internal/realtime` 事件总线与 `GET /api/v1/tasks/stream`（SSE）：任务在入队、派发、落定三处广播，前端收到后重新取数（事件只说明"什么变了"）。慢订阅者丢弃并推送 `gap` 告知条数——不说的话用户会以为那段时间没有变化
  - 视角过滤在服务端：租户只收到自己名下资源的事件
  - 新增底部**常驻任务栏**：只在进行中任务存在时出现，完成的由事件驱动消失；不在这里做取消（窄栏里误点取消会中断一个正在搬运磁盘的任务）
  - 任务中心、详情页与底部栏的轮询改为 10 秒兜底（此前 2 秒）：实时通道是主路径，轮询只用于 SSE 断开时，否则界面会安静地停在旧状态
- **模板族与版本（F-3-04）**
  - 迁移 `0033_template_family`：`template` 增加 `family_id`（整条派生链共享的根模板 ID）与五个默认硬件列（磁盘驱动 / 网卡型号 / 机型 / 固件 / 显示设备）。此前模板侧只有 CPU 与内存，于是"从模板克隆"会把一台本来用 SATA 的 Windows 模板克隆成 VirtIO——开机就是蓝屏
  - 制备支持**派生**：指定 `parent_id` 即从某个模板派生新版本，版本按**族内最大值 + 1** 自增（按父版本算会让同一父的两次派生都叫 v2，第二次直接撞名字唯一约束）；必须同节点
  - 新增 `GET /templates/:id/family`：返回族内全部版本；删除预览的派生 blocker 现在统计**整棵子树**而不只是直接子级（只报直接子级会让用户处理完 v2 才发现还有 v3）
  - 删除新增两条策略：`cascade`（级联删除整条链）与 `promote`（下一级改挂到自己的父级，只删这一代，族不变）。链式克隆仍然没有任何策略可绕过——任何策略都意味着接受数据丢失
- **快照补两项能力（F-2-07 / F-2-11）**
  - **删除全部快照**：一个整体任务而不是循环调用单条删除（逐个入队会让"删到第三个失败"留下既删了一部分、又无处跟踪的中间状态）；按**先删后建的**顺序下发，反过来会在第一个上撞到"存在子快照"。可保留当前所处的快照；受二次验证保护
  - **修复 UEFI 启动项**：恢复快照之后最常见的后遗症——磁盘回到了过去，而固件里记的启动项仍指向当时的路径，表现为开机直接进 UEFI Shell。只对 UEFI 机器可用（BIOS 时明确拒绝，而不是"可用但什么也不做"）
  - 虚拟机视图新增 `firmware` 字段
- **控制台：独立窗口与 SPICE 连接文件（F-2-09）**
  - 新增 `/vm/:id/console-window` 路由（**不带布局**）：控制台要占满屏幕才好用，而布局里的侧栏、标签栏与顶栏会各吃掉一条。控制台页新增「在独立窗口中打开」
  - 新增 SPICE 连接文件下载（`.vv`）：SPICE 的体验优势全在本地客户端（USB 重定向、剪贴板、音频），而那些只有在 virt-viewer 里才存在；VNC 没有标准连接文件，仍走浏览器内的 noVNC
  - 节点新增**控制台对外地址**（迁移 `0034_node_console_host`，管理员填写）：它与监听地址（常为 127.0.0.1）是两回事，填的是用户在自己网络里能连到的那个地址。没填时不给连接文件——猜一个的代价是"下载了却连不上"
  - 连接文件**默认不含密码**（用户手输）；含密码的版本需要二次验证，且文件内容带 `delete-this-file=1`。控制台密码平时只写不读，默认路径不泄漏
  - 修掉一处端口错误：控制台配置此前无论选哪种协议都返回 VNC 端口，SPICE 的连接文件会指向错误的端口
- **模板导入导出（F-3-05）**
  - 导出：`POST /templates/:id/exports` 把模板盘打包成 tar.gz（清单 + 摘要），产物放在**我的存储**里——因此可下载、可作导入来源、也受存储配额约束。产物记在 `template_export`（迁移 `0035`），列表挂在 `/template-exports` 而不是模板之下：模板删掉之后这些包依然存在
  - 导入：强制**先预览、后导入**（`POST /templates/imports/preview`）。名字撞了、格式不对、摘要不符都在导入之前被看见——导完之后才发现，用户已经等完了整个解包过程。受理时再验一次：预览与导入之间可能隔了几分钟
  - 来源必须是「我的存储」里 category=`template_package` 的文件（新增文件类别）。族的接续只在本节点已有同名族根时才成立，否则作为独立模板导入——凭空建一个族会让用户误以为它与别处某个模板同源
  - agent 侧新增四个操作（export / export.delete / import.preview / import）与对应 mock。此前记为「卡在节点能力」是我表述保守：agent 本来就是契约 + mock，这与之前加 `vm.nvram.repair` 是同一件事
  - 仍未做：**Linux 模板预处理**——它需要节点上的 virt-customize 一类工具链，与"加一个操作"不是一回事（后续批次已补齐：`template.preprocess`，见「Linux 模板离线预处理」条目）
- **工作台对齐（页面功能对齐第一批）**
  - 宿主机资源改为**双进度条**：上栏是此刻实际占用，下栏是「理论最大」（运行中的虚拟机同时满载时的占用）。只看实际不知道还能开几台，只看理论最大又会以为余量已被用掉
  - 新增「宿主机细节」区块（按节点）：KSM / zRAM（复用调优服务，同一份口径）、硬件构成（CPU 拓扑 + 每核占用色块 + 内存插槽）、网络统计（NAT/DNAT 规则数、交换机与网桥计数），并给出调优设置入口
  - agent 新增 `host.hardware` 与 `host.netstats` 两个按需操作 + mock；三块各自独立失败，某块不支持时该块显示原因而不影响其他块
  - 新增「最近访问」（localStorage，不写库：它只是"我刚才看了哪几台机器"这一层便利）
  - 补上导出包**下载**端点（上一轮只做了列表与删除，产物拿不到）
- **虚拟机列表对齐（页面功能对齐第二批）**
  - 列表新增**标签列并可就地编辑**：写接口复用详情页那一个（`PUT /vms/:id/tags`），弹窗里列出已有标签供挑选——让人从里面选，比凭记忆打出同一个词的变体（prod / Prod / 生产）更能保持标签可用
  - 标签由后端**一次批量查询**填充（`vmtag.TagsOf`），不是逐台关联：后者会让每次列表请求变成上百次数据库往返。只在列表填充，详情仍由它自己的标签组件按需取
  - 列表新增**占用列**（CPU / 内存百分比）：数据来自 `vm_stats_record` 的每台最新一条（一次 SQL），**不做逐台实时探测**——一页 20 台就是 20 次跨节点往返。停机机器没有采样时显示"—"而不是 0：0 会被读成"这台机器很闲"
  - 占用列取不到数据时整列不显示，而不是让列表失败：列表是找机器的入口，为一行显示不出百分比而拒绝它代价不对等
- **创建向导补齐字段（页面功能对齐第三批）**
  - 新增**数据盘**（最多 4 块，各带大小 / 格式 / 驱动）：格式与驱动的可选值**复用矩阵**，与系统盘同一套候选，新增格式时不会静默拒绝。容量计入存储配额
  - 新增**系统版本**（`os_variant`，内置 libosinfo short id 候选，可持久化到 `vm.os_variant`，迁移 `0036`）
  - 新增**主机名**、**初始登录密码**、**首次启动初始化**（nocloud / configdrive / openwrt）、**主网口静态地址**
  - 初始密码加密写入来宾凭据（只写不读），**不进草稿**：localStorage 里的凭据是一个不长期存在却不出门的泄漏面
  - 校验独立成 `create_extras.go`
- **详情页与定时任务（页面功能对齐第四批）**
  - 新增「事件」页：后端把**审计**与**任务**两条流水合并成一条时间线返回（`GET /vms/:id/timeline`）。用户问的是"这台机器发生过什么"，让他自己按时间去对两个列表等于把答案藏起来
  - 新增 **PCIe 热插槽余量**（agent 新增 `vm.pcie.info` + mock）：热插一块盘或网卡需要一个空闲根端口，槽位用完的表现是"挂载成功但设备不出现"——而那个报错里不会提到槽位。"不支持热插拔"（i440FX）与"槽位用完"分开显示，前者换机型、后者只是扩容
  - 新增 **邻居表**（ARP / NDP，agent 新增 `vm.neighbor` + mock）：排查"虚拟机之间不通"时先看它能学到对端 MAC 没有，比抓包快一个数量级；MAC 由控制面回填成机器名
  - 新增 **CPU 亲和性（绑核）**（`vm.cpu_affinity`，迁移 `0037`）：与宿主机级"亲和性预设"分开存——预设是可复用候选，实际绑在哪几核是实例上的事实
  - 定时任务新增 **「创建快照」动作**（迁移 `0038`）：周期快照**允许**周期执行（与删除相反——快照是累加且可再删的），但必须填名字模板以便与手动快照区分；调度器通过接口复用 `vm.Service.CreateSnapshot`，不复制配额与探测逻辑
- **网络中心对齐（页面功能对齐第五批）**
  - 新增 **VPC ACL**（迁移 `0039_vpc_acl`）：挂在**网段**上的访问控制，与安全组（挂在虚拟机网口上）并列但不同。完整 CRUD + **先预览后应用**，应用必须带回预览版本号；控制面先判"匹配全部且拒绝"的遮蔽隐患
  - 交换机新增 **迁移**（换物理网卡 / VLAN，搬的是现有端口）与 **重配置**（按记录重新下发，对应"配置对、节点状态漂了"）；迁移成功后写回新的上行与 VLAN——否则面板会一直显示旧网卡
  - 新增 **流量计数重置**：计数是累计值，换环境或迁移后旧基数会让"这个月用了多少"完全失真
  - 新增 **IPv6 保护策略**与可信前缀：语义是"只放行可信前缀"而**不是**禁用 IPv6；开启保护必须至少给一个前缀，否则等于禁用全部 IPv6 转发
  - 新增 **端口释放**：回收节点上的端口资源。与"删除网卡"（控制面记录）不同——只删记录会留下一个谁也不用却一直占着的端口
  - agent 新增 `vpc.acl.preview` / `vpc.acl.apply` / `vpc.switch.migrate` / `vpc.switch.reconfigure` / `vpc.port.release` / `network.counter.reset` / `network.ipv6.policy` 七个操作与 mock
- **存储池对齐（页面功能对齐第六批）**
  - 新增 **分区管理**：查看 / 创建 / 删除 / 删除全部。控制面**不缓存**分区——手工改过之后任何缓存都会变成错的，而「面板说有 3 个分区、实际只有 1 个」比读不到更危险；系统分区与被池占用的分区不给删除入口
  - 删除全部与删单个分成两个按钮，并**分别登记**为两个高风险动作：一次点击抹掉整张分区表，与「删掉第三个」不是一个量级
  - 新增 **池配置**（挂载点 / 开机自动挂载 / 备注，迁移 `0040_storage_pool_auto_mount`）。它单独一个接口而不是扩展 PATCH：PATCH 是「只改控制面一列、同步返回」，而配置要下发到节点改挂载与 fstab——放在一起会让「保存」在不同字段上有完全不同的耗时与失败语义
  - 开机自动挂载**默认 true**：默认 false 的表现是「重启之后所有虚拟机找不到磁盘」——那是要重启才会暴露的默认值
  - 新增 **卸载存储池**（保留数据）：与删除不同；卸载后状态记为已卸载
  - 新增 **trim**（回收空间）：删掉虚拟磁盘之后宿主机的块设备往往还占着那些块，没有它「删了 200 GB 但可用空间没变」无法解释
  - agent 新增 `storage.partition.list` / `storage.partition.create` / `storage.partition.delete` / `storage.pool.config` / `storage.pool.unmount` / `storage.trim` 六个操作与 mock
- **用户与配额对齐（页面功能对齐第七批）**
  - 新增 **带宽配额**（`user.max_bandwidth_mbps`）：速率型，不按月累计，因此**不放进** resource_quota（那张表按月累计、有"用满"时刻）。消费点在网口限速——该用户在某节点上各网卡限速之和不得超过它，修改时限速排除自己那块
  - 新增 **SSH 访问控制**（`user.ssh_access_enabled`，列在 0001 已建好但此前无代码读写）：单独一个接口而不是塞进 PATCH，因为它会**结束在线会话**；不能改自己（把自己关在门外无法自助恢复），封禁中的用户不能开启；节点未实现时如实注明"宿主机上尚未生效"而不假装生效
  - 新增 **分配虚拟机归属**（`PUT /vms/:id/owner`，仅管理员）：补的是一个已知欠账——删除用户时若其名下还有虚拟机只能拒绝并提示"请先转移"，而"转移"此前没有入口。封禁中的用户与回收站里的机器不接受分配
  - agent 新增 `user.ssh.access`（改登录 shell + sshd 拒绝名单 + 结束在线会话）与 mock
- **设置与安全中心对齐（页面功能对齐第八批）**
  - 新增 **路径类配置**：ISO / 虚拟磁盘 / 模板 / 临时文件的存放目录（留空用内置默认目录）。默认布局在多数部署里够用，但只要数据盘挂在别处，它就变成必须能改的配置——改不了的结果是把数据盘塞进系统盘直到写满
  - 新增 **请求日志**（表 `request_log`，迁移 `0041`）：记录每次接口调用的方法、路径、状态码与耗时，**默认关闭**——它的量级远大于审计，默认打开只会把磁盘用在心跳与轮询上。中间件挂在最外层以便记到 401（"为什么一直 401"恰恰最需要它）；健康检查与静态资源不记；可查看与清空
  - 新增 **口令安全检查**（F-10-06）：判定由**节点**完成——控制面只有 argon2 哈希、无从比对，也不该为这项检查联网。命中**不自动改密**（改密会让会话全部失效，而"我还没准备好改"是合理的），只在安全中心标出。带每日循环并登记到调度器
  - 新增设置项：`security.request_log_enabled` / `security.request_log_keep_days` / `security.password_breach_check`
  - agent 新增 `security.password_audit` 与 mock
- **会话签名密钥轮换（F-1-09，页面对齐第九批）**
  - 密钥从"只在配置里"改为**存库**（表 `session_signing_key`，迁移 `0042`，加密存放）：放在配置里也能换，但换一次就要改环境变量并重启，而"怀疑泄漏"时没有时间安排重启
  - 轮换语义是**更换密钥即让全部旧令牌失效**（R-012），因此不保留旧密钥与宽限期——留了就等于轮换之后旧令牌还能用一段时间，而那正是轮换要消除的东西
  - 令牌签发器改为**每次签发/校验都重新取密钥**：缓存一把旧密钥会让轮换看起来没生效，那是最危险的一种错觉；令牌带上标准 `kid` 头标明用的是哪一代
  - 新增 `GET /settings/auth-key` 与 `POST /settings/auth-key/rotate`（受二次验证保护：它等价于一次全员登出，包括正在操作的自己）
  - 新增设置项 `security.auth_key_rotate_days`（0 = 不自动轮换）：轮换是否自动发生取决于部署形态——自建自用不该因为"到日子了"把所有人踢出去，托管多租户则应定期换
- **模板派生链维护（页面对齐第十批）**
  - 新增四个维护动作：`rebase`（backing 切到上级）、`flatten`（**在线**拉平到上级，不中断在用机器）、`promote_child`（提升子模板）、`promote_delete`（删除这一代，下游先改挂上级）
  - 关键点是 `promote_delete`：直接删中间一代会让下游全部失效，而节点上的表现是"读某个块时才报错"——那种故障要等数据被访问才暴露
  - 四个动作共用一个任务类型、下发时各自映射到不同的 agent 操作；**先让节点成功、再改控制面的父子关系**——反过来会在节点失败时留下一条"已经改挂了、实际没改"的记录
  - 每个动作都要勾选确认，并给出**具体的**理由（会改写多少、影响谁），而不是一句"操作有风险"
  - agent 新增 `template.rebase` / `template.flatten` / `template.promote_child` / `template.promote_delete` 与 mock
  - 补全 **族视图**：按 `parent_id` 拼出层级、同一层按 version 排序，族标题写明"共几个版本"。列表视图用于找模板，族视图用于看"这条链上有几代、谁派生自谁"——后者在扁平列表里只能看到一句"派生自 #N"
  - 视图切换从表头挪到区块头部：它是整个视图的选择，而不是"名称"这一列的属性
- **公网 IP 三项运维能力（页面对齐第十一批）**
  - 新增 **IPv6 前缀检测**：一个 /64 有 2^64 个地址，手填既容易错、也回答不了"还剩多少能分"。而"能不能用"取决于本地路由与上游通告，这个判断只有节点能给（Trusted 字段）——**未授信的前缀也列出来**，过滤掉会让人以为检测到的都能用
  - 新增 **重载规则**：按当前绑定关系重新应用全部规则。它**不需要参数**——参数是"要改成什么"，而重载做的是"让节点与我们记录的一致"，对应"记录对、节点上漂了"这一场景
  - 新增 **来宾地址状态**：回答"绑定成功之后为什么还是不通"——**绑定成功 ≠ 来宾里配好了**，控制面只保证规则下发，来宾里还要有人把地址配上
  - 重载结果如实返回失败条数：只报"已重载"会让失败的几条被当成成功
  - agent 新增 `public_ip.ipv6_detect` / `public_ip.reload` / `public_ip.guest_status` 与 mock
- **侧栏与协议（页面对齐第十二批，P2 收尾）**
  - 侧栏改为**六组分组**（概览 / 计算 / 网络 / 存储 / 系统 / 支持）：菜单三十多项，扁平列表里找一项要扫完全表，而"属于哪一类"是扫读时唯一的线索
  - 分组可折叠并记在 localStorage（个人偏好而非系统状态）；每项带图标与 `title`（收窄或被截断时仍能认出）
  - 数量徽标：虚拟机总数、进行中任务数、未确认告警数。三个查询都只取总数（`page_size=1`），而不是把列表拉回来
  - 新增**用户协议与公测协议**：关于页展示（默认收起：全文展开会把页面推得很远），初始化页需勾选同意才能创建管理员——初始化是面板的第一次使用，在这一步确认最合适
  - 协议是**前端常量而非可编辑设置项**：法律文本需要与版本绑定、可审计，可编辑会让"当前生效的是哪一版"变成要额外回答的问题
- **详情页与创建向导补齐（P1 第 3、4 项）**
  - 来宾地址状态接入详情页网络 Tab：后端与接口早就有了，缺的只是界面入口。它回答"绑定成功之后为什么还是不通"——**绑定成功 ≠ 来宾里配好了**
  - 创建向导新增 **CPU 拓扑**（路数 × 每路核数 × 每核线程，乘积必须等于 vCPU）、**隐藏虚拟化特征**、**嵌套虚拟化**、**软盘镜像**、**直通设备**、**网口数量**
  - 软盘按**独立设备**建模（新增 agent 操作 `vm.floppy.apply` + mock）而不是塞进光驱表：vm_cdrom 的 bus 里没有 floppy，硬加会让一个字段同时表达设备与总线
  - 直通在创建时挂载：先下发绑定（bind 到 vfio-pci）、成功后再写记录——"设备是否存在 / 是否被占用 / 分组是否冲突"这些判断只有节点能做，先写记录会留下一条"挂了但实际没挂上"的数据
  - 直通选择界面显示**同组设备**：IOMMU 分组里的设备只能一起直通，用户选一块卡必须知道另一块也被带上了
- **邀请注册（P1 第 1 项）**
  - 新增 `user_invite` 表与 `internal/invite` 包：创建邀请、列表、撤销、重发、公开预览与公开接受
  - 为什么是邀请而不是开放注册：面板能接管宿主机上的全部虚拟机；邀请让谁能进来成为一次由管理员发出、有期限（默认三天）、可追溯的动作
  - 库里只存令牌**哈希**：链接会出现在邮件与聊天记录里；重发会生成新链接并把旧链接作废——旧链接可能已泄漏，换新成本为零收益明确
  - 与新建用户的区别：密码由受邀人自设（≥12 位）且管理员不知道它，因此不走强制改密；配额在邀请时定好，避免进来之后再谈额度
  - 接受邀请**不自动建会话**：注册完需自己登录一次；自动登录会让这个账号是谁在用变得含糊
  - 公开注册页 `/invite/:token` 先预览后注册：链接失效时表单根本不出现——让用户填完再告诉他失效是很糟的体验，且会让他以为自己填错了
- **Linux 模板离线预处理（P1 第 2 项）**
  - 新增 `internal/template` 的 Preprocess 与 PreprocessExecutor，以及 agent 操作 `template.preprocess` + mock
  - 四个可组合选项：安装来宾代理、注入 SSH 公钥、重置 machine-id、清理 cloud-init 残留
  - 为什么必须在节点侧：预处理要挂载镜像并改写里面的内容（virt-customize 一类工具链），控制面既没有工具链也不挂载镜像
  - 节点缺工具链时返回 Unavailable 且判为失败：把它当成功会让用户以为镜像已经处理过，而实际上一项都没做
  - 模板新增 `preprocessed_at`；不存预处理选项列——选项是每次执行时选的，存成模板属性会让下次执行沿用旧选项

- **创建虚拟机向导（F-2-02）**：列表页的单页表单改为多步向导——创建方式（ISO 安装 / 模板克隆，导入两条置灰并说明入口）→ 基础信息 → 硬件规格 → 存储介质 → 网络设置 → 系统配置 → 高级选项 → 确认信息（分组摘要 + 缺失项提示）
  - **步骤、字段、取值、默认值与前置条件全部由后端下发**（`GET /api/v1/vms/create-form`），前端不维护第二份规则；字段矩阵与编辑页共用，创建与编辑允许的组合因此天然一致
  - 新增配置项：磁盘格式、磁盘驱动、网卡型号、系统类型、机器类型、固件与安全启动、引导顺序、开机自启、看门狗、CPU 型号与使用率上限、APIC / PAE、启动时冻结、磁盘 IOPS 限值；创建时可指定安装镜像（创建后自动挂载光驱）、主网口接入的交换机与安全组
  - 支持**批量创建**（一次最多 5 台，N 台 = N 个独立任务）与 `client_token` 幂等（重复提交只产生一个任务）；重名时给出可用建议名
  - 未覆盖：硬件直通步骤（F-2-06，页面标注待实现）、首次启动初始化（F-2-17，M2 仅「不初始化」）、OVF/OVA 导入（F-2-13）（三项均已在此后批次落地：直通在创建时挂载、nocloud / configdrive / openwrt 初始化、G-34 真实上传与 OVF/OVA 导入）

- **磁盘管理（F-2-06）**：虚拟机详情新增「磁盘」标签页——列表（设备、配置容量与实际占用、格式、总线、镜像路径）+ 挂载「我的存储」里的虚拟磁盘 + 卸载 + 换总线
  - 列表**实时向节点探测**，控制面不建表（磁盘是虚拟化层状态，存一份就要对账，而多一块少一块不会报错）；节点未返回时显示「节点没有返回磁盘」而不是空态
  - 系统盘保护、运行中热插拔限制、换总线需关机这三条约束**前后端各一份**（界面禁用并给出原因，后端再拦一次）
  - 新增 agent 契约 `vm.disk.list` / `vm.disk.change`（attach / detach / bus）与任务类型 `vm.disk.change`，`MockClient` 返回系统盘 + 数据盘两块，形状完整可供界面逐列渲染
  - 未覆盖：IOPS 限值（走「编辑 → 磁盘与驱动器」，与其它配置项同一条路）、离线扩容、槽位耗尽自动降级（三者均已补齐：IOPS 与吞吐后来进入磁盘 Tab、`vm.disk.resize` 离线扩容、q35 槽位耗尽自动降级 virtio-scsi）

- **列表页交互（F-2-01）**：表格 / 卡片双视图、四种分组（不分组 / 按状态 / 按模板 / 自定义分组，组头带数量与组内全选）、表头排序（名称 / 配置 / IP / 创建时间，走后端白名单排序）、行内「更多」菜单（详情、控制台、编辑备注、锁定解锁、删除）
  - 视图与分组是**偏好**，跨会话保留；分组在前端做（它只改变当前页怎么看，交给后端会引出"每组取多少"这个没有正确答案的问题），排序在后端做（当前页只有 20 条，页内排序没有意义）
- **监控增强（F-8-01 / F-8-02）**：新增磁盘 IO 曲线（支持**吞吐量 / IOPS 口径切换**，宿主机侧无 IOPS 时明确说明）、宿主机可按**物理网卡与物理硬盘**筛选（只影响网络与磁盘，CPU 与内存仍是整机值）
  - 新增 agent 契约 `HostDeviceStat`（每块设备的累计量），`MockClient` 给出两块网卡 + 两块磁盘
- **API 文档页（F-9-07）**：新增 `/api-docs`，列出全部已注册接口、按模块分组、可复制 curl。**清单从框架的路由表实时取出**，不维护手写清单；不标注角色要求（无法可靠取得，鉴权以 `docs/03-api/API.md` 为准）
- **计算资源配额（F-1-08）**：新增 vCPU / 内存 / 实例数三个上限（按用户 × 节点），创建虚拟机时校验，超限**拒绝新建**并一次报全部超出的维度与余量
  - 单独一张表（`compute_quota`）而不是复用 `resource_quota`：后者是周期累计型（按月重置、超限限速或断网），这里是存量型（长期有效、超限拒绝新建），两种语义挤在一张表里每一列都会出现例外
  - 三个上限在同一行填写（它们总是一起设置）；全为 0 即取消配额；失效（虚拟化层已不存在）的机器不占额度

- **主题切换**：浅色 / 深色 / 跟随系统，顶栏一键循环，偏好持久化；首屏前由 `index.html` 内联脚本定好主题，避免深色偏好的人先看到一帧浅色。色值全部是 CSS 变量，因此切换不需要重新渲染任何组件
- **全局搜索**：⌘K / Ctrl+K 唤起命令面板，跨虚拟机 / 节点 / 模板检索，键盘可全程操作（↑↓ 选择、回车跳转）。面板**只做跳转不做操作**——一个能随手触发写操作的搜索框，等于把危险操作放在离误击最近的地方
- **页面标签栏**：顶部多标签，访问即注册、可关闭、可"关闭其他"，工作台固定；虚拟机详情页把标签标题改成机器名（路由只能给出"这是哪一类页面"，而用户认的是名字）
- **回收站（F-2-16）**：删除 = 移入回收站（记录保留、磁盘未动，可逆），彻底删除 = 删盘 + 物理删记录（不可逆）。回收站不是新表，就是 `present = false` 的那些记录，因此不存在"两张表对不上"；恢复不自动开机
- **告警中心（F-8-07）**：覆盖节点离线 / 未接入 / 维护中、虚拟机失效、任务失败、配额超限、存储不足、宿主 CPU 与内存过高；每 5 分钟评估一次并按 (种类, 资源) 去重，问题消失自动关闭但保留记录；支持单条与全部确认，**确认不等于解决**（条目仍在列表里）
- **i18n 框架与语言切换**：新增 `locales` 与语言 store，导航、通用按钮、状态名已双语化，默认跟随浏览器语言。**渐进迁移**：业务页面的长句仍以中文写在页面里，`t()` 未命中时回落中文原文，界面不会出现空白或裸露的 key
- **关于项目页**：版本号取自构建信息（与「版本与依赖清单」同一接口），开源地址与许可证按仓库实际情况显示——仓库里没有 LICENSE 时如实说明，而不是编一个协议出来

### Changed
- **界面观感与信息层次重做**（对标 QVMConsole 逐页截图后重画，不含功能增删）
  - 工作台：首行改为问候 + 状态（状态点 + 一句话），正常时不再单独占一条绿色横幅——状态已在首行表达过；统计卡补图标与语义底色、数字加大；新增「资源走势」（CPU / 内存 × 1h / 6h / 24h / 7d，取自采集器的分钟级采样），它与「宿主机资源」是同一份数据的两种尺度
  - 创建向导：横向胶囊步骤条换成左侧竖向步骤栏，内容按语义分卡片，每步补标题与说明，创建方式卡片带图标与浮起选中态
  - 虚拟机列表：搜索与视图偏好并成一条工具栏，搜索框内嵌图标，行悬停改用可见底色
  - 虚拟机详情页：标题下集中「节点 / 地址 / 规格」，操作按后果分组（日常 ｜ 改配置 ｜ 不可逆），停机时资源卡改列规格而不是一句「无指标」
  - 全站：41 处页面标题统一为 `text-xl`，24 处数据行补悬停反馈，`Segmented` 提为通用组件
- **明确标注模拟节点**：节点侧由 `internal/agent` 的 mock 实现扮演时，其数值是常量（CPU 23.5% / 内存 6144MB 写死），「资源走势」因此是一条直线。节点列表与工作台的走势卡会标注「模拟」，把这与「没采到数据」区分开；接入真实 agent 后标识自动消失

### Removed
- 脚手架阶段的示例模型（`model.User`）与示例接口（`/api/v1/users` 系列）：与正式表结构不兼容，保留会让自动迁移给正式表加错列
- 启动时的自动迁移与 `DB_AUTO_MIGRATE` 配置项：表结构统一由 `internal/database/migrations/` 下的 SQL 迁移管理，服务启动不建表

### Fixed
- **接口字段命名不符合契约，导致页面读不到数据**（API.md §1.2 要求响应字段用下划线）：`internal/agent` 中直接嵌入 HTTP 响应的结构体缺 json tag，序列化出的是 Go 字段名 —— `GET /dashboard/host-detail` 的 `hardware` / `netstats`（`MemSlots`、`NATRules`…）与 `GET /host-firewall/connections` 的连接清单（`RemoteAddr`、`LocalPort`…）均受影响，前者会让工作台首页整页崩在 `hardware.mem_slots.map`
- **存储接口的路径参数名与路由不一致**：`GET /nodes/:id/disks`、`GET /nodes/:id/storage-pools` 读的是 `nodeId` 而路由给的是 `id`，两个接口恒返回 400「节点 ID不合法」，存储池页面因此加载不出磁盘与池列表
- **审计筛选项接口在 PostgreSQL 下 500**：`internal/auditlog` 复用同一个 GORM 查询对象构造两次 DISTINCT，第二次被带上第一次的 `ORDER BY action`，在 PG 上是非法语句（42P10）
- **前端“确认绑定邮箱”用了 POST**，后端是 `PUT /auth/email`，提交必然不匹配
- **开发代理吞掉前端页面路由**：`web/vite.config.ts` 的 `/api` 前缀会把 `/api-keys`、`/api-docs` 也转发给控制面，直接打开或刷新这两个页面拿到的是后端 404；改为只代理 `/api/v1`
- **分片上传的第一片永远传不上去**：`PutChunkData` 用 `namedPathID` 解析分片序号，而它按「ID 必须大于 0」校验，把 0 基的第一片（序号 0）判成非法 —— 于是「我的存储」上传文件、导入镜像、导入模板包三条链路都在第一步 400，还会留下一个 `pending` 上传会话，让同一个目标在 24 小时内只能收到 409「已有一个进行中的上传」（前端没有取消或续传的入口）。序号改为按 0 基解析，与服务层的校验口径一致
- **四个写接口把 `node_id` 放在请求体、后端却从查询参数读**，后端一律读到 0：开通存储空间 → 404「节点不存在」、重载公网 IP 规则 / 重置流量计数 / 回收空间（trim）→ 400「必须指定 node_id」。按项目既有惯例改成 `?node_id=`（`capture`、`firewall`、`host-firewall` 等写接口都这么传）
- **创建虚拟机向导的字段矩阵永不加载**：向导的步骤与 42 个字段全部来自 `create-form`，而该请求要 `node_id > 0` 才发出，节点选择器却硬编码在后一步里 —— 于是第一次打开只有「创建方式 + 确认信息」两屏、0 个输入框，用户看得见「还需要填写：虚拟机名、节点、安装镜像」却无处可填。节点选择器移到第一步，默认节点按「草稿 → 首个在线节点」派生
- **分片上传的每一片是 8 MiB，而 Hertz 默认只收 4 MiB**：超过 4 MiB 的文件必然 413，小于 4 MiB 的照常通过，表现像偶发故障。请求体上限放到 16 MiB，与前端 `CHUNK_SIZE` 留一倍余量
- **虚拟机列表的「占用」列漏了 `<td>`**：渲染成 `<tr><span>`（DOM 非法），且该列之后整体错位
- **Windows 上「目录共享」必然失败（被管宿主机路径误用了控制面本机语义）**：`internal/storage/share.go` 用 `filepath` 处理**被管宿主机**的路径，Windows 上 `/srv/users/7/data` 会被转成 `\srv\users\7\data`，而紧随其后的 `path.Base` 只认斜杠、于是把整串路径当成目录名，默认 tag 校验必然失败（`tag 只能由字母、数字…`）——不显式填 tag 的挂载 100% 不可用。改为统一走 `path` 语义（`path.Clean` / `path.Join` / `path.Base`），并在代码与架构文档中写明判断依据；Linux 上行为不变
- **Windows 上「清理日志」必然失败**：`internal/logging` 的 `Purge` 依赖对已打开句柄做 `Truncate(0)`，而 Windows 上以 `O_APPEND` 打开的句柄只有 `FILE_APPEND_DATA` 权限，截断会被拒绝为 `Access denied`。改为**关闭句柄后以 `O_TRUNC` 重新打开**——文件保留、内容清空，语义与 Linux 一致（不删文件本身，否则旧句柄会指向不存在的 inode）

<!--
## [0.1.0] - YYYY-MM-DD

### Added
- 首个版本：<!-- 简述 -->
-->
