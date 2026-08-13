---
layer: L1
type: task
status: done
created: 2026-08-10
updated: 2026-08-10T12:20:00+08:00
task: agnes-model-command
---

# agnes-back /model 指令

## 需求
实现 doc 注释中的待办指令 `/model`，采用单卡三问弹卡 + 配置化模型列表。

## 关键决策
- 弹卡形态：一张 Question 卡含三个问题（chat/image/video 槽位），前端 `parseQuestionFormValue` 按索引拼接 Choices[i] 对应槽位 i。
- 不开 `Custom`：`AnswerPayload.Custom` 是全局单串（多问题 custom 用 `\n` 拼接），无法归属到具体槽位；列表外型号走直设，且直设受配置列表校验（`knownModel`）。
- 模型列表来自配置 `agnes.chat_models/image_models/video_models`，空列表回落为 `[chat_model]` 等单元素列表（config_defaults.go）。
- override 状态放 Handler（非 Client）：`APIClient` 接口 + 测试 mock 不感知 override，Handler 在调用生成方法时注入生效值；进程级生效（不按 chat 隔离），重启回落配置。
- TypeAnswer 事件此前被 agnes-back 丢弃，本次新增分发到 `bridgebase.AnswerBroker`。
- 选项首项为「保持不变（当前：X）」兜底；picker 提交后原地 patch（NoticePayload.UpdateMessageID = ans.MessageID）。
- 每群同时只允许一个 picker（pickerSlots map + TryLock）。

## 改动文件
- internal/config/config.go、config_defaults.go、config_test.go
- config.example.json
- internal/agnesback/client.go（三个生成方法签名加 model 参数；New 改为 NewWithHTTP）
- internal/agnesback/handler.go（TypeAnswer 分发、/model 直设与弹卡分发、override 状态、helpText）
- internal/agnesback/picker.go（新）
- internal/agnesback/handler_test.go

## 验证
- go test ./... 全量 1443 通过（37 包）。
