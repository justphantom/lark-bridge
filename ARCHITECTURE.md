# lark-bridge 项目架构说明文档

> 本文档基于仓库 `github.com/justphantom/lark-bridge` 实际代码归纳。
> 调查日期：2026-07-27 | 当前版本：v1.2.0（`[Unreleased]` 段在做飞书客户端自实现）

---

## 1. 项目概述

### 1.1 定位与目标

**lark-bridge** 是一个把 **飞书（Lark/Feishu）群聊桥接到本地编程 agent** 的中间层服务。用户在飞书群里 @机器人，即可驱动本地的 Claude Code / opencode / miniagent 等 CLI agent 完成编码任务，并把 agent 的流式进度、工具调用、最终回复渲染成飞书交互卡片。

- 仓库：`github.com/justphantom/lark-bridge`
- 当前版本：**v1.2.0**（git tag），`[Unreleased]` 段正在做飞书客户端自实现
- 核心问题：飞书开放平台 ↔ 本地 CLI agent 之间没有原生通道；直接把 agent 暴露给飞书缺少鉴权、并发、流式渲染、会话路由等能力
- 解决思路：采用 **1 前端 + N 后端** 的拆分架构（`README.md:3`、`CHANGELOG.md:131`）

### 1.2 核心能力

| 能力 | 实现 |
|---|---|
| 飞书长连接收事件 / REST 发卡片 | 自实现 WebSocket + REST 客户端（`internal/lark/`） |
| chat→后端路由（Layer-1） | `internal/router/` + `internal/feishufront/routing.go` |
| 双向 IPC 协议（SSE + POST） | `internal/protocol/` + `internal/backendrpc/` + `internal/feishufront/ipcserver*.go` |
| agent CLI 子进程驱动 | `internal/claude/`、`internal/opencode/`、`internal/miniclient/` |
| 流式进度卡 / 权限卡 / 问答卡 / 结果卡 | `internal/feishufront/renderer/` + `cardkit/` |
| 斜杠命令体系 | 各 `*bridge/commands*.go` + `internal/cmdutil/` |
| 单飞部署触发 | `internal/deploymonitor/` |
| systemd 部署 + 二进制 tarball | `deploy/deploy.sh`、`Makefile` |

### 1.3 规模

- **241 个 Go 文件，42,286 行**，其中测试 **20,947 行（约 49.5%）**
- `internal/` 下 **21 个子包**
- `go.mod` **零外部依赖**（`go.sum` 为空），仅用 Go 标准库

---

## 2. 技术栈

| 维度 | 选型 | 证据 |
|---|---|---|
| 语言 | Go 1.25.0 | `go.mod:3` |
| 外部依赖 | **无**（仅标准库） | `go.mod` 无 `require`；`go.sum` 0 字节 |
| 飞书对接 | **自实现** RFC 6455 WebSocket 客户端 + 手写 protobuf 帧编解码 + REST | `internal/lark/`，背景见 `docs/lark-client-rewrite.md` |
| 日志 | 标准库 `log/slog` 封装 | `internal/log/logger.go:29`（`type Logger = slog.Logger`） |
| 配置 | JSON + `${VAR}` 环境变量展开 | `internal/config/config.go:263` |
| HTTP/IPC | 标准库 `net/http`（SSE 长连接 + POST） | `internal/feishufront/ipcserver.go:277` |
| 构建工具 | GNU Make（git 版本号注入 `-X main.version`） | `Makefile:31-49` |
| 部署 | systemd unit + shell 脚本 | `deploy/deploy.sh`、`deploy/upgrade-monitor.sh` |
| 静态检查 | golangci-lint（`.golangci.yml` 9594 字节） | 根目录 |
| 测试 | `go test -race ./...`，表驱动 + fake 注入 | `Makefile:78` |
| CI 工具脚本 | `scripts/openapi_to_md.py`（拉取飞书 OpenAPI 生成参考文档） | `scripts/` |

**历史依赖变迁**（`CHANGELOG.md:9-14, 44-47`）：原本依赖 `larksuite/oapi-sdk-go/v3` + `gorilla/websocket` + `gogo/protobuf` + `claude-go-sdk`，经两个版本演进：
- v1.2.0：内联 `claude-go-sdk` → `internal/claude/`
- Unreleased：自实现飞书客户端 → `internal/lark/`，最终实现"零外部依赖"

---

## 3. 目录结构

