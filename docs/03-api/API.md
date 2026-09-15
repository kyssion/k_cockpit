# 接口规范

> 状态：生效（通用规范 + 接口清单）
> 最后更新：2026-09-15
> 关联文档：[`../02-architecture/ARCHITECTURE.md`](../02-architecture/ARCHITECTURE.md) · [`../07-specs/`](../07-specs/README.md)（功能规格）

接口风格为 **REST + JSON**。本文件是**对外契约的单一事实来源**：接口先在此登记，再实现于 `internal/handler/`。

---

## 1. 通用规范

### 1.1 版本管理

- 版本在**路径**中体现：`/api/v1/...`。
- **不兼容变更**必须升版本（`/api/v2/`），旧版本保留至少 1 个发布周期。
- 兼容性变更（新增可选字段）不需要升版本。

### 1.2 命名

- 路径使用**小写 + 连字符**，资源名用**复数**：`/api/v1/vms`。
- 路径表示资源层级，动词用 HTTP 方法表达，**不在路径中出现动词**。
- 查询参数与请求/响应字段统一使用**下划线**：`page_size`、`created_at`。

### 1.3 HTTP 方法语义

| 方法 | 语义 | 幂等 |
|---|---|---|
| GET | 查询，不产生副作用 | 是 |
| POST | 创建或触发动作 | 否 |
| PUT | 全量替换 | 是 |
| PATCH | 部分更新 | 否 |
| DELETE | 删除 | 是 |

### 1.4 状态码

| 状态码 | 使用场景 |
|---|---|
| 200 | 查询/更新成功 |
| 201 | 创建成功 |
| 204 | 删除成功，无返回体 |
| 400 | 参数校验失败 |
| 401 | 未认证 |
| 403 | 已认证但无权限 |
| 404 | 资源不存在 |
| 409 | 状态冲突（如重复创建） |
| 422 | 语义校验失败 |
| 428 | 高风险操作需先完成二次验证（见 [`f-10-01`](../07-specs/f-10-01-high-risk-verification.md)） |
| 429 | 触发限流 |
| 500 | 服务端异常 |
| 503 | 服务不可用 |

---

## 2. 请求约定

- 请求体统一 `Content-Type: application/json`（文件上传除外）。
- 公共请求头：

| 请求头 | 必填 | 说明 |
|---|---|---|
| Authorization | 是 | 认证凭据 |
| X-Request-Id | 否 | 链路追踪，未传则服务端生成 |
| Accept-Language | 否 | 多语言 |

- 分页参数（**统一命名，全项目一致**）：

| 参数 | 类型 | 默认 | 说明 |
|---|---|---|---|
| page | int | 1 | 页码，从 1 开始 |
| page_size | int | 20 | 每页条数，最大 100 |

- 所有**外部输入必须校验**：类型、长度、范围、枚举、格式。

---

## 3. 响应约定

### 3.1 成功响应

```json
{
  "data": {},
  "request_id": "xxx"
}
```

分页响应：

```json
{
  "data": [],
  "pagination": {
    "page": 1,
    "page_size": 20,
    "total": 100,
    "total_pages": 5
  },
  "request_id": "xxx"
}
```

### 3.2 错误响应

```json
{
  "error": {
    "code": "INVALID_PARAMETER",
    "message": "参数校验失败",
    "details": [
      { "field": "email", "reason": "格式不正确" }
    ]
  },
  "request_id": "xxx"
}
```

**规则**：
- `code` 为**稳定的机器可读枚举**，前端据此分支；`message` 可读，可随文案调整。
- 错误信息**不得泄漏**堆栈、SQL、内部路径、服务名版本等敏感信息。
- 所有响应都带 `request_id`，便于排查。

### 3.3 错误码登记

| code | HTTP | 含义 | 处理建议 |
|---|---|---|---|
| INVALID_PARAMETER | 400 | 参数不合法 | 修正参数 |
| UNAUTHENTICATED | 401 | 未认证 | 重新登录 |
| PERMISSION_DENIED | 403 | 无权限 | 联系管理员 |
| RESOURCE_NOT_FOUND | 404 | 资源不存在 | 检查 ID |
| RESOURCE_CONFLICT | 409 | 资源冲突 | 刷新后重试 |
| VALIDATION_FAILED | 422 | 语义校验失败（格式合法但业务不可接受） | 按提示修正 |
| RISK_VERIFICATION_REQUIRED | 428 | 高风险操作需先完成二次验证 | 完成验证后重放原请求 |
| RATE_LIMITED | 429 | 请求过于频繁 | 退避重试 |
| INTERNAL_ERROR | 500 | 服务内部错误 | 携带 request_id 反馈 |
| SERVICE_UNAVAILABLE | 503 | 依赖不可用（如节点离线） | 稍后重试 |

> 新增错误码必须先在此登记，禁止临时编造。

---

## 4. 安全要求

