# OpenCode 对接规范

> 本文档归纳 `lark-bridge` 与 OpenCode CLI 的对接协议、事件格式、配置项、生命周期与接入方式。
> 对应实现：`cmd/opencode-back/`、`internal/opencode/`、`internal/opencodebridge/`。

## 1. 整体架构

```
飞书用户 → feishu-front (IPC SSE/POST) → opencode-back → OpenCode CLI
                                            ↓
                                       router / usage / stream archive
```

- `opencode-back` 是 OpenCode 侧后端进程，**每个 prompt fork 一次 `opencode run` 子进程**。
- 与前端通过 `internal/backendrpc` 通信：前端以 SSE 推送 `protocol.Event`，后端以 POST 回写 `protocol.Control`。
- `internal/opencode` 负责封装 `opencode run --format json` 的调用与 NDJSON 事件解析。
- `internal/opencodebridge` 负责把 OpenCode 事件翻译成前端协议、管理会话/目录/模型/agent 绑定、处理斜杠命令。

## 2. 启动与配置

### 2.1 启动参数

```bash
lark-opencode-back -config ./opencode-config.json
lark-opencode-back -version
```

### 2.2 配置字段（`config.Opencode`）

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `cli_path` | string | `"opencode"` | OpenCode CLI 路径 |
| `default_directory` | string | `"${STATE_DIR}/opencode"` | 各群工作目录根；首次 prompt 通常仍需先 `/cd` 选择目录（待确认） |
| `max_concurrent` | int | 4 | 并行 CLI 子进程上限 |
| `stream_history` | int | 50 | 保留的原始 NDJSON 归档数 |
| `list_cache_ttl` | int | 3600 | `models` / `agents` 列表缓存秒数 |

> 旧版 HTTP 模式字段（`base_url` / `username` / `password`）在配置中保留但已被忽略。

### 2.3 共享配置

- `backend_id`、`frontend_url`、`ipc_secret`、`router_path`、`state_dir`、`log_*`、`timeouts` 与 claude-back 共享，见 `config.example.json`。
- 环境变量 `WORKSPACE_ROOT` 限制 `/cd` 选择器范围。
- fork CLI 子进程前，`cmdutil.SanitizeChildEnv()` 会移除包含 `SECRET`、`TOKEN`、`ENCRYPT`、`PASS`、`PRIVATE_KEY`、`CREDENTIAL` 子串的环境变量，防止子进程读取桥接机密；但保留 CLI 自身所需的 `*_API_KEY`。

## 3. 协议层

### 3.1 CLI 调用形态

每轮 prompt 调用：

```bash
opencode run \
  --format json \
  --auto \
  --thinking \
  [--session <sessionID>] \
  [--model <model>] \
  [--agent <agent>] \
  "<prompt>"
```

- prompt 作为**位置参数**传入，不读 stdin。
- `--format json`：输出 NDJSON 事件流，每行一个 JSON 对象。
- `--auto`：自动批准非显式拒绝的权限，防止 CLI 阻塞等待交互输入。
- `--thinking`：在 json 模式下输出 `reasoning` 事件（默认关闭）。
- 子进程放入独立进程组，`ctx` 取消时 SIGKILL 整组。

### 3.2 输出格式：NDJSON

每行一个完整 JSON 对象，统一信封字段：

```jsonc
{
  "type": "step_start" | "step_finish" | "text" | "reasoning" | "tool_use" | "error",
  "timestamp": 1234567890,   // ms
  "sessionID": "ses_xxx",
  "part": { ... },           // 除 error 外的事件负载
  "error": { ... }           // 仅 error 事件
}
```

### 3.3 事件类型

| type | 含义 | 是否终态 |
|---|---|---|
| `step_start` | 新一轮 agent step 开始 | 否 |
| `text` | assistant 文本输出 | 否 |
| `reasoning` | thinking 块 | 否 |
| `tool_use` | 工具调用完成 | 否 |
| `step_finish` | step 结束，`reason="stop"` 为终态 | 仅 `reason=stop` |
| `error` | 错误事件 | 是 |

> 终止不靠专用 `done` 事件，而靠内部 `session.status.idle` 使 CLI 退出，退出码 0 表示成功，1 表示失败。

## 4. 会话管理

- 会话 ID 从首条事件行的 `sessionID` 字段提取（1.18+ 每行都携带，不再单独发 `session.created`）。
- `router` 按 `chatID` 持久化绑定：`sessionID`、`directory`、`modelSpec`、`agent`。
- 新 prompt 若未绑定目录，在 `default_directory/<chatID>` 创建；切换目录（`/cd`）会重置会话。
- 会话持久化文件：`{state_dir}/router.v5.json`。
- 用量按 `sessionID` 累计，独立文件：`{state_dir}/usage-opencode.json`。

### 4.1 会话操作注意事项

- `opencode session list` 只列出**当前工作目录**下创建的 sessions，跨目录需在每个目录单独调用。
- `opencode session delete <sessionID>` 删除指定 session。
- `opencode export <sessionID>` 可获取完整累计 token/cost，但 CLI 启动慢（8–15s），bridge 选择自己按 turn 累加。

