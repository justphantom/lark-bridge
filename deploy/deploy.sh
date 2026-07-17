#!/usr/bin/env bash
#
# lark-bridge 一键部署脚本（systemd）
#
# 用法：
#   ./deploy/deploy.sh            # 使用 repo 根目录已有的 claude-config.json 和 .env
#   ./deploy/deploy.sh --init     # 首次部署，自动从 example 生成 claude-config.json 和 .env
#
# 可选环境变量：
#   IPC_ADDR   IPC 监听地址（默认 localhost:6060）
#   STATE_DIR  持久化目录（默认 /var/lib/lark-bridge）
#
set -euo pipefail

# ── 路径 ──────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$PROJECT_ROOT/bin"

DEPLOY_DIR="/opt/lark-bridge/bin"
CONFIG_DIR="/etc/lark-bridge"
STATE_DIR="${STATE_DIR:-/var/lib/lark-bridge}"

# ── 运行用户（脚本内嵌 sudo；禁止整体以 root 运行）────
# 直接 sudo 调用会让 whoami 返回 root，导致服务以 root 运行。
# 此处从 SUDO_USER 还原真实调用者；无则报错退出（fail 尚未定义，内联等价实现）。
if [[ "$EUID" -eq 0 ]]; then
    RUN_USER="${SUDO_USER:-}"
    [[ -n "$RUN_USER" ]] || { echo "[FAIL] 请勿直接以 root 运行本脚本；它会在需要时自行 sudo。若必须，请用 'sudo -E' 以保证 SUDO_USER 可用" >&2; exit 1; }
else
    RUN_USER="$(whoami)"
fi

# ── IPC 地址 ─────────────────────────────────────────
IPC_ADDR="${IPC_ADDR:-localhost:6060}"

# ── 颜色 ──────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; exit 1; }

# ── 服务列表 ──────────────────────────────────────────
SERVICES=(lark-feishu-front lark-claude-back lark-opencode-back lark-peri-back lark-goose-back)

# 强制停止所有服务；确认全部退出后才返回，避免覆盖运行中的二进制（Text file busy）
# systemctl stop 抑制 Restart=on-failure；但默认会阻塞至 TimeoutStopSec（90s），
# 故用 timeout 15 限定等待。超时后 systemd 仍在异步停止，下面用 SIGKILL 兜底。
stop_services() {
    info "停止旧服务（systemctl stop，限时 15s）..."
    timeout 15 sudo systemctl stop "${SERVICES[@]}" 2>/dev/null || true
    sleep 1

    # 仍存活的进程：SIGKILL 连同 cgroup 内子进程一并清理。systemd 的
    # cgroup kill 已覆盖单元内所有子进程，无需再 pgrep 兜底——后者会
    # 误伤 deploy-monitor（其 cmdline 含 $DEPLOY_DIR/ 前缀，正是它 fork
    # 出本次 make deploy，被杀会连带终结整个部署脚本进程树）。
    for svc in "${SERVICES[@]}"; do
        local pid
        pid="$(systemctl show -p MainPID --value "$svc" 2>/dev/null || true)"
        if [[ -n "$pid" && "$pid" != "0" ]] && kill -0 "$pid" 2>/dev/null; then
            warn "$svc 仍在运行（PID=$pid），SIGKILL"
            sudo systemctl kill --signal=SIGKILL "$svc" 2>/dev/null || true
        fi
    done
    sleep 1

    # 最终确认：任一仍 active 则中止部署
    for svc in "${SERVICES[@]}"; do
        if systemctl is-active --quiet "$svc" 2>/dev/null; then
            fail "$svc 无法停止，中止部署以避免覆盖运行中的二进制"
        fi
    done
    info "旧服务已全部停止"
}

