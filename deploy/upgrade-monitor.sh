#!/usr/bin/env bash
#
# upgrade-monitor.sh — 独立管理 lark-deploy-monitor 的部署。
#
# 与 deploy.sh 完全解耦：deploy.sh 默认管 4 个业务服务（feishu-front/claude/
# opencode/miniagent），不碰 monitor。monitor 是「部署的触发者」，
# 让它管自己的升级会形成循环依赖，故分离。
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

    # base 固定为 config.example.json（与 repo 同步演进）。不用 repo root 的
    # claude-config.json——它不在 git 里，schema 可能滞后（曾导致 memory_enabled
    # 字段触发 DisallowUnknownFields，monitor 反复重启 19 次）。
    [[ -f "$PROJECT_ROOT/config.example.json" ]] \
        || fail "找不到 base：$PROJECT_ROOT/config.example.json"
    cp "$PROJECT_ROOT/config.example.json" "$stage/$CONFIG_NAME"
    sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "deploy-monitor-1",|' "$stage/$CONFIG_NAME"
    sed -i '/"router_path"/d' "$stage/$CONFIG_NAME"
    # 删 monitor 不消费的业务子块，免疫上游 schema 变动。要求 base 2 空格缩进、
    # 块闭合行 ^  }, 独占——config.example.json 满足。删后显式校验，防 base
    # 格式漂移时 sed 静默失败。
    for block in claude opencode miniagent; do
        sed -i '/^  "'"$block"'":/,/^  },/d' "$stage/$CONFIG_NAME"
        grep -q "\"$block\":" "$stage/$CONFIG_NAME" \
            && fail "清理 $block 块失败：检查 base 是否 2 空格缩进"
    done
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
    # Avoid SC2015 (P && A || B): if `info` ever returned non-zero the fail
    # branch would fire spuriously. Explicit if/else keeps exit-status flow
    # honest.
    if systemctl is-active --quiet "$UNIT_NAME"; then
        info "✓ $UNIT_NAME 已安装并运行"
    else
        fail "$UNIT_NAME 启动失败，检查 journalctl -u $UNIT_NAME"
    fi
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

# ── 配置迁移：清理已部署 config 里残留的、代码侧已移除的字段 ──
# DisallowUnknownFields 模式下，未知字段会让 monitor 反复 crash。init 路径
# 从最新 config.example.json 派生，不会撞坑；但已部署的 /etc config 不会自动
# 同步。升级路径在替换二进制前先迁移，避免每次升级都要人工编辑 config。
#
# 缩进约定与 init 路径一致：base 是 2 空格缩进、块闭合行 ^  }, 独占。
# removed_blocks 增量维护：新增迁移目标在此追加。
migrate_config() {
    local cfg="$CONFIG_DIR/$CONFIG_NAME"
    [[ -f "$cfg" ]] || return 0

    local removed_blocks=("opencode_serve")
    for block in "${removed_blocks[@]}"; do
        sudo grep -q "^  \"$block\":" "$cfg" || continue
        info "迁移：删除残留字段 $block"
        sudo sed -i '/^  "'"$block"'":/,/^  },/d' "$cfg"
        if sudo grep -q "\"$block\":" "$cfg"; then
            fail "清理 $block 失败：$cfg 缩进可能不符 2 空格约定，请手工编辑后重跑"
        fi
        sudo chmod 600 "$cfg"
        sudo chown "$RUN_USER":"$RUN_USER" "$cfg"
    done
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

    # 替换二进制前先迁移 config：迁移失败则 fail 退出，二进制和服务都不动。
    migrate_config

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
    --init) init_monitor ;;
    "")     upgrade_monitor ;;
    *)      fail "未知参数：$1。用法：$0 [--init]" ;;
esac
