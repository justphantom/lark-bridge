# lark-bridge

把飞书群聊桥接到本地编程 agent（Claude Code / miniagent）。采用 **1 前端 + N 后端** 的拆分架构，前端通过 SSE/POST 与后端通信，飞书群里一个会话绑定一个后端。

## 架构

```
飞书用户 ←→ 飞书开放平台 ←→ feishu-front (WS Bot + IPC SSE)
                                    ↕ SSE/POST (Bearer 鉴权)
        ┌──────────┬──────────┬────────────────┬──────────────┐
   claude-back miniagent-back  deploy-monitor  status-monitor
   (Claude CLI)(LLM API 直调)  (make deploy)   (状态总览)
```

- `feishu-front`：持有飞书 WebSocket 机器人，IPC 服务（SSE + Control POST），chatID→后端路由，分发器（消息→Prompt 事件，Control→卡片）。
- `claude-back`：每个 prompt fork 一次 Claude Code CLI 子进程。
- `miniagent-back`：每个 prompt fork 一次 miniagent 二进制（自带 ReAct 循环与 LLM 调用）。
- `deploy-monitor`：收到 `/deploy`、`/pull`、`/push` 在项目根执行 `make`，单飞（single-flight），结果回执。**独立部署**，避免「部署脚本管自己的触发者」循环依赖。
- `status-monitor`：每 N 秒（`status_monitor.interval`，默认 60s）向绑定的每个群推送一张总览卡（在线后端 + 运行中会话数与时长），有则 PATCH、被删则重发。**独立部署**，push-only。

## 协议（internal/protocol）

- **Event**（前端→后端，SSE）：`Prompt` / `Answer` / `Abort` / `Ping`。
- **Control**（后端→前端，POST）：`Text` / `Result` / `ToolUse` / `Question` / `Notice` …。
- 纯结构定义 + Validate，无业务逻辑。

## 斜杠命令

- 前端：`/backend`（弹出在线后端选择卡片，绑定后端）、`/skill <指令>`（透传，绕过后端本地命令分发）。
- claude-back：`/running` `/session` `/new` `/abort` `/current` `/model` `/cd` `/config` `/perm` `/effort` `/send` `/pull` `/push` `/help`。
- miniagent-back：`/current` `/model` `/cd` `/new` `/send` `/pull` `/push` `/running` `/abort` `/help`。
- deploy-monitor：`/deploy` `/deploy-force` `/deploy-some` `/pull` `/push` `/running`。
- status-monitor：无斜杠命令（被动推送；绑定后每 `status_monitor.interval` 自动刷新总览卡）。

## 构建

```bash
make build      # 产物在 bin/：5 个二进制，git 版本号注入
make test       # build-check + vet + go test -race ./...
make vet        # go vet ./...
make fmt        # gofmt -s -w .
make clean
```

Go 1.25+。**直接依赖仅 Go 标准库**——飞书 WebSocket 长连接、REST 收发、卡片回调、protobuf 帧编解码均由 `internal/lark/` 自实现（RFC 6455 WebSocket 客户端 + 手写 protobuf），无任何第三方模块。

## 配置

JSON 文件，支持 `${VAR}` 引用环境变量（空值/未设置报错退出）。多服务可共享单文件（`config.example.json` 是唯一真源，各进程只读自己需要的字段），也可复制成每服务一份按需裁剪。机密用环境变量，不写进 JSON。

完整字段与默认值见 `config.example.json` 与 `internal/config/config_defaults.go`。

### miniagent 后端配置要点

- **bare 模式**：配 `miniagent.chat_url` + `miniagent.models_url`（完整 URL），`api_key` 通过环境变量 `${MINIAGENT_API_KEY}` 注入。
- **多 provider 模式**：配 `miniagent.config_path` 指向部署期生成的 `miniagent.json`，并可选 `provider` 切换 provider；此模式下 `chat_url`/`models_url` 由 config 文件提供，bridge 只透传 `-config <abspath>`。
- **每 chat 覆盖**：`/mode`、`/thinking`、`/model` 会持久化到 router binding；`key_file` 让 bridge 从文件读取密钥并注入子进程 env，替代直接在 JSON 里写 `api_key`。