```
lark-bridge/
├── cmd/                      # 6 个二进制入口
│   ├── feishu-front/         # 前端：飞书 WS Bot + IPC server + 调度器
│   ├── claude-back/          # Claude CLI 后端
│   ├── opencode-back/        # opencode CLI 后端
│   ├── miniagent-back/       # miniagent (LLM 直调) 后端
│   ├── deploy-monitor/       # /deploy /pull /push 触发器（独立部署）
│   └── status-monitor/       # 周期性总览卡推送（独立部署，push-only）
├── internal/                 # 21 个内部包（详见第 5 节）
│   ├── protocol/             # Event/Control 协议（纯结构 + Validate）
│   ├── router/               # chatID ↔ 后端绑定持久化
│   ├── config/               # JSON 配置加载/默认值/校验
│   ├── log/                  # slog 封装 + 统一字段名
│   ├── lark/                 # ★ 自实现飞书客户端（WS + REST + protobuf）
│   │   ├── websocket/        #   RFC 6455 WS 客户端
│   │   └── ws/               #   帧编解码 + 重连 + 分片重组
│   ├── feishu/               # lark.Client 的业务封装层（Bot/IncomingMessage）
│   ├── feishufront/          # ★ 前端核心（9824 行，最大包）
│   │   ├── cardkit/          #   飞书卡片元素 schema
│   │   └── renderer/         #   progress/result/interactive 渲染器
│   ├── backendrpc/           # 后端↔前端 IPC 客户端（SSE + 重连）
│   ├── bridgebase/           # ★ 后端通用脊梁（router+rpc+cancel+emit+git）
│   ├── claude/               # claude CLI 子进程驱动（stream-json 解析）
│   ├── claudebridge/         # claude-back 业务逻辑
│   ├── opencode/             # opencode CLI 子进程驱动（NDJSON 解析）
│   ├── opencodebridge/       # opencode-back 业务逻辑
│   ├── miniagent/            # miniagent-back 业务逻辑
│   ├── miniclient/           # miniagent CLI 子进程封装
│   ├── deploymonitor/        # /deploy 单飞执行器
│   ├── usage/                # 每 session token/cost 累计
│   ├── streamarchive/        # 每轮 CLI stdout 原文落盘（带保留期）
│   ├── cmdutil/              # 斜杠命令公共框架
│   ├── atomicwrite/          # tmp+fsync+rename 原子写
│   └── strutil/              # 截断/脱敏/环境变量工具
├── deploy/                   # 部署脚本 + 配置模板 + systemd 示例
├── docs/                     # 6 篇设计/审查文档（.gitignore，本地）
├── scripts/                  # openapi_to_md.py（外部工具，.gitignore）
├── bin/                      # 编译产物（gitignore）
├── config.example.json       # 配置模板
├── Makefile                  # build/test/vet/fmt/deploy/pack
├── go.mod / go.sum           # 仅 module 行 + go 1.25.0
├── CHANGELOG.md              # 1.0.0 → 1.2.0 → Unreleased
├── README.md                 # 项目速览
├── NOTICES.txt               # 上游依赖归属
└── .golangci.yml             # lint 规则
```

**顶层目录职责表**

| 目录 | 职责 | 备注 |
|---|---|---|
| `cmd/` | 6 个二进制的 `main.go`（每个含 `main_test.go` 覆盖错误路径） | 入口极薄，组装 internal |
| `internal/` | 全部业务代码，21 个包 | 不对外暴露 |
| `deploy/` | `deploy.sh`（业务 4 服务）、`upgrade-monitor.sh`（独立）、`*.json` 配置模板、`env.example`、`README.md` | 部署真源 |
| `docs/` | 设计文档与代码审查记录（本地不入仓） | 决策溯源 |
| `scripts/` | 单个 Python 脚本（拉取飞书 OpenAPI） | 工具，非运行时 |
| `bin/` | `make build` 产物（6 个二进制） | gitignore |

---

## 4. `cmd/` 入口点

6 个二进制共享一致的骨架：`flag.Parse → config.Load → buildLogger → 校验 IPC 三件套 → 组装依赖 → signal.NotifyContext → 阻塞运行`。入口都极薄（~130-270 行），仅做依赖注入。

| 二进制 | 入口文件 | 产物名 | 职责 | 关键依赖装配 |
|---|---|---|---|---|
| **feishu-front** | `cmd/feishu-front/main.go:57` | `lark-feishu-front` | 持有飞书 WS Bot，提供 IPC server，分发消息与卡片回调 | `feishu.NewBotWithLogger` (:100) + `feishufront.NewLayer1Router` (:114) + `NewBackendRegistry` (:120) + `NewIPCServer` (:121) + `NewTurnManager` (:125) + `NewDispatcher` (:126) |
| **claude-back** | `cmd/claude-back/main.go:31` | `lark-claude-back` | 每 prompt fork 一次 `claude` CLI | `claude.New` (:83) + `router.New` (:68) + `usage.New` (:77) + `backendrpc.Connect` (:93) + `claudebridge.NewWithLogger` (:100) + `backendrpc.Run` (:134) |
| **opencode-back** | `cmd/opencode-back/main.go:31` | `lark-opencode-back` | 每 prompt fork 一次 `opencode run` | `opencode.New` (:66) + 其余同 claude-back（:97/:105/:127） |
| **miniagent-back** | `cmd/miniagent-back/main.go:37` | `lark-miniagent-back` | 每 prompt fork 一次 `miniagent` 二进制（独立项目） | `miniclient.New` (:105) + `miniagent.New` (:113) + `backendrpc.Run` (:129) |
| **deploy-monitor** | `cmd/deploy-monitor/main.go:33` | `lark-deploy-monitor` | 收 `/deploy` `/pull` `/push` 执行 `make`/git，单飞 | `deploymonitor.New` (:72) + `backendrpc.Run` (:96) + 优雅 drain (:110) |
| **status-monitor** | `cmd/status-monitor/main.go:29` | `lark-status-monitor` | 按 `status_monitor.interval` 轮询 `GET /v1/status`，向绑定群推送常驻总览卡（PATCH/重发）；push-only | `statusmonitor.New` (:66) + `backendrpc.Run` (:82)（独立部署） |

