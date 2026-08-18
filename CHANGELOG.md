# Changelog

本项目遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 风格，版本号
遵循 [Semantic Versioning](https://semver.org/lang/zh-CN/)。
## [Unreleased]

### Removed
- **移除 miniagent.mode 配置字段 + `/mode` 命令**（**breaking**）：miniagent v5.0.0 删除 `-mode` 双模式（default/auto 合并为单模式），bridge 硬切对齐——不再发射 `-mode` flag、移除 `/mode` 斜杠命令与 `settableModes`/`modeOptions`、`Binding.Mode` + `Router.SetMode`、`Config.MiniAgent.Mode` + `applyDefaults` 默认 + `validate` 校验、`Client.Mode`/`DefaultMode()`/`RunOptions.Mode`、`activeMode()`/`clientDefaultMode()`、`activeTurnConfig` 从 6 元组降 5 元组。`minSupportedVersion` 从 `4.2.0` 提升到 `5.0.0`（4.x 二进制启动健康门直接拒绝，避免双安全语义并行）。**迁移**：从 config JSON 删除 `"miniagent.mode"` 键（DisallowUnknownFields 会使旧配置启动失败）。router JSON 的 `mode` key 被 `json.Unmarshal` 静默忽略，无需迁移。
- **移除 agnes-back 后端**（`89e29e6`，**breaking**）：Agnes AI 图片/视频生成后端整目录删除（`cmd/agnes-back` + `internal/agnesback`，8 文件 / 2494 行），接受永久丢失媒体生成能力。config/Makefile/deploy 脚本全量清零；`deploy-status.sh` removed_blocks 加 `"agnes"` 迁移。
- **移除 deploy-monitor 独立服务**（`89e29e6`）：状态监控逻辑合并回 `deploy.sh`，`cmd/deploy-monitor` + `deploy/deploy-monitor.sh` 删除。
- **移除 config Timeouts 废弃字段**（`0021ca1`）：`PromptTimeout`/`IdleTimeout`/`UsageSessionTTL` 删除。
- **移除 bridgebase 死代码**（`f8a7239`）：`ack_registry`/`interactive`/`util` 清理。
- **移除 strutil / eventmetrics 死代码**（`7f5c85c`）。
- **移除 protocol subagent 委派区**（`7f8e172`）：`ToolUse`/`ToolResult` payload 简化。

### Changed
- **backendrpc 移除 Ack 机制**（`ad84f98`）：`Run` 重命名为 `RunWithClient`。
- **goSafe 独立为 internal/gosafe 包**（`be5131f`）：`commands_dispatch` 从 bridgebase 迁至 miniagent。
- **deploy.sh 不再编译**（`d396df7`）：`make build` 和 `make deploy` 成为独立步骤。
- **deploy 脚本简化**（`520a11c`/`7cd9996`）：移除 `ComponentLogLevels` + `SERVICES` 影子数组，合并 per-service 分支。
- **miniagent 服务 HOME 统一**（`df3fb94`）：指向 `/home/$RUN_USER`，注入固定 PATH。
- **renderer 错误行聚合**（`5b91a10`）：旧错误行折叠为按名称聚合的摘要。

### Fixed
- **status-monitor backend_id 冲突**（`6d5c9af`）：独立 backend_id 避免注册碰撞。
- **deploy shell 逻辑修复**（`6edf3f1`）：grep/systemctl 包裹在 if 块中以修正退出状态。
- **smoke.sh / lib-common.sh 权限修复**（`e76a153`/`8814850`）。

### Developer Experience
- **ARCHITECTURE.md 全量刷新**（`b965415`）：gosafe 拆分、bridgebase 瘦身、deploy/build 分离。
- **miniagent CLI workdir 契约钉死**（`0494eed`）：非空 + 绝对路径 + 只认 `-workdir` flag。

## [1.14.0] - 2026-08-10

主线：**agnes-back 新后端**（Agnes AI 图片/视频生成接入飞书）+ **卡片链路全量切换 schema 2.0**（删除 legacy 1.0 双路灰度，**breaking**）+ **心跳 / 重试 / 诊断日志加固**（三后端补 Pong 应答、SendControl 瞬态重试、ws 看门狗放宽）。含新增功能与 breaking change，升 minor。发版顺序：先 feishu-front，后各 backend。

### Added

- **agnes-back 新后端**（`08e3033`）。新增 `lark-agnes-back`（backendType `agnes`），封装 Agnes AI 三个模型：`agnes-2.5-flash`（文本提示词 `/image-prompt`、`/video-prompt`）、`agnes-image-2.1-flash`（图片生成 `/image`，图片经 TypeFile 内联到群）、`agnes-video-v2.0`（视频生成 `/video`，异步轮询）。架构仿 status-monitor：SSE 注册 + POST 发 Control，不 fork 子进程；慢任务跑在 GoSafe goroutine 不阻塞 SSE 循环，视频轮询期间用 Progress 卡片显示进度。配置段 `agnes`（api_key / base_url / 三模型名 / size / ratio）全部可配（`config.example.json` + `AGNES_*` env 已补）；新增 `deploy-agnes.sh` 与 Makefile `build-agnes-back` / `deploy-agnes` 目标。
- **agnes 视频生成后下载并以文件发送到飞书**（`bf3d54e`）。视频结果从仅下发 URL 改为下载后作为文件消息发送到群。
- **backendrpc SendControl 瞬态错误重试**（`b367ca1`）。指数退避，最多 3 次，降低视频生成等长任务期间前端抖动导致的通知丢失。
- **全链路诊断日志**（`b3553ee` / `e5611b3` / `296ec8f`）。agnesback handler / client、feishu bot_send、dispatcher、ipcserver 在关键控制流节点补结构化日志（notify 请求/结果、PatchMessage 失败 Warn 含 message_id/card_size、prompt 转发、control auth 失败），并清理每步成功/失败双份 Info 及 ipcserver 无效 `http.MaxBytesReader` 废代码。

### Changed

- **卡片链路全量切换 schema 2.0，删除 legacy 1.0 双路灰度**（`b214834`，**breaking**）。所有卡片仅以 schema 2.0 渲染，legacy 1.0 渲染路径与灰度开关一并删除；任何依赖 schema 1.0 输出的定制不再适用。
- **ws 看门狗超时 5m → 10m**（`e73713e`）。容忍视频生成（1–2min）期间瞬态网络中断，避免误杀健康长连接。
- **两个独立后端升级路径补 .env 同步**（`8ef267b`）。agnes / status-monitor 的 deploy 脚本升级时同步刷新 .env。
- **migrate_config 增加块内叶子字段清理**（`5e7f59f`）。`card_patch_delay` 等废弃字段迁移时自动清除。
- **agnesback 错误处理增强**（`e73713e`）。`handleImage` / `handleVideo` 的 SendControl / notify 返回值改为显式错误处理，失败 Error 日志含 card_msg_id；notify 拆分为参数日志 + 发送日志两步。
- **agnes 视频轮询查询级容错**（`8c6dd2b`）。连续 3 次失败才放弃，单次瞬态失败不再中断整个轮询。

### Fixed

- **schema 2.0 交互卡片按钮点击全部无回调（P0）**（`e223940`）。根因：`b214834` 全量切 schema 2.0 后 button 无条件声明 `behaviors:[{type:callback}]`，而飞书 schema 2.0 inline 卡片携带此声明时反而不触发 `card.action.trigger` 事件（实测：移除 behaviors 后回调立即恢复），导致 `/backend` picker、permission、question 等所有交互卡片按钮失效。修复：`ButtonAction` / `SubmitButtonAction` 移除 behaviors 声明（button 的 value 字段自动触发回调，与 schema 1.0 行为一致）；同提交修复 picker 列表省略号问题。
- **agnes / status-monitor 周期性 offline/online 抖动（P1）**（`dad828b`）。两者 `HandleEvent` 此前静默丢弃前端 TypePing 健康探测，missedPongs 只增不减，每 ~60–90s 被 feishufront 按 `maxMissedPongs=3` 判定消费循环卡死而强制 Unregister。修复：对齐 miniagent-back 模式，GoSafe fire-and-forget 回 TypePong。
- **agnesback 图片/视频结果更新回弹**（`8091aa6`）。不再用 UpdateMessageID 更新原卡，改为发新卡。
- **lint 回归**（`110e66d`）。agnesback handler_test goimports 对齐 + SA2001 空临界区。

### Notes

- **operator 升级提示**：agnes-back 为新增可选服务，按 `deploy/README.md` §6.7 执行 `make deploy-agnes ARGS=--init` 初始化；bridge config 需新增 `agnes` 段（或经 env 注入 `AGNES_*`）。
- **已知限制**：agnes 视频 CDN 偶发 TLS reset，属外部服务问题，重试可恢复；form_submit 交互路径尚未实测。
- **发版顺序**：先 feishu-front，后各 backend（claude / miniagent / agnes / status-monitor）。

## [1.13.0] - 2026-08-09

主线：**CardKit 卡片实体引擎**（解决 cardID 透传与卡片验证问题）+ **miniagent v4.0.1 / v4.2.0 跟进**（会话/provider/config）+ **跨后端指令命名统一**（`/mode` `/effort` `/abort` `/config`）+ **scaffold 层 prompt runner 重构**（idle watchdog + session generation guard）。新增功能为主，升 minor。

### Added

- **CardKit 卡片实体引擎（方案 B）**（`3aa9375`）。`internal/feishufront/cardkit` 引入显式 cardID 透传机制，将卡片生命周期从 messageID 耦合解绑到独立的 cardID 维度，解决 `/send` 多阶段卡片流中 cardID 丢失导致更新落空的问题。
- **卡片更新验证机制（UpdateCardVerified）**（`95b1711` / `3918a84`）。`Bot.UpdateCardVerified` 在 PATCH 后回读飞书实际存储的卡片内容，与预期做 elements 内容指纹比对（从 header.template 指纹升级为 elements 指纹），检测静默丢失/拒收。
- **`turn_started` / `turn_finished` 协议控制**（`9b79c34`）。新增两个 protocol control type，用于 running-session 在前端侧的精确对账：turn 开始/结束时各发一帧，前端据此校正运行中状态的时序边界。
- **miniagent `/config` 斜杠命令**（`8389dce` / `71349e0`）。每 chat 切换 `-config` 文件（miniagent-cli.json）；config_path 支持经 config_dir 解析 + 自动发现，picker 列出配置目录下的候选文件。
- **miniagent 适配 v4.0.1 会话机制**（`64c88a8`）。支持 `-save-session` / `-session` 双模式：新会话自动 save-session 落盘 sessionID，后续 turn 经 `-session` 复用，与 claude-back 的 session 持久化对齐。
- **miniagent 适配 v4.0.1 provider 机制**（`5f9314d`）。`-model` / `-provider` 成对传递；`/model` picker 选中后按 label 反查 ModelRef 写入 binding，保持 provider/model 配对。
- **miniagent `/current` 显示会话 ID**（`bb556b3`）。
- **miniagent todo 工具卡片化渲染**（`bbabcb5`）。TodoWrite 工具调用渲染为卡片待办区，替代原始工具行。
- **统一 mode/perm 与 effort/thinking 指令**（`645b479`）。claude-back `/perm` → `/mode`（函数名 `cmdPermission` → `cmdMode` 等同步重命名）；miniagent-back `/mode` 空参补交互式 picker（对齐 claude-back），`/thinking` → `/effort` 且空参补 picker。

### Changed

- **scaffold 层 prompt runner 重构**（`a461101`）。引入 idle watchdog（每轮独立超时）+ per-invocation result metadata，替代旧的固定 timeout + 全局 metadata。
- **session generation guard + cancelableWaitGroup**（`2b90352`）。`Binding` 新增进程内 `Generation` 字段，`SetSessionIDIfGeneration` 防止旧 turn 的 session id 覆盖新 binding；`sync.WaitGroup` → `cancelableWaitGroup`，`WaitPrompts` 超时后可取消。
- **跟进 miniagent v4.2.0：移除 `-system`/`-max-tokens` 交付**（`4febfd9`）。上游 4.2.0 删除两 flag（stdlib `flag` 遇未知 flag `exit(2)`），桥停止发射并从 config 删除 `system_prompt`/`max_tokens` 字段；system prompt 与 max output-token cap 改由 miniagent-cli.json 的 `defaults.system_prompt` / `run.max_tokens` 配置。**迁移**：因 `DisallowUnknownFields`，手改 config 须删此两键。
- **统一 claude-back 指令 `/settings` → `/config`**（`57bbf33`），与 miniagent-back 命名对齐。
- **统一 `session-abort` 指令为 `/abort`**（`d597ab8`）。
- **部署脚本简化与重命名**（`780dc42`）。Makefile 拆出单二进制 build target（`build-status-monitor` / `build-services`），消除冗余跨二进制编译；`upgrade-status.sh` → `deploy-status.sh`，动词统一为 `deploy`。
- **交互卡锁顺序统一**（`2b90352`）。先操作 `TurnManager`，再操作 `Dispatcher.cardMu`，消除 `sendInteractive` 与 `expireInteractive`/`finalizeLinkedInteractive`/`DispatchCardAction` 之间的锁顺序反转死锁风险。
- **示例配置默认脱敏**（`config.example.json`）。`stream_archive_redact` 由 `false` 改为 `true`，与代码默认值一致。
- **后端 token 比较改为恒定时间**（`2b90352`）。`validateBackendToken` 使用 `crypto/subtle.ConstantTimeCompare`，降低 per-backend session token 时序攻击风险。
- **minSupportedVersion 3.5.0**（`d1b748c`）。miniagent v4.0.1 引入 breaking changes，版本门同步提升。

### Fixed

- **`/send` 目录浏览改即点按钮（P0）**（`78be0d0` / `bcd6150` / `5cb3c1d`）。修复提交回弹（选完文件后卡片弹回浏览态）、已发送双卡（发送结果卡片重复）、以及选择文件卡片被飞书拒收（按钮格式不兼容）。改为即点按钮模式，一阶段完成。
- **文件发送后"已选择"残留 PATCH**（`36538f1`）。`/send` 文件发出后清除上一轮选择残留，避免后续 PATCH 落到已失效的 picker card。
- **UpdateCardVerified 指纹改 elements 内容指纹**（`3918a84`）。原 header.template 指纹在卡片无 header 时恒为空导致误判通过，改为 elements 内容指纹（解析 + re-marshal 规范化 key 顺序）。
- **终端卡片守卫泛化**（`ed99b65`）。将 terminal-card guard 从仅守护 `/send` 扩展到所有延迟/TTL 卡片写入，防止过期 TTL 卡片 PATCH 覆盖已最终化的卡片。
- **reclaim 清理 registry runningTurns**（`5feefd0`）。reclaim 路径未清理 `runningTurns` 导致 turn 槽位泄漏。
- **`Core.WaitPrompts` goroutine 泄漏**（`2b90352`）。`cancelableWaitGroup` 使 `WaitPrompts` 超时返回后不再泄漏阻塞在 `Wait` 的 goroutine。
- **miniclient pump 无保护发送**（`2b90352`）。子进程 pump 在 ctx 取消后向 `out` 发送时加 `select`，避免消费者退出后阻塞并耗尽 `MaxConcurrent` 槽位。
- **router 持久化静默失败与损坏 binding 丢失**（`2b90352`）。`save` 增加 3 次线性退避重试；`load` 遇到损坏 binding 时先备份为 `.corrupt.<timestamp>` 再跳过，防止下次 save 永久丢失原始数据。
- **SSE 握手定时器竞态**（`2b90352`）。`httpClient.Do` 返回后、停止握手定时器前，若定时器已触发导致 ctx 取消，主动关闭响应 body 并返回握手超时。
- **Claude sessionID 写入竞态**（`2b90352`）。`SetSessionIDIfGeneration` 防止旧 turn 的 session id 写入 `/session-del` 后新建的 binding。
- **miniagent 工具输入卡片显示原始 JSON**（`44c7952`）。工具输入摘要化展示，避免裸 JSON envelope 塞入卡片。

### Removed

- **`/models` 和 `/session-del` 指令**（`08cd211`）。`/models` 功能收敛进 `/current` 显示；`/session-del` 收敛进 session 管理流程。

### Notes

- **operator 升级提示**：miniagent v4.0.1+ 需更新本地 miniagent 二进制（`minSupportedVersion` 3.5.0）；手改 config 须删 `miniagent.system_prompt` / `max_tokens` 两键（v4.2.0 适配）。
- **`/mode` 行为变更**：claude-back `/perm` 改名为 `/mode`，miniagent-back `/mode` 空参从显示当前值改为弹出 picker；miniagent-back `/thinking` 改名为 `/effort` 且空参改为 picker。
- **发版顺序**：先 feishu-front，后各 backend（claude / miniagent）。

## [1.12.0] - 2026-08-03

主线：**opencode / omp 后端整体移除**（13 个 commit）+ **claude 会话命令命名精简**（`/session-new/list/clean/use` → `/new` / `/session` / `/clean` / `/use`）+ **miniagent v3.3.0+1 / v3.4.0 跟进**（deploy `${VAR}` 展开修复、health gate 升级、`-key-file` flag 移除适配）。协议层零 breaking change，新增 / 重构均升 minor。

### Added

- **miniagent health gate**（760620b，v1.11.0 合入）。`cmd/miniagent-back` 连接前端前调用 `client.IsReady`，CLI 缺失或版本过旧时快速失败。
- **MiniAgent 指标**（760620b，v1.11.0 合入）。`internal/eventmetrics` 新增 `MiniAgentTurnCount`、`MiniAgentTurnDurationMs`、`MiniAgentTurnInputTokens`、`MiniAgentTurnOutputTokens`、`MiniAgentTurnIncomplete` 五个计数器。

### Changed

- **claude 会话命令命名精简**（`b5b3017` / `7d1b37f` / `ae0e9de` / `4ecbd59`）。`/session-new` → `/new`、`/session-use` → `/use`、`/session-clean` → `/clean`、`/session-list` → `/session`，与 miniagent-back 命名对齐。涉及命令注册、实现函数注释、错误提示、feishufront 卡名同步及文档。
- **`minSupportedVersion` 3.1.0 → 3.3.0**（`0414716`）。`internal/miniclient/client.go` 版本门提升，health gate 明确拒绝旧版二进制。新增 `TestCompareVersion` / `TestSatisfiesVersion` 钉死 component-wise 比较规则。
- **`StreamArchiveRedact` 默认值翻转**（760620b，v1.11.0 合入）。字段类型由 `bool` 改为 `*bool`，nil → 默认 true；新增 `RedactStreams()` 辅助方法。
- **`config_path` 绝对路径校验**（760620b，v1.11.0 合入）。`cmd/miniagent-back` 启动时拒绝相对路径。
- **miniagent v3.4.0 `-key-file` flag 移除**（`712ed9b`）。上游移除 `-key-file`，bridge 改由 `effectiveAPIKey()` 直接读取 `key_file` 并将内容注入 `$MINIAGENT_API_KEY`；KeyFile 取代之 APIKey 优先，每次 `Run` 新鲜读取以支持密钥轮换。

### Fixed

- **deploy 期 `${VAR}` 展开（P0，升级即崩）**（`0414716`）。miniagent v3.3.0 `c51d91c` 移除了 config 加载时的 `${VAR}` 展开，`deploy.sh` 生成 `miniagent-cli.json` 的 heredoc 原先带 `'EOF'` 引号导致 URL 被字面量传递。改为 unquoted heredoc 让 bash 在 deploy 期展开，与 bridge 自身 config（仍走 `expandEnvVars`）解耦。
- **`/models` 当前模型标记适配 v3.3.0+1**（`8a472c2`）。上游 `b9a38fa` 统一输出为 `provider/model_id`，`cmdModels` 现检测单 provider 后将裸 current 规范化为 `provider/model_id` 再比较；多 provider 下裸 default 保持不标记。
- **lint 4 项**（760620b，v1.11.0 合入）。
- **renderer 删除 `Multi_edit` 死分支**（`0414716`）。miniagent v3.2.0 `edd6ba5` 将 `multi_edit` 并入 `edit` 的 `edits` 数组，`progress_category.go` 移除对应 case。

### Removed

- **`opencode-back` 与 `omp-back` 整体移除**（`e22ac59` / `45bd5ea` / `7ffda4c`）。后端对接收敛到 claude + miniagent 两个 agent。移除范围：`cmd/opencode-back/`、`cmd/omp-back/` 与 `internal/opencode/`、`internal/opencodebridge/`、`internal/omp/`、`internal/ompbridge/`（约 1.3 万行）；`config.Opencode` / `config.OMP` 结构体、默认值、校验与测试；`OPENCODE_INTEGRATION_SPEC.md` / `OMP_INTEGRATION_SPEC.md`；`Makefile` 的 `lark-opencode-back` / `lark-omp-back` build 与 pack 项；`config.example.json` 的 `opencode{}` / `omp{}` 块；`deploy.sh` 服务列表、产物检查、config 派生、CLI 警告及 `lib-common.sh` 映射；`upgrade-status.sh` 遗留清理扩展。
- **`/session-new` 等 claude 会话命令旧名**（`b5b3017` / `7d1b37f` / `ae0e9de` / `4ecbd59`）。`/session-new` / `/session-use` / `/session-clean` / `/session-list` 旧名从命令注册表中移除，统一替换为 `/new` / `/use` / `/clean` / `/session`。
- **`/memory` 命令与 `.miniagent/memory.jsonl`**（`0d90584` / `9b07db4`）。项目级记忆系统移除，`/memory` 命令及其相关处理逻辑、`memory.jsonl` 写入与 `.gitignore` 条目一并清理。

### Notes

- **operator 升级提示**：`make deploy` 会自动清理 `lark-opencode-back` / `lark-omp-back` unit、state 及 config；已绑定 opencode/omp 的群需重新 `/backend` 切到 claude 或 miniagent。
- **`/new` vs `/session-del`**：`/new` 保留工作目录仅重置会话上下文；`/session-del` 删除绑定（含目录）。claude 与 miniagent 行为一致。
- **`key_file` 安全提示**：miniagent v3.4.0 后 `-key-file` flag 已移除，bridge 自身读取文件并注入 `$MINIAGENT_API_KEY` 到子进程 env。key 隔离依赖 OS 权限（dedicated user + 0600 config/key files）。
- `config_path` 字段在 v3.1+ 已进入必填状态；本次追加绝对路径约束。

## [1.10.0] - 2026-08-02

miniagent **v2.0.0** 全量接入。主线：吸收上游 v2.0.0 的外部契约破坏（`shell` 退出码语义）+
新事件（`tool_result` / 流式 `text_delta`·`reasoning_delta`）+ 新 CLI flags + 新工具分类；协议层零
改动（`TypeToolResult`/`TypeThinking`/`TypeText` 早被三桥使用，miniagent 仅转发）。详见
`docs/MINIAGENT_V2_INTEGRATION_PLAN.md`。新增功能 → 按 semver 升 minor。

miniagent **v3.0.0** 全面落地（破坏性迁移）。主线：吸收上游 v3.0.0 的外部契约破坏
（`-base-url`/`-confine` 删除 → 完整 URL + `-mode`）、新增代码开发能力（`-mode default` 护栏）、
每 chat 自动会话记忆（`-session`）、可选多 provider 配置（`-config`）。详见
`docs/MINIAGENT_V3_MIGRATION.md`。破坏性配置变更 → 按当前 1.x 惯例仍升 minor（CHANGELOG 显式标注）。

### Added

- **miniclient 解析 v2.0.0 事件**（`4635804`）。`event.go` 扩 `Event`/`rawEvent` 容纳 `tool_result`（`output`/
  `truncated`/`is_error`/`exit_code`）、流式 `text_delta`/`reasoning_delta`（`step`/`text`）；新增
  `KindToolResult`/`KindTextDelta`/`KindReasoningDelta` 常量。未知 type 仍 `ok=true` 非终态（前向兼容，
  未来事件不破泵）。旧版 v1.1.0 二进制不产出这些事件，零影响。
- **`tool_result` 透传**（`4635804`）。`miniagent.emitCLIEvent` 增 `KindToolResult`→`TypeToolResult`。v2.0.0 破坏性
  变更的唯一落地处：`shell` 非 0 退出 `is_error` 由 true 改 false，改由 `exit_code` 表达——handler 据此
  把 `[exit N]` 拼进 `Output` 首行（不改协议），`IsError` 仅在 `exit_code<0`（超时/启动失败）或非 shell
  工具的 `is_error=true` 时置位。
- **流式输出**（`4635804`）。`config.MiniAgent.Stream`（默认关）→ `miniclient` 传 `-stream`；`emitCLIEvent` 增
  `KindTextDelta`→`TypeText`、`KindReasoningDelta`→`TypeThinking`（append 模式，渲染层「思考中」区已截尾）。
  与 claude/opencode 流式体验对齐。
- **可选 CLI flags**（`4635804`）。`config.MiniAgent` 增 `max_iterations`/`shell_timeout`/`confine`/
  `key_file`；`miniclient.buildArgs` 据此拼 `-max-iterations`/`-shell-timeout`/`-confine workdir`/`-key-file`。
  `key_file` 非空时改由文件注入 key 且不再经 `$MINIAGENT_API_KEY` env 注入（规避 `/proc/$PPID/environ`
  泄漏）。`max_iterations` 与既有 `Incomplete`（撞迭代上限）联动，让上限可配而非撞硬编码 20。
- **`-list-models` 委托 CLI**（`4635804`）。`miniclient` 增 `ListModels(ctx)` fork `miniagent -list-models` 读
  stdout；`miniagent/models.go` 删除自带 `GET /v1/models` 的 HTTP/解析/4 MiB 限流代码，改委托 CLI
  （v2.0.0 重新暴露 `-list-models`，回归 v1.1.0 前 behavior）。
- **`multi_edit` 工具分类**（`4635804`）。`renderer.toolCategory` 把 `multi_edit`（→`Multi_edit`）归入 edit 类，进度卡
  summary「编辑 N」段纳入；`grep`/`glob` 经 `isReadTool` 早被归入 read。
- **`/mode`、`/thinking`、`/new` 命令**（`a3cdfef`）。每 chat 钉住 `-mode`/`-thinking`（覆盖全局默认，
  经 `router.SetMode`/`SetThinking` 持久化）；`/new` 删除本 chat 的会话 jsonl，下次提问开新对话。`/help`
  与 `/current` 同步纳入新命令。`-mode default`（新默认）：写工具限 workdir + shell 拒 11 类提权器
  （sudo/doas/su/…）；`-mode auto`：放开。
- **每 chat 自动会话记忆**（`a3cdfef`）。`miniagent.Handler` 在 `{state_dir}/miniagent-sessions/<sha256(chatID)>.jsonl`
  落 v3 `-session`，第二轮记得第一轮内容；`/new` 清空。同 chat 串行由既有 `startTurn` busy-then-drop
  保证（busy 时第二个 prompt 直接被「处理中」拒），单进程下不依赖 v3 跨进程 `flock`。chatID 经 sha256 hex
  防路径穿越/碰撞。
- **可选 `config_path` 多 provider 模式**（`a3cdfef`）。`miniagent.config_path` 指向 `miniagent.json` 时进入
  config 模式：bridge 透传 `-config <abspath>`，**不传** `-chat-url`/`-models-url`；`/model provider/id`
  切换 provider。`miniagent.json` **部署期生成**（operator 按 `.env` 产出，bridge 不维护、不生成——R3）。
  示例骨架（`/etc/miniagent/miniagent.json`）：
  ```json
  {
    "providers": [
      {"name": "main", "chat_url": "${MINIAGENT_CHAT_URL}", "models_url": "${MINIAGENT_MODELS_URL}", "key": "${MINIAGENT_API_KEY}", "models": ["model-a", "model-b"]}
    ],
    "defaults": {"model": "main/model-a", "mode": "default", "thinking": "off"}
  }
  ```
- **`miniclient.DefaultMode`/`DefaultThinking` 访问器**（`a3cdfef`）。给 `miniagent.Handler` 读 client 默认值用
  （`activeMode`/`activeThinking` 的 fallback），避免改 `New(handler)` 签名。nil 安全（测试）。
- **启动期校验测试 + config 模式分支测试**（`a3cdfef`）。`cmd/miniagent-back/main_test.go` 钉死
  `workspace_root` 必配、bare 模式 `chat_url`/`config_path` 二选一、`config_path` 非空时放行 chat_url 缺位；
  `miniclient` 钉死 config 模式下 `-config`/`-mode`/`-thinking`/`-session` 共存（只换端点源，不瘦身每轮形状）。

### Changed

- **`base_url` → `chat_url` + `models_url`（破坏性）**（`a3cdfef`）。v3 删 `-base-url`，bridge 拆成两个完整 URL：
  `chat_url`（chat completions 全 URL，bare 模式必填）+ `models_url`（models 全 URL，`/models` 用）。
  旧 `miniagent.base_url` 字段经 `DisallowUnknownFields` 启动期明确拒绝。
- **`confine` 移除 → `mode`（破坏性）**（`a3cdfef`）。v2 `confine`（workdir 沙箱）合并进 v3 `-mode`：新默认 `default`
  = 写工具限 workdir + shell 拒提权器；`auto` = 不限。`miniagent.mode` 经 `applyDefaults` 默认 `default`、
  经 `validate` 限 `default`/`auto`。`/mode` 每 chat 覆盖。
- **`workspace_root` 改为必填**（`a3cdfef`）。v3 `-mode default` 需要 workdir；不配启动期 fatal（`/cd` picker 也依赖它）。
  `cmd/miniagent-back/main.go` 强校验。
- **`-thinking` 升为一等字段**（`a3cdfef`）。`miniagent.thinking`（off|minimal|low|medium|high|xhigh|max）经 `applyDefaults`
  默认 `off`、经 `validate` 限七值；`/thinking` 每 chat 覆盖。
- **`-list-models` 适配 v3**（`a3cdfef`）。bare 模式传 `-chat-url`+`-models-url`；config 模式只传 `-config`。
- **`env.example` 字段同步**（`f649c37`）。`MINIAGENT_BASE_URL` 废弃（保留仅兼容旧文档）；新增
  `MINIAGENT_CHAT_URL`、`MINIAGENT_MODELS_URL`；`MINIAGENT_DEFAULT_MODEL` 保留。

### Fixed

- **`backendrpc` SSE + POST 复用同一 token**（`935f8b0`）。修复每次 POST 重新创建 `BackendToken` 导致
  SSE 连接被抢占的问题；改为进程内单例 `BackendToken` 复用。

### Removed

- **`miniagent.base_url`**（`a3cdfef`）。旧配置启动期拒绝，operator 必须迁移到 `chat_url`+`models_url`。
- **`miniagent.confine`**（`a3cdfef`）。旧配置启动期拒绝；既有的 `confine: true` 等价于新默认 `mode: default`，
  `confine: false` 等价于 `mode: auto`。

### Notes

- **上游依赖**：miniagent 二进制建议升级到 **v3.0.0**（旧二进制不认 `-chat-url`/`-mode`/`-thinking`/
  `-session`/`-config`，exit 2）。详见 `docs/MINIAGENT_V3_MIGRATION.md`。lark-bridge 不打包 miniagent 二进制。
- **`-approve all` 必须保持默认**（`4635804`）：**bridge 每 prompt fork 一次、stdin 写完即 EOF，
  `dangerous`/`always` 会在 EOF 后拒绝危险工具**。严禁改为此值。
- **operator 迁移清单**（v3.0.0，见 `docs/MINIAGENT_V3_MIGRATION.md`）：
  1. 升级 miniagent 二进制到 **v3.0.0**。
  2. 配置文件 `miniagent.base_url` → `chat_url`+`models_url`（完整 URL）；删 `confine`，按需配 `mode`
     （默认 `default` 等价旧 `confine: true`）。
  3. 配 `miniagent.workspace_root`（v3 必填）。
  4. （可选）多 provider：配 `miniagent.config_path` 指向部署期生成的 `miniagent.json`。
- **行为变化警告**：operator 的 shell 工具若依赖 sudo/doas/su 等提权器，新默认 `-mode default` 下会被拒，
  需 `/mode auto`（每 chat）或全局配 `miniagent.mode: auto` 放开。这是有意的护栏。
- **`miniagent.json` 部署期生成**：bridge 不生成、不维护该文件（R3）。operator 用 `.env` 模板产出后填 `config_path`。
- **发版顺序**：先 feishu-front 后各 backend（deploy.sh 顺序天然满足）；miniagent 二进制独立安装。

---

## [1.9.0] - 2026-08-01

v1.8.0 之后的增量。主线：**miniagent v1.1.0 接入**（finish→Incomplete、shell 进度摘要）、
**全量代码审查修复**（2 高危 + 11 中危 + 低危）、**REMEDIATION_PLAN 架构加固**（runSession
死锁、per-backend router、app heartbeat、IPC TLS/mTLS）、**SSE handshake deadline 重连风暴
修复**、**idle_timeout 推荐值放宽 600s→1800s**、**systemd restart-burst 单元修正**。新增功能 →
按 semver 升 minor。无协议层 breaking change；`config.example.json` 的 `idle_timeout` 变更需
部署侧同步（见 Notes）。**发版顺序**：先 feishu-front 后各 backend（deploy.sh 顺序天然满足）。

### Added

- **miniagent v1.1.0 finish 透传**（`28f63ad`）。miniclient 解析 result 事件的 `finish`
  字段并暴露 `FinishStop`/`FinishMaxIterations` 常量；miniagent handler 在 `max_iterations`
  时设 `ResultPayload.Incomplete` 并为空回复填入提示文案。该字段自协议建立即存在却从未
  赋值——本版接线。`RenderResult` 对 `Incomplete` 渲染为 orange「未完成」卡，消除撞迭代
  上限时「空回复绿卡」的静默失败。
- **miniagent shell 工具计入进度摘要**（`28f63ad`）。`toolCategory` 把 `Shell`（miniagent 的
  `shell` 经 `normalizeToolName` 大写化）归入 exec 类，进度卡 summary「执行 N」段纳入
  miniagent；`read`/`write`/`edit` 经 normalize 早被正确分类。
- **IPC TLS/mTLS**（`0454f27`，M10-1）。新增配置 `ipc_tls_cert_file`/`key_file`/`client_ca_file`
  （frontend）与 `ipc_tls_ca_file`/`client_cert_file`/`client_key_file`（backend），启动校验
  配对与文件存在；非 loopback 绑定强制 TLS（双护栏：config 校验 + 运行时拒绝），可选 mTLS
  （`RequireAndVerifyClientCert`）。README 文档化。
- **per-backend session token**（`2641838`，M10-2）。POST `/v1/control`|`/v1/metrics` 绑定到
  注册该 backendID 的 SSE 连接，关闭共享 bearer 下的跨后端冒充。
- **WS dispatch worker pool**（`2641838`，M4）。完整帧后立即 ACK，dispatch 投递到有界 worker
  （满则 inline fallback）——慢文件消息不再队头阻塞读循环或推迟 ACK 至服务器重投。
- **picker per-chat single-flight**（`2641838`，M3）。`ErrPickerInFlight`，`/model` 刷屏不再堆
  9 分钟 goroutine；omp/opencode 的 `cachedList`（TTL + inflight 单飞）让 list fork 去重。
- **app-level heartbeat**（`d0bdb17`，C2）。`TypePing`→`TypePong` 在 dispatch loop，
  `missedPongs≥3` 驱逐——补 `lastSeen` 无法驱逐卡死 consumer 的漏洞（其自身 ping 的 flush
  会刷新 lastSeen）。
- **per-backend router 文件**（`d0bdb17`，R2）。claude/opencode/omp/miniagent 各自独立 router
  文件 + 旧 `router.v5.json` 迁移，结束多后端共写单文件的跨进程 lost-update。
- **streamarchive 单文件 100 MiB 上限**（`2641838`，M7）超限写截断 marker 行；**行级脱敏**
  （`0454f27`，低危#25）`RedactingWriter` 按行缓冲，多行/部分写入与数组套数组均覆盖。
- **eventmetrics UnknownEvent 键上限 256**（`2641838`，M8）超限落入共享 `__overflow__` 计数器。
- **mid-run notice 走 TypeProgress**（`d0bdb17`，S1）避免与终态去重冲突吞掉最终回复。
- **status report 异步化**（`d0bdb17`，C3）移出 control pump。

### Changed

- **`idle_timeout` 推荐值 `600s`→`1800s`**（`28f63ad`，`config.example.json`）。opencode / omp
  的无输出看门狗阈值放宽到 30 分钟，上游 LLM 长卡顿不再 10 分钟被误杀。**部署行为变化**：
  各环境实际 `config.json` 需手动同步（默认 `0`=禁用不变）。

### Fixed

- **H1 TypeFile 异步投递 OOM**（`2641838`）。`fileSendSem`/`statusSendSem` 前移到 spawn 之前
  获取（满则丢弃 + busy notice），排队 TypeFile（单条 ~40 MB）不再无界堆积到 OOM。
  涉及 `internal/feishufront/dispatcher_file_send.go`、`dispatcher_status.go`。
- **H2 WS 写路径无 deadline**（`2641838`）。`WriteMessage` 加 15s 写 deadline，半开连接不再
  持 `closeMu` 分钟级阻塞重连。涉及 `internal/lark/websocket/conn.go`。
- **R1 runSession sweep-goroutine 死锁**（`d0bdb17`）。退出前先 cancel 再等 `exitCh`。
- **SSE body 被 handshake deadline 提前关闭（重连风暴）**（`86318f7`）。`context.WithTimeout`
  统管响应 body 整个生命周期，handshake 的 `AfterFunc` 自动触发会把 body 撕裂致重连风暴；
  改 `WithCancel` + `AfterFunc`（header 到达即 stop），仅 `Close()` 结束流。
  涉及 `internal/backendrpc/client.go`；`handshakeTimeout` 改 var 以便测试缩小。
- **M1 stderr 截断后不 drain 双向死锁**（`2641838`）。三桥 + miniclient 在 64 KiB 上限后继续
  `io.Copy(io.Discard, stderr)`，子进程不再因满管道与父进程双向死锁（默认 prompt_timeout=0、
  idle_timeout=0 无兜底）。涉及 `claude/stream.go`、`opencode/stream.go`、`omp/stream.go`、
  `miniclient/client.go`。
- **M2 文件转换全量内存缓冲**（`2641838`）。docx/xlsx/pptx 按 section/sheet/slide 流式 flush，
  不再整文档驻留 `bytes.Buffer`。涉及 `internal/fileconvert/`。
- **M5 WS 升级握手读无 deadline**（`2641838`）。无条件 30s 读 deadline，`Start` 不再永久挂起。
  涉及 `internal/lark/websocket/dial.go`。
- **M6 pong 重连参数无下限**（`2641838`）。字段级合并（零值保留旧）+ 5s 下限钳制，部分 pong
  不再清零重连预算或触发热循环。涉及 `internal/lark/ws/wsclient.go`。
- **M9 opencode/omp prompt 走 argv**（`2641838`）。>100 KiB 提前报错，避免 `MAX_ARG_STRLEN`
  硬失败与 `/proc` 明文暴露。涉及 `internal/opencode/client.go`、`internal/omp/client.go`。
- **M11 离线通知 timer Reset 未 bump generation**（`2641838`）。重武装时 `generation++` +
  `AfterFunc` 重建，不再重复发「后端离线」卡。涉及 `dispatcher_backend.go`。
- **C1 readSSE stalled-consumer 超时强制重连**、**C13 bounded SSE handshake ctx（Close 取消）**
  （`d0bdb17`）。**W1** `ErrReconnectBudgetExhausted` 替代静默 nil + `fireReconnected`（`d0bdb17`）。
- **S2 auto-retry-limit 单终态文案**、**S3 scaffold 在 EmitTerminal 前 cancel**（`d0bdb17`）。
- **P2 ApplyGroupCancel ESRCH probe**、**P3 RunGC group-kill**、**P4 StderrPipe 失败关 stdout-pipe（×4 桥）**（`d0bdb17`）。
- **systemd restart-burst 单元修正**（`446e82f`）。`StartLimitIntervalSec`/`Burst` 移到 `[Unit]`——
  systemd <230 在 `[Service]` 静默忽略它们，使重启突发限流失效。涉及 `deploy/`。
- **低危（节选）**（`2641838`/`0454f27`/`d0bdb17`）：inbox 每日清扫 ticker（#1）；flap/statusCards
  上限与回收（#2）；复用已连 SSE client 消除双重连接 + 修复 batch1 的 stale token 导致 POST
  被判冒充的回归（#9）；`AckRegistry.Close` 以 `ErrAckRegistryClosed` 唤醒、不再当已送达（#10）；
  `StartPrompt` 的 `Wg.Add` 与 `Close` 同在 `CancelMu` 下，消除 WaitGroup 误用竞态（#11）；
  miniagent-back 用 `os.Executable`（#15）；`/cd` 用 `EvalSymlinks` containment（#17）；
  card-action 日志截断脱敏（#18）；config 文件权限宽于 0600 且含明文密钥时告警（#20）；
  opencode sessionID 拒前导 dash（#22）；权限/问答卡明示群成员可批准（#23）；atomicwrite 注释
  修正（#24）；README 补日志轮转 / 环境密钥命名陷阱说明（#7/#21）。#3（debug 日志，部署侧确认）、
  #12（Close 最多泄漏 1 goroutine）按设计接受。

### Notes

- **上游依赖**：miniagent 二进制建议升级到 `v1.1.0`——`finish` 透传在旧版退化为空值
  （`Incomplete` 恒 false，不报错但无新效果）。lark-bridge 不打包 miniagent 二进制。
- **idle_timeout 部署同步**：`config.example.json` 已改 `1800s`，但各环境实际 `config.json`
  不自动跟随，部署侧重办时需手动同步（opencode / omp 无输出终止阈值由此改变）。
- **发版顺序**：先 feishu-front 后各 backend；新协议字段 `ResultPayload.Incomplete` 对旧后端
  向后兼容（未知字段忽略）。
- 本版完成全量代码审查 P0–P3 修复（2 高危 + 11 中危 + 低危 23/25；余 #3/#12 按设计接受）。

## [1.8.0] - 2026-07-31

v1.7.0 之后的增量。主线是**会话管理命令对齐**（claude `/use`/`/clean`/`/session`、
omp `/session`/`/use`/`/clean`/`/session-gc`）、**部署加固**（`LARK_RUN_MODE` 双模式）、**事件流健壮性**（超长行截断、未知事件计数、流归档字段脱敏），以及修复
**inflight 会话状态不一致导致的部署死锁**。无协议层 breaking change，新增功能 → 按 semver 升 minor。
**发版顺序**：先 feishu-front 后各 backend（deploy.sh 顺序天然满足）。

### Added

- **claude 会话管理命令对齐 omp/opencode**（`6a15532`）。此前 claude 的 `/session` 只读
  本地 chat→session 绑定表（注释自承 "The Claude backend has no central session registry"），
  且缺 `/use`/`/clean`。现按 claude 的 `~/.claude/projects/<编码cwd>/` 落盘布局
  实现文件系统会话驱动，三命令全部对齐：
  - 新增 `internal/claude/sessions.go`：`encodeProjectDir`（cwd 绝对路径每个 `/`→`-`，含前导）、
    `ListSessions`（枚举目录下 `*.jsonl`，按 mtime 倒序）、`DeleteSession`（删 `.jsonl` + 同名
    子目录，保留项目级共享 `memory/`）。编码规则、resume 的 cwd-bound 语义、删除等同 `--resume`
    失效（命中 `IsStaleSession`）均经实测确认。
  - `/session` 改枚举当前绑定目录下真实会话，当前绑定会话标 `★`（不再读 `router.AllBindings()`）。
  - `/use` 新增：同目录会话切换，无参弹选择卡、带参支持序号/id；切到当前会话 no-op。
  - `/clean` 新增：无参删当前目录除当前会话外的全部，带参仅删指定 id；走 `AskPermission`
    确认卡 + 原地 PATCH 刷新；保护当前绑定会话不可删。
  - `claudeAPI` 接口 +2 方法（`ListSessions`/`DeleteSession`），所有 fake 补实现；表驱动测试
    覆盖无绑定/无目录/保护当前会话/确认删除/取消五条路径。

- **omp 会话管理命令补齐**（`49c3d5b`）。omp 的 session store 是 cwd-bound 且慢，此前仅部分
  命令可达。现 `/session`/`/use`/`/clean`/`/session-gc` 全部走磁盘 session
  store；新增配置 `omp.agent_dir`、`omp.gc_cold_archive_after_days`/`gc_retain_newest_per_cwd`/
  `gc_timeout`；terminal-event 检测加固。

- **`LARK_RUN_MODE` 双模式门 + CLI 健康探测按可执行位判定**（`316f6be` + `d8a586d`）。`LARK_RUN_MODE=pro`
  控制 deploy 流程行为；CLI 健康探测去掉 `--version`、改为按可执行位判定（适配不实现该 flag
  的 CLI），过滤后无存活服务时 fail-fast。

- **streamarchive 字段级脱敏 + miniagent 流归档**（`662e7d2`，F7/F9）。stream 归档新增 opt-in
  字段级 redaction；miniagent 对话输出持久化到 `{state_dir}/streams/miniagent/`（与
  claude/opencode/omp 字节级同构）。

- **eventmetrics 进程内事件流计数器**（`6bb4cd9`）。为事件流可观测性新增 in-process counters
  （lenient-parse 命中、未知事件类型、超长行截断等），供排障。

- **status-monitor 主机去重 + NAT 消歧**（`a017fae`）。总览卡主机行按 machine-id 去重，NAT
  环境下显示 IP 消歧。

- **omp 流事件处理补齐**（`6ba58fd`）。omp 桥对 OMP CLI 流的事件映射全面补齐，与
  claude/opencode 对齐：`notice` / `custom-nudge`（mid-run-todo-nudge）→ `TypeNotice`；
  `todowrite` 完成事件改写为 `TypeTodo`（todo 区，不再叠加同 call 的 `TypeToolResult`）；
  `task` 工具按 `subagent_type` 归入子代理专区（`SubagentSummary`）；`turn_end` 携带的完整
  assistant message 作为兜底回复（流式路径无文本时）；`session` 头的 title/cwd 解析；
  assistant `message_end` 的错误码以 `[status/id]` 拼入错误文案；`tool_execution_update`
  按 fileCount 汇成 `TypeProgress` 描述（长时 glob/bash 不再静默）。
  - `thinking_level_changed` 仅记 debug 日志，**不**发独立控制——该事件在流开头早于终态
    `TypeResult`，发 `TypeNotice` 会与终态去重冲突并吞掉最终回复（见 Fixed 段 B2）。
- **miniagent 终态投递重试对齐**（`6369348`）。`bridgebase.EmitTerminalControl` 提为包级函数
  （基于新 `backendrpc.ControlSender` 接口），让无 `Core` 的后端共享 `c7d67ed` 的终态重试+ACK
  循环。miniagent 的最终 Result/Error 现走该路径（`nil` acks = 纯发送失败重试，`appCtx` 在
  `Close` 时取消退避）——丢失的 miniagent 最终回复被重发而非静默吞，与三个 CLI 后端行为对齐。

### Changed

- **超长流行截断而非中止 turn**（`aec61ba`，F1）。此前单个超过 `maxLineLen` 的行会以
  `bufio.ErrTooLong` 终止整轮 turn；现改为截断该行（保留前缀 + `...`）并继续，配合计数器可观测。

- **claude lenient-parse 命中与未知事件计数**（`b10f830`，F2/F8）。lenient 解析路径与未知事件
  类型现经 eventmetrics 计数，不再静默。

- **omp `text_end` 兜底 + `auto_retry` turn 限制**（`56fe984`，F3/F4）。无 delta 的轮次用
  `text_end` 兜底；`auto_retry` 加 turn 上限防无限重试。

- **strutil 统一 stringify 跨后端**（`3f3a468`，F5/F6）。`stringifyContent`/`stringifyJSON`
  收敛到 `internal/strutil`，opencode failure-signal 识别随之定型。

### Fixed

- **inflight 会话状态不一致导致部署死锁（P0）**。前端 `TurnManager`（乐观事件簿记）与后端
  `CancelByChat`（goroutine 存活）无同步机制：后端被 `SIGKILL`/OOM/重启时，正在跑的 turn 连同
  终态事件一起消失，前端那条 turn 永不 `Finish`，`/v1/deploy-preflight` 永久返回 409，
  `deploy.sh` 永久拒绝部署——运维死锁，此前只能手动重启 feishu-front 或 `--force` 绕过。
  现状：`fireOfflineNotice`（后端离线超过 `offlineNoticeDebounce` 窗口，已排除短暂抖动）触发
  `reclaimStrandedTurns`——`TurnManager.ReclaimBackend` 释放该后端的全部 stranded turn，并把
  每张进度卡原地 PATCH 为「会话已失效」错误卡（进度卡被撤回时回退独立 notice），同步释放
  progress 状态与关联的交互卡绑定。`/v1/deploy-preflight` 在 TTL 内自动收敛恢复 200。
  - `internal/feishufront/turn.go`：`ReclaimBackend(backendID) []Turn`；`TURNSBYBACKEND` 注释刷新。
  - `internal/feishufront/dispatcher_backend.go`：`fireOfflineNotice` 接 `reclaimStrandedTurns`；
    新增 `reclaimStrandedTurns`/`invalidateTurnCard`；离线卡文案改为「进行中任务已被自动结束」。
  - 既有「turn 只在 `/session-abort` 结束」政策保留于 `OnBackendOffline` 的 arm 阶段（短暂抖动
    不回收），仅当离线持续过 debounce 窗口才回收——无在线判定，无误杀长 LLM turn 风险。
  - 测试：`TestReclaimBackend`、`TestFireOfflineNotice_ReclaimsStrandedTurns`、
    `TestFireOfflineNotice_BlipKeepsTurns`、`TestInvalidateTurnCard_WithdrawnProgressCardFallsBackToSend`；
    既有 `TestOnBackendOffline_KeepsInFlightTurn`（短暂离线保留）仍绿。

- **终态控制投递 ACK + 重试，杜绝最终回复静默丢失**（`c7d67ed`）。3 个 CLI 后端
  （claude/omp/opencode）的终态控制（Result/Error/Notice）此前是"单次即丢"：一次 HTTP POST
  返回 202 即视为成功，但前端可能在渲染前崩溃/重启，导致最终回复卡永久丢失且无任何告警。现
  `Core.EmitTerminal` 改为 4 次重试 + ACK 确认（指数退避 1/2/4s）：前端 SSE 在收到终态控制时回
  一个 `TypeAck` Event（即便渲染失败或命中去重重复也回），后端 `AckRegistry` 据此停止重发；
  4 次全失败时发兜底 notice 并 bump `TerminalEmitLost` 指标——丢失不再静默。新增 `protocol.TypeAck`
  事件类型（`allowedEventTypes` + 表驱动校验）与 `bridgebase.AckRegistry`（PromptID 配对的一次性
  wait）；前端 `dispatcher_control.go` 对每个终态控制回 ACK。
  - **B2 连带修复**：`thinking_level_changed` 在 `6ba58fd` 中被映射为 `TypeNotice`，因其早于
    最终 `TypeResult` 到达前端而命中 `terminals` 终态去重，系统性吞掉 OMP 后端（thinking=auto）
    的最终回复卡；现改为仅 debug 日志，不再发独立终态控制。

- **提交后翻灰 + 延迟兜底 PATCH**（`3846b89`）。问答/权限卡提交后按钮置灰、显示「已提交/✓ …」；
  追加延迟兜底 PATCH 绕过飞书点击处理窗口（~3-5s）的静默回退。`0eebfae` 修复 notice-patch 时
  未释放 interactive binding 致延迟兜底 PATCH 误命中的连带 bug（`/clean` 悬卡）。

- **status-monitor `/status` 按需刷新**（`3846b89`）。`buildStatusReport` 抽取共用，`/status`/
  `/refresh` 立即推一张总览卡（不等 `interval`）；`/running`/`/help` 分派补齐。

- **主机按 `ReportedAt` 去重 + ws 帧合并 flake**（`3846b89`）。总览卡主机行按上报时间去重；
  修 `wsclient_test` 帧合并导致的偶发 flake。
- **长时 job goroutine panic 恢复**（`b0e4a8c`）。git / singleflight / deploy job 内的 panic
  此前会崩溃整个后端进程（连带所有在途 turn）。现经 `bridgebase.GoSafe` 运行：延迟的槽位释放
  （`mu.Unlock` / `jobWg.Done`）在 `recover` 前的 panic unwind 中仍执行，无槽位泄漏，`Close`
  不会卡在死 job 上。涉及 `internal/bridgebase/git_runner.go`、`singleflight_job.go`。
- **config.example.json 启动即崩**（`73bf2e8`）。示例配置的 `timeouts.prompt_timeout: "0s"` 在
  `config.Load` 解析阶段被拒，导致每个 deploy 命令启动即失败。删除该显式字段（由 `applyDefaults`
  默认值生效）；新增 `TestLoadConfigExample` 钉住「示例配置永远可加载」，防回归。

### Notes

- `docs/` 目录重新加入 `.gitignore`（设计/评估文档本地化，不入仓）（`5caff43`/`076570d`）。
- `golangci-lint run ./...` 恢复 0 issues（修 23 处 v1.7.0 后回归：errorlint×12 改 `errors.Is`、
  goimports×4、nilerr×3 加 nolint 注释、errcheck×2、staticcheck×1、unused×1）。
- Makefile 新增 `lint` 与 `prerelease` 目标——后者把 `go test ./...` + `golangci-lint run ./...`
  作为打 tag 前闸门（`1d8f9c9`）。
- 测试覆盖补充（`c3c63b5`）：health-tick 陈旧后端驱逐、omp stale-session 重试编排、omp
  idle-vs-cancel 终态分流。

## [1.7.0] - 2026-07-30

v1.6.0 之后的增量。主线是**新增 omp-back（Oh My Pi CLI）agent 后端**——一个新的
业务子系统 + 一次把四套 CLI 后端的共享代码下沉的重构。无 breaking change，新增功能 →
按 semver 升 minor。**发版顺序**：先 feishu-front 后各 backend（deploy.sh 顺序天然满足）。
**升级既有部署**：`make deploy` 不需 `--init`；新增 omp-back 见 `deploy/README.md` 的 omp 段。

### Added

- **omp-back：Oh My Pi (omp) CLI agent 后端**（`3f07e05` / `dca1719` / `7b65459`）。第 7 个
  二进制 `lark-omp-back`，第 5 个业务后端。每个 prompt fork 一次 `omp -p --mode json`
  子进程，消费其 NDJSON 事件流。对接规范见 `OMP_INTEGRATION_SPEC.md`。
  - 新增 `internal/omp`（CLI 子进程驱动 + models 列表缓存）、`internal/ompbridge`
    （业务逻辑）。斜杠命令对齐 claude/opencode：`/running` `/new` `/session-abort`
    `/session-del` `/current` `/model` `/perm` `/thinking` `/cd` `/send` `/pull` `/push` `/help`。
  - 配置段 `omp.*`：`cli_path` / `default_directory` / `max_concurrent` / `stream_history` /
    `append_system_prompt` / `approval_mode`（默认 `write`）/ `thinking_level`（默认 `auto`）/
    `approval_options` / `thinking_options` / `model_options`。
  - 安全对齐既有后端：`cmdutil.SanitizeChildEnv()`（剥离桥接自身 secret）+
    `cmdutil.ApplyGroupCancel()`（进程组 + ctx 取消 + WaitDeadline）。
  - **设计性缺失**：不支持 `/session` / `/use`——omp 的 session store 是
    cwd-bound 且慢（见 `internal/ompbridge/deps.go` 注释），非缺陷。
- **omp-back 动态 `/model` picker**（`39c849d`）：picker 选项改为 fork `omp models --json`
  取真实 provider/id selector 列表（冷启动 ~100-150s，故带 TTL 缓存）；fetch 失败时回退
  静态 `model_options`。新增配置 `omp.model_list_timeout`（默认 `300s`）与
  `omp.list_cache_ttl`（默认 `3600`，负值禁用缓存）。
- **deploy 流程纳入 omp-back**（`146db59`）：`deploy/deploy.sh` 现管理 5 个业务服务
  （feishu / claude / opencode / omp / miniagent）；`make deploy` / `make pack` / Makefile
  产物校验同步覆盖第 7 个二进制。
- **`cmd/omp-back/main_test.go`**：补齐 `TestCLIRunner_BadConfigReturnsError`，使 7 个
  cmd 入口对「坏 config fail-fast」契约的覆盖一致（修复 ARCHITECTURE.md「每个 cmd 含
  `main_test.go`」的最后一处遗漏）。

### Changed

- **后端共享代码下沉重构**（`5e36e05`）。
  行为保持（behaviour-preserving），不改变对外协议。四套 CLI 后端（claude / opencode / omp /
  miniagent）的重复 prologue / helper / emit-forwarder 收敛到共享包：
  - `internal/bridgebase`：新增共享工具（`NonEmpty`/`TruncateForDebug`/`ResolveModel` 等）、
    `ValidateAbsDir`/`CreateSessionDir`、`PromptResult`+`RecordUsage`+`EmitTerminal`、
    `RunPromptScaffold`+`RecoverPromptPanic`（每 prompt 的 ctx/超时/看门狗装配）、
    `MakeEnumPicker`（合并 claude `/effort` 与 omp `/thinking`）、
    `SingleFlightJobRunner` / `PeriodicReporter` 抽象。
  - `internal/clibase`：共享 `CheckVersion` + 常量（3 份字节级相同的 ready.go 合一）。
  - `internal/backendhost`：`CLIRunner[H]` 统一 claude/opencode/omp 的 main 生命周期
    （config → router → usage → backendrpc → metrics → Run）。
  - `internal/log`：`BaseLogger` + `ComponentLogger` 取代每二进制各自的 buildBaseLogger。
  - 回归保护：每个 CLI 后端新增 `testdata/*.jsonl` + replay 测试，把抓取的真实 NDJSON 流
    灌过 `ParseEvent`+`streamRun` 断言 reply/usage/stale-shape（含 omp 三个经验性 bug 的
    覆盖：turn_start reset、跨 message_end 的 usage 累积、未知事件前向兼容）。
  - 净变化：约 +3290/−2630，每个后端减 80-120 行重复 prologue。
- **配置默认值刷新**：`omp.model_list_timeout` / `omp.list_cache_ttl` 进入 deploy 配置表
  与 `config.example.json`。

### Fixed

- **feishufront：confirm/cancel 卡片选项本地化 + 补 submitSummary 测试**（`39e3239`）。
  opencode `/clean` 的确认卡片用 `Value:"confirm"/"cancel"`，提交回显经
  `choiceLabel` 命中 default 分支，显示英文原文而非中文。`choiceLabel` 补
  `confirm→确认`/`cancel→取消` 映射；`submitSummary`/`questionAnswerSummary`/`choiceLabel`/
  `parseQuestionFormValue` 四个纯函数补单测覆盖。
- **backends：cache 命中路径防御性拷贝 + 清 omp lint 回归**（`cb88f31`）。omp + opencode 的
  `cachedList` 命中路径此前直接返回缓存内部切片，调用者改写会污染 TTL 窗口内的缓存；
  改为返回拷贝，与未命中路径的「防御性拷贝」契约一致。同时修复 omp `client_list_test.go`
  的 `ineffassign` + `intrange`（恢复 `golangci-lint run ./...` 0 issues）。

### Notes

- 内部：`.codegraph/` 索引目录加入 `.gitignore`（`8eef512`）。
- 文档：`ARCHITECTURE.md` 统计刷新（7 二进制 / 28 内部包 / 行数）；`Makefile` deploy 注释
  更正为 5 个业务服务；`deploy/README.md` 配置表补 `omp.model_list_timeout` /
  `omp.list_cache_ttl`。

## [1.6.0] - 2026-07-29

v1.5.0 之后的增量。含 2 处 breaking change（fileconvert 输出语义、配置字段
`pandoc_path` 删除）+ 多个 feat，按 semver 升 minor。**发版顺序**：先 feishu-front
后各 backend（deploy.sh 顺序天然满足）。升级前请从配置中移除 `pandoc_path`。

> 修正（2026-07-30，随 v1.7.0）：本节早前曾误述 xlsx 「首次引入第三方依赖
> `xuri/excelize/v2`、打破零依赖硬约束」。实际发布的 v1.6.0 xlsx/pptx 均为纯 Go
> 标准库自研（`go.mod` 无 `require`、无 `go.sum`），零依赖硬约束从未被打破。
> 相关条目已据实更正。

### Added

- **pptx / xlsx 文件上传 → markdown 管线**：feishu-front 现可接收群聊上传的
  `.pptx` / `.xlsx` 文件，转成 GitHub-flavoured Markdown 落到 inbox，agent
  走与 docx 完全相同的 Read 路径。
  - pptx 走纯 Go 标准库自研（`archive/zip` + `encoding/xml`），L1 档位：
    全页提标题 / 正文 / 项目列表 / 简单表格；图表与 SmartArt 输出 HTML 注释
    占位（决策 9A）；图片完全忽略（决策 3A）。幻灯片顺序按
    `presentation.xml` 的 `sldIdLst`，不按文件名。
  - xlsx 走纯 Go 标准库自研（`archive/zip` + `encoding/xml`，零第三方依赖）：
    数据本体全量 GFM 表格写盘（含 chart/pivot 占位、合并单元格只填左上角、
    公式提缓存值），递交 agent 的 prompt 只含路径 + 每 sheet 列名 + 每 sheet
    总行数（决策 Q11），由 agent 用 Read 工具（支持 offset/limit）按需读取区间。
  - 新增 `internal/fileconvert/convert_pptx.go`、`convert_xlsx.go`、
    `convert_xlsx_scan.go`（chart/pivot 的 OOXML 关系链 zip 扫描）、
    `gfm.go`（GFM 表格渲染 + 宽表 >20 列降级为 fenced CSV）；新增
    `internal/strutil/gfm.go` 单元格消毒（`|` / 换行 / 空白）。
  - `dispatcher_file.go`：上传白名单新增 `.pptx` / `.xlsx` 与错误文案；
    xlsx 走专用 `ConvertXlsx`（返回 `XlsxMeta`）+ 可选 `xlsx_prompt_template`；
    dispatcher 新增 `SetXlsxPromptTemplate` 独立 setter（不改 `SetFilePipeline`
    签名，现有调用点零改动）。
  - 配置项 `file_convert` 段新增 `pptx_max_slides` / `pptx_extract_notes` /
    `pptx_text_only` / `xlsx_max_sheets` / `xlsx_formula_mode` /
    `xlsx_prompt_template`，`xlsx_formula_mode` 强制 `value|formula|both`
    枚举校验。
  - **零第三方依赖硬约束保持不变**：xlsx 与 pptx 均为纯 Go 标准库自研解析
    （`go.mod` 无 `require`、无 `go.sum`），与 `CODING_STANDARDS.md` 的
    「直接依赖仅 Go 标准库」一致。

- **`/send` 指令：从绑定工作目录发送文件到飞书群**：三个业务后端
  （claude-back / opencode-back / miniagent-back）现支持 `/send` 与
  `/send <relative-path>`。后端读文件并 emit `TypeFile` Control，**前端**完成
  飞书上传 + 发送（凭证与网络出口集中在前端，后端不持飞书凭证）；**不经过
  agent LLM**。
  - 协议新增 `TypeFile` Control + `FilePayload`（base64 内容，30 MiB 上限），
    validate 要求 payload + chatID。
  - `internal/lark`：新增 `Client.UploadFile`（multipart POST `/im/v1/files`）；
    `SendInput.FileKey` 支持 `msg_type=file`。
  - `internal/feishu`：新增 `Bot.SendFile`（upload + send 两段式）；
    `feishuClient` 接口加 `UploadFile`。
  - `internal/bridgebase`：`commands_send.go` 提供 `CmdSend`（Core 版，claude/
    opencode 共用）+ 导出纯函数 `SafeJoin` / `BuildSendOptions` /
    `ParseSendOption` / `ReadFilePayload`（miniagent 复用，因它无 Core 且命令
    系统独立）。目录浏览器复用 `AskAndWait` 多轮导航，隐藏 dotfile，>100 项截断。
  - 前端 `dispatcher`：新增 `FileSender` 接口 + `SetFileSender` +
    `handleFileControl`（成功 PATCH 选择卡 / 失败独立 notice）；`DispatchControl`
    加 `TypeFile` 分支。
  - 安全：后端 `SafeJoin` 强制目标在 `Binding.Directory` 内（Abs/Clean +
    EvalSymlinks + Rel 越界检查），单文件 30 MiB 上限。

- **`/send` 目录浏览器原地 PATCH 选择卡**：跳转目录时不再每层堆一张 standalone
  picker 卡，改为从第 2 轮起对同一张卡原地 PATCH。根因：首轮 `AskAndWait` 的
  takeover 调用了 `turns.Finish`，第 2 轮+ takeover 失败回退 `SendCard`。
  - 协议新增 `QuestionPayload.UpdateMessageID`（omitempty，前端老版本忽略）：
    指示前端对既有卡做一次延迟 PATCH（过飞书 ~3-5s 点击处理窗口）。
  - `bridgebase.AskCardUpdate`（+ `Core.AskCardUpdate` 接收端形式）：从第 2 轮
    起刷新上一轮 picker 卡；miniagent 镜像同款流程（无 `Core`，独立 `askCardUpdate`）。
  - 前端 `sendInteractiveCard` 走延迟 PATCH；`sendInteractive` 驱逐上一轮
    `requestID` 绑定，新轮独占卡片，无 cache/timer 泄漏（`TurnManager.RequestIDsByMessageID`）。

### Notes

- /send：miniagent 因无 `bridgebase.Core` 且命令系统独立（map + 不同 emit/
  answer 机制），不复用 `CmdSend`，而是复用导出的纯函数 + 自有的 `askAndWait`/
  `sendCtrl`（与 `/cd`、`/model` 同模式）。`/send` 的两段浏览器/直发均跑在
  `context.Background` 的 goroutine 中（用户点击/大文件读取可能远超命令 15s 超时）。
- `nilerr` 约束：`handleFileControl` 把 decode/send 错误处理放进无返回值的
  `deliverFile`，避免 `if err != nil { return nil }` 模式触发 nilerr——失败已作为
  notice 反馈，返回 nil 表示 TypeFile 已"handled"。

- 集成规格文档：新增 `CLAUDE_INTEGRATION_SPEC.md` / `OPENCODE_INTEGRATION_SPEC.md`
  （仓库根），描述外部 agent CLI 接入 lark-bridge 桥层的协议契约、事件流、斜杠
  命令对齐要求，供后续桥接新 agent（或回归对比）参考。纯文档，无代码影响。
  本批同时刷新了 `ARCHITECTURE.md` / `CODING_STANDARDS.md` 的包/接口统计数字。

- chart 检测：纯 Go 实现下，chart 读取通过对 `xl/` 关系链做 `archive/zip`
  扫描（workbook→worksheet→drawing→chart）按 sheet 计数；pivot 仅做 workbook
  级检测（pivotTable 的 `<location>` 不含 sheet 名，精确 sheet 关联在 zip 层面
  成本过高），二者均保留 HTML 注释占位以遵守"不静默跳过"契约。
- value 模式（默认）不对每个 cell 反查公式，避免大表 O(cells) 调用拖垮
  性能；公式文本/聚合注释仅在 `formula` / `both` 模式触发。

### Changed

- **docx 转换去 pandoc 化（breaking）**：docx → GFM 改为纯 Go 标准库进程内
  解析（`archive/zip` + `encoding/xml`），不再调用外部 pandoc 子进程。
  该方案推翻了 2026-07-28 的「不替换 pandoc」结论。
  - 新增 `internal/fileconvert/convert_docx*.go`：`styles.xml`（标题 /
    样式列表）与 `numbering.xml`（多级编号）两遍预解析 + `document.xml`
    流式提取；支持标题 H1-H9（outlineLvl 优先、name 回退）、加粗 / 斜体 /
    删除线 / 行内代码（等宽字体白名单 + code 样式启发式）、多级列表
    （编号重启语义）、GFM 表格（宽表降 fenced CSV）、超链接
    `[text](url)`、文末脚注；图片 / 图表 / SmartArt / OLE / 文本框 /
    嵌套表格按决策 9A 输出 HTML 注释占位，不静默跳过。
  - **配置 breaking**：`file_convert.pandoc_path` 字段删除。配置解析开启
    `DisallowUnknownFields`，老配置携带该键会启动报错——升级前请从配置
    中移除 `pandoc_path`。`convert_timeout` 语义变为单次转换的 ctx 预算
    （纯 Go 解析，不再有子进程 SIGKILL）。
  - 部署侧移除 pandoc 依赖：`deploy.sh` 软预检、feishu-front 启动
    `exec.LookPath` 硬预检、`deploy/README.md` 安装章节全部删除；交付
    恢复单静态二进制，无外部运行时。
  - 降级面（vs pandoc）：非 decimal 编号（letter/roman/中文）统一按数字
    渲染、合并单元格只留左上值、嵌套表格平铺、页眉页脚 / 批注 / 隐藏
    文本不提取、编号模板 `lvlText` 不还原多级串。
  - 测试 fixture 全部程序化生成（zip writer 内联 OOXML），CI 不再依赖
    pandoc / MS Word；预取消 ctx 的确定性预算中止测试替代原子进程超时
    测试。

- **xlsx 转换去 excelize 化（输出 breaking）**：xlsx → GFM 改为纯 Go 标准库
  进程内解析（`archive/zip` + `encoding/xml`），`go.mod` 恢复零第三方依赖
  （excelize 及其 8 个间接依赖全部移除，此前的依赖豁免随之撤销）。
  - 新增 `convert_xlsx_sst.go`（sharedStrings 含富文本拼接、注音跳过）、
    `convert_xlsx_style.go`（numFmt L1：内建日期 ID 查表 + 保守自定义
    模式识别 + 1900/1904 双纪元序列值转换）、`convert_xlsx_sheet.go`
    （worksheet 流式解析，公式文本零成本提取）。`ConvertXlsx` / `XlsxMeta`
    对外 API 不变，dispatcher 零改动。
  - **输出 breaking**：日期/时间统一输出 ISO 8601（`yyyy年m月d日` →
    `2026-07-01`）；数字不再应用显示格式（`15%` → `0.15`、`1,200` →
    `1200`）；无法识别的自定义日期模式输出原始序列值 + 聚合占位注释。
  - 附带收益：`formula`/`both` 模式消除 O(cells) 公式反查；每 sheet
    元数据派生复用解析结果（excelize 时代每 sheet 读两次），大表 IO
    减半；shared 公式从属单元格与无缓存值公式显式占位（原先依赖
    excelize 隐式行为）。
  - 测试 fixture 重写为内联 XML（与 docx/pptx 同模式），CI 无任何第三方
    依赖。

### Fixed

- **`/deploy-some` picker 卡与后端超时不一致（静默吞提交）**：多选卡文案承诺
  10 分钟失效，后端 `confirmTimeout` 只等 5 分钟，5–10 分钟窗口内的提交因
  `AnswerBroker` 槽已取消而被静默丢弃。`confirmTimeout` 改为单源派生
  `cardkit.InteractiveTimeout + time.Minute`（leaf pkg 引入无环依赖，杜绝再次
  漂移）。`/deploy-force`
  共用 `confirmTimeout`，同步受益。
  - 附带 P2：`acquireAndRun` 不再吃 caller 的 picker-wait ctx，notify/notifyProgress
    自派 `deployNoticeTimeout`（10s），避免 deadline 临近时 banner POST 因 ctx
    超时失败导致任务不启动；banner POST 失败时回滚 `h.running`（否则 runJob 不
    启动、其清理 defer 不跑，单飞会永久卡死）。
  - 附带 P3：busy 拒绝返回 nil，busy-notice 失败 best-effort 记日志，不再被
    调用方误标为「部署失败：启动部署失败」。

- stale-session 检测改为经 `claude.IsStaleSession` 单点判定：
  `finalizeResult` 在 error result 上置 `promptResult.stale`，
  `handlePrompt` 不再手撸 `strings.Contains(err, "No conversation found")`
  ——字符串匹配集中在 `internal/claude` 内，CLI 改文案时单点修复。

### Removed

- 死代码清理（全仓 deadcode + 交叉核对，2026-07-29 仓库审计）：
  - `bridgebase.WithReplyToID`（零调用，`Dispatch` 直接 `context.WithValue`）
  - `bridgebase/throttle.go` 整文件（`ControlThrottle`/`TextThrottle`，
    生产无调用方，连同其测试）
  - `claudebridge.validateSettingsPath`（/settings 禁自定义路径后失去
    调用点，连同其测试）
  - `lark/ws.appendHeader`（仅测试使用，移入 `frame_test.go`）
  - 空 `go.sum`（模块零第三方依赖）
- 文件名统一为 snake_case：`gitjob.go`→`git_runner.go`、
  `dircache.go`→`dir_cache.go`（测试文件同步），与 CODING_STANDARDS
  §3.2 及兄弟包 `dir_cache.go` 对齐。

## [1.5.0] - 2026-07-28

v1.4.0 之后的增量完善。无协议层破坏性改动：`PromptPayload.CardMessageID` 为
omitempty 新字段（非 frontend-override），`/v1/metrics/<id>` 与
`/v1/deploy-preflight` 对旧组件静默降级。**发版顺序**：先 feishu-front 后
各 backend（deploy.sh 顺序天然满足）。

### Added

- **总览卡主机/进程监控（多机部署）**：status-monitor 总览卡新增「主机」
  （load / 内存 / state_dir 磁盘占用）与「进程」（各 backend 版本号 +
  systemd cgroup 内存）两个 section。采用 push-via-frontend 方案（D1/D2 已定）。
  - 新增 `internal/hostmetrics` 包：纯函数采集 `/proc/loadavg`、
    `/proc/meminfo`、`statfs`（state_dir 挂载点，失败 fallback `/`）、
    cgroup v2 `memory.current`（读不到 → 卡片显示 `—`），6 个 binary 共用；
    `OutboundIP` 经 UDP 路由查询探测 frontend 实际看到的本机 IP。
  - 协议扩展（全部 `omitempty`，新旧任意组合零影响）：`HostStats` /
    `ServiceStat` / `MetricsReport`；`StatusSnapshot` 与
    `StatusReportPayload` 各加 `Hosts` / `Services`。
  - 上行通道：版本走 SSE handshake `?version=`（一次性）；指标走新
    endpoint `POST /v1/metrics/<backendID>`（周期推送，复用 IPC_SECRET
    Bearer 认证，64KB body 上限，仅接受已注册 SSE 的 backendID）。旧
    frontend 返回 404，backend 静默跳过——非破坏性。
  - `backendrpc.Connect` 改 `ConnectOptions` struct（D1）；新增
    `Client.PushMetrics` 与 `backendrpc.StartMetricsLoop`（推送周期绑死
    `status_monitor.interval`，单一真源）。
  - frontend：`BackendConn` 加 version/metrics 原子字段，`/v1/status`
    按 IP 去重聚合主机行；feishu-front 不 POST 自己，在 handler 内直读
    本机数据合并（D4）；frontend 不持久化 metrics（D5）。
  - 渲染（cardkit `StatusReportInput` options struct，D2）：版本漂移
    （落后于众数版本）行标 `🔴 版本漂移` + 卡头 blue→orange；超过
    `3 × interval` 未更新的行标 `(stale)`；`unknown` 版本不参与漂移判定。
  - **发版顺序约束**：先部署 feishu-front 再部署各 backend（deploy.sh
    顺序天然满足；反序时 metrics 在旧 frontend 上 404，主机/进程 section
    空白至 frontend 升级完成，业务路径不受影响）。

- **总览卡移动端分组布局**：飞书 markdown 用比例字体，原空格对齐的列布局
  在手机上永远错位，~70 字符行会在数字中间折行。改为按主机/服务分块：粗体
  身份行带版本漂移 / stale 标记，下面每指标一行缩进；版本号不再截断，
  summary 拆为两行。

- **terminal notice 原地 patch 命令卡（`PromptPayload.CardMessageID`）**：后端重启
  feishu-front，内存中的 promptID→turn 映射被清空，terminal notice 此前
  回退为独立卡，导致原进度卡永久卡死。现在前端在 Prompt
  事件里盖进度卡的 message_id（`PromptPayload.CardMessageID`，omitempty，
  非 override），后端在每个 terminal notice 上原样回带
  （`Notice.UpdateMessageID`），frontend 的 UpdateMessageID 快路径（裸
  message_id 跨重启存活）即可原地 patch 原卡。同时补两个洞：快路径现在会
  释放该 prompt 的 turn（否则 feishu-front 未重启场景如 `/pull` 会留下活
  turn 污染 `/running`），引用卡被撤回时回退 `SendCard`（结果不再丢失）。

### Changed

- **deploy.sh 拆 `lib-common.sh` + preflight 冲突规则前置到 frontend**：
  - A) deploy.sh（935 行多职责混杂）重构：共享样板（路径 / 颜色 / RUN_USER /
    env+systemd helper / svc 映射表）移到 `deploy/lib-common.sh`，三个脚本
    source 它；主流程改为 source guard 后面的具名 step 函数，helper 可单元
    测试（`deploy/tests/smoke.sh`，19 条断言，接入 `make test`）。同时修复
    长期存在的 shellcheck SC1073 解析错误，四个脚本 shellcheck-clean。
  - B) 在途 preflight 规则离开 bash 的 sed/grep JSON 解析，改走前端新端点
    `GET /v1/deploy-preflight?services=...`：200 安全 / 409 + 受影响
    backends + 预渲染原因。deploy.sh 收敛为一次 curl + status-code 分支；前端老于该
    端点时保守回退（任何 inflight 都阻断）。
