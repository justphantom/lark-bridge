# lark-bridge 部署指南

## 架构

```
飞书用户 ←→ 飞书开放平台 ←→ feishu-front (WS Bot + IPC SSE)
                                    ↕ SSE/POST (Bearer 鉴权)
        ┌───────────┬───────────┬──────────────┐    ┌──────────────┐
   claude-back  opencode-back  miniagent-back      deploy-monitor
   (Claude CLI) (opencode CLI) (LLM API 直调)      (make deploy)
                                                    ↑ 独立部署
```

前端 feishu-front + 三个 agent 后端（claude/opencode/miniagent）由 `make deploy`
管理（4 个 systemd 服务）。deploy-monitor 是部署触发者，**独立管理**（`make
upgrade-monitor`），避免「部署脚本管自己的触发者」循环依赖。

## 前置条件

| 组件 | 要求 |
|------|------|
| Go | 1.25+ |
| Claude CLI | `claude` 在 PATH 中（仅 claude-back） |
| opencode | `opencode` CLI 在 PATH 中（仅 opencode-back） |
| miniagent | OpenAI 兼容 endpoint 的 API key（stateless，无 sessions/memory；见 .env） |
| 飞书应用 | 自建应用，开启机器人能力，添加 IM 权限 |

## 1. 构建

```bash
make build
# 产物：bin/lark-feishu-front, bin/lark-claude-back, bin/lark-opencode-back,
#       bin/lark-miniagent-back, bin/miniagent（deploy-monitor 由 upgrade-monitor.sh 单独构建）
# miniagent 是 miniagent-back 的子进程：每个 prompt fork 一次，跑完退出。
# 类比 claude CLI 被 claude-back fork 的模式。
```

## 2. 准备配置

```bash
# 环境变量（机密，不入配置文件）
cp deploy/env.example .env
# 编辑 .env，填入真实凭证
# 生成 IPC_SECRET：openssl rand -hex 32

# 方式 A：单文件（进程共享，各自只读需要的字段）
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

# miniagent 后端（可选）
./bin/lark-miniagent-back -config miniagent-config.json &
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
| `miniagent.api_key` | miniagent-back | OpenAI 兼容 endpoint 的 API key（stateless，无 sessions/memory；`${MINIAGENT_API_KEY}`） |

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
sudo systemctl enable --now lark-feishu-front lark-claude-back lark-opencode-back lark-miniagent-back
```

## 6.5. deploy-monitor 部署（独立）

deploy-monitor 是「部署触发者」（收到飞书群 `/deploy` → `make deploy`），
**不由 deploy.sh 管理**，避免循环依赖。它有自己的部署脚本：

```bash
# 首次安装（生成 config + unit + enable + start）
make upgrade-monitor ARGS=--init

# 后续升级（构建 + 替换二进制 + restart，~2s 离线）
make upgrade-monitor
```

升级时 monitor 短暂离线（systemd restart 期间无法响应 `/deploy`）。monitor 代码
极少变更，这个代价可接受。

## 7. 验证

```bash
# 前端健康：IPC 监听
curl -s localhost:6060/v1/events  # 应返回 401（鉴权拦截）

# 日志
journalctl -u lark-feishu-front -f
journalctl -u lark-claude-back -f
journalctl -u lark-opencode-back -f
journalctl -u lark-miniagent-back -f

# 在飞书群里 @机器人 发消息，观察日志输出
```

## 8. 二进制分发与灵活部署

deploy.sh 支持三种正交维度，组合使用：

- `--binaries <tar|dir>`：从已编译产物部署，目标机无需 Go/repo。
- `--services <list>`：只部署服务子集（逗号分隔：`feishu claude opencode miniagent`）。
- `--init` / `--force`：首次生成配置 / 跳过运行中会话检查。

### 8.1 打包分发（编译与部署解耦）

```bash
# 构建机（有 Go + repo）
make pack                          # 本机平台
make pack GOOS=linux GOARCH=arm64  # 交叉编译
# 产物：bin/lark-bridge-<ver>-<os>-<arch>.tar.gz，含 5 个二进制 + VERSION
#       + config.example.json + env.example（供 --init 首次部署）

# 分发到目标机
scp bin/lark-bridge-*.tar.gz host:/tmp/
scp -r deploy host:/opt/lark-bridge/   # deploy.sh 本身不在 tarball 内

# 目标机（免 Go / 免 repo）
cd /opt/lark-bridge
./deploy/deploy.sh --init --binaries /tmp/lark-bridge-*.tar.gz
```

### 8.2 部分部署

```bash
# 只更新 claude 后端（其余服务不动）
./deploy/deploy.sh --binaries /tmp/xxx.tar.gz --services claude

# 前端机只装前端
./deploy/deploy.sh --init --binaries /tmp/xxx.tar.gz --services feishu
```

### 8.3 多主机分布式部署

前后端分机部署：前端机跑 feishu-front（持有飞书长连接 + IPC server），backend 机跑 CLI 后端，通过 IPC 连前端。代码层无需改动——`ipc_addr` 经标准 `ListenAndServe` 监听，`frontend_url` 是 backend 拨号地址。

```bash
# ── 前端机（192.168.1.10）──────────────────────────
# .env: IPC_ADDR 监听非 loopback；FRONTEND_URL 留空（前端不用）
IPC_ADDR=0.0.0.0:6060
./deploy/deploy.sh --binaries /tmp/xxx.tar.gz --services feishu

# ── backend 机（192.168.1.20）──────────────────────
# .env: FRONTEND_URL 指前端机；IPC_ADDR 本机无关（backend 不监听）
FRONTEND_URL=http://192.168.1.10:6060
./deploy/deploy.sh --binaries /tmp/xxx.tar.gz --services claude,opencode
```

要点：
- `IPC_SECRET` 三机必须一致（鉴权共享）。
- IPC 为明文 HTTP，跨机仅限可信内网；跨不可信网络请走 SSH 隧道或 wireguard。
- `state_dir` 各机独立（会话绑定经 router_path 文件，前后端同机时才共享；分机时 router 文件随前端）。