> **注意**：`cmd/` 下共 6 个二进制。其中 `deploy-monitor` 与 `status-monitor` 因会触发部署 / 需独立刷新，分别由 `upgrade-monitor.sh` / `upgrade-status.sh` 管理，不纳入 `deploy.sh` 的 4 个业务服务（feishu / claude / opencode / miniagent）。

`version` 变量由 Makefile 的 `-ldflags "-X main.version=$(VERSION)"` 注入（`Makefile:32`），`git describe --tags --always --dirty`。

---

## 5. `internal/` 模块职责详述

按规模（行数）降序，共 21 个包。

### 5.1 `feishufront/`（9824 行，36 文件）——前端核心

前端的所有业务逻辑集中地，是体量最大的包。

| 文件 | 职责 |
|---|---|
| `dispatcher.go` | **`Dispatcher` 编排器**（:62）。接收飞书消息与后端 Control，路由到对应卡片。核心方法 `DispatchIncoming`（:245）、`updateCard`（:231） |
| `dispatcher_control.go` | `DispatchControl`（:20）：把后端 Control 分派到 `updateProgress` / `sendResult` / `sendNoticeControl` / `sendInteractive` |
| `dispatcher_interactive.go` | 问答/权限卡生命周期 |
| `dispatcher_backend.go` | `/backend` 选择卡处理 |
| `ipcserver.go` | **`IPCServer`**（:20）：HTTP 服务，含 Bearer 鉴权（`subtle.ConstantTimeCompare` :147）+ 每 IP 限流 |
| `ipcserver_sse.go` | `GET /v1/events` SSE handler（:16），注册后端、流式推送 Event |
| `ipcserver_control.go` | `POST /v1/control/{backendID}` handler |
| `ipcserver_health.go` | 后端健康检查（静默超时驱逐） |
| `registry.go` | **`BackendRegistry`**（:93）：backendID → `BackendConn` 映射 + Control 通道 |
| `turn.go` | **`TurnManager`**：每轮对话（prompt）的状态机，`/v1/status` 数据源 |
| `routing.go` | `Layer1Router`：chatID → backendID 路由（持久化 `routing.json`），`ChatRouter` 接口实现（:54-59 定义在 dispatcher.go） |
| `dedup.go` | 三套 TTL+LRU 去重集（eventIDs/actionIDs/terminals），防重放 |
| `debouncer.go` | 卡片 PATCH 合并器（500ms 窗口），防飞书 API 限流（`main.go:34-41` 注释） |
| `form.go` | 表单解析 |
| `cardkit/` | 飞书卡片 schema 的纯结构定义（Header/Footer/元素） |
| `renderer/` | 四类卡片渲染：`progress.go`（流式进度，含工具行/todo/banner）、`result.go`（终态结果）、`interactive.go`（问答/权限）、`progress_snapshot.go`/`progress_category.go`（内部状态） |

### 5.2 `lark/`（4385 行，19 文件）——自实现飞书客户端

Unreleased 版本的核心改动（`docs/lark-client-rewrite.md` 858 行方案文档）。

| 文件 | 职责 |
|---|---|
| `client.go` | **`Client`**（:54）：高层组合，`Start` 阻塞跑 WS（:104），`Send`/`PatchMessage` 走 REST |
| `auth.go` | `tokenManager`：tenant_access_token 获取 + 提前 5 分钟刷新（设计文档 §4.3） |
| `rest.go` | `restClient`：通用 HTTP + IM API（SendMessage/PatchMessage），区分业务错误码（230025 内容过大 → `ErrCardContentRejected`） |
| `types.go` | `Mention`/`SendInput`/`SendResult`/`CardActionEvent`/`MessageReceiveEvent` |
| `doc.go` | 包文档 |
| `websocket/dial.go` + `conn.go` | **RFC 6455 客户端最小实现**（client-side only，含 mask/ping/pong/分片重组） |
| `ws/frame.go` + `proto.go` | **手写 protobuf** Frame 编解码（9 字段，~150 行 marshal + 100 行 unmarshal，黄金对比测试 fixture） |
| `ws/wsclient.go` | WS 客户端主循环（bootstrap → dial → receiveLoop → pingLoop） |
| `ws/reconnect.go` | 指数退避重连（按服务端 ClientConfig.ReconnectInterval/Count） |
| `ws/dispatcher.go` | 分片重组（`reassembler`，按 message_id+seq）+ 事件路由 |
| `ws/session.go` | 会话状态 |

### 5.3 `claudebridge/`（4265 行，28 文件）与 `opencodebridge/`（4182 行，25 文件）——后端业务

两个 bridge 高度对称（共享 `bridgebase`）。

| 文件（claudebridge 为例） | 职责 |
|---|---|
| `handler.go` | **`Handler`**（:19）嵌入 `bridgebase.Core`，加 agent client + 选项列表 |
| `handler_event.go` | `HandleEvent`：分发 Event（Prompt/Answer/Abort/Ping） |
| `handler_prompt.go` | **`runPrompt`**（:30）：单 turn 核心——fork CLI、流式收事件、emit 终态 Control |
| `handler_incoming.go` | 入站预处理 |
| `stream_loop.go` | 消费 `claude.Run` 事件流 → 转换为 protocol.Control |
| `stream_archive.go` | 每 turn 落盘原始 stream-json（去 thinking_tokens） |
| `commands*.go`（6 个） | 斜杠命令：`/running` `/session-*` `/current` `/model` `/cd` `/perm` `/effort` `/pull` `/push` 等 |
| `todo.go` | todo 清单渲染 |
| `deps.go` | `claudeAPI` 接口（便于测试 fake） |
| `domain.go` | 业务领域类型 |
| `dir_cache.go` | `/cd` 目录扫描缓存 |

