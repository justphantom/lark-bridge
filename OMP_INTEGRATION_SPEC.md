# OMP 对接规范

> 本文档归纳 `lark-bridge` 与 Oh My Pi（omp）CLI 的对接协议、事件格式、配置项、生命周期与接入方式。
> 对应实现：`cmd/omp-back/`、`internal/omp/`、`internal/ompbridge/`。
> 配套文档：`docs/omp-back-design.md`（设计）、`docs/omp-integration.md`（CLI 调研）。

## 1. 整体架构

```
飞书用户 → feishu-front (IPC SSE/POST) → omp-back → omp CLI (-p --mode json)
                                            ↓
                                       router / usage / stream archive
```

- `omp-back` 是 OMP 侧后端进程，**每个 prompt fork 一次 `omp -p --mode json` 子进程**。
- 与前端通过 `internal/backendrpc` 通信：前端以 SSE 推送 `protocol.Event`，后端以 POST 回写 `protocol.Control`。
- `internal/omp` 负责封装 omp CLI 的调用与 NDJSON 事件解析。
- `internal/ompbridge` 负责把 omp 事件翻译成前端协议、管理会话/目录/模型/审批/思考级别绑定、处理斜杠命令。

## 2. 启动与配置

### 2.1 启动参数

```bash
lark-omp-back -config ./omp-config.json
lark-omp-back -version
```

### 2.2 配置字段（`config.OMP`）

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `cli_path` | string | `"omp"` | omp CLI 路径 |
| `default_directory` | string | `"${STATE_DIR}/omp"` | 各群工作目录根；首次 prompt 通常仍需先 `/cd` 选择目录（待确认） |
| `max_concurrent` | int | 4 | 并行 CLI 子进程上限（≥1） |
| `stream_history` | int | 50 | 保留的原始 NDJSON 归档数 |
| `append_system_prompt` | string | `"你的回答应该简洁，通常不超过1000字"` | 每轮追加 system prompt |
| `approval_mode` | string | `"write"` | 默认审批模式（always-ask/write/yolo） |
| `thinking_level` | string | `"auto"` | 默认思考级别（off/minimal/low/medium/high/xhigh/max/auto） |
| `model_options` | []string | nil | `/model` 选择器的**静态兜底**列表；动态 `omp models --json` 失败或为空时使用（模型可用性部署相关，无编译期默认） |
| `model_list_timeout` | duration | `300s` | `omp models --json` 获取超时。该子命令需联网拉取 provider catalog，实测冷启动 ~137s。picker 外层另有 bridgebase `listFnTimeout`(300s) 兜底，设小于此值可让 omp 提前失败而非等满预算 |
| `list_cache_ttl` | int | `3600` | `omp models --json` 结果缓存秒数。0/未设→3600(1h)，负值禁用缓存（每次 fork，~100s+） |
| `agent_dir` | string | `""` | 覆盖 omp 的 session/agent 目录；空值使用 omp 默认（`~/.omp/agent` 或 `PI_CODING_AGENT_DIR`）。仅当需要与 omp 默认路径隔离时显式设置，且必须是已存在的绝对路径 |
| `gc_cold_archive_after_days` | int | `30` | `/session-gc` 调用 `omp gc --cold-archive-after-days`：超过该天数的非当前会话会被归档/清理 |
| `gc_retain_newest_per_cwd` | int | `5` | `/session-gc` 调用 `omp gc --retain-newest-per-cwd`：每个工作目录至少保留的最新的会话数 |
| `gc_timeout` | duration | `300s` | `/session-gc` 执行 `omp gc` 的最大等待时间 |
| `approval_options` | []string | `["always-ask","write","yolo"]` | `/perm` 选择器选项 |
| `thinking_options` | []string | `["off","minimal","low","medium","high","xhigh","max","auto"]` | `/thinking` 选择器选项 |

### 2.3 共享配置

- `backend_id`、`frontend_url`、`ipc_secret`、`router_path`、`state_dir`、`log_*`、`timeouts` 与 claude/opencode-back 共享，见 `config.example.json`。
- 环境变量 `WORKSPACE_ROOT` 限制 `/cd` 选择器范围。
- fork CLI 子进程前，`cmdutil.SanitizeChildEnv()` 会移除包含 `SECRET`、`TOKEN`、`ENCRYPT`、`PASS`、`PRIVATE_KEY`、`CREDENTIAL` 子串的环境变量，防止子进程读取桥接机密；但保留 CLI 自身所需的 `*_API_KEY`。

