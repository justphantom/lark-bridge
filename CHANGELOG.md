# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 风格，版本号
遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [1.0.0] - 2026-07-25

首次正式发布。

### 架构

1 前端 + N 后端的拆分架构：飞书 WebSocket 机器人 + IPC 服务（SSE + Control POST）
作为前端，按 chatID 绑定一个后端；后端按场景分四类。

- **feishu-front**：飞书 WS 长连接 + IPC 服务 + 路由 + 分发器
- **claude-back**：每个 prompt fork `claude` CLI
- **opencode-back**：每个 prompt fork `opencode` CLI
- **opencode-serve-back**：连常驻 `opencode serve` HTTP server，长连接复用，适合长期高并发
- **miniagent-back**：每个 prompt fork miniagent 二进制（自带 ReAct 循环 + LLM 直调）
- **deploy-monitor**：接收飞书群 `/deploy` `/pull` `/push`，独立部署（避免循环依赖）

### 关键能力

- **协议层**：纯结构 + Validate 的 Event/Control 双向协议
- **并发安全**：router/usage 双锁、backendrpc 原子重连、SDK 子进程组 SIGKILL（cmdutil.ApplyGroupCancel）
- **鉴权**：IPC 共享 Bearer + `subtle.ConstantTimeCompare` 防 timing attack
- **资源管理**：bridgebase.Close 顺序幂等、atomicwrite 原子落盘、streamarchive 路径净化
- **卡片渲染**：飞书卡片 progress / question / permission / result 四类渲染
- **可观测**：所有进程统一 slog 配置，支持级别 / 输出 / 格式 / 分组件级别
- **部署**：systemd unit + `deploy.sh`（含 `--binaries` `--services` `--init`）+ 二进制 tarball 分发

### 测试

测试代码占比 50.4%（18,975 / 37,686 行），644 个测试函数，`go test -race ./...`
全绿，`go vet` 干净。`cmd/*/main_test.go` 覆盖各二进制入口的错误路径，
`internal/protocol`/`internal/config`/`internal/feishufront` 表驱动覆盖每条
validate 与 enum 校验路径。

### 1.0.0 发布前审计修复

发布前对照 [`docs/release-1.0-audit.md`](docs/release-1.0-audit.md) 完成全部 P0
与 P1 阻断/严重项，以及 P2 大部分打磨项，主要落地：

- **稳定性**：abort 后子进程组 SIGKILL（`cmdutil.ApplyGroupCancel`/`RunCombinedBounded`）、
  关键路径 panic recover（control pump + 3 个 SDK 入口）、picker RPC 30s 超时、
  `bot.Restart` 串行化、卡片 element 50 上限防御、`PromptPayload` 覆写字段协议级拒绝。
- **协议**：`todo.status`/`priority`/`notice.level` enum 硬校验。
- **资源**：`wasOffline` 上限触发全量重置、`opencode-serve` Close 与 LRU 并发守门、
  `accText` 峰值减半、`MaxStreams` 从 `AgentConfig` 注入、`DispatchCardAction`
  单 Lock 段防双 finalize。
- **配置/部署**：`OpencodeServe.MaxConcurrent` defaults+validate 对齐、
  `deploy/*.json` 与 `.env` 经 `${VAR}` 联动、deploy.sh 二进制存在性全检 +
  `log_level` 占位符 regex 容错、upgrade-monitor.sh SC2015 修正。
- **文档**：Makefile/README/deploy 二进制与服务数真源统一（6 个二进制 / 5 个业务
  systemd 服务）、deploy/README 双重真源警示、补 LICENSE（MIT）与 CHANGELOG。

### 已知限制

详见 [`docs/release-1.0-audit.md`](docs/release-1.0-audit.md) 的 P2/P3 章节。