`opencodebridge` 多一个 `commands_abort.go`、`commands_session_mgmt.go`（`/session-use`），少 `effort/settings`（`CHANGELOG.md:30-31, 39-40`）。

### 5.4 `bridgebase/`（3156 行，23 文件）——后端通用脊梁

**核心抽象**，三个 agent 后端共享的 non-business 基础设施。

| 文件 | 职责 |
|---|---|
| `core.go` | **`Core`**（:66）嵌入结构：Router、RPC、Logger、AppCtx、CancelByChat、Answers、Usage、Git、emitSem。`NewCore`（:135）、`Emit`/`EmitAsync`/`EmitLogged`、`Close`（:249） |
| `prompt.go` + `prompt_slot.go` | 每 chat 并发槽（busy-then-drop） |
| `answer.go` | **`AnswerBroker`**：问答/权限卡的请求-应答配对（RequestID → chan） |
| `interactive.go` | `AskPermission`/`AskQuestion` 共享逻辑 |
| `commands.go` | 斜杠命令 dispatcher（绑定 `*Handler`） |
| `gitjob.go` + `git_job.go` + `gosafe.go` | `/pull` `/push` 每 chat 单飞，`GitRunner.AcquireAndRun` |
| `running.go` | `/running` 命令 |
| `toolinput.go` | 工具输入摘要（`SummarizeToolInput`） |
| `throttle.go` | 节流 |
| `dircache.go` | `/cd` 工作目录缓存 |

### 5.5 `opencode/`（2188 行）与 `claude/`（2170 行）——CLI 驱动层

封装对应 CLI 子进程的 fork + 事件流解析。
- `claude/`：`claude-go-sdk` 内联（`CHANGELOG.md:44-47`），`doc.go` 声明包为 "wraps the Claude Code CLI as a standalone SDK"。文件含 `client.go`（fork）、`stream.go`、`event*.go`（stream-json 解析）、`permission.go`、`settings.go`、`ready.go`（健康门）。
- `opencode/`：`opencode run` 子进程驱动，解析 NDJSON 事件流。

### 5.6 `miniagent/`（1701 行）与 `miniclient/`（677 行）

- `miniclient/`：fork `miniagent` 二进制（独立项目 `github.com/justphantom/miniagent`），`Run`（client.go:88）启动子进程，信号量限并发，ctx 取消走 SIGKILL 进程组。
- `miniagent/`：handler + 斜杠命令（`/current` `/model` `/models` `/cd` `/pull` `/push` `/running` `/session-abort`）+ 模型列表（GET /v1/models）。**stateless**（无 session/memory）。

### 5.7 `feishu/`（1490 行）——飞书业务封装层

`lark/`（协议层）与 `feishufront`（前端业务）之间的适配层。
- `bot.go`：**`Bot`**（:74）封装 `*lark.Client`，对外签名稳定（`NewBotWithLogger`/`Start`/`Stop`/`SendCard`/`UpdateCard`/`OnIncoming`/`OnCardAction`）。`ShouldExitUnhealthy`（:249）看门狗判定。`feishuClient` interface（:16）便于测试注入 fake。
- `bot_dispatch.go`：`handleMessageReceive`/`handleCardAction` 把 lark 事件转成 `IncomingMessage`/`CardAction`。
- `bot_send.go`：`SendCard`/`SendText`/`UpdateCard`，`isCardContentRejected` 错误识别。
- `mention.go`：`Mention` 类型 + `StripMentionPlaceholders`。

### 5.8 `backendrpc/`（1458 行）——后端 IPC 客户端

| 文件 | 职责 |
|---|---|
| `client.go` | **`Client`**（:83）：SSE 长连接读 Event + POST 写 Control。`Connect`（:125）、`readSSE`（:193）、`SendControl`（:270）、`Status`（:306）、`Close`（:339） |
| `reconnect.go` | **`Run`**（:58）：带指数退避 + 抖动的重连循环，`ErrGiveUpReconnect`（:47）连续失败 20 次放弃 |
| `config_validate.go` | `ValidateBackendConfig`：IPC 三件套校验 |

### 5.9 `config/`（1146 行）

| 文件 | 职责 |
|---|---|
| `config.go` | **`Config`** 顶层结构（:30）+ 各后端子结构（Claude/Opencode/DeployMonitor/MiniAgent）+ `Load`（:296，5 步 pipeline）+ `expandEnvVars`（:263） |
| `config_defaults.go` | `applyDefaults`（:11）：所有零值字段的默认值 |
| `config_validate.go` | `validate`：跨字段一致性校验 |

### 5.10 `deploymonitor/`（1021 行）

`/deploy` `/deploy-force` `/pull` `/push` 处理。
- `handler.go`：**`Handler`**（:63）单飞（`running bool`），`Commander` interface（:48）便于测试。
- `confirm.go`：`/deploy-force` 二次确认门（复用 `bridgebase.AnswerBroker`，`CHANGELOG.md:99-100`）。
- `render.go`：结果格式化（独立成文件以守住 300 行上限，`CHANGELOG.md:122`）。

