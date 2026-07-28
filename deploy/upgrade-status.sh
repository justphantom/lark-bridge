#!/usr/bin/env bash
#
# upgrade-status.sh — 独立管理 lark-status-monitor 的部署。
#
# 与 deploy.sh 完全解耦（与 upgrade-monitor.sh 同模式）：deploy.sh 管 4 个业务
# 服务，不碰 status-monitor。status-monitor 是「观察者」，独立升级避免与业务
# 服务互相牵连；它只读 GET /v1/status 并 push 卡片，无副作用、无需提权。
#
# 用法：
#   ./deploy/upgrade-status.sh           # 升级（构建 + 替换二进制 + restart）
#   ./deploy/upgrade-status.sh --init    # 首次安装（config + unit + enable + start）
#
# 升级时短暂离线 ~2s（systemd restart），期间总览卡停推一帧，下个 tick 自动恢复。
#
set -euo pipefail

# 共享样板：路径 / 颜色 / RUN_USER / info-warn-fail（与 deploy.sh 同源）。
# shellcheck source=deploy/lib-common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-common.sh"

UNIT_NAME="lark-status-monitor"
CONFIG_NAME="status-monitor-config.json"

# ── 构建 ──────────────────────────────────────────────
build_status() {
    info "构建 $UNIT_NAME..."
    make -C "$PROJECT_ROOT" build
    [[ -x "$BIN_DIR/$UNIT_NAME" ]] || fail "构建失败：$BIN_DIR/$UNIT_NAME 不存在"
}

# ── 首次安装：生成 config + 写 unit + enable + start ──
init_status() {
    info "首次安装 $UNIT_NAME..."

    local stage
    stage="$(mktemp -d)"
    trap 'rm -rf "${stage:-}"' EXIT

    # base 固定为 config.example.json（与 repo 同步演进）。status_monitor 块已在
    # base（interval=60s），无需注入；只删 status-monitor 不消费的业务子块。
    [[ -f "$PROJECT_ROOT/config.example.json" ]] \
        || fail "找不到 base：$PROJECT_ROOT/config.example.json"
    cp "$PROJECT_ROOT/config.example.json" "$stage/$CONFIG_NAME"
    sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "status-monitor-1",|' "$stage/$CONFIG_NAME"
    sed -i '/"router_path"/d' "$stage/$CONFIG_NAME"
    # 删 status-monitor 不消费的业务子块（含 deploy_monitor）。要求 base 2 空格缩进、
    # 块闭合行 ^  }, 独占——config.example.json 满足。删后显式校验，防 base 格式
    # 漂移时 sed 静默失败。
    for block in claude opencode miniagent deploy_monitor; do
        sed -i '/^  "'"$block"'":/,/^  },/d' "$stage/$CONFIG_NAME"
        grep -q "\"$block\":" "$stage/$CONFIG_NAME" \
            && fail "清理 $block 块失败：检查 base 是否 2 空格缩进"
    done
    # status_monitor 块必须存活（它是本后端唯一的业务配置）。base 里该块是多行
    # 格式（key 与 interval 不在同一行），不能像 upgrade-monitor 那样单行匹配，
    # 因此用双 token 校验：key 与 interval 都在即视为块存活。
    # shellcheck disable=SC2015  # A && B || fail：fail 必退出，语义正确
    grep -q '"status_monitor"' "$stage/$CONFIG_NAME" \
        && grep -q '"interval"' "$stage/$CONFIG_NAME" \
        || fail "status_monitor 块缺失：$PROJECT_ROOT/config.example.json 需含 status_monitor 段"

    sudo mkdir -p "$CONFIG_DIR"
    sudo cp "$stage/$CONFIG_NAME" "$CONFIG_DIR/"
    sudo chmod 600 "$CONFIG_DIR/$CONFIG_NAME"
    sudo chown "$RUN_USER":"$RUN_USER" "$CONFIG_DIR/$CONFIG_NAME"

    # 二进制
    sudo cp "$BIN_DIR/$UNIT_NAME" "$DEPLOY_DIR/$UNIT_NAME"
    sudo chmod 755 "$DEPLOY_DIR/$UNIT_NAME"

    # unit：无沙箱（与 upgrade-monitor 同结构）。status-monitor 无副作用、不提权，
    # 将来可加硬化（ProtectSystem/NoNewPrivileges 等），但先用简单 unit 保证一致。
    write_status_unit
    sudo systemctl daemon-reload
    sudo systemctl enable "$UNIT_NAME"
    sudo systemctl start "$UNIT_NAME"
    sleep 1
    if systemctl is-active --quiet "$UNIT_NAME"; then
        info "✓ $UNIT_NAME 已安装并运行"
    else
        fail "$UNIT_NAME 启动失败，检查 journalctl -u $UNIT_NAME"
    fi
}

# write_status_unit 写一个 systemd unit。After/Wants feishu-front：status-monitor
# 依赖前端的 GET /v1/status，前端不在线时查询会失败（tick 跳过，下个周期重试）。
write_status_unit() {
    sudo tee "/etc/systemd/system/$UNIT_NAME.service" > /dev/null <<EOF
[Unit]
Description=lark-bridge $UNIT_NAME (overview card pusher)
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
upgrade_status() {
    # 前置检查：unit + config 必须已存在（否则提示先 --init）
    if ! systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
        fail "$UNIT_NAME 未安装。首次部署请用：$0 --init"
    fi
    [[ -f "$CONFIG_DIR/$CONFIG_NAME" ]] \
        || fail "$CONFIG_DIR/$CONFIG_NAME 不存在。首次部署请用：$0 --init"

    build_status

    info "替换二进制（原子 rename）..."
    sudo cp "$BIN_DIR/$UNIT_NAME" "$DEPLOY_DIR/.${UNIT_NAME}.new"
    sudo mv -f "$DEPLOY_DIR/.${UNIT_NAME}.new" "$DEPLOY_DIR/$UNIT_NAME"
    sudo chmod 755 "$DEPLOY_DIR/$UNIT_NAME"

    info "重启 $UNIT_NAME（短暂离线 ~2s）..."
    sudo systemctl restart "$UNIT_NAME"
    sleep 1
    if systemctl is-active --quiet "$UNIT_NAME"; then
        info "✓ $UNIT_NAME 已升级并运行"
    else
        fail "$UNIT_NAME 重启失败，检查 journalctl -u $UNIT_NAME"
    fi
}

# ── main ──────────────────────────────────────────────
case "${1:-}" in
    --init) init_status ;;
    "")     upgrade_status ;;
    *)      fail "未知参数：$1。用法：$0 [--init]" ;;
esac
