---
layer: L1
type: session
updated: 2026-08-10T00:45:00+08:00
---

# 当前会话

## 最近聚焦
- **修复 schema 2.0 交互卡片回调失效**（e223940）：b214834 全量切 CardKit 后，button 的 `behaviors:[{type:"callback"}]` 对 inline 卡片**抑制** `card.action.trigger` 回调。修复：移除 behaviors（依赖 button value 触发回调）；交互卡片改用 `SendCardInline`（inline JSON），通知/进度卡保持 CardKit 实体。picker 改用 `CardWithColumns(maxCols=1)` 修复省略号。实测确认 callback 恢复。
- **agnes-back 后端**（08e3033）：Agnes AI 图片/视频生成后端已部署，6 服务全部 active。

## 已知限制
- **form_submit 未实测**：SubmitButtonAction 也移除了 behaviors，理论上 value+action_type 触发回调，但未实测。permission/question 用 ButtonAction 已验证 OK。
- **图片下载偶发 reset**：agnes image CDN TLS 偶发 reset（非代码问题）。
