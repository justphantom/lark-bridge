#!/usr/bin/env bash
#
# lark-bridge 一键部署脚本（systemd）
#
# 用法：
#   ./deploy/deploy.sh            # 使用 repo 根目录的 .env；config 从 config.example.json 派生
#   ./deploy/deploy.sh --init     # 首次部署，自动从 example 生成 .env
#   ./deploy/deploy.sh --force    # 强制部署，跳过运行中会话检查
#   ./deploy/deploy.sh --binaries <tar|dir>
#                                # 跳过 make build，从已编译产物部署（目标机免 Go/免 repo）。
#                                # <tar>：make pack 产出的 tarball，解包取顶层二进制；
#                                # <dir>：已解包目录，内含 lark-* 二进制。
#   ./deploy/deploy.sh --services claude,opencode
#                                # 只部署指定服务子集（逗号分隔，可用：feishu claude
#                                # opencode miniagent）。默认全量。多主机部署时每台机
#                                # 用不同子集：前端机 --services feishu，后端机 --services claude,...
#
# 可选环境变量：
#   IPC_ADDR   IPC 监听地址。优先级：环境变量 > repo 根 .env > localhost:6060
#              （改 .env 即生效；环境变量优先用于一次性覆盖）
#   STATE_DIR  持久化目录（默认 /var/lib/lark-bridge，环境变量覆盖）
#
set -euo pipefail

# ── 路径 ──────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$PROJECT_ROOT/bin"

DEPLOY_DIR="/opt/lark-bridge/bin"
CONFIG_DIR="/etc/lark-bridge"
STATE_DIR="${STATE_DIR:-/var/lib/lark-bridge}"

# ── 超时/轮询常量（集中调参，避免 magic number 散落）─────────
# HTTP_TIMEOUT       curl 探测 IPC 的上限（本地/局域网）
# WAIT_RETRIES       systemctl 冷启动轮询次数（每次 sleep 1s，约 15s 上限）
# STOP_TIMEOUT       systemctl stop 限时；超过用 SIGKILL 兜底（默认 TimeoutStopSec=90s 太长）
# CLI_PROBE_TIMEOUT  外部 CLI --version 探测上限，与 backend readyTimeout 同源
#                    （internal/opencode/client.go:27）
HTTP_TIMEOUT=3
WAIT_RETRIES=15
STOP_TIMEOUT=15
CLI_PROBE_TIMEOUT=30

# ── 运行用户（脚本内嵌 sudo；禁止整体以 root 运行）────
# 直接 sudo 调用会让 whoami 返回 root，导致服务以 root 运行。
# 此处从 SUDO_USER 还原真实调用者；无则报错退出（fail 尚未定义，内联等价实现）。
if [[ "$EUID" -eq 0 ]]; then
    RUN_USER="${SUDO_USER:-}"
    [[ -n "$RUN_USER" ]] || { echo "[FAIL] 请勿直接以 root 运行本脚本；它会在需要时自行 sudo。若必须，请用 'sudo -E' 以保证 SUDO_USER 可用" >&2; exit 1; }
else
    RUN_USER="$(whoami)"
fi

# ── 颜色 ──────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; exit 1; }

# ERR trap：set -e 触发的失败（非 || true 容忍的）打出失败行 + 命令，定位根因。
# -E 让 trap 在函数内也生效。fail() 自己 exit 1（非失败命令），不触发 ERR，
# 故 fail 的退出消息独占一行。${RED}/${NC} 在 trap 触发时（运行时）求值，已定义。
set -E
trap 'echo -e "${RED}[FAIL]${NC} 错误行 $LINENO: $BASH_COMMAND" >&2' ERR

# 从 repo 根 .env 读 KEY=VALUE 的 VALUE；文件不存在或键缺失返回空。首次部署
# （.env 还未生成）时返回空，由调用方默认值兜底。
# || true 兜底：pipefail 下 grep 无匹配会返回 1，使命令替换在 set -e 下误退出。
env_get() {
    local key="$1"
    grep -E "^${key}=" "$PROJECT_ROOT/.env" 2>/dev/null | head -1 | cut -d= -f2- || true
}

# ── IPC 地址 ─────────────────────────────────────────
# 优先级：环境变量 > repo 根 .env > 默认值（localhost:6060）。让"改 .env 不
# 带环境变量启动"也生效（与运行时配置同源）；环境变量优先用于一次性覆盖。
IPC_ADDR="${IPC_ADDR:-$(env_get IPC_ADDR)}"
IPC_ADDR="${IPC_ADDR:-localhost:6060}"

# ── 服务列表 ──────────────────────────────────────────
# SERVICES（unit 名数组）由参数解析块按 --services 派生；默认全量 4 个业务服务。

