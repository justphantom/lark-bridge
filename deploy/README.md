# lark-bridge 部署指南

## 架构

```
飞书用户 ←→ 飞书开放平台 ←→ feishu-front (WS Bot + IPC SSE)
                                    ↕ SSE/POST (Bearer 鉴权)
   ┌──────────┬──────────┬──────────┬──────────────────┬──────────────┐
 claude-back opencode-back omp-back miniagent-back       deploy-monitor
 (Claude CLI)(opencode CLI)(omp CLI)(LLM API 直调)       (make deploy)
                                                                ↑ 独立部署
```

前端 feishu-front + 四个 agent 后端（claude/opencode/omp/miniagent）由 `make deploy`
管理（默认 5 个 systemd 服务）。deploy-monitor 是部署触发者，**独立管理**
（`make upgrade-monitor`），避免「部署脚本管自己的触发者」循环依赖。

## 前置条件

| 组件 | 要求 |
|------|------|
| Go | 1.25+ |
| Claude CLI | `claude` 在 PATH 中（仅 claude-back） |
| opencode | `opencode` CLI 在 PATH 中（仅 opencode-back） |
| omp | `omp` CLI 在 PATH 中（仅 omp-back；对接 Oh My Pi，对接规范见 `OMP_INTEGRATION_SPEC.md`） |
| miniagent | OpenAI 兼容 endpoint 的 API key（stateless，无 sessions/memory；见 .env） |
| 飞书应用 | 自建应用，开启机器人能力，添加 IM 权限 |

文件上传（docx/pptx/xlsx → Markdown）由内置纯 Go 解析器完成，**无外部运行时依赖**。

### 飞书应用权限（启用文件上传时）

接收并下载群聊中上传的文件，需要在飞书开发者后台为自建应用追加以下权限并
经管理员审批后生效：

- `im:resource`（读取消息中的文件 / 图片资源）— 必需
- `im:message`（接收消息，机器人默认已开）

缺权限时下载请求会返回 403 / 业务码 99991672，前端按 `下载失败` 通知用户，
不影响文本对话。

### 提示词模板（启用文件上传时）

`file_convert.prompt_template` 是发送给 agent 的 prompt 文本模板（Go
`text/template` 语法），**必填**。代码层不内置默认文案，默认模板写在
`config.example.json` 里，操作员可直接编辑。

可用变量：

| 变量 | 含义 |
|------|------|
| `{{.FileName}}` | 用户上传的原始文件名（如 `notes.md`） |
| `{{.Path}}` | 转换后的 `.md` 在主机上的绝对路径 |
| `{{.UserText}}` | 上传消息附带的文本（无则为空字符串） |

需要"无附加说明时不渲染该段"，用 `{{if .UserText}}…{{end}}`。模板语法错误会在
`feishu-front` 启动时立即报错（fail-fast）。

### 富文本消息（post）

`file_convert.post_prompt_template` 是发送给 agent 的富文本消息（`MsgType=post`）
prompt 模板。**可选**：留空时 post 消息降级为纯 Markdown 文本路径（图片渲染为
`[图片]` 占位符，不下载）；填上则启用完整 post 管线：

- 解析 post AST（text / a / at / img / media / emotion / code_block）
- 内联图片（`tag=img`）下载到 inbox，扩展名按响应 `Content-Type` 推断
- `body.md` 写到 `{inbox}/{chatID}/{promptID}/`，图片用 `![](abs/path)` 引用
- 视频（`tag=media`）渲染为 `[视频]` 占位符（agent 读不了视频，不下载）
- 单图失败转占位符（`[图片下载失败]` / `[图片过大]`），整条消息不阻塞

可用变量：

| 变量 | 含义 |
|------|------|
| `{{.Path}}` | `body.md` 绝对路径（agent 的 Read 入口） |
| `{{.UserText}}` | 附带文本（post 本身就是内容，一般为空） |

`@bot` / `@all` 在解析阶段被剔除（与文本消息的 `StripMentionPlaceholders`
语义一致），普通 `@用户` 保留为 `@<name>`。

默认模板见 `config.example.json`，操作员可直接编辑换语言、改措辞。

## 1. 构建

```bash
make build
# 产物（7 个二进制）：
#   bin/lark-feishu-front, bin/lark-claude-back, bin/lark-opencode-back,
#   bin/lark-omp-back, bin/lark-miniagent-back, bin/lark-deploy-monitor,
#   bin/lark-status-monitor.
# miniagent 是 miniagent-back fork 的子进程（独立项目，不在本仓库 make build 范围内）：
# 每个 prompt fork 一次，跑完退出。类比 claude CLI 被 claude-back fork 的模式。
```

## 2. 准备配置

```bash
# 环境变量（机密，不入配置文件）
cp deploy/env.example .env
# 编辑 .env，填入真实凭证
# 生成 IPC_SECRET：openssl rand -hex 32

# 单文件派生：config.example.json 是唯一真源（deploy.sh / upgrade-*.sh
# 均从此派生运行时 config）。手动启动也可直接共用 config.example.json，
# 或复制成每服务一份按需裁剪。
cp config.example.json claude-config.json
# 编辑 backend_id / frontend_url / state_dir
# feishu/opencode/omp/miniagent 各自再复制一份（或直接共用 claude-config.json）
```

## 3. 创建 state 目录

