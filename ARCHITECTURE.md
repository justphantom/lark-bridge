# lark-bridge 项目架构说明文档

> 本文档基于仓库 `github.com/justphantom/lark-bridge` 实际代码归纳。
> 最近核对：2026-08-14 | 基线：v1.14.0（HEAD = v1.14.0-36-g13cd729）

---

## 1. 项目概述

### 1.1 定位与目标

**lark-bridge** 是一个把 **飞书（Lark/Feishu）群聊桥接到本地编程 agent** 的中间层服务。用户在飞书群里 @机器人，即可驱动本地的 miniagent（独立项目 `github.com/justphantom/miniagent` 的 CLI 二进制，LLM 直调）完成编码任务，并把 agent 的流式进度、工具调用、最终回复渲染成飞书交互卡片。

- 仓库：`github.com/justphantom/lark-bridge`
- 当前版本：**v1.14.0**（git tag）
- 核心问题：飞书开放平台 ↔ 本地 CLI agent 之间没有原生通道；直接把 agent 暴露给飞书缺少鉴权、并发、流式渲染、会话路由等能力
- 解决思路：采用 **1 前端 + N 后端** 的拆分架构（`README.md`、`CHANGELOG.md`）

### 1.2 核心能力

| 能力 | 实现 |
|---|---|
| 飞书长连接收事件 / REST 发卡片 | 自实现 WebSocket + REST 客户端（`internal/lark/`） |
| chat→后端路由（Layer-1） | `internal/router/` + `internal/feishufront/routing.go` |
| 双向 IPC 协议（SSE + POST） | `internal/protocol/` + `internal/backendrpc/` + `internal/feishufront/ipcserver*.go` |
| agent CLI 子进程驱动 | `internal/miniclient/`（fork miniagent 二进制 + NDJSON 事件流） |
| 流式进度卡 / 权限卡 / 问答卡 / 结果卡 | `internal/feishufront/renderer/` + `cardkit/` |
| 斜杠命令体系 | 各后端 `commands*.go`（如 `internal/miniagent/commands*.go`）+ `internal/cmdutil/` |
| systemd 部署 + 二进制 tarball | `deploy/deploy.sh`、`Makefile` |

### 1.3 规模

- **222 个 Go 文件，约 47,944 行**，其中生产代码 **约 24,941 行**，测试代码 **约 23,003 行（约 48.0%）**
- `internal/` 下 **19 个顶层子包**（`go list` 计 25 个含嵌套）
- `go.mod` **零外部依赖**（无 `go.sum`），仅用 Go 标准库

---

## 2. 技术栈

| 维度 | 选型 | 证据 |
|---|---|---|
| 语言 | Go 1.25.0 | `go.mod:3` |
| 外部依赖 | **无**（仅标准库） | `go.mod` 无 `require`；无 `go.sum` |
| 飞书对接 | **自实现** RFC 6455 WebSocket 客户端 + 手写 protobuf 帧编解码 + REST | `internal/lark/` |
| 日志 | 标准库 `log/slog` 封装 | `internal/log/logger.go:29`（`type Logger = slog.Logger`） |
| 配置 | JSON + `${VAR}` 环境变量展开 | `internal/config/config.go:435` |
| HTTP/IPC | 标准库 `net/http`（SSE 长连接 + POST） | `internal/feishufront/ipcserver_sse.go:16` |
| 构建工具 | GNU Make（git 版本号注入 `-X main.version`） | `Makefile:35`、build targets `Makefile:49-67` |
| 部署 | systemd unit + shell 脚本 | `deploy/deploy.sh` |
| 静态检查 | golangci-lint（`.golangci.yml`） | 根目录 |
| 测试 | `go test -race ./...`，表驱动 + fake 注入 | `Makefile:109` |
| CI 工具脚本 | `scripts/openapi_to_md.py`（拉取飞书 OpenAPI 生成参考文档） | `scripts/` |

**历史依赖变迁**：bridge 早期曾内置一个 claude 后端（把 `claude-go-sdk` 内联为 `internal/claude/`，配合 `internal/claudebridge`、`internal/clibase`、`internal/backendhost`）；该后端连同这些包已被整体移除。当前唯一的编码 agent 后端是 miniagent，由 `internal/miniclient/` 通过 fork 外部 `miniagent` 二进制、读 stdout NDJSON 驱动（无 SDK、无内联协议库）。飞书侧客户端为自实现（`internal/lark/`）。最终实现"零外部依赖"——`go.mod` 仅 module 行 + `go 1.25.0`，无 `go.sum`。

---

## 3. 目录结构

