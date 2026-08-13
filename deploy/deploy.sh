#!/usr/bin/env bash
#
# lark-bridge one-shot deployment script (systemd). Deploys ALL 3 services
# (feishu-front + miniagent-back + status-monitor) in a single run.
#
# Usage:
#   ./deploy/deploy.sh            # deploy all 3 services
#   ./deploy/deploy.sh --init     # first-time: auto-generate .env from example
#   ./deploy/deploy.sh --force    # skip in-flight session check
#   ./deploy/deploy.sh --binaries <tar|dir>  # use pre-built tarball/dir
#
# Build is a separate concern: run `make build` first. Without --binaries,
# deploy.sh expects the 3 binaries to already exist under bin/ and fails
# fast otherwise (it never compiles).
#
set -euo pipefail

# shellcheck source=deploy/lib-common.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-common.sh"

set -E
trap 'echo -e "${RED}[FAIL]${NC} error at line $LINENO: $BASH_COMMAND" >&2' ERR

# -- IPC address (env var > .env > default) -------------------------------------
IPC_ADDR="${IPC_ADDR:-$(env_get IPC_ADDR)}"
IPC_ADDR="${IPC_ADDR:-localhost:6060}"

# -- State ---------------------------------------------------------------------
BINARIES_SRC=""
INIT=false
FORCE=false
DEBUG=false
STAGE=""

# The 3 services, always deployed together.
ALL_UNITS=(lark-feishu-front lark-miniagent-back lark-status-monitor)
ALL_CONFIGS=(feishu-config.json miniagent-config.json status-monitor-config.json)

# -- Args ----------------------------------------------------------------------
parse_args() {
    local prev="" arg
    for arg in "$@"; do
        if [[ -n "$prev" ]]; then
            case "$prev" in --binaries) BINARIES_SRC="$arg" ;; esac
            prev=""; continue
        fi
        case "$arg" in
            --init)     INIT=true ;;
            --force)    FORCE=true ;;
            --debug)    DEBUG=true ;;
            --help|-h)  awk 'NR==1{next} /^#!/{next} /^[^#]/{exit} {sub(/^#[[:space:]]?/,""); print}' "$0" | sed 's/^$//'; exit 0 ;;
            --binaries) prev="$arg" ;;
            --binaries=*) BINARIES_SRC="${arg#*=}" ;;
            *) fail "Unknown argument: $arg (valid: --init --force --debug --help --binaries <path>)" ;;
        esac
    done
    [[ -z "$prev" ]] || fail "${prev} requires an argument"
}

# -- Preflight -----------------------------------------------------------------
deploy_sudo_check() {
    if sudo -n true >/dev/null 2>&1; then
        info "$INVOKER_USER has passwordless sudo"
    else
        fail "$INVOKER_USER lacks passwordless sudo (NOPASSWD required). Grant e.g. '$INVOKER_USER ALL=(ALL) NOPASSWD: ALL' under /etc/sudoers.d/lark-bridge."
    fi
}

preflight_toolchain() {
    if [[ -z "$BINARIES_SRC" ]]; then
        [[ -f "$PROJECT_ROOT/Makefile" ]] || fail "Makefile not found; run from the repo root"
        command -v go   >/dev/null || fail "Go is not installed"
        command -v make >/dev/null || fail "make is not installed"
    fi
}

# Check if deploying would disrupt in-flight sessions (GET /v1/deploy-preflight).
# Hardcoded services=feishu,miniagent (status-monitor has no turns to disrupt).
preflight_inflight_check() {
    if ! systemctl is-active --quiet lark-feishu-front 2>/dev/null; then return 0; fi
    local secret
    secret="$(env_get IPC_SECRET)"
    [[ -z "$secret" ]] && { warn "IPC_SECRET missing; skipping in-flight check"; return 0; }
    local resp body code
    resp="$(curl -s -m "$HTTP_TIMEOUT" -w $'\n%{http_code}' -H "Authorization: Bearer $secret" \
        "http://$IPC_ADDR/v1/deploy-preflight?services=feishu,miniagent" 2>/dev/null || true)"
    code="$(tail -1 <<<"$resp")"
    [[ "$code" =~ ^[0-9]+$ ]] || code=000
    body="$(sed '$d' <<<"$resp")"
    case "$code" in
        000) return 0 ;;
        401) fail "IPC 401 (.env IPC_SECRET mismatch with running service)" ;;
        200) info "No in-flight sessions; safe to deploy"; return 0 ;;
        409) local reason; reason="$(grep -oE '"reason":"[^"]*"' <<<"$body" | head -1 | cut -d'"' -f4)"
             fail "Preflight rejected: ${reason:-in-flight sessions disrupted}. Retry when done (or --force)." ;;
        404) preflight_inflight_check_legacy ;;
        *)   warn "Unexpected preflight status $code; skipping"; return 0 ;;
    esac
}