# 强制停止所有服务；确认全部退出后才返回，避免覆盖运行中的二进制（Text file busy）
# systemctl stop 抑制 Restart=on-failure；但默认会阻塞至 TimeoutStopSec（90s），
# 故用 timeout $STOP_TIMEOUT 限定等待。超时后 systemd 仍在异步停止，下面用 SIGKILL 兜底。
stop_services() {
    info "停止旧服务（systemctl stop，限时 ${STOP_TIMEOUT}s）..."
    timeout "$STOP_TIMEOUT" sudo systemctl stop "${SERVICES[@]}" 2>/dev/null || true
    sleep 1

    # 仍存活的进程：SIGKILL 连同 cgroup 内子进程一并清理。systemd 的
    # cgroup kill 已覆盖单元内所有子进程，无需再 pgrep 兜底——后者可能
    # 误伤 deploy-monitor（它可能正 fork 出本次 make deploy 进程树）。
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
# 避免中途重启打断用户正在进行的对话。读 repo 根 .env 取 IPC_SECRET
# 以访问 GET /v1/status；服务未运行或端点不可达时放行（首次部署/已停止场景）。
preflight_inflight_check() {
    # 服务未运行 → 无 in-flight 风险，直接放行。
    if ! systemctl is-active --quiet "$(svc_unit feishu)" 2>/dev/null; then
        return 0
    fi

    local secret
    secret="$(env_get IPC_SECRET)"
    if [[ -z "$secret" ]]; then
        warn "未从 $PROJECT_ROOT/.env 读取到 IPC_SECRET，跳过 in-flight 检查"
        return 0
    fi

    # 单次 curl 用 -w $'\n%{http_code}' 把状态码追加到 body 末尾，再 tail/sed 拆分。
    # 比原先 body 与 code 各请求一次少一轮 IPC 往返；失败时 resp 空 → code 兜底 000。
    local resp body code
    resp="$(curl -s -m "$HTTP_TIMEOUT" -w $'\n%{http_code}' -H "Authorization: Bearer $secret" "http://$IPC_ADDR/v1/status" 2>/dev/null || true)"
    code="$(tail -1 <<<"$resp")"
    [[ "$code" =~ ^[0-9]+$ ]] || code=000
    body="$(sed '$d' <<<"$resp")"

    if [[ "$code" == "000" ]]; then
        # 端口不可达（服务在 active 但端口还没 listen）→ 放行，后续 stop_services 会处理。
        return 0
    fi
    if [[ "$code" == "401" ]]; then
        fail "IPC 返回 401（repo 根 .env 的 IPC_SECRET 与运行中的服务不一致）；请核对后重试"
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

# 探测外部 CLI（claude/opencode/miniagent）二进制是否就绪：command -v 命中 +
# `<cli> --version` 退出 0。30s 超时与 backend IsReady 的 readyTimeout 一致
# （internal/opencode/client.go:27），防止 hang 阻塞部署。作为对应 backend
# 部署的硬性条件：CLI 不在 PATH → backend 启动必崩，systemd Restart=on-failure
# 每 5s 重试。提前探测并停禁剔除，等运维装好 CLI 重新部署。
probe_cli() {
    local cli="$1"
    if ! command -v "$cli" >/dev/null 2>&1; then
        warn "$cli 二进制未就绪：command -v 找不到（请装到 PATH）"
        return 1
    fi
    if ! timeout "$CLI_PROBE_TIMEOUT" "$cli" --version >/dev/null 2>&1; then
        warn "$cli 二进制未就绪：$cli --version 退出非 0 或超时（${CLI_PROBE_TIMEOUT}s）"
        return 1
    fi
    info "$cli 二进制就绪（$(command -v "$cli")）"
    return 0
}

# 轮询等待服务 active，最多 ~15s；避免冷启动时固定 sleep 导致的误判
wait_active() {
    local svc="$1" i
    for ((i=0; i<WAIT_RETRIES; i++)); do
        systemctl is-active --quiet "$svc" && return 0
        sleep 1
    done
    return 1
}

# 轮询等待 feishu-front 的 IPC 端口 listen，最多 ~15s。
# 后端启动即连 6060，若 feishu-front 未 listen 会崩溃-重启（RestartSec=5），
# deploy.sh 在崩溃窗口抓 MainPID 会得到 0 → 误报。故先起前端、等端口通，
# 再起后端，从根因消除崩溃-重启。
# 带 .env 的 IPC_SECRET 请求 /v1/status（非流式 GET；/v1/events 是 SSE，鉴权
# 通过后服务会持续推流让 curl hang）。-m $HTTP_TIMEOUT 兜底防任何流式端点误阻塞轮询。
# 000=端口未通仍重试；任何非 000 响应都算 listen（401=端口 listen 但 secret
# 不匹配，可能是部署间隙旧 secret；不阻塞起后端）。
wait_listen() {
    local secret auth=() i
    secret="$(env_get IPC_SECRET)"
    [[ -n "$secret" ]] && auth=(-H "Authorization: Bearer $secret")
    for ((i=0; i<WAIT_RETRIES; i++)); do
        local code
        code="$(curl -s -o /dev/null -m "$HTTP_TIMEOUT" -w '%{http_code}' "${auth[@]}" "http://$IPC_ADDR/v1/status" 2>/dev/null || echo 000)"
        [[ "$code" != "000" ]] && return 0
        sleep 1
    done
    return 1
}

# 生成单个 systemd unit。$unit 同时作为 Description 后缀和二进制名
# （unit=lark-xxx-back，二进制同路径同名，Description=lark-bridge $unit）。
#   $1=unit 名  $2=配置文件名  $3=依赖 unit（可空，仅 feishu-front 留空）
#   $4=额外的 Environment= 行（可空，多行用 $'\n' 分隔）  $5=privileged（默认 false）
# 用 Wants= 而非 Requires=：前端崩溃时后端不被连带停止，in-flight Claude 对话
# 继续运行，backendrpc.Run 的重连机制在前端恢复后重新接上 SSE。
write_unit() {
    local unit="$1" config="$2" requires="${3:-}" extra_env="${4:-}" privileged="${5:-false}"
    local deps="After=network.target"
    [[ -n "$requires" ]] && deps="After=$requires.service"$'\n'"Wants=$requires.service"
    # extra_env 非空时尾部补一个换行，使 heredoc 里 ExecStart 独立成行；空则留空。
    local env_block=""
    [[ -n "$extra_env" ]] && env_block="$extra_env"$'\n'
    # privileged=true 时整个沙箱块省略：供需要 sudo 提权的单元用（如
    # deploy-monitor 跑 `make deploy` 时要 systemctl/cp 到 /etc）。沙箱的
    # NoNewPrivileges 会禁止 sudo 的 setuid 提权，故提权单元必须绕过沙箱。
    # claude/opencode/miniagent 三个 backend 同样用 privileged=true：它们透传执行
    # 任意外部 CLI 链（git/node/npm/bash 及子进程），保守沙箱（NoNewPrivileges/
    # RestrictSUIDSGID 拦 setuid helper、ProtectSystem=full 拦写 /usr）会误伤，故裸跑；
    # 仅 feishu-front 不 fork 外部 CLI，保留沙箱。
    local sandbox=""
    if [[ "$privileged" != "true" ]]; then
        sandbox='# 沙箱加固（保守集，只加确定不阻断 backend 正常 fork/exec CLI 的项）：
#   NoNewPrivileges      禁 setuid 提权（backend 不需要）
#   ProtectSystem=full   /usr /boot 只读；/var/lib(state_dir) 与 /home 仍可写
#                        （不用 strict：claude 写 ~/.claude、opencode 读 ~/.config）
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
CapabilityBoundingSet='
    fi
    sudo tee "/etc/systemd/system/$unit.service" > /dev/null <<EOF
[Unit]
Description=lark-bridge $unit
$deps

[Service]
EnvironmentFile=$CONFIG_DIR/.env
${env_block}ExecStart=$DEPLOY_DIR/$unit -config $CONFIG_DIR/$config
Restart=on-failure
RestartSec=5
TimeoutStopSec=10
User=$RUN_USER
${sandbox}
[Install]
WantedBy=multi-user.target
EOF
}

# ── 参数解析 ──────────────────────────────────────────
# 全部 flag 在此一次性解析到变量，后续不再直接读 $1（旧的 --init/--force
# 位置检查改为读 $INIT/$FORCE）。--binaries / --services 接受紧跟的下一个参数。
BINARIES_SRC=""
SERVICES_ARG=""
INIT=false
FORCE=false
DEBUG=false
prev=""
for arg in "$@"; do
    if [[ -n "$prev" ]]; then
        case "$prev" in
            --binaries) BINARIES_SRC="$arg" ;;
            --services) SERVICES_ARG="$arg" ;;
        esac
        prev=""; continue
    fi
    case "$arg" in
        --init)        INIT=true ;;
        --force)       FORCE=true ;;
        --debug)       DEBUG=true ;;
        --help|-h)     awk 'NR==1{next} /^#!/{next} /^[^#]/{exit} {sub(/^#[[:space:]]?/,""); print}' "$0" | sed 's/^$//'; exit 0 ;;
        --binaries|--services) prev="$arg" ;;
        *)             fail "未知参数：$arg（可用：--init --force --debug --help --binaries <path> --services <list>）" ;;
    esac
