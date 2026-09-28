# 安全自查记录（0.1.0）

> 状态：生效
> 自查日期：2026-09-28
> 关联：[`../../SECURITY.md`](../../SECURITY.md)（安全承诺）· [`CODING_STANDARDS.md`](CODING_STANDARDS.md) §红线 · [`../01-product/ROADMAP.md`](../01-product/ROADMAP.md) M4

对照 [`SECURITY.md`](../../SECURITY.md) 的支持承诺与 AGENTS.md §7 红线，在 0.1.0 切版前对代码库做的一轮自查。**结论：未发现需要在发版前修复的问题**；两项定为"随真实 agent 落地后复查"（见 §3）。

---

## 1. 自查项与结论

| # | 检查项 | 结论 | 依据 |
|---|---|---|---|
| 1 | 仓库内无真实凭据 / 密钥 | 通过 | 工作树与历史中未发现真实 `DB_PASSWORD` / 令牌类提交；`.env` 不入库，`.env.example` 仅含空值示例 |
| 2 | 密码存储 | 通过 | Argon2id（`internal/auth/password.go`），带随机盐；找回验证码与邀请令牌用 bcrypt / SHA-256 哈希落库，明文只在签发响应里出现一次 |
| 3 | 会话凭据 | 通过 | HttpOnly + Secure（随 `APP_ENV=production`）+ SameSite=Strict Cookie（ADR 约定 f-1-01 Q-008）；中间态令牌不进 Cookie；签名密钥存库加密、轮换即全员失效 |
| 4 | 登录防爆破 | 通过 | IP 维度失败上限 + 账号维度延迟（`internal/auth/limiter.go`），阈值走系统设置；失败上限可由环境变量锁定 |
| 5 | 开发期万能验证码 | 通过 | 生产环境配置 `SECURITY_DEV_BYPASS_CODE` 会**启动失败**而非静默忽略（`internal/config.Validate`）；每次命中写审计 |
| 6 | 高风险操作二次验证 | 通过 | 判定集中在 `internal/risk` 清单（R-001），含迁移新加的站点维护进入；凭据 / API Key 认证路径不触发二次验证已在界面明示代价 |
| 7 | 安全响应头 | 通过 | 0.1.0 起全站中间件统一（nosniff / DENY / CSP frame-ancestors / no-referrer，G-42），不再散落在下载接口手设 |
| 8 | 输入侧防护 | 通过 | 请求过滤中间件默认开启（路径穿越 / 空字节 / 扫描器探测 / 异常 Content-Type，G-42）；分片上传的无 Content-Type 裸二进制有明确放行口径 |
| 9 | API 凭证 | 通过 | 仅存哈希 + 前缀；来源 IP 白限与到期为创建时主要选项；撤销后带 Authorization 的请求直接 401，不回落 Cookie |
| 10 | 一次性动作令牌 | 通过 | 单条 UPDATE 内完成校验与消费（并发不可重放）；用途绑定 download；短期有效 |
| 11 | 越权访问 | 通过 | 租户视角走 `authz.Viewer` 过滤，未命中一律 404（防枚举）；公开路由为显式清单（16 条，apidocs 元数据与注册处一致） |
| 12 | 敏感信息出站 | 通过 | 控制台密码只写不读；初始凭据读取写审计（只记"谁读了"）；设置敏感项不回传明文、不入审计；日志写入前统一脱敏 |
| 13 | SQL 注入 | 通过 | 全部查询走 GORM 参数化；无字符串拼接 SQL |
| 14 | CSRF | 通过 | SameSite=Strict Cookie + 无自定义头不产生写操作；API 凭证走 Authorization 头（跨站请求带不上） |

## 2. 已知的接受项（非缺陷）

- **API 凭证不触发二次验证**：自动化场景没有"人"来完成验证，这是文档化的取舍（API 凭证页有醒目代价说明），缓解手段是来源 IP 白限 + 到期。
- **请求过滤的 Content-Type 白名单不拦"未声明 Content-Type 的请求体"**：分片上传的裸二进制体就是这样发的，拦它会打断「我的存储」与导入链路（G-42 的取舍记录）。

## 3. 随真实 agent 落地后复查

| 项 | 为什么现在查不了 |
|---|---|
| 节点通道安全（mTLS、注册令牌） | agent 未实现，通道不存在 |
| libvirt / 命令注入面（XML 直编、救援、直通） | 执行侧在节点，mock 下没有真实 shell/libvirt 调用 |
| 日志中节点侧输出的脱敏（libvirt / cmd 分类） | 同 G-43：输出尚不存在 |

## 变更记录

| 日期 | 内容 |
|---|---|
| 2026-09-28 | 初版：0.1.0 切版前的 14 项自查，两项接受项、三项延后至真实 agent |
