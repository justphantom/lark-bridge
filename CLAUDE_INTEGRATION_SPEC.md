# Claude 对接规范

> 本文档归纳 `lark-bridge` 与 Claude Code CLI 的对接协议、事件格式、配置项、生命周期与接入方式。
> 对应实现：`cmd/claude-back/`、`internal/claude/`、`internal/claudebridge/`。

## 1. 整体架构

```
飞书用户 → feishu-front (IPC SSE/POST) → claude-back → Claude Code CLI
                                            ↓
                                       router / usage / stream archive
```

- `claude-back` 是 Claude 侧后端进程，**每个 prompt fork 一次 `claude` CLI 子进程**。
- 与前端通过 `internal/backendrpc` 通信：前端以 SSE 推送 `protocol.Event`，后端以 POST 回写 `protocol.Control`。
- `internal/claude` 负责封装 Claude CLI 的调用与 `stream-json` 事件解析。
- `internal/claudebridge` 负责把 Claude 事件翻译成前端协议、管理会话/目录/模型绑定、处理斜杠命令。

## 2. 启动与配置

### 2.1 启动参数

```bash
lark-claude-back -config ./claude-config.json
lark-claude-back -version
```

### 2.2 配置字段（`config.Claude`）

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `cli_path` | string | `"claude"` | Claude Code CLI 路径 |
| `permission_mode` | string | `"acceptEdits"` | 默认权限模式 |
| `default_directory` | string | `"${STATE_DIR}/claude"` | 仅保留字段，当前首次 prompt 仍需用户先 `/cd` 选择目录（待确认是否作为默认目录使用） |
| `max_concurrent` | int | 4 | 并行 CLI 子进程上限 |
| `append_system_prompt` | string | `"你的回答应该简洁，通常不超过1000字"` | 每轮追加 system prompt |
| `stream_history` | int | 50 | 保留的原始流归档数 |
| `model_options` | []string | `["haiku","sonnet","opus"]` | `/model` 选择器选项 |
| `permission_options` | []string | `["acceptEdits","plan","bypassPermissions"]` | `/perm` 选择器选项 |
| `effort_options` | []string | `["low","medium","high","xhigh","max"]` | `/effort` 选择器选项 |
| `settings_dir` | string | `"~/.claude"` | `/settings` 扫描目录 |
| `settings_cache_ttl` | int | 3600 | settings 列表缓存秒数 |

### 2.3 共享配置

- `backend_id`、`frontend_url`、`ipc_secret`、`router_path`、`state_dir`、`log_*`、`timeouts` 与 opencode-back 共享，见 `config.example.json`。
- 环境变量 `WORKSPACE_ROOT` 限制 `/cd` 选择器范围。
- fork CLI 子进程前，`cmdutil.SanitizeChildEnv()` 会移除包含 `SECRET`、`TOKEN`、`ENCRYPT`、`PASS`、`PRIVATE_KEY`、`CREDENTIAL` 子串的环境变量，防止 `Bash` 工具读取桥接机密；但保留 CLI 自身所需的 `*_API_KEY`。

## 3. 协议层

### 3.1 CLI 调用形态

每轮 prompt 调用：

```bash
claude -p \
  --output-format stream-json \
  --verbose \
  --permission-mode <mode> \
  [--append-system-prompt <text>] \
  [--resume <sessionID>] \
  [--model <model>] \
  [--effort <level>] \
  [--max-turns N] \
  [--allowedTools <list>] \
  [--disallowedTools <list>] \
  [--add-dir <dir> ...] \
  [--settings <file>]
```

- prompt 通过 **stdin** 传入。
- `-p`（print/stream）模式是非交互式，必须显式设置非阻塞的 `permission_mode`；`default` 权限模式被明确排除，因为它会在 `-p` 模式下挂起。
- `AllowedTools` / `DisallowedTools` 当前在配置和命令中未暴露入口，仅在单元测试中验证过 CLI 参数拼装。
- 子进程放入独立进程组，`ctx` 取消时 SIGKILL 整组，防止孙进程孤儿化。

### 3.2 输出格式：`stream-json`

每行一个 JSON 对象，由 `internal/claude/event_parse.go` 解析。顶层 `type` 取值：

| 顶层 type | 含义 |
|---|---|
| `system` | 系统事件：`init`、`thinking_tokens`、子任务 `task_*` |
| `assistant` | assistant 消息，内含 content blocks |
| `user` | user/tool_result 回显 |
| `result` | 终端结果/错误 |

### 3.3 Content block 类型

`assistant` / `user` 消息的 `message.content` 数组元素类型：

