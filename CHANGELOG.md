# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 风格，版本号
遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)。

## [Unreleased]

## [1.3.0] - 2026-07-28

飞书客户端完全自实现（移除 `oapi-sdk-go` 全部依赖），并新增 status-monitor 总览卡
后端。同步落地 P0–P3 一轮代码评审加固（资源边界 / 鉴权 / 子进程组 / WS 健壮性 /
systemd hardening）。所有改动向后兼容：对外协议字段不变；移除的内部 SDK 类型仅影响
`internal/feishu/`。

### Added

- **status-monitor 后端**（`cmd/status-monitor` + `internal/statusmonitor/`）：
  以 backendType `status-monitor` 注册到前端，按 `status_monitor.interval`（默认
  60s）轮询 `GET /v1/status` 并发 `TypeStatusReport`；前端向每个绑定此后端的群推送
  一张常驻总览卡（在线后端 + 运行中会话数与时长），有则 PATCH、被删则重发。
  push-only；与 deploy-monitor 同样解耦于 `deploy.sh` 之外，由 `make upgrade-status`
  独立管理。
- **renderer subagent zone**：进度卡为 agent 委派（sub-agent delegation）新增专属
  呈现区域，与主 turn 的工具行分离。
- **架构与规范文档**：新增 `ARCHITECTURE.md`（仓库级架构真源）与 `CODING_STANDARDS.md`；
  补充飞书开放平台 API 参考文档。
- **renderer `max_thinking_runes` 配置项**：进度卡"思考中"区的 rune 上限从硬编码 50
  改为可配置（`renderer.max_thinking_runes`，默认仍 50）。同时 Claude 的 thinking
  内容块接入 `TypeThinking` zone（`Replace: true`，与 opencode reasoning 行为对齐）。

### Changed

- **飞书客户端完全自实现**：移除 `github.com/larksuite/oapi-sdk-go/v3` 及其间接依赖
  `gorilla/websocket`、`gogo/protobuf`。`go.mod` 现仅含标准库。新增 `internal/lark/`
  （RFC 6455 WebSocket 客户端 + 手写 protobuf 帧编解码 + 鉴权/REST/重连/分片重组），
  `internal/feishu/` 改为对 `*lark.Client` 的业务封装层。
- **`feishu.Bot.Restart` / `feishu.ErrTooManyRestarts` / `restartMax` 删除**：新客户端
  自管重连、无 goroutine 泄漏，软重启机制不再需要。`cmd/feishu-front` 看门狗
  简化为「超 fatalAfter 仍不健康 → 退出交 supervisor 拉起」。
- **`feishu.IncomingMessage.Mentions` 类型替换**：由 SDK 的 `sdktypes.Mention` 改为
  本包 `Mention`（字段集不变，下游 `internal/feishufront` 零改动）。
- **P1 资源/鉴权加固**：补全各类上界（buffer / 重连预算 / sweep）、IPC 鉴权强度提升、
  子进程 env 收敛到白名单传递。
- **P2 清理**：死代码删除、flap 状态机竞态修复、deploymonitor graceful drain、
  wsclient 按职责拆分。
- **P3 工程化**：build tags、systemd unit hardening、sudoers 指引、补测试。

### Fixed

- **WS `card.action.trigger` ACK 3 秒回滚**：ACK 现把业务字段置于顶层并携带 `card`，
  阻止飞书侧 3 秒未应答导致的卡片状态回滚。
- **picker 点击无更新**：卡片 schema 回退到 v1 让 PATCH 持久化，点击改为延迟 PATCH
  （`context.WithoutCancel` 脱离请求生命周期），并删除失效的 ACK 死代码。
- **claude `server_tool_use` 不再丢失**：服务端工具调用现正常呈现，`task_kind`
  在整个 turn 生命周期内稳定。
- **P0 修复**：飞书 mention bot 识别字段名修正、子进程漏接进程组 kill 致
  `cmd.Wait()` 永久阻塞、WS receiveLoop 半开连接（新增读超时）。
- **status-monitor 多行 `base` 配置校验**：`deploy.sh` 在多行 base 下不再误判。
- **deploy.sh 配置字段迁移**：升级时在重启 monitor 前迁移已移除的 config 字段。

## [1.2.0] - 2026-07-26

opencode-back 与 claude-back 的工具事件呈现重构，外加 `claude-go-sdk` 内联
消除一项直接 module 依赖。所有改动向后兼容（`TypeTodo` 协议早就有，本次只是
首次接入；`Mention` 类型替换在内部包，对外 API 不变）。