### 5.11 `protocol/`（875 行）——双向协议

**纯结构 + Validate，无业务逻辑**（`README.md:24`）。
- `protocol.go`：**`Event`**（:18，前端→后端，SSE）+ 4 类 payload（Prompt/Answer/Abort/Ping）。`PromptPayload.HasFrontendOverride`（:68）是安全护栏。
- `protocol_control.go`：**`Control`**（:6，后端→前端，POST）+ 12 类 payload。
- `protocol_validate.go`：enum 校验（todo.status/priority、notice.level 等，`CHANGELOG.md:166`）。
- `status.go`：`StatusSnapshot`（/v1/status 响应）。

### 5.12 `router/`（750 行）——chatID 绑定持久化

- `router.go`：**`Router`**（:37）+ **`Binding`**（:25，SessionID/Directory/ModelSpec/Agent/PermissionMode/EffortLevel/SettingsFile 的并集）。单 worker save 合并器（`saveLoop`，:52-55 注释）。
- `persistence.go`：load/save（atomicwrite）。
- `accessors.go`/`binding.go`：Get/Set/Lookup。

### 5.13 其余工具包

| 包 | 行数 | 职责 |
|---|---|---|
| `cmdutil/` | 596 | 斜杠命令公共框架：`Spec`（:45）、`Result`（:34）、`Timeout=15s`（:22）、纯 helper（错误配对、设置变更日志、/help 渲染） |
| `usage/` | 548 | 每 session token/cost 累计，`Delta`（:42），atomicwrite 持久化，TTL 7d + 定期 prune（:31-38） |
| `log/` | 303 | slog 封装 + 统一字段名常量（`FieldChatID` 等，:42+）+ `LevelVar` 支持热更 |
| `streamarchive/` | 288 | 每 turn stdout 落盘到 `{state_dir}/streams/{backend}/`，文件名时间序，保留期裁剪。**含敏感数据**（`README.md:54` 警告） |
| `atomicwrite/` | 172 | tmp+fsync+rename+dirfsync（:31），崩溃安全。`open_flags_unix.go`/`_other.go` 平台分叉（O_NOFOLLOW） |
| `strutil/` | 149 | `Truncate`（rune 边界，:14）、`redact.go`（脱敏）、`env.go`（`EnvVarPattern`，config 与 strutil 共享避免漂移，config.go:26） |

---

## 6. 核心数据流 / 调用链

以"用户在飞书群 @机器人 发消息 → claude-back 处理 → 回复卡片"为例，标注关键文件:行号。

### 6.1 配置加载（所有二进制共享）

```
cmd/*/main.go: main()
   └─ config.Load(cfgPath)                              internal/config/config.go:296
        ├─ os.ReadFile                                  :297
        ├─ expandEnvVars(raw)                           :263  ← ${VAR} 展开，空值报错
        ├─ json.NewDecoder + DisallowUnknownFields     :310  ← 拼写错误硬拒绝
        ├─ applyDefaults(&cfg, path)                   config_defaults.go:11
        └─ validate(&cfg)                              config_validate.go
```

### 6.2 前端启动（`cmd/feishu-front/main.go:76 run()`）

```
config.Load ............................................. :77
buildLogger(cfg) ....................................... :88
feishu.NewBotWithLogger(appID, appSecret, logger, ...) .. :100   ← 内部 lark.NewClient
feishufront.NewLayer1Router(routingPath) ................ :114   ← routing.json 持久化
feishufront.NewBackendRegistry() ....................... :120
feishufront.NewIPCServer(registry, cfg.IPCSecret) ...... :121
feishufront.NewTurnManager() ........................... :125
feishufront.NewDispatcher(bot, registry, turns, router)  :126
  └ SetDedupConfig / SetCardPatchDelay .................. :130/:137
signal.NotifyContext(SIGINT, SIGTERM) .................. :139
dispatcher.InitDebouncer(ctx, 500ms) ................... :143
dispatcher.StartDedupPrune(ctx) ........................ :146
router.StartPrune(ctx, 14d) ............................ :150
ipc.SetOnOffline / SetOnOnline / SetInFlightTurns ...... :152-160
bot.OnIncoming(dispatcher.DispatchIncoming) ............ :162
bot.OnCardAction(dispatcher.DispatchCardAction) ........ :163
go ipc.StartHealthCheck(ctx, 30s, 90s) ................ :166
go { control pump: registry.Controls() → DispatchControl } :173-194  ← recover 防 panic
go ipc.Listen(listenAddr) .............................. :199   ← HTTP server
go bot.Start(ctx) ..................................... :208   ← 阻塞主 select
go { WS 看门狗: ShouldExitUnhealthy → os.Exit(1) } .... :219
select { ctx.Done | ipcErr | botErr } .................. :239   ← 优雅停机
```

### 6.3 入站消息流（飞书 → 后端）