# 部署前检查：若 feishu-front 正在运行且报告有 in-flight 会话，中止部署，
# 避免中途重启打断用户正在进行的对话。读 /etc/lark-bridge/.env 取 IPC_SECRET
# 以访问 GET /v1/status；服务未运行或端点不可达时放行（首次部署/已停止场景）。
preflight_inflight_check() {
    # 服务未运行 → 无 in-flight 风险，直接放行。
    if ! systemctl is-active --quiet lark-feishu-front 2>/dev/null; then
        return 0
    fi

    local env_file="$CONFIG_DIR/.env"
    local secret=""
    if [[ -f "$env_file" ]]; then
        secret="$(grep -E '^IPC_SECRET=' "$env_file" 2>/dev/null | head -1 | cut -d= -f2- || true)"
    fi
    if [[ -z "$secret" ]]; then
        warn "未从 $env_file 读取到 IPC_SECRET，跳过 in-flight 检查"
        return 0
    fi

    local body code
    body="$(curl -s -m 3 -H "Authorization: Bearer $secret" "http://$IPC_ADDR/v1/status" 2>/dev/null || true)"
    code="$(curl -s -o /dev/null -m 3 -w '%{http_code}' -H "Authorization: Bearer $secret" "http://$IPC_ADDR/v1/status" 2>/dev/null || echo 000)"

    if [[ "$code" == "000" ]]; then
        # 端口不可达（服务在 active 但端口还没 listen）→ 放行，后续 stop_services 会处理。
        return 0
    fi
    if [[ "$code" == "401" ]]; then
        fail "IPC 返回 401（$env_file 的 IPC_SECRET 与运行中的服务不一致）；请核对后重试"
    fi
    if [[ "$code" != "200" ]]; then
        warn "IPC /v1/status 返回非预期状态码 $code，跳过 in-flight 检查"
        return 0
    fi

    local inflight
    inflight="$(echo "$body" | grep -oE '"inflight":[0-9]+' | head -1 | cut -d: -f2 || echo 0)"
    if [[ "${inflight:-0}" -gt 0 ]]; then
        fail "检测到 ${inflight} 个运行中会话（in-flight turn），中止部署以避免打断对话。请在对话结束后重试"
    fi
    info "无运行中会话，可安全部署"
}

# 轮询等待服务 active，最多 ~15s；避免冷启动时固定 sleep 导致的误判
wait_active() {
    local svc="$1"
    for _ in {1..15}; do
        systemctl is-active --quiet "$svc" && return 0
        sleep 1
    done
    return 1
}

# 轮询等待 feishu-front 的 IPC 端口 listen，最多 ~15s。
# 后端启动即连 6060，若 feishu-front 未 listen 会崩溃-重启（RestartSec=5），
# deploy.sh 在崩溃窗口抓 MainPID 会得到 0 → 误报。故先起前端、等端口通，
# 再起后端，从根因消除崩溃-重启。
# 任何 HTTP 响应都算 listen（401=鉴权正常，000=端口未通仍重试）。
wait_listen() {
    for _ in {1..15}; do
        local code
        code="$(curl -s -o /dev/null -w '%{http_code}' "http://$IPC_ADDR/v1/events" 2>/dev/null || echo 000)"
        [[ "$code" != "000" ]] && return 0
        sleep 1
    done
    return 1
}

