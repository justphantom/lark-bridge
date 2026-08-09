---
layer: L1
type: task
status: done
created: 2026-08-09T18:45:00+08:00
---

# 任务：终态标记模式推广到全项目

## 目标
将 `/send` 回弹修复的「终态标记 + 落地前自弃」模式推广到 feishufront 所有卡片写者。

## 方案（用户已确认）
1. 终态帧落地成功后一律 `markCardTerminal`：
   - `finalizeLinkedInteractive`、`handleBackendChoice`（延迟 PATCH 成功后）、
     `expireInteractive`、`expirePicker`、`reflectFileOutcome` 降级路径（标记旧卡）
2. 延迟/定时写者 PATCH 前一律 `isCardTerminal` 检查，命中自弃：
   - `scheduleSubmitFallback`、`expireInteractive`、`expirePicker`
3. 回归测试：expire-vs-refresh 复活场景、fallback 自弃场景
4. 完成后沉淀 `.agent/L2/patterns/card-terminal-state-guard.md` 并与 incident 互链

## 进度
- [ ] 代码改动
- [ ] 回归测试
- [ ] go test ./... 通过
- [ ] L2 沉淀

## 结果（2026-08-09 完成）
- 6 处写者补齐标记/检查（file_send、backend、interactive）。
- 新增回归测试 dispatcher_terminal_guard_test.go，全部通过（go test ./feishufront/ ok）。
- 沉淀 L2/patterns/card-terminal-state-guard.md。