- 所有接口默认**需要认证**，公开接口必须显式标记并说明理由。
- 每个数据访问接口都要做**归属校验**（防止越权访问他人数据）。
- 敏感字段（口令、令牌、证件号）在响应中**一律脱敏或不下发**。
- 接口必须防重放（幂等键或时间戳 + 签名，按场景）。
- 涉及写操作的接口应校验幂等性，避免重复提交产生重复数据。

---

## 5. 接口清单

| 编号 | 方法 | 路径 | 说明 | 认证 | 状态 | 关联功能 |
|---|---|---|---|---|---|---|
| API-001 | GET | `/health` | 健康检查，含数据库连通性探测 | 否 | 已实现 | 探活 / 就绪检查 |
| API-002 | POST | `/api/v1/nodes/registration-tokens` | 生成一次性注册令牌并创建待接入节点；**明文令牌只返回一次** | 是（管理员） | 已实现 | F-6-01 |
| API-003 | GET | `/api/v1/nodes` | 节点列表（元数据 + 运行态；状态由心跳推导） | 是（管理员） | 已实现 | F-6-02 |
| API-004 | GET | `/api/v1/nodes/:id` | 节点详情（含能力清单） | 是（管理员） | 已实现 | F-6-02 |
| API-005 | DELETE | `/api/v1/nodes/:id` | 移除节点（软删除，保留审计与历史引用） | 是（管理员） | 已实现（**二次验证待 f-10-01 接入**） | F-6-01 |
| API-006 | POST | `/api/v1/auth/login` | 用户名密码登录；令牌经 **HttpOnly Cookie** 下发，响应只含用户信息 | 否（公开，理由见 ADR-0008 同类的自举问题） | 已实现 | F-1-01 |
| API-007 | POST | `/api/v1/auth/logout` | 登出当前会话（仅撤销当前会话，并清除 Cookie） | 是 | 已实现 | F-1-02 |
| API-008 | GET | `/api/v1/auth/session` | 当前会话与用户信息 | 是 | 已实现 | F-1-02 |
| API-009 | GET | `/api/v1/auth/sessions` | 会话与登录记录列表（含当前会话标记，响应不含 session_id） | 是 | 已实现 | F-1-02 |
| API-010 | DELETE | `/api/v1/auth/sessions/:id` | 撤销指定会话（越权与不存在均返回 404） | 是 | 已实现（**二次验证待 f-10-01 接入**） | F-1-02 |
| API-011 | GET | `/api/v1/tasks` | 任务列表（按状态 / 类型 / 资源 / 时间筛选，归属过滤） | 是 | 规划中 | F-7-02 |
| API-012 | GET | `/api/v1/tasks/:id` | 任务详情（含参数、结果与阶段时间线） | 是 | 规划中 | F-7-02 |
| API-013 | POST | `/api/v1/tasks/:id/cancel` | 请求取消任务 | 是 | 规划中 | F-7-02 |
| API-014 | DELETE | `/api/v1/tasks` | 清理终态任务（拒绝含非终态） | 是 | 规划中 | F-7-02 |
| API-015 | GET | `/api/v1/events/{channel}` | **SSE 实时通道**（`vm-list` / `vm-detail` / `tasks` / `host-metrics`） | 是 | 规划中 | F-7-03 |
| API-016 | GET | `/api/v1/nodes/:nodeId/disks` | 块设备清单（容量、状态标签、是否系统盘、是否含数据） | 是（管理员） | 规划中 | F-5-01 |
| API-017 | GET | `/api/v1/nodes/:nodeId/storage-pools` | 该节点的存储池列表（含空间与新鲜度） | 是（管理员） | 规划中 | F-5-01 |
| API-018 | POST | `/api/v1/storage-pools` | 创建存储池（格式化 + 挂载），返回任务标识 | 是（管理员） | 规划中 | F-5-01 |
| API-019 | PATCH | `/api/v1/storage-pools/:id` | 设为默认池 / 修改备注 | 是（管理员） | 规划中 | F-5-01 |
| API-020 | DELETE | `/api/v1/storage-pools/:id` | 删除存储池（含占用检查），返回任务标识 | 是（管理员） | 规划中 | F-5-01 |
| API-021 | GET | `/api/v1/nodes/:nodeId/network` | 网络后端模式、能力清单、降级说明与默认网络状态 | 是 | 规划中 | F-4-01 |
| API-022 | GET | `/api/v1/nodes/:nodeId/networks` | 该节点可用网络列表（M2 仅系统基础网络） | 是 | 规划中 | F-4-01 |
| API-023 | GET | `/api/v1/vms/create-form` | 创建向导的表单元数据（字段、联动规则、可选值、前置条件） | 是 | 规划中 | F-2-02 |
| API-024 | POST | `/api/v1/vms` | 创建虚拟机（支持批量），返回任务标识 | 是 | 规划中 | F-2-02 |
| API-025 | GET | `/api/v1/vms` | 虚拟机列表（筛选 / 排序 / 分页，含数据新鲜度） | 是 | 规划中 | F-2-01 |
| API-026 | GET | `/api/v1/vms/:id` | 虚拟机详情（配置、投影状态与最近同步时间） | 是 | 规划中 | F-2-03 |
| API-027 | POST | `/api/v1/vms/:id/power-actions` | 电源操作（`start` / `shutdown` / `reboot` / `poweroff` / `reset`） | 是 | 规划中 | F-2-04 |
| API-028 | POST | `/api/v1/vms/batch-actions` | 批量操作（逐台独立任务，可部分成功） | 是 | 规划中 | F-2-01 |
| API-029 | DELETE | `/api/v1/vms/:id` | 删除虚拟机（`disk_action` 必填：`delete` / `keep`） | 是 | 规划中 | F-2-04 |
| API-030 | GET | `/api/v1/vms/:id/console` | 控制台配置与状态（开启状态、端口、暴露状态、显示设备） | 是 | 规划中 | F-2-08 |
| API-031 | PATCH | `/api/v1/vms/:id/console` | 开启/关闭、设置密码、切换对外暴露（暴露需二次验证） | 是 | 规划中 | F-2-08 |
| API-032 | GET | `/api/v1/vms/:id/console/screenshot` | 控制台截帧预览（服务端短时缓存） | 是 | 规划中 | F-2-08 |
| API-033 | GET | `/api/v1/vms/:id/console/ws` | **WebSocket**：VNC 流量代理（经 agent 通道转发，不直连宿主机） | 是 | 规划中 | F-2-08 |
| API-034 | GET | `/api/v1/security/high-risk-policy` | 高风险操作清单与判定口径（供前端渲染提示） | 是 | 规划中 | F-10-02 |
| API-035 | POST | `/api/v1/auth/risk-verification` | 提交验证码，换取一次性高风险许可 | 是 | 规划中 | F-10-01 |
| API-036 | GET | `/api/v1/settings` | 设置项清单（元数据、当前生效值、来源、是否被环境变量锁定） | 是 | 规划中 | F-9-01 |
| API-037 | PATCH | `/api/v1/settings` | 批量更新设置（部分成功语义，失败项自动回滚） | 是 | 规划中 | F-9-01 |
| API-038 | POST | `/api/v1/settings/rollback` | 将指定设置项回滚到最近一次变更前的值 | 是 | 规划中 | F-9-01 |
| API-039 | GET | `/api/v1/setup/status` | 系统是否已完成初始化（供前端决定跳初始化页或登录页） | 否（公开，理由见 [ADR-0008](../06-decisions/0008-first-admin-bootstrap.md)） | 已实现 | 首次初始化 |
| API-040 | POST | `/api/v1/setup/admin` | 用一次性令牌创建首个管理员；成功后自动建立会话 | 否（公开，理由同上） | 已实现 | 首次初始化 |