| block type | 说明 |
|---|---|
| `text` |  assistant 文本 |
| `thinking` | reasoning trace |
| `tool_use` | 工具调用请求 |
| `tool_result` | 工具调用结果 |
| `server_tool_use` | 服务端工具（如 webReader），按 `tool_use` 处理 |

## 4. 会话管理

- 会话 ID 从首条 `system/init` 事件的 `session_id` 提取。
- `router` 按 `chatID` 持久化绑定：`sessionID`、`directory`、`modelSpec`、`permissionMode`、`effortLevel`、`settingsFile`。
- 首次 prompt 时要求用户先通过 `/cd` 选择工作目录；`default_directory` 字段当前未作为默认目录使用（待确认）。
- 切换目录（`/cd`）会清空并重建会话。
- 若 `--resume` 返回 `No conversation found with session ID`，bridge 判定为**过期会话**，自动清空 `sessionID` 重试一次。
- 会话持久化文件：`{state_dir}/router.v5.json`。
- 用量按 `sessionID` 累计，独立文件：`{state_dir}/usage-claude.json`。

## 5. 消息/事件流

`internal/claude.Event` 扁平化后的类型（`internal/claude/event.go`）：

| Event Type | 来源 | 说明 |
|---|---|---|
| `EventSystem` | `system/*` | 仅 `init` 用于取 session/model |
| `EventText` | `assistant.content.text` | 文本块 |
| `EventThinking` | `assistant.content.thinking` | thinking 块 |
| `EventToolUse` | `assistant.content.tool_use` | 工具调用 |
| `EventToolResult` | `assistant.content.tool_result` / `user.content.tool_result` | 工具结果 |
| `EventResult` | `result` 行 | 终态结果 |
| `EventError` | 合成 | 子进程崩溃/取消/解析失败 |
| `EventTaskStarted` | `system.task_started` | 子任务开始 |
| `EventTaskProgress` | `system.task_progress` | 子任务进度（local_agent） |
| `EventTaskNotification` | `system.task_notification` | 子任务结束 |

### 5.1 终态策略

- 成功：`result` 行 `is_error=false`，bridge 取**最后一条 assistant message** 的文本作为回复。
- 失败：`result` 行 `is_error=true` 或合成 `EventError`。
- `IsStaleSession()` 识别 `"No conversation found"` 以便前端提示会话过期。

### 5.2 Token / Cost

`result` 行 `usage` 字段：

| 字段 | 含义 |
|---|---|
| `input_tokens` | 输入 token |
| `output_tokens` | 输出 token |
| `cache_read_input_tokens` | cache read |
| `cache_creation_input_tokens` | cache write |
| `total_cost_usd` | 美元费用 |
| `duration_ms` / `duration_api_ms` | 总耗时 / API 耗时 |
| `num_turns` | 轮数 |
| `stop_reason` | 停止原因 |

## 6. 工具调用

- `tool_use` 携带 `id`、`name`、`input`（JSON）。
- `tool_result` 仅携带 `tool_use_id`，bridge 用 `toolNames[tool_use_id]` 反查工具名。
- `TodoWrite` 工具被特殊处理：成功时转为 `protocol.TypeTodo` 控制，失败时以 `TypeToolResult` 显示。
- 子任务（Task/Agent 工具）走 `protocol.TypeToolUse`/`TypeToolResult` + `SubagentSummary` 独立渲染区。

## 7. 权限与安全

### 7.1 权限模式

对应 CLI `--permission-mode`：

| 模式 | 行为 |
|---|---|
| `acceptEdits` | 自动接受编辑类工具，其余需显式授权 |
| `plan` | 只读规划模式，不修改文件 |
| `bypassPermissions` | 跳过所有权限检查，最危险 |

> 默认**不包含** CLI 的 `"default"` 模式，因为它在 `-p` 非交互模式下会永远挂起。

### 7.2 工作目录安全

- `PromptPayload.Directory` 等 override 字段禁止由前端直接设置，bridge 在 `handlePromptEvent` 入口校验。
- `/cd` 选择器被 `WORKSPACE_ROOT` 限制为子目录。
- 子进程环境变量经 `cmdutil.SanitizeChildEnv()` 脱敏，移除 `FEISHU_APP_SECRET`、`IPC_SECRET` 等桥接自身机密。

### 7.3 工具白名单

- `--allowedTools` / `--disallowedTools` 支持逗号分隔列表，如 `"Bash,Read"`。

## 8. 交互命令

claude-back 支持以下斜杠命令：

