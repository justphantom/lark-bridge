---
layer: L2
type: pattern
tags: [feishu, card, patch, race-condition, terminal-state, delayed-write, ttl, guard]
created: 2026-08-09
confidence: high
---

# 卡片终态守卫模式：终态标记 + 落地前自弃

## 问题原型
一张卡片存在多个异步写者（立即结果帧、延迟刷新 PATCH、TTL 失效帧、兜底
PATCH），写者落地顺序不可控时，后到写者可能把已到达终态的卡片"复活"或
回弹。典型案例见 [incidents/feishu-card-bounce-back.md](../incidents/feishu-card-bounce-back.md)。

## 模式（两条不变式）

1. **终态帧落地成功后一律 `markCardTerminal(messageID)`**。
   终态帧包括：结果帧（成功/失败）、finalize 绿帧、TTL 失效帧、降级发新卡
   时的旧卡。
2. **延迟/定时写者 PATCH 前一律 `isCardTerminal(messageID)` 检查，命中自弃**。
   延迟/定时写者包括：点击窗口延迟刷新、submit fallback 兜底、TTL 失效回调。

`terminalCards` 带 TTL（= `cardkit.InteractiveTimeout`），必须长于最长延迟
写者的睡眠时长。

## 适用判断
每新增一个"延迟/兜底/重试/TTL"写者时，问两个问题：
- 我落地时，目标卡是否可能已被别的写者终结？→ 落地前检查。
- 我落地的是否是该卡的最后一帧？→ 落地后标记。

不要试图重排时序或靠读回判断解决——终态标记比两者都干净。

## 全项目覆盖点（2026-08-09 推广）

| 写者 | 文件 | 标记 | 检查 |
|---|---|---|---|
| reflectFileOutcome 结果帧/降级 | dispatcher_file_send.go | ✅ | — |
| sendInteractiveCard 延迟刷新 | dispatcher_interactive.go | — | ✅（原有） |
| scheduleSubmitFallback 兜底 | dispatcher_interactive.go | — | ✅（新增，binding 检查外第二道防线） |
| handleBackendChoice 延迟结果帧 | dispatcher_backend.go | ✅（新增） | ✅（新增） |
| finalizeLinkedInteractive 绿帧 | dispatcher_interactive.go | ✅（新增） | — |
| expireInteractive 失效帧 | dispatcher_interactive.go | ✅（新增） | ✅（新增） |
| expirePicker 失效帧 | dispatcher_backend.go | ✅（新增） | ✅（新增） |

回归测试：`dispatcher_terminal_guard_test.go`
（`TestExpireInteractive_DropsWhenCardAlreadyTerminal` /
`TestDelayedRefresh_DropsAfterExpire`）。

## 参考
- 原始事故与修复：[incidents/feishu-card-bounce-back.md](../incidents/feishu-card-bounce-back.md)
