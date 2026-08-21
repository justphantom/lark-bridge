---
layer: L2
type: pattern
tags: [miniagent, config-only, version-check, eventmetrics, stream-archive, v5-breaking, v5.1-compatible]
created: 2026-08-09
confidence: high
verified_at: 2026-08-19
applies_to: HEAD (miniagent v5.1.0)
---

# Miniagent 对接模式与常见缺口

## 对接架构
`feishu-front` → `miniagent-back` → 启动 `miniagent` CLI 子进程，通过 NDJSON stdout 交换事件。bridge 侧通过 `internal/miniclient` 管理子进程，通过 `internal/miniagent` 做命令分发。

## 已确认模式

1. **config-only（v3.1+）**
   - 端点/API key/providers/run/compaction 全部收敛到 `miniagent.json`。
   - bridge 启动子进程时恒带 `-config <abs>`；model/thinking/max-iterations/session 仍可用 flag 覆盖（`-mode` 已随 miniagent v5.0.0 删除）。
2. **事件流**
   - NDJSON 事件：`tool_use` / `tool_result` / `text_delta` / `reasoning_delta` / `result` / `error`。
   - `text_delta` 在 bridge 侧故意不转发；`reasoning_delta` 作为 Thinking 增量直发前端。

## 已踩过的缺口

1. **版本兼容性检查缺失**
   - 启动时用 `miniagent --version` 做 `DetectVersion()`，低于最低版本则拒绝启动。
   - 必须特判 `dev` / 脏构建，避免挡住本地开发。
2. **ConfigPath 未校验绝对路径**
   - 启动时检查 `filepath.IsAbs(cfg.MiniAgent.ConfigPath)`，相对路径会导致 session/workdir 解析失败。
3. **per-turn 指标已接入 eventmetrics** — `[x] 已实现`
   - `handler_cli.go` 已结构化记录 `steps/finish/input_tokens/output_tokens/duration`，并同步到 `internal/eventmetrics/`（`MiniAgentTurnCount/Duration/InputTokens/OutputTokens`）。
4. **StreamArchiveRedact 默认关闭**
   - `applyDefaults` 阶段应显式置为 `true`，否则流归档中敏感字段明文落盘。
   - 注意：`bool + omitempty` 无法区分“未设置”与“显式 false”；若需保留关闭能力，字段应改为 `*bool`。
5. **不可直接引用上游 internal 包**
   - `../miniagent` 是独立 Go module，bridge 无法 import 其 `internal` 函数（如 `readMemoryRecords`）。需在 bridge 侧自实现 jsonl 解析。
6. **workdir 契约已钉死（2026-08-12，上游 fbfd848+）** — `[x] 已实现`
   - miniagent CLI 的 workdir：非空 + 必须绝对路径 + **只来自 `-workdir` flag**（删 config `run.workdir`、删 `absWorkdir` 的 `os.Getwd()` 回退、auto 模式也强制）。bridge 恒传绝对 `-workdir`（`workspaceRoot`/`b.Directory` 均绝对），本就合规、无需改动。
   - **教训（排查"漂移到 /home/dev"）**：那是 conflation——`/home/dev/.miniagent` 是 `-config` 配置目录，`/current` 把 `配置文件：…` 印在 `工作目录：…` 下一行易被误读；`/var/lib/lark-bridge/router-miniagent.v5.json`（非 config 里失效的 `router_path`）才是真 binding 来源。systemd 下进程 cwd=`/`、HOME=`/var/lib/lark-bridge`，全链路 workdir 恒为 `/opt/code/*`。
   - **部署生效前提**：在线 `/usr/local/bin/miniagent` 须 `make build` 重装，否则仍是旧契约。

## v5.0.0 破坏性变更对齐（2026-08-18）

miniagent v5.0.0 删除 `-mode` 双模式（default/auto 合并为单模式），**删 agent 层全部安全保障**（confineWrap / `.git` 封锁 / 白名单子命令工具 / deny-args / rtk proxy），安全完全靠运行用户 OS 权限。工具精简到 8 个（read/write/edit/grep/glob/ast/shell/web），新增 OpenAI Responses provider（`kind=responses`）和 `web` 抓取工具。

**bridge 对齐**：硬切——`minSupportedVersion` 从 `4.2.0` 提升到 `5.0.0`（4.x 直接拒绝，避免双安全语义并行）；删 `-mode` 发射、`/mode` 命令（`settableModes`/`modeOptions`/`cmdMode`/`cmdModeBridge`）、`Binding.Mode`/`Router.SetMode`、`Config.MiniAgent.Mode`（含 `applyDefaults` 默认 + `validate` 校验）、`Client.Mode`/`DefaultMode()`/`RunOptions.Mode`、`activeMode()`/`clientDefaultMode()`；`activeTurnConfig` 从 6 元组降 5 元组；`/current` 去掉"权限模式"行。

**兼容性分析**：NDJSON 事件契约不变（tool_use/tool_result/text_delta/reasoning_delta/result/error/session/model 全保留），result 事件新增 `llm_requests` 字段被 `json.Unmarshal` 忽略未知字段安全处理。`-provider/-model` 成对规则、`-list-models` 格式、`-workdir` 必填绝对路径均不变。bridge config `DisallowUnknownFields` 意味着旧配置里的 `"mode"` 键会启动失败——operator 必须删除该键（迁移提示已写入 CHANGELOG）。router JSON 不用 `DisallowUnknownFields`，旧 `mode` key 静默忽略，无需迁移。

## v5.1.0 兼容跟进（2026-08-19）

miniagent v5.1.0 是 v5.0.0 之后的**兼容**版本，对 bridge 唯一可见的接口变化：

- **`result` NDJSON 事件新增 `compacted` / `thinking_downgraded` 布尔字段**（恒出键，false 默认）。`compacted` 表示本轮触发过上下文摘要压缩；`thinking_downgraded` 表示请求的 thinking 级别被降级、reasoning 输出被丢弃。非破坏：旧 CLI 不发此键 → `json.Unmarshal` 零值，行为不变。
- 内部重构（模型清单聚合从 `openai` 包上移至 `cmd` 层、清 v5.0.0 工具残留）——CLI/NDJSON/config 契约零变化。

**bridge 对齐**：
- `minSupportedVersion` 从 `5.0.0` 提升到 `5.1.0`（非硬切：v5.1.0 非 breaking，5.0.0 CLI 仍可工作；提升下限仅为"跟进最新已发版"语义，同时保留 v5.0.0 硬切拒绝 4.x 的既有语义）。
- `Event` / `rawEvent`（`internal/miniclient/event.go`）各加 `Compacted` / `ThinkingDowngraded` 字段（`omitempty`），`parseEvent` 透传。
- `handler_cli.go` 的 `KindResult` 分支把两字段纳入 `miniagent turn done` 诊断日志（仅日志，不进 `ResultPayload`——协议层无对应字段，避免扩散到前端渲染，保持最小改动）。

**未跟进（miniagent HEAD 未发版）**：HEAD 领先 v5.1.0 三提交，含**未打 tag 的破坏性变更**——删 `anthropic`+`responses` provider（`ProviderConfig.Kind` 只收 `""`/`"openai"`）+ config `DisallowUnknownFields`。bridge 部署生成的 `miniagent-cli.json` 不设 `kind`（默认 openai）、无 anthropic/responses provider，不受影响；未发版不对齐，待其发版后再评估。

## 参考
- 相关代码：`internal/miniclient/client.go`、`internal/miniagent/handler_cli.go`