```bash
mkdir -p /var/lib/lark-bridge/claude /var/lib/lark-bridge/opencode /var/lib/lark-bridge/omp
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

# omp 后端（可选）
./bin/lark-omp-back -config omp-config.json &

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
| `omp.default_directory` | omp-back | 每个群的工作目录基路径 |
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
| `omp.cli_path` | `omp` |
| `omp.max_concurrent` | `4` |
| `omp.stream_history` | `50` |
| `omp.approval_mode` | `write` |
| `omp.thinking_level` | `auto` |
| `omp.model_list_timeout` | `300s` |
| `omp.list_cache_ttl` | `3600` |
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
sudo systemctl enable --now lark-feishu-front lark-claude-back lark-opencode-back lark-omp-back lark-miniagent-back
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

运行模式 `LARK_RUN_MODE` 控制是否部署 deploy-monitor：

| 模式 | 行为 |
|------|------|
| `dev`（默认） | 正常安装/升级 deploy-monitor |
| `pro` | `make upgrade-monitor` 为 no-op，不部署 deploy-monitor；若从 dev 切换而来，会停用已有的 unit |

配置源优先级：环境变量 `LARK_RUN_MODE` > repo 根 `.env` > 默认 `dev`。

升级时 monitor 短暂离线（systemd restart 期间无法响应 `/deploy`）。monitor 代码
极少变更，这个代价可接受。

## 6.6. status-monitor 部署（独立）

status-monitor 是「观察者」（每 `status_monitor.interval` 秒向绑定的群推送一张
在线后端 + 运行中会话总览卡），无副作用、不需提权，**同样不由 deploy.sh 管理**，
与 deploy-monitor 同模式独立部署：

```bash
# 首次安装（生成 config + unit + enable + start）
make upgrade-status ARGS=--init

# 后续升级（构建 + 替换二进制 + restart，~2s 离线）
make upgrade-status
```

部署后在飞书群里 `/backend` 选择 `status-monitor` 绑定，下个 tick 起开始收卡。
卡片 PATCH 失败（被用户删除/撤回，飞书返回 `code:230011`）时自动重发，不叠加。
升级时短暂离线（systemd restart），期间停推一帧，下个 tick 自动恢复。

## 6.7. 总览卡主机/进程监控（多机部署）

总览卡新增两个 section：**主机**（load / 内存 / state_dir 所在磁盘）与**进程**
（每个 backend 的版本号 + systemd cgroup 内存）。数据流：

- 每个 backend 启动时在 SSE handshake 带 `?version=` 上报版本；
- 每个 backend 每 `status_monitor.interval` 秒 `POST /v1/metrics/<backendID>`
  上报本机主机+进程快照（推送周期与卡片刷新周期同源，改一处全集群生效）；
- feishu-front 不 POST 自己，在 `/v1/status` handler 里直读本机数据合并；
- frontend 按 IP 去重主机行（同机多 backend 取最新），聚合进 `/v1/status`，
  status-monitor 原样透传渲染。

**多机部署**：backend 分布在多台主机时各自上报本机数据（IP 取 dial frontend
的出站地址，即 frontend 实际看到的地址），无需额外配置——只要把 backend
部署到目标机并指向同一 `frontend_url` 即可，卡片自然每台主机一行。

**渲染规则**：

- cgroup 内存读不到（非 systemd 环境 / 单元未注册）显示 `—`；
- 某行数据超过 `3 × interval` 未更新（backend 在线但 metrics 通道挂掉）
  行尾标 `(stale)`；
- 在线 backend 版本不一致（某台落后于众数版本，疑似升级失败留旧版）时
  该行标 `🔴 版本漂移`，卡片头由蓝色变橙色；版本为 `unknown`（老 backend）
  的行不参与判定。

**发版顺序约束**：先升级 feishu-front，再升级各 backend（deploy.sh 的重启
顺序天然满足）。反序时新版 backend 的 metrics 推送在旧 frontend 上是 404
（静默跳过、业务路径无影响），但卡片上的主机/进程 section 会空白到
frontend 升级完成。新旧任意组合下 SSE event / control 业务路径零影响。

## 7. 验证

```bash
# 前端健康：IPC 监听
curl -s localhost:6060/v1/events  # 应返回 401（鉴权拦截）

# 日志
journalctl -u lark-feishu-front -f
journalctl -u lark-claude-back -f
journalctl -u lark-opencode-back -f
journalctl -u lark-omp-back -f
journalctl -u lark-miniagent-back -f
journalctl -u lark-status-monitor -f

# 在飞书群里 @机器人 发消息，观察日志输出
```

## 8. 二进制分发与灵活部署

deploy.sh 支持三种正交维度，组合使用：

- `--binaries <tar|dir>`：从已编译产物部署，目标机无需 Go/repo。
- `--services <list>`：只部署服务子集（逗号分隔：`feishu claude opencode omp miniagent`）。
- `--init` / `--force`：首次生成配置 / 跳过运行中会话检查。

**运行中会话检查（preflight）**：部署前 deploy.sh 调用 feishu-front 的
`GET /v1/deploy-preflight?services=<子集>`，由前端按内存中的 turn 列表判断——
部署 feishu（重启 IPC）会中断所有会话，返回 409 全拒；只部署 backend 子集时
仅当会话挂在目标 backend 上才拒绝，其它 backend 的会话不阻塞。旧版前端无此
端点（404）时退化为保守策略：只要有会话就拒绝。判断逻辑在 Go 侧
（`internal/feishufront/ipcserver_preflight.go`，含单测），bash 只读状态码。

### 8.1 打包分发（编译与部署解耦）

```bash
# 构建机（有 Go + repo）
make pack                          # 本机平台
make pack GOOS=linux GOARCH=arm64  # 交叉编译
# 产物：bin/lark-bridge-<ver>-<os>-<arch>.tar.gz，含 7 个二进制 + VERSION
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

