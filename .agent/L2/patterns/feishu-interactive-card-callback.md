---
layer: L2
type: pattern
tags: [feishu, cardkit, interactive-card, callback, inline-vs-entity, card-action-trigger]
created: 2026-08-10
confidence: high
---

# 飞书卡片交互回调的正确实践

## 核心结论（实测 2026-08-10）

飞书卡片要触发 `card.action.trigger` 事件（即按钮点击回调），**两个条件必须同时满足**：

1. **必须用 inline JSON 发送**（`SendCardInline` → `msg_type=interactive` + content=完整 card JSON）。
   CardKit 实体引用卡片（`content={"type":"card","data":{"card_id":...}}`）**不触发** button 点击回调。

2. **button 不能声明 `behaviors:[{type:"callback"}]`**。
   inline 卡片的 button 声明此字段会**抑制**回调（不是声明回调）。
   正确做法：只靠 button 的 `value` 字段触发回调（与 schema 1.0 一致）。

## 卡片类型 vs 发送方式决策表

| 卡片用途 | 需要按钮回调 | 需要流式 PUT 更新 | 发送方式 |
|---|---|---|---|
| /backend picker | ✅ | ✅（点击后 PATCH） | `SendCardInline` + `UpdateCard`(im PATCH) |
| permission/question | ✅ | ✅（刷新/失效） | `SendCardInline` + `UpdateCard`(im PATCH) |
| 通知/结果/notice | ❌ | ❌ | `SendCard`（CardKit 实体） |
| 进度卡（流式更新） | ❌ | ✅ | `SendCard`（CardKit 实体）+ `UpdateCard`(PUT) |
| status-monitor 总览 | ❌ | ✅ | `SendCard`（CardKit 实体）+ `UpdateCard`(PUT) |

## 实现锚点

- **发送**：`feishu/bot_send.go` — `SendCard`（CardKit 实体）vs `SendCardInline`（inline JSON）
- **更新**：`feishu/bot_send.go` — `UpdateCard`：`cardID!=""` 走 CardKit PUT，`cardID==""` 走 im PATCH
- **接口**：`feishufront/dispatcher.go` — `CardSink` 含 `SendCard` + `SendCardInline` + `UpdateCard`
- **按钮**：`cardkit/elements.go` — `ButtonAction` / `SubmitButtonAction` **不带** behaviors

## 何时检查本条

- 新增任何带按钮的卡片 → 确认用 `SendCardInline`
- 改 `ButtonAction` / `SubmitButtonAction` → 确认没有 reintroduce behaviors
- card action 回调消失 → 先查 `journalctl | grep "card action"`，再查发送方式（inline vs 实体）和 button JSON（有无 behaviors）

## 参考
- 事故：[incidents/schema2-card-action-regression.md](../incidents/schema2-card-action-regression.md)
- 决策：[decisions/cardkit-migration.md](../decisions/cardkit-migration.md)
- 修复提交：`e223940`
