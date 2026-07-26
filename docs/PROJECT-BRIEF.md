# unring — 项目说明文档

> **unring** · Agent 副作用可逆层 · MIT License
> 名字出自谚语 *you can't unring a bell*（钟敲了就收不回了）—— 而这个项目的全部承诺就是：现在可以了。

> 本文档用于在**新的对话窗口**中启动实现工作。它包含所有已做的决策、已完成的可行性验证（含实测结果）、以及尚未决定的事项。
>
> 日期：2026-07-25 · 状态：设计与可行性验证完成，未开始实现

---

## 1. 一句话定义

一个**包裹式 CLI**，把 AI coding agent 对真实世界产生的副作用（数据库写入、外部 API 调用）拦截下来。跑完之后你看到「它到底做了什么 / 即将做什么」，然后决定 **commit** 或 **discard**。

**主标语（承诺可逆）**

> **Make everything your agent does undoable.**
> 让 agent 做的每一件事都能反悔。

**技术叙事（三层机制，一个承诺）**

| 层 | 机制 | 可逆性 |
|---|---|---|
| 数据库 | 真实事务，`discard` = `ROLLBACK` | 完全可逆 |
| 可暂存的外部调用 | 根本不发出，只记录 | 完全可逆（因为从未发生）|
| 必须真跑的调用 | 事前批准 + 事后补偿撤销 | 尽力而为，边界明确 |

⚠️ **禁止使用的讲法**：不要说 "dry run" / "preview"（显得没技术含量且不准确 —— 数据库操作是真跑的，agent 看得见真实结果）。核心承诺是**可逆**，不是"预演"。

---

## 2. 目标与硬约束

| 项 | 决定 |
|---|---|
| 成功标准 | **很多人日常在用**（星星优先，不为变现扭曲产品形态）|
| 目标用户 | coding agent 重度使用者（Claude Code / Codex / OpenCode），**本地开发场景** |
| 投入 | 3-6 个月半投入（每周 15-25 小时），首发 10-12 周 |
| 语言 | **Go**（已确认本机 go1.26.4 darwin/arm64 可用）|
| 项目类型 | 给别人的 agent 用的组件 —— **不是** agent 产品本身，**不是** 搭 agent 的平台 |

### 选择这个方向的核心理由（相对于被否掉的方案）

1. **价值一眼可验证** —— 不需要自建 benchmark 去论证效果。改动要么被暂存了要么没有，你要么按了 commit 要么按了 discard。
2. **失败是显式且立刻可见的** —— 不会像"agent 经验/记忆"类项目那样静默地帮倒忙。这是用户明确提出的筛选标准。
3. **论文密集但产品为零** —— Atomix、Cordon、Shepherd、Sandlock（均为 2026 论文）都在啃这个问题，工业侧（Temporal / Restate / Airflow `durable=True`）在收敛到"两阶段 + 幂等键"，但**没有一个能装上就用的开源产品**。
4. **可持续长大** —— 每接一个服务就是一个声明式 adapter，社区可贡献。


---

## 3. 架构决策（全部已确定）

### 3.1 拦截点：协议代理

在**网络协议层**拦截，而不是 MCP 层、系统调用层或 SDK 层。

**理由**：本地开发场景下 agent 的危险副作用几乎全部走网络。协议层是唯一一个既能横跨 `bash` 命令 / 脚本 / MCP 工具三种调用方式、又不依赖平台特性的拦截点。而且代理天然能合成响应。

**文件系统不做** —— 交给 git（worktree + checkout 已经解决了代码文件的暂存与丢弃）。这砍掉了一半工程量，且砍掉的正好是平台相关性最强的那一半（macOS 没有 overlayfs）。

### 3.2 注入方式：包裹式 CLI

```
unring claude          # 包裹任意 agent
unring run -- <任意命令>
```

包裹进程时注入环境变量，把子进程的流量指向本地代理。agent 无感知，不改 agent 配置，不绑定任何 harness。

### 3.3 数据库：共享事务

- 代理连上真实数据库后立刻 `BEGIN`
- agent 的所有读写都在这个事务里 —— **真跑**，看得见自己的改动，拿到真实结果
- **同一会话的所有客户端连接复用同一个后端事务**（关键：解决"agent 跑完 migration 再跑测试"时新连接看不见改动的问题）
- `discard` = `ROLLBACK`，`commit` = `COMMIT`
- 代价：并发请求会被串行化（本地开发可接受，需在文档写明）

