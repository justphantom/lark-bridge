# lark-bridge 项目代码规范文档

> 基于仓库 `github.com/justphantom/lark-bridge` 实际代码（约 20,238 行非测试 Go 代码 + 93 个测试文件，不含 `internal/claude/` 内联 SDK）归纳而成。每条规范尽量给出真实文件:行号或代码片段作为依据。
>
> 调查日期：2026-07-27 | 当前 HEAD：`53ea21d`

---

## 0. 概述

| 项目 | 取值 | 依据 |
|---|---|---|
| 语言版本 | **Go 1.25.0** | `go.mod:3` |
| Module path | `github.com/justphantom/lark-bridge` | `go.mod:1` |
| 直接依赖 | **仅 Go 标准库**（`go.mod` 无 `require`，`go.sum` 0 行） | `go.mod`、`go.sum` |
| Lint 工具 | **golangci-lint v2**（`version: "2"`），采用社区 "golden config"（maratori/golangci-lint-config） | `.golangci.yml:1-10` |
| Formatter | **goimports**（含 `local-prefixes` 分组） | `.golangci.yml:16-24` |
| 构建工具 | Make（默认目标 `build`），ldflags 注入 git 版本号 | `Makefile` |
| 部署形态 | 7 个二进制 + systemd 单元（`deploy/`） | `cmd/`、`Makefile:43-50` |
| 平台约束 | `//go:build linux || darwin`（agent 子进程包） | `internal/claude/*`、`internal/opencode/*` |

设计哲学（从配置文件中明示）：
- **显式允许名单制**：`.golangci.yml` 用 `default: none` + 显式 `enable:`，**不**用 `default: all`，并把每个**未启用**的 linter 都附上理由注释（`.golangci.yml:76-148`）——「这份文件就是项目的风格契约」。
- **零第三方依赖是硬约束**：飞书 WebSocket（RFC 6455）、REST、protobuf 帧编解码全部 `internal/` 自实现（v1.3.0 起，`internal/lark/`）。
- **注释写"为什么"，不写"是什么"**：`.golangci.yml:144-145` 明确禁用 `godoclint/godot/godox`，理由即「AGENTS.md: comments only for non-obvious why」。

---

## 1. 开发环境与工具链

### 1.1 必需工具

| 工具 | 用途 | 来源 |
|---|---|---|
| Go 1.25+ | 编译/测试 | `go.mod:3`、`README.md:44` |
| `make` | 入口 | `Makefile` |
| `golangci-lint` v2 | lint（注：Makefile 未集成目标，需手动运行） | `.golangci.yml:10` |
| `gofmt -s` | 格式化 | `Makefile:73-74` |
| `git` | 版本号注入（`git describe`） | `Makefile:31` |

### 1.2 常用命令

```bash
make            # = make build（默认目标）
make test       # build-check → vet → go test -race ./...
make vet        # go vet ./...
make fmt        # gofmt -s -w .
make build-check # go build ./...（提前发现 internal 包编译错误）
make clean
golangci-lint run   # 风格检查（手动）
```

依据：`Makefile:23-82`、`README.md:36-42`。

---

## 2. 项目布局规范

遵循 **标准 Go 布局**（`README.md:71-76`）：

```
lark-bridge/
├── cmd/                         # 7 个二进制入口，每个一个子目录
│   ├── feishu-front/            #   main.go (+ main_test.go)
│   ├── claude-back/
│   ├── opencode-back/
│   ├── miniagent-back/
│   ├── deploy-monitor/
│   └── status-monitor/
├── internal/                    # ~24 个包，禁止跨项目引用
│   ├── atomicwrite/             # 单一职责小包
│   ├── backendrpc/              # 后端→前端 IPC 客户端
│   ├── bridgebase/              # 三桥（claude/opencode/miniagent）共享脊柱
│   ├── claude/                  # 内联的 claude-go-sdk（带 doc.go）
│   ├── claudebridge/  opencodebridge/  miniagent/
│   ├── cmdutil/                 # 斜杠命令基础设施
│   ├── config/                  # JSON 配置 load+validate+defaults
│   ├── deploymonitor/
│   ├── feishu/                  # 业务封装层
│   ├── feishufront/             # 含子包 cardkit/、renderer/
│   ├── lark/                    # 自实现飞书客户端（含 doc.go）
│   │   ├── websocket/           #   RFC 6455 传输
│   │   └── ws/                  #   lark WS 子协议
│   ├── log/  protocol/  router/  streamarchive/  strutil/  usage/
│   └── ...
├── deploy/                      # systemd 部署脚本与配置模板
├── scripts/                     # 辅助脚本（.gitignore 内）
├── bin/                         # 编译产物（.gitignore 内）
├── config.example.json
├── go.mod / go.sum              # 后者 0 字节
├── Makefile
├── .golangci.yml
├── CHANGELOG.md  README.md  LICENSE  NOTICES.txt
└── .gitignore
```