## 3. 协议层

### 3.1 CLI 调用形态

每轮 prompt 调用：

```bash
omp -p --mode json \
  --approval-mode <mode> \
  --thinking <level> \
  [--append-system-prompt <text>] \
  [--resume <sessionID>] \
  [--model <pattern>] \
  [--tools <list> | --no-tools] \
  [--max-time <dur>] \
  [--cwd <dir>] \
  "<prompt>"
```

- prompt 作为**位置参数**传入（omp `-p` 读 argv，**不读 stdin**，与 claude 不同）。
- `--mode json`：输出 NDJSON 事件流。
- `--approval-mode`：审批策略（**不传** `always-ask` 在非交互 `-p` 下会阻塞等待输入）；默认 `write`（≈ claude `acceptEdits`），`yolo`（≈ `bypassPermissions`）为显式 opt-in。
- `--thinking`：思考级别；默认 `auto`。
- **不传 `--no-session`**：OMP 默认持久化 session，首轮 `session` header 的 `id` 回填 router 后下轮才能 `--resume` 续接；加 `--no-session` 会抑制落盘、使 `--resume` 必然失败（业务 prompt 不加；`IsReady` 健康检查是唯一例外）。
- **不传 `--max-time`**：触发后会以 `stopReason:"aborted"` 收尾并丢弃当前 turn 文本；硬性时限由 ctx + `ApplyGroupCancel` 兜底。
- **不传 `--add-dir`**：v1 router/config 无数据来源（`internal/omp/client.go` buildCommand）。
- 子进程放入独立进程组，`ctx` 取消时 SIGKILL 整组。
- `buildCommand` 对 `ApprovalMode`/`ThinkingLevel` 非空值做枚举防御校验（与 config validate 同值域）。

### 3.2 输出格式：NDJSON

每行一个完整 JSON 对象，顶层 `type` 取值：

| 顶层 type | 含义 |
|---|---|
| `session` | 首行 header，含 `id` / `cwd` / `title` |
| `agent_start` / `agent_end` | 一轮 agent 生命周期（`agent_end` 为终态） |
| `turn_start` / `turn_end` | 单轮 assistant turn 边界 |
| `message_start` / `message_end` | 消息开始/结束（**用量仅在 role=assistant 的 message_end**） |
| `message_update` | 流式增量（按内层 `assistantMessageEvent.type` 分流，见 §5） |
| `tool_execution_start` / `tool_execution_update` / `tool_execution_end` | 工具调用生命周期 |
| `auto_retry_start` | 失败自动重试 |
| `notice` | 运行时通知 |

> `printableEvent` 已主动去掉 `providerPayload` 与 `message_update` 中的完整 `partial` 快照。
> 未捕获异常（无效模型/无 API key）时 stdout 可能混入 minified JS 源码行，parser 必须对不可解析行容错。

## 4. 会话管理

- 会话 ID 从首行 `session` header 的 `id` 提取。
- `router` 按 `chatID` 持久化绑定：`sessionID`、`directory`、`modelSpec`、`permissionMode`、`effortLevel`（思考级别）。
- 首次 prompt 通常要求用户先 `/cd` 选择工作目录；切换目录（`/cd`）会清空并重建会话。
- `--resume <id>` 若 id 不存在，omp 以 exit 1 + stderr `Error: Session "<id>" not found.` 退出；bridge 判定为**过期会话**，清空 `sessionID` 重试一次。
- 会话持久化文件：`{state_dir}/router.v5.json`。
- 用量按 `sessionID` 累计，独立文件：`{state_dir}/usage-omp.json`。

### 4.1 会话操作注意事项

- OMP 的 session 文件以 `{agent_dir}/sessions/<encoded-cwd>/<base>.jsonl` 形式落盘，并附带可选的 `<base>`  sidecar 目录。bridge 直接读取文件头来实现 `/session-list` / `/session-use` / `/session-clean`，无需 fork omp CLI，因此**不会**受 `omp models --json` 那种 ~100s 冷启动的影响。
- `/session-list` 列出当前绑定目录下的所有会话；`/session-use` 切换当前群绑定的会话 ID；`/session-clean` 删除指定或当前目录下的会话（保留当前绑定），并通过权限卡片确认。
- `/session-gc` 调用官方 `omp gc --apply --archive --json` 来整理 session 文件、重建 `history.db` 与 FTS 索引；配置项 `gc_cold_archive_after_days`、`gc_retain_newest_per_cwd`、`gc_timeout` 控制其行为。
- `--resume <id>` 若 id 不存在，omp 以 exit 1 + stderr `Error: Session "<id>" not found.` 退出；bridge 判定为**过期会话**，清空 `sessionID` 重试一次。