```
lark-bridge/
├── cmd/                      # 3 个二进制入口
│   ├── feishu-front/         # 前端：飞书 WS Bot + IPC server + 调度器
│   ├── miniagent-back/       # miniagent (LLM 直调) 后端
│   └── status-monitor/       # 周期性总览卡推送（独立部署，push-only）
├── internal/                 # 内部包（详见第 5 节）
│   ├── protocol/             # Event/Control 协议（纯结构 + Validate）
│   ├── router/               # chatID ↔ 后端绑定持久化
│   ├── config/               # JSON 配置加载/默认值/校验
│   ├── log/                  # slog 封装 + 统一字段名
│   ├── lark/                 # ★ 自实现飞书客户端（WS + REST + protobuf）
│   │   ├── websocket/        #   RFC 6455 WS 客户端
│   │   └── ws/               #   帧编解码 + 重连 + 分片重组
│   ├── feishu/               # lark.Client 的业务封装层（Bot/IncomingMessage）
│   ├── feishufront/          # ★ 前端核心（最大包）
│   │   ├── cardkit/          #   飞书卡片元素 schema
│   │   └── renderer/         #   progress/result/interactive 渲染器
│   ├── backendrpc/           # 后端↔前端 IPC 客户端（SSE + 重连）
│   ├── bridgebase/           # ★ 后端通用脊梁（包级 helper：cancel/answers/commands/git/emit）
│   ├── miniagent/            # miniagent-back 业务逻辑（handler + 命令）
│   ├── miniclient/           # miniagent CLI 子进程封装（fork + NDJSON）
│   ├── eventmetrics/         # 每 turn 计量（duration/tokens/incomplete）
│   ├── statusmonitor/        # 周期性总览卡数据组装（push-only）
│   ├── hostmetrics/          # 主机 CPU/内存/磁盘采样
│   ├── fileconvert/          # docx/xlsx/pptx → Markdown 提取
│   ├── gosafe/               # panic-recovered goroutine 启动器（统一 GoSafe 模式）
│   ├── usage/                # 每 session token/cost 累计
│   ├── streamarchive/        # 每轮 CLI stdout 原文落盘（带保留期）
│   ├── cmdutil/              # 斜杠命令公共框架 + 子进程组取消
│   ├── atomicwrite/          # tmp+fsync+rename 原子写
│   └── strutil/              # 截断/脱敏/环境变量工具
├── deploy/                   # 部署脚本 + 配置模板 + systemd 示例
├── scripts/                  # openapi_to_md.py（外部工具，.gitignore）
├── bin/                      # 编译产物（gitignore）
├── config.example.json       # 配置模板
├── Makefile                  # build/test/vet/fmt/deploy/pack
├── go.mod                    # 仅 module 行 + go 1.25.0（无 go.sum）
├── CHANGELOG.md              # 历史版本记录
├── README.md                 # 项目速览
├── NOTICES.txt               # 上游依赖归属
└── .golangci.yml             # lint 规则
```

**顶层目录职责表**

| 目录 | 职责 | 备注 |
|---|---|---|
| `cmd/` | 3 个二进制的 `main.go`（每个含 `main_test.go` 覆盖错误路径） | 入口极薄，组装 internal |
| `internal/` | 全部业务代码 | 不对外暴露 |
| `deploy/` | `deploy.sh`（业务 2 服务：feishu/miniagent）、`deploy-status.sh`（独立）、`*.json` 配置模板、`env.example`、`README.md` | 部署真源 |
| `scripts/` | 单个 Python 脚本（拉取飞书 OpenAPI） | 工具，非运行时 |
| `bin/` | `make build` 产物（3 个二进制） | gitignore |

---

## 4. `cmd/` 入口点

3 个二进制共享一致的骨架：`flag.Parse → config.Load → buildLogger → 校验 IPC 三件套 → 组装依赖 → signal.NotifyContext → 阻塞运行`。入口都极薄（~130-270 行），仅做依赖注入。

| 二进制 | 入口文件 | 产物名 | 职责 | 关键依赖装配 |
|---|---|---|---|---|
| **feishu-front** | `cmd/feishu-front/main.go:64` | `lark-feishu-front` | 持有飞书 WS Bot，提供 IPC server，分发消息与卡片回调 | `feishu.NewBotWithLogger` (:110) + `feishufront.NewLayer1Router` (:124) + `NewBackendRegistry` (:130) + `NewIPCServer` (:131) + `NewTurnManager` (:138) + `NewDispatcher` (:139) |
| **miniagent-back** | `cmd/miniagent-back/main.go:39` | `lark-miniagent-back` | 每 prompt fork 一次 `miniagent` 二进制（独立项目） | `backendrpc.ValidateBackendConfig` (:70) + `router.MigrateLegacyBindings` (:116) + `router.New` (:117) + `miniclient.New` (:141) + `client.IsReady` 健康门 (:157) + `backendrpc.Connect` (:177) + `miniagent.New` (:189) + `backendrpc.StartMetricsLoop` (:201) + `backendrpc.RunWithClient` (:226) |
| **status-monitor** | `cmd/status-monitor/main.go` | `lark-status-monitor` | 按 `status_monitor.interval` 轮询 `GET /v1/status`，向绑定群推送常驻总览卡（PATCH/重发）；push-only | `statusmonitor.New` + `backendrpc.Run`（独立部署） |

> **注意**：`cmd/` 下共 3 个二进制。其中 `status-monitor` 需独立刷新，由 `deploy-status.sh` 管理，不纳入 `deploy.sh` 的 2 个业务服务（feishu / miniagent）。

`version` 变量由 Makefile 的 `-ldflags "-X main.version=$(VERSION)"` 注入（`Makefile:35`，miniagent-back 的 `version` 声明在 `cmd/miniagent-back/main.go:37`），`git describe --tags --always --dirty`。

---

## 5. `internal/` 模块职责详述

按规模（行数）降序，共 22 个包。

### 5.1 `feishufront/`（约 17,000 行，54 文件）——前端核心

前端的所有业务逻辑集中地，是体量最大的包。