规范要点：
- **`cmd/<bin>/main.go`** 只做装配（flag 解析 → 构造依赖 → 调 `run()`），业务逻辑全在 `internal/`。例：`cmd/feishu-front/main.go:57-74` 仅 18 行的 `main()`。
- **包粒度**：单一职责，小而专。`atomicwrite`（63 行）、`strutil`（多文件 ≤60 行）、`log`（176 行）都是典型。
- **`doc.go`** 用于「需要解释分层/协议方向的较大公共包」：`internal/lark/doc.go`（17 行，含分层图）、`internal/claude/doc.go`（19 行，含用法示例）。小工具包（atomicwrite/strutil）只在入口 `*.go` 顶部写包注释即可。
- **子包按抽象层切分**：`internal/lark/{websocket,ws}` 明确「RFC 6455 传输 / lark WS 子协议」分层（`internal/lark/doc.go:8-13`）。
- **文件按前缀分组**：`commands_*.go`、`dispatcher_*.go`、`bot_*.go`、`config_*.go`、`event_parse*.go`，避免单文件膨胀（`internal/feishufront/`、`internal/bridgebase/` 是范例）。

---

## 3. 命名规范

### 3.1 包命名
全小写、单词型，**无下划线/驼峰**：
- 单词：`log`、`config`、`router`、`protocol`、`usage`、`strutil`
- 复合（仍全小写连写）：`atomicwrite`、`bridgebase`、`cmdutil`、`backendrpc`、`streamarchive`、`deploymonitor`、`miniclient`、`feishufront`、`claudebridge`

依据：`internal/` 下全部 24 个目录。

### 3.2 文件命名
**snake_case**，常带语义前缀做分组：
- 单词：`core.go`、`answer.go`、`conn.go`、`dial.go`、`auth.go`、`debouncer.go`
- 下划线分组：`commands_session_mgmt.go`、`dispatcher_backend.go`、`dispatcher_control.go`、`dispatcher_interactive.go`、`bot_dispatch.go`、`bot_send.go`、`config_defaults.go`、`config_validate.go`、`event_parse_content.go`、`client_session.go`、`wsclient_test.go`

依据：`internal/feishufront/`、`internal/opencodebridge/`、`internal/claude/` 全部文件名。

### 3.3 标识符
- **导出**：PascalCase。例：`Config`、`Core`、`NewCore`、`Emit`、`EmitLogged`、`EmitAsync`、`FieldError`、`TypePrompt`。
- **未导出**：camelCase。例：`newLogger`、`handlerOpts`、`ensureDir`、`tailRunes`、`applyDefaults`、`envVarPattern`、`replyToIDKey`。
- 缩写词全大写保持：`APIError`、`IPCAddr`、`IPCSecret`、`URL`、`HTTP`、`ID`、`TTL`（见 `internal/config/config.go:30-62`、`internal/lark/auth.go:145`）。

### 3.4 接口命名
**`-er` 后缀**（行为型接口），小写未导出居多：
```go
// internal/lark/rest.go:16
type httpDoer interface { Do(*http.Request) (*http.Response, error) }
// internal/lark/ws/session.go:187
type frameWriter interface { ... }
// internal/bridgebase/git_runner.go:28
type GitCommander interface { Run(ctx, dir, name string, args ...string) ([]byte, error) }
// internal/deploymonitor/handler.go:33,40,48
type controlSender interface { ... }
type statusQuerier  interface { ... }
type Commander      interface { ... }
// internal/lark/types.go:64、internal/feishufront/dispatcher.go:55
type Handler     interface { ... }
type ChatRouter  interface { ... }
```
共 20 个 interface，绝大多数 `-er` 结尾。

### 3.5 常量命名
PascalCase（导出）/ camelCase（未导出），**分组放进 `const ( ... )` 块**：
```go
// internal/protocol/protocol.go:30-35   —— 字符串枚举
const (
    TypePrompt = "prompt"
    TypeAnswer = "answer"
    TypeAbort  = "abort"
    TypePing   = "ping"
)

// internal/log/logger.go:42-68   —— 日志字段名统一收口
const (
    FieldChatID    = "chat_id"
    FieldMessageID = "message_id"
    ...
    FieldError     = "error"
)

// internal/bridgebase/core.go:16-26   —— 私有调优常量带 why 注释
const (
    shutdownGrace   = 5 * time.Second
    emitConcurrency = 32
)
```
- 字符串值用 **snake_case**（JSON/日志键友好）：`"prompt"`、`"chat_id"`、`"operation"`。
- 数字魔法值原则：`.golangci.yml` **禁用 `mnd`**，理由「most are self-documenting inline; magic numbers worth a name are already extracted」（`.golangci.yml:98-101`）。