### 3.4 写操作三级分类

| 档 | 适用 | 处理 |
|---|---|---|
| **真事务** | 数据库操作 | 真跑，可回滚 |
| **可暂存** | 不依赖返回值（发邮件、Slack 消息、webhook）| 不发出，合成响应，记入待执行清单 |
| **需批准** | 依赖返回值（建 issue 后要评论）、事务内无法执行的 SQL、未知服务 | **停下来问用户**，批准后真跑 |

### 3.5 分类机制：adapter + 启发式，不用 LLM

- 已知服务 → 声明式 adapter（YAML/JSON：哪些端点可暂存、哪些必须真跑、怎么撤销、幂等键怎么算）
- 未知服务 → HTTP 语义启发式，**默认归到「需批准」**
- **不用 LLM 判定** —— 保证永不静默出错。不认识的东西就停下来问。

### 3.6 默认行为：总是开启，只读无感

默认包裹一切。只读会话（未产生任何写）结束时静默退出，不弹任何确认。用户只在真正产生副作用时才看到审阅界面。

---

## 4. 可行性验证结果（已实测，非推测）

验证环境：macOS darwin 25.5.0 / go1.26.4 / PostgreSQL 17.10 / Node v25.9 / Python 3.14.4
验证代码位于：`/private/tmp/claude-501/-Users-hongyuan/cf890460-7872-40cf-9d6d-7bf073eefa46/scratchpad/spike/`（临时目录，可能已清理；如需要可重建）

### ✅ V1 — Postgres 事务可回滚一切（含 DDL）

事务内执行：`CREATE TABLE` + `ALTER TABLE ADD COLUMN` + `DELETE` 1000 行。

- 事务内 agent 看到 2000 行（**读得见自己的写**）
- `ROLLBACK` 后：3000 行完好无损，新建的表消失

**结论：方案基石成立。** 注意这是 Postgres 特有优势 —— MySQL 的 DDL 是隐式提交的，不可回滚（这也是首发不做 MySQL 的技术原因，不只是工作量原因）。

### ✅ V2 — 多连接复用同一事务（最关键的验证）

用 Go + `github.com/jackc/pgx/v5/pgproto3` 写了一个 ~180 行的代理原型，实测：

- 连接 A（模拟跑 migration）：建表 + 删 1000 行，然后断开
- 连接 B（**全新独立连接**，模拟跑测试）：**看得见** A 的改动 ✓
- 真实数据库（绕过代理直连）：**完全看不到** ✓
- 代理收到 SIGINT → `ROLLBACK` → 数据库恢复原状 ✓

**结论：核心架构成立。** 实现要点：代理自己应答客户端握手（`AuthenticationOk` / `ParameterStatus` / `BackendKeyData` / `ReadyForQuery{TxStatus:'T'}`），用全局 mutex 串行化后端访问，客户端 `Terminate` 时**不关事务**。

### ⚠️ V3 — 事务内不能执行的命令（必须处理）

实测以下命令在事务块内**直接报错**：

```
CREATE DATABASE                  ERROR: cannot run inside a transaction block
DROP DATABASE                    ERROR: cannot run inside a transaction block
VACUUM                           ERROR: cannot run inside a transaction block
CREATE INDEX CONCURRENTLY        ERROR: cannot run inside a transaction block
ALTER SYSTEM                     ERROR: cannot run inside a transaction block
```

**影响**：agent 跑 `VACUUM` 或某些 migration 工具时，被我们包进事务会**弄坏本来能跑的活儿**。

**处理**：这些语句归入「需批准」档，用**独立的非事务连接**执行（不可逆，需用户确认）。架构不用改，但**首发必须实现这个 escape hatch**，否则工具会被认为是"坏东西"。

### ⚠️ V4 — 序列不回滚

`ROLLBACK` 后自增序列不回退（实测 3000 → 3050，回滚后仍是 3050）。ID 会留空洞。无害，但**必须写进文档**，否则用户会认为"回滚不干净"。

### ✅ V5 — CA 信任可以只注入到被包裹的进程（重大发现）

这解决了 HTTPS 拦截最大的安装摩擦和心理抵触（"你在中间人攻击我"）。实测结果：

| 运行时 | 环境变量 | 结果 |
|---|---|---|
| Node.js | `NODE_EXTRA_CA_CERTS` | ✅ 信任 |
| curl | `CURL_CA_BUNDLE` | ✅ 信任 |
| Python (stdlib) | `SSL_CERT_FILE` | ✅ 信任 |
| **Go 二进制（macOS）** | `SSL_CERT_FILE` | ❌ **不生效** |

