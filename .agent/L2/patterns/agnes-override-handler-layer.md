---
layer: L2
type: pattern
tags: [agnes-back, override, handler-state, apiclient-interface, testability]
created: 2026-08-10
confidence: high
---

# agnes-back 运行时覆盖状态放 Handler 层（而非 Client）

agnes-back 的 `/model` 运行时覆盖（override）状态保存在 `Handler` 的
`modelMu + chat/image/video` 字段中，而不是放进 `Client`。原因与适用条件
如下。

## 为什么不放 Client

1. **APIClient 接口约束**：`Handler.client` 是 `APIClient` 接口
   （`internal/agnesback/handler.go`），只声明三个生成方法。给 Client 加
   `SetModel/EffectiveModels` 会要求改接口 + 所有 mock（handler_test.go 的
   `fakeClient`），波及面大。
2. **配置列表属 Handler 职责**：picker 的选项来自 `config.AgnesBack` 的
   `chat_models/image_models/video_models`，`FromConfig` 已把整份 Config 留在
   Handler，无需再向 ClientConfig 透传列表字段。
3. **override 是表现层概念**：「某群把 image 模型临时切到 X」属于指令层状态；
   Client 保持「给定配置就按配置调 API」的纯粹性。

## Handler 层 override 的标准形状

```go
// Handler 内
modelMu   sync.RWMutex
chatModel, imageModel, videoModel string // "" = 用配置值

// 生成路径每次调用时解析，不缓存快照
func (h *Handler) effectiveChatModel() string {
    h.modelMu.RLock(); defer h.modelMu.RUnlock()
    if h.chatModel != "" { return h.chatModel }
    return h.cfg.ChatModel
}
```

- 进程级生效（不按 chat 隔离）：agnes 后端无 per-chat 状态，与配置语义一致。
- 重启回落配置值，文案需注明「进程级生效，重启后回落配置文件值」。

## 适用条件

后端满足以下任一条件时，优先考虑 Handler 层而非 Client 层：
- client 以接口注入且有 mock 实现；
- override 选项依赖配置中的列表字段；
- override 语义是指令层临时状态而非 API 调用参数。

## 参考
- 实现：`internal/agnesback/handler.go`（modelMu/effective*）、`picker.go`
- 接口：`internal/agnesback/handler.go` APIClient 定义