## 5. 消息/事件流

`internal/omp.Event` 扁平化后的类型（`internal/omp/event.go`）：

| Event Type | 来源 | 说明 |
|---|---|---|
| `EventSession` | `session` header | 提取 `sessionID`，emit `TypeSessionInit` |
| `EventAgentStart` | `agent_start` | 一轮开始（每 turn + 每 auto_retry 各一次）；bridge 累加 stepCount、emit `TypeProgress` |
| `EventTurnStart` | `turn_start` | 单轮 assistant turn 边界；bridge 在此重置文本累加器，丢弃上一轮 preamble |
| `EventMessageUpdate` | `message_update`(内层 `text_delta`) | assistant 文本增量 |
| `EventThinking` | `message_update`(内层 `thinking_end`) | 完整 thinking 块 |
| `EventMessageEnd` | `message_end` | **用量唯一来源**（仅 role=assistant） |
| `EventToolStart` | `tool_execution_start` | emit `TypeToolUse` |
| `EventToolEnd` | `tool_execution_end` | emit `TypeToolResult` |
| `EventAutoRetry` | `auto_retry_start` | emit `TypeProgress`（"自动重试 #N"），不终止 turn |
| `EventNotice` | `notice` | emit `TypeNotice` |
| `EventAgentEnd` | `agent_end` | 终态：构建 `promptResult` |
| `EventError` | 合成 / `message_end`(stopReason=error) | 子进程崩溃/取消/解析失败/模型错误 |

### 5.1 文本 / thinking 累积策略

- `message_update` 行按内层 `assistantMessageEvent.type` 路由（`event_parse.go` 分流表）：
  - `text_delta` → `EventMessageUpdate`（`delta` 追加到文本累加器）。
  - `thinking_end` → `EventThinking`（`content` 为完整块；bridge emit `TypeThinking` 且 `Replace=true`）。
  - `text_end` / `thinking_delta` / `toolcall_*` / `done` / `error` → **忽略**（`text_end` 与 delta 累积重复会双倍输出；`thinking_delta` 因 Replace 逐块覆盖不可读；工具以 `tool_execution_*` 为权威）。
- 每个 `turn_start` 清空文本累加器：工具调用轮中，agnes/glm 模型在上一轮 emit 内联 thinking preamble（带孤立 `</think>`），不清空会泄漏进最终回复（`StripThinking` 无法挽救，因 OMP 只发闭合标签）。

### 5.2 Token / Cost 累计

- 用量**只在 role=assistant 的 `message_end.message.usage`** 出现（camelCase 字段 `input`/`output`/`cacheRead`/`cacheWrite`/`totalTokens`/`cost.total`）；`message_start` 中全为 0；`agent_end` 实测**无 telemetry**。
- bridge 在 `EventMessageEnd` case 累加 `accInput/accOutput/accCacheRead/accCacheWrite/accCost`，由 `finalizeResult` 统一消费（§A.1 验证）。
- 模型错误以 assistant `message` 的 `stopReason="error"` + `errorMessage` 呈现，parser 合成为 `EventError`（无独立 error 事件）。

### 5.3 终态策略

- 成功：`agent_end`（`isTerminal:true`），退出码 0；最终回复取累加文本经 `StripThinking` 后的结果。
- 失败：`EventError`（合成或模型错误），或子进程无终态事件时 client 补一个。
- omp 流不携带模型名，结果卡回退到 `modelSpec` 或 `"omp"`。

## 6. 工具调用

`tool_execution_start` / `tool_execution_end` 是工具行的权威来源（`message_update` 的 `toolcall_*` delta 被忽略，避免重复 emit）。

- `tool_execution_start`：`toolName` → 工具名；`ToolUsePayload.Input` 在 parser 层取 `intent`（模型声明的调用目的，可读性更好）优先于 `args` JSON，再经 `bridgebase.SummarizeToolInput` 渲染。
- `tool_execution_end`：`result.content[].text` 提取为 `ToolResultPayload.Output`；`isError` → `ToolResultPayload.IsError`。
- OMP 的 todo / subagent 输出结构与 claude/opencode 不同，**v1 按普通工具行展示**，不映射为 `TypeTodo` / `SubagentSummary`（设计 §6.4 / §12）。