### 3.6 测试命名
- 文件：`xxx_test.go`，**与被测代码同包**（见 §8）。
- 函数：`TestXxx` 或 `TestXxx_Yyy`（描述子场景）。
  ```go
  // internal/bridgebase/throttle_test.go
  func TestShouldEmitText_FirstCallTrue(t *testing.T) { ... }
  func TestShouldEmitText_ThrottlesWithinInterval(t *testing.T) { ... }
  ```
- 辅助：小写未导出 `newTestXxx` / 泛型 helper。
  ```go
  // internal/protocol/protocol_test.go:10
  func roundTrip[T any](t *testing.T, v T) T { t.Helper(); ... }
  // internal/bridgebase/running_test.go:32
  func newTestCore(t *testing.T) *Core { ... }
  ```

---

## 4. 错误处理规范

### 4.1 三种定义方式（按使用频率）

**① `fmt.Errorf`（最常用，58 处）—— 带上下文 + `%w` 包装**
```go
// internal/config/config_validate.go:23
return fmt.Errorf("log_level must be one of debug/info/warn/error, got %q", cfg.LogLevel)
// internal/config/config_validate.go:81
return fmt.Errorf("state_dir: failed to resolve absolute path: %w", err)
// internal/lark/auth.go:115
return nil, fmt.Errorf("lark: token request: %w", err)
```
统一加**包前缀**（`log_level:`、`state_dir:`、`lark:`、`protocol:`），便于在日志里定位来源。

**② `errors.New`（31 处）—— 包级哨兵错误**
```go
// internal/lark/websocket/conn.go:46
var ErrCloseSent = errors.New("websocket: close sent")
// internal/backendrpc/reconnect.go:47
var ErrGiveUpReconnect = errors.New("backendrpc: give up reconnecting after sustained failures")
// internal/feishu/bot_send.go:52
var ErrCardContentRejected = errors.New("feishu: card content rejected by server")
```
哨兵命名遵循 `errname` linter：`Err…` 前缀、Error 类型 `…Error` 后缀。

**③ 自定义 error 类型**（带结构化字段）
```go
// internal/lark/auth.go:142-152
type APIError struct {
    Code int
    Msg  string
}
func (e *APIError) Error() string { return fmt.Sprintf("code:%d msg:%s", e.Code, e.Msg) }
```

### 4.2 错误包装（`%w`）—— 强制
- `errorlint` linter **强制**使用 `%w`（而非 `%v`/`%s`）包装、`errors.Is/As` 比较（`.golangci.yml:48`）。
- `%w` 实际出现 **43 处**，遍布 config/lark/feishu/cmd 层。

### 4.3 错误判别 —— `errors.Is/As`
```go
// cmd/feishu-front/main.go:242   —— 标准 ctx 取消判定
if err != nil && !errors.Is(err, context.Canceled) { firstErr = err }
// internal/feishufront/dispatcher_control.go:217   —— 哨兵分支
} else if errors.Is(err, feishu.ErrCardContentRejected) && ctrl.Result.Text != "" {
// internal/claudebridge/handler_prompt.go:178   —— 超时归因
if errors.Is(context.Cause(ctx), context.DeadlineExceeded) { ... }
// internal/lark/ws/reconnect.go:57   —— 类型断言
return errors.As(err, &f)
```
统计：`errors.Is` ×11、`errors.As` ×3。

### 4.4 错误返回与记录
- **返回**：错误沿调用链冒泡，`run()` 在 `main()` 里转成退出码：
  ```go
  // cmd/feishu-front/main.go:70-73
  if err := run(*cfgPath, *addr); err != nil {
      fmt.Fprintf(os.Stderr, "lark-feishu-front: %v\n", err)
      os.Exit(1)
  }
  ```
- **记录**：用 `Logger.Error/Warn` + `log.FieldError` 字段，**不直接 `log.Fatal`**。
  ```go
  // internal/bridgebase/core.go:181-186
  if err := c.Emit(ctx, promptID, ctrl); err != nil {
      c.Logger.Warn("emit failed",
          log.FieldChatID, chatID,
          log.FieldError, err)
  }
  ```
- **fire-and-forget 失败**：用 `*Logged` 后缀的封装（`EmitLogged` / `EmitNoticeLogged` / `EmitCardUpdateLogged`），即"Emit + 失败 Warn"，避免静默吞错（`internal/bridgebase/core.go:180-207`）。
- **零值/空 rpc 安全**：`Emit` 遇 `c.RPC == nil` 直接 `return nil`，让测试不接 IPC 也能跑（`core.go:171-173`）。