done
[[ -z "$prev" ]] || fail "${prev} 需要一个参数"

# --debug：开启 set -x 跟踪每条命令（含变量展开），用于排查部署链路。
# 放参数解析后，避免解析过程本身被 trace 淹没。
$DEBUG && set -x

# 服务短名 ↔ unit/配置/依赖/提权 映射。新增 backend 仅在此登记四处即可被
# --services 识别，无需改部署流程的各操作点。
svc_unit()  { case "$1" in feishu) echo lark-feishu-front;; claude) echo lark-claude-back;; opencode) echo lark-opencode-back;; miniagent) echo lark-miniagent-back;; *) return 1;; esac; }
svc_config(){ case "$1" in feishu) echo feishu-config.json;; claude) echo claude-config.json;; opencode) echo opencode-config.json;; miniagent) echo miniagent-config.json;; esac; }
# backend 依赖前端 listen 且需提权（透传外部 CLI）；feishu-front 两者皆无。
svc_depends(){ [[ "$1" == "feishu" ]] && echo "" || echo "lark-feishu-front"; }
svc_privileged(){ [[ "$1" == "feishu" ]] && echo "false" || echo "true"; }
# CLI 二进制名（probe_cli 用）；feishu 无 CLI。
svc_cli(){ case "$1" in claude) echo "claude";; opencode) echo "opencode";; miniagent) echo "miniagent";; *) echo "";; esac; }

# SELECTED → SERVICES：短名 → unit 名重建。改 SELECTED 后必须调用，否则
# stop/enable/start/验证等用 SERVICES 的地方会偏离。
rebuild_services() {
    SERVICES=()
    local s
    for s in "${SELECTED[@]}"; do SERVICES+=("$(svc_unit "$s")"); done
}

# 从 SELECTED 剔除指定短名（probe 不就绪/env 占位时调用），再同步 SERVICES。
# 用 _keep 数组保留除目标外的项，避免 splice 索引计算。
drop_service() {
    local drop="$1" s
    _keep=(); for s in "${SELECTED[@]}"; do [[ "$s" != "$drop" ]] && _keep+=("$s"); done
    SELECTED=("${_keep[@]}")
    rebuild_services
}

# 等待单元就绪失败时先 stop 再 fail：单元已 enabled，systemd 会按 Restart=on-failure
# 每 5s 反复重启留下半残状态，stop 后才让运维介入。$1=unit 名 $2=失败消息。
fail_after_stop() {
    local unit="$1" msg="$2"
    sudo systemctl stop "$unit" 2>/dev/null || true
    fail "$msg"
}

# 体检：RUN_USER 是否具备免密 sudo。remote /deploy（经 deploy-monitor 触发本
# 脚本）无 tty，sudo 没配 NOPASSWD 会挂起到 deploy-monitor 超时——前置到步骤0
# 前，让运维第一时间看到修复建议，而不是部署完才发现下次远程会挂。
deploy_sudo_check() {
    if sudo -u "$RUN_USER" sudo -n systemctl is-active "$(svc_unit feishu)" >/dev/null 2>&1; then
        info "$RUN_USER 具备免密 sudo"
    else
        warn "$RUN_USER 无免密 sudo，remote /deploy 将挂起至超时失败"
        warn "  修复：配 /etc/sudoers.d/lark-bridge，例如："
        warn "    $RUN_USER ALL=(ALL) NOPASSWD: /usr/bin/systemctl, /usr/bin/cp, /usr/bin/mkdir, /usr/bin/chmod, /usr/bin/chown, /usr/bin/sed, /usr/bin/tee, /usr/bin/rm, /usr/bin/mv"
        warn "  （仅授予本脚本用到的命令，遵循最小权限）"
    fi
}