## 7. 权限与安全

### 7.1 审批模式（approval_mode）

对应 CLI `--approval-mode`：

| 模式 | 行为 |
|---|---|
| `always-ask` | 每次工具调用都询问（**非交互 `-p` 下会阻塞**，仅作显式 opt-in） |
| `write` | 自动接受编辑类工具，其余需授权（默认，≈ claude `acceptEdits`） |
| `yolo` | 自动通过所有工具调用（≈ `bypassPermissions`，最危险，需显式配置） |

> claude 名称 `bypassPermissions`/`acceptEdits`/`plan` 在 `mapApprovalMode` 被映射为 `yolo`/`write`/`always-ask`，便于从 claude-back 迁移的配置。

### 7.2 工作目录安全

- `PromptPayload.Directory` 等 override 字段禁止由前端直接设置，bridge 在 `handlePromptEvent` 入口校验。
- `/cd` 选择器被 `WORKSPACE_ROOT` 限制为子目录。
- 子进程环境变量经 `cmdutil.SanitizeChildEnv()` 脱敏。
- `buildCommand` 对 `--approval-mode` / `--thinking` 非空值做枚举防御校验；`/perm`、`/thinking` 的 picker 与直接参数路径均校验回写值合法性。

## 8. 交互命令

omp-back 支持以下斜杠命令：

| 命令 | 说明 |
|---|---|
| `/running` | 显示运行中的 omp 会话 |
| `/session-new` | 开启新对话（保留目录，重置上下文） |
| `/session-list` | 列出当前目录下的 omp 会话 |
| `/session-use [n]` | 切换当前群绑定的会话 |
| `/session-clean [id]` | 删除当前目录下其他会话（或指定 id），需确认 |
| `/session-gc` | 调用 `omp gc` 整理归档会话并重建索引 |
| `/session-abort` | 中止当前调用 |
| `/session-del` | 删除当前群绑定的会话 |
| `/current` | 显示当前会话/目录/模型/审批/思考级别 |
| `/model [model\|clear]` | 设置模型 |
| `/perm [mode\|clear]` | 设置审批模式 |
| `/thinking [level\|clear]` | 设置思考级别 |
| `/cd [dir\|clear]` | 切换工作目录（重置会话） |
| `/pull` / `/push` | 在当前目录执行 git 操作 |
| `/send [relative-path]` | 发送工作目录文件到群 |
| `/help` | 帮助 |

> 与 opencode-back 相比：无 `/agent`（omp 无 agent 概念），无 `/session-list` / `/session-use` / `/session-clean`（§4.1）。

### 8.1 交互卡片机制

- `/model` 无参数时先发加载横幅（TypeProgress——`omp models --json` 冷启动需联网拉取 provider catalog，实测 ~137s），再异步发 `Question` 卡片；选项首选动态获取的 `provider/id` selector，获取失败/为空时回退 `model_options` 静态列表（仍带自定义输入框）。`/cd` 发送 `Question` 卡片；`/perm` 发送 `Permission` 卡片；`/thinking` 经 `bridgebase.MakeEnumPicker` 发送 `Question` 卡片。均通过 `AnswerBroker` 阻塞等待用户选择。
- `/perm`、`/thinking` 的卡片选择在回写 router 前再次校验值合法性（防 option 列表误配污染 binding）。
- 等待超时约 9 分钟。
- `/pull`、`/push`、`/send` 每 chat 单飞执行。

## 9. 错误处理

| 错误来源 | 处理方式 |
|---|---|
| 解析失败 | WARN 日志，丢弃该行，继续处理后续行 |
| 模型错误（stopReason=error） | parser 合成 `EventError`，附 `errorMessage` |
| 无终端事件 | client 合成 `EventError`，附 stderr（截断 64KiB） |
| ctx 取消 | 返回 `"已取消"` notice |
| idle 超时 | `IdleTimeout` 内无 stdout 事件则 SIGKILL，返回 `"响应超时"` |
| 会话过期 | `isStaleSessionErr` 识别 "session"+"not found"，清空 sessionID 重试一次 |
| IPC POST 失败 | WARN 日志；中间控制可丢弃，终端控制同步发送 |

## 10. 生命周期与优雅关闭

### 10.1 启动顺序

