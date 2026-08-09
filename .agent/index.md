---
updated: 2026-08-10T00:50:00+08:00
---

# .agent 记忆索引

## L0 永久约束
- [constraints.md](L0/constraints.md)
- [policies.md](L0/policies.md)
- [persona.md](L0/persona.md)

## L1 活跃过程
- [session.md](L1/active/session.md)

## L2 经验教训
- [incidents/feishu-card-bounce-back.md](L2/incidents/feishu-card-bounce-back.md) — `/send` 文件卡片回弹事故
- [decisions/cardkit-migration.md](L2/decisions/cardkit-migration.md) — 迁移到 CardKit 实体 API 决策
- [patterns/miniagent-integration.md](L2/patterns/miniagent-integration.md) — Miniagent 对接模式与缺口
- [patterns/miniagent-session-archive.md](L2/patterns/miniagent-session-archive.md) — Miniagent 会话 JSONL 解析与清理
- [patterns/inflight-turn-consistency.md](L2/patterns/inflight-turn-consistency.md) — 前后端运行中 Turn 一致性模型
- [patterns/feishu-interactive-card-callback.md](L2/patterns/feishu-interactive-card-callback.md) — 飞书卡片交互回调正确实践（inline + 不声明 behaviors）
- [patterns/new-backend-skeleton.md](L2/patterns/new-backend-skeleton.md) — 新后端搭建标准骨架（config→包→入口→部署）
- [patterns/agnes-api-integration.md](L2/patterns/agnes-api-integration.md) — Agnes AI 三模型对接经验（含 video url 字段差异坑）
- [incidents/schema2-card-action-regression.md](L2/incidents/schema2-card-action-regression.md) — schema 2.0 全量切换后交互卡片回调失效
- [patterns/card-terminal-state-guard.md](L2/patterns/card-terminal-state-guard.md) — 卡片终态守卫：终态标记 + 落地前自弃
