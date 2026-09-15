# 文档索引

本目录是 `k_cockpit` 的**全部项目文档**。AI 代理在开始任何任务前应先阅读 `AGENTS.md`，再按需查阅本目录。

---

## 目录结构

```
docs/
├── README.md                      # 本文件：索引与维护规范
├── 01-product/                    # 产品层：做什么、为谁做
│   ├── PRD.md                     # 产品需求文档（功能清单 F-x-xx 与优先级）
│   ├── CAPABILITY_MAP.md          # 能力地图：分层、依赖关系、跨切面、覆盖度对照
│   └── ROADMAP.md                 # 路线图与里程碑
├── 02-architecture/               # 架构层：怎么做
│   ├── ARCHITECTURE.md            # 整体架构设计
│   ├── TECH_STACK.md              # 技术选型与理由
│   ├── DATA_MODEL.md              # 数据模型
│   └── FRONTEND.md                # 前端设计：信息架构、设计系统、逐页设计
├── 03-api/                        # 接口层：对外契约
│   └── API.md                     # 接口规范与清单
├── 04-engineering/                # 工程层：怎么协作
│   ├── CODING_STANDARDS.md        # 编码规范
│   ├── DEVELOPMENT.md             # 本地开发指南
│   ├── TESTING.md                 # 测试规范
│   ├── GIT_WORKFLOW.md            # 分支与提交流程
│   ├── TROUBLESHOOTING.md         # 排障与运维规范
│   └── REFERENCE_*.md             # 已迁移至 08-reference/（仅保留跳转存根）
├── 05-ai/                         # AI 层：如何与 AI 协作
│   ├── AI_DEVELOPMENT.md          # AI 驱动开发规范
│   ├── PROMPTS.md                 # 提示词库
│   └── CONTEXT.md                 # 上下文管理策略
├── 06-decisions/                  # 决策层：为什么这么选
│   ├── README.md                  # ADR 说明
│   └── 0001-record-architecture-decisions.md
├── 07-specs/                      # 规格层：单个功能怎么做
│   ├── README.md                  # 规格说明
│   └── _TEMPLATE.md               # 规格模板
└── 08-reference/                  # 参考层：外部参考项目的功能与实现资料
    ├── README.md                  # 参考项目总览：使用原则、只读与同步
    ├── qvmconsole/                # QVMConsole：工程概览 / 工程架构 / 功能清单 / 详细功能清单(接口·任务) / 功能结论 / 踩坑清单
    └── kite/                      # kite：功能与实现结论
```

---

## 阅读路径

**新人 / AI 首次接触项目**：

1. [`../AGENTS.md`](../AGENTS.md) — 项目规则与红线（必读）
2. [`01-product/PRD.md`](01-product/PRD.md) — 项目要做什么（功能清单与优先级）
3. [`01-product/CAPABILITY_MAP.md`](01-product/CAPABILITY_MAP.md) — 有哪些能力、什么关系、先做谁
4. [`02-architecture/ARCHITECTURE.md`](02-architecture/ARCHITECTURE.md) — 整体怎么组织
5. [`04-engineering/DEVELOPMENT.md`](04-engineering/DEVELOPMENT.md) — 本地怎么跑起来
6. [`05-ai/AI_DEVELOPMENT.md`](05-ai/AI_DEVELOPMENT.md) — 与 AI 协作的正确姿势

**准备开发一个功能**：

1. 在 [`01-product/CAPABILITY_MAP.md`](01-product/CAPABILITY_MAP.md) 定位能力归属与**前置依赖**
2. 在 [`01-product/PRD.md`](01-product/PRD.md) §4 取功能编号、优先级与边界说明
3. 在 [`07-specs/`](07-specs/README.md) 找到或编写功能规格
4. 查阅 [`02-architecture/ARCHITECTURE.md`](02-architecture/ARCHITECTURE.md) 确认归属模块
5. 查阅 [`04-engineering/CODING_STANDARDS.md`](04-engineering/CODING_STANDARDS.md) 与 [`TESTING.md`](04-engineering/TESTING.md)
6. 涉及接口 → [`03-api/API.md`](03-api/API.md)
7. 涉及界面 → [`02-architecture/FRONTEND.md`](02-architecture/FRONTEND.md)（页面设计、设计令牌与交互约定）
8. 需要借鉴外部实现 → [`08-reference/README.md`](08-reference/README.md) 及各参考项目资料（[QVMConsole](08-reference/qvmconsole/README.md) / [kite](08-reference/kite/README.md)）

**排查问题 / 操作运行环境**：

1. [`04-engineering/TROUBLESHOOTING.md`](04-engineering/TROUBLESHOOTING.md) — 排障纪律、定界方法、systemd 检查清单与案例复盘
2. 参考项目对应条目 → [`08-reference/README.md`](08-reference/README.md) 下各项目的 `pitfalls.md`（已踩过的坑）

---

## 文档维护规范

### 命名

- 目录使用 `两位数字-英文小写` 前缀，保证排序稳定。
- 固定文档使用**大写英文**名（如 `ARCHITECTURE.md`），便于辨识。
- 非固定数量的文档使用**小写连字符**名（如 `07-specs/user-login.md`）。
- ADR 使用 `NNNN-动词短语.md`，四位序号从 `0001` 起，**永不重复、永不删除**。

### 内容原则

- **单一事实来源**：一个事实只在一处描述，其他地方用链接引用，不复制粘贴。
- **面向读者**：面向 AI 的文档要自包含、无歧义；面向人的文档要说明「为什么」。
- **及时更新**：改动需求/架构/接口后同步更新文档（见 `AGENTS.md` 第 8 节）。
- **状态标记**：尚未确定的内容用 `<!-- TODO: ... -->` 标注，不要留空段落。

### AI 写入约束

AI 代理创建文档时必须遵守：

- 不要在根目录新增零散 `.md` 文件，一律放入本目录对应分区。
- 不要修改历史 ADR，需要变更时新建 ADR 并注明取代关系。
- 不要删除本文档树中已有的文件；如需废弃，在文件顶部标注 `> 状态：已废弃，见 xxx`。