| 命令 | 说明 |
|---|---|
| `/running` | 显示运行中的 Claude 会话 |
| `/session-list` | 列出本群绑定的会话 |
| `/session-new` | 新对话（保留目录，重置上下文） |
| `/session-abort` | 中止当前调用 |
| `/session-del` | 删除当前群绑定的会话 |
| `/current` | 显示当前会话/目录/模型/权限/effort |
| `/model [model\|clear]` | 设置模型 |
| `/cd [dir\|clear]` | 切换工作目录 |
| `/settings [clear]` | 设置 `--settings` 文件 |
| `/perm [mode\|clear]` | 设置权限模式 |
| `/effort [level\|clear]` | 设置推理级别 |
| `/pull` / `/push` | 在当前目录执行 git 操作 |
| `/send [relative-path]` | 发送工作目录文件到群 |
| `/help` | 帮助 |

### 8.1 交互卡片机制

- `/model`、`/cd`、`/settings`、`/perm`、`/effort` 无参数时发送 `Question`/`Permission` 卡片，通过 `AnswerBroker` 阻塞等待用户选择。
- 选择器使用 `TakeOverProgress` 把原命令的进度卡片变形为选择卡片，保持单卡片交互。
- 等待超时约 9 分钟。
- `/pull`、`/push`、`/send` 每 chat 单飞，git 命令超时 5 分钟，输出保留最后 500 rune。

## 9. 错误处理

| 错误来源 | 处理方式 |
|---|---|
| 解析失败 | WARN 日志，丢弃该行，继续处理后续行 |
| 无终端事件 | 合成 `EventError`，附 stderr |
| ctx 取消 | 返回 `"已取消"` notice |
| 会话过期 | `IsStaleSession` 识别，提示用户新建会话 |
| IPC POST 失败 | WARN 日志；中间控制可丢弃，终端控制同步发送 |

## 10. 生命周期与优雅关闭

### 10.1 启动顺序

1. `flag.Parse`
2. `config.Load`：读取 JSON 配置并校验必填字段
3. `backendrpc.ValidateBackendConfig`：校验 `ipc_secret` / `backend_id` / `frontend_url`
4. `router.New`：加载持久化绑定
5. `usage.New`：打开用量文件
6. `claude.New`：创建 CLI 客户端
7. `backendrpc.Connect`：建立 SSE 连接
8. `claudebridge.NewWithLogger`：创建 Handler
9. `api.IsReady()`：执行 `claude --version` 健康检查，失败 fast-fail
10. `backendrpc.StartMetricsLoop`：启动指标上报
11. `backendrpc.Run`：进入事件接收循环

### 10.2 关闭顺序

- 信号触发 `ctx.Done()`，`backendrpc.Run` 退出。
- `defer` 链逆序关闭：
  1. `Handler.Close`：取消运行中 prompt、排空交互卡片等待、最多等 5s、关闭 usage store。
  2. `backendrpc.Client.Close`：关闭 SSE body、释放 HTTP idle 连接。
  3. `router.Router.Close`：最终持久化绑定。
- 原始流归档按 `stream_history` 保留在 `{state_dir}/streams/claude/`。

### 10.3 重连策略

- 初始 SSE 连接失败即退出。
- 运行中 SSE 断开后指数退避：首退 5s，每次翻倍，上限 60s，±50% 抖动。
- 连续 20 次重连失败返回 `ErrGiveUpReconnect`，应由 systemd 等监管器重启。

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

### 11.3 `claude.Event`

见 `internal/claude/event.go`，字段覆盖 text/thinking/tool/task/result/error 全生命周期。

## 12. 接入示例

### 12.1 最小配置 `claude-config.json`

```json
{
  "ipc_secret":   "${IPC_SECRET}",
  "backend_id":   "claude-1",
  "frontend_url": "${FRONTEND_URL}",
  "state_dir":    "${STATE_DIR}",
  "claude": {
    "cli_path":          "claude",
    "default_directory": "${STATE_DIR}/claude",
    "permission_mode":   "acceptEdits"
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
./bin/lark-claude-back -config ./claude-config.json
```

### 12.3 验证 CLI 健康

```bash
claude --version
```

---

**待确认/演进点**：
- `claude.default_directory` 当前未作为默认工作目录使用，首次 prompt 仍需用户先 `/cd` 选择目录。
- `AllowedTools` / `DisallowedTools` 当前在配置和命令中未暴露入口，仅在单元测试中验证过 CLI 参数拼装。
- Claude CLI 的 `task_notification` 行目前缺失 `task_type`，bridge 用 `taskKinds` 缓存回填；若未来 CLI 补齐，可移除缓存逻辑。
- `server_tool_use` 已按 `tool_use` 处理；若 CLI 新增其他 block 类型，需在 `event_parse_content.go` 增加 case。
- 文件上传由 `feishu-front` 的 `file_convert` 处理并转为文本 prompt，Claude 后端仅接收文本，未直接处理文件二进制。
