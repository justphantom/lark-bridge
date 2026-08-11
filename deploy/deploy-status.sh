#!/usr/bin/env bash
#
# deploy-status.sh — 独立管理 lark-status-monitor 的部署。
#
# 与 deploy.sh 完全解耦：deploy.sh 管 3 个业务
# 服务，不碰 status-monitor。status-monitor 是「观察者」，独立升级避免与业务
# 服务互相牵连；它只读 GET /v1/status 并 push 卡片，无副作用、无需提权。
#
# 用法：
#   ./deploy/deploy-status.sh           # 升级（构建 + 替换二进制 + restart）
#   ./deploy/deploy-status.sh --init    # 首次安装（config + unit + enable + start）
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
    make -C "$PROJECT_ROOT" build-status-monitor
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
    # 删 status-monitor 不消费的业务子块。要求 base 2 空格缩进、
    # 块闭合行 ^  }, 独占——config.example.json 满足。删后显式校验，防 base 格式
    # 漂移时 sed 静默失败。
    for block in claude miniagent; do
        sed -i '/^  "'"$block"'":/,/^  },/d' "$stage/$CONFIG_NAME"
        grep -q "\"$block\":" "$stage/$CONFIG_NAME" \
            && fail "清理 $block 块失败：检查 base 是否 2 空格缩进"
    done
    # status_monitor 块必须存活（它是本后端唯一的业务配置）。base 里该块是多行
    # 格式（key 与 interval 不在同一行），因此用双 token 校验：key 与 interval
    # 都在即视为块存活。
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

    # unit：无沙箱。status-monitor 无副作用、不提权，
    # 将来可加硬化（ProtectSystem/NoNewPrivileges 等），但先用简单 unit 保证一致。
    write_status_unit
    sudo systemctl daemon-reload
    sudo systemctl enable "$UNIT_NAME"
    # wait_active 轮询 ~15s（lib-common），覆盖冷启动窗口；取代固定 sleep 1 +
    # 单次 is-active——后者在冷启动窗口内必误判失败。
    if wait_active "$UNIT_NAME"; then
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

# ── 配置迁移：清理已部署 config 里残留的、代码侧已移除的字段 ──
# DisallowUnknownFields 模式下，未知字段会让 status-monitor 反复 crash。init
# 路径从最新 config.example.json 派生，不会撞坑；但已部署的 /etc config 不会
# 自动同步。升级路径在替换二进制前先迁移，避免每次升级都要人工编辑 config。
# removed_blocks 增量维护：删后端时在此追加其 config 块名（已部署 config 迁移用）。
migrate_config() {
    local cfg="$CONFIG_DIR/$CONFIG_NAME"
    [[ -f "$cfg" ]] || return 0

    local removed_blocks=("opencode_serve" "opencode" "omp" "claude" "agnes")
    for block in "${removed_blocks[@]}"; do
        sudo grep -q "^  \"$block\":" "$cfg" || continue
        info "迁移：删除残留字段 $block"
        sudo sed -i '/^  "'"$block"'":/,/^  },/d' "$cfg"
        if sudo grep -q "\"$block\":" "$cfg"; then
            fail "清理 $block 失败：$cfg 缩进可能不符 2 空格约定，请手工编辑后重跑"
        fi
    done

    # 块内叶子字段：timeouts/card_patch_delay、根级 feishu_card_engine 等。
    # 整块迁移只删 removed_blocks（完整 JSON 对象），但这些 key 是存活块
    # （timeouts）内的单个叶子，不能用块删除的 sed 范围匹配。
    # 用 python regex 删行 + 修复悬空尾逗号 + json.dump 美化，一步到位；
    # 避免 sed 删行后 json.load 因悬空逗号失败（先有鸡还是先有蛋）。
    local removed_keys=("card_patch_delay" "feishu_card_engine")
    local need_migrate=0
    for key in "${removed_keys[@]}"; do
        sudo grep -q "\"$key\"" "$cfg" && need_migrate=1
    done
    if [[ "$need_migrate" -eq 1 ]]; then
        info "迁移：清理已移除的叶子字段"
        sudo python3 -c '
import re, json, sys
p = sys.argv[1]
raw = open(p).read()
for key in sys.argv[2:]:
    raw = re.sub(r"^[ \t]*\"" + key + r"\".*$\n?", "", raw, flags=re.MULTILINE)
raw = re.sub(r",(\s*[}\]])", r"\1", raw)
data = json.loads(raw)
json.dump(data, open(p, "w"), indent=2, ensure_ascii=False)
        ' "$cfg" "${removed_keys[@]}" || fail "config JSON 迁移失败：请手工编辑 $cfg"
    fi

    sudo chmod 600 "$cfg"
    sudo chown "$RUN_USER":"$RUN_USER" "$cfg"
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

    # 替换二进制前先迁移 config：迁移失败则 fail 退出，二进制和服务都不动。
    migrate_config

    # 复制 .env 到 CONFIG_DIR（与 deploy.sh 同逻辑：repo-root .env 是 source of truth）
    info "复制环境变量配置..."
    sudo cp "$PROJECT_ROOT/.env" "$CONFIG_DIR/.env"
    sudo chmod 600 "$CONFIG_DIR/.env"
    sudo chown "$RUN_USER":"$RUN_USER" "$CONFIG_DIR/.env"

    info "替换二进制（原子 rename）..."
    sudo cp "$BIN_DIR/$UNIT_NAME" "$DEPLOY_DIR/.${UNIT_NAME}.new"
    sudo mv -f "$DEPLOY_DIR/.${UNIT_NAME}.new" "$DEPLOY_DIR/$UNIT_NAME"
    sudo chmod 755 "$DEPLOY_DIR/$UNIT_NAME"

    info "重启 $UNIT_NAME（短暂离线 ~2s）..."
    sudo systemctl restart "$UNIT_NAME"
    if wait_active "$UNIT_NAME"; then
        info "✓ $UNIT_NAME 已升级并运行"
    else
        fail "$UNIT_NAME 重启失败，检查 journalctl -u $UNIT_NAME"
    fi
}

# ── main ──────────────────────────────────────────────
case "${1:-}" in
    --help|-h)
        awk 'NR==1{next} /^#!/{next} /^[^#]/{exit} {sub(/^#[[:space:]]?/,""); print}' "$0"
        exit 0 ;;
    --init) init_status ;;
    "")     upgrade_status ;;
    *)      fail "未知参数：$1。用法：$0 [--init | --help]" ;;
esac