```
飞书开放平台
   │ WebSocket 帧（protobuf）
   ▼
lark.ws.wsclient.receiveLoop ..................... internal/lark/ws/wsclient.go
   │ Frame.Unmarshal（手写 protobuf）.............. internal/lark/ws/frame.go
   │ reassembler 重组分片 ......................... internal/lark/ws/dispatcher.go
   ▼
lark.Client → handlerSinkAdapter.OnMessage ...... internal/lark/client.go:198
   ▼
feishu.Bot.handleMessageReceive .................. internal/feishu/bot_dispatch.go
   │ （构造 IncomingMessage）
   ▼
feishu.Bot.onIncoming.Load() → Dispatcher.DispatchIncoming  internal/feishufront/dispatcher.go:245
   ├─ isStale(msg.CreateTimeMs) 丢弃过期 ........... :249
   ├─ eventIDs.Add(msg.EventID) 去重 ............... :252
   ├─ MsgType != "text" 拒绝非文本 ................. :259
   ├─ StripMentionPlaceholders ..................... :263
   ├─ /backend 命令 → handleBackendCommand ......... :271
   ├─ /skill 透传标记 ............................. :278
   ├─ router.Resolve(chatID) → backendID ........... :289   ← Layer-1 路由
   ├─ registry.BackendType(backendID) 检查在线 ...... :293
   ├─ renderer.NewProgressState + Render ........... :306  ← 占位进度卡
   ├─ bot.SendCard(chatID, card, replyTo) .......... :315  ← 发卡片到飞书群
   ├─ turns.Start(promptID, chatID, msgID, backend)  :321
   └─ registry.SendEvent(backendID, Event{Prompt}) . :331  ← 推入 BackendConn.eventCh
        ▼
        IPCServer.handleSSE 从 BackendConn.eventCh 读 ... internal/feishufront/ipcserver_sse.go
        │ 按 `data: <json>\n\n` 帧写给 SSE 长连接
        ▼
        后端的 backendrpc.Client.readSSE 接收 ....... internal/backendrpc/client.go:193
        │ json.Unmarshal → Event
        ▼
        Client.eventCh → RecvEvent ................. :257
```

### 6.4 后端处理（以 claude-back 为例）

```
backendrpc.Run(ctx, ...) .......................... internal/backendrpc/reconnect.go:58
   │ Connect → RecvEvent 循环 + 指数退避重连
   ▼
handle(ctx, *Event) = h.HandleEvent ............... cmd/claude-back/main.go:135
   ▼
claudebridge.Handler.HandleEvent .................. internal/claudebridge/handler_event.go
   ├─ Prompt → handlePromptEvent
   │     ├─ HasFrontendOverride 拒绝前端覆写字段 .. protocol.go:68（安全护栏）
   │     ├─ ensureBinding(chatID) ................. router 查/建绑定
   │     ├─ busy-then-drop 并发检查 ............... bridgebase/prompt_slot.go
   │     └─ go runPrompt(ctx, chatID, binding, ...)  handler_prompt.go:30
   │          ├─ context.WithCancelCause + PromptTimeout  :74
   │          ├─ claude.RunOptions{Prompt, Directory, SessionID, Model, ...}  :84
   │          ├─ api.Run(ctx, opts) → event channel ...... fork claude CLI 子进程
   │          ├─ stream_archive.NewSink ............ 落盘 stream-json
   │          ├─ stream loop（stream_loop.go）：消费 claude.Event
   │          │    └─ 转 protocol.Control{ToolUse/Text/Thinking/Todo/Progress...}
   │          │       └─ Core.EmitAsync(promptID, ctrl) .. bridgebase/core.go:216
   │          │            └─ rpc.SendControl POST ...... backendrpc/client.go:270
   │          ├─ （中途权限/问答）AskPermission/AskQuestion → AnswerBroker
   │          └─ emitTerminal (Result/Error) ............. 同步 Emit（core.go:167）
   ├─ Answer → Answers.Resolve（权限/问答回调）
   └─ Abort → CancelByChat[chatID].Cancel → SIGKILL 进程组（cmdutil/spawn_group.go）
```

### 6.5 Control 流（后端 → 飞书卡片）

```
后端 rpc.SendControl POST /v1/control/{backendID}
   ▼
IPCServer.handleControl ........................... internal/feishufront/ipcserver_control.go
   ├─ authOK（Bearer + 限流）...................... ipcserver.go:136
   └─ registry.ReceiveControl(RoutedControl) ...... registry.go:198
        ▼
        registry.ctrlCh（buffered 1024）
        ▼
前端 control pump goroutine（main.go:178）读取
   ├─ recover() 防 panic 挂掉 pump
   ▼
Dispatcher.DispatchControl(ctx, rc) ............... dispatcher_control.go:20
   ├─ TypeSessionInit/ToolUse/ToolResult/Progress/Todo/Thinking
   │     → updateProgress → renderer 渲染 → updateCard（经 debouncer）:231
   ├─ TypeResult → sendResult ..................... :48
   ├─ TypeError/TypeNotice → sendNoticeControl .... :51
   │     （terminals 去重，PromptID 维度）.......... :45
   └─ TypeQuestion/TypePermission → sendInteractive  :53
        ▼
bot.UpdateCard / SendCard → lark REST PatchMessage / SendMessage
   ↓
飞书卡片更新（用户实时看到工具行翻动、最终结果卡）
```

### 6.6 关键并发与生命周期