### 4.5 panic 使用 —— 仅限测试
- 生产代码 **0 处 `panic`**。
- 测试中 **4 处**（全部是故意触发以验证 `recover`）：
  ```
  internal/feishufront/ipcserver_test.go:408   panic("boom")
  internal/feishufront/dispatcher_test.go:718  panic("callback boom")
  internal/opencodebridge/handler_prompt_test.go:91  panic("simulated agent panic")
  internal/claudebridge/handler_prompt_test.go:103   panic("simulated agent panic")
  ```
- **goroutine 必须配 `recover`**：`bridgebase.GoSafe`（`gosafe.go:24-35`）统一封装——任何长生命周期 goroutine 用它启动，panic 自动 `recover` + 记 `log.FieldPanic/FieldStack`。main 控制泵同样手工 `defer recover`（`cmd/feishu-front/main.go:179-187`）。

---

## 5. 日志规范

### 5.1 日志库
**标准库 `log/slog`**，经 `internal/log` 薄封装。**完全不使用旧 `log` 包**（`log.Print` 全仓 0 处）。

封装方式是**类型别名**，让调用方只 import `internal/log`，未来可整体替换底层而不动调用点（`internal/log/logger.go:23-29`）：
```go
type Logger = slog.Logger
type Level  = slog.Level
const (
    LevelDebug = slog.LevelDebug
    LevelInfo  = slog.LevelInfo
    ...
)
```

### 5.2 级别与格式
- **四级**：`debug / info / warn / error`（`FromString` 严格枚举校验，`logger.go:128-143`）。级-义指南写在包注释里（`logger.go:6-11`）：
  - Debug：开发调试，不应出现在生产
  - Info：正常业务关键节点（绑定创建、请求完成）
  - Warn：需关注但不影响服务（可重试错误）
  - Error：影响功能但服务可继续
- **两种格式**：`text`（默认，`slog.NewTextHandler`）/ `json`（`NewJSONHandler`），由 config `log_format` 决定。
- **输出**：`stdout` / `stderr`，由 `log_output` 决定（默认 stderr）。
- **统一时间戳**：`2006-01-02 15:04:05.000`（`logger.go:118-122` `ReplaceAttr`）。
- **组件标签**：`logger.With("component", "feishu-front")`（`logger.go:106-112`），每个二进制 main 注入自己的组件名（`cmd/feishu-front/main.go:267`）。

### 5.3 结构化字段（统一字段名常量）
所有字段名收口在 `internal/log/logger.go:42-68` 的 `FieldXxx` 常量，**禁止散落字符串字面量**：
```go
// 推荐
c.Logger.Warn("emit failed", log.FieldChatID, chatID, log.FieldError, err)
// 反例（code-review-2026-07.md:80 指出）
logger.Error("...", "error", err)   // 应用 log.FieldError
```
常用字段：`FieldChatID`、`FieldMessageID`、`FieldSessionID`、`FieldDuration`（值为 `duration_ms` 毫秒数）、`FieldError`、`FieldOperation`、`FieldPath`、`FieldReason`、`FieldPanic`、`FieldStack`、`FieldGoroutine`、`FieldControlType`、`FieldModel` 等。

### 5.4 便捷函数
- `log.NewFromConfig(level, output, format, component)`：4 个 main 共用（`logger.go:90-103`），消除重复样板。
- `log.Nop()`：测试专用空 logger；构造函数对 `nil` logger 自动替换为 Nop（`bridgebase.NewCore:136-138`、`git_runner.go:62-64`）。
- `log.LogOperation(logger, name, fn)`：包裹一段操作，自动计时 + 成功/失败两态分别记一条（`logger.go:159-176`）——失败时**只记 Error 一条**，不先记 Info 再记 Error 制造噪音（注释 `logger.go:164-166` 明示）。

### 5.5 脱敏
`strutil.DebugRedact(s, redact)`：开启时整体替换为 `"<redacted N bytes>"`，保留长度信息便于排障（`strutil/redact.go`）。受 config `log_debug_redact` 控制。

---

## 6. 注释与文档规范

### 6.1 包注释（godoc）
**每个包都有包注释**，写在入口文件（`doc.go` 或主 `*.go`）的 `package X` 之前，常包含：目的 / 分层 / 协议方向 / 数据流 / 用法示例。

```go
// internal/protocol/protocol.go:1-13
// Package protocol defines the metadata contract between the frontend and
// the backends in the 1-frontend/N-backend split.
//
// Direction convention:
//   - SSE carries Event (frontend→backend): user-side input and actions
//     (Prompt / Answer / Abort / Ping).
//   - POST /v1/control/{backendID} carries Control (backend→frontend): ...
//
// This package is pure struct definitions + Validate helpers. No business
// logic. All errors are standard library fmt.Errorf.
package protocol
```