### 敏感数据落盘

- **streamarchive**：`stream_history > 0` 时，每个 backend 把该轮 CLI stdout 原文落盘到 `{state_dir}/streams/{backend}/`（仅剔除 claude 的 thinking_tokens 行）。**含用户 prompt、agent 读到的文件内容、模型回复**。文件权限 0600，但若 `state_dir` 进了备份/日志转发/共享快照，这些内容随之离开主机。生产环境若不需要排障，设 `stream_history: 0` 关闭，或把 `streams/` 排除出备份。`stream_archive_redact` 默认 `true`，会对 archive 中的敏感字段做行级脱敏；`log_debug_redact` 只影响日志，不影响 archive。
- **debug 日志**：`log_debug_redact` 默认 `false`（零值）时 prompt/result/error 文本原样进 debug 日志。生产建议显式设 `true`。`stream_archive_redact` 与 `log_debug_redact` 独立，示例配置已把前者设为 `true`。

## 部署

```bash
make deploy                              # 构建 + 安装 3 个业务 systemd 服务
make deploy ARGS=--init                  # 首次：从示例生成 config.json + .env
make deploy ARGS=--services claude       # 单独部署某服务子集（逗号分隔）
make upgrade-monitor                     # 单独升级 deploy-monitor（~2s 离线）
make upgrade-monitor ARGS=--init
make upgrade-status                      # 单独升级 status-monitor（~2s 离线）
make upgrade-status ARGS=--init
```

> **升级注意**：`opencode-back` 与 `omp-back` 已移除（后端对接收敛到 claude + miniagent）。`make deploy` 会自动检测并清理遗留的 `lark-opencode-back` / `lark-omp-back`（及更早的 `lark-opencode-serve-back`）systemd 单元、router/usage state 文件与 config 模板；已部署 config 里残留的 `opencode` / `omp` 块也会被 upgrade-monitor / upgrade-status 迁移剥离。

systemd unit 示例、健康检查、验证步骤详见 [`deploy/README.md`](deploy/README.md)。

### 运维注意事项

- **日志轮转**：`internal/log` 只输出到 stdout/stderr，**无内建轮转**。生产部署必须依赖 journald 或容器运行时做日志轮转；切勿把 stdout 用 shell 重定向到文件长期运行——磁盘增长无任何保护。
- **环境密钥命名**：传给 CLI 子进程的环境采用 **deny-list 清洗**（变量名含 `SECRET`/`TOKEN`/`ENCRYPT`/`PASS`/`PRIVATE_KEY`/`CREDENTIAL` 片段会被剥离）。请勿用不含这些片段的名字存放敏感环境变量——如 `LARK_VERIFICATION_KEY`、`APP_KEY` 这类命名会**原样**传给 CLI 子进程及其 spawn 的工具孙子进程。`MINIAGENT_API_KEY` 按设计放行，会被 CLI 及其工具继承。
- **IPC 非 loopback 必须 TLS**：`ipc_addr` 绑定非 loopback 地址时，必须配置 `ipc_tls_cert_file`/`ipc_tls_key_file`（否则 feishu-front 拒绝启动——明文 HTTP 上的 bearer 可被同网段嗅探冒用）；可选 `ipc_tls_client_ca_file` 启用 mTLS（要求并校验后端客户端证书）。启用 TLS 后，各后端的 `frontend_url` 需使用 `https://`；mTLS 时后端还需配置客户端证书。

## 目录约定

- `cmd/`：5 个二进制的入口（feishu-front、claude-back、miniagent-back、deploy-monitor、status-monitor）。
- `internal/`：`protocol` `router` `config` `log` `feishu` `feishufront` `claude` `claudebridge` `miniagent` `miniclient` `deploymonitor` `backendrpc` `bridgebase` `streamarchive` `usage` `cmdutil` `atomicwrite` `strutil` 等。
- `bin/`：编译产物（gitignore）。
- `deploy/`：部署脚本与配置模板。
