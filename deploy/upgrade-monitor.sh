#!/usr/bin/env bash
#
# upgrade-monitor.sh — 独立管理 lark-deploy-monitor 的部署。
#
# 与 deploy.sh 完全解耦：deploy.sh 只管 4 个业务服务（feishu-front/claude/
# opencode/miniagent），不碰 monitor。monitor 是「部署的触发者」，让它管自己
# 的升级会形成循环依赖，故分离。
#
# 用法：
#   ./deploy/upgrade-monitor.sh           # 升级（构建 + 替换二进制 + restart）
#   ./deploy/upgrade-monitor.sh --init    # 首次安装（config + unit + enable + start）
#
# monitor 升级时短暂离线 ~2s（systemd restart），期间 /deploy 不可达。
# monitor 代码极少变更（统计上远低于业务服务），这个代价可接受。
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$PROJECT_ROOT/bin"
DEPLOY_DIR="/opt/lark-bridge/bin"
CONFIG_DIR="/etc/lark-bridge"

UNIT_NAME="lark-deploy-monitor"
CONFIG_NAME="deploy-monitor-config.json"

# 颜色（与 deploy.sh 一致）
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
info() { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail() { echo -e "${RED}[FAIL]${NC}  $*" >&2; exit 1; }

# RUN_USER 还原（与 deploy.sh 同逻辑）
if [[ "$EUID" -eq 0 ]]; then
    RUN_USER="${SUDO_USER:-}"
    [[ -n "$RUN_USER" ]] || fail "请勿直接以 root 运行；用 sudo -E 保证 SUDO_USER 可用"
else
    RUN_USER="$(whoami)"
fi

# ── 构建 ──────────────────────────────────────────────
build_monitor() {
    info "构建 $UNIT_NAME..."
    make -C "$PROJECT_ROOT" build
    [[ -x "$BIN_DIR/$UNIT_NAME" ]] || fail "构建失败：$BIN_DIR/$UNIT_NAME 不存在"
}

# ── 首次安装：生成 config + 写 unit + enable + start ──
init_monitor() {
    info "首次安装 $UNIT_NAME..."

    # config：从 repo 的基础 config 派生，设独立 backend_id + 注入 deploy_monitor 块。
    local stage
    stage="$(mktemp -d)"
    trap 'rm -rf "$stage"' EXIT

    if [[ -f "$PROJECT_ROOT/claude-config.json" ]]; then
        cp "$PROJECT_ROOT/claude-config.json" "$stage/base.json"
    else
        cp "$PROJECT_ROOT/config.example.json" "$stage/base.json"
    fi
    cp "$stage/base.json" "$stage/$CONFIG_NAME"
    sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "deploy-monitor-1",|' "$stage/$CONFIG_NAME"
    sed -i '/"router_path"/d' "$stage/$CONFIG_NAME"
    # 先删既有 deploy_monitor 块（防重复键 → Go json 取最后一个 → 占位覆盖注入值）
    sed -i '/"deploy_monitor"[[:space:]]*:/,/^[[:space:]]*}/d' "$stage/$CONFIG_NAME"
    sed -i '/"backend_id"/a\  "deploy_monitor": {"project_root": "'"$PROJECT_ROOT"'", "deploy_target": "deploy"},' "$stage/$CONFIG_NAME"
    grep -q '"deploy_monitor"[[:space:]]*:[[:space:]]*{"project_root"' "$stage/$CONFIG_NAME" \
        || fail "deploy_monitor 注入失败：$stage/$CONFIG_NAME 缺少 backend_id（注入锚点缺失）"

    sudo mkdir -p "$CONFIG_DIR"
    sudo cp "$stage/$CONFIG_NAME" "$CONFIG_DIR/"
    sudo chmod 600 "$CONFIG_DIR/$CONFIG_NAME"
    sudo chown "$RUN_USER":"$RUN_USER" "$CONFIG_DIR/$CONFIG_NAME"

    # 二进制
    sudo cp "$BIN_DIR/$UNIT_NAME" "$DEPLOY_DIR/$UNIT_NAME"
    sudo chmod 755 "$DEPLOY_DIR/$UNIT_NAME"

    # unit：privileged（无沙箱），因为 monitor 要 sudo 跑 make deploy
    write_monitor_unit
    sudo systemctl daemon-reload
    sudo systemctl enable "$UNIT_NAME"
    sudo systemctl start "$UNIT_NAME"
    sleep 1
    systemctl is-active --quiet "$UNIT_NAME" \
        && info "✓ $UNIT_NAME 已安装并运行" \
        || fail "$UNIT_NAME 启动失败，检查 journalctl -u $UNIT_NAME"
}

# write_monitor_unit 写一个无沙箱的 systemd unit（monitor 需要 sudo 提权）。
write_monitor_unit() {
    sudo tee "/etc/systemd/system/$UNIT_NAME.service" > /dev/null <<EOF
[Unit]
Description=lark-bridge $UNIT_NAME
After=lark-feishu-front.service
Wants=lark-feishu-front.service

[Service]
EnvironmentFile=$CONFIG_DIR/.env
ExecStart=$DEPLOY_DIR/$UNIT_NAME -config $CONFIG_DIR/$CONFIG_NAME
Restart=on-failure
RestartSec=5
TimeoutStopSec=10
User=$RUN_USER

[Install]
WantedBy=multi-user.target
EOF
}

# ── 升级：替换二进制 + restart ────────────────────────
upgrade_monitor() {
    # 前置检查：unit + config 必须已存在（否则提示先 --init）
    if ! systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
        fail "$UNIT_NAME 未安装。首次部署请用：$0 --init"
    fi
    [[ -f "$CONFIG_DIR/$CONFIG_NAME" ]] \
        || fail "$CONFIG_DIR/$CONFIG_NAME 不存在。首次部署请用：$0 --init"

    build_monitor

    info "替换二进制（原子 rename）..."
    sudo cp "$BIN_DIR/$UNIT_NAME" "$DEPLOY_DIR/.${UNIT_NAME}.new"
    sudo mv -f "$DEPLOY_DIR/.${UNIT_NAME}.new" "$DEPLOY_DIR/$UNIT_NAME"
    sudo chmod 755 "$DEPLOY_DIR/$UNIT_NAME"

    info "重启 $UNIT_NAME（短暂离线 ~2s）..."
    sudo systemctl restart "$UNIT_NAME"
    sleep 1
    systemctl is-active --quiet "$UNIT_NAME" \
        && info "✓ $UNIT_NAME 已升级并运行" \
        || fail "$UNIT_NAME 重启失败，检查 journalctl -u $UNIT_NAME"
}

# ── main ──────────────────────────────────────────────
case "${1:-}" in
    --init) init_monitor ;;
    "")     upgrade_monitor ;;
    *)      fail "未知参数：$1。用法：$0 [--init]" ;;
esac
