# 参考项目结论：kite

> 状态：生效
> 最后更新：2026-09-13
> 结论基于提交：`546820c`（`reference/kite`，分支 `main`）
> 关联：[`../README.md`](../README.md)（参考项目总览与只读/同步约定）

本文只记录 kite **有哪些功能、分别是怎么实现的**，作为我们实现 Kubernetes 看板能力时的参考。**不含** `k_cockpit` 自身的技术选型与方案（见 [`../README.md`](../README.md) §2）。文中路径均相对 `reference/kite/`。

> kite 定位是"多集群 Kubernetes 工作空间"（看板 + 运维 + 可观测 + 治理 + AI 辅助）。它是我们的**功能参考**。

---

## 1. 多集群管理（核心抽象）

- **运行态 + 期望态 reconcile**：`ClusterManager` 用 `map[string]*ClientSet` 与 `errors` 保存运行态，DB（`model.Cluster`）是期望态；后台**每 1 分钟**（或收到 `TriggerClusterSync()`）全量 reconcile：并发构建客户端、失败写 errors、DB 已删除的停掉。`pkg/cluster/cluster_manager.go`
- **统一句柄 `ClientSet`**：每集群一个，含 `Name`/`Version`/`K8sClient`/`PromClient`/`PrometheusURL`——**多集群能力的统一抽象**。
- **注册/导入**：支持 kubeconfig、in-cluster、cluster-agent 三种；`ImportClustersFromKubeconfig` 把一个 kubeconfig 按 context 拆成多条。`pkg/cluster/cluster_handler.go`
- **凭证加密**：`Config`（kubeconfig）与 agent 私钥用 `SecretString`（AES-GCM）加密落库。`pkg/model/cluster.go`、`pkg/utils/secure.go`
- **客户端构建**：`kube.NewClient`（controller-runtime，默认带 informer 缓存，可用 `DISABLE_CACHE=true` 关闭；QPS/Burst 可调）+ 发现 server 版本 + 建 Prometheus client。`pkg/kube/client.go`
- **重建判定**：enable 开关 / kubeconfig 变化 / prom URL 变化 / server 版本变化 / agent `generation` 变化时重建；`K8sClient.Stop()` 通过 cancel context 回收。
- **默认集群**：DB `is_default` → `cm.defaultContext`；`GetClientSet("")` 取默认或任意一个。
- **集群选择的请求传递**：中间件依次从 path `/_clusters/:cluster` → header `x-cluster-name` → query 解析，注入 ctx。`pkg/middleware/cluster.go`

---

## 2. Cluster Agent（解决"连不到私有网络 apiserver"）

- **思路**：把连接方向**反过来**——目标集群内的 Agent 主动连回控制端，建立**反向隧道**（基于 `rancher/remotedialer`）。`docs/guide/kite-cluster-agent.md`
- **注册与密钥交换**：创建 agent 集群时生成随机 token（库中只存 SHA-256）+ X25519 密钥对（公钥可见、私钥加密存）；Agent 用服务端公钥 `box.SealAnonymous` 加密 kubeconfig 凭据后 POST `/cluster-agent/register`，**每 10 分钟刷新**，服务端解密后**只保存在内存**。`pkg/clusteragent/registration.go`
- **隧道的鉴权**：Agent 用同一 token `Authorization: Bearer` 连 `/cluster-agent/connect`，服务端回调校验 token 并绑定 clusterID。`pkg/clusteragent/manager.go`
- **把隧道当 kubeconfig 用**：`Manager.RESTConfig` 构造 `rest.Config`，`Dial` 设为 `remotedialer.Dialer(clusterID)`，`WrapTransport` 注入鉴权并**剥离所有 `Impersonate-*` 头**——于是 exec/日志/watch 全部透明复用同一套 client-go 代码。
- **部署清单生成**：输出 Secret(token) + SA + ClusterRoleBinding(cluster-admin) + Deployment（单副本，默认 `kube-system`）。`pkg/clusteragent/manifest.go`
- **授权码防泄漏**：manifest 下载 token 用 HKDF(JWT secret) + AES-GCM 加密 `{token,exp}`（10 分钟过期）放进下载 URL。

---

## 3. Kubernetes 资源管理（"一套代码支持 80+ 资源"）