- **`idle_timeout` 示例改为 `600s`**：`config.example.json` 的
  `timeouts.idle_timeout` 从零值默认（禁用）改为推荐的 `600s`，与 README
  建议对齐；零值依然表示禁用，旧部署行为不变。

### Fixed

- **loopback frontend 探测回退 `PrimaryIPv4`**：与前端同机部署
  （`frontend_url=localhost/127.0.0.1/[::1]`）时，`probeOutboundIP` 报出
  loopback IP，永远匹配不上 feishu-front 自报的 `PrimaryIPv4` ——
  `mergeHostByIP` 遂把一台物理机拆成两行（如 `10.0.2.15` vs `::1`）。
  探测结果为 loopback 时回退 `PrimaryIPv4`；远端前端仍走真实路由 IP。

## [1.4.0] - 2026-07-28

file upload (pandoc) / post rich-text / opencode idle watchdog。所有改动 opt-in：缺依赖 / 权限时降级通知，旧部署行为不变。

### Added

- **opencode-back 空闲看门狗（idle watchdog，opt-in）**：当 opencode
  子进程在 `idle_timeout` 内未吐出任何 stdout 事件时，判定为卡死（上游
  LLM 卡顿 / 内部死锁 / tool 等待 stdin），SIGKILL 整个进程组并向飞书
  发"响应超时"通知，避免用户永久等待却收不到回复（根因：观察到的
  glm-5.2 build agent 偶发于某个 step 中途停滞，`streamRun` 永久阻塞在
  `for ev := range events`，永不 `emitTerminal`）。
  - 与 `prompt_timeout`（总墙钟）互补：`idle_timeout` 每收到一个事件就
    重置，长但活跃的 turn 不会被误杀。
  - 配置：`timeouts.idle_timeout`（0 = 禁用，默认；建议 `120s`）。
  - 实现：`runPrompt` 起一个 `time.AfterFunc`，经 `onActivity` 回调由
    `streamRun` 每事件 reset；触发时 `cancel(errIdleTimeout)` → 进程组被
    `ApplyGroupCancel` SIGKILL → `streamRun` 检测 `context.Cause` 返回
    `isIdleTimeout`，`emitTerminal` 据此发"响应超时"而非"已取消"。
  - 回归测试：`TestStreamRun_IdleTimeoutMarked` /
    `TestStreamRun_UserCancelNotIdle` / `TestLoad_IdleTimeout`。