补充：Claude Code 官方支持 `HTTPS_PROXY`、`NODE_EXTRA_CA_CERTS`（以及 mTLS 的 `CLAUDE_CODE_CLIENT_CERT` 等），npm/pnpm 和所有 Node 版 MCP server 同样认 `NODE_EXTRA_CA_CERTS`。

**⚠️ Go 二进制在 macOS 上的限制是真实痛点**：`crypto/x509` 在 darwin 上只查系统钥匙串，忽略 `SSL_CERT_FILE`。而 **`gh` 正是 Go 二进制且本机已装（v2.96.0）** —— agent 用 `gh` 操作 GitHub 是极常见的场景。

处理方案（三选一或组合，**需在实现时决定**）：
- 首发只覆盖 Node/Python/curl 流量（已覆盖 Claude Code 本体、多数 MCP server、agent 写的多数脚本），文档明确说明限制
- 提供可选的一次性钥匙串安装命令（把摩擦变成用户主动选择）
- 对 Go 二进制的目标主机走 CONNECT 透传（能看到主机名但看不到请求体 → 无法暂存，只能记录）

### ⚠️ V6 — 撤销能力的真实边界

- **GitHub**：REST API **没有**删除 issue 的接口。只有 GraphQL `deleteIssue` mutation，且需要管理员权限。→ "撤销已创建的 issue" 只能部分兑现
- **Slack**：`chat.delete` 可用，bot token 可删自己发的消息 ✓
- 通用限制：已发出的邮件、已被人看到的消息**撤不回**

**文档必须明说**，不能把撤销吹成万能。真正的卖点是「大部分副作用根本没有发生过」，撤销只是兜底。

---

## 5. 首发范围（10-12 周）

### 做

1. **包裹式 CLI** — 子进程管理 + 环境变量注入（proxy + CA bundle 变量）
2. **Postgres 代理** — 共享事务 + 多连接复用 + 事务外语句的 escape hatch
3. **HTTPS 代理** — MITM + 本地 CA 生成；GitHub / Slack 两个 adapter + 通用启发式
4. **终端审阅界面** — 展示待提交清单（数据库改动摘要 + 待执行的外部调用），支持 commit / discard
5. **审计日志** — 记录会话真实发生了什么（信任与调试的基础，工程量小）
6. **基础撤销** — 限于有明确逆操作的动作（Slack 删消息等），边界在文档中明确

### 不做（首发明确排除）

- 文件系统 CoW（交给 git）
- MySQL（DDL 不可回滚，价值主张被削弱）
- 团队 / 审批流 / 多人协作（与"个人日常使用"目标不符）
- 多 agent 并发控制（规划中的第二阶段）
- Web UI（终端界面优先）

### 实现顺序建议

先做 Postgres 那条线并跑通端到端（`包裹 CLI → 代理 → 审阅 → commit/discard`），因为它是**零妥协的杀手锏 demo**，且已被验证成立。HTTPS 那条线后做，因为它有 CA 信任的不确定性。

---

## 6. 已知风险清单

| 风险 | 严重度 | 缓解 |
|---|---|---|
| Go 二进制（`gh` 等）在 macOS 不认 `SSL_CERT_FILE` | **高** | 见 V5 三个方案；首发可先只覆盖 Node/Python/curl |
| 扩展查询协议（Parse/Bind/Execute）未实现 | **高** | ORM（Prisma、SQLAlchemy、node-pg）都用扩展协议。原型只支持简单查询协议。**必须实现**，并处理多客户端间 prepared statement 名字冲突（参考 pgbouncer 的已知坑）|
| 事务外语句弄坏正常工作流 | 中 | 见 V3，必须实现 escape hatch |
| 共享事务导致并发串行化 | 中 | 本地开发可接受，文档写明 |
| agent 绕过代理（直连 IP / 自带证书）| 中 | **定位为防事故不防恶意**，文档说明白 |
| 撤销的诚实边界 | 中 | 文档明说，卖点放在"根本没发生"而非"能撤回" |
| 序列不回滚导致 ID 空洞 | 低 | 文档说明 |

---

## 7. 已决事项（原待定项，现已全部确定）

### 7.1 名字：`unring`

出自 *you can't unring a bell*。npm 可用、词罕见所以搜索无歧义、自带故事。CLI 读作 `unring claude`。

### 7.2 License：MIT