- **元数据注册表是唯一来源**：`common.Registry` 的 `ResourceMeta{Kind,Singular,Plural,Short,Group,Version,ClusterScoped,Searchable,Related}`，`init` 构建索引；**新增资源只需加一行 + 注册一个泛型 handler**。`pkg/common/resource.go`
- **泛型处理器**：`GenericResourceHandler[T client.Object, V client.ObjectList]` 用反射拿类型，controller-runtime client 做 CRUD。`pkg/resources/generic_resource_handler.go`
- **多版本兼容**：`newVersionedResourceHandler` 为同一资源提供多个 GVR 候选，按集群实际支持选择。`pkg/resources/versioned_resource_handler.go`
- **CRD**：以 CRD 名作路由前缀，查 CRD → 推 GVR → 用 `unstructured` 做全套操作。`pkg/resources/cr_handler.go`
- **列表**：处理 `_all`/命名空间、`limit`/`continue`、label/field selector，创建时间倒序；**跨命名空间时逐条做 RBAC 过滤**。`pkg/resources/generic_resource_handler_list.go`
- **详情/YAML**：返回前**清空 `managedFields`、剥离 `last-applied-configuration`**（减噪）。
- **apply**：拆多文档 YAML → decode 成 unstructured → `RESTMapper` 映射 → 逐个判 create/update 并做 RBAC 校验 → 写 → 记审计。`pkg/resources/apply_handler.go`
- **写操作的 RBAC 校验点**：① 中间件统一校验；② `ApplyResource` 内部显式校验；③ Node Drain 逐 Pod 校验；④ AI 工具 `AuthorizeTool`。

---

## 4. 工作负载运维

- **扩缩容**：无专门接口，复用通用 PATCH 改 `spec.replicas`。
- **重启**：给 Pod 模板打 `kite.kubernetes.io/restartedAt` 注解后 Update 触发滚动。`pkg/resources/deployment_handler.go`
- **版本历史/回滚**：Deployment 用 `deployment.kubernetes.io/revision` 注解列 owned ReplicaSet，回滚用 kubectl `polymorphichelpers.RollbackerFor`；StatefulSet/DaemonSet 用 `ControllerRevision`。`pkg/resources/workload_revisions.go`
- **其他**：Pod `Resize`（K8s ≥1.35）、Node `Drain`/`Cordon`（逐 Pod 校验）。

---

## 5. Web 终端（Pod / Node / kubectl）

- **共同底座**：`kube.TerminalSession`（`remotecommand` exec/attach + TTY），消息 `{type: stdin/resize/stdout}`，resize 写入 `sizeChan`（实现 `TerminalSizeQueue`）。`pkg/kube/terminal.go`
- **Pod 终端**：`exec` 跑 `sh -c "bash || sh"`；debug container 场景先轮询等 ephemeral container 就绪再 `attach`。`pkg/terminal/terminal_handler.go`
- **Node 终端**：先在目标节点创建 `hostNetwork/hostPID/privileged + chroot /host` 的 agent pod，就绪后 attach。`pkg/terminal/node_terminal_handler.go`
- **kubectl 终端**：仅 admin；创建带 `cluster-admin` SA 的每会话 Pod 再 attach。
- **优先 WebSocket、退回 SPDY**：先试 WS executor，失败 fallback SPDY；Agent 隧道下直接 SPDY。`pkg/kube/remotecommand.go`
- **鉴权**：WS 路由经 `RequireAuth`（httpOnly Cookie 自动携带）+ `ClusterMiddleware`，handler 内再做 `rbac.CanAccess(..., "exec")`。

---

## 6. 日志

- **单 Pod**：组 `PodLogOptions`（container/tailLines/timestamps/previous）→ 流式跟随。`pkg/resources/logs_handler.go`
- **多 Pod 聚合**：`podName=_all` + label selector 时列 Pod，只对 Running 建流，并 **watch 动态增删**；多 Pod 时行前缀 `[podName]:`（用半行缓冲处理跨包行）。`pkg/kube/log.go`
- **实时流**：全程 WebSocket，带 ping/pong 心跳与 `log`/`pod_added`/`pod_removed`/`close`/`error` 消息类型。

---

## 7. Kube Proxy

- 路由 `/namespaces/:namespace/:kind/:name/proxy/*path`（kind 仅 `pods|services`），先做 `rbac.CanAccess`。
- 通过 **apiserver 代理**：拼成 `/api/v1/namespaces/{ns}/{kind}/{name}:{port}/proxy/...`，转发时剥离 hop-by-hop 头。
- **路径穿越双重防护**：拒绝含 `..` 的段；`url.JoinPath` 后二次校验清理后的 path 仍以期望前缀开头。`pkg/proxy/handler.go`、`pkg/kube/proxy.go`

---

## 8. Helm 管理

- 用 `helm.sh/helm/v4` + `blang/semver`；自定义 `restClientGetter` 把 rest.Config 适配 Helm，storage driver=`secret`。`pkg/helmutil/`
- **仓库**：支持 http/https/OCI + 用户名密码（加密存）；创建时校验 URL scheme 并先拉 index 验证。
- **Chart 列表/内容**：`loadRepositoryIndex`（5min 缓存）、`loadChartContent`（10min 缓存）、ArtifactHub 远端搜索（5min 缓存）。
- **Release**：封装 install/upgrade/rollback/uninstall/list/history；作为合成资源 `helmrelease` 挂资源路由，Delete 即 uninstall。
- **自动升级**：`scheduler` + `model.ScheduledTask` 定时执行器。