```go
// internal/claude/doc.go:3-18   —— 带可运行示例
// Package claude wraps the Claude Code CLI as a standalone SDK.
// ...
// Minimal example:
//
//	c := claude.New(claude.Options{})
//	ch, err := c.Run(ctx, claude.RunOptions{Prompt: "hello"})
```

### 6.2 导出标识符注释
**每个导出标识符都有 godoc 注释**（以标识符名字开头），强调「为什么」而非「是什么」：
```go
// internal/bridgebase/core.go:62-65
// Core is the backend-agnostic spine every bridge's Handler embeds: the
// router, the IPC client, per-chat cancel tracking, the answer broker, the
// emit helpers, and shutdown. Bridge code keeps only its agent client and
// option lists on top.
type Core struct { ... }

// internal/protocol/protocol.go:38-47   —— 字段级注释解释不变量/安全约束
// PromptPayload carries a user prompt. Text has already been @-stripped.
//
// Frontend (feishu-front) constructs this only from a chat message ...
// The override fields below — Directory / ModelSpec / ... — are reserved
// for trusted sources only (config, slash-command handlers, ...). They
// MUST NOT be set by the frontend pipeline: ...
```
注意 MUST/MUST NOT 等大写关键词用于标注硬约束（RFC 风格）。

### 6.3 语言约定
- **代码注释 / godoc / commit message / 变量名 / 错误消息**：**英文**。
- **用户可见文案**（飞书卡片正文、notice、slash 命令回复）：**中文**。例：`"本群已有一次 "+label+" 操作正在执行，请等待其完成后再试。"`（`git_runner.go:84`）；`.golangci.yml:95-97` 明确禁用 `gosmopolitan`，理由即这些中文字符串是「intentional user-facing copy」。
- **config 结构分组注释**：中文分隔线 `// —— 飞书凭证：feishu-front 用；后端忽略 ——`（`config.go:31,37,44,50,60`）。
- **少量函数级中文注释**：在 `internal/log/logger.go:114`（`handlerOpts` 共享选项）、`Makefile:34,51`（中文 build 注释）等位置出现。

### 6.4 代码块/分段注释
- 分节横幅：`// === Event (frontend → backend, over SSE) ===`（`protocol.go:15`、`feishu-front/main.go:99` 等大量使用）。
- 内联 `//` 行注释为主，**没有块注释 `/* */`**（gofmt 风格）。
- 包注释里的列表/缩进代码块遵循 godoc 格式（空行 + 两空格缩进）。

### 6.5 TODO/FIXME
基本**不使用**。全仓 `TODO/FIXME/XXX` 实际为 **0** 处（grep 命中的 4 处均为误报：`fireXXX` 命名、`context.TODO()` 调用）。`.golangci.yml:143` 明确禁用 `godox`。

---

## 7. 代码组织规范

### 7.1 函数长度
- **无硬性上限**：`funlen/cyclop/gocyclo/gocognit/nestif/maintidx` 全部禁用（`.golangci.yml:124-128`），理由「最大的函数是事件分发 switch（claudebridge.streamRun, opencodebridge.streamRun），拆分反而降低可读性」。
- **软目标**：单文件 ≤300 行（项目惯例，源自历史 AGENTS.md/CLAUDE.md）。当前 14 个文件超 300 行，其中最大的 `internal/lark/ws/frame.go` 422 行、`internal/opencodebridge/commands_session_mgmt.go` 400 行被 review 标为「建议触及时拆分」。

### 7.2 函数顺序
**按语义关注点分组**（生命周期 / emit / state），**不**按可见性。`.golangci.yml:135-139` 禁用 `funcorder`，理由「按可见性重排会把相关方法打散」。

### 7.3 接收者
- **命名**：类型首字母（短）。`recvcheck` linter 强制同一类型所有方法接收者名一致（`.golangci.yml:70`）。
  ```go
  c *Core           // internal/bridgebase/core.go
  t *tokenManager   // internal/lark/auth.go
  e *APIError       // internal/lark/auth.go:150
  n *nopHandler     // internal/log/logger.go:152
  r *GitRunner      // internal/bridgebase/git_runner.go:79
  d *Duration       // internal/config/config.go:196
  ```
- **值 vs 指针**：默认指针；小值/只读访问器用值（`func (d Duration) MarshalJSON`）。`recvcheck` 强制同一类型不混用。

