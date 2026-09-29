# 部署指南（控制面）

> 状态：生效
> 最后更新：2026-09-28
> 关联：[`DEVELOPMENT.md`](DEVELOPMENT.md)（开发环境）· [`../../02-architecture/ARCHITECTURE.md`](../02-architecture/ARCHITECTURE.md) · [`TROUBLESHOOTING.md`](TROUBLESHOOTING.md)

本文说明**控制面**的部署、升级与回滚。被管宿主机上的节点代理（agent）尚未实现（[ADR-0007](../06-decisions/0007-mock-agent-first.md)），单机生产形态待其落地后另行补充——当前部署所得的是一个以 mock agent 驱动的完整控制面，适合演示、联调与前端验收，**不能真实管理宿主机**。

---

## 1. 产物与要求

| 组件 | 要求 |
|---|---|
| 控制面二进制 | Linux amd64/arm64；Go 1.27 构建 |
| 数据库 | 开发/演示：SQLite（纯 Go，无 CGO）；生产：PostgreSQL 14+ |
| 端口 | 默认 `0.0.0.0:8080`（`APP_HOST` / `APP_PORT`） |
| 目录 | `data/`（SQLite、上传暂存、日志）；需要可写 |

前端构建产物（`web/dist`）由控制面静态托管，**单独部署前端没有意义**。

## 2. 构建与发布物

```bash
# 后端（版本号注入「关于」页；不注入则如实显示 dev）
go build -ldflags "-X k_cockpit/internal/version.panelVersion=0.1.0 \
  -X k_cockpit/internal/version.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
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

## 8. 当前边界（演示形态）

控制面在 `AGENT_TRANSPORT=mock`（默认）下运行：节点侧操作全部由 mock 返回假数据，界面上标注「模拟」。这是 [ADR-0007](../06-decisions/0007-mock-agent-first.md) 的已知代价，真实 agent 落地后：

- 本节标注的边界自动消失（无需改部署方式）；
- 补充宿主机侧 agent 的安装章节（systemd 单元、注册令牌、mTLS）。

## 变更记录

| 日期 | 内容 |
|---|---|
| 2026-09-28 | 初版：控制面的构建、安装、迁移、升级、回滚与上线清单（agent 侧待落地） |