> **公开接口共 4 个**（`/health`、`/api/v1/setup/*`、`/api/v1/auth/login`）。前两个的公开理由是「系统尚无可用凭据时的自举需要」：初始化接口靠**只能从服务端日志获取**的一次性令牌保护（[ADR-0008](../06-decisions/0008-first-admin-bootstrap.md)），登录接口是获取凭据的入口本身。新增公开接口必须在此说明理由。

### 5.1 开发专用接口

> 以下接口**不属于对外契约**，仅在开发期存在，接入真实 agent 后不再注册路由（[ADR-0007](../06-decisions/0007-mock-agent-first.md)）。

| 编号 | 方法 | 路径 | 说明 | 认证 | 状态 | 关联功能 |
|---|---|---|---|---|---|---|
| DEV-001 | POST | `/api/v1/dev/agent-register` | 模拟 agent 注册：调用与真实 agent **相同的注册逻辑**，使节点可在无 agent 的情况下接入 | 否（凭注册令牌，与真实 agent 一致） | 仅 `AGENT_TRANSPORT=mock` 时注册 | F-6-01 |

**状态口径**：

- `已实现` = 代码中已注册且可用；
- `规划中` = 已在功能规格中确定契约、尚未实现。

> 本表**允许先于实现登记**（契约先行），但状态必须如实标注——禁止把未实现的接口标为已实现，也禁止实现后不更新状态。
>
> **脚手架阶段的示例接口（`/api/v1/users` 系列）已随示例模型一并移除**——它与正式表结构不兼容，留着会让"按模型自动建表"给正式表加错列。
> 新增接口的顺序：先在功能规格（[`../07-specs/`](../07-specs/README.md)）确定契约 → 在本表登记 → 实现于 `internal/handler/` → 注册于 `internal/router/router.go`。

---

## 6. 接口详情模板

### <!-- 接口名 -->

- **方法与路径**：`GET /example`
- **说明**：
- **认证要求**：
- **请求参数**：

| 名称 | 位置 | 类型 | 必填 | 说明 |
|---|---|---|---|---|
| | | | | |

- **请求示例**：

```json
{}
```

- **响应示例**：

```json
{}
```

- **错误码**：
-

- **变更记录**：

| 日期 | 变更 | 兼容性 |
|---|---|---|
| | | |