- **飞书富文本消息（post）支持 + 内联图片下载（opt-in）**：feishu-front
  现可接收 `MsgType=post` 的富文本消息，转换成 Markdown 物化到 inbox。
  采用方案 C（已按推荐定档全部 5 个决策点）。
  - 新增 `internal/feishu/post.go`：`Post` / `PostNode` AST +
    `ParsePost(content)`（locale 兜底）+ `RenderNodeToMarkdown`（纯函数）+
    `RenderPostToMarkdown`（含占位符完整渲染）+ `StripBotMentionsFromPost`
    （@bot / @all 剔除）。
  - 新增 `internal/feishufront/dispatcher_post.go`：`handlePostMessage` 串联
    AST 遍历 → 图片下载 → body.md 落盘 → post 模板渲染 → dispatchPrompt。
  - 内联图片：`tag=img` 下载到 `{inbox}/{chatID}/{promptID}/img-NNN.<ext>`，
    扩展名按响应 `Content-Type` 推断（png/jpg/gif/webp，默认 png）。
  - 错误降级矩阵：单图失败转占位符（`[图片下载失败]` / `[图片过大]` /
    `[图片存储失败]`），整条消息不阻塞；解析失败才整体阻塞。
  - `tag=media`（视频）渲染为 `[视频]` 占位符（agent 读不了视频，不下载）。
  - inbox 布局：扁平（`body.md` + `img-NNN.<ext>` + `raw/post.json`），
    与单文件场景统一。
  - 顺序下载（95% post ≤ 3 图，<3s），错误聚合无需锁。
  - 配置：新增 `file_convert.post_prompt_template`（**可选**，留空时走
    "降级路径"——post 转 Markdown 文本直接发，不下载图）。启用时若配错
    语法在启动时 fail-fast。
  - `IncomingMessage` 加 `Post *Post` 字段；`feishu.buildIncomingMessage`
    在 `case "post"` 时调用 `ParsePost` 一次，dispatcher 直接消费 AST。
  - 安全：复用 `inboxMaxSize` 单图大小上限；`@bot` / `@all` 在渲染前
    剔除；AST 深拷贝避免污染原始事件。
  - 协议层零改动：post 最终仍以纯文本 prompt 抵达后端（含 body.md 路径）。

