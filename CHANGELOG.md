# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 风格，版本号
遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Removed

- **opencode-serve-back 整体移除**：CLI 模式（`opencode-back`）功能已对齐，
  独立维护两套 opencode 对接代码（CLI 子进程 vs `opencode serve` HTTP）的成本
  超过收益。本次移除包括：
  - `cmd/opencode-serve-back/` 与 `internal/opencodeservebridge/`（约 7800 行）。
  - `opencode-go-sdk-lite` Go 依赖（仅此包使用）。
  - `config.OpencodeServe` 字段/默认值/校验/测试。
  - `deploy/opencode-serve-config.json`、`Makefile` build target、`deploy.sh`
    的 `probe_opencode_serve` / `svc_unit` 分支 / SELECTED 默认数组 / 派生 config。
  - `deploy.sh` 新增遗留清理：升级时自动 `disable --now` 并删除已部署的
    `lark-opencode-serve-back.service` unit、`/etc/lark-bridge/opencode-serve-config.json`
    以及 `STATE_DIR` 下的 `opencode-serve-router.json` / `usage-opencode-serve.json`。

### Added

- **opencode-back 新增 `/session-use`**：从 opencode-serve-back 移植。CLI 模式
  通过 `--session <id>` 续接历史会话，无 serve 模式的 `SessionStatuses` 实时
  busy 检查（CLI 模式无跨进程 session 状态共享，目标 session 始终视为可切换）。
  与 `/session-list` 共享排序（`sortSessionsByUpdated`），序号一致。

## [1.1.0] - 2026-07-25

交互卡片与 git/deploy 命令的呈现与生命周期重构。所有改动向后兼容（新增协议字段均
omitempty；旧前端遇 null 自动回退）。

### Added

- **进度卡交互门横幅**：mid-turn permission/question 阻塞态、picker 加载态现以
  `TypeProgress` banner 呈现在流式进度卡顶部（`ProgressPayload.Gate`/`Description`），
  不再走被 dispatcher 丢弃的 `TypeText`。banner 四态：⏸ waiting / ✓ answered / ✗
  denied / • loading。
- **权限卡结构化正文**：`PermissionPayload` 增 `Type/Title/Detail`，渲染为徽标 + 标题
  + 代码块详情；`Title` 空时自动回退 `Message`。
- **question 自适应渲染**：单问、单选、≤4 项、无自定义输入 → 即时按钮卡（免下拉+提
  交两步）；其余仍走表单。
- **/deploy-force 二次确认门**：destructive 部署现需 TypePermission 卡片确认（复用
  `bridgebase.AnswerBroker` + TypeAnswer 路由）；普通 /deploy 不加门。
- SSE 静默时 `OnIdle` 兜底取回最终回复。

### Fixed

- **turn 泄漏修复**：`/pull` `/push` `/deploy` `/deploy-force` 终态现绑定 replyToID，
  进度卡不再卡死"处理中"、`/v1/status InFlight` 不再虚高（原会阻塞 `deploy.sh`）。
  命令改为单卡生命周期（非终态 banner → 终态 in-place patch）。
- **已应答卡推进到"已完成"**：submit 不再删缓存+摘绑，`finalizeLinkedInteractive` 能
  把同一张卡从 submitted 推进到 finalized，保留"✓ 已回答"echo（C5）；`rewriteFooterStatus`
  推广为 `待确认|处理中 → X`，终态粘住不可回退。
- **死回显清理**：删除桥层发出的、被 dispatcher 丢弃的 `TypeText` 应答回显；picker 加载
  文案迁到 `TypeProgress.Description`（opencode-back / opencode-serve-back / miniagent）。
- **tail 输出按 rune+行边界截断**：修中文日志（3 字节/字）被字节截断产生的乱码与半行。
- **"始终允许"标注全局作用域**：`PermissionReplyAlways` 的全局持久授权在按钮上显式标
  注「（全局）」。

### Changed

- `bridgebase.AskPermission` 签名收 `protocol.PermissionMessage`（结构化正文载体）。
- `bridgebase.WithReplyToID` 导出（`ReplyToID` 的逆，供测试/直驱命令 handler 用）。
- `bridgebase.GitRunner.AcquireAndRun` 改返回 `bool`，不再自发"已触发"（caller 发 banner）。
- deploymonitor 拆 `confirm.go`（force 确认门）/`render.go`（格式化）以守住 300 行上限。
- `opencode-go-sdk-lite` → v0.2.0；ListModels/ListAgents 缓存移至 `lists.go`。

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