| 文件 | 职责 |
|---|---|
| `dispatcher.go` | **`Dispatcher` 编排器**（:89）。接收飞书消息与后端 Control，路由到对应卡片。核心方法 `DispatchIncoming`（:371）、`updateCard`（:357）；`ChatRouter` 接口（:80） |
| `dispatcher_control.go` | `DispatchControl`（:20）：把后端 Control 分派到 `updateProgress`（:130） / `sendResult`（:287） / `sendNoticeControl` / `sendInteractive`（:95） |
| `dispatcher_interactive.go` | 问答/权限卡生命周期 |
| `dispatcher_backend.go` | `/backend` 选择卡处理 |
| `ipcserver.go` | **`IPCServer`**（:24）：HTTP 服务，含 Bearer 鉴权（`subtle.ConstantTimeCompare` :179/:307）+ 每 IP 限流（`authFailuresCap=256` :107） |
| `ipcserver_sse.go` | `GET /v1/events` SSE handler（:16），注册后端、流式推送 Event |
| `ipcserver_control.go` | `POST /v1/control/{backendID}` handler（:21） |
| `ipcserver_health.go` | 后端健康检查（静默超时驱逐） |
| `registry.go` | **`BackendRegistry`**（:138）：backendID → `BackendConn` 映射 + Control 通道（`ReceiveControl` :473，`Controls` :483） |
| `turn.go` | **`TurnManager`**：每轮对话（prompt）的状态机，`/v1/status` 数据源 |
| `routing.go` | `Layer1Router`：chatID → backendID 路由（持久化 `routing.json`） |
| `dedup.go` | 三套 TTL+LRU 去重集（eventIDs/actionIDs/terminals），防重放 |
| `debouncer.go` | 卡片 PATCH 合并器（500ms 窗口），防飞书 API 限流 |
| `form.go` | 表单解析 |
| `cardkit/` | 飞书卡片 schema 的纯结构定义（Header/Footer/元素） |
| `renderer/` | 四类卡片渲染：`progress.go`（流式进度，含工具行/todo/banner）、`result.go`（终态结果）、`interactive.go`（问答/权限）、`progress_snapshot.go`/`progress_category.go`（内部状态） |

### 5.2 `lark/`——自实现飞书客户端

v1.3.0 的核心改动。

| 文件 | 职责 |
|---|---|
| `client.go` | **`Client`**（:55）：高层组合，`Start` 阻塞跑 WS（:105），`Send`/`PatchMessage` 走 REST；`handlerSinkAdapter.OnMessage`（:229）转 lark 事件 |
| `auth.go` | `tokenManager`：tenant_access_token 获取 + 提前 5 分钟刷新 |
| `rest.go` | `restClient`：通用 HTTP + IM API（SendMessage/PatchMessage），区分业务错误码（230025 内容过大 → `ErrCardContentRejected`） |
| `types.go` | `Mention`/`SendInput`/`SendResult`/`CardActionEvent`/`MessageReceiveEvent` |
| `doc.go` | 包文档 |
| `websocket/dial.go` + `conn.go` | **RFC 6455 客户端最小实现**（client-side only，含 mask/ping/pong/分片重组） |
| `ws/frame.go` | **手写 protobuf** Frame 编解码（`Frame` :25，`Marshal` :106 / `Unmarshal` :148） |
| `ws/wsclient.go` | WS 客户端主循环（bootstrap → dial → receiveLoop → pingLoop） |
| `ws/reconnect.go` | 指数退避重连（按服务端 ClientConfig.ReconnectInterval/Count） |
| `ws/dispatcher.go` | 分片重组（`reassembler`，按 message_id+seq）+ 事件路由 |
| `ws/session.go` | 会话状态 |

### 5.3 `miniagent/`——后端业务

miniagent-back 的业务逻辑。Handler 复用 `bridgebase` 的包级 helper（`PromptCancel`、`AnswerBroker`、`Commands`、`EmitTerminalControl` 等），**不嵌入任何 Core 结构**——bridgebase 已不再导出 `Core` 类型，只提供包级工具。

| 文件 | 职责 |
|---|---|
| `handler.go` | **`Handler`**（:46）+ `New`（:85）+ `HandleEvent` 派发（:146，Prompt/Answer/Abort/Ping，ping 回 Pong 走 `gosafe.Go` :170）。`sendCtrl`（:269）/ `sendTerminalCtrl`（:286，走 `bridgebase.EmitTerminalControl` 终态重试） |
| `handler_cli.go` | **`runViaCLI`**（:26）：单 turn 核心——fork miniagent、流式收 NDJSON 事件、emit 终态 Control。`emitCLIEvent`（:112）把 `miniclient.Event` 翻译成 `protocol.Control`；`activeTurnConfig`（:317）解析 per-chat binding |
| `handler_lifecycle.go` | **turn 生命周期**：`startTurn`/`endTurn`（:44/:70，busy-then-drop）、`Close`（:126，幂等 `sync.Once` + 5s grace）、`abortChat`（:153）、`RunningSessions`（:26）、`sendTurnStarted/Finished`（:86/:106） |
| `handler_todo.go` | 每 turn todo 累加器（miniagent 的 todo 工具单条输出 → 卡片 todo 区快照） |
| `picker.go` | 模型/目录 picker 公共逻辑 |
| `commands.go` | 斜杠命令表 + 派发（:23-52），复用 `bridgebase.Commands` |
| `commands_picker.go` | `/model` `/cd` `/config` `/effort` 选择卡 |
| `commands_send.go` | `/send` 文件投递（复用 `bridgebase.BuildSendOptions`/`ReadFilePayload`） |
| `commands_task.go` | `/pull` `/push` `/build`（复用 `bridgebase.TaskRunner`，原 `GitRunner`） |
| `commands_config.go` | `/config` 切换配置文件 |
| `commands_settings.go` | `/maxiter` `/new` 等 |
| `commands_misc.go` | `/current` `/help` 等 |

miniagent-back 支持的斜杠命令：`/current` `/model` `/cd` `/config` `/effort` `/maxiter` `/new` `/send` `/pull` `/push` `/build` `/running` `/abort` `/help`（`/running` `/abort` 在 `HandleEvent` 内早于 startTurn 派发，不占 turn 槽）。

### 5.4 `bridgebase/`——后端通用脊梁

**包级 helper 集合**（无 `Core` 结构）：各 agent 后端按需调用，不通过嵌入共享状态。miniagent 自己持有 router/rpc/logger/cancelBy，只在需要时调用下列工具。

