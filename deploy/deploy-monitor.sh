#!/usr/bin/env bash
#
# deploy-monitor.sh — 独立管理 lark-deploy-monitor 的部署。
#
# 与 deploy.sh 完全解耦：deploy.sh 默认管 3 个业务服务（feishu-front/claude/
# miniagent），不碰 monitor。monitor 是「部署的触发者」，
# 让它管自己的升级会形成循环依赖，故分离。
#
# 用法：
#   ./deploy/deploy-monitor.sh           # 升级（构建 + 替换二进制 + restart）
#   ./deploy/deploy-monitor.sh --init    # 首次安装（config + unit + enable + start）
#
# monitor 升级时短暂离线 ~2s（systemd restart），期间 /deploy 不可达。
# monitor 代码极少变更（统计上远低于业务服务），这个代价可接受。
#
set -euo pipefail

# 共享样板：路径 / 颜色 / RUN_USER / info-warn-fail（与 deploy.sh 同源）。
# shellcheck source=deploy/lib-common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-common.sh"

UNIT_NAME="lark-deploy-monitor"
CONFIG_NAME="deploy-monitor-config.json"

# guard_pro_mode skips deploy-monitor installation in pro mode. A clean dev→pro
# transition disables any previously-installed unit so "no deploy backend" is
# actually true; otherwise a stale monitor would keep responding to /deploy.
guard_pro_mode() {
    local mode; mode="$(run_mode)"
    case "$mode" in
        dev)  return 0 ;;
        pro)
            info "LARK_RUN_MODE=pro：跳过 $UNIT_NAME 部署（deploy 后端不部署）"
            if systemctl is-enabled --quiet "$UNIT_NAME" 2>/dev/null; then
                info "停用已存在的 $UNIT_NAME（dev→pro 切换）..."
                sudo systemctl disable --now "$UNIT_NAME" 2>/dev/null || true
            fi
            exit 0 ;;
        *)  fail "LARK_RUN_MODE 非法值：$mode（仅支持 dev / pro）" ;;
    esac
}
guard_pro_mode

# ── 构建 ──────────────────────────────────────────────
build_monitor() {
    info "构建 $UNIT_NAME..."
    make -C "$PROJECT_ROOT" build-deploy-monitor
    [[ -x "$BIN_DIR/$UNIT_NAME" ]] || fail "构建失败：$BIN_DIR/$UNIT_NAME 不存在"
}

# ── 首次安装：生成 config + 写 unit + enable + start ──
init_monitor() {
    info "首次安装 $UNIT_NAME..."

    # config：从 repo 的基础 config 派生，设独立 backend_id + 注入 deploy_monitor 块。
    local stage
    stage="$(mktemp -d)"
    # 用全局变量，确保 EXIT trap 能访问（local 在函数返回后出作用域 → unbound variable）
    trap 'rm -rf "$INIT_STAGE"' EXIT
    INIT_STAGE="$stage"

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
    for block in claude miniagent; do
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
    # wait_active 轮询 ~15s（lib-common），覆盖冷启动窗口；取代固定 sleep 1 +
    # 单次 is-active——后者在冷启动窗口内必误判失败。
    if wait_active "$UNIT_NAME"; then
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
# cgroup 内存回收：make deploy 子进程读取的文件页（go-build 缓存、源码、
# docker 层）作为 inactive_file 留在本 cgroup 的内核记账里，systemctl
# 报告的 Memory 长期 80M+ 不回落（进程本体 anon 仅 7M）。MemoryHigh 让
# 内核在超过阈值时主动 reclaim inactive 页，idle 回落到 ~10-15M；MemoryMax
# 是硬上限，防一次失控 deploy 把整机吃满。MemoryPeak 实测 ~207M，留出
# 余量到 300M。
MemoryHigh=50M
MemoryMax=300M

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

# ── unit 迁移：给已部署 unit 注入 cgroup 内存回收 ──
# 根因见 write_monitor_unit 的 MemoryHigh 注释：make deploy 后 80M+ 的
# 文件页缓存留在本 cgroup 不回收（进程本体 anon 仅 7M）。仅在缺失时
# 注入，保证可重入；插入到 User= 行之后，与 init 路径写出的 unit 同构。
migrate_unit() {
    local unit="/etc/systemd/system/$UNIT_NAME.service"
    [[ -f "$unit" ]] || return 0
    if sudo grep -q '^MemoryHigh=' "$unit"; then
        return 0
    fi
    info "迁移：注入 MemoryHigh/MemoryMax 到 $unit..."
    if ! sudo sed -i '/^User=/a MemoryHigh=50M\nMemoryMax=300M' "$unit"; then
        fail "注入 MemoryHigh/MemoryMax 失败：请手工编辑 $unit 后重跑"
    fi
    sudo systemctl daemon-reload
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

    # 同步给已部署 unit 注入 MemoryHigh/MemoryMax（缺失才注入，可重入）。
    # daemon-reload 在 migrate_unit 内部按需触发。
    migrate_unit

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
        --init) init_monitor ;;
        "")     upgrade_monitor ;;
        *)      fail "未知参数：$1。用法：$0 [--init | --help]" ;;
    esac
}

# Source guard: allow sourcing for tests without executing the deploy flow.
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "${1:-}"
fi