- **飞书文件上传 → pandoc → markdown 管线（opt-in）**：feishu-front 现可接收
  群聊中上传的 `file` 类型消息（docx/md/markdown/txt），通过自实现的 IM resources
  端点下载二进制，pandoc 转换为 GitHub-flavoured Markdown，落地到
  `{inbox_dir}/{chatID}/{promptID}/{base}.md`，并把绝对路径以指令形式塞进
  prompt 文本发送给绑定的后端 agent（agent 用 Read 工具读取）。
  - 新增 `internal/fileconvert/` 包：docx 走外部 pandoc 子进程
    （`cmdutil.ApplyGroupCancel` 同款进程组 + SIGKILL 超时保护），md/txt 直拷。
  - 新增 `lark.Client.DownloadResource` 与 `feishu.Bot.DownloadFile`。
  - 新增 `config.FileConvert` 段（`enabled` / `pandoc_path` / `inbox_dir` /
    `max_file_size` / `convert_timeout` / `retention` / `prompt_template`），
    默认全部关闭，旧部署行为不变。`prompt_template` 为 Go `text/template`
    字符串（变量 `{{.FileName}}`/`{{.Path}}`/`{{.UserText}}`），启用时必填、
    启动时语法预检；代码层不内置默认文案，默认模板写在示例配置里供操作员
    直接编辑。
  - dispatcher 新增 `SetFilePipeline` + `PruneInbox`；inbox 启动时按 retention
    一次性裁剪。
  - 部署侧 `deploy.sh` 加 pandoc 软预检（warn 不 fail），feishu-front 启动时
    硬预检 `pandoc --version`，缺失且启用时拒绝启动。
  - 安全：inbox 目录 0700、文件 0600、路径元素白名单清洗（防 `..` 穿越），
    下载字节上限 30 MiB（可配），转换超时 60s（可配）。

