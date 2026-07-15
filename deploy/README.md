# lark-bridge 部署指南

## 架构

```
飞书用户 ←→ 飞书开放平台 ←→ feishu-front (WS Bot + IPC SSE)
                                    ↕ SSE/POST (Bearer 鉴权)
                    ┌───────────────┴───────────────┐
                claude-back                    opencode-back
                (Claude CLI)                   (opencode CLI)
```

三个独立进程，共享一份配置文件和 `ipc_secret`。

## 前置条件

| 组件 | 要求 |
|------|------|
| Go | 1.25+ |
| Claude CLI | `claude` 在 PATH 中（仅 claude-back） |
| opencode | `opencode` CLI 在 PATH 中（仅 opencode-back） |
| 飞书应用 | 自建应用，开启机器人能力，添加 IM 权限 |

## 1. 构建

```bash
make build
# 产物：bin/lark-feishu-front, bin/lark-claude-back, bin/lark-opencode-back
```

## 2. 准备配置

```bash
# 环境变量（机密，不入配置文件）
cp deploy/env.example .env
# 编辑 .env，填入真实凭证
# 生成 IPC_SECRET：openssl rand -hex 32

# 方式 A：单文件（三个进程共享，各自只读需要的字段）
cp config.example.json claude-config.json
# 编辑 backend_id / frontend_url / state_dir
# feishu/opencode 各自再复制一份（或直接共用 claude-config.json）

# 方式 B：分文件（deploy/ 下的示例模板）
# deploy/feishu-config.json   — 飞书凭证 + ipc_secret + state_dir
# deploy/claude-config.json   — backend_id + frontend_url + claude 配置
# deploy/opencode-config.json — backend_id + frontend_url + opencode 配置
```

## 3. 创建 state 目录

```bash
mkdir -p /var/lib/lark-bridge/claude /var/lib/lark-bridge/opencode
```

## 4. 启动

```bash
# 加载环境变量
set -a; source .env; set +a

# 前端（先启动）
./bin/lark-feishu-front \
  -config feishu-config.json &

# Claude 后端
./bin/lark-claude-back -config claude-config.json &

# opencode 后端（可选）
./bin/lark-opencode-back -config opencode-config.json &
```

新群首次发消息时会提示"未绑定后端"，需用户发送 `/backend use {id}` 绑定。

## 5. 配置字段说明

### 必填

| 字段 | 谁需要 | 说明 |
|------|--------|------|
| `feishu_app_id` | feishu-front | 飞书应用 App ID |
| `feishu_app_secret` | feishu-front | 飞书应用 App Secret |
| `ipc_secret` | 三者 | IPC 共享密钥，必须一致；留空拒绝启动 |
| `backend_id` | 后端 | 在前端 registry 的唯一标识 |
| `frontend_url` | 后端 | 前端 IPC 地址 |
| `claude.default_directory` | claude-back | 每个群的工作目录基路径 |
| `opencode.default_directory` | opencode-back | 每个群的工作目录基路径 |

### 机密字段

用 `${VAR}` 语法引用环境变量，不直接写在 JSON 里：

```json
{ "ipc_secret": "${IPC_SECRET}" }
```

`config.Load` 会展开 `${VAR}`，未设置或空值时报错退出。

### 有默认值可省略的字段

| 字段 | 默认值 |
|------|--------|
| `log_level` | `info` |
| `log_output` | `stderr` |
| `log_format` | `text` |
| `log_debug_redact` | `false` |
| `state_dir` | 配置文件所在目录 |
| `claude.cli_path` | `claude` |
| `claude.permission_mode` | `acceptEdits` |
| `claude.max_concurrent` | `4` |
| `claude.stream_history` | `50` |
| `opencode.cli_path` | `opencode` |
| `opencode.max_concurrent` | `4` |
| `opencode.stream_history` | `50` |
| `opencode.list_cache_ttl` | `3600` |
| `timeouts.backend_health` | `90s` |
| `timeouts.prompt_timeout` | `0`（禁用） |
| `component_log_levels` | `{}`（当前仅 opencode-back 生效） |
| `dedup.stale_window` | `300s` |
| `dedup.event_ttl` | `5m` |
| `dedup.event_max_entries` | `1000` |

完整默认值见 `internal/config/config_defaults.go`。

### permission_mode

| 值 | 行为 |
|----|------|
| `acceptEdits` | 自动放行文件编辑（默认） |
| `plan` | 只读，不修改文件 |
| `bypassPermissions` | 跳过所有权限检查 |
| ~~`default`~~ | **不可用**——非交互模式会卡死 |

## 6. systemd 部署

每个进程一个 unit，共用 `EnvironmentFile`：

```ini
# /etc/systemd/system/lark-feishu-front.service
[Unit]
Description=lark-bridge lark-feishu-front
After=network.target

[Service]
EnvironmentFile=/etc/lark-bridge/.env
ExecStart=/opt/lark-bridge/bin/lark-feishu-front \
  -config /etc/lark-bridge/feishu-config.json
Restart=on-failure
User=user

[Install]
WantedBy=multi-user.target
```

```ini
# /etc/systemd/system/lark-claude-back.service
[Unit]
Description=lark-bridge lark-claude-back
After=lark-feishu-front.service

[Service]
EnvironmentFile=/etc/lark-bridge/.env
ExecStart=/opt/lark-bridge/bin/lark-claude-back \
  -config /etc/lark-bridge/claude-config.json
Restart=on-failure
User=user

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now lark-feishu-front lark-claude-back
```

## 7. 验证

```bash
# 前端健康：IPC 监听
curl -s localhost:6060/v1/events  # 应返回 401（鉴权拦截）

# 日志
journalctl -u lark-feishu-front -f
journalctl -u lark-claude-back -f

# 在飞书群里 @机器人 发消息，观察日志输出
```
