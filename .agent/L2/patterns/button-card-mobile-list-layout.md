---
layer: L2
type: pattern
tags: [feishu, cardkit, picker, mobile-layout]
created: 2026-08-10
confidence: high
verified_at: 2026-08-13
applies_to: b965415
---

# 飞书按钮卡移动端布局：长标签必须纵向单列

## 现象
`/send` 目录浏览卡在手机上按钮文字全部变成省略号，文件/目录名不可读。PC 端正常。

## 根因
不是代码截断（`truncateRunes` 上限 4000 runes，文件名碰不到），而是**飞书客户端对 `column_set` 窄列内按钮文本做的 UI 级省略**。卡片按钮默认走 `actionColumnSets(actions, 5)` 每行 5 列等宽（`weight:1`），手机卡片宽约 350pt → 每列仅 ~65pt，emoji+文件名必然被截断。列数越多、屏幕越窄，截断越早。

## 模式
1. **按钮卡布局按选项内容选择列数**：短标签（是/否、模型名）用 5 列网格保持紧凑；文件名/路径/长标签用 `CardWithColumns(maxCols=1)` 纵向单列全宽。
2. **启发式判断放在 renderer**（`questionButtonColumns`）：含 `📁 `/`📄 ` 前缀、含 `/`、或 > `questionButtonRunesPerRow` runes 即判为长标签 → 返回 1 列。无需改协议，一个入口覆盖所有后端。
3. **阈值从已知案例标定**：backend picker 的长标签约 19 runes 是已知最短会截断的案例（`cardkit.CardWithColumns` 注释记录的历史案例），阈值取 16 留余量。
4. **项目已有先例**：`CardWithColumns` 的 maxCols 参数就是为长标签 picker 设计的，但只有 backend picker 用了；新 picker 默认 5 列是漏网之鱼。改通用入口比逐个调用点指定更防漏。

## 测试要点
- 断言每行 `column_set` 的列数（`columnSetColumnCounts`），而不是断言文案——布局才是被修复的行为。
- 必须同时 pin 两个方向：长选项 → 单列；短选项 → 仍为 5 列（防回归把紧凑卡片撑成列表）。

## 参考
- 实现：`internal/feishufront/renderer/interactive.go` `questionButtonColumns`（返回列数 0=默认5列 / 1=单列）
- 布局机制：`internal/feishufront/cardkit/cardkit.go` `actionColumnSets` / `CardWithColumns`
- 相关事故：[feishu-card-bounce-back.md](../incidents/feishu-card-bounce-back.md)（不要为长列表改回 select_static 下拉，会重引入回弹 callback 窗口）