理由：涉星型开发者工具的事实标准（OpenCode、OpenClaw 皆 MIT），采用摩擦最低。本项目是本地 CLI 而非托管服务，不存在"云厂商拿去开 SaaS"的风险，因此不需要防御型 license。

### 7.3 Go 二进制 CA 问题：shim + 透传告警

- 已知 CLI 工具（**首发先做 `gh`**）用 **PATH shim** 拦截 —— 因为我们控制子进程 PATH，可在其最前面注入同名 shim。**无需任何证书**，且拿到的是结构化意图（"创建 issue，标题 X"）而非靠猜 HTTP 请求，比 MITM 更可靠
- 其余无法拦截的流量走透传，但**必须在审阅界面明确列出「以下流量未被拦截」**

⚠️ **不可妥协的原则**：静默漏拦是本项目最严重的失败模式。宁可显式告警说"这块我没管住"，也不能让用户以为全被保护了。

### 7.4 adapter 格式：YAML + 表达式

声明式 YAML 为主，需要条件逻辑处（如"同一端点根据 body 不同行为不同"）嵌入表达式（CEL 或 JSONPath，实现时定）。需表达：匹配哪些请求、归入哪一档、幂等键怎么算、撤销操作是什么。

⚠️ **不可妥协的原则**：**内置的 GitHub / Slack adapter 必须用与社区完全相同的格式编写**。一旦内置的走特权路径（比如用 Go 写），社区格式会因无人真正使用而腐烂 —— 这是插件生态最常见的死法。

### 7.5 审阅界面：整体提交 + 逐项查看

只有 `commit` / `discard` 两个决定，但可展开查看每一项细节（diff、受影响行、邮件内容、请求体）。

**不支持部分提交**，理由是它会造出无法推理的不一致状态：数据库本就是单一事务无法部分提交；而"提交数据库改动但不发邮件"会产生「库里记了 `notified_at` 但通知从未发出」这类脏状态。文档需说明这个设计取舍。

---

## 8. 尚未决定的事项

以下留到实现阶段按实际情况决定：

1. 表达式语言选 CEL 还是 JSONPath
2. adapter YAML 的具体字段 schema
3. 终端界面用什么库（Bubble Tea 是 Go TUI 主流选择）
4. `discard` 后是否把反馈交回 agent 重跑（首发不做，但架构上别堵死这条路）

---

## 9. 竞品与参考（研究阶段收集）

**直接前身：`p-e-w/maybe`** —— "See what a program does before deciding whether you really want it to happen"。系统调用级拦截，当年很火。**已停止维护**，且仅支持 Linux、Python 2.7/3.3 实现、只拦截少数几个 syscall（作者自己承认防护不完整）。

它验证了这个概念对开发者的吸引力，同时那个位置现在是空的 —— 而 unring 的差异在于：面向 agent 而非人类手敲的命令、协议层而非 syscall 层、且提供**真正的事务回滚**而不只是阻断。


**同问题域的 2026 论文（有理论无产品）**
- Atomix — 进度感知的事务性工具调用
- Cordon — 工具分发处的语义事务拦截
- Shepherd / Sandlock / Mirage — overlay 文件系统的 commit/abort/keep 三态

**工业侧收敛方向**：两阶段 + 幂等键（Temporal 记录副作用供重放、Restate durable steps、Airflow `AgentOperator(durable=True)`）

**关键市场数据**
- 88% 的 agent pilot 进不了生产
- 2026 agent 相关支出 $206B（+139%）；治理是增长最快的预算行（占 8-12%，2024 仅 3-5%）
- 开发者用 AI 后合并的 PR +98%，但 PR review 时间 +91%

**相邻但不同的项目**（不是直接竞品，但会被拿来比较）：Docker `sbx`、yoloAI、vibebox（沙箱类）；HumanLayer（人类回路，主力已转型 IDE 平台）

---

## 10. 在新窗口开始工作时

建议第一步：重建验证原型并跑通，确认环境一致，然后从「包裹式 CLI + Postgres 代理」开始实现。

需要复现验证环境的话：

```bash
# 启动测试用 Postgres
LC_ALL=C LANG=C initdb -D ./pgdata -U postgres --auth=trust --locale=C
LC_ALL=C pg_ctl -D ./pgdata -o "-p 55432 -k /tmp" -l ./pg.log start

# Go 依赖
go get github.com/jackc/pgx/v5/pgproto3
```

原型代理的关键实现要点已记录在 §4 的 V2 中。