| 文件 | 职责 |
|---|---|
| `core.go` | **`PromptCancel`**（:11）：cancel 条目（CancelFunc/StartTime/ChatID/PromptID），各后端自管 chatID→PromptCancel 映射 |
| `prompt_result.go` | **`EmitTerminalControl`**（:33）：终态 Control 的 send-error 重试路径（miniagent 无状态，已删 AckRegistry，纯重试） |
| `answer.go` | **`AnswerBroker`**（:20）/ `NewAnswerBroker`（:26）：问答/权限卡的 RequestID → chan 请求-应答配对 |
| `interactive.go` | `PickAnswerValue`（:10）：picker 应答取值（`AskAndWait`/`EmitNotice` 等交互发送逻辑已迁至 `miniagent/commands_send.go`/`commands_picker.go`） |
| `commands_send.go` | `/send` 文件工具：`SafeJoin`/`BuildSendOptions`/`ParseSendOption`/`ReadFilePayload` |
| `dir_cache.go` | `DirCache`（:29）/ `NewDirCache`：`/cd` 工作目录扫描缓存 |
| `taskrunner.go` | **`TaskRunner`**（原 `GitRunner`）/ `AcquireAndRun`：`/pull` `/push` `/build` 每 chat 单飞（内部 goroutine 走 `gosafe.Go` :93），支持任意命令执行 |
| `running.go` | `FormatDuration`（:10） |
| `toolinput.go` | `SummarizeToolInput`（:19）：工具输入摘要（提取 command/file_path 等） |
| `linereader/` | 有上限的行读取器（miniclient pump 用于 NDJSON stdout） |

> 注：历史 `ack_registry.go`（终态 ACK 跟踪）、`commands.go`（泛型 dispatcher）、`gosafe.go`、`util.go` 已删——ACK 机制整体移除（`ad84f98`）、命令 dispatch 迁至 `miniagent/commands_dispatch.go`、goroutine 启动统一走顶层 `internal/gosafe/`、`ResolveModel`/`ParseTodoItems` 等 helper 随用随并。

### 5.5 `miniclient/`——CLI 驱动层

封装 miniagent CLI 子进程的 fork + NDJSON 事件流解析。是已移除的 `internal/claude` 的对应物（但 miniagent 是外部二进制，不是内联 SDK）。

| 文件 | 职责 |
|---|---|
| `client.go` | **`Client`**（:58）/ `New`（:72）：fork miniagent 二进制。`Run`（:300）启动子进程、信号量限并发（`defaultMaxConcurrent=4`）、ctx 取消走进程组 SIGKILL（`cmdutil.ApplyGroupCancel`）。`buildArgs`（:371）拼 CLI flags（`-provider/-model` 配对、`-config`、`-workdir`、`-thinking`、`-max-iterations`、`-session/-save-session`；`-mode` 已随 miniagent v5.0.0 删除）。`pump`（:450）读 stdout 行 + 捕获 stderr。`IsReady`（:250）启动健康门（`miniagent --version`，最低 `5.1.0`——v5.0.0 删 `-mode` 为 breaking CLI 契约变更硬切至 5.0.0，v5.1.0 为兼容跟进：result 事件增 `compacted`/`thinking_downgraded` 布尔字段）。`DetectVersion`（:170）/ `satisfiesVersion` 组件数值比较 |
| `event.go` | **`Event`**（:40）+ 事件 kind 常量（:9-28：`tool_use`/`tool_result`/`text_delta`/`reasoning_delta`/`result`/`error`/`session`）+ `parseEvent`（:132，NDJSON 行解码，未知 type 不中断 pump） |
| `models.go` | `ModelRef`（:36）+ `ListModels`（:60）：跑 `miniagent -list-models`，按行解析 `{"type":"model","provider","model"}`，返回 provider/model 配对 |

### 5.6 `feishu/`——飞书业务封装层

`lark/`（协议层）与 `feishufront`（前端业务）之间的适配层。
- `bot.go`：**`Bot`**（:97）封装 `*lark.Client`，对外签名稳定（`NewBotWithLogger`/`Start`/`Stop`/`SendCard`/`UpdateCard`/`OnIncoming`/`OnCardAction`）。`ShouldExitUnhealthy`（:301）看门狗判定。`feishuClient` interface（:18）便于测试注入 fake。
- `bot_dispatch.go`：`handleMessageReceive`/`handleCardAction` 把 lark 事件转成 `IncomingMessage`/`CardAction`。
- `bot_send.go`：`SendCard`/`SendText`/`UpdateCard`，`isCardContentRejected` 错误识别。
- `mention.go`：`Mention` 类型 + `StripMentionPlaceholders`。

### 5.7 `backendrpc/`——后端 IPC 客户端

| 文件 | 职责 |
|---|---|
| `client.go` | **`Client`**（:180）：SSE 长连接读 Event + POST 写 Control。`Connect`（:260）、`readSSE`（:378）、`RecvEvent`（:463）、`SendControl`（:480）、`Status`（:628）、`Close`（:661）；事件经 `eventCh`（:198） |
| `reconnect.go` | **`Run`**（:58）/ `RunWithClient`（:84）：带指数退避 + 抖动的重连循环，`ErrGiveUpReconnect`（:47）连续失败放弃 |
| `config_validate.go` | `ValidateBackendConfig`：IPC 三件套校验 |

### 5.8 `config/`

| 文件 | 职责 |
|---|---|
| `config.go` | **`Config`** 顶层结构（:32）+ 各后端子结构（StatusMonitor/MiniAgent，无 Claude/Agnes 字段）+ `Load`（:468）/ `LoadWithWarnings`（:477）+ `expandEnvVars`（:435，5 步 pipeline 见 :10 注释） |
| `config_defaults.go` | `applyDefaults`（:14）：所有零值字段的默认值 |
| `config_validate.go` | `validate`（:37）：跨字段一致性校验 |

### 5.9 `protocol/`——双向协议

