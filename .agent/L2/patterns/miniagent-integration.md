---
layer: L2
type: pattern
tags: [miniagent, integration, config-only, version-check, eventmetrics, stream-archive]
created: 2026-08-09
confidence: high
---

# Miniagent 对接模式与常见缺口

## 对接架构
`feishu-front` → `miniagent-back` → 启动 `miniagent` CLI 子进程，通过 NDJSON stdout 交换事件。bridge 侧通过 `internal/miniclient` 管理子进程，通过 `internal/miniagent` 做命令分发。

## 已确认模式

1. **config-only（v3.1+）**
   - 端点/API key/providers/run/compaction 全部收敛到 `miniagent.json`。
   - bridge 启动子进程时恒带 `-config <abs>`；model/mode/thinking/max-iterations/session 仍可用 flag 覆盖。
2. **事件流**
   - NDJSON 事件：`tool_use` / `tool_result` / `text_delta` / `reasoning_delta` / `result` / `error`。
   - `text_delta` 在 bridge 侧故意不转发；`reasoning_delta` 作为 Thinking 增量直发前端。

## 已踩过的缺口

1. **版本兼容性检查缺失**
   - 启动时用 `miniagent --version` 做 `DetectVersion()`，低于最低版本则拒绝启动。
   - 必须特判 `dev` / 脏构建，避免挡住本地开发。
2. **ConfigPath 未校验绝对路径**
   - 启动时检查 `filepath.IsAbs(cfg.MiniAgent.ConfigPath)`，相对路径会导致 session/workdir 解析失败。
3. **per-turn 指标未进 eventmetrics**
   - `handler_cli.go` 已结构化记录 `steps/finish/input_tokens/output_tokens/duration`，需同步到 `internal/eventmetrics/`。
4. **StreamArchiveRedact 默认关闭**
   - `applyDefaults` 阶段应显式置为 `true`，否则流归档中敏感字段明文落盘。
   - 注意：`bool + omitempty` 无法区分“未设置”与“显式 false”；若需保留关闭能力，字段应改为 `*bool`。
5. **不可直接引用上游 internal 包**
   - `../miniagent` 是独立 Go module，bridge 无法 import 其 `internal` 函数（如 `readMemoryRecords`）。需在 bridge 侧自实现 jsonl 解析。

## 参考
- 分析文档：`docs/miniagent-integration-analysis.md`
- 相关代码：`internal/miniclient/client.go`、`internal/miniagent/handler_cli.go`
