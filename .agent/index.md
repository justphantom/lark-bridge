---
updated: 2026-08-13T18:00:00+08:00
---

# .agent 记忆索引

## L0 永久约束
- [constraints.md](L0/constraints.md)
- [policies.md](L0/policies.md)
- [persona.md](L0/persona.md)

## L1 活跃过程
- [active/session.md](L1/active/session.md) — 会话状态（当前：deploy 脚本简化，进行中）
- [tasks/deploy-simplification.md](L1/tasks/deploy-simplification.md) — deploy 脚本简化（进行中，3 文件未提交）

## L1 归档（已完成）
- [archive/agnes-backend-removal.md](L1/archive/agnes-backend-removal.md) — 彻底移除 agnes-back C 档全量清零（`89e29e6`）
- [archive/claude-backend-removal.md](L1/archive/claude-backend-removal.md) — 清理 Claude 对接 B+C（`254bcd4`）
- [archive/config-dir-env-injection.md](L1/archive/config-dir-env-injection.md) — /config 扫描路径 MINIAGENT_CONFIG_DIR（`71349e0` / `89e29e6`）
- [archive/run-user-env-injection.md](L1/archive/run-user-env-injection.md) — 运行用户 .env 注入 RUN_USER（`3d31396` / `89e29e6`）
- [archive/miniagent-workdir-pin.md](L1/archive/miniagent-workdir-pin.md) — 钉死 miniagent CLI workdir 契约（miniagent 仓 / `0494eed`）
- [archive/release-v1.14.0.md](L1/archive/release-v1.14.0.md) — v1.14.0 发版评估与执行（已完成）
- [archive/card-terminal-guard.md](L1/archive/card-terminal-guard.md) — 卡片终态守卫统一（已完成）
- [archive/agnes-model-command.md](L1/archive/agnes-model-command.md) — agnes-back /model 指令：单卡三问弹卡 + 配置化模型列表（已完成）
- [archive/send-picker-ellipsis-fix.md](L1/archive/send-picker-ellipsis-fix.md) — /send 弹卡省略号修复：按钮卡启发式纵向单列布局（已完成）

## L2 经验教训

### 按主题检索
以下为高频 tag → 文件映射，供定向检索：

| 主题 tags | 相关文件 |
|---|---|
| `cardkit` `schema-2.0` `card-update` | [cardkit-migration](L2/decisions/cardkit-migration.md)（决策）、[feishu-interactive-card-callback](L2/patterns/feishu-interactive-card-callback.md)（实践）、[schema2-regression](L2/incidents/schema2-card-action-regression.md)（事故）、[feishu-card-bounce-back](L2/incidents/feishu-card-bounce-back.md)（事故）、[card-terminal-state-guard](L2/patterns/card-terminal-state-guard.md)（⚠️historical） |
| `card-callback` `inline-vs-entity` | [feishu-interactive-card-callback](L2/patterns/feishu-interactive-card-callback.md)、[schema2-regression](L2/incidents/schema2-card-action-regression.md) |
| `picker` `question-card` `mobile-layout` | [button-card-mobile-list-layout](L2/patterns/button-card-mobile-list-layout.md)、[multi-question-card](L2/patterns/multi-question-card.md) |
| `miniagent` `session` `jsonl` | [miniagent-integration](L2/patterns/miniagent-integration.md)、[miniagent-session-archive](L2/patterns/miniagent-session-archive.md) |
| `deploy` `backend-removal` `disallowunknownfields` | [backend-removal-checklist](L2/patterns/backend-removal-checklist.md)、[new-backend-skeleton](L2/patterns/new-backend-skeleton.md) |
| `diagnostics` `control-flow` | [diagnostic-logging](L2/patterns/diagnostic-logging.md) |
| `race-condition` `terminal-state` `inflight-turn` | [inflight-turn-consistency](L2/patterns/inflight-turn-consistency.md)、[feishu-card-bounce-back](L2/incidents/feishu-card-bounce-back.md)、[card-terminal-state-guard](L2/patterns/card-terminal-state-guard.md)（⚠️historical） |
| `agnes-ai` | [agnes-api-integration](L2/patterns/agnes-api-integration.md)（⚠️historical）、[agnes-override-handler-layer](L2/patterns/agnes-override-handler-layer.md)（⚠️historical） |

### 全部条目
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
- [patterns/button-card-mobile-list-layout.md](L2/patterns/button-card-mobile-list-layout.md) — 飞书按钮卡移动端布局：长标签必须纵向单列
- [patterns/backend-removal-checklist.md](L2/patterns/backend-removal-checklist.md) — 移除一个后端的清单（Go→Makefile→svc_*→cleanup_legacy ghost 段→smoke；共享 Config+DisallowUnknownFields 陷阱）
