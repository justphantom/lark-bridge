---
layer: L2
type: decision
tags: [feishu, cardkit, card, migration, schema-v2, sequence, poc]
created: 2026-08-09
confidence: high
---

# 卡片更新链路迁移到 CardKit 实体 API

## 背景
原链路：schema 1.0 卡片 + im PATCH（`PatchMessage`）。飞书点击后存在 3-5s 静默回弹窗口，为此引入延迟 PATCH、`UpdateCardVerified` 读回校验、`scheduleSubmitFallback`、`skipSubmitFlip` 等整套防御。

## 决策
迁移到 **CardKit 实体 API**（schema 2.0 + `card_id` + PUT 全量更新）。

PoC 结论（2026-08-09）：
- 点击窗口内 PUT **直接成功**，无回弹、无闪变。
- 乱序 sequence 被平台显式拒绝（错误码 300317）。
- 因此延迟 PATCH、读回校验、fallback 等防御可全部删除。

## 关键约束

1. **渲染层**：v1 → v2 结构迁移。v2 顶层为 `schema/config/header/body.elements`；`update_multi` 必须为 `true`；每个组件须分配全局唯一 `element_id`。
2. **按钮容器**：v2 不支持 `{"tag":"action","actions":[...]}`，须用 `column_set > column > button`（实测错误码 200861）。
3. **双键**：发送后需同时保存 `messageID`（收回调）和 `card_id`（更新操作）。
4. **sequence**：每张卡维护单调递增计数器；进程重启后可用时间戳作为大基数重启。
5. **读回校验不可行**：`GetMessage` 对实体卡只返回 card_id 引用或降级文本，无卡片 JSON，`UpdateCardVerified` 整套删除。注意：**legacy v1 im PATCH 路径仍保留 `UpdateCardVerified`**，其指纹校验的陷阱（read-back 深度重写卡片、只有归一化可见文本可比对）详见 [incidents/feishu-card-bounce-back.md](../incidents/feishu-card-bounce-back.md)。
6. **流式接口限制**：`elements/:element_id/content` 打字机接口**仅限流式模式开启期间**使用；关流式后调用返回 300309。非流式组件更新用 elements 增删改或全量 PUT。
7. **TTL 与过期**：实体卡 14 天有效期（错误码 200750），过期后降级为发新卡。
8. **权限**：需新增 scope `cardkit:card:write`。
9. **灰度**：配置开关 `card_engine: cardkit|legacy`；存量 v1 卡继续走旧 PATCH 路径（按是否存 card_id 区分）。

## 参考
- 评估文档：`docs/feishu-cardkit-migration-assessment.md`
