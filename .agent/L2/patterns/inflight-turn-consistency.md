---
layer: L2
type: pattern
tags: [miniagent, race-condition, inflight-turn]
created: 2026-08-09
confidence: high
verified_at: 2026-08-13
applies_to: b965415
---

# 前后端运行中 Turn 的一致性模型

## 状态分层
一次 turn 涉及三层独立状态机：

1. **持久化层**（磁盘，跨进程存活）
   - `Layer1Router`：chatID → backendID
   - `router.Binding`：chatID → {sessionID, dir, model, ...}
   - miniagent `.id` 文件：chatID(hash) → sessionID
2. **运行时层**（内存，进程重启即丢）
   - 前端 `feishu-front`：`TurnManager.turns`（渲染生命周期主表）、`BackendConn.runningTurns`（status 门控）
   - 后端 `claude-back` / `miniagent-back`：`cancelBy` / `CancelByChat`（权威来源）

## 核心原则
- **真相源在后端**：`cancelBy` 里有 entry，turn 就真在跑。
- 前端 `TurnManager` 和 `runningTurns` 都是派生视图，通过控制消息和周期快照同步。

## 已有保障机制

| 风险 | 机制 |
|---|---|
| `TurnStarted`/`Finished` 控制丢失导致前端漂移 | MetricsReport 周期快照全量替换 `replaceRunningTurns` |
| 重复 terminal 控制双发卡片 | `terminals` dedup set（PromptID 键，10m TTL） |
| 后端无限重试 | terminal ACK（前端回 `TypeAck`） |
| 后端 crash 后 turn 永不结束 | `reclaimStrandedTurns`：离线超 30s 回收 |
| 后端 flapping 刷屏 | offline notice debounce 30s |
| binding 被替换导致旧 session ID 回写串台 | 历史用 `SetSessionIDIfGeneration`（generation incarnation token）；router 重构后该机制已移除，当前依赖 miniagent 独立 `.id` 文件隔离 |
| 持久化文件截断/损坏 | 原子写、version 校验、corrupt 备份 |

## 残余风险与建议

> 状态核实于 2026-08-13（对照当前代码：HEAD `b965415`）。

1. **SSE 重连后短暂漏计** — `[ ] 未实现`
   - 后端 SSE 断开后重连会新建 conn，`runningTurns` 为空，需等 MetricsReport 自愈。
   - **建议**：SSE handshake 成功后立即推送一次 turn 快照。（`registry.go` 握手路径无 snapshot 推送，仍是周期自愈）
2. **双视图不同步** — `[x] 已实现`
   - `cmd/feishu-front/main.go:205-206`：`ipc.SetInFlightTurns(turns.InFlight)` / `SetInFlightDetail(turns.InFlightTurns)`，`/v1/status` 已直接复用 `TurnManager`。
3. **reclaim 未清 runningTurns** — `[x] 已修复（2026-08-09）`
   - `reclaimStrandedTurns`（`dispatcher_backend.go`）现同步调 `registry.ReclaimTurns(backendID)`（`registry.go`），清空该后端在 registry 的 `runningTurns`，两套 in-flight 视图不再分歧。
   - 回归测试：`TestBackendRegistry_ReclaimTurns` + `TestFireOfflineNotice_ReclaimsStrandedTurns`（断言镜像清理）。
4. **miniagent session ID 双写策略** — `[x] 已记录（有意保留差异）`
   - claude 后端用 `Binding.SessionID`；miniagent 后端用独立 `.id` 文件。
   - 不在代码层统一，差异及原因以本文档为准。