**纯结构 + Validate，无业务逻辑**。
- `protocol.go`：**`Event`**（:18，前端→后端，SSE）+ payload（Prompt/Answer/Abort/Ping/TurnStarted/TurnFinished）。`PromptPayload.HasFrontendOverride`（:79）是安全护栏。
- `protocol_control.go`：**`Control`**（:6，后端→前端，POST）+ 14 类 payload（含 `TypeFile`：后端→前端投递文件；`QuestionPayload.UpdateMessageID`：原地刷新既有 picker 卡；`TypeTurnStarted`/`TypeTurnFinished` 用于运行中会话对账）。
- `protocol_validate.go`：enum 校验（todo.status/priority、notice.level 等）。
- `status.go`：`StatusSnapshot`（/v1/status 响应）。

### 5.10 `router/`——chatID 绑定持久化

- `router.go`：**`Router`**（:47）+ **`Binding`**（:26，SessionID/Directory/ModelSpec/Provider/Thinking/MaxIterations/ConfigFile 的并集）。
- `persistence.go`：单 worker save 合并器（`saveLoop` :164，`saveAsync` :149，`save` :98；load/save 走 atomicwrite）。
- `accessors.go`/`binding.go`：Get/Set/Lookup。`Router.Close`（`router.go:120`）关 saveLoop 后同步 save 一次防丢失。

### 5.11 其余工具包

| 包 | 职责 |
|---|---|
| `fileconvert/` | docx/xlsx/pptx → Markdown 提取（流式 zip/XML 解析，GFM 表格，宽表降 CSV） |
| `cmdutil/` | 斜杠命令公共框架：`Spec`（:61）、`Result`（:48）、`Timeout=15s`（:22）、纯 helper；`spawn_group.go` 子进程组 SIGKILL 取消（`ApplyGroupCancel`） |
| `usage/` | 每 session token/cost 累计，`Delta`（usage.go:42），atomicwrite 持久化，`defaultSessionTTL=7d`（:31）+ `pruneInterval` 定期 prune（:38） |
| `log/` | slog 封装 + 统一字段名常量（`FieldChatID` 等，logger.go:43）+ `LevelVar` 支持热更（logger.go:68） |
| `streamarchive/` | 每 turn NDJSON stdout 落盘到 `{state_dir}/streams/{backend}/`，`NewSink`（sink.go:59），保留期裁剪。**含敏感数据**（`README.md` 警告，可用 `stream_archive_redact` 字段级脱敏） |
| `atomicwrite/` | tmp+fsync+rename+dirfsync（`Write` atomicwrite.go:33），崩溃安全。`open_flags_unix.go`/`_other.go` 平台分叉（O_NOFOLLOW） |
| `strutil/` | `Truncate`（rune 边界，strutil.go:17）、`redact.go`（脱敏）、`env.go`（`EnvVarPattern`，env.go:11，config 与 strutil 共享避免漂移） |
| `hostmetrics/` | 主机 CPU/内存/磁盘采样（/proc 解析，`CollectHost` hostmetrics.go:39），供 statusmonitor 总览卡 |
| `statusmonitor/` | 周期性总览卡数据组装（push-only，无 IPC 入站） |
| `eventmetrics/` | 每 turn 计量（duration/input/output tokens/incomplete），供 SLO 聚合 |

---

## 6. 核心数据流 / 调用链

以"用户在飞书群 @机器人 发消息 → miniagent-back 处理 → 回复卡片"为例，标注关键文件:行号。

### 6.1 配置加载（所有二进制共享）

```
cmd/*/main.go: main()
   └─ config.Load(cfgPath)                              internal/config/config.go:468
        ├─ os.ReadFile
        ├─ expandEnvVars(raw)                            :435  ← ${VAR} 展开，空值报错
        ├─ json.NewDecoder + DisallowUnknownFields       :497  ← 拼写错误硬拒绝
        ├─ applyDefaults(&cfg, path)                    config_defaults.go:14
        └─ validate(&cfg)                               config_validate.go:37
```

### 6.2 前端启动（`cmd/feishu-front/main.go:83 run()`）

```
config.LoadWithWarnings ................................ :84
buildLogger(cfg) ....................................... :95
feishu.NewBotWithLogger(appID, appSecret, logger, ...) .. :110   ← 内部 lark.NewClient
feishufront.NewLayer1Router(routingPath) ................ :124   ← routing.json 持久化
feishufront.NewBackendRegistry() ....................... :130
feishufront.NewIPCServer(registry, cfg.IPCSecret) ...... :131
feishufront.NewTurnManager() ........................... :138
feishufront.NewDispatcher(bot, registry, turns, router)  :139
  └ SetDedupConfig ...................................... :143
signal.NotifyContext(SIGINT, SIGTERM) .................. :168
dispatcher.InitDebouncer(ctx, 500ms) ................... :172
dispatcher.StartDedupPrune(ctx) ........................ :175
router.StartPrune(ctx, 14d) ............................ :179
ipc.SetOnOffline / SetOnOnline / SetInFlightTurns ...... :187/:188/:194
bot.OnIncoming(dispatcher.DispatchIncoming) ............ :218
bot.OnCardAction(dispatcher.DispatchCardAction) ........ :219
go ipc.StartHealthCheck(ctx, ...) ...................... :230
go { control pump: registry.Controls() → DispatchControl } :234-253  ← recover 防 panic
go ipc.Listen(listenAddr) .............................. :264   ← HTTP server
go bot.Start(ctx) ..................................... :273   ← 阻塞主 select
go { WS 看门狗: ShouldExitUnhealthy → os.Exit(1) } .... :292
select { ctx.Done | ipcErr | botErr } .................. :304   ← 优雅停机
```

### 6.3 入站消息流（飞书 → 后端）

