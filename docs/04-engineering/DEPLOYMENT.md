# 部署指南（控制面）

> 状态：生效
> 最后更新：2026-09-28
> 关联：[`DEVELOPMENT.md`](DEVELOPMENT.md)（开发环境）· [`../../02-architecture/ARCHITECTURE.md`](../02-architecture/ARCHITECTURE.md) · [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md)

本文说明**控制面**与**节点 agent** 的部署、升级与回滚。控制面只持有期望态，宿主侧动作全部由 agent 经 mTLS 反向长连接执行；2026-11-04 起已移除 mock 传输，控制面必须配合真实 agent 才能管理宿主机。

---

## 1. 产物与要求

| 组件 | 要求 |
|---|---|
| 控制面二进制 | Linux amd64/arm64；Go 1.27 构建 |
| 数据库 | 开发/演示：SQLite（纯 Go，无 CGO）；生产：PostgreSQL 14+ |
| 端口 | 默认 `0.0.0.0:8080`（`APP_HOST` / `APP_PORT`） |
| 目录 | `data/`（SQLite、上传暂存、日志）；需要可写 |

前端构建产物（`web/dist`）由控制面静态托管，**单独部署前端没有意义**。

托管目录由 `STATIC_DIR` 指定（默认 `public`）。启动时该目录下没有 `index.html` 就只提供 API，不会阻断启动（开发期前端跑在 vite dev server 上，这是常态）。行为约定：

- 未匹配的 `/api/*` 始终返回 **JSON 404**，不做 SPA 回退——否则前端会把 HTML 当 JSON 解析，报错位置与真实原因无关；
- 其余未匹配的 GET/HEAD 回退到 `index.html`（SPA 深链刷新）；
- `assets/` 下的文件按内容哈希长缓存（`immutable`），`index.html` 等每次回源。

## 2. 构建与发布物

```bash
# 后端（版本号注入「关于」页；不注入则如实显示 dev）
go build -ldflags "-X k_cockpit/internal/platform/version.panelVersion=0.1.0 \
  -X k_cockpit/internal/platform/version.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o dist/server ./cmd/server

# 前端
cd web && pnpm install --frozen-lockfile && pnpm build
```

发布物目录：

```
dist/
├── server              # 控制面二进制
├── migrations/         # internal/platform/database/migrations 的完整拷贝
└── public/             # web/dist 的完整拷贝（如控制面需要外置静态目录）
```

> 注意：`server` 默认从**工作目录**读取 `internal/platform/database/migrations/`（`cmd/migrate` 的 `-dir` 可改）。发布时把迁移目录与二进制放在一起，避免升级脚本与代码库耦合。

## 3. 安装（systemd 示例）

```ini
# /etc/systemd/system/k-cockpit.service
[Unit]
Description=K Cockpit control plane
After=network-online.target

[Service]
Type=simple
User=kcockpit
WorkingDirectory=/opt/k-cockpit
EnvironmentFile=/etc/k-cockpit/env
ExecStart=/opt/k-cockpit/server
Restart=on-failure
# 日志同时进 journald（stderr 始终有输出，见 internal/logging）

[Install]
WantedBy=multi-user.target
```

`/etc/k-cockpit/env` 的最小配置：

```bash
APP_ENV=production
DB_DRIVER=postgres
DB_HOST=127.0.0.1
DB_PORT=5432
DB_USER=kcockpit
DB_PASSWORD=<真实凭据走部署密管，不落仓库>
DB_NAME=kcockpit
SESSION_SECRET=< openssl rand -hex 32 的产物 >
# 站点对外地址（邀请链接等发给站外用户的链接会拼上它）
SITE_URL=https://panel.example.com
LOG_DIR=/var/log/k-cockpit
```

生产环境的硬性校验（启动即失败，见 `internal/config`）：必须配置 `SESSION_SECRET`；不得配置 `SECURITY_DEV_BYPASS_CODE`。HTTPS 由反向代理终结（Cookie 的 Secure 标记随 `APP_ENV=production` 默认开启，代理必须传递）。

## 4. 数据库迁移

**服务启动不做自动迁移**（表结构由 SQL 文件显式管理）。升级流程：

```bash
# 1) 备份（回滚的依据，见 §6）
pg_dump "$DB_NAME" > backup-$(date +%F-%H%M).sql

# 2) 执行迁移（支持 -dry-run 预演）
./migrate -dir migrations -dry-run
./migrate -dir migrations

# 3) 换二进制并重启
systemctl restart k-cockpit
```

迁移记录在 `schema_migration` 表，重复执行是幂等的（已应用的语句会跳过）。

## 5. 升级流程

