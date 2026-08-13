---
layer: L2
type: pattern
tags: [diagnostics, feishu, card-update, ipc, control-flow]
created: 2026-08-10
verified_at: 2026-08-13
applies_to: b965415
confidence: high
---

# 全链路诊断日志的铺设模式

## 背景
排查 /backend 卡片回调和（已移除的）agnes notice 未到达的问题时，发现纯靠
猜链路断点低效——多次返工。解决方案：在每条事件路径的关键决策点铺
`Debug`/`Warn` 日志，带语义标签（`"card action"`、`"sendResult"`）+ 关键 ID
（chatID / promptID / messageID），让一次复现即可定位断点。

## 铺设位点（三层）

| 层 | 位点 | 示例 |
|---|---|---|
| **入口层** | 事件接收/丢弃时 | `bot_dispatch.go`：`card action received` / `drop card action: empty operator openid` |
| **分发层** | 路由解析、转发后端时 | `dispatcher_interactive.go`：`card action: sending answer to backend` / `router is nil` / `failed to resolve backend` / `event sent successfully` |
| **出口层** | Control 发送结果 | `dispatcher_control.go`：`sendResult` |

## 写法约定

1. **"已发生的"日志（Debug/Info）带主语 + 动作 + ID**：
   `d.logger.Load().Debug("card action: sending event to backend", "chat_id", chatID, "prompt_id", promptID)`
2. **"异常分支"日志（Warn）带原因 + 可定位字段**：
   `b.logger.Warn("card action: no handler registered", "event_id", ev.EventID)`
3. **不记录敏感值**：API key / token / 用户内容正文只记长度或哈希。
4. **使用结构化键值**（`logger.Info("msg", "key", val)`），不用 `Printf` 拼串。
   便于后续按 key 过滤。

## 诊断方法

当"某个行为没发生"时，沿链路从入口到出口逐层 grep 日志：

```
card action received → card action: sending answer → card action: event sent → sendResult
```

**最后一条出现的日志的下一跳就是断点。** "该出现的日志没出现"是关键信号：
日志消失说明走了另一条路径，需沿 ID 把用户操作时间戳与日志时间戳对齐定位。

## 参考
- 入口层：`internal/feishu/bot_dispatch.go`（card action 日志）
- 分发层：`internal/feishufront/dispatcher_interactive.go`（card action 转发日志）
- 出口层：`internal/feishufront/dispatcher_control.go`（sendResult 日志）
- 相关 incident：[incidents/feishu-card-bounce-back.md](../incidents/feishu-card-bounce-back.md)
  （诊断日志的应用案例——want/got 指纹对比一次命中根因）