# SELECTED：本次部署的服务短名；未传 --services 则全部（默认全量，行为不变）。
# SERVICES 为对应 unit 名数组，供 stop/enable/start/验证等沿用旧变量名。
SELECTED=()
if [[ -n "$SERVICES_ARG" ]]; then
    IFS=',' read -ra _parts <<< "$SERVICES_ARG"
    for s in "${_parts[@]}"; do
        svc_unit "$s" >/dev/null || fail "未知服务：$s（可用：feishu claude opencode miniagent）"
        SELECTED+=("$s")
    done
else
    SELECTED=(feishu claude opencode miniagent)
fi
rebuild_services

# ── 前置检查 ──────────────────────────────────────────
# 仅源码构建模式（无 --binaries）才要求本机有 Makefile/go/make；
# --binaries 模式目标机无需 Go 工具链与 repo 源码。
if [[ -z "$BINARIES_SRC" ]]; then
    [[ -f "$PROJECT_ROOT/Makefile" ]] || fail "未找到 Makefile，请在 repo 根目录运行"
    command -v go   >/dev/null || fail "未安装 Go"
    command -v make >/dev/null || fail "未安装 make"
fi

# ── 步骤 0：部署前会话检查 + sudo 体检（先于构建，避免浪费编译时间）──
deploy_sudo_check
if $FORCE; then
    warn "--force：跳过运行中会话检查，强制部署（可能打断正在进行的对话）"
else
    info "检查运行中会话..."
    preflight_inflight_check
fi

