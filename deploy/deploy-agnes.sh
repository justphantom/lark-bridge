#!/usr/bin/env bash
#
# deploy-agnes.sh — 独立管理 lark-agnes-back 的部署。
#
# 与 deploy.sh 完全解耦（与 deploy-monitor.sh / deploy-status.sh 同模式）：
# deploy.sh 默认管 3 个业务服务（feishu-front/claude/miniagent），不碰 agnes。
# agnes-back 是 Agnes AI 图片/视频生成后端，HTTP 直调、无副作用、不需提权，
# 独立升级避免与业务服务互相牵连。
#
# 用法：
#   ./deploy/deploy-agnes.sh           # 升级（构建 + 替换二进制 + restart）
#   ./deploy/deploy-agnes.sh --init    # 首次安装（config + unit + enable + start）
#
# 升级时短暂离线 ~2s（systemd restart），期间 /image /video 指令不可达。
#
set -euo pipefail

# 共享样板：路径 / 颜色 / RUN_USER / info-warn-fail（与 deploy.sh 同源）。
# shellcheck source=deploy/lib-common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-common.sh"

UNIT_NAME="lark-agnes-back"
CONFIG_NAME="agnes-back-config.json"

# ── 构建 ──────────────────────────────────────────────
build_agnes() {
    info "构建 $UNIT_NAME..."
    make -C "$PROJECT_ROOT" build-agnes-back
    [[ -x "$BIN_DIR/$UNIT_NAME" ]] || fail "构建失败：$BIN_DIR/$UNIT_NAME 不存在"
}

# ── 首次安装：生成 config + 写 unit + enable + start ──
init_agnes() {
    info "首次安装 $UNIT_NAME..."

    build_agnes

    local stage
    stage="$(mktemp -d)"
    trap 'rm -rf "${stage:-}"' EXIT

    # base 固定为 config.example.json（与 repo 同步演进）。agnes 块已在 base
    # （api_key/base_url/三模型名/size/ratio），无需注入；只删 agnes 不消费的
    # 业务子块，免疫上游 schema 变动。
    [[ -f "$PROJECT_ROOT/config.example.json" ]] \
        || fail "找不到 base：$PROJECT_ROOT/config.example.json"
    cp "$PROJECT_ROOT/config.example.json" "$stage/$CONFIG_NAME"
    sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "agnes-1",|' "$stage/$CONFIG_NAME"
    sed -i '/"router_path"/d' "$stage/$CONFIG_NAME"
    # 删 agnes 不消费的业务子块（claude/miniagent/deploy_monitor）。要求 base 2 空格
    # 缩进、块闭合行 ^  }, 独占——config.example.json 满足。删后显式校验，防 base
    # 格式漂移时 sed 静默失败。
    for block in claude miniagent deploy_monitor; do
        sed -i '/^  "'"$block"'":/,/^  },/d' "$stage/$CONFIG_NAME"
        grep -q "\"$block\":" "$stage/$CONFIG_NAME" \
            && fail "清理 $block 块失败：检查 base 是否 2 空格缩进"
    done
    # agnes 块必须存活（它是本后端唯一的业务配置）。base 里该块是多行格式，
    # 用双 token 校验：api_key 与 base_url 都在即视为块存活。
    # shellcheck disable=SC2015  # A && B || fail：fail 必退出，语义正确
    grep -q '"agnes"' "$stage/$CONFIG_NAME" \
        && grep -q '"api_key"' "$stage/$CONFIG_NAME" \
        && grep -q '"base_url"' "$stage/$CONFIG_NAME" \
        || fail "agnes 块缺失：$PROJECT_ROOT/config.example.json 需含 agnes 段"

    sudo mkdir -p "$CONFIG_DIR"
    sudo cp "$stage/$CONFIG_NAME" "$CONFIG_DIR/"
    sudo chmod 600 "$CONFIG_DIR/$CONFIG_NAME"
    sudo chown "$RUN_USER":"$RUN_USER" "$CONFIG_DIR/$CONFIG_NAME"

    # 二进制
    sudo cp "$BIN_DIR/$UNIT_NAME" "$DEPLOY_DIR/$UNIT_NAME"
    sudo chmod 755 "$DEPLOY_DIR/$UNIT_NAME"

    # unit：无沙箱（与 deploy-status 同结构）。agnes-back 无副作用、不提权。
    write_agnes_unit
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

# write_agnes_unit 写一个 systemd unit。After/Wants feishu-front：agnes-back
# 依赖前端的 SSE/POST 通道转发指令与结果，前端不在线时指令不可达（SSE 连不上
# 会 crash-loop 重连，前端起来后自动恢复）。
write_agnes_unit() {
    sudo tee "/etc/systemd/system/$UNIT_NAME.service" > /dev/null <<EOF
[Unit]
Description=lark-bridge $UNIT_NAME (Agnes AI image/video generator)
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
# DisallowUnknownFields 模式下，未知字段会让 agnes-back 反复 crash。init 路径
# 从最新 config.example.json 派生，不会撞坑；但已部署的 /etc config 不会自动
# 同步。升级路径在替换二进制前先迁移，避免每次升级都要人工编辑 config。
# 与 deploy-monitor.sh / deploy-status.sh 的 migrate_config 同构；removed_blocks
# 增量维护。
migrate_config() {
    local cfg="$CONFIG_DIR/$CONFIG_NAME"
    [[ -f "$cfg" ]] || return 0

    local removed_blocks=("opencode_serve" "opencode" "omp" "claude")
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
upgrade_agnes() {
    # 前置检查：unit + config 必须已存在（否则提示先 --init）
    if ! systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
        fail "$UNIT_NAME 未安装。首次部署请用：$0 --init"
    fi
    [[ -f "$CONFIG_DIR/$CONFIG_NAME" ]] \
        || fail "$CONFIG_DIR/$CONFIG_NAME 不存在。首次部署请用：$0 --init"

    build_agnes

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
main() {
    case "${1:-}" in
        --help|-h)
            # 打印文件头部注释块（shebang 到首行非注释）作为 usage。
            awk 'NR==1{next} /^#!/{next} /^[^#]/{exit} {sub(/^#[[:space:]]?/,""); print}' "$0"
            exit 0 ;;
        --init) init_agnes ;;
        "")     upgrade_agnes ;;
        *)      fail "未知参数：$1。用法：$0 [--init | --help]" ;;
    esac
}

# Source guard: allow sourcing for tests without executing the deploy flow.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "${1:-}"
fi