# 生成单个 systemd unit
#   $1=unit 名  $2=描述  $3=二进制名  $4=配置文件名  $5=依赖 unit（可空，仅 feishu-front 留空）
#   $6=额外的 Environment= 行（可空，多行用 $'\n' 分隔）
# 用 Wants= 而非 Requires=：前端崩溃时后端不被连带停止，in-flight Claude 对话
# 继续运行，backendrpc.Run 的重连机制在前端恢复后重新接上 SSE。
write_unit() {
    local unit="$1" desc="$2" binary="$3" config="$4" requires="${5:-}" extra_env="${6:-}"
    local deps="After=network.target"
    [[ -n "$requires" ]] && deps="After=$requires.service"$'\n'"Wants=$requires.service"
    # extra_env 非空时尾部补一个换行，使 heredoc 里 ExecStart 独立成行；空则留空。
    local env_block=""
    [[ -n "$extra_env" ]] && env_block="$extra_env"$'\n'
    sudo tee "/etc/systemd/system/$unit.service" > /dev/null <<EOF
[Unit]
Description=lark-bridge $desc
$deps

[Service]
EnvironmentFile=$CONFIG_DIR/.env
${env_block}ExecStart=$DEPLOY_DIR/$binary -config $CONFIG_DIR/$config
Restart=on-failure
RestartSec=5
TimeoutStopSec=10
User=$RUN_USER
# 沙箱加固（保守集，只加确定不阻断 backend 正常 fork/exec CLI 的项）：
#   NoNewPrivileges      禁 setuid 提权（backend 不需要）
#   ProtectSystem=full   /usr /boot 只读；/var/lib(state_dir) 与 /home 仍可写
#                        （不用 strict：claude 写 ~/.claude、goose 读 ~/.config/goose）
#   ProtectHome 不设：backend 依赖用户 home 下的 CLI 配置与缓存
#   PrivateTmp           独立 /tmp 命名空间，不共享系统 tmp
#   ProtectKernel*       禁止改内核运行时/模块/日志/cgroup
#   RestrictSUIDSGID     拒绝执行 setuid/setgid 二进制
#   CapabilityBoundingSet=  清空能力集（无需任何 Linux capability）
# 不设 SystemCallFilter：backend 透传执行任意外部 CLI（git/node/shell…），
# 系统调用白名单极易误伤，收益不抵风险。
NoNewPrivileges=true
ProtectSystem=full
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
RestrictSUIDSGID=true
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOF
}

# ── 前置检查 ──────────────────────────────────────────
[[ -f "$PROJECT_ROOT/Makefile" ]] || fail "未找到 Makefile，请在 repo 根目录运行"
command -v go   >/dev/null || fail "未安装 Go"
command -v make >/dev/null || fail "未安装 make"

# ── 步骤 0：部署前会话检查（先于构建，避免浪费编译时间）──
info "检查运行中会话..."
preflight_inflight_check

# ── 步骤 1：构建 ──────────────────────────────────────
info "构建二进制..."
make -C "$PROJECT_ROOT" build
[[ -x "$BIN_DIR/lark-feishu-front" ]]   || fail "构建产物缺失：lark-feishu-front"
[[ -x "$BIN_DIR/lark-claude-back" ]]    || fail "构建产物缺失：lark-claude-back"
[[ -x "$BIN_DIR/lark-opencode-back" ]]  || fail "构建产物缺失：lark-opencode-back"
[[ -x "$BIN_DIR/lark-peri-back" ]]      || fail "构建产物缺失：lark-peri-back"
[[ -x "$BIN_DIR/lark-goose-back" ]]     || fail "构建产物缺失：lark-goose-back"
[[ -x "$BIN_DIR/lark-deploy-monitor" ]] || fail "构建产物缺失：lark-deploy-monitor"

# ── 步骤 2：在临时目录生成三个 config（不修改 repo 源文件）──
# 三个进程各用独立 config：claude-config.json / opencode-config.json / feishu-config.json。
# 三者都从同一份基础 config 派生（各进程只读自己需要的字段，多余字段无害）。
# 两个后端必须用不同的 router_path，否则写同一文件互相覆盖。
#
# 所有 sed 在临时副本上操作，repo 里的源 config 不被污染（git 不变 dirty）。
info "准备配置文件..."
STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

if [[ "${1:-}" == "--init" ]]; then
    [[ -f "$PROJECT_ROOT/.env" ]] || cp "$PROJECT_ROOT/deploy/env.example" "$PROJECT_ROOT/.env"
    # 生成 IPC_SECRET（仅匹配未改过的占位符）
    if grep -q '^IPC_SECRET=change-me' "$PROJECT_ROOT/.env" 2>/dev/null; then
        secret="$(openssl rand -hex 32)"
        sed -i "s|^IPC_SECRET=.*|IPC_SECRET=$secret|" "$PROJECT_ROOT/.env"
        info "已自动生成 IPC_SECRET"
    fi
    warn ".env 中的飞书凭证等仍需手动填写"
fi
[[ -f "$PROJECT_ROOT/.env" ]] || fail "未找到 .env（用 --init 自动生成或手动 cp deploy/env.example）"