```
飞书开放平台
   │ WebSocket 帧（protobuf）
   ▼
lark.ws.wsclient.receiveLoop ..................... internal/lark/ws/wsclient.go
   │ Frame.Unmarshal（手写 protobuf）.............. internal/lark/ws/frame.go:148
   │ reassembler 重组分片 ......................... internal/lark/ws/dispatcher.go
   ▼
lark.Client → handlerSinkAdapter.OnMessage ...... internal/lark/client.go:229
   ▼
feishu.Bot.handleMessageReceive .................. internal/feishu/bot_dispatch.go
   │ （构造 IncomingMessage）
   ▼
feishu.Bot.onIncoming.Load() → Dispatcher.DispatchIncoming  internal/feishufront/dispatcher.go:371
   ├─ isStale(msg.CreateTimeMs) 丢弃过期 ........... :375
   ├─ eventIDs.Add(msg.EventID) 去重 ............... :378
   ├─ MsgType != "text" → handleTextMessage ........ :433
   ├─ StripMentionPlaceholders ..................... :434
   ├─ /backend 命令 → handleBackendCommand ......... :442
   ├─ /skill 前缀剥离（skill=true） ................. :449
   └─ dispatchPrompt ............................... :465
        ├─ router.Resolve(chatID) → backendID ...... :469   ← Layer-1 路由
        ├─ registry.BackendType(backendID) 检查在线 . :473
        ├─ renderer.NewProgressState + Render ...... :486  ← 占位进度卡
        ├─ bot.SendCard(chatID, card, replyTo) .....        ← 发卡片到飞书群
        └─ registry.SendEvent(backendID, Event{Prompt})     ← 推入 BackendConn.eventCh
        ▼
        IPCServer.handleSSE 从 BackendConn.eventCh 读 ... internal/feishufront/ipcserver_sse.go:16
        │ 按 `data: <json>\n\n` 帧写给 SSE 长连接
        ▼
        后端的 backendrpc.Client.readSSE 接收 ....... internal/backendrpc/client.go:378
        │ json.Unmarshal → Event
        ▼
        Client.eventCh → RecvEvent ................. :463
```

### 6.4 后端处理（以 miniagent-back 为例）

```
backendrpc.RunWithClient(ctx, rpc, connOpts, handle) .. cmd/miniagent-back/main.go:226
   │ 复用已 Connect 的 rpc 作为 SSE + control/metrics POST 的唯一 client
   ▼
handle(ctx, *Event) = h.HandleEvent ................... cmd/miniagent-back/main.go:228
   ▼
miniagent.Handler.HandleEvent ........................ internal/miniagent/handler.go:145
   ├─ Prompt → 校验
   │     ├─ HasFrontendOverride 拒绝前端覆写字段 .... protocol.go:79（安全护栏）
   │     ├─ /running /abort 前置派发（不占 turn 槽）. handler.go:207/:217
   │     ├─ 斜杠命令 → handleSessionCommand ........ :228
   │     ├─ busy-then-drop 并发检查 ................. handler_lifecycle.go:44 (startTurn)
   │     └─ GoSafe → runTurn ....................... handler.go:244
   │          └─ runViaCLI ........................ handler_cli.go:26
   │               ├─ activeTurnConfig 取 model/provider/workdir/thinking/config :28
   │               ├─ streamarchive.NewSink 落盘 NDJSON :40
   │               ├─ client.Run(ctx, RunOptions{...}) :55 → fork miniagent 子进程
   │               │    └─ miniclient/client.go:300（信号量限并发，ctx 取消 SIGKILL 进程组）
   │               ├─ for ev := range events: emitCLIEvent :89/:93
   │               │    └─ 转 protocol.Control{ToolUse/ToolResult/Thinking/Todo/Result/Error}
   │               │       ├─ sendCtrl（非终态）.............. handler.go:268 → rpc.SendControl POST
   │               │       └─ sendTerminalCtrl（终态 Result/Error）handler.go:286
   │               │            └─ bridgebase.EmitTerminalControl .. bridgebase/prompt_result.go:47
   │               │                 └─ rpc.SendControl POST ........ backendrpc/client.go:480
   │               └─ ctx 取消且无终态事件 → "已中止" Notice :105
   ├─ Answer → Answers.Deliver（权限/问答回调）...... handler.go:163
   ├─ Abort → abortChat → cancel turn ctx ........... handler_lifecycle.go:153
   └─ Ping → sendCtrl(TypePong) 在派发循环内回 ...... handler.go:169
```

### 6.5 Control 流（后端 → 飞书卡片）

```
后端 rpc.SendControl POST /v1/control/{backendID}      backendrpc/client.go:480
   ▼
IPCServer.handleControl ........................... internal/feishufront/ipcserver_control.go:21
   ├─ authOK（Bearer + 限流）...................... ipcserver.go:165
   └─ registry.ReceiveControl(RoutedControl) ...... registry.go:473
        ▼
        registry.ctrlCh（buffered）
        ▼
前端 control pump goroutine（main.go:234）读取
   ├─ recover() 防 panic 挂掉 pump
   ▼
Dispatcher.DispatchControl(ctx, rc) ............... dispatcher_control.go:20
   ├─ TypeSessionInit/ToolUse/ToolResult/Progress/Todo/Thinking
   │     → updateProgress → renderer 渲染 → updateCard（经 debouncer）:119
   ├─ TypeResult → sendResult ..................... :214
   ├─ TypeError/TypeNotice → sendNoticeControl .... :297
   │     （terminals 去重，PromptID 维度）.......... :45
   └─ TypeQuestion/TypePermission → sendInteractive  dispatcher_interactive.go:23
        ▼
bot.UpdateCard / SendCard → lark REST PatchMessage / SendMessage
   ↓
飞书卡片更新（用户实时看到工具行翻动、最终结果卡）
```

### 6.6 关键并发与生命周期

