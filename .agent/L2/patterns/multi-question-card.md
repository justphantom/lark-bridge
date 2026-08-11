---
layer: L2
type: pattern
tags: [question-card, multi-question, picker, custom-input, choices-mapping]
created: 2026-08-10
confidence: high
---

# 多问题 Question 卡的 Choices 映射与 Custom 全局单串限制

一张 Question 卡可携带多个 `QuestionItem`（`Questions []QuestionItem`，上限
15 问，`feishufront/renderer/interactive.go` `maxQuestions`），提交后答案的
映射规则与自定义输入的局限如下。

## Choices[i] ↔ Questions[i]

前端表单解析（`feishufront/form.go` `parseQuestionFormValue`）按问题索引
`q_0/q_1/...` 排序后拼接：`Choices[i]` 对应第 i 个问题的选择值。后端按索引
逐项消费即可，无需 requestID 以外的关联信息。参考：
`internal/agnesback/picker.go`（agnes-back，已移除）runModelPicker 的三槽位应用循环——历史实例，抽象仍由 `protocol.TypeQuestion` 共享。

## Custom 是全局单串，多问题时不可用

`AnswerPayload.Custom string` 把所有 `custom_<idx>` 输入框的值用 `\n` 拼成
**一个串**（form.go:48-53）。多问题卡片若给多个问题都开 `Custom: true`，后端
无法区分哪个输入属于哪一问。

**结论**：多问题卡一律不开 `Custom`；列表外值通过「直设文本命令」兜底（如
`/model <slot> <型号>`）。单问题卡（claude/miniagent `/model`）不受影响，
可以安全开 `Custom`。

## 单问单选 ≤N 项会退化为按钮渲染

`canRenderQuestionAsButtons`（renderer/interactive.go）：恰好一个问题、单选、
无自定义输入、选项数在卡片元素预算内时，前端渲染为立即点击的按钮而非下拉
表单。多问题卡（≥2 问）固定走下拉表单路径。

## 参考
- 解析逻辑：`internal/feishufront/form.go`
- 渲染分支：`internal/feishufront/renderer/interactive.go`
- 多问题实例（历史）：`internal/agnesback/picker.go`（agnes-back /model 三槽位单卡，已移除）