## 5. 消息/事件流

`internal/opencode.Event` 扁平化后的类型（`internal/opencode/event.go`）：

| Event Type | 来源 | 说明 |
|---|---|---|
| `EventSession` | `session.created` | 保留但 1.18+ json 模式通常不触发 |
| `EventStepStart` | `step_start` | 新 step，重置文本累加器 |
| `EventText` | `text` | 文本块 |
| `EventThinking` | `reasoning` | thinking 块 |
| `EventToolUse` | `tool_use` | 前向兼容，当前主要走 `EventToolResult` |
| `EventToolResult` | `tool_use`（status 终态） | 工具调用结果 |
| `EventStepFinish` | `step_finish`（非 stop） | 中间 step 的 token/cost |
| `EventResult` | `step_finish`（reason=stop） | 终态结果 |
| `EventError` | `error` 或合成 | 错误/崩溃/取消 |

### 5.1 文本处理

- 每个 `step_start` 会清空文本累加器，确保最终回复只取最后一步的文本（避免把中间工具调用前的 preamble 拼到答案）。
- 若 `step_finish` 的 `result` 为空，回退到累加的文本。

### 5.2 Token / Cost 累计

- `step_finish` 分为 `reason="tool-calls"`（中间步）和 `reason="stop"`（终态）。
- bridge 必须累加所有 `step_finish` 的 `tokens` 和 `cost`，否则工具密集轮次会丢失约 96% 的 input token。
- `tokens` 字段结构：

```jsonc
{
  "total": 1234,
  "input": 500,
  "output": 700,
  "reasoning": 34,
  "cache": { "read": 1792, "write": 0 }
}
```

### 5.3 终态策略

- 成功：最后一条 `step_finish(reason="stop")`，退出码 0。
- 失败：`error` 事件或退出码非 0，bridge 合成 `EventError`。
- opencode 流不携带模型名，结果卡回退到 `modelSpec` 或 `"opencode"`。

## 6. 工具调用

`tool_use` 事件 `part` 结构：

```jsonc
{
  "type": "tool",
  "tool": "bash" | "read" | "write" | "edit" | "task" | "todowrite" | ...,
  "callID": "call_xxx",
  "state": {
    "status": "completed" | "error",
    "input": { ... },
    "output": "...",
    "error": "...",           // status=error 时出现
    "title": "ls",
    "metadata": {
      "exit": 0,              // bash 专用
      "truncated": false,
      "sessionId": "...",     // task 专用
      "model": { "modelID": "..." }  // task 专用
    },
    "time": { "start": ..., "end": ... }
  }
}
```

### 6.1 重要约定

- `status="completed"` **不表示命令成功**；bash 命令失败时 `status` 仍为 `"completed"`，需检查 `metadata.exit != 0`。
- `status="error"` 用于工具框架级失败（超时、权限拒绝等）。
- `edit` 工具的 `metadata.filediff` 用于显示 `+N -M`。
- `todowrite` 工具成功时转为 `protocol.TypeTodo` 控制。
- `task` 工具是 OpenCode 的子代理委派，bridge 提取 `SubagentMeta` 转为 `protocol.SubagentSummary` 独立渲染区。

### 6.2 子代理（`task` 工具）

opencode 把整个子代理生命周期**坍缩成单条 `tool_use` 事件**：

| 字段来源 | 含义 |
|---|---|
| `input.subagent_type` | 子代理类型（explore/general/...） |
| `metadata.sessionId` | 子会话 ID |
| `metadata.model.modelID` | 子代理模型 |
| `metadata.truncated` | 结果是否截断 |
| `time.start/end` | 耗时 |
| `output`（剥 XML 后） | 完整产出，bridge 取前 ~200 字作 preview |

## 7. 权限与安全

- `--auto` 模式自动批准非显式拒绝的权限请求；不带 `--auto` 时 CLI 会阻塞等待交互输入。
- 子进程环境变量经 `cmdutil.SanitizeChildEnv()` 脱敏，移除桥接自身机密。
- `/cd` 选择器被 `WORKSPACE_ROOT` 限制为子目录。
- `PromptPayload.Directory` 等 override 字段禁止由前端直接设置。

## 8. 交互命令

opencode-back 支持以下斜杠命令：

| 命令 | 说明 |
|---|---|
| `/running` | 显示运行中的 opencode 会话 |
| `/session-new` | 新对话（保留目录，重置上下文） |
| `/session-abort` | 中止当前调用 |
| `/session-del` | 删除当前群绑定的会话 |
| `/session-list` | 列出当前工作目录下的 sessions |
| `/session-use [n]` | 切换到同目录下其他 session |
| `/session-clean [sessionID]` | 清理会话 |
| `/current` | 显示当前会话/目录/模型/agent |
| `/model [model\|clear]` | 设置模型 |
| `/agent [agent\|clear]` | 设置 agent |
| `/cd [dir\|clear]` | 切换工作目录 |
| `/pull` / `/push` | 在当前目录执行 git 操作 |
| `/send [relative-path]` | 发送工作目录文件到群 |
| `/help` | 帮助 |

