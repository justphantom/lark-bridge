# lark-bridge

把飞书群聊桥接到本地编程 agent（Claude Code / opencode / miniagent）。采用 **1 前端 + N 后端** 的拆分架构，前端通过 SSE/POST 与后端通信，飞书群里一个会话绑定一个后端。

## 架构

```
飞书用户 ←→ 飞书开放平台 ←→ feishu-front (WS Bot + IPC SSE)
                                    ↕ SSE/POST (Bearer 鉴权)
         ┌───────────┬───────────┬──────────────┬─────────────────────┐    ┌──────────────┐
    claude-back  opencode-back  opencode-serve-back  miniagent-back      deploy-monitor
    (Claude CLI) (opencode CLI)  (opencode serve)    (LLM API 直调)      (make deploy)
```

- `feishu-front`：持有飞书 WebSocket 机器人，IPC 服务（SSE + Control POST），chatID→后端路由，分发器（消息→Prompt 事件，Control→卡片）。
- `claude-back` / `opencode-back`：每个 prompt fork 一次对应 CLI 子进程。
- `opencode-serve-back`：连接常驻 `opencode serve` HTTP server（用户自管进程），每 turn POST `/session/{id}/message?async=true` + 全局 `/event` SSE 订阅。适合长期高并发场景，避免每 turn 6-11s 的 CLI 启动开销。
- `miniagent-back`：每个 prompt fork 一次 miniagent 二进制（自带 ReAct 循环与 LLM 调用）。
- `deploy-monitor`：收到 `/deploy`、`/pull`、`/push` 在项目根执行 `make`，单飞（single-flight），结果回执。**独立部署**，避免「部署脚本管自己的触发者」循环依赖。

## 协议（internal/protocol）

- **Event**（前端→后端，SSE）：`Prompt` / `Answer` / `Abort` / `Ping`。
- **Control**（后端→前端，POST）：`Text` / `Result` / `ToolUse` / `Question` / `Notice` …。
- 纯结构定义 + Validate，无业务逻辑。

## 斜杠命令

- 前端：`/backend list|use {id}`（绑定后端）、`/skill <指令>`（透传，绕过后端本地命令分发）。
- claude-back：`/running` `/session-list` `/session-new` `/session-abort` `/session-del` `/current` `/model` `/cd` `/settings` `/perm` `/effort` `/help`。
- opencode-back：`/running` `/session-new` `/session-abort` `/session-del` `/current` `/model` `/agent` `/cd` `/help`。
- opencode-serve-back：`/running` `/session-new` `/session-abort` `/session-del` `/session-clean` `/session-list` `/session-use` `/current` `/model` `/agent` `/cd` `/help`（连 opencode serve HTTP server，而非 fork CLI）。
- miniagent-back：`/current` `/model` `/models` `/cd` `/running` `/session-abort` `/help`。
- deploy-monitor：`/deploy` `/deploy-force` `/pull` `/push` `/running`。

## 构建

```bash
make build      # 产物在 bin/：6 个二进制，git 版本号注入
make test       # build-check + vet + go test -race ./...
make vet        # go vet ./...
make fmt        # gofmt -s -w .
make clean
```

Go 1.25+。直接依赖仅 `github.com/larksuite/oapi-sdk-go/v3`（飞书开放平台 SDK）；`gorilla/websocket`、`gogo/protobuf` 为 SDK 的间接依赖。

## 配置

JSON 文件，支持 `${VAR}` 引用环境变量（空值/未设置报错退出）。可单文件共享或分文件（`deploy/` 下有 `feishu-config.json` / `claude-config.json` / `opencode-config.json` 示例）。机密用环境变量，不写进 JSON。

完整字段与默认值见 `config.example.json` 与 `internal/config/config_defaults.go`。

## 部署

```bash
make deploy                              # 构建 + 安装 4 个业务 systemd 服务（不含 opencode-serve）
make deploy ARGS=--init                  # 首次：从示例生成 config.json + .env
make deploy ARGS=--services opencode-serve  # 单独部署 opencode-serve-back（前置：用户已启动 `opencode serve`）
make upgrade-monitor                     # 单独升级 deploy-monitor（~2s 离线）
make upgrade-monitor ARGS=--init
```

opencode-serve-back 默认不入全量部署（要求外部 `opencode serve --port 4096 --hostname 127.0.0.1` 已就绪）。运维准备好 serve 进程后，用 `--services opencode-serve` 显式启用。

systemd unit 示例、健康检查、验证步骤详见 [`deploy/README.md`](deploy/README.md)。

## 目录约定

- `cmd/`：6 个二进制的入口（feishu-front、claude-back、opencode-back、opencode-serve-back、miniagent-back、deploy-monitor）。
- `internal/`：`protocol` `router` `config` `log` `feishu` `feishufront` `claude` `claudebridge` `opencode` `opencodebridge` `opencodeserve` `opencodeservebridge` `miniagent` `miniclient` `deploymonitor` `backendrpc` `bridgebase` `streamarchive` `usage` `cmdutil` `atomicwrite` `strutil` 等。
- `bin/`：编译产物（gitignore）。
- `deploy/`：部署脚本与配置模板。
