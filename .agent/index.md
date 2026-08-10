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
- [archive/card-terminal-guard.md](L1/archive/card-terminal-guard.md) — 卡片终态守卫统一（已完成）
- [archive/release-v1.14.0.md](L1/archive/release-v1.14.0.md) — v1.14.0 发版评估与执行（已完成）
- [archive/agnes-model-command.md](L1/archive/agnes-model-command.md) — agnes-back /model 指令：单卡三问弹卡 + 配置化模型列表（已完成）

## L2 经验教训
- [incidents/feishu-card-bounce-back.md](L2/incidents/feishu-card-bounce-back.md) — `/send` 文件卡片回弹事故
- [incidents/schema2-card-action-regression.md](L2/incidents/schema2-card-action-regression.md) — schema 2.0 全量切换后交互卡片回调失效
- [decisions/cardkit-migration.md](L2/decisions/cardkit-migration.md) — 迁移到 CardKit 实体 API 决策
- [patterns/miniagent-integration.md](L2/patterns/miniagent-integration.md) — Miniagent 对接模式与缺口
- [patterns/miniagent-session-archive.md](L2/patterns/miniagent-session-archive.md) — Miniagent 会话 JSONL 解析与清理
- [patterns/inflight-turn-consistency.md](L2/patterns/inflight-turn-consistency.md) — 前后端运行中 Turn 一致性模型
- [patterns/feishu-interactive-card-callback.md](L2/patterns/feishu-interactive-card-callback.md) — 飞书卡片交互回调正确实践（inline + 不声明 behaviors）
- [patterns/new-backend-skeleton.md](L2/patterns/new-backend-skeleton.md) — 新后端搭建标准骨架（config→包→入口→部署）
- [patterns/agnes-api-integration.md](L2/patterns/agnes-api-integration.md) — Agnes AI 三模型对接经验（含 video url 字段差异坑）
- [patterns/diagnostic-logging.md](L2/patterns/diagnostic-logging.md) — 全链路诊断日志铺设模式（入口 Info/出口 Warn/错误 Warn）
- [patterns/card-terminal-state-guard.md](L2/patterns/card-terminal-state-guard.md) — 卡片终态守卫：终态标记 + 落地前自弃
- [patterns/multi-question-card.md](L2/patterns/multi-question-card.md) — 多问题 Question 卡 Choices 映射与 Custom 全局单串限制
- [patterns/agnes-override-handler-layer.md](L2/patterns/agnes-override-handler-layer.md) — agnes-back 运行时覆盖状态放 Handler 层（APIClient 接口/mock 兼容性）