### 8.1 交互卡片机制

- `/model`、`/agent`、`/cd`、`/session-use` 无参数时发送选择卡片，通过 `AnswerBroker` 阻塞等待用户选择。
- 等待超时约 9 分钟。
- `/pull`、`/push`、`/send` 每 chat 单飞执行。

## 9. 错误处理

| 错误来源 | 处理方式 |
|---|---|
| 解析失败 | WARN 日志，丢弃该行，继续处理后续行 |
| `error` 事件 | `error.name` / `error.data.message` 提取人类可读信息 |
| 无终端事件 | 合成 `EventError`，附 stderr |
| ctx 取消 | 返回 `"已取消"` notice |
| idle 超时 | `IdleTimeout` 内无 stdout 事件则 SIGKILL，返回 `"响应超时"` |
| IPC POST 失败 | WARN 日志；中间控制可丢弃，终端控制同步发送 |

## 10. 生命周期与优雅关闭

### 10.1 启动顺序

1. `flag.Parse`
2. `config.Load`：读取 JSON 配置并校验必填字段
3. 构建 base logger 与各组件 logger
4. `backendrpc.ValidateBackendConfig`：校验 `ipc_secret` / `backend_id` / `frontend_url`
5. `opencode.New`：创建 CLI 客户端
6. `router.New`：加载持久化绑定
7. `usage.New`：打开用量文件
8. `client.IsReady()`：执行 `opencode --version` 健康检查（超时 30s，因 CLI 加载 provider/config 较慢）
9. `backendrpc.Connect`：建立 SSE 连接
10. `opencodebridge.NewWithLogger`：创建 Handler
11. `backendrpc.StartMetricsLoop`：启动指标上报
12. `backendrpc.Run`：进入事件接收循环

### 10.2 关闭顺序

- 信号触发 `ctx.Done()`，`backendrpc.Run` 退出。
- `defer` 链逆序关闭：
  1. `Handler.Close`：取消运行中 prompt、排空交互卡片等待、最多等 5s、关闭 usage store。
  2. `backendrpc.Client.Close`：关闭 SSE body、释放 HTTP idle 连接。
  3. `router.Router.Close`：最终持久化绑定。
- 原始流归档按 `stream_history` 保留在 `{state_dir}/streams/opencode/`。

### 10.3 重连策略

- 初始 SSE 连接失败即退出。
- 运行中 SSE 断开后指数退避：首退 5s，每次翻倍，上限 60s，±50% 抖动。
- 连续 20 次重连失败返回 `ErrGiveUpReconnect`，应由 systemd 等监管器重启。

### 10.4 指标上报

- `backendrpc.StartMetricsLoop` 每 `status_monitor.interval`（默认 60s）向 `/v1/metrics/{backendID}` 推送主机/进程快照，供 `status-monitor` 生成总览卡。

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

常用类型：`session_init`、`text`、`thinking`、`tool_use`、`tool_result`、`result`、`error`、`progress`、`todo`、`question`、`permission`、`notice`。

### 11.3 `opencode.Event`

字段为非导出，通过 `GetType()`、`GetSessionID()`、`GetText()`、`GetToolName()`、`GetInputTokens()`、`GetSubagentMeta()` 等访问器读取。

### 11.4 `opencode.SubagentMeta`

```go
type SubagentMeta struct {
    Type         string
    ChildSession string
    Model        string
    DurationMs   int64
    OutputBytes  int
    Truncated    bool
}
```

## 12. 接入示例

### 12.1 最小配置 `opencode-config.json`

```json
{
  "ipc_secret":   "${IPC_SECRET}",
  "backend_id":   "opencode-1",
  "frontend_url": "${FRONTEND_URL}",
  "state_dir":    "${STATE_DIR}",
  "opencode": {
    "cli_path":          "opencode",
    "default_directory": "${STATE_DIR}/opencode",
    "max_concurrent":    4
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
./bin/lark-opencode-back -config ./opencode-config.json
```

### 12.3 验证 CLI 健康

```bash
opencode --version
```

### 12.4 手动测试 NDJSON 流

```bash
opencode run --format json --auto --thinking "ping"
```

---

**待确认/演进点**：
- OpenCode CLI 版本迭代可能新增事件类型；`event_parse.go` 的 `default` 分支会保留未知事件用于调试。
- `session.created` 已不在 json 模式出现，`EventSession` 分支主要作前向兼容。
- `opencode.default_directory` 是否作为首次 prompt 的默认目录使用，当前仍需用户先 `/cd` 选择（待确认）。
- `/send` 端到端流程是否已完整落地（设计已定，需核对运行分支）。
- `status_report` 与 `POST /v1/metrics` 在新旧前端混合部署时的降级行为：先部署 feishu-front，再部署后端。
- bridge 层目前未维护允许/拒绝工具白名单，全部依赖 CLI 的 `--auto` 策略与 `WORKSPACE_ROOT` 目录边界。