1. 读 `CHANGELOG.md` 的 **Changed** 与 **Removed**——有行为变更时先在预发环境过一遍核心链路；
2. 按 §4 备份并迁移；
3. 替换 `server` 二进制（与迁移目录同批发布）；
4. `systemctl restart`，验证：`GET /health` 返回 200、登录可用、工作台概览能加载；
5. 观察日志 5 分钟（`journalctl -u k-cockpit -f`），确认无启动期错误。

## 6. 回滚

**二进制可以退，数据不随便退。**

- 迁移**仅新增**的版本：直接换回旧二进制即可——旧代码不认识的新表/新列不影响它运行；
- 迁移**改写既有结构**的版本（删列、改类型）：用 §4 的 `pg_dump` 备份恢复，并接受备份之后的数据丢失窗口。因此备份必须在每次升级前执行，且高变更窗口建议停机升级。

## 7. 上线检查清单

- [ ] `APP_ENV=production`（漏配会把开发期的宽松默认带上线）
- [ ] `SESSION_SECRET` 已配置且与其它环境不同（同一密钥 = 跨环境会话互认）
- [ ] `SECURITY_DEV_BYPASS_CODE` 未配置
- [ ] HTTPS 已由代理终结，`SESSION_SECURE_COOKIE` 未被显式关闭
- [ ] `SITE_URL` 已配置（否则邀请链接是站外打不开的相对路径，G-49）
- [ ] 数据库为 PostgreSQL（SQLite 不用于生产）
- [ ] 迁移已执行且 `./migrate -status` 无 pending
- [ ] 首个管理员已创建（启动日志里的 bootstrap 令牌流程，ADR-0008），且令牌已失效
- [ ] 管理员的两步验证 / 邮箱已按需绑定（安全初始化引导）
- [ ] 备份任务就位（数据库定时 `pg_dump`，日志目录有清理策略）
- [ ] `GET /health` 与登录链路验证通过

## 8. 节点 agent 部署

每台被管宿主机执行一次：

```bash
# 1) 构建与控制面同版本的 agent
go build -o bin/agent ./cmd/agent

# 2) 安装二进制与 systemd 单元（root）
install -m 0755 bin/agent /usr/local/bin/k_cockpit-agent
install -m 0644 deploy/agent/k_cockpit-agent.service /etc/systemd/system/
systemctl daemon-reload

# 3) 在面板「节点」页生成一次性注册令牌，然后在宿主机执行
k_cockpit-agent enroll -server <控制面地址>:8443 -token <一次性令牌>

# 4) 启动常驻通道
systemctl enable --now k_cockpit-agent
```

要求：宿主机仅需**出向**连通控制面 `:8443`；身份材料写入 `/var/lib/k_cockpit-agent`（`agent.json` + CA/客户端证书）。证书签发后令牌即销毁，重装需重新生成。

## 9. 当前边界

- live 热迁移因需要节点间 libvirt 互信凭据、与 ADR-0005 冲突而**诚实阻断**（已交付 offline / warm）；
- 跨节点迁移 relay、agent 自动升级与遥测仍待后续批次；模板 tar 包通道已接通，待真机验收；
- **发布制品（2026-11-05）**：push `v*` tag 触发 `.github/workflows/release.yml`，产出 linux/amd64 与 arm64 两个 tarball（server / agent / migrate / public 前端产物 / migrations / agent systemd 单元）与 `SHA256SUMS`，并创建 GitHub Release。流水线**只出制品不部署**：分发端点、目标主机清单与回滚方案仍待确定，见 [ADR-0014](../06-decisions/0014-node-channel-and-agent-lifecycle.md)。
- 单控制面实例：PostgreSQL 形态启动时会取会话级 advisory lock（`internal/platform/database/instancelock.go`），第二个实例会**明确启动失败**而不是双写。要求 `DB_MAX_OPEN_CONNS >= 2`（一条被锁独占）、连接不经 PgBouncer transaction pooling（会话级锁与它不兼容）、`ConnMaxLifetime` 大于 5 分钟的保活间隔；SQLite 本地开发形态不启用锁，仅记日志。多实例部署仍需先外部化会话、任务队列与实时总线，单实例锁**不等于**支持多实例。

## 变更记录

| 日期 | 内容 |
|---|---|
| 2026-09-28 | 初版：控制面的构建、安装、迁移、升级、回滚与上线清单（agent 侧待落地） |
| 2026-11-04 | 移除 mock 传输；补充 agent 安装/enroll/systemd 章节与当前能力边界 |
| 2026-11-05 | 补充 PostgreSQL 单实例锁的行为、连接池与 PgBouncer 约束 |