- **TurnManager**（`feishufront/turn.go`）：每 promptID 一个 Turn 状态，`/v1/status` 数据源，`InFlight` 排除 deploy-monitor 自身的 /deploy（`main.go:156-160`）防自阻塞。
- **CancelByChat**（`bridgebase/core.go:103`）：chatID → cancelFunc，busy-then-drop。
- **Close 顺序**（`bridgebase/core.go:249`）：AppCancel → CancelAll → Answers.Drain → WaitPrompts（5s grace）→ Usage.Close，幂等 via `sync.Once`。
- **router.Close**（`router/router.go:102`）：关 saveLoop → 同步 save 一次（防丢失，:96-101 注释）。
- **deploy-monitor drain**（`cmd/deploy-monitor/main.go:110`）：SIGTERM 后等 make 跑完（10min+30s），避免半构建。

---

## 7. 配置体系

### 7.1 加载 pipeline（config.go:10 注释）

`readRaw → expandEnvVars → json.Unmarshal(DisallowUnknownFields) → applyDefaults → validate`

### 7.2 顶层结构（`config.example.json` + `config.go:30`）

| 字段 | 所属二进制 | 说明 |
|---|---|---|
| `feishu_app_id` / `feishu_app_secret` | feishu-front | 飞书自建应用凭证（`${VAR}` 引用） |
| `feishu_domain` | feishu-front | `feishu`/`larksuite`，默认 `feishu` |
| `feishu_log_level` | feishu-front | info/Warn/Debug/Error |
| `ipc_secret` | 三者共享 | Bearer 令牌，空值前端拒绝启动（`main.go:95`） |
| `ipc_addr` | feishu-front | 监听地址，默认 `localhost:6060` |
| `backend_id` | 后端 | 在前端 registry 的唯一 ID |
| `frontend_url` | 后端 | 前端 IPC 地址 |
| `router_path` | 共用 | router 持久化文件路径 |
| `claude{}` | claude-back | cli_path/permission_mode/default_directory/max_concurrent/stream_history/model_options/permission_options/effort_options/settings_dir/settings_cache_ttl |
| `opencode{}` | opencode-back | cli_path/default_directory/max_concurrent/stream_history/list_cache_ttl |
| `miniagent{}` | miniagent-back | api_key/base_url/model/system_prompt/max_tokens/workspace_root |
| `deploy_monitor{}` | deploy-monitor | project_root/deploy_target |
| `log_level`/`log_output`/`log_format`/`log_debug_redact` | 共用 | 日志 |
| `component_log_levels{}` | 共用 | 分组件级别（router/opencode/feishu/bridge/dedup/deploy_monitor） |
| `state_dir` | 共用 | 持久化根目录 |
| `timeouts{}` | 共用 | backend_health/prompt_timeout/usage_session_ttl/card_patch_delay |
| `dedup{}` | feishu-front | stale_window/event_ttl/event_max_entries |

### 7.3 配置特点

1. **`${VAR}` 环境变量展开**（`config.go:263`）：机密走环境变量不进 JSON；未设置或空值报错退出；值做 JSON escape 防 secret 破坏 JSON 结构。
2. **DisallowUnknownFields**（`config.go:311`）：拼错字段名硬拒绝，避免"已改未生效"陷阱。
3. **单文件共享 or 分文件**（`README.md:48`）：进程各取所需字段，跨二进制必填校验在各自 `main.go`。
4. **Duration 自定义类型**（`config.go:190`）：JSON 编码为 `"5m"` 而非纳秒，负值拒绝。

---

## 8. 部署与运行

### 8.1 运行形态：**1 个长驻前端 + N 个长驻后端 + 1 个独立部署监控**

不是 CLI 工具，而是 **5 个长驻 systemd 服务**（`deploy/README.md:151`）。

```
飞书用户 ←→ 飞书开放平台 ←→ feishu-front (WS Bot + IPC SSE)
                                    ↕ SSE/POST (Bearer 鉴权)
   ┌──────────┬──────────┬─────────────────────┬──────────────┐
claude-back opencode-back miniagent-back           deploy-monitor
(Claude CLI)(opencode CLI)(LLM API 直调)           (make deploy)
                                                     ↑ 独立部署
```

### 8.2 部署矩阵

| 命令 | 作用 | 范围 |
|---|---|---|
| `make build` | 编译 5 二进制到 `bin/`，注入 git 版本号 | 本机 |
| `make pack [GOOS= GOARCH=]` | 交叉编译 + 打 tarball（`bin/lark-bridge-<ver>-<os>-<arch>.tar.gz`） | 分发 |
| `make deploy` | 调 `deploy/deploy.sh`，构建 + 装 **4 个业务服务** | systemd |
| `make deploy ARGS=--init` | 首次：从示例生成 config.json + .env | systemd |
| `make deploy ARGS=--services opencode` | 只部署子集 | systemd |
| `make deploy ARGS=--binaries <tar>` | 从 tarball 部署（目标机免 Go） | systemd |
| `make upgrade-monitor [ARGS=--init]` | 单独升级 deploy-monitor（~2s 离线） | systemd |

### 8.3 循环依赖规避（`README.md:18`, `deploy/README.md:194-208`）

deploy-monitor 收 `/deploy` 触发 `make deploy`，**若 deploy.sh 能管 deploy-monitor 自己**，会形成"部署脚本管自己的触发者"。解法：deploy-monitor 由独立的 `upgrade-monitor.sh` 管理，deploy.sh 仅管 4 个业务服务。

### 8.4 分布式部署（`deploy/README.md:261-280`）

`ipc_addr` 监听非 loopback + `frontend_url` 指前端机即可前后端分机。IPC 为明文 HTTP，跨机限可信内网（跨不可信网络走 SSH 隧道/wireguard）。

