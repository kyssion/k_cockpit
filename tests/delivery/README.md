# 交付测试的自包含资产

本目录存放**不依赖 `~/.kc_test/` 外部脚本**的、可随代码交付复跑的测试资产，
用于关闭 [`DELIVERY_TEST_PLAN.md`](../../docs/04-engineering/DELIVERY_TEST_PLAN.md)
的 G-13（测试资产未随代码交付）中可自足的部分。

真机的 26 套件编排（Python）仍位于 `~/.kc_test/`，**未入库**；它们的环境前提、
复位步骤与证据索引记录在 [`DELIVERY_TEST_REPORT.md`](../../docs/04-engineering/DELIVERY_TEST_REPORT.md)。
Go 侧与前端侧的常规测试分别按 `TESTING.md` 用 `go test ./...`、`pnpm test` 运行。

## 前置

- 控制面已启动且可访问（默认 `http://127.0.0.1:18080`）；
- 已登录管理员，Cookie 存为 Netscape 格式 jar（默认 `data/lab2/jar.txt`）；
- `python3` ≥ 3.9，无第三方依赖（只用标准库）。

## 用法

```bash
# 1) 日志端点真实功能验证（TP-14 / TC-19-4，19 条断言）
KC_BASE=http://127.0.0.1:18080 \
KC_COOKIE=./data/lab2/jar.txt \
KC_FORBIDDEN_LITERAL="<本环境的开发期万能码，可选>" \
python3 tests/delivery/check_log_endpoints.py

# 2) §6.8 四份机械对账（路由 / 能力 / 矩阵 / 证据）
KC_ROOT="$(pwd)" \
KC_RECONCILE_OUT=./data/lab2/reconcile.json \
python3 tests/delivery/reconcile.py
```

`reconcile.py` 不要求控制面在线：节点能力页读取失败时会在报告里记为 `ERROR`，
其余三份对账仍会产出结果。

## 覆盖内容

| 脚本 | 覆盖 | 判定 |
|---|---|---|
| `check_log_endpoints.py` | `/settings/log/status`、`read`（含 level / keyword 过滤）、`export`（脱敏）、`level`（含审计）、`delete`、`stream`（SSE 首帧） | 19 条断言全过；任一失败退出码 1 |
| `reconcile.py` | 340 条域路由与 `API.md` / `publicAPIRoutes` / `adminAPIRoutes` 对账；131 项能力与节点实际上报、控制面投影对账；94 个 `F-x-xx` 与追溯矩阵、46 条前端路由对账；R1/R2/R3 证据盘点 | 输出 orphan 清单，不预设"全绿" |

## 口径

- **端点与权限的唯一权威是 `internal/router/routes_*.go`**；`API.md` 是契约登记，
  `/api-endpoints` 运行产物与真机报告只作历史证据。
- 对账里出现的「有路由无文档」需逐条判定文档滞后还是实现缺漏，不允许静默忽略。
- 脚本不写真实 IP、账号、口令或证书；连接地址与 Cookie 路径均由环境变量注入。