### 7.4 import 分组（goimports + local-prefixes）
**三段式**，空行分隔，goimports 自动维护（`.golangci.yml:18-24`，`local-prefixes: github.com/justphantom/lark-bridge`）：
```go
// internal/bridgebase/core.go:3-14
import (
    "context"
    "sync"
    "sync/atomic"
    "time"

    "github.com/justphantom/lark-bridge/internal/backendrpc"
    "github.com/justphantom/lark-bridge/internal/log"
    "github.com/justphantom/lark-bridge/internal/protocol"
    "github.com/justphantom/lark-bridge/internal/router"
    "github.com/justphantom/lark-bridge/internal/usage"
)
```
当前因零第三方依赖，只有「标准库 / 本模块」两组；将来加第三方时插在中间组。

### 7.5 其他组织习惯
- **构造函数**统一 `NewXxx`（全仓 18 处），如 `NewCore`、`NewCommands`、`NewGitRunner`、`NewFromConfig`、`NewIPCServer`。
- **构造函数对 nil 参数兜底**：`NewCore(nil logger) → log.Nop()`（`core.go:136-138`），降低测试负担。
- **零值优先**：`.golangci.yml:146-148` 禁用 `exhaustruct`，理由「project uses zero-value defaults intentionally」。`applyDefaults` 只填空字段（`config_defaults.go`）。
- **不预建（no speculative code）**：review 把 `var _ = strings.TrimSpace`（假性引用）、未被引用的 `HeadersView()`/`WithLogLevel` 都标为违反「不预建」原则，要求删除（`code-review-2026-07.md:60-69`）。

### 7.6 并发原语
- **goroutine 必须有退出路径**：`context.Context` 驱动 + `select { case <-ctx.Done(): return }`（`feishu-front/main.go:173-194`）。
- **panic-safe goroutine** 一律走 `GoSafe`（§4.5）。
- **per-chat 单飞**用 `sync.Mutex.TryLock`（`git_runner.go:81`）；进程级单飞用 `sync.Once`（`core.go:250`）。
- **embedded mutex 禁用**：`embeddedstructfieldcheck: forbid-mutex: true`（`.golangci.yml:162-165`），避免外层类型暴露 `Lock/Unlock`。

---

## 8. 测试规范

### 8.1 位置与包名
- 测试文件与被测代码**同目录同包**（**不**用 `xxx_test` 后缀包）。
  - 93 个 `*_test.go` 文件中 **93/93** 与生产代码同 `package`。
- 理由（`.golangci.yml:86-90`）：`testpackage` linter **被显式禁用**——需要直接测未导出 helper（`parseEvent`、`newTestCore`、`emitCLIEvent`、`captureSender` …），用 `_test` 包会丢失这部分覆盖。

### 8.2 测试函数命名
- `TestXxx` 或 `TestXxx_Yyy`（下划线分场景），例：
  ```
  TestShouldEmitText_FirstCallTrue
  TestShouldEmitText_ThrottlesWithinInterval
  TestEventRoundTrip / TestPromptEventRoundTrip
  TestControlRoundTrip / TestEventValidate
  ```

### 8.3 Table-driven（普遍采用）
```go
// internal/protocol/protocol_test.go:67-92
func TestControlRoundTrip(t *testing.T) {
    cases := []struct {
        name string
        ctrl *Control
    }{
        {"session_init", &Control{Type: TypeSessionInit, ...}},
        {"text",         &Control{Type: TypeText,     ...}},
        ...
    }
    for _, tc := range cases {
        t.Run(tc.name, func(t *testing.T) {
            got := roundTrip(t, tc.ctrl)
            if got.Type != tc.ctrl.Type {
                t.Fatalf("type lost: got %q want %q", got.Type, tc.ctrl.Type)
            }
        })
    }
}
```

### 8.4 测试辅助函数
- 小写未导出，常用泛型：
  ```go
  // internal/protocol/protocol_test.go:10
  func roundTrip[T any](t *testing.T, v T) T {
      t.Helper()
      data, err := json.Marshal(v)
      ...
  }
  ```
- 每包的 `newTestXxx`：`newTestCore`、`newTestHandler`、`newTestBot`、`newTestRouter`、`newTestRestClient`。
- **断言**：原生 `t.Errorf/t.Fatalf/t.Fatal`，**不使用 testify**（`testifylint` 在 `.golangci.yml:119-122` 标为「library-specific; project does not use testify」）。

### 8.5 并行与子测试
- **不并行**：全仓 `t.Parallel()` 出现 **0 次**。理由（`.golangci.yml:91-94`）：很多测试通过 `newTestCore` / `router.v5.json` / `t.TempDir` 共享文件系统状态，强制并行需引入额外隔离复杂度且无正确性收益。`paralleltest/tparallel` 因此禁用。
- 子测试用 `t.Run(tc.name, ...)`。

