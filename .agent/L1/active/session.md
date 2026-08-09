---
layer: L1
type: session
updated: 2026-08-10T00:05:00+08:00
---

# 当前会话

## 当前任务
无（已完成 agnes-back 后端 + 部署 + 实测）

## 最近聚焦
- **新增 agnes-back 后端**：实现 Agnes AI 图片/视频生成指令。仿 deploy-monitor / status-monitor 的轻量后端形态（SSE 注册 + POST 发 Control），不 fork 子进程，直接用 net/http 调 Agnes API。固定 BackendToken + RunWithClient（policies#4）。四条指令：`/image-prompt`、`/image`、`/video-prompt`、`/video`、`/agnes-help`。
- **部署脚本**：`deploy/deploy-agnes.sh`（独立管理，仿 deploy-status.sh）。Makefile `deploy-agnes` 目标；smoke 34 passed。
- **实测部署**（2026-08-09 23:40）：`make deploy-agnes ARGS=--init` 成功，agnes-1 注册到前端 registry，6 个服务全部 active。
- **实测修复**：发现 Agnes video API 实际把 url 放在顶层 `url` 字段（文档写的是 `metadata.url`，实测 `metadata` 为空 `{}`）。已在 `client.go` queryVideo 加 fallback：优先 metadata.url，回退顶层 url。已补测试 `TestGenerateVideo_TopLevelURL`。
- **API 验证**：image API key 有效（返回 CDN url）；prompt 生成完美（中文→英文专业提示词）；video 完整轮询链路 queued→in_progress→completed，成功拿到 mp4 url。
- go build/vet/test ./... 全绿 + deploy-smoke 34 passed。

## 已知限制（实测发现）
- **图片下载偶发 connection reset**：`platform-outputs.agnes-ai.space` CDN 的 TLS 在本机网络环境偶发被 reset（非代码问题）。video CDN（cos-platform-outputs）正常。
- **飞书群未绑定 agnes-1**：需在飞书群发 `/backend` 选 agnes-1 绑定后，指令才在该群生效（routing.json 无热加载，需通过 /backend 指令或重启 feishu-front）。