---

## 9. 认证

| 方式 | 实现要点 | 文件 |
|---|---|---|
| 本地密码 | bcrypt；首次仅当用户数为 0 时创建超管 | `pkg/auth/login_handler.go` |
| LDAP | service bind → 搜用户 → 用户 DN 再 bind 验密 → 搜组写入 `OIDCGroups` | `pkg/auth/ldap.go` |
| OAuth/OIDC | 通用 provider；写 `oauth_state` cookie 防 CSRF；callback 换 token → GetUserInfo → upsert 用户 | `pkg/auth/oauth_manager.go` |
| MFA/TOTP | 自实现（HMAC-SHA1/30s/6 位） | `pkg/mfa/totp.go` |
| Passkey | `go-webauthn` begin/finish | `pkg/passkey` |
| API Key | 作为 `provider=api_key` 的用户；`Authorization: kite<id>-<key>` | `pkg/auth/middleware.go`、`pkg/apikeys` |

- **会话载体**：JWT 存 **httpOnly Cookie `auth_token`**（`SameSite=Lax`）；也接受 `Authorization` Bearer。401 自动 `RefreshJWT`。
- **登录限流**：按客户端 IP，1 分钟 ≥10 次失败封 5 分钟。
- **首次初始化 bootstrap**：`GET /api/v1/bootstrap` 返回 `setup.step`、可用登录方式、能力开关、当前用户与侧边栏偏好。`pkg/auth/bootstrap_handler.go`
- **用户热路径缓存**：`GetUserByIDCached` 30s LRU，写后失效。

---

## 10. RBAC 设计

- **角色四维**：`Clusters × Namespaces × Resources × Verbs`；动词仅 `get/create/update/delete/log/exec`（**patch 归 update、watch 归 get**）。`pkg/common/rbac.go`、`pkg/model/rbac.go`
- **分配对象**：`RoleAssignment`（Role ↔ Subject），Subject 可为 `user` 或 `group`（对应 OIDC/LDAP 组）。
- **判定算法**：`match()` **先扫 `!pattern`（命中即拒绝），再扫正向**；支持 `*`、正则（含正则元字符时按 `^(?:pattern)$` 编译）、取反；四维全 match 才放行。`pkg/rbac/rbac.go`
- **映射到 HTTP**：中间件把 method → verb、URL → (namespace, resource)，再 `CanAccess`；跨命名空间 LIST/WATCH 有受控 passthrough + 逐条过滤。
- **热更新**：每 1 分钟或 `TriggerSync()` 从 DB 重载；config file 可"托管"相应章节（UI 只读）。

---

## 11. 审计

- **实体**：`model.ResourceHistory{ClusterName, ResourceType, ResourceName, Namespace, OperationType, OperationSource, ResourceYAML, PreviousYAML, Success, ErrorMessage, OperatorID}`。`pkg/model/resource_history.go`
- **写入时机**：所有写操作（create/update/patch/delete/apply/rollback）后 `defer recordHistory`；Helm、AI、自动任务也会写（`OperationSource` 区分来源）。
- **查询**：管理员 `GET /api/v1/admin/audit-logs`（多条件分页）；资源级 `/:resource/:ns/:name/history`。`pkg/audit/handler.go`

---

## 12. 可观测性

- **每集群 Prometheus 自动发现**：按一组 label（prometheus / vmsingle / kube-prometheus-stack）列 Service，取 9090/8428/8429 端口，拼集群内 DNS。`pkg/cluster/prometheus.go`
- **跨网访问**：集群本地地址用 **apiserver service proxy** 改写；ClusterAgent 场景走隧道。
- **查询接口**：`GET /api/v1/prometheus/resource-usage-history`（duration 30m/1h/24h）与 Pod 指标；AI 有 `query_prometheus` 工具。`pkg/metrics/handler.go`
- **降级**：Prometheus 不可用 → 回退 **metrics-server**（结果标 `Fallback=true`），否则 503；内存缓存 30 分钟采样点，15s 去重覆盖。

---

## 13. AI 助手（安全模型值得借鉴）