### 8.6 资源与隔离
- `usetesting` linter 强制用 `testing.TB.TempDir` / `testing.TB.Context`（`.golangci.yml:74`）。
- `go test -race ./...` 是 CI/Makefile `test` 目标的默认跑法（`Makefile:78-79`）。
- `_test.go` 文件对 `noctx/gosec/errcheck/gocheckcompilerdirectives` 放宽（`.golangci.yml:184-189`）：测试桩用 `os.WriteFile`、假 SSE server、硬编码 `sk-test` 等是预期行为。

### 8.7 测试专用导出 API 白名单
个别导出符号生产代码不调用、仅测试使用（`deadcode` 会报）。**决策：保留导出现状**（已逐一审阅，移入 `export_test.go` 收口对当前调用点无收益且增加跨包测试阻力），以下为显式白名单，新增前须先评估能否改用未导出 + 同包测试：

| 符号 | 位置 | 调用分布 | 保留理由 |
|------|------|---------|---------|
| `backendrpc.ConnectWithHTTPClient` | `backendrpc/client.go` | 跨包（feishufront 测试） | 跨包测试注入自定义 `http.Client` 起假 SSE server |
| `bridgebase.AnswerBroker.PendingIDs` | `bridgebase/answer.go` | 跨包（claudebridge / opencodebridge 测试） | 跨包测试取待选槽 requestID（picker 刚注册的槽） |
| `lark/websocket.ComputeAccept` | `lark/websocket/dial.go` | 跨包（lark/ws 测试） | 假 test server 构造 `Sec-WebSocket-Accept` 握手回执 |
| `claude.ParseEvent` | `claude/event_parse.go` | 跨包（claudebridge 测试）+ 同包 | 跨包回放抓包的真实 stream-json 行 |
| `opencode.ParseEvent` | `opencode/event_parse.go` | 跨包（opencodebridge 测试）+ 同包 | 跨包测试构造 `Event`（同构理由） |
| `feishufront.TurnManager.TurnsByBackend` | `feishufront/turn.go` | 仅同包测试 | 零生产调用（原服务 `OnBackendOffline` 清理，该路径已废弃）；保留作外部诊断接口 |
| `usage.Store.Snapshot` | `usage/usage.go` | 仅同包测试 | 同包测试断言用量快照；保留供未来跨包诊断/CLI 复用 |

> 触发于 2026-07-29 仓库审计 §1.2 的「保留 vs 收口」决策项；本节即该决策的持久化记录。后两项（仅同包使用）可在后续维护中改为未导出收口，不阻塞发版。

---

## 9. Makefile 与开发流程

### 9.1 目标清单（`Makefile`）

| 目标 | 作用 |
|---|---|
| `build`（默认） | 编译 7 个二进制到 `bin/`，注入 `main.version`（`git describe --tags --always --dirty`），`-s -w` strip |
| `build-check` | `go build ./...`，提前发现 internal 包编译错误（不只编 7 个 cmd） |
| `vet` | `go vet ./...` |
| `fmt` | `gofmt -s -w .`（simplify） |
| `test` | `build-check` → `vet` → `go test -race ./...`（CGO 默认开，启用 race） |
| `clean` | `rm -rf bin/` |
| `pack` | 交叉编译 7 个二进制 + `VERSION` + 配置示例 → `bin/lark-bridge-<ver>-<goos>-<goarch>.tar.gz`（命令行 `GOOS=/GOARCH=` 覆盖） |
| `deploy` | 调 `./deploy/deploy.sh $(ARGS)` 构建 + 安装 4 个业务 systemd 服务 |
| `upgrade-monitor` | 单独构建并重启 `lark-deploy-monitor`（独立于 deploy.sh，避免循环依赖） |

### 9.2 开发流程（推荐顺序）
```
make fmt  →  golangci-lint run  →  make build-check  →  make test  →  make build
```
- 改动后先 `make fmt`（gofmt -s）。
- `make test` 已含 `build-check + vet + race test`，作为 PR 前的完整门禁。
- golangci-lint **不在 Makefile 里**，开发者需手动 `golangci-lint run`（配置 `.golangci.yml` 会被自动读取）。

### 9.3 部署流程
```
make deploy                  # 构建 + 4 业务服务 systemd 装机
make deploy ARGS=--init      # 首次：从示例生成 config.json + .env
make deploy ARGS=--services opencode   # 子集部署
make upgrade-monitor         # ~2s 离线升级 deploy-monitor
```
环境变量：`IPC_ADDR`、`STATE_DIR` 可命令行覆盖（`Makefile:19-21`）。

---

## 10. Git 提交规范

### 10.1 分支策略
**Trunk-based / 单主干**：仓库仅 `main` 分支（本地与 `origin/main` 同步），`git branch -a` 无其他长期分支，`git log` 也未见 merge commit。所有改动直接提交到 `main`。

