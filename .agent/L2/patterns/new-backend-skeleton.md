---
layer: L2
type: pattern
tags: [backend, new-backend, deploy, config, http-backend, skeleton]
created: 2026-08-10
confidence: high
---

# 新后端搭建标准骨架

适用于「调外部 API、不 fork 子进程、SSE 注册 + POST 发 Control」的轻量后端
（参考 status-monitor）。CLI 子进程型后端
（miniagent）骨架不同，不在此列。

## 五层结构（自下而上）

| # | 层 | 文件 | 做什么 |
|---|---|---|---|
| 1 | config 段 | `internal/config/config.go` + `config_defaults.go` + `config_validate.go` | 新增 `XxxBack` struct + `Config` 字段（`json:"xxx,omitempty"`）+ `ComponentLogLevel` 字段 + applyDefaults + validate |
| 2 | config 模板 | `config.example.json` + `deploy/env.example` | 补配置段 + `${VAR}` 占位 + env.example 变量说明 |
| 3 | 业务包 | `internal/xxxback/` | `client.go`（HTTP 客户端）+ `handler.go`（SSE 事件分发 + 异步执行）+ `prompt.go`（固定提示词） |
| 4 | 入口 | `cmd/xxx-back/main.go`（+ `main_test.go`） | 薄入口：`config.Load → logger → backendrpc.ValidateBackendConfig → Connect → NewHandler → RunWithClient` |
| 5 | 部署 | `deploy/deploy-xxx.sh` + `Makefile`（`build-xxx-back` / `deploy-xxx` 目标） + `deploy/tests/smoke.sh`（source guard） | systemd unit + 首次安装 / 升级 |

## 关键约束

1. **固定 BackendToken + RunWithClient**（policies#4）：进程级钉扎一个 token，避免 SSE 握手注册的 token 与 POST token 互异被前端 403。
2. **慢任务跑 GoSafe**：不阻塞 SSE 事件循环；任务自带 `jobTimeout` ctx（`context.Background()` 根，不随 SSE ctx 取消）。
3. **config 缺失项 fail-fast**：API key 等必填项在 main.go 的 `run()` 开头校验，缺则 return error（main 提升为 os.Exit(1)）。
4. **BackendType 在 registry 无白名单**：前端 `Register(id, typ)` 接受任意类型字符串，不需改前端代码。
5. **deploy 脚本 init 路径须自带 build**：deploy-monitor/deploy-status 的 init 依赖外部预 build，但健壮做法是 init 开头调 `build_xxx()`。
6. **config `${VAR}` 展开在二进制启动时**：systemd `EnvironmentFile=/etc/lark-bridge/.env` 注入环境变量，`config.Load` 展开 `${VAR}`。init 路径只写 config 文件，env 变量需另行补到 `.env`。

## 常见陷阱

- **`.env` 注释行**：systemd EnvironmentFile 不容忍 `#` 注释行混在 `KEY=VALUE` 之间（会导致解析异常）。实测注释行在文件开头/末尾 OK。
- **wait_active 冷启动窗口**：systemd restart 后服务可能 crash-loop（RestartSec=5），`wait_active`（15s 轮询）可能误判。首次 `--init` 后手动 restart 一次可绕过。
- **文档与实际 API 差异**：实测验证优于文档信任（历史案例：agnes video url 文档写 `metadata.url`，实际在顶层 `url` 字段）。

## 参考
- 完整实现（轻量 HTTP 后端范式）：status-monitor（`cmd/status-monitor/`、`internal/statusmonitor/`、`deploy/deploy-status.sh`）
- 轻量后端模板：`cmd/status-monitor/main.go`（最薄）