- **TurnManager**（`feishufront/turn.go`）：每 promptID 一个 Turn 状态，`/v1/status` 数据源。
- **cancelBy**（`miniagent/handler.go:64`）：chatID → `*bridgebase.PromptCancel` 映射，miniagent 自管；busy-then-drop 由 `startTurn`（handler_lifecycle.go:44）把关。
- **Close 顺序**（`miniagent/handler_lifecycle.go:126`）：`appCancel` → 置 `closed` → 遍历 cancelBy 取消所有 in-flight turn → `Answers.Drain` → `wg.Wait`（5s grace），幂等 via `sync.Once`。
- **router.Close**（`router/router.go:106`）：关 saveLoop → 同步 save 一次（防丢失）。

---

## 7. 配置体系

### 7.1 加载 pipeline（config.go:10 注释）

`readRaw → expandEnvVars → json.Unmarshal(DisallowUnknownFields) → applyDefaults → validate`

### 7.2 顶层结构（`config.example.json` + `config.go:32`）

| 字段 | 所属二进制 | 说明 |
|---|---|---|
| `feishu_app_id` / `feishu_app_secret` | feishu-front | 飞书自建应用凭证（`${VAR}` 引用） |
| `feishu_domain` | feishu-front | `feishu`/`larksuite`，默认 `feishu` |
| `feishu_log_level` | feishu-front | info/Warn/Debug/Error |
| `ipc_secret` | 前后端共享 | Bearer 令牌，空值前端拒绝启动 |
| `ipc_addr` | feishu-front | 监听地址，默认 `localhost:6060` |
| `ipc_tls_*` | feishu-front / 后端 | TLS 证书 / mTLS 客户端证书（跨机部署） |
| `backend_id` | 后端 | 在前端 registry 的唯一 ID |
| `frontend_url` | 后端 | 前端 IPC 地址 |
| `router_path` | 共用 | router 持久化文件路径 |
| `miniagent{}` | miniagent-back | api_key/model/provider/max_iterations/stream_history/workspace_root/stream/thinking/key_file/config_path/config_dir |
| `status_monitor{}` | status-monitor | interval |
| `log_level`/`log_output`/`log_format`/`log_debug_redact`/`stream_archive_redact` | 共用 | 日志与流归档脱敏 |
| `component_log_levels{}` | 共用 | 分组件级别（router/feishu/dedup/miniagent/status_monitor 等） |
| `state_dir` | 共用 | 持久化根目录 |
| `timeouts{}` | 共用 | backend_health（其余历史字段已随死代码清理移除） |
| `dedup{}` | feishu-front | stale_window/event_ttl/event_max_entries |

### 7.3 配置特点

1. **`${VAR}` 环境变量展开**（`config.go:435`）：机密走环境变量不进 JSON；未设置或空值报错退出；值做 JSON escape 防 secret 破坏 JSON 结构。
2. **DisallowUnknownFields**（`config.go:497`）：拼错字段名硬拒绝，避免"已改未生效"陷阱。
3. **单文件共享 or 分文件**：进程各取所需字段，跨二进制必填校验在各自 `main.go`（如 miniagent-back 校验 `api_key`/`model`/`workspace_root`/`config_path`，`cmd/miniagent-back/main.go:77-103`）。
4. **Duration 自定义类型**（`config.go:243`）：JSON 编码为 `"5m"` 而非纳秒，负值拒绝。

---

## 8. 部署与运行

### 8.1 运行形态：**1 个长驻前端 + N 个长驻后端 + 独立部署/状态监控**

不是 CLI 工具，而是 **2 个业务长驻 systemd 服务**（feishu / miniagent，由 `deploy.sh` 管理）+ 独立的 status-monitor（独立脚本管理）。

```
飞书用户 ←→ 飞书开放平台 ←→ feishu-front (WS Bot + IPC SSE)
                                    ↕ SSE/POST (Bearer 鉴权)
   ┌──────────────┬──────────────────┐
miniagent-back  status-monitor
(fork miniagent)(周期总览卡 push)
                 ↑独立部署
```

### 8.2 部署矩阵

| 命令 | 作用 | 范围 |
|---|---|---|
| `make build` | 编译 3 二进制到 `bin/`，注入 git 版本号 | 本机 |
| `make pack [GOOS= GOARCH=]` | 交叉编译 + 打 tarball（`bin/lark-bridge-<ver>-<os>-<arch>.tar.gz`） | 分发 |
| `make deploy` | 调 `deploy/deploy.sh`，只装机：把 `bin/` 已有产物装成 **2 个业务服务**（feishu / miniagent）；不再触发编译，缺产物会提示先 `make build` | systemd |
| `make deploy ARGS=--init` | 首次：从示例生成 config.json + .env | systemd |
| `make deploy ARGS=--services miniagent` | 只部署子集（合法值：`feishu miniagent`） | systemd |
| `make deploy ARGS=--binaries <tar>` | 从 tarball 部署（目标机免 Go） | systemd |

### 8.3 分布式部署

`ipc_addr` 监听非 loopback + `frontend_url` 指前端机即可前后端分机（`--services feishu` / `--services miniagent` 各占一机）。IPC 为明文 HTTP，跨机限可信内网；跨不可信网络走 TLS（`ipc_tls_*`）或 SSH 隧道/wireguard。

### 8.4 健康检查

- 前端：`curl localhost:6060/v1/events` 应返回 401（鉴权拦截）。
- 后端：启动时 `client.IsReady` fail-fast——跑 `miniagent --version` 并校验最低版本（`cmd/miniagent-back/main.go:157`，`miniclient/client.go:250`）；缺失或过旧直接退出，不注册前端。
- WS 看门狗：`ShouldExitUnhealthy`（`feishu/bot.go:301`），长时间无健康信号 → `os.Exit(1)` 交 systemd 拉起。
- 应用级心跳（C2）：前端 healthTick 向 `lastSeen` 过期或有未应答 ping 的后端发 `TypePing`，后端在**事件派发循环内**回 `TypePong` control（miniagent `HandleEvent`，handler.go:169）。前端按 conn 计 `missedPongs`：连续 3 个 ping 无 pong 即判消费循环死锁并驱逐——`lastSeen` 只证明 SSE 管道可写（ping 的 flush 自己就会刷新它），无法发现 handler 卡死。`TypePong` 由 `ipcserver_control.go` 拦截，不进 dispatcher pump。