# Fallback for frontends older than /v1/deploy-preflight.
preflight_inflight_check_legacy() {
    local secret resp code body inflight
    secret="$(env_get IPC_SECRET)"
    resp="$(curl -s -m "$HTTP_TIMEOUT" -w $'\n%{http_code}' -H "Authorization: Bearer $secret" "http://$IPC_ADDR/v1/status" 2>/dev/null || true)"
    code="$(tail -1 <<<"$resp")"; [[ "$code" =~ ^[0-9]+$ ]] || code=000
    body="$(sed '$d' <<<"$resp")"
    [[ "$code" == "000" ]] && return 0
    [[ "$code" != "200" ]] && { warn "IPC /v1/status $code; skipping"; return 0; }
    inflight="$(grep -oE '"inflight":[0-9]+' <<<"$body" | head -1 | cut -d: -f2 || echo 0)"
    [[ "${inflight:-0}" -gt 0 ]] && fail "$inflight in-flight session(s) (old frontend); aborting. Retry when done (or --force)."
    info "No in-flight sessions; safe to deploy"
}

# -- Binaries -------------------------------------------------------------------
# ensure_binaries never compiles — building is `make build`'s job. Without
# --binaries it only asserts the bin/ artifacts already exist; with --binaries
# it unpacks the pre-built tarball/dir into bin/. verify_artifacts then guards
# the full unit set either way.
ensure_binaries() {
    mkdir -p "$BIN_DIR"
    if [[ -z "$BINARIES_SRC" ]]; then
        info "Using existing binaries in $BIN_DIR (build via 'make build' if stale)"
        verify_artifacts
        return
    fi
    if [[ -f "$BINARIES_SRC" ]]; then
        info "Extracting binaries from tarball: $BINARIES_SRC"
        tar -xzf "$BINARIES_SRC" -C "$BIN_DIR"
    elif [[ -d "$BINARIES_SRC" ]]; then
        info "Copying binaries from directory: $BINARIES_SRC"
        cp "$BINARIES_SRC"/lark-* "$BIN_DIR/" 2>/dev/null || cp "$BINARIES_SRC"/* "$BIN_DIR/"
    else
        fail "--binaries path does not exist: $BINARIES_SRC"
    fi
    chmod 755 "$BIN_DIR"/lark-* 2>/dev/null || true
}

verify_artifacts() {
    local u missing=()
    for u in "${ALL_UNITS[@]}"; do
        [[ -x "$BIN_DIR/$u" ]] || missing+=("$u")
    done
    if ((${#missing[@]})); then
        fail "Binary artifact(s) missing: ${missing[*]}. Run 'make build' first (or pass --binaries <tar|dir>)."
    fi
}

# -- .env ----------------------------------------------------------------------
check_env_placeholder() {
    local key="$1" pattern="$2" hint="$3"
    if grep -q "^${key}=${pattern}" "$PROJECT_ROOT/.env" 2>/dev/null; then
        warn "$key is still placeholder; edit .env: $hint"
    fi
}

init_env() {
    if $INIT; then
        if [[ ! -f "$PROJECT_ROOT/.env" ]]; then
            if [[ -f "$PROJECT_ROOT/deploy/env.example" ]]; then
                cp "$PROJECT_ROOT/deploy/env.example" "$PROJECT_ROOT/.env"
            elif [[ -f "$BIN_DIR/env.example" ]]; then
                cp "$BIN_DIR/env.example" "$PROJECT_ROOT/.env"
            else
                fail "env.example not found"
            fi
        fi
        if grep -q '^IPC_SECRET=change-me' "$PROJECT_ROOT/.env" 2>/dev/null; then
            local _secret; _secret="$(openssl rand -hex 32)"
            sed -i "s|^IPC_SECRET=.*|IPC_SECRET=$_secret|" "$PROJECT_ROOT/.env"
            info "Generated IPC_SECRET"
        fi
        warn "Feishu credentials and secrets in .env need manual editing"
    fi
    [[ -f "$PROJECT_ROOT/.env" ]] || fail ".env not found (use --init to generate)"
}

backfill_env() {
    if [[ -f "$PROJECT_ROOT/deploy/env.example" ]]; then
        local line key
        while IFS= read -r line; do
            [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)= ]] || continue
            key="${BASH_REMATCH[1]}"
            grep -q "^${key}=" "$PROJECT_ROOT/.env" && continue
            printf '%s\n' "$line" >> "$PROJECT_ROOT/.env"
            info "Backfilled $key (from env.example)"
        done < "$PROJECT_ROOT/deploy/env.example"
    fi
}

# -- Config staging ------------------------------------------------------------
# All 3 configs derive from config.example.json. Only miniagent gets extra
# injections (router_path, backend_id, config_dir, miniagent-cli.json).
# No migration needed: fresh-from-example configs never carry legacy fields
# (DisallowUnknownFields accepts known fields like miniagent even for status-monitor).
stage_configs() {
    info "Preparing config files..."
    STAGE="$(mktemp -d)"
    trap 'rm -rf "$STAGE"; sudo rm -f "$DEPLOY_DIR"/.lark-*.new 2>/dev/null || true' EXIT

    init_env
    backfill_env
    check_env_placeholder FEISHU_APP_ID 'cli_xxx' 'Feishu App ID'
    check_env_placeholder FEISHU_APP_SECRET 'xxx' 'Feishu App Secret'
    check_env_placeholder MINIAGENT_API_KEY 'sk-xxx' 'API key'
    check_env_placeholder IPC_SECRET 'change-me' 'IPC secret'

    # Base config from example (repo > tarball).
    if [[ -f "$PROJECT_ROOT/config.example.json" ]]; then
        cp "$PROJECT_ROOT/config.example.json" "$STAGE/base-config.json"
    elif [[ -f "$BIN_DIR/config.example.json" ]]; then
        cp "$BIN_DIR/config.example.json" "$STAGE/base-config.json"
    else
        fail "config.example.json not found"
    fi

    # Rewrite log_level to ${LOG_LEVEL} placeholder (expanded by config.Load at startup).
    # shellcheck disable=SC2016
    sed -i 's|"log_level"[[:space:]]*:[[:space:]]*"[^"]*"|"log_level":            "${LOG_LEVEL}"|' "$STAGE/base-config.json"
    # shellcheck disable=SC2016
    grep -Eq '"log_level"[[:space:]]*:[[:space:]]*"\$\{LOG_LEVEL\}"' "$STAGE/base-config.json" \
        || fail "log_level injection failed: base-config has no log_level field"

    # miniagent-config: distinct router_path + backend_id + optional config_dir.
    cp "$STAGE/base-config.json" "$STAGE/miniagent-config.json"
    inject_router_path "$STAGE/miniagent-config.json" "$STATE_DIR/miniagent-router.json" "miniagent-1"
    local _config_dir; _config_dir="$(env_get MINIAGENT_CONFIG_DIR)"
    if [[ -n "$_config_dir" ]]; then
        inject_config_dir "$STAGE/miniagent-config.json" "$_config_dir"
    fi

    # miniagent CLI config (v3.1+ config-only mode). Deploy-time ${VAR} expansion
    # from .env (miniagent v3.3.0 removed config-load-time expansion).
    local chat_url models_url default_model
    chat_url="$(env_get MINIAGENT_CHAT_URL)"
    models_url="$(env_get MINIAGENT_MODELS_URL)"
    default_model="$(env_get MINIAGENT_DEFAULT_MODEL)"
    cat > "$STAGE/miniagent-cli.json" <<EOF
{
  "providers": [{"name": "default", "chat_url": "${chat_url}", "models_url": "${models_url}"}],
  "defaults": {"model": "${default_model}"},
  "run": {"shell_timeout": "60s"}
}
EOF

    # feishu: plain copy of base (frontend uses backend_id only for self-report).
    cp "$STAGE/base-config.json" "$STAGE/feishu-config.json"

    # status-monitor: needs a UNIQUE backend_id (the default "backend-1" would
    # collide / not be recognized as status-monitor by the frontend's registry).
    cp "$STAGE/base-config.json" "$STAGE/status-monitor-config.json"
    sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "status-monitor-1",|' "$STAGE/status-monitor-config.json"

    info "Generated 3 service configs from config.example.json"
}

inject_router_path() {
    local file="$1" path="$2" backend_id="${3:-}"
    sed -i -e '/"router_path"/d' \
           -e '/"backend_id"/a\  "router_path":  "'"$path"'",' "$file"
    [[ -n "$backend_id" ]] && sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "'"$backend_id"'",|' "$file"
    grep -q '"router_path"' "$file" \
        || fail "router_path injection failed: $file has no backend_id field"
}

inject_config_dir() {
    local file="$1" val="$2"
    local esc="${val//\\/\\\\\\\\}"
    esc="${esc//\"/\\\\\"}"; esc="${esc//&/\\&}"; esc="${esc//|/\\|}"
    sed -i 's|"config_dir"[[:space:]]*:[[:space:]]*"[^"]*"|"config_dir": "'"${esc}"'"|' "$file"
    grep -q '"config_dir"' "$file" \
        || fail "config_dir injection failed: $file has no config_dir field"
}

# -- Legacy cleanup ------------------------------------------------------------
cleanup_legacy() {
    local legacy_unit matched=0 installed
    installed="$(sudo systemctl list-unit-files 2>/dev/null)"
    for legacy_unit in lark-opencode-serve-back lark-opencode-back lark-omp-back lark-claude-back lark-agnes-back; do
        if grep -q "^${legacy_unit}\.service" <<<"$installed"; then
            warn "Removing legacy unit ${legacy_unit}.service..."
            sudo systemctl disable --now "$legacy_unit" 2>/dev/null || true
            sudo rm -f "/etc/systemd/system/${legacy_unit}.service"
            matched=1
        fi
    done
    [[ "$matched" -eq 0 ]] || sudo systemctl daemon-reload
    sudo rm -f \
        "$STATE_DIR/opencode-serve-router.json" "$STATE_DIR/usage-opencode-serve.json" \
        "$STATE_DIR/opencode-router.json" "$STATE_DIR/usage-opencode.json" \
        "$STATE_DIR/omp-router.json" "$STATE_DIR/usage-omp.json" \
        "$STATE_DIR/claude-router.json" "$STATE_DIR/usage-claude.json" \
        "$CONFIG_DIR/opencode-serve-config.json" "$CONFIG_DIR/opencode-config.json" \
        "$CONFIG_DIR/omp-config.json" "$CONFIG_DIR/claude-config.json" \
        "$CONFIG_DIR/agnes-back-config.json" 2>/dev/null || true
}

# -- Stop ----------------------------------------------------------------------
stop_services() {
    info "Stopping services..."
    timeout "$STOP_TIMEOUT" sudo systemctl stop "${ALL_UNITS[@]}" 2>/dev/null || true
    sleep 1
    local u pid
    for u in "${ALL_UNITS[@]}"; do
        pid="$(systemctl show -p MainPID --value "$u" 2>/dev/null || true)"
        if [[ -n "$pid" && "$pid" != "0" ]] && kill -0 "$pid" 2>/dev/null; then
            warn "$u still running (PID=$pid), SIGKILL"
            sudo systemctl kill --signal=SIGKILL "$u" 2>/dev/null || true
        fi
    done
    sleep 1
    for u in "${ALL_UNITS[@]}"; do
        if systemctl is-active --quiet "$u" 2>/dev/null; then
            fail "$u could not be stopped"
        fi
    done
    info "All services stopped"
}

# -- Install -------------------------------------------------------------------
install_files() {
    info "Creating directories..."
    sudo mkdir -p "$DEPLOY_DIR" "$CONFIG_DIR" "$STATE_DIR"
    stop_services

    info "Copying binaries (atomic rename)..."
    local u
    for u in "${ALL_UNITS[@]}"; do
        [[ -f "$BIN_DIR/$u" ]] || fail "Build artifact missing: $u"
        sudo cp "$BIN_DIR/$u" "$DEPLOY_DIR/.${u}.new"
        sudo mv -f "$DEPLOY_DIR/.${u}.new" "$DEPLOY_DIR/$u"
    done
    sudo chmod 755 "$DEPLOY_DIR"/*

    info "Copying configs..."
    local i
    for i in "${!ALL_UNITS[@]}"; do
        sudo cp "$STAGE/${ALL_CONFIGS[$i]}" "$CONFIG_DIR/"
    done
    sudo cp "$STAGE/miniagent-cli.json" "$CONFIG_DIR/"
    sudo chmod 600 "$CONFIG_DIR"/*.json

    sync_env

    info "Fixing permissions -> owner=$RUN_USER"
    sudo chown -R "$RUN_USER:$RUN_USER" "$DEPLOY_DIR" "$CONFIG_DIR" "$STATE_DIR"

    # Ensure miniagent CLI is world-executable (deployed separately, not chowned above).
    local _cli; _cli="$(command -v miniagent 2>/dev/null || true)"
    if [[ -n "$_cli" && -f "$_cli" ]]; then
        sudo chmod 0755 "$_cli"
        info "Ensured miniagent CLI exec bit: $_cli"
    fi
}

# .env sync: repo-root .env is source of truth. Sync deploy params, then overwrite /etc.
sync_env() {
    update_env_key IPC_ADDR "$IPC_ADDR" "$PROJECT_ROOT/.env"
    update_env_key STATE_DIR "$STATE_DIR" "$PROJECT_ROOT/.env"
    update_env_key PROJECT_ROOT "$PROJECT_ROOT" "$PROJECT_ROOT/.env"
    update_env_key RUN_USER "$RUN_USER" "$PROJECT_ROOT/.env"
    grep -q '^LOG_LEVEL=' "$PROJECT_ROOT/.env" 2>/dev/null || update_env_key LOG_LEVEL info "$PROJECT_ROOT/.env"
    if ! grep -q '^WORKSPACE_ROOT=' "$PROJECT_ROOT/.env" 2>/dev/null || \
       grep -q '^WORKSPACE_ROOT=$\|^WORKSPACE_ROOT=/home/user/your-project' "$PROJECT_ROOT/.env" 2>/dev/null; then
        update_env_key WORKSPACE_ROOT "$(dirname "$PROJECT_ROOT")" "$PROJECT_ROOT/.env"
    fi
    if ! grep -q '^FRONTEND_URL=' "$PROJECT_ROOT/.env" 2>/dev/null || \
       grep -q '^FRONTEND_URL=$' "$PROJECT_ROOT/.env" 2>/dev/null; then
        update_env_key FRONTEND_URL "http://$IPC_ADDR" "$PROJECT_ROOT/.env"
    fi
    sudo cp "$PROJECT_ROOT/.env" "$CONFIG_DIR/.env"
    sudo chmod 600 "$CONFIG_DIR/.env"
}

# -- Units ---------------------------------------------------------------------
write_unit() {
    local unit="$1" config="$2" requires="${3:-}" extra_env="${4:-}" privileged="${5:-false}"
    local deps="After=network.target"
    [[ -n "$requires" ]] && deps="After=$requires.service"$'\n'"Wants=$requires.service"
    local env_block=""
    [[ -n "$extra_env" ]] && env_block="$extra_env"$'\n'
    local sandbox=""
    if [[ "$privileged" != "true" ]]; then
        sandbox='NoNewPrivileges=true
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
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
EnvironmentFile=$CONFIG_DIR/.env
${env_block}ExecStart=$DEPLOY_DIR/$unit -config $CONFIG_DIR/$config
Restart=on-failure
RestartSec=5
TimeoutStopSec=20
MemoryMax=1G
LimitNOFILE=4096
User=$RUN_USER
${sandbox}
[Install]
WantedBy=multi-user.target
EOF
}

write_units() {
    info "Generating systemd units (User=$RUN_USER)..."
    # feishu-front: sandboxed (no external CLI fork), no deps.
    write_unit lark-feishu-front feishu-config.json "" "" false
    # miniagent-back: privileged (forks CLI), HOME/PATH env, depends on feishu.
    write_unit lark-miniagent-back miniagent-config.json lark-feishu-front \
        "Environment=HOME=/home/$RUN_USER"$'\n'"Environment=PATH=/usr/local/bin:/usr/bin:/bin" true
    # status-monitor: privileged (reads /proc, makes HTTP calls), depends on feishu.
    write_unit lark-status-monitor status-monitor-config.json lark-feishu-front "" true
}

# -- Start ---------------------------------------------------------------------
fail_after_stop() {
    local unit="$1" msg="$2"
    sudo systemctl stop "$unit" 2>/dev/null || true
    fail "$msg"
}

start_services() {
    info "Starting services..."
    sudo systemctl daemon-reload
    sudo systemctl enable "${ALL_UNITS[@]}" || true

    # Front-end first (backends connect to its IPC port).
    sudo systemctl start lark-feishu-front
    wait_active lark-feishu-front || fail_after_stop lark-feishu-front "feishu-front failed to start"
    wait_listen || fail_after_stop lark-feishu-front "feishu-front IPC port not listening"

    # Backends (independent of each other).
    sudo systemctl start lark-miniagent-back lark-status-monitor
}

# -- Verify --------------------------------------------------------------------
verify_services() {
    info "Verifying..."
    local all_ok=true u
    for u in "${ALL_UNITS[@]}"; do
        if wait_active "$u"; then
            echo -e "  ${GREEN}OK${NC}   $u  $(systemctl show -p MainPID --value "$u")"
        else
            echo -e "  ${RED}FAIL${NC} $u"
            all_ok=false
        fi
    done

    # IPC auth check (Bearer → 200 = chain works end-to-end).
    local ipc_secret code
    ipc_secret="$(env_get IPC_SECRET)"
    if [[ -n "$ipc_secret" ]]; then
        code="$(curl -s -o /dev/null -m "$HTTP_TIMEOUT" -w '%{http_code}' -H "Authorization: Bearer $ipc_secret" "http://$IPC_ADDR/v1/status" 2>/dev/null || echo 000)"
        case "$code" in
            200) echo -e "  ${GREEN}OK${NC}   IPC ($IPC_ADDR) 200 (ready)" ;;
            401) echo -e "  ${YELLOW}WARN${NC} IPC 401 (.env IPC_SECRET mismatch)" ;;
            *)   echo -e "  ${YELLOW}WARN${NC} IPC $code (expected 200)" ;;
        esac
    fi

    if $all_ok; then
        info "Deploy complete"
    else
        for u in "${ALL_UNITS[@]}"; do
            systemctl is-active --quiet "$u" 2>/dev/null && continue
            warn "Recent $u logs (journalctl -u $u -n 10):"
            sudo journalctl -u "$u" -n 10 --no-pager 2>/dev/null | sed 's/^/    /' || true
        done
        fail "Some services failed to start (see logs above)"
    fi
}

# -- Main ----------------------------------------------------------------------
main() {
    parse_args "$@"
    $DEBUG && set -x

    # No toolchain preflight: deploy never compiles; missing binaries fail fast
    # in ensure_binaries with a "run make build" hint.
    deploy_sudo_check
    if $FORCE; then
        warn "--force: skipping in-flight check"
    else
        info "Checking for in-flight sessions..."
        preflight_inflight_check
    fi

    ensure_binaries
    verify_artifacts
    stage_configs
    cleanup_legacy

    install_files
    write_units
    start_services
    verify_services
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    main "$@"
fi
