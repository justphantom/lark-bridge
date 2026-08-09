---
layer: L2
type: pattern
tags: [lark-bridge, feishu-front, claude-back, miniagent-back, turn, consistency, race-condition]
created: 2026-08-09
confidence: high
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
| binding 被替换导致旧 session ID 回写串台 | `SetSessionIDIfGeneration`（generation incarnation token） |
| 持久化文件截断/损坏 | 原子写、version 校验、corrupt 备份 |

## 残余风险与建议

1. **SSE 重连后短暂漏计**
   - 后端 SSE 断开后重连会新建 conn，`runningTurns` 为空，需等 MetricsReport 自愈。
   - **建议**：SSE handshake 成功后立即推送一次 turn 快照。
2. **双视图不同步**
   - `TurnManager` 与 `runningTurns` 分别维护，Start/Finish 触发点不同。
   - **建议**：`/v1/status` inflight 直接复用 `TurnManager.InFlight()`，废弃 `runningTurns` 的 status 用途。
3. **reclaim 未清 runningTurns**
   - `reclaimStrandedTurns` 只清 `TurnManager`。
   - **建议**：同步调用 `registry.ReclaimTurns(backendID)`。
4. **miniagent session ID 双写策略**
   - claude 后端用 `Binding.SessionID`；miniagent 后端用独立 `.id` 文件。
   - **建议**：不在代码层统一，只在文档中明确记录差异及原因。

## 参考
- 分析文档：`docs/session-consistency-analysis.md`
