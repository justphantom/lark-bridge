---
layer: L2
type: pattern
tags: [agnes-ai, image-generation, video-generation, prompt-engineering, async-polling]
created: 2026-08-10
verified_at: 2026-08-12
applies_to: pre-89e29e6
status: historical
confidence: high
---

# Agnes AI API 对接经验

> ⚠️ **历史参考**：agnes-back 已于 `89e29e6` 彻底移除（C 档全量清零）。以下为
> 再对接资产保留。引用的代码路径 `internal/agnesback/` 已删除。

对接 Agnes AI 三个模型（agnes-2.5-flash / agnes-image-2.1-flash /
agnes-video-v2.0）的实操经验，含 API 细节差异与踩坑记录。参考实现：
`internal/agnesback/client.go`。

## 三个模型的调用方式

### 1. agnes-2.5-flash（提示词生成）

- **端点**：`POST /v1/chat/completions`（OpenAI 兼容）
- **用途**：把用户的简短描述扩写为完整英文提示词
- **结构**：`messages[{role:"system",...},{role:"user",...}]`，system prompt
  嵌入文档推荐的提示词结构（图片 `[主体]+[场景]+[风格]+[光照]+[构图]`，
  视频 `[主体]+[动作]+[场景]+[镜头运动]+[光线]+[风格]`）
- **响应**：`choices[0].message.content`，标准 OpenAI 格式，无坑

### 2. agnes-image-2.1-flash（图片生成）

- **端点**：`POST /v1/images/generations`
- **关键参数**：`model` + `prompt` + `size`（档位 `1K/2K/3K/4K`）+ `ratio`
  （宽高比）+ `extra_body.response_format`（`url` 或 `b64_json`）
- **size 配 ratio**：文档推荐用档位式 `size: "2K"` + `ratio: "16:9"` 而非
  精确尺寸 `1920x1080`（后者会被标准化映射）
- **response_format 必须在 extra_body 内**：放顶层会被拒绝
- **响应路径**：`data[0].url`（URL 输出）或 `data[0].b64_json`（Base64）
- **超时建议**：60s-360s（文档）；实测 1K 图约 5-10s
- **坑**：图片 CDN（`platform-outputs.agnes-ai.space`）下载偶发 TLS reset，
  属网络环境问题非代码 bug，需重试

### 3. agnes-video-v2.0（视频生成，异步）

- **创建端点**：`POST /v1/videos`
- **查询端点（推荐）**：`GET /agnesapi?video_id=<VIDEO_ID>`
- **查询端点（兼容）**：`GET /v1/videos/<TASK_ID>`
- **异步流程**：创建任务 → 拿 `video_id` → 轮询直到 `completed`
- **num_frames 规则**：≤441 且 `8n+1`（81→3s, 121→5s, 241→10s, 441→18s）
- **轮询间隔**：10s 合适；超时 5min 够 81 帧

## 踩过的坑

### 坑 1：video URL 字段路径（文档 vs 实际）

**文档说**：完成后的 URL 在 `metadata.url`。
**实际返回**：`metadata` 是空对象 `{}`，URL 在**顶层 `url` 字段**。

```json
// 实际响应
{"status":"completed","url":"https://cos-platform-outputs.../xxx.mp4","metadata":{}}
```

**修复**：`url = metadata.url; if url == "" { url = top_level_url }`。
参考：`client.go` queryVideo 的 fallback 逻辑。

### 坑 2：video_id 很长且含特殊字符

`video_id` 是 base64 编码的长字符串（如
`video_bGl0ZWxsbTpjdXN0b21...`），手动复制易截断/打错（`bGxt` vs `bGlt`）。
**教训**：不要手动复制 video_id 做调试，用变量传递。

### 坑 3：video 创建响应 vs 查询响应的 ID 不一致

创建响应同时返回 `id` / `task_id` / `video_id`，三者可能不同。
文档推荐用 `video_id` 查询，实测可靠。

## 图片交付策略（飞书群）

- **方案选择**：image API 返回 URL → 后端下载字节 → `TypeFile` Control
  （base64）→ 前端上传发到群（图片内联可见）
- **限制**：飞书 30 MiB 上限。大图（4K+）可能超限 → client 内做 size 校验
- **video 交付**：URL 放 Notice 文本（不下载，视频大且 CDN 可能 reset）

## 参考
- 实现代码：`internal/agnesback/client.go`、`internal/agnesback/handler.go`
- API 文档：`agnes-25-flash` / `agnes-image-21-flash` / `agnes-video-v20`
- 提交：`08e3033`（初版）、`296ec8f`（video url 诊断）