### 10.2 Commit message 风格
**轻量 Conventional Commits**，全部**英文、祈使句、单行、首字母大写**。统计最近 50 条：
- 严格 Conventional（`type(scope): desc`）约占 6 条：`fix(feishufront): ...`、`fix(ws): ...`。
- **`scope:` 前缀无 type**（最常见）：`feishufront: ...`、`lark: ...`、`deploy: ...`、`renderer: ...`、`vendor: ...`、`opencode: ...`、`claude-back: ...`、`deploy.sh: ...`。
- **自由句首大写**（描述性）：`Refine /backend picker: in-place outcome flip + 10min TTL`、`Slim progress card: drop step banner dup, fix dead SessionInit path`、`Remove opencode-serve-back; port /session-use to opencode-back (CLI)`。
- **阶段化代号**：`P0 fixes: ...`、`P1 hardening: ...`、`P2 cleanup: ...`、`P3 engineering: ...`（一组连续提交的工程化整改）。

**典型示例**（`git log --oneline -30`）：
```
53ea21d fix(feishufront): simplify picker click fix to delayed PATCH + drop dead ACK code
831ac20 fix(ws): put card action ACK business fields at top level
8b7b605 deploy: migrate removed config fields before restarting monitor
4b39726 feishufront: log card-action ingress + empty-kind value to diagnose picker no-update
396a278 P1 hardening: resource bounds, auth, env, reconnect budget
345c2a4 Release v1.2.0
2c9ae0a vendor: inline claude-go-sdk into internal/claude
4ad0950 deploy.sh: translate all logs and comments to English ASCII
```

要点：
- **scope 用包名 / 组件名**（`feishufront`、`ws`、`lark`、`claude-back`、`renderer`）。
- 描述里允许冒号接副标题（`fix X, drop Y`）。
- 几乎**不带 body**，信息密度全在标题。
- **不发"Update X"、"修复"这类空泛/中文 message**。

### 10.3 版本与发版
- **SemVer**：`v1.1.0`、`v1.2.0`（`CHANGELOG.md:21,84`）。
- 发版提交：`Release v1.2.0` / `Release v1.1.0: interactive card lifecycle and git/deploy commands`（一行式）。
- `make pack` 产物文件名嵌入 `git describe` 短哈希（`Makefile:65`）。

### 10.4 CHANGELOG 规范
遵循 [Keep a Changelog 1.1.0](https://keepachangelog.com/zh-CN/1.1.0/)（中文版）+ SemVer（`CHANGELOG.md:3-4`）：
- `## [Unreleased]` 段 + `## [x.y.z] - YYYY-MM-DD` 段。
- 四类小节：`### Added` / `### Changed` / `### Fixed` / `### Removed`。
- 每条以加粗短语开头 + 段落解释，常带 `file:line` 或配置字段名引用。
- 允许「补记」（`> 本段同时补记 v1.1.0 期间合入但当时未在 CHANGELOG 注明的两项`）。

### 10.5 .gitignore 约定（`.gitignore`）
不入库的：`/bin/`、`/scripts/`、`/docs/`、`.env`、`.zcode/`、`*.log`、二进制副本、AI 工具缓存（`.agents/`、`.claude/skills/`、`.goose/` 等）。

---

## 附：关键规范速查表

| 维度 | 规范 |
|---|---|
| Go 版本 | 1.25.0，零第三方依赖 |
| Lint | golangci-lint v2，`default: none` + 显式 allow-list，~30 个 linter |
| Format | goimports（local-prefixes 分组） |
| 包名 | 全小写单词（`atomicwrite`、`bridgebase`） |
| 文件名 | snake_case，常带前缀分组（`commands_*.go`） |
| 接口 | `-er` 后缀（`GitCommander`、`httpDoer`） |
| 常量 | 分组 `const ( ... )`，值用 snake_case 字符串 |
| 错误 | `fmt.Errorf("pkg: ctx: %w", err)` 为主；哨兵 `ErrXxx = errors.New(...)`；自定义类型 `…Error` |
| 错判 | `errors.Is/As`（errorlint 强制），禁止 `==` 比 error |
| 日志 | `log/slog` + `internal/log` 别名封装；字段名用 `log.FieldXxx` 常量；四级 debug/info/warn/error |
| panic | 生产代码禁用；goroutine 必走 `GoSafe` 的 recover |
| 注释 | 英文 godoc；强调 why；导出标识符必注释；中文仅用于用户可见文案 |
| 文件长度 | 软目标 ≤300 行（无硬性 linter） |
| 接收者 | 类型首字母；同类型一致（recvcheck 强制） |
| 测试 | 同包 `*_test.go`；table-driven；不用 testify；不并行；`-race` |
| 提交 | 英文祈使句；`scope:` 前缀；单行；trunk-based 单 `main` 分支 |
| 版本 | SemVer + Keep a Changelog（中文） |