# 基础 config：优先用 repo 里用户自定义的 claude-config.json，否则 fallback 到 example
if [[ -f "$PROJECT_ROOT/claude-config.json" ]]; then
    cp "$PROJECT_ROOT/claude-config.json" "$STAGE/claude-config.json"
else
    cp "$PROJECT_ROOT/config.example.json" "$STAGE/claude-config.json"
fi

# state_dir / ipc_addr / frontend_url 已在 config 模板里写成 ${STATE_DIR} / ${IPC_ADDR}
# 占位符，由各进程的 config.Load 在启动时从环境变量展开（见 internal/config 的
# expandEnvVars）。deploy.sh 只需保证 IPC_ADDR / STATE_DIR 进入 EnvironmentFile（见
# 步骤 3 的 .env 写入），无需 sed 改 JSON——既消除字面量替换的元字符转义陷阱，也避免
# sed 静默失败导致 state 分裂。

# claude-back / opencode-back 各注入独立 router_path。两后端共享同一个
# state_dir，若用默认的同一 router.v5.json 会互相覆盖会话绑定，故部署脚本
# 显式拆为 claude-router.json / opencode-router.json（文件名仅本脚本约定，
# 与 config 默认的 router.v5.json 不同；router_path 字段本身可配）。
#
# 注入用 sed '/"backend_id"/a\...'：以 backend_id 行为锚点在其后追加。若
# 用户自定义 config 缺 backend_id 字段，sed 静默不追加，两后端回退到同一
# 默认 router.v5.json 会互相覆盖会话绑定，故注入后必须显式校验存在。
inject_router_path() {
    local file="$1" path="$2"
    sed -i '/"router_path"/d' "$file"
    sed -i '/"backend_id"/a\  "router_path":  "'"$path"'",' "$file"
    grep -q '"router_path"' "$file" \
        || fail "router_path 注入失败：$file 缺少 backend_id 字段（注入锚点缺失），两后端将共用默认 router 文件互相覆盖"
}

inject_router_path "$STAGE/claude-config.json" "$STATE_DIR/claude-router.json"

# opencode-back：独立 backend_id + 独立 router_path
cp "$STAGE/claude-config.json" "$STAGE/opencode-config.json"
sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "opencode-1",|' "$STAGE/opencode-config.json"
inject_router_path "$STAGE/opencode-config.json" "$STATE_DIR/opencode-router.json"

# peri-back：独立 backend_id + 独立 router_path
cp "$STAGE/claude-config.json" "$STAGE/peri-config.json"
sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "peri-1",|' "$STAGE/peri-config.json"
inject_router_path "$STAGE/peri-config.json" "$STATE_DIR/peri-router.json"

# goose-back：独立 backend_id + 独立 router_path（同 peri 模式）
cp "$STAGE/claude-config.json" "$STAGE/goose-config.json"
sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "goose-1",|' "$STAGE/goose-config.json"
inject_router_path "$STAGE/goose-config.json" "$STATE_DIR/goose-router.json"

# deploy-monitor：独立 backend_id，无 session/router 需求；注入 deploy_monitor 块
# 指向 repo 根。monitor 不参与本脚本的 stop/start（见 SERVICES 数组），其二进制更新需单独重启。
cp "$STAGE/claude-config.json" "$STAGE/deploy-monitor-config.json"
sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "deploy-monitor-1",|' "$STAGE/deploy-monitor-config.json"
sed -i '/"router_path"/d' "$STAGE/deploy-monitor-config.json"
# 注入 deploy_monitor 块必须用「先删后插」而非直接 a\ 追加。base config 可能已含
# deploy_monitor 块——例如无 claude-config.json 时从 config.example.json 派生，其带
# 占位 project_root（/home/user/ZCodeProject/...）。直接在 backend_id 后追加会产生两
# 个 deploy_monitor 键；Go encoding/json 对重复键静默取最后一个，而原块在后，于是占位
# 路径覆盖注入值 → monitor 指向不存在的目录，首次远程 /deploy 必失败。故先范围删除
# 既有 deploy_monitor 块（键行到其后首个 } 闭合行；deploy_monitor 当前为扁平对象无嵌套
# }, 首个 } 即其闭合），再注入唯一的新块。deploy_target 显式给出（config_defaults 亦有兜底）。
sed -i '/"deploy_monitor"[[:space:]]*:/,/^[[:space:]]*}/d' "$STAGE/deploy-monitor-config.json"
sed -i '/"backend_id"/a\  "deploy_monitor": {"project_root": "'"$PROJECT_ROOT"'", "deploy_target": "deploy"},' "$STAGE/deploy-monitor-config.json"
# 校验注入成功（sed 在锚点不匹配时静默返回 0，须显式确认；与 inject_router_path 同模式）
grep -q '"deploy_monitor"[[:space:]]*:[[:space:]]*{"project_root"' "$STAGE/deploy-monitor-config.json" \
    || fail "deploy_monitor 注入失败：$STAGE/deploy-monitor-config.json 缺少 backend_id（注入锚点缺失），monitor 的 project_root 将为空"

