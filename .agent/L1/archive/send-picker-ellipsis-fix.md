---
updated: 2026-08-10T19:15:00+08:00
---

# /send 弹卡省略号修复（方案A：纵向单列布局）

## 状态
已完成。build/vet/test 全过（1449 passed, 37 packages）。

## 背景
手机上 /send 目录浏览卡的按钮文件名被飞书 UI 级省略号截断（非代码截断）：
- 链路：bridgebase.BuildSendOptions → renderer.RenderQuestionButtons → cardkit.actionColumnSets 默认每行 5 列
- 手机卡片窄，5 列每列仅 ~65pt，飞书客户端自动 ellipsis

## 改动（仅 2 文件，+96 行）
1. `internal/feishufront/renderer/interactive.go`
   - 新增 `questionButtonListLayoutMaxRunes = 16` + `questionButtonsNeedListLayout()` 启发式
   - 命中条件：选项含 `📁 `/`📄 ` 前缀、含 `/`、或任一选项 > 16 runes
   - 命中时 `CardWithColumns(..., 1)` 纵向单列；否则保持默认 5 列
2. `internal/feishufront/renderer/renderer_test.go`
   - 新增 `columnSetColumnCounts` 辅助 + `TestRenderInteractive_QuestionButtonsListLayout`（4 子用例）+ `TestRenderInteractive_QuestionButtonsGridLayout`（钉住 7 短选项仍为 [5 2]）

## 未动
- `internal/agnesback/{handler,picker}.go`：gofmt/errcheck 问题为基线已存在（工作区与 HEAD 一致），不属本任务
- /model、/backend、permission 卡不受影响（短选项仍 5 列）

## 待办
- 无（用户未要求提交；如需提交请告知）