### Removed

- **删除 `deploy/{claude,opencode,feishu}-config.json` 死模板**：这三个文件
  是 init commit 带进来的早期方案，自 `2f23369` 起 deploy.sh 改为从
  `config.example.json` 统一派生，再无任何 `.go`/`.sh` 消费它们（grep 验证）。
  保留反而误导操作员以为编辑它们会影响 deploy.sh 流程（曾因 schema drift
  触发 DisallowUnknownFields 反复 crash），且与 `Makefile:64` pack 行为
  不一致（tarball 从不打包它们）。`config.example.json` 成为唯一真源。

### Fixed

- **修 flaky 测试 `TestWSClient_PingSent`**：`PingInterval: 80ms` 在 `-race`
  下调度抖动，约 10% 概率在 3s 窗口内抢不到时间片而失败（20 次跑出 2 次）。
  interval 提升到 `200ms`（窗口内仍有 ~15 次 ping 机会），`-race -count=30`
  全绿。

- **ParsePost 支持 flat 形态的 post 富文本**：新版飞书客户端发的 post 是
  flat 形态（`title`/`content`/`content_v2` 直接挂在顶层，无 `{"zh_cn":{...}}`
  locale 包裹），旧解析器走 locale-wrapped 分支失败，整个 content 作为
  fallback 文本下发——观察到的现象是 UserText 里塞了完整 content_v2 JSON。
  `ParsePost` 改为顶层有 `content` 键即按 flat 解析，`content_v2` 一律忽略；
  `bot_dispatch` 解析失败时日志补 `raw_content`（1024 rune 截断）便于排查。