### 8.5 健康检查

- 前端：`curl localhost:6060/v1/events` 应返回 401（鉴权拦截，`deploy/README.md:214`）
- 后端：启动时 `api.IsReady` fail-fast（如 claude CLI 未安装，`cmd/claude-back/main.go:118`）
- WS 看门狗：`ShouldExitUnhealthy`（`feishu/bot.go:249`），5 分钟无健康信号 → `os.Exit(1)` 交 systemd 拉起

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
业务层 (feishufront.dispatcher / *bridge)            卡片渲染 + CLI 驱动
   ↑↓
适配层 (claude / opencode / miniclient)              各 CLI 子进程封装
```

`internal/feishu/` 是 `lark/`（协议）与 `feishufront`（业务）之间的刻意适配层（`docs/lark-client-rewrite.md §3.4`），下游零改动。

### 9.2 协议优先（Protocol-First）

`internal/protocol/` 是前后端契约的单一真源：
- **Event**（前端→后端，SSE）：Prompt/Answer/Abort/Ping
- **Control**（后端→前端，POST）：12 种含 Text/Result/ToolUse/Question/Permission/Todo/Notice/Progress...
- **`HasFrontendOverride`**（`protocol.go:68`）：运行时护栏——前端管道不得设置 Directory/ModelSpec 等"信任源专属"字段，否则被后端拒绝（防任意 CWD 注入）。
- 协议纯结构，新接入 agent 后端只需实现 Control 发送。

### 9.3 后端无关脊梁（bridgebase）

三个 agent 后端的非业务逻辑抽到 `bridgebase.Core`（router/rpc/cancel/answers/emit/git/usage）。`claudebridge.Handler` 与 `opencodebridge.Handler` 嵌入 `*bridgebase.Core`（handler.go:20），各自只加 agent client + 命令。新增 agent 类型成本可控（miniagent 是范例，仅 ~1700 行）。

### 9.4 零外部依赖

`go.mod` 仅 module 行 + `go 1.25.0`，`go.sum` 0 字节。飞书 WS/REST/protobuf/鉴权全部自实现（~4400 行），背景与方案见 `docs/lark-client-rewrite.md`。收益：消除 SDK 已知问题（goroutine 泄漏）、完全可控、交叉编译无依赖链。

### 9.5 健壮性细节

- **鉴权**：`subtle.ConstantTimeCompare` 防 timing attack（`ipcserver.go:147`）+ 每 IP 失败 10 次锁 1 分钟。
- **并发安全**：router/usage 双锁、backendrpc 原子重连、子进程组 SIGKILL（`cmdutil/spawn_group.go`）。
- **资源边界**：`wasOffline` 上限 64 触发全量重置（`ipcserver.go:333`）、authFailures 上限 256、emitSem 32 并发上限（`core.go:25`）、卡片元素 50 上限、SSE 帧 1MiB 上限。
- **去重三件套**：eventIDs（5min TTL+1000 LRU）/actionIDs（5min TTL）/terminals（10min TTL），防重放与终态重复（`CHANGELOG.md:104-107`）。
- **优雅停机**：所有 Close 幂等（`sync.Once`），deploy-monitor 等 make 跑完。
- **recover 防 panic 蔓延**：control pump goroutine 与 3 个 SDK 入口（`CHANGELOG.md:163-165`）。
- **原子写**：`atomicwrite`（tmp+fsync+rename+dirfsync），崩溃不留截断文件。

### 9.6 可观测性

统一 slog，支持级别 / 输出（stderr/stdout）/ 格式（text/json）/ 分组件级别（`component_log_levels`）。`log_debug_redact` 控制 prompt/error 文本是否脱敏进日志。`/v1/status` 暴露在途 turn 明细，deploy.sh 据此避免切断活跃对话。

### 9.7 测试文化

测试占比 ~50%（20947/42286 行），`go test -race ./...` 全绿。模式：表驱动 + interface fake 注入（`feishuClient` interface、`Commander` interface、`claudeAPI` interface、`feishufront.NewLayer1Router` 等）。`cmd/*/main_test.go` 覆盖各入口错误路径。

### 9.8 文档化决策

`docs/` 6 篇文档记录关键设计与审查：
- `lark-client-rewrite.md`（858 行）：飞书客户端自实现方案，含 milestone 拆分、降级路径、风险表
- `release-1.0-audit.md`：发布前 P0/P1/P2 审查清单
- `code-review-2026-07.md`（357 行）、`event-parsing-review-2026-07.md`：事件解析审查
- `status-monitor-design.md`：/v1/status 设计
- `opencode-cli-reference.md`/`opencode-run-streaming.md`：opencode CLI 对接参考

---

## 附录：快速参考

- **module**：`github.com/justphantom/lark-bridge`（go.mod:1）
- **Go 版本**：1.25.0（go.mod:3）
- **二进制数**：5（cmd/ 下 5 子目录）
- **internal 包数**：21
- **代码行**：42,286（测试 20,947，~49.5%）
- **外部依赖**：0
- **版本**：v1.2.0（Unreleased 正在做飞书客户端自实现）
- **License**：MIT（LICENSE）
- **构建**：`make build` / `make test` / `make deploy`
- **入口**：`cmd/<binary>/main.go:main()`
- **配置**：`config.example.json` + `${VAR}` 环境变量