> 本段同时补记 v1.1.0 期间合入但当时未在 CHANGELOG 注明的两项
> （`opencode-serve-back` 整体移除、`opencode-back /session-use`）。

### Added

- **进度卡 Zone 3.5 todo 清单渲染**：opencode-back 的 `todowrite` 与
  claude-back 的 `TodoWrite` 工具事件现路由到 `TypeTodo`，进度卡片原生展示
  ✅/⏳/⬜/✘ 清单（≤10 项展开、>10 项折叠为 `清单 N/M · ✅a ⏳b ⬜c ✘d`、
  cancelled 灰显），不再以 raw-JSON 工具行呈现。失败 fallback 仍走 `TypeToolResult`
  以保留可见性。
- **`/backend` picker 10min TTL**：未被点击的选择卡 10 分钟后自动翻"已失效"，
  与后端 interactive 卡的 TTL 行为对齐；点击即取消定时器。
- **opencode-back 新增 `/session-use`**（v1.1.0 期间合入，补记）：从
  opencode-serve-back 移植，CLI 模式通过 `--session <id>` 续接历史会话。

### Changed

- **`claude-go-sdk` 内联**：从外部 module 依赖变为 `internal/claude/` 子包
  （9 个非测试文件 + 10 个测试，~2200 行）。lark-bridge 的 `require` 从 2 个
  缩减到 1 个（仅剩 `larksuite/oapi-sdk-go/v3`）。`NOTICES.txt` 记录上游 commit。
  Logger 通过 `log.Logger = slog.Logger` type alias 零适配。
- **`/backend` picker 一卡片原则**：离线/`router.Set` 失败路径改为原地 `UpdateCard`
  翻红（不再发独立 notice），与成功路径的绿色翻红对称。`renderBackendResult`
  泛化为 `renderBackendOutcome(level)`，picker footer status 从「选择后端」改
  为「待确认」让 `RenderInteractiveExpired` 的 footer 翻转生效。
- **进度卡工具行字段上限 50 runes**：`name` 与 `desc` 各自独立常量
  （`maxToolNameLen` / `maxToolDescLen`，与已有 `maxToolOutputLen` 同值但独立
  命名保留语义），通过 `truncateRunes` 截断 + `…` 后缀。` ×N` 计数后缀不算
  入 50 runes 预算。

### Fixed

- **opencode `edit` 工具的 title 缺失不再 dump 完整 input JSON**：
  opencode CLI 部分版本不在 edit 工具事件里填 `part.title`，旧 fallback
  `stringifyJSON(state.input)` 会把整个 input（含 `oldString`/`newString`
  数百字符）当作工具行 desc，且后续 `(+N -M)` diffstat 拼接破坏 JSON 结构
  让下游 `SummarizeToolInput` 无法二次提取。新增 `extractToolInputField`
  按优先级表（`file_path`/`filePath`/`command`/`pattern`/`path`/`query`/
  `description`）提取单字段，仅当无匹配时落回 `stringifyJSON`。
- **`/backend` 提示文案错误**：`dispatcher.go` 的"后端离线"提示指向不存在的
  `/backend use {id}` 子命令（README 同样过时），改为「请用 `/backend`
  重新选择在线后端」。

### Removed

- **`opencode-serve-back` 整体移除**（v1.1.0 期间合入，补记）：CLI 模式
  （`opencode-back`）功能已对齐，独立维护两套 opencode 对接代码（CLI 子进程
  vs `opencode serve` HTTP）的成本超过收益。本次移除包括：
  - `cmd/opencode-serve-back/` 与 `internal/opencodeservebridge/`（约 7800 行）。
  - `opencode-go-sdk-lite` Go 依赖（仅此包使用）。
  - `config.OpencodeServe` 字段/默认值/校验/测试。
  - `deploy/opencode-serve-config.json`、`Makefile` build target、`deploy.sh`
    的 `probe_opencode_serve` / `svc_unit` 分支 / SELECTED 默认数组 / 派生 config。
  - `deploy.sh` 新增遗留清理：升级时自动 `disable --now` 并删除已部署的
    `lark-opencode-serve-back.service` unit、`/etc/lark-bridge/opencode-serve-config.json`
    以及 `STATE_DIR` 下的 `opencode-serve-router.json` / `usage-opencode-serve.json`。

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

发布前完成全部 P0 与 P1 阻断/严重项，以及 P2 大部分打磨项，主要落地：

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