- **飞书权限**：使用本功能需在开发者后台为自建应用追加 `im:resource` 权限并
  经管理员审批；缺权限时下载返回 403，前端按 `下载失败` 通知用户。
- **协议层未变**：file 消息最终仍以纯文本 prompt 的形式抵达后端（文本里包含
  绝对路径 + 强指令），`PromptPayload` 结构未改，三个 agent 后端零改动。

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
  push-only；解耦于 `deploy.sh` 之外，由 `make upgrade-status`
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
- **P2 清理**：死代码删除、flap 状态机竞态修复、wsclient 按职责拆分。
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
> （`opencode-serve-back` 整体移除、`opencode-back /use`）。

### Added

- **进度卡 Zone 3.5 todo 清单渲染**：opencode-back 的 `todowrite` 与
  claude-back 的 `TodoWrite` 工具事件现路由到 `TypeTodo`，进度卡片原生展示
  ✅/⏳/⬜/✘ 清单（≤10 项展开、>10 项折叠为 `清单 N/M · ✅a ⏳b ⬜c ✘d`、
  cancelled 灰显），不再以 raw-JSON 工具行呈现。失败 fallback 仍走 `TypeToolResult`
  以保留可见性。
- **`/backend` picker 10min TTL**：未被点击的选择卡 10 分钟后自动翻"已失效"，
  与后端 interactive 卡的 TTL 行为对齐；点击即取消定时器。
- **opencode-back 新增 `/use`**（v1.1.0 期间合入，补记）：从
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
  `log_level` 占位符 regex 容错。
- **文档**：Makefile/README/deploy 二进制与服务数真源统一（5 个二进制 / 5 个业务
  systemd 服务）、deploy/README 双重真源警示、补 LICENSE（MIT）与 CHANGELOG。