# ── 步骤 1：准备二进制 ────────────────────────────────
# 源码模式：make build 本机编译。--binaries 模式：从 tarball 解包或从目录复制，
# 解耦编译与部署（目标机无需 Go/repo）。两种模式产物都落到 BIN_DIR，后续
# cp 到 DEPLOY_DIR 的流程不变。
ensure_binaries() {
    mkdir -p "$BIN_DIR"
    if [[ -z "$BINARIES_SRC" ]]; then
        info "构建二进制（源码编译）..."
        make -C "$PROJECT_ROOT" build
        return
    fi
    if [[ -f "$BINARIES_SRC" ]]; then
        info "从 tarball 解包二进制：$BINARIES_SRC"
        tar -xzf "$BINARIES_SRC" -C "$BIN_DIR"
    elif [[ -d "$BINARIES_SRC" ]]; then
        info "从目录复制二进制：$BINARIES_SRC"
        cp "$BINARIES_SRC"/lark-* "$BIN_DIR/" 2>/dev/null || cp "$BINARIES_SRC"/* "$BIN_DIR/"
    else
        fail "--binaries 路径不存在：$BINARIES_SRC"
    fi
    chmod 755 "$BIN_DIR"/lark-* 2>/dev/null || true
}
ensure_binaries
[[ -x "$BIN_DIR/lark-feishu-front" ]]         || fail "构建产物缺失：lark-feishu-front"
[[ -x "$BIN_DIR/lark-claude-back" ]]          || fail "构建产物缺失：lark-claude-back"
[[ -x "$BIN_DIR/lark-opencode-back" ]]        || fail "构建产物缺失：lark-opencode-back"
[[ -x "$BIN_DIR/lark-miniagent-back" ]]       || fail "构建产物缺失：lark-miniagent-back"
# NOTE: miniagent 二进制（github.com/justphantom/miniagent）独立项目，需通过其
# 自带 Makefile 单独部署到 /usr/local/bin/miniagent，不归本 deploy.sh 管。
# lark-deploy-monitor 同理：本 tarball 内但由 upgrade-monitor.sh 独立部署；
# 解包后留在 BIN_DIR 无害，下方 cp 会一并落到 DEPLOY_DIR 供其覆盖。

# ── 步骤 2：在临时目录生成各 backend 独立 config（不修改 repo 源文件）──
# 四个进程各用独立 config：claude/opencode/miniagent/feishu-config.json。
# 都从同一份基础 config 派生（各进程只读自己需要的字段，多余字段无害）。
# 各 backend 必须用不同的 router_path（feishu-front 除外），否则
# 写同一文件互相覆盖。
# deploy-monitor 的 config/unit 由 upgrade-monitor.sh 独立管理，不在此流程内。
#
# 所有 sed 在临时副本上操作，repo 里的源 config 不被污染（git 不变 dirty）。
info "准备配置文件..."
STAGE="$(mktemp -d)"
# EXIT/HUP/INT/TERM 时清理 STAGE 临时目录与 DEPLOY_DIR 残留 .new 临时文件
# （cp 后 mv 前被中断会留下 .X.new，归 root；不清理会污染目录但每次 cp 覆盖亦无害，
# 仍清理以保持整洁）。trap 即使正常完成也触发一次，那时已 mv 完成，rm 无害。
trap 'rm -rf "$STAGE"; sudo rm -f "$DEPLOY_DIR"/.lark-*.new 2>/dev/null || true' EXIT

if $INIT; then
    if [[ ! -f "$PROJECT_ROOT/.env" ]]; then
        if [[ -f "$PROJECT_ROOT/deploy/env.example" ]]; then
            cp "$PROJECT_ROOT/deploy/env.example" "$PROJECT_ROOT/.env"
        elif [[ -f "$BIN_DIR/env.example" ]]; then
            cp "$BIN_DIR/env.example" "$PROJECT_ROOT/.env"
        else
            fail "找不到 env.example 模板（repo deploy/ 或 tarball）"
        fi
    fi
    # 生成 IPC_SECRET（仅匹配未改过的占位符）。用 _init_secret 前缀避免与下方
    # 函数内的 local secret 同名混淆（顶层赋值在 bash 中是全局污染）。
    if grep -q '^IPC_SECRET=change-me' "$PROJECT_ROOT/.env" 2>/dev/null; then
        _init_secret="$(openssl rand -hex 32)"
        sed -i "s|^IPC_SECRET=.*|IPC_SECRET=$_init_secret|" "$PROJECT_ROOT/.env"
        info "已自动生成 IPC_SECRET"
    fi
    warn ".env 中的飞书凭证等仍需手动填写"
fi
[[ -f "$PROJECT_ROOT/.env" ]] || fail "未找到 .env（用 --init 自动生成或手动 cp deploy/env.example）"

# 补齐缺失变量：env.example 里有、而 repo 根 .env 里没有的 KEY，用 example 的
# 默认值整行追加。已存在的 KEY 一律不动（尊重运维已配置的值）。应对升级新增了
# 变量、旧 .env 没有的情况（如 OPENCODE_SERVER_PASSWORD 缺失导致进程启动 expand 失败）。
if [[ -f "$PROJECT_ROOT/deploy/env.example" ]]; then
    while IFS= read -r line; do
        [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)= ]] || continue
        key="${BASH_REMATCH[1]}"
        grep -q "^${key}=" "$PROJECT_ROOT/.env" && continue
        printf '%s\n' "$line" >> "$PROJECT_ROOT/.env"
        info "补齐缺失变量 ${key}（用 env.example 默认值）"
    done < "$PROJECT_ROOT/deploy/env.example"
fi

# 检查 .env 是否仍含占位值（首次部署容易忘改）
check_env_placeholder() {
    local key="$1" pattern="$2" hint="$3"
    if grep -q "^${key}=${pattern}" "$PROJECT_ROOT/.env" 2>/dev/null; then
        warn "$key 仍为占位值，请编辑 .env 后重新部署：$hint"
    fi
}
check_env_placeholder FEISHU_APP_ID 'cli_xxx' '飞书应用 App ID'
check_env_placeholder FEISHU_APP_SECRET 'xxx' '飞书应用 App Secret'
check_env_placeholder MINIAGENT_API_KEY 'sk-xxx' 'OpenAI 兼容 API key'
check_env_placeholder IPC_SECRET 'change-me' 'IPC 共享密钥（用 --init 自动生成或 openssl rand -hex 32）'

# 服务部署条件：基于 repo 根 .env 的占位值判定（占位 = 不具备条件）。
# feishu 依赖飞书凭证非占位；miniagent 依赖 MINIAGENT_API_KEY 非占位；
# claude/opencode 无需用户密钥 → 恒具备。
svc_env_ready() {
    local envf="$PROJECT_ROOT/.env"
    case "$1" in
        feishu)
            grep -q '^FEISHU_APP_ID=cli_xxx' "$envf" 2>/dev/null && return 1
            grep -q '^FEISHU_APP_SECRET=xxx' "$envf" 2>/dev/null && return 1
            return 0 ;;
        miniagent)
            grep -q '^MINIAGENT_API_KEY=sk-xxx' "$envf" 2>/dev/null && return 1
            return 0 ;;
        *) return 0 ;;
    esac
}

# 按 env 条件筛选本次选中服务：不具备的停止并禁用现有单元（避免反复重启），
# 并从 SELECTED 剔除。feishu 是前端基础（所有 backend 经 Wants= 依赖它），
# 选中却不具备条件直接 fail——其余 backend 部署了也连不上前端。未选中的
# 服务不触碰（多机分离部署时后端机 .env 无飞书凭证属正常）。
READY=()
for s in "${SELECTED[@]}"; do
    if svc_env_ready "$s"; then READY+=("$s"); continue; fi
    u="$(svc_unit "$s")"
    if [[ "$s" == "feishu" ]]; then
        fail "FEISHU_APP_ID/SECRET 仍为占位值，无法部署前端（所有 backend 依赖前端）。请编辑 .env 填入真实飞书凭证后重试"
    fi
    if systemctl is-active --quiet "$u" 2>/dev/null || systemctl is-enabled --quiet "$u" 2>/dev/null; then
        warn "$s 不具备部署条件（env 占位值），停止并禁用 $u"
        sudo systemctl disable --now "$u" 2>/dev/null || true
    else
        warn "$s 不具备部署条件（env 占位值），跳过"
    fi
done
[[ ${#READY[@]} -gt 0 ]] || fail "选中服务均不具备部署条件"
SELECTED=("${READY[@]}")
rebuild_services

# 基础 config 真源：repo example > tarball 解包的 example（--binaries 部署目标机
# 可能无 repo 源码，仅 tarball + deploy.sh）。不再用 repo root 的 claude-config.json
# ——它不在 git 里（git ls-files 为空），schema 可能滞后，曾让业务 backend 因
# DisallowUnknownFields 启动失败（memory_enabled 字段已被上游删除但本地残留）。
if [[ -f "$PROJECT_ROOT/config.example.json" ]]; then
    cp "$PROJECT_ROOT/config.example.json" "$STAGE/claude-config.json"
elif [[ -f "$BIN_DIR/config.example.json" ]]; then
    cp "$BIN_DIR/config.example.json" "$STAGE/claude-config.json"
else
    fail "找不到 config 基底（config.example.json）"
fi

# log_level 改写为 ${LOG_LEVEL} 占位符：与 ${STATE_DIR} / ${IPC_ADDR} 同一展开
# 机制（进程启动时 config.Load 从 EnvironmentFile 展开，见下方注释）。运维改
# repo 根 .env 的 LOG_LEVEL 重新部署即全服务生效，无需碰 JSON。改的是 STAGE
# 副本，repo 里的基底原样保留；注入后显式校验，防基底缺字段时 sed 静默失败。
# 单引号禁止 shell 展开 ${LOG_LEVEL}（留给 Go config.Load 展开），shellcheck SC2016
# 是预期告警，行内 disable 声明意图。
# shellcheck disable=SC2016
sed -i 's|"log_level"[[:space:]]*:[[:space:]]*"[^"]*"|"log_level":            "${LOG_LEVEL}"|' "$STAGE/claude-config.json"
# shellcheck disable=SC2016
# Verify the log_level placeholder took effect. Use a regex so the assertion
# is robust to whitespace alignment changes in the sed substitution above.
grep -Eq '"log_level"[[:space:]]*:[[:space:]]*"\$\{LOG_LEVEL\}"' "$STAGE/claude-config.json" \
    || fail "log_level 占位符注入失败：$STAGE/claude-config.json 缺少 log_level 字段（注入锚点缺失）"

# state_dir / ipc_addr / frontend_url 已在 config 模板里写成 ${STATE_DIR} / ${IPC_ADDR}
# 占位符，由各进程的 config.Load 在启动时从环境变量展开（见 internal/config 的
# expandEnvVars）。deploy.sh 只需保证 IPC_ADDR / STATE_DIR 进入 EnvironmentFile（见
# 步骤 3 的 .env 写入），无需 sed 改 JSON——既消除字面量替换的元字符转义陷阱，也避免
# sed 静默失败导致 state 分裂。

# 各 backend（claude/opencode/miniagent）注入独立 router_path。三者共享同一个
# state_dir，若用默认的同一 router.v5.json 会互相覆盖会话绑定，故部署脚本
# 显式拆为 claude/opencode/miniagent-router.json（文件名仅本脚本约定，
# 与 config 默认的 router.v5.json 不同；router_path 字段本身可配）。
#
# 可选第 3 参数 backend_id：非空则同时改写 backend_id（opencode/miniagent 派生
# 自 claude-config 时需改）；空则保留基底（claude/feishu）。
# router_path 注入用 sed '/"backend_id"/a\...'：以 backend_id 行为锚点在其后
# 追加。若用户自定义 config 缺 backend_id 字段，sed 静默不追加，回退到同一
# 默认 router.v5.json 会互相覆盖会话绑定，故注入后必须显式校验存在。
inject_router_path() {
    local file="$1" path="$2" backend_id="${3:-}"
    # delete 旧 router_path + 在 backend_id 行后 append 新值，合并为 1 次 sed -i
    # （1 次 fsync）；backend_id 改写条件执行（claude/feishu 派生时为空，跳过）。
    sed -i -e '/"router_path"/d' \
           -e '/"backend_id"/a\  "router_path":  "'"$path"'",' "$file"
    [[ -n "$backend_id" ]] && sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "'"$backend_id"'",|' "$file"
    grep -q '"router_path"' "$file" \
        || fail "router_path 注入失败：$file 缺少 backend_id 字段（注入锚点缺失），backend 将共用默认 router 文件互相覆盖"
}

inject_router_path "$STAGE/claude-config.json" "$STATE_DIR/claude-router.json"

# opencode-back：独立 backend_id + 独立 router_path
cp "$STAGE/claude-config.json" "$STAGE/opencode-config.json"
inject_router_path "$STAGE/opencode-config.json" "$STATE_DIR/opencode-router.json" "opencode-1"

# miniagent-back：独立 backend_id + 独立 router_path（同 opencode 模式）
cp "$STAGE/claude-config.json" "$STAGE/miniagent-config.json"
inject_router_path "$STAGE/miniagent-config.json" "$STATE_DIR/miniagent-router.json" "miniagent-1"

# feishu-front：派生自 claude-config（同一份 base）。注意：所有 backend 共用
# internal/config.Config struct + DisallowUnknownFields，没有"多余字段无害"——
# struct 必须识别 config 的所有 key，否则 parse fail。当前安全只因 struct 是
# example 字段的超集；schema 漂移时会一起失败。
cp "$STAGE/claude-config.json" "$STAGE/feishu-config.json"

info "claude-config / opencode-config / miniagent-config / feishu-config 已生成"

# 移除历史遗留：opencode-serve-back 已从代码库移除（CLI 模式替代），部署时一并
# 清理已存在的 systemd unit + state 文件，避免机器上留下"幽灵服务"反复重启。
# 即使本次 --services 不含相关项，也强制清理一次（升级路径必须收敛到无 unit）。
legacy_unit="lark-opencode-serve-back"
if sudo systemctl list-unit-files 2>/dev/null | grep -q "^${legacy_unit}\.service"; then
    warn "检测到遗留单元 ${legacy_unit}.service（opencode-serve-back 已移除），停止并禁用..."
    sudo systemctl disable --now "$legacy_unit" 2>/dev/null || true
    sudo rm -f "/etc/systemd/system/${legacy_unit}.service"
    sudo systemctl daemon-reload
    info "${legacy_unit}.service 已清理"
fi
# 同时清理遗留的 state 文件（router 持久化 + usage 统计 + session 列表）
for legacy_state in \
    "$STATE_DIR/opencode-serve-router.json" \
    "$STATE_DIR/usage-opencode-serve.json"; do
    if [[ -e "$legacy_state" ]]; then
        sudo rm -f "$legacy_state"
        info "清理遗留状态文件：$legacy_state"
    fi
done
# 遗留 config 模板（CONFIG_DIR 下的派生文件）
if [[ -e "$CONFIG_DIR/opencode-serve-config.json" ]]; then
    sudo rm -f "$CONFIG_DIR/opencode-serve-config.json"
    info "清理遗留配置：$CONFIG_DIR/opencode-serve-config.json"
fi

# claude/opencode/miniagent 三 backend 的 CLI 二进制就绪是对应 backend 部署的硬性
# 条件：CLI 不在 PATH → backend 启动必崩（IsReady 跑 `<cli> --version`），
# systemd 每 5s 重试。提前探测并停禁剔除，避免反复崩溃噪音。放在 stop_services
# 前：停禁先于本次服务重启。
# 迭代中改 SELECTED/SERVICES 不影响本次 for-in（bash 先把数组展开为位置参数）。
for s in "${SELECTED[@]}"; do
    cli="$(svc_cli "$s")"
    [[ -z "$cli" ]] && continue
    if probe_cli "$cli"; then
        info "$s-back 纳入部署（$cli 就绪）"
        continue
    fi
    u="$(svc_unit "$s")"
    warn "$s-back 不具备部署条件（$cli CLI 未就绪），停止并禁用 $u（本次不部署）"
    case "$s" in
        claude)    warn "  安装 Claude Code CLI 后重新部署即可纳入：https://github.com/anthropics/claude-code" ;;
        opencode)  warn "  安装 opencode CLI 后重新部署即可纳入：https://github.com/sst/opencode" ;;
        miniagent) warn "  安装 miniagent CLI 后重新部署即可纳入：https://github.com/justphantom/miniagent" ;;
    esac
    sudo systemctl disable --now "$u" 2>/dev/null || true
    drop_service "$s"
done

# ── 步骤 3：创建目录 + 复制文件 + 修权限 ─────────────
# STATE_DIR/{claude,opencode} 是两个 backend 的 default_directory，
# per-chat 工作目录在运行时由 MkdirAll 在其下自动创建。
info "创建系统目录..."
sudo mkdir -p "$DEPLOY_DIR" "$CONFIG_DIR" "$STATE_DIR/claude" "$STATE_DIR/opencode"

# 必须先停服务，否则覆盖二进制会 "Text file busy"
stop_services

info "复制二进制和配置..."

# 二进制用「写临时文件 + 原子 rename」更新，而非直接 cp 覆盖。rename(2) 替换
# 路径不触发 ETXTBSY——运行中的进程继续持有旧 inode，直到被重启才换上新文件。
# 临时文件落在同一 $DEPLOY_DIR，确保 mv 是同卷 rename（原子、不跨设备）。
# 业务服务已在 stop_services 停止，rename 同样安全。
# 注意：deploy-monitor 的二进制不在此流程内——它由 upgrade-monitor.sh 独立管理。
for s in "${SELECTED[@]}"; do
    u="$(svc_unit "$s")"
    [[ -f "$BIN_DIR/$u" ]] || fail "构建产物缺失：$u（--binaries 产物或 make build 输出不全）"
    sudo cp "$BIN_DIR/$u" "$DEPLOY_DIR/.${u}.new"
    sudo mv -f "$DEPLOY_DIR/.${u}.new" "$DEPLOY_DIR/$u"
done
sudo chmod 755 "$DEPLOY_DIR"/*

# config 是部署产物，每次从 STAGE 覆盖到 CONFIG_DIR
for s in "${SELECTED[@]}"; do
    sudo cp "$STAGE/$(svc_config "$s")" "$CONFIG_DIR/"
done
sudo chmod 600 "$CONFIG_DIR"/*.json

# .env 以 repo 根目录的为唯一真源：每次部署先在 repo 根 .env 上同步本次
# 部署参数（IPC_ADDR / STATE_DIR），再整文件覆盖到 CONFIG_DIR/.env。运维改
# 了 .env 的任何键（凭证、模型、工作区等）重新部署即生效；不再保留
# CONFIG_DIR 上的旧 .env。
# update_env_key 幂等更新一个键：存在用 sed 改整行，不存在追加。
update_env_key() {
    local key="$1" val="$2" file="$3"
    # 内联 sed replacement 转义（& \ 和分隔符 |），避免 PROJECT_ROOT 等路径
    # 含元字符时被解释为反向引用或截断分隔符。仅此一处使用，不抽 helper。
    local esc_val="${val//\\/\\\\}"
    esc_val="${esc_val//&/\\&}"
    esc_val="${esc_val//|/\\|}"
    if sudo grep -q "^${key}=" "$file"; then
        sudo sed -i "s|^${key}=.*|${key}=${esc_val}|" "$file"
    else
        echo "${key}=${val}" | sudo tee -a "$file" > /dev/null
    fi
}
# 先同步部署参数到 repo 根 .env，否则整文件覆盖后 CONFIG_DIR/.env 会丢这两项，
# config 模板里的 ${IPC_ADDR} / ${STATE_DIR} 展开会失败。
update_env_key IPC_ADDR "$IPC_ADDR" "$PROJECT_ROOT/.env"
update_env_key STATE_DIR "$STATE_DIR" "$PROJECT_ROOT/.env"
update_env_key PROJECT_ROOT "$PROJECT_ROOT" "$PROJECT_ROOT/.env"
# LOG_LEVEL：缺省补 info（config 的 ${LOG_LEVEL} 展开对未设/空值报错）；已设值
# 不覆盖，运维调 debug 后重新部署即生效。
if ! grep -q '^LOG_LEVEL=' "$PROJECT_ROOT/.env" 2>/dev/null; then
    update_env_key LOG_LEVEL info "$PROJECT_ROOT/.env"
    warn ".env 缺少 LOG_LEVEL，已追加 LOG_LEVEL=info（改 debug 后重新部署生效）"
fi
# WORKSPACE_ROOT: 如果 .env 里没设或仍是占位值，自动推导为 PROJECT_ROOT 的上一级
# （repo 的父目录，通常是所有项目的公共根）。运维可在 .env 里显式覆盖。
if ! grep -q '^WORKSPACE_ROOT=' "$PROJECT_ROOT/.env" 2>/dev/null || \
   grep -q '^WORKSPACE_ROOT=$\|^WORKSPACE_ROOT=/home/user/your-project' "$PROJECT_ROOT/.env" 2>/dev/null; then
    WORKSPACE_ROOT_DEFAULT="$(dirname "$PROJECT_ROOT")"
    update_env_key WORKSPACE_ROOT "$WORKSPACE_ROOT_DEFAULT" "$PROJECT_ROOT/.env"
    info "WORKSPACE_ROOT 自动设为 $WORKSPACE_ROOT_DEFAULT（PROJECT_ROOT 的上一级）"
fi
# FRONTEND_URL：单机默认 http://$IPC_ADDR。仅当 .env 未设或为空时推导；
# 多机部署由运维显式设前端机可达地址（不被覆盖）。
if ! grep -q '^FRONTEND_URL=' "$PROJECT_ROOT/.env" 2>/dev/null || \
   grep -q '^FRONTEND_URL=$' "$PROJECT_ROOT/.env" 2>/dev/null; then
    update_env_key FRONTEND_URL "http://$IPC_ADDR" "$PROJECT_ROOT/.env"
fi
sudo cp "$PROJECT_ROOT/.env" "$CONFIG_DIR/.env"
sudo chmod 600 "$CONFIG_DIR/.env"
info "已覆盖 $CONFIG_DIR/.env（以 repo 根 .env 为真源）"

info "修复目录和文件权限 → owner=$RUN_USER"
sudo chown -R "$RUN_USER:$RUN_USER" "$DEPLOY_DIR" "$CONFIG_DIR" "$STATE_DIR"

# ── 步骤 4：生成 systemd unit（动态用户）─────────────
info "生成 systemd unit 文件（User=$RUN_USER）..."

for s in "${SELECTED[@]}"; do
    u="$(svc_unit "$s")"
    write_unit "$u" "$(svc_config "$s")" "$(svc_depends "$s")" "" "$(svc_privileged "$s")"
done

# ── 步骤 5：启动（串行：前端先 listen，再起后端）─────
info "启动服务..."
sudo systemctl daemon-reload
# enable 所有服务开机自启，但不 --now；下面显式控制启动顺序。
# 不吞错到 /dev/null：write_unit 已生成文件，enable 真失败说明 systemd 自身
# 异常（路径冲突等），保留 stderr 让 set -e 立即暴露根因。仍 || true：
# 部分 systemctl 版本对已 enabled 的单元返回非 0，不视为部署失败。
sudo systemctl enable "${SERVICES[@]}" || true

# 先起前端（若本次部署含前端），等 IPC 端口 listen，避免后端连不上而崩溃-重启。
# 仅部署 backend 时前端已在运行，跳过等待直接起 backend。
if [[ " ${SELECTED[*]} " == *" feishu "* ]]; then
    front_unit="$(svc_unit feishu)"
    sudo systemctl start "$front_unit"
    wait_active "$front_unit" || fail_after_stop "$front_unit" "$front_unit 启动失败"
    wait_listen || fail_after_stop "$front_unit" "feishu-front IPC 端口 $IPC_ADDR 未 listen，后端无法连接"
fi

# 端口已通，再起选中的 backend（不含 feishu，并行互不依赖）
backends=()
for s in "${SELECTED[@]}"; do
    [[ "$s" == "feishu" ]] && continue
    backends+=("$(svc_unit "$s")")
done
[[ ${#backends[@]} -eq 0 ]] || sudo systemctl start "${backends[@]}"

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

# IPC 就绪+鉴权检查：带 .env 的 IPC_SECRET 请求 /v1/status，期望 200。
# 旧版裸请求期望 401 只能证明"鉴权机制存在"，不能证明"密钥与 .env 一致 →
# 服务真正可用"；带正确 Bearer 得 200 才算鉴权链路通。
ipc_secret="$(env_get IPC_SECRET)"
if [[ -z "$ipc_secret" ]]; then
    echo -e "  ${YELLOW}!${NC} repo 根 .env 无 IPC_SECRET，跳过 IPC 鉴权检查"
else
    code="$(curl -s -o /dev/null -m "$HTTP_TIMEOUT" -w '%{http_code}' -H "Authorization: Bearer $ipc_secret" "http://$IPC_ADDR/v1/status" 2>/dev/null || echo 000)"
    if [[ "$code" == "200" ]]; then
        echo -e "  ${GREEN}✓${NC} IPC ($IPC_ADDR) 带鉴权返回 200（就绪）"
    elif [[ "$code" == "401" ]]; then
        echo -e "  ${YELLOW}!${NC} IPC ($IPC_ADDR) 返回 401（.env 的 IPC_SECRET 与运行中服务不一致）"
    else
        echo -e "  ${YELLOW}!${NC} IPC ($IPC_ADDR) 返回 $code（期望 200）"
    fi
fi

if $all_ok; then
    info "部署完成"
else
    # 失败单元输出最近 10 行日志，避免运维手敲 journalctl；sudo 因为日志归 root。
    for svc in "${SERVICES[@]}"; do
        systemctl is-active --quiet "$svc" 2>/dev/null && continue
        warn "$svc 最近日志（journalctl -u $svc -n 10）："
        sudo journalctl -u "$svc" -n 10 --no-pager 2>/dev/null | sed 's/^/    /' || true
    done
    fail "部分服务启动失败（详见上方 journalctl 摘要）"
fi