- **Agent 循环**：`runConversation` 最多 100 轮，每次调 provider 流式拿消息 + `tool_calls`，有工具就批量执行后继续。`pkg/ai/agent.go`
- **工具集**：get/list/describe_resource、get_pod_logs、exec_in_pod、get_cluster_overview、apply/patch/delete_resource、query_prometheus，以及交互式 `request_choice`/`request_form`。`pkg/ai/tools.go`
- **先鉴权、再确认、后执行**：写工具（`MutationTools`）先 `AuthorizeTool`（用 `requiredToolPermissions` 把参数解析成 (resource, verb, namespace) 再 `rbac.CanAccess`，**完全复用用户权限**）→ 通过后把 pending session 落库并发 `confirmation_required` 事件暂停 → 前端确认后 `POST /ai/continue` → `ContinuePending` → `ExecuteTool`（claim 删除保证一次性）。`pkg/ai/tool_authorization.go`、`pending_session.go`
- **交互式表单**：生成 `input_required` 事件带 schema，前端提交后由 `buildInteractionToolResult` 归一化校验。`pkg/ai/interaction.go`
- **流式输出**：SSE（逐事件写 `event:`/`data:` 并 Flush）。`pkg/ai/handler.go`

---

## 14. 全局搜索

- 服务端 `GET /api/v1/search`，只搜 `Searchable=true` 的资源。`pkg/search/handler.go`
- 支持 `kind:name` 与 label `k: v`/`k=v`；`errgroup` 并行各资源搜索，合并后**按 RBAC 过滤**、按优先级排序、limit 50/100。
- **LRU 缓存**（key = cluster+user+query+limit，10min），支持"更短前缀查询"结果复用。

---

## 15. 前端实现要点（机制层面）

- **请求层**：统一 `credentials: include` + 自动加 `x-cluster-name`；**401 用单飞 `refreshPromise` 刷新后重试**，失败跳登录。`ui/src/lib/api-client.ts`、`current-cluster.ts`
- **当前集群上下文**：local/sessionStorage 持久；切换集群时 `invalidateQueries`（排除 user/auth/clusters）。
- **资源目录元数据驱动 UI**：`resource-catalog.ts` 定义每种资源的图标/分组/作用域，侧边栏/列表/详情/i18n **全部由此驱动**（与后端 `common.Registry` 对应）。
- **侧边栏可定制**：分组显隐/折叠/固定、自定义分组、把 CRD 加入分组，配置可存本地或后端。
- **react-query 策略**：全局 `staleTime=5min`、`refetchOnWindowFocus=true`、仅 5xx 重试 <3 次；开 SSE 时轮询置 0，否则 5s。
- **Monaco** 懒加载编辑/查看 YAML；**i18next** 双语；**WebSocket 统一封装**（连接状态/重连/心跳）。

---

## 16. 其他有设计含量的点

- **定时任务多副本互斥**：DB 行级锁 `lock_until/locked_by`，`NextRunAt` 支持 interval/daily。`pkg/scheduler/scheduler.go`
- **状态总览并发聚合**：`errgroup` 并行拉 node/pod/ns/svc，用 int64 累加避免大数运算。`pkg/system/handler.go`
- **相关资源**：按 label/owner/selector 反查（如 Service↔Pod、Ingress↔Service），路由 `/:namespace/:name/related`。
- **配置托管（managed）**：config file 接管的章节在 UI 中只读。

---

## 附：关键文件速查

| 功能 | 关键文件（相对 `reference/kite/`） |
|---|---|
| 入口/装配 | `main.go`、`app.go`、`routes.go`、`static.go`、`internal/config.go` |
| 多集群 | `pkg/cluster/cluster_manager.go`、`cluster_handler.go`、`pkg/model/cluster.go`、`prometheus.go` |
| Cluster Agent | `pkg/clusteragent/{manager,registration,manifest,run}.go` |
| K8s 客户端 | `pkg/kube/{client,terminal,exec,log,proxy,remotecommand}.go` |
| 资源处理 | `pkg/resources/{handler,generic_resource_handler*,cr_handler,apply_handler,deployment_handler,workload_revisions,logs_handler}.go`、`pkg/common/resource.go` |
| 认证/鉴权 | `pkg/auth/*`、`pkg/rbac/{rbac,manager}.go`、`pkg/middleware/{rbac,cluster}.go`、`pkg/mfa`、`pkg/passkey`、`pkg/apikeys` |
| 审计/数据 | `pkg/model/{cluster,custom_type,general_setting,resource_history}.go`、`pkg/audit/handler.go` |
| 可观测 | `pkg/prometheus/client.go`、`pkg/metrics/handler.go` |
| AI | `pkg/ai/{agent,tools,tool_authorization,tool_resource_execution,handler,interaction,pending_session}.go` |
| 前端 | `ui/src/lib/api-client.ts`、`lib/api/*`、`lib/resource-catalog.ts`、`components/app-sidebar.tsx`、`contexts/*` |

---

## 变更记录

| 日期 | 变更内容 |
|---|---|
| 2026-09-12 | 创建文档：基于提交 `546820c` 整理功能与实现结论 |
| 2026-09-13 | 由 `docs/04-engineering/REFERENCE_KITE.md` 迁移至 `docs/08-reference/kite/README.md`，仅调整关联链接 |