1. `flag.Parse`
2. `config.Load`：读取 JSON 配置并校验必填字段（含 omp.approval_mode/thinking_level enum 校验）
3. 构建 base logger 与各组件 logger
4. `backendrpc.ValidateBackendConfig`：校验 `ipc_secret` / `backend_id` / `frontend_url`
5. `omp.New`：创建 CLI 客户端
6. `router.New`：加载持久化绑定
7. `usage.New`：打开用量文件
8. `client.IsReady()`：执行 `omp --version` 健康检查（超时 10s）
9. `backendrpc.Connect`：建立 SSE 连接
10. `ompbridge.NewWithLogger`：创建 Handler
11. `backendrpc.StartMetricsLoop`：启动指标上报
12. `backendrpc.Run`：进入事件接收循环

### 10.2 关闭顺序

- 信号触发 `ctx.Done()`，`backendrpc.Run` 退出。
- `defer` 链逆序关闭：
  1. `Handler.Close`：取消运行中 prompt、排空交互卡片等待、最多等 5s、关闭 usage store。
  2. `backendrpc.Client.Close`：关闭 SSE body、释放 HTTP idle 连接。
  3. `router.Router.Close`：最终持久化绑定。
- 原始流归档按 `stream_history` 保留在 `{state_dir}/streams/omp/`。

### 10.3 重连策略

- 初始 SSE 连接失败即退出。
- 运行中 SSE 断开后指数退避：首退 5s，每次翻倍，上限 60s，±50% 抖动。
- 连续 20 次重连失败返回 `ErrGiveUpReconnect`，应由 systemd 等监管器重启。

### 10.4 指标上报

- `backendrpc.StartMetricsLoop` 每 `status_monitor.interval`（默认 60s）向 `/v1/metrics/{backendID}` 推送主机/进程快照。

## 11. 关键数据结构

### 11.1 `protocol.Event`（前端→后端）

```go
type Event struct {
    Type     string          // "prompt" / "answer" / "abort" / "ping"
    PromptID string
    ChatID   string
    Prompt   *PromptPayload
    Answer   *AnswerPayload
    Abort    *AbortPayload
}
```

### 11.2 `protocol.Control`（后端→前端）

常用类型：`session_init`、`text`、`thinking`、`tool_use`、`tool_result`、`result`、`error`、`progress`、`question`、`permission`、`notice`。

### 11.3 `omp.Event`

字段为导出（claude-style），bridge 在 switch 中直接读 `ev.Type`/`ev.SessionID`/`ev.Text`/`ev.Role`/`ev.Attempt`/`ev.InputTokens` 等。一条输入行产出恰好一个 Event。详见 `internal/omp/event.go`。

## 12. 接入示例

### 12.1 最小配置 `omp-config.json`

```json
{
  "ipc_secret":   "${IPC_SECRET}",
  "backend_id":   "omp-1",
  "frontend_url": "${FRONTEND_URL}",
  "state_dir":    "${STATE_DIR}",
  "omp": {
    "cli_path":          "omp",
    "default_directory": "${STATE_DIR}/omp",
    "max_concurrent":    4,
    "approval_mode":     "write",
    "thinking_level":    "auto"
  },
  "log_level": "info",
  "log_debug_redact": true
}
```

### 12.2 启动

```bash
export IPC_SECRET=xxx
export FRONTEND_URL=http://localhost:6060
export STATE_DIR=/var/lib/lark-bridge
./bin/lark-omp-back -config ./omp-config.json
```

### 12.3 验证 CLI 健康

```bash
omp --version
# omp/17.1.8
```

### 12.4 手动测试 NDJSON 流

```bash
omp -p --mode json --model glm-5.2 --approval-mode write --thinking auto "ping"
```

---

**待确认/演进点**：
- `omp.default_directory` 是否作为首次 prompt 的默认目录使用，当前仍需用户先 `/cd` 选择（待确认）。
- OMP 冷启动慢，health check / 首条 prompt 可能贴近 `PromptTimeout`；硬性兜底靠 ctx + `ApplyGroupCancel`（不用 `--max-time`，见 §3.1）。
- `message.usage` 字段随版本可能变动；`agent_end.telemetry` 实测不存在（§A.1），用量唯一来源是 role=assistant 的 `message_end`。
- `--add-dir` / todo-subagent `TypeTodo` 渲染为 v1 未支持项（设计 §5.2 / §6.4）。（动态 `ListModels` **已支持**，见 §8。）
- 文件上传由 `feishu-front` 的 `file_convert` 处理并转为文本 prompt，omp 后端仅接收文本。