# feishu-front：从 claude-config 派生（feishu 只读飞书凭证+ipc 字段，多余字段无害）
cp "$STAGE/claude-config.json" "$STAGE/feishu-config.json"

info "claude-config / opencode-config / peri-config / goose-config / deploy-monitor-config / feishu-config 已生成"

# ── 步骤 3：创建目录 + 复制文件 + 修权限 ─────────────
# STATE_DIR/{claude,opencode,peri} 是三个后端的 default_directory，per-chat 工作目录
# 在运行时由 MkdirAll 在其下自动创建。
info "创建系统目录..."
sudo mkdir -p "$DEPLOY_DIR" "$CONFIG_DIR" "$STATE_DIR/claude" "$STATE_DIR/opencode" "$STATE_DIR/peri" "$STATE_DIR/goose"

# 必须先停服务，否则覆盖二进制会 "Text file busy"
stop_services

info "复制二进制和配置..."

# 二进制用「写临时文件 + 原子 rename」更新，而非直接 cp 覆盖。rename(2) 替换
# 路径不触发 ETXTBSY——运行中的进程继续持有旧 inode，直到被重启才换上新文件。
# 因此连正在运行的 deploy-monitor 也能就地更新其磁盘二进制：它本次部署保持存活
# 以发送完成通知，下次手动 restart 即生效。这消除了原先「monitor 运行中就跳过其
# 二进制更新、需手动 cp」的限制。临时文件落在同一 $DEPLOY_DIR，确保 mv 是同卷
# rename（原子、不跨设备）。业务服务已在 stop_services 停止，rename 同样安全。
for f in "$BIN_DIR"/*; do
    [[ -f "$f" ]] || continue
    name="$(basename "$f")"
    sudo cp "$f" "$DEPLOY_DIR/.${name}.new"
    sudo mv -f "$DEPLOY_DIR/.${name}.new" "$DEPLOY_DIR/$name"
done
sudo chmod 755 "$DEPLOY_DIR"/*

# config 是部署产物，每次从 STAGE 覆盖到 CONFIG_DIR
sudo cp "$STAGE/claude-config.json"        "$CONFIG_DIR/"
sudo cp "$STAGE/opencode-config.json"      "$CONFIG_DIR/"
sudo cp "$STAGE/peri-config.json"          "$CONFIG_DIR/"
sudo cp "$STAGE/goose-config.json"         "$CONFIG_DIR/"
sudo cp "$STAGE/deploy-monitor-config.json" "$CONFIG_DIR/"
sudo cp "$STAGE/feishu-config.json"        "$CONFIG_DIR/"
sudo chmod 600 "$CONFIG_DIR"/*.json

# .env 含真实凭证，仅首次部署写入；后续部署保留现有的 .env 不覆盖。
# 但 IPC_ADDR / STATE_DIR 是部署参数（可随部署变更），非凭证——每次部署
# 都强制同步为本次值，否则 config 模板里的 ${IPC_ADDR} / ${STATE_DIR} 会
# 展开成旧值，state 目录与 IPC 地址对不上。用 sed 幂等更新：键存在则改，
# 不存在则追加。
if [[ ! -f "$CONFIG_DIR/.env" ]]; then
    sudo cp "$PROJECT_ROOT/.env" "$CONFIG_DIR/.env"
    info "首次部署：已写入 .env"
else
    info "保留现有 .env（不覆盖凭证，仅同步部署参数）"
fi
# 幂等更新部署参数键。grep -q 判断存在性，存在用 sed 改整行，不存在追加。
update_env_key() {
    local key="$1" val="$2" file="$3"
    if sudo grep -q "^${key}=" "$file"; then
        sudo sed -i "s|^${key}=.*|${key}=${val}|" "$file"
    else
        echo "${key}=${val}" | sudo tee -a "$file" > /dev/null
    fi
}
update_env_key IPC_ADDR "$IPC_ADDR" "$CONFIG_DIR/.env"
update_env_key STATE_DIR "$STATE_DIR" "$CONFIG_DIR/.env"
sudo chmod 600 "$CONFIG_DIR/.env"

info "修复目录和文件权限 → owner=$RUN_USER"
sudo chown -R "$RUN_USER:$RUN_USER" "$DEPLOY_DIR" "$CONFIG_DIR" "$STATE_DIR"

# ── 步骤 4：生成 systemd unit（动态用户）─────────────
info "生成 systemd unit 文件（User=$RUN_USER）..."

write_unit lark-feishu-front   lark-feishu-front   lark-feishu-front   feishu-config.json
write_unit lark-claude-back    lark-claude-back    lark-claude-back    claude-config.json   lark-feishu-front
write_unit lark-opencode-back  lark-opencode-back  lark-opencode-back  opencode-config.json lark-feishu-front
write_unit lark-peri-back      lark-peri-back      lark-peri-back      peri-config.json     lark-feishu-front
# goose 把 API key 存在用户 D-Bus keyring（登录会话独有）或 ~/.config/goose/
# secrets.yaml。systemd 启动不带 D-Bus 会话环境 → 读不到 keyring → 本地代理 401。
# GOOSE_DISABLE_KEYRING=true 强制 goose 走文件存储（secrets.yaml），绕过 keyring。
# 凭证文件需运维用 `goose configure` 预先生成（见下方 deploy_goose_secret_check）。
write_unit lark-goose-back     lark-goose-back     lark-goose-back     goose-config.json    lark-feishu-front \
    "Environment=GOOSE_DISABLE_KEYRING=true"
write_unit lark-deploy-monitor lark-deploy-monitor lark-deploy-monitor deploy-monitor-config.json lark-feishu-front

# goose 凭证检测：GOOSE_DISABLE_KEYRING=true 下 goose 从 ~/.config/goose/secrets.yaml
# 读 key。该文件需运维预先以 RUN_USER 身份运行 `goose configure` 生成（交互式，无法
# 在本脚本内完成）。缺失则 goose-back 必 401（已实测）——仅告警不中止，让运维先完成
# 其他部署再补凭证；明确给出修复命令避免无方向排查。
deploy_goose_secret_check() {
    local home_dir
    home_dir="$(getent passwd "$RUN_USER" | cut -d: -f6)"
    [[ -n "$home_dir" ]] || { warn "无法解析 $RUN_USER 的 home 目录，跳过 goose 凭证检查"; return; }
    local secret="$home_dir/.config/goose/secrets.yaml"
    if [[ ! -s "$secret" ]]; then
        warn "goose 凭证缺失：$secret 不存在或为空"
        warn "  goose-back 将无法认证（401）。请以 $RUN_USER 身份执行："
        warn "    sudo -u $RUN_USER goose configure   # 选 openai provider 填 API key"
        warn "  生成的 secrets.yaml 即为 GOOSE_DISABLE_KEYRING=true 时的凭证来源"
    else
        info "goose 凭证就绪：$secret"
    fi
}
deploy_goose_secret_check

# ── 步骤 5：启动（串行：前端先 listen，再起后端）─────
info "启动服务..."
sudo systemctl daemon-reload
# enable 三服务开机自启，但不 --now；下面显式控制启动顺序。
sudo systemctl enable "${SERVICES[@]}" 2>/dev/null || true

# 先起前端，等 IPC 端口 listen，避免后端连不上而崩溃-重启
sudo systemctl start lark-feishu-front
wait_active lark-feishu-front || fail "lark-feishu-front 启动失败"
wait_listen || fail "feishu-front IPC 端口 $IPC_ADDR 未 listen，后端无法连接"

# 端口已通，再起三个后端（并行，互不依赖）
sudo systemctl start lark-claude-back lark-opencode-back lark-peri-back lark-goose-back

# deploy-monitor 不在 SERVICES 中（避免被 stop_services 杀掉中断部署通知）。
# 首次部署（unit 未 enable）时 enable + start；后续部署不触碰，monitor 进程
# 全程存活以发送部署完成通知。其磁盘二进制已随上面 rename 步骤就地更新，但运行中
# 进程仍持旧 inode，需运维手动 systemctl restart lark-deploy-monitor 才换上新版本。
#
# ⚠ 运行前提：monitor 以非 root 的 $RUN_USER 运行，收到 /deploy 时执行
# `make deploy` → 本脚本 → 大量 `sudo systemctl ...`。systemd 无 TTY，sudo 一旦
# 需要密码就会挂起直到 monitor 的 10 分钟超时，部署静默失败。故 $RUN_USER 必须对
# systemctl/cp/mkdir/chmod 等具备 NOPASSWD sudo（或配 /etc/sudoers.d 条目）。
# 这里主动探测一次免密 sudo（monitor 真实路径），失败则明确告警，避免远程 /deploy
# 静默挂死 10 分钟才超时。
deploy_sudo_check() {
    # sudo -n 非交互，无免密配置时立即失败而非挂起等密码
    if sudo -u "$RUN_USER" sudo -n systemctl is-active lark-feishu-front >/dev/null 2>&1; then
        info "$RUN_USER 具备免密 sudo（remote /deploy 可用）"
    else
        warn "$RUN_USER 无免密 sudo，remote /deploy 将挂起至超时失败"
        warn "  修复：配 /etc/sudoers.d/lark-bridge，例如："
        warn "    $RUN_USER ALL=(ALL) NOPASSWD: /usr/bin/systemctl, /usr/bin/cp, /usr/bin/mkdir, /usr/bin/chmod, /usr/bin/chown, /usr/bin/sed, /usr/bin/tee, /usr/bin/rm, /usr/bin/mv"
        warn "  （仅授予本脚本用到的命令，遵循最小权限）"
    fi
}
deploy_sudo_check

if ! systemctl is-enabled --quiet lark-deploy-monitor 2>/dev/null; then
    info "首次部署：enable + start lark-deploy-monitor"
    sudo systemctl enable lark-deploy-monitor 2>/dev/null || true
    sudo systemctl start lark-deploy-monitor
else
    info "lark-deploy-monitor 已 enabled，保持运行（不重启）"
fi

# ── 步骤 6：验证（轮询 is-active，替代固定 sleep）─────
info "验证..."
all_ok=true
for svc in "${SERVICES[@]}"; do
    if wait_active "$svc"; then
        echo -e "  ${GREEN}✓${NC} $svc  $(systemctl show -p MainPID --value "$svc")"
    else
        echo -e "  ${RED}✗${NC} $svc  FAILED"
        all_ok=false
    fi
done

# IPC 鉴权检查
code="$(curl -s -o /dev/null -w '%{http_code}' "http://$IPC_ADDR/v1/events" 2>/dev/null || echo 000)"
if [[ "$code" == "401" ]]; then
    echo -e "  ${GREEN}✓${NC} IPC ($IPC_ADDR) 返回 401（鉴权正常）"
else
    echo -e "  ${YELLOW}!${NC} IPC ($IPC_ADDR) 返回 $code（期望 401）"
fi

$all_ok && info "部署完成" || fail "部分服务启动失败，请检查 journalctl -u {service}"