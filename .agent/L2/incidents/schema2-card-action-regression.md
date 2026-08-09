---
layer: L2
type: incident
tags: [feishu, cardkit, schema-2.0, card-action, callback, behaviors, regression]
created: 2026-08-10
confidence: high
---

# schema 2.0 全量切换后交互卡片回调全部失效

## 现象
b214834（卡片链路全量切换 schema 2.0）部署后，所有交互卡片（/backend
picker、permission、question）的按钮点击不再触发 card.action.trigger 事件。
消息接收（im.message.receive_v1）正常，websocket 连接健康，但 card action
事件完全消失。

## 根因
飞书 schema-2.0 **inline 卡片**（直接发送 card JSON，非 CardKit 实体引用）的
button 如果声明 `behaviors:[{type:"callback",callback:{}}]`，**反而不触发**
card.action.trigger 事件。只有不声明 behaviors、仅靠 button 的 `value` 字段
才能触发回调（与 schema 1.0 行为一致）。

b214834 的 ButtonAction 把 `maybeBehaviors()`（条件性，legacy 模式下返回 nil）
改成了 `callbackBehaviors()`（无条件添加），导致所有 button 都带 behaviors，
回调全部失效。

## 诊断过程
1. 最初怀疑 CardKit 实体引用（card_id 模式）不触发 callback → 把交互卡片改为
   inline 发送（SendCardInline）→ 仍无回调。
2. 对比 b214834 前后的 ButtonAction：发现 `maybeBehaviors` → `callbackBehaviors`
   的无条件化。
3. 实验验证：临时移除 behaviors → card action 立即恢复（00:29:16 日志确认）。

## 修复
- ButtonAction / SubmitButtonAction：移除 `behaviors` 声明，依赖 `value` 字段
  自动触发回调。
- 交互卡片改用 `SendCardInline`（inline JSON），通知/进度卡保持 CardKit 实体。
- 恢复 `lark.PatchMessage`（inline 卡片的 im PATCH 更新路径）。
- `UpdateCard`：cardID=="" 时 fallback 到 im PATCH。

## 可复用经验
1. **飞书 schema 2.0 的 behaviors:callback 对 inline 卡片是陷阱**：它不是
   "声明回调"而是"抑制回调"。button 的 value 字段才是触发回调的机制。
2. **CardKit 实体卡片（card_id 引用）也不触发 card.action.trigger**——交互卡片
   必须用 inline JSON 发送。这是 b214834 的两个叠加回归（实体 + behaviors）。
3. **诊断 card action 失效的方法**：对比旧/新进程发送的 card JSON 差异
   （schema + behaviors），用 grep card action 日志确认零回调。
4. **picker 列表省略号**是另一个独立问题：actionColumnSets 把 N 个按钮平铺在一
   行，长文本被截断。修复：CardWithColumns(maxCols=1) 让每个按钮独占一行。

## 参考
- 引入回归：`b214834`
- 修复提交：`e223940`
- 相关决策：[decisions/cardkit-migration.md](../decisions/cardkit-migration.md)
  （PoC 验证了 PUT 更新但遗漏了 callback 回归验证）