---

## 9. 架构特点 / 设计亮点

### 9.1 分层清晰

```
协议层 (protocol)          纯结构 + Validate，零业务
   ↑↓
传输层 (lark / backendrpc / feishufront.ipcserver)   自实现 WS/REST/SSE
   ↑↓
路由层 (router / feishufront.routing)                chatID ↔ backend 绑定
   ↑↓
业务层 (feishufront.dispatcher / miniagent)  卡片渲染 + agent 驱动
   ↑↓
适配层 (miniclient)                         miniagent CLI 子进程封装
```

`internal/feishu/` 是 `lark/`（协议）与 `feishufront`（业务）之间的刻意适配层，下游零改动。

### 9.2 协议优先（Protocol-First）

`internal/protocol/` 是前后端契约的单一真源：
- **Event**（前端→后端，SSE）：Prompt/Answer/Abort/Ping。
- **Control**（后端→前端，POST）：14 种含 Text/Result/ToolUse/ToolResult/Thinking/Todo/Question/Permission/Notice/Progress/File/TurnStarted/TurnFinished...
- **`HasFrontendOverride`**（`protocol.go:79`）：运行时护栏——前端管道不得设置 Directory/ModelSpec 等"信任源专属"字段，否则被后端拒绝（防任意 CWD 注入）。
- 协议纯结构，新接入 agent 后端只需实现 Control 发送。

### 9.3 后端通用工具脊梁（bridgebase）

bridgebase 已不再导出 `Core` 结构，改为**包级 helper 集合**：`PromptCancel`（cancel 条目）、`AnswerBroker`（问答/权限卡配对）、`Commands`/`CommandSpec`（泛型命令 dispatcher）、`EmitTerminalControl`（终态重试）、`TaskRunner`（`/pull` `/push` `/build` 单飞，支持任意命令）、`SafeJoin`/`BuildSendOptions`（`/send`）、`SummarizeToolInput`、`AskAndWait`/`EmitNotice` 等。各后端**按需调用**这些工具、自管 router/rpc/cancel 状态，而非嵌入共享结构。新增 agent 后端成本可控（miniagent 是范例：自持 `Handler`，仅在需要处调用 bridgebase helper）。

### 9.4 零外部依赖

`go.mod` 仅 module 行 + `go 1.25.0`，无 `go.sum`。飞书 WS/REST/protobuf/鉴权全部自实现；miniagent 走外部二进制子进程 + NDJSON，不引入 SDK。收益：消除 SDK 已知问题（goroutine 泄漏）、完全可控、交叉编译无依赖链。

### 9.5 健壮性细节

- **鉴权**：`subtle.ConstantTimeCompare` 防 timing attack（`ipcserver.go:179`）+ 每 IP 失败 10 次锁 1 分钟（`authFailuresCap=256` :107）。
- **并发安全**：router/usage 双锁、backendrpc 原子重连、子进程组 SIGKILL（`cmdutil/spawn_group.go` 的 `ApplyGroupCancel`）。
- **资源边界**：`wasOffline` 上限 64 触发全量重置（`maxWasOffline` ipcserver.go:425）、authFailures 上限 256、miniclient 信号量 `defaultMaxConcurrent=4`（client.go:35）、stdout 单行 8MiB 上限（`maxLineLen` client.go:24）、卡片元素 50 上限、SSE 帧 1MiB 上限。
- **去重三件套**：eventIDs（5min TTL+1000 LRU）/actionIDs（5min TTL）/terminals（10min TTL），防重放与终态重复。
- **优雅停机**：所有 Close 幂等（`sync.Once`）。
- **recover 防 panic 蔓延**：control pump goroutine（`GoSafe`）与各 turn goroutine。
- **原子写**：`atomicwrite`（tmp+fsync+rename+dirfsync），崩溃不留截断文件。

### 9.6 可观测性

统一 slog，支持级别 / 输出（stderr/stdout）/ 格式（text/json）/ 分组件级别（`component_log_levels`）。`log_debug_redact` 控制 prompt/error 文本是否脱敏进日志；`stream_archive_redact` 控制流归档字段级脱敏。`/v1/status` 暴露在途 turn 明细，deploy.sh 据此避免切断活跃对话。miniclient 把 stderr 折进错误事件，启动失败（缺 key / bad model / panic 栈）能到达用户。

### 9.7 测试文化

测试占比 ~48%（23003/47944 行），`go test -race ./...` 全绿。模式：表驱动 + interface fake 注入（`feishuClient` interface、`Commander` interface、`backendrpc.ControlSender` alias、`feishufront.NewLayer1Router` 等）。`cmd/*/main_test.go` 覆盖各入口错误路径。

---

## 附录：快速参考

- **module**：`github.com/justphantom/lark-bridge`（go.mod:1）
- **Go 版本**：1.25.0（go.mod:3）
- **二进制数**：3（cmd/ 下 3 子目录）
- **internal 包数**：22（顶层）/ 25（含嵌套）
- **Go 文件**：222 个，约 47,944 行（生产代码约 24,941 行，测试代码约 23,003 行，约 48.0%）
- **外部依赖**：0（无 go.sum）
- **版本**：v1.14.0
- **License**：MIT（LICENSE）
- **构建**：`make build` / `make test`
- **部署**：`make build && make deploy`（deploy 只装机，不再代为编译）
- **入口**：`cmd/<binary>/main.go:main()`
- **配置**：`config.example.json` + `${VAR}` 环境变量