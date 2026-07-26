#!/usr/bin/env bash
#
# lark-bridge one-shot deployment script (systemd).
#
# Usage:
#   ./deploy/deploy.sh            # use repo-root .env; config derived from config.example.json
#   ./deploy/deploy.sh --init     # first-time deploy; auto-generate .env from example
#   ./deploy/deploy.sh --force    # force deploy, skip in-flight session check
#   ./deploy/deploy.sh --binaries <tar|dir>
#                                # skip make build; deploy from pre-built artifacts (no Go/repo needed on target host).
#                                # <tar>: tarball produced by `make pack`, top-level binaries extracted.
#                                # <dir>: already-extracted directory containing lark-* binaries.
#   ./deploy/deploy.sh --services claude,opencode
#                                # deploy only the given service subset (comma-separated; one of: feishu claude
#                                # opencode miniagent). Default is all. For multi-host deployments each host
#                                # uses its own subset: front-end host --services feishu, back-end host --services claude,...
#
# Optional environment variables:
#   IPC_ADDR   IPC listen address. Precedence: env var > repo-root .env > localhost:6060
#              (edit .env to persist; env var wins for one-shot overrides)
#   STATE_DIR  persistence directory (default /var/lib/lark-bridge; env var overrides)
#
set -euo pipefail

# -- Paths ----------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN_DIR="$PROJECT_ROOT/bin"

DEPLOY_DIR="/opt/lark-bridge/bin"
CONFIG_DIR="/etc/lark-bridge"
STATE_DIR="${STATE_DIR:-/var/lib/lark-bridge}"

# -- Timeouts / polling constants (centralised to avoid magic numbers) ----------
# HTTP_TIMEOUT       upper bound for curl IPC probes (local/LAN)
# WAIT_RETRIES       systemctl cold-start poll retries (1s each, ~15s cap)
# STOP_TIMEOUT       systemctl stop deadline; SIGKILL fallback beyond it (default TimeoutStopSec=90s is too long)
# CLI_PROBE_TIMEOUT  external CLI --version probe cap; mirrors backend readyTimeout
#                    (internal/opencode/client.go:27)
HTTP_TIMEOUT=3
WAIT_RETRIES=15
STOP_TIMEOUT=15
CLI_PROBE_TIMEOUT=30

# -- Run user (script uses embedded sudo; running as root is forbidden) ---------
# Direct sudo would make whoami return root and the services would run as root.
# Restore the real caller from SUDO_USER; bail out if absent (fail is not defined
# yet, so the inline equivalent runs).
if [[ "$EUID" -eq 0 ]]; then
    RUN_USER="${SUDO_USER:-}"
    [[ -n "$RUN_USER" ]] || { echo "[FAIL] Do not run this script as root directly; it sudo's internally when needed. If you must, use 'sudo -E' so SUDO_USER is preserved." >&2; exit 1; }
else
    RUN_USER="$(whoami)"
fi

# -- Colors ---------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; exit 1; }

# ERR trap: failures triggered by set -e (not those tolerated with || true)
# print the failing line + command to locate the root cause. -E propagates the
# trap into functions. fail() itself exits 1 (not a failing command) so it does
# not trigger ERR; its exit message therefore stands alone on its own line.
# ${RED}/${NC} are evaluated at trap-fire time (runtime), by which point they
# are defined.
set -E
trap 'echo -e "${RED}[FAIL]${NC} error at line $LINENO: $BASH_COMMAND" >&2' ERR

# env_get reads KEY=VALUE from the repo-root .env; returns empty if the file is
# missing or the key absent. First-time deploy (.env not generated yet) returns
# empty and the caller's default kicks in.
# `|| true` guards: under pipefail a grep with no match returns 1 and would
# otherwise abort the command substitution under set -e.
env_get() {
    local key="$1"
    grep -E "^${key}=" "$PROJECT_ROOT/.env" 2>/dev/null | head -1 | cut -d= -f2- || true
}

# -- IPC address ----------------------------------------------------------------
# Precedence: env var > repo-root .env > default (localhost:6060). Lets "edit
# .env without an env var" still take effect (same source as runtime config);
# env var wins for one-shot overrides.
IPC_ADDR="${IPC_ADDR:-$(env_get IPC_ADDR)}"
IPC_ADDR="${IPC_ADDR:-localhost:6060}"

# -- Service list ---------------------------------------------------------------
# SERVICES (unit-name array) is derived from --services by the arg parser;
# default is all 4 business services.

# Force-stop every service and confirm it exited before returning, to avoid
# overwriting a running binary (ETXTBSY). systemctl stop suppresses
# Restart=on-failure but blocks until TimeoutStopSec (90s) by default, so we
# bound the wait with `timeout $STOP_TIMEOUT`. After the timeout systemd keeps
# stopping asynchronously and the SIGKILL loop below mops up.
stop_services() {
    info "Stopping existing services (systemctl stop, ${STOP_TIMEOUT}s timeout)..."
    timeout "$STOP_TIMEOUT" sudo systemctl stop "${SERVICES[@]}" 2>/dev/null || true
    sleep 1

    # Survivors: SIGKILL along with everything in the cgroup. systemd's cgroup
    # kill already reaches all children of the unit, so no pgrep fallback is
    # needed -- that could even kill deploy-monitor (it may be forking this
    # very `make deploy` process tree).
    for svc in "${SERVICES[@]}"; do
        local pid
        pid="$(systemctl show -p MainPID --value "$svc" 2>/dev/null || true)"
        if [[ -n "$pid" && "$pid" != "0" ]] && kill -0 "$pid" 2>/dev/null; then
            warn "$svc still running (PID=$pid), sending SIGKILL"
            sudo systemctl kill --signal=SIGKILL "$svc" 2>/dev/null || true
        fi
    done
    sleep 1

    # Final check: abort the deploy if any unit is still active.
    for svc in "${SERVICES[@]}"; do
        if systemctl is-active --quiet "$svc" 2>/dev/null; then
            fail "$svc could not be stopped; aborting deploy to avoid overwriting a running binary"
        fi
    done
    info "All existing services stopped"
}

# Pre-deploy check: if feishu-front is running and reports in-flight sessions,
# abort to avoid interrupting user conversations mid-turn. Reads IPC_SECRET
# from repo-root .env to access GET /v1/status; passes when the service is not
# running or the endpoint is unreachable (first deploy / already-stopped).
preflight_inflight_check() {
    # Service not running -> no in-flight risk, pass.
    if ! systemctl is-active --quiet "$(svc_unit feishu)" 2>/dev/null; then
        return 0
    fi

    local secret
    secret="$(env_get IPC_SECRET)"
    if [[ -z "$secret" ]]; then
        warn "IPC_SECRET missing from $PROJECT_ROOT/.env; skipping in-flight check"
        return 0
    fi

    # Single curl uses -w $'\n%{http_code}' to append the status code to the
    # body, then tail/sed split it back out. Saves one IPC round-trip vs.
    # fetching body and code separately; on failure resp is empty -> code=000.
    local resp body code
    resp="$(curl -s -m "$HTTP_TIMEOUT" -w $'\n%{http_code}' -H "Authorization: Bearer $secret" "http://$IPC_ADDR/v1/status" 2>/dev/null || true)"
    code="$(tail -1 <<<"$resp")"
    [[ "$code" =~ ^[0-9]+$ ]] || code=000
    body="$(sed '$d' <<<"$resp")"

    if [[ "$code" == "000" ]]; then
        # Port unreachable (service active but not listening yet) -> pass; stop_services will handle it.
        return 0
    fi
    if [[ "$code" == "401" ]]; then
        fail "IPC returned 401 (repo-root .env IPC_SECRET does not match the running service); please verify and retry"
    fi
    if [[ "$code" != "200" ]]; then
        warn "IPC /v1/status returned unexpected status $code; skipping in-flight check"
        return 0
    fi

    local inflight
    inflight="$(echo "$body" | grep -oE '"inflight":[0-9]+' | head -1 | cut -d: -f2 || echo 0)"
    if [[ "${inflight:-0}" -gt 0 ]]; then
        fail "Detected ${inflight} in-flight session(s); aborting deploy to avoid disrupting conversations. Retry once they finish."
    fi
    info "No in-flight sessions; safe to deploy"
}

# Probe whether the external CLI (claude/opencode/miniagent) binary is ready:
# `command -v` hits AND `<cli> --version` exits 0. The 30s timeout mirrors the
# backend IsReady readyTimeout (internal/opencode/client.go:27) so a hang does
# not block the deploy. Hard precondition for the corresponding backend: a
# missing CLI -> backend crashes on startup, systemd Restart=on-failure retries
# every 5s. We probe up front, stop+disable and drop the service so the
# operator can install the CLI and re-deploy.
probe_cli() {
    local cli="$1"
    if ! command -v "$cli" >/dev/null 2>&1; then
        warn "$cli binary not ready: command -v not found (install it onto PATH)"
        return 1
    fi
    if ! timeout "$CLI_PROBE_TIMEOUT" "$cli" --version >/dev/null 2>&1; then
        warn "$cli binary not ready: $cli --version non-zero exit or timeout (${CLI_PROBE_TIMEOUT}s)"
        return 1
    fi
    info "$cli binary ready ($(command -v "$cli"))"
    return 0
}

# Poll up to ~15s for the service to become active; avoids fixed sleeps that
# misreport during cold start.
wait_active() {
    local svc="$1" i
    for ((i=0; i<WAIT_RETRIES; i++)); do
        systemctl is-active --quiet "$svc" && return 0
        sleep 1
    done
    return 1
}

# Poll up to ~15s for the feishu-front IPC port to listen.
# Backends connect to 6060 on startup; if feishu-front is not listening yet
# they crash-loop (RestartSec=5), and catching MainPID during that crash window
# returns 0 -> false negative. So we start the front-end first, wait for the
# port, then start backends -- eliminating the crash-loop at the root.
# Uses the .env IPC_SECRET to GET /v1/status (non-streaming; /v1/events is SSE
# and would hang curl after auth). -m $HTTP_TIMEOUT guards against any
# streaming-endpoint stall. 000=port not up yet, retry; any non-000 response
# counts as listening (401=port up but secret mismatch, possibly a stale secret
# from a deploy gap; does not block backends).
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

# Generate one systemd unit. $unit doubles as the Description suffix and the
# binary name (unit=lark-xxx-back, same path/name for the binary,
# Description=lark-bridge $unit).
#   $1=unit name  $2=config filename  $3=depends-on unit (empty for feishu-front only)
#   $4=extra Environment= lines (empty allowed; multi-line with $'\n')  $5=privileged (default false)
# Uses Wants= rather than Requires=: a front-end crash does not tear down
# backends, so in-flight Claude sessions keep running and the backendrpc.Run
# reconnect logic picks up the SSE stream once the front-end is back.
write_unit() {
    local unit="$1" config="$2" requires="${3:-}" extra_env="${4:-}" privileged="${5:-false}"
    local deps="After=network.target"
    [[ -n "$requires" ]] && deps="After=$requires.service"$'\n'"Wants=$requires.service"
    # Pad extra_env with a trailing newline so ExecStart stays on its own line
    # in the heredoc; empty leaves it as-is.
    local env_block=""
    [[ -n "$extra_env" ]] && env_block="$extra_env"$'\n'
    # privileged=true drops the sandbox block entirely: units that need sudo
    # (deploy-monitor running `make deploy` -> systemctl/cp to /etc). The
    # sandbox's NoNewPrivileges would block sudo's setuid step, so privileged
    # units must skip it. claude/opencode/miniagent backends also use
    # privileged=true: they spawn arbitrary external CLIs
    # (git/node/npm/bash and their children); a conservative sandbox
    # (NoNewPrivileges/RestrictSUIDSGID blocking setuid helpers,
    # ProtectSystem=full blocking writes to /usr) would break them, so they
    # run unsandboxed. Only feishu-front (no external CLI fork) is sandboxed.
    local sandbox=""
    if [[ "$privileged" != "true" ]]; then
        sandbox='# Sandbox hardening (conservative set; only entries known not to
# break backend fork/exec of CLIs):
#   NoNewPrivileges      no setuid escalation (backends do not need it)
#   ProtectSystem=full   /usr /boot read-only; /var/lib (state_dir) and /home stay writable
#                        (not strict: claude writes ~/.claude, opencode reads ~/.config)
#   ProtectHome not set: backends depend on user-home CLI config and caches
#   PrivateTmp           private /tmp namespace, not shared with system tmp
#   ProtectKernel*       forbid changing kernel runtime/modules/logs/cgroup
#   RestrictSUIDSGID     refuse to exec setuid/setgid binaries
#   CapabilityBoundingSet=  empty capability set (no Linux capability needed)
# SystemCallFilter intentionally omitted: backends spawn arbitrary external
# CLIs (git/node/shell...); a syscall allowlist is too easy to break for the
# benefit it gives.
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
# Bounded restart burst so a misconfig that bypasses the readiness probe does
# not bounce the unit every 5s forever (systemd gives up + stays down for an
# interval, then the operator fixes config and clears manually).
StartLimitIntervalSec=60
StartLimitBurst=5
# Graceful shutdown: feishu-front needs up to ~10s (bot+ipc) to drain; give
# systemd headroom past that so it does not SIGKILL mid-drain.
TimeoutStopSec=20
# Resource backstop so a leak class of bug (goroutine/connection/file) cannot
# OOM the host. 1G is well above the observed steady-state footprint (~50M)
# but low enough to protect a co-located host.
MemoryMax=1G
LimitNOFILE=4096
User=$RUN_USER
${sandbox}
[Install]
WantedBy=multi-user.target
EOF
}

# -- Argument parsing ----------------------------------------------------------
# All flags parse into variables here; the rest of the script reads $INIT/$FORCE
# rather than $1. --binaries / --services consume the next argument.
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
        *)             fail "Unknown argument: $arg (valid: --init --force --debug --help --binaries <path> --services <list>)" ;;
    esac
done
[[ -z "$prev" ]] || fail "${prev} requires an argument"

# --debug: enable `set -x` tracing of every command (with variable expansion)
# for diagnosing the deploy chain. Run after arg parsing so the parse itself
# does not drown in trace noise.
$DEBUG && set -x

# Service short-name -> unit/config/depends/privileged mapping. Add a new
# backend here in four spots and --services picks it up; no other deploy-flow
# touch-points need changing.
svc_unit()  { case "$1" in feishu) echo lark-feishu-front;; claude) echo lark-claude-back;; opencode) echo lark-opencode-back;; miniagent) echo lark-miniagent-back;; *) return 1;; esac; }
svc_config(){ case "$1" in feishu) echo feishu-config.json;; claude) echo claude-config.json;; opencode) echo opencode-config.json;; miniagent) echo miniagent-config.json;; esac; }
# Backends depend on the front-end listening and need privileged mode
# (passthrough of external CLIs); feishu-front has neither.
svc_depends(){ [[ "$1" == "feishu" ]] && echo "" || echo "lark-feishu-front"; }
svc_privileged(){ [[ "$1" == "feishu" ]] && echo "false" || echo "true"; }
# CLI binary name (for probe_cli); feishu has no CLI.
svc_cli(){ case "$1" in claude) echo "claude";; opencode) echo "opencode";; miniagent) echo "miniagent";; *) echo "";; esac; }

# SELECTED -> SERVICES: rebuild unit names from short names. Must be called
# after any SELECTED mutation, else stop/enable/start/verify (which use
# SERVICES) drift.
rebuild_services() {
    SERVICES=()
    local s
    for s in "${SELECTED[@]}"; do SERVICES+=("$(svc_unit "$s")"); done
}

# Drop a short-name from SELECTED (used when probe/env placeholder says "not
# ready") and re-sync SERVICES. Uses a _keep array to retain everything except
# the target, avoiding splice-index arithmetic.
drop_service() {
    local drop="$1" s
    _keep=(); for s in "${SELECTED[@]}"; do [[ "$s" != "$drop" ]] && _keep+=("$s"); done
    SELECTED=("${_keep[@]}")
    rebuild_services
}

# When a unit fails to become ready, stop it before failing: the unit is
# already enabled, so systemd would otherwise Restart=on-failure every 5s and
# leave a half-broken state. Stopping lets the operator intervene cleanly.
# $1=unit name  $2=failure message.
fail_after_stop() {
    local unit="$1" msg="$2"
    sudo systemctl stop "$unit" 2>/dev/null || true
    fail "$msg"
}

# Health check: does RUN_USER have passwordless sudo? A remote /deploy (this
# script triggered by deploy-monitor) has no tty; without NOPASSWD sudo hangs
# until deploy-monitor times out. Front-load this to step 0 so the operator
# sees the fix hint immediately, not after the next remote call hangs.
deploy_sudo_check() {
    if sudo -u "$RUN_USER" sudo -n systemctl is-active "$(svc_unit feishu)" >/dev/null 2>&1; then
        info "$RUN_USER has passwordless sudo"
    else
        warn "$RUN_USER lacks passwordless sudo; remote /deploy will hang until timeout"
        warn "  Fix: configure /etc/sudoers.d/lark-bridge, e.g.:"
        warn "    $RUN_USER ALL=(ALL) NOPASSWD: /usr/bin/systemctl"
        warn "  Least-privilege: grant NOPASSWD only on systemctl. The script's"
        warn "  cp/mkdir/chmod/chown/tee steps already run under a root-owned"
        warn "  install (deploy.sh is invoked by an operator with root sudo),"
        warn "  and the remote /deploy path only needs systemctl to restart units."
        warn "  Avoid NOPASSWD on sed/tee/rm/mv: with arbitrary args those are"
        warn "  equivalent to root (read/write/delete any file)."
    fi
}

# SELECTED: short-name list for this deploy; if --services is absent, all
# (default-all behaviour preserved). SERVICES is the matching unit-name array
# and reuses the historical variable name for stop/enable/start/verify.
SELECTED=()
if [[ -n "$SERVICES_ARG" ]]; then
    IFS=',' read -ra _parts <<< "$SERVICES_ARG"
    for s in "${_parts[@]}"; do
        svc_unit "$s" >/dev/null || fail "Unknown service: $s (valid: feishu claude opencode miniagent)"
        SELECTED+=("$s")
    done
else
    SELECTED=(feishu claude opencode miniagent)
fi
rebuild_services

# -- Pre-flight ----------------------------------------------------------------
# Only source-build mode (no --binaries) requires Makefile/go/make locally;
# --binaries mode needs neither Go toolchain nor repo source on the target host.
if [[ -z "$BINARIES_SRC" ]]; then
    [[ -f "$PROJECT_ROOT/Makefile" ]] || fail "Makefile not found; run from the repo root"
    command -v go   >/dev/null || fail "Go is not installed"
    command -v make >/dev/null || fail "make is not installed"
fi

# -- Step 0: pre-deploy session check + sudo health (before building, to avoid wasting compile time)
deploy_sudo_check
if $FORCE; then
    warn "--force: skipping in-flight session check; force-deploy may interrupt active conversations"
else
    info "Checking for in-flight sessions..."
    preflight_inflight_check
fi

# -- Step 1: prepare binaries --------------------------------------------------
# Source mode: `make build` compiles locally. --binaries mode: extract from
# tarball or copy from a directory, decoupling build from deploy (target host
# needs neither Go nor repo). Both modes drop artifacts into BIN_DIR; the
# subsequent cp to DEPLOY_DIR is identical.
ensure_binaries() {
    mkdir -p "$BIN_DIR"
    if [[ -z "$BINARIES_SRC" ]]; then
        info "Building binaries (source compile)..."
        make -C "$PROJECT_ROOT" build
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
ensure_binaries
[[ -x "$BIN_DIR/lark-feishu-front" ]]         || fail "Build artifact missing: lark-feishu-front"
[[ -x "$BIN_DIR/lark-claude-back" ]]          || fail "Build artifact missing: lark-claude-back"
[[ -x "$BIN_DIR/lark-opencode-back" ]]        || fail "Build artifact missing: lark-opencode-back"
[[ -x "$BIN_DIR/lark-miniagent-back" ]]       || fail "Build artifact missing: lark-miniagent-back"
# NOTE: the miniagent binary (github.com/justphantom/miniagent) is a separate
# project; deploy it to /usr/local/bin/miniagent via its own Makefile. Not
# managed by this script. Same for lark-deploy-monitor: shipped in this
# tarball but deployed independently by upgrade-monitor.sh; leaving the binary
# in BIN_DIR is harmless -- the cp below moves it to DEPLOY_DIR for
# upgrade-monitor to overwrite.

# -- Step 2: generate per-backend configs in a staging dir (no repo-source mutation)
# Each of the four processes gets its own config:
# claude/opencode/miniagent/feishu-config.json. All derived from the same base
# (each process reads only the fields it needs; extras are inert).
# Each backend must use a distinct router_path (except feishu-front), otherwise
# they overwrite each other's chat bindings.
# deploy-monitor's config/unit is managed by upgrade-monitor.sh and not in this flow.
#
# All sed runs operate on the staging copy; repo source configs stay untouched
# (git tree does not go dirty).
info "Preparing config files..."
STAGE="$(mktemp -d)"
# On EXIT/HUP/INT/TERM clean up STAGE and any .new temp files left in DEPLOY_DIR
# (an interrupt between cp and mv would leave .X.new owned by root; harmless on
# next cp but cleaned up for tidiness). The trap also fires once on normal
# completion, by which point mv is done and rm is a no-op.
trap 'rm -rf "$STAGE"; sudo rm -f "$DEPLOY_DIR"/.lark-*.new 2>/dev/null || true' EXIT

if $INIT; then
    if [[ ! -f "$PROJECT_ROOT/.env" ]]; then
        if [[ -f "$PROJECT_ROOT/deploy/env.example" ]]; then
            cp "$PROJECT_ROOT/deploy/env.example" "$PROJECT_ROOT/.env"
        elif [[ -f "$BIN_DIR/env.example" ]]; then
            cp "$BIN_DIR/env.example" "$PROJECT_ROOT/.env"
        else
            fail "env.example template not found (repo deploy/ or tarball)"
        fi
    fi
    # Generate IPC_SECRET (only if the placeholder is still unchanged). The
    # _init_secret prefix avoids colliding with a same-named local in a
    # function below (top-level assignment would otherwise pollute globals).
    if grep -q '^IPC_SECRET=change-me' "$PROJECT_ROOT/.env" 2>/dev/null; then
        _init_secret="$(openssl rand -hex 32)"
        sed -i "s|^IPC_SECRET=.*|IPC_SECRET=$_init_secret|" "$PROJECT_ROOT/.env"
        info "Generated IPC_SECRET"
    fi
    warn "Feishu credentials and other secrets in .env still need manual editing"
fi
[[ -f "$PROJECT_ROOT/.env" ]] || fail ".env not found (use --init to generate it, or manually cp deploy/env.example)"

# Backfill missing variables: any KEY present in env.example but absent from
# repo-root .env is appended with the example's default. Existing KEYs are
# never touched (operator-set values are respected). Covers the upgrade case
# where new variables were added that an old .env lacks (e.g. a missing
# OPENCODE_SERVER_PASSWORD would make config.Load fail at process expand time).
if [[ -f "$PROJECT_ROOT/deploy/env.example" ]]; then
    while IFS= read -r line; do
        [[ "$line" =~ ^([A-Za-z_][A-Za-z0-9_]*)= ]] || continue
        key="${BASH_REMATCH[1]}"
        grep -q "^${key}=" "$PROJECT_ROOT/.env" && continue
        printf '%s\n' "$line" >> "$PROJECT_ROOT/.env"
        info "Backfilled missing variable ${key} (from env.example default)"
    done < "$PROJECT_ROOT/deploy/env.example"
fi

# Warn if .env still contains placeholder values (a common first-deploy oversight)
check_env_placeholder() {
    local key="$1" pattern="$2" hint="$3"
    if grep -q "^${key}=${pattern}" "$PROJECT_ROOT/.env" 2>/dev/null; then
        warn "$key is still the placeholder; edit .env and re-deploy: $hint"
    fi
}
check_env_placeholder FEISHU_APP_ID 'cli_xxx' 'Feishu app App ID'
check_env_placeholder FEISHU_APP_SECRET 'xxx' 'Feishu app App Secret'
check_env_placeholder MINIAGENT_API_KEY 'sk-xxx' 'OpenAI-compatible API key'
check_env_placeholder IPC_SECRET 'change-me' 'IPC shared secret (auto-generated with --init, or use openssl rand -hex 32)'

# Per-service deploy readiness, based on placeholder values in repo-root .env
# (placeholder = not ready). feishu needs real Feishu credentials; miniagent
# needs a non-placeholder MINIAGENT_API_KEY; claude/opencode need no user
# key -> always ready.
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

# Filter the selected services by env readiness: stop+disable any unit that is
# not ready (to avoid crash-loops) and drop it from SELECTED. feishu is the
# front-end foundation (every backend Wants= it); selecting it without
# readiness is a hard fail -- other backends cannot connect without it.
# Services not selected are untouched (multi-host split: back-end host .env
# without Feishu credentials is normal).
READY=()
for s in "${SELECTED[@]}"; do
    if svc_env_ready "$s"; then READY+=("$s"); continue; fi
    u="$(svc_unit "$s")"
    if [[ "$s" == "feishu" ]]; then
        fail "FEISHU_APP_ID/SECRET are still placeholders; cannot deploy the front-end (every backend depends on it). Edit .env with real Feishu credentials and retry."
    fi
    if systemctl is-active --quiet "$u" 2>/dev/null || systemctl is-enabled --quiet "$u" 2>/dev/null; then
        warn "$s not ready (env placeholder); stopping and disabling $u"
        sudo systemctl disable --now "$u" 2>/dev/null || true
    else
        warn "$s not ready (env placeholder); skipping"
    fi
done
[[ ${#READY[@]} -gt 0 ]] || fail "None of the selected services are ready to deploy"
SELECTED=("${READY[@]}")
rebuild_services

# Base config source of truth: repo example > tarball-extracted example
# (--binaries target host may have no repo source, only tarball + deploy.sh).
# Do NOT use repo-root claude-config.json -- it is not in git (git ls-files is
# empty), its schema can drift, and it once broke business backends via
# DisallowUnknownFields (a memory_enabled field removed upstream was still
# present locally).
if [[ -f "$PROJECT_ROOT/config.example.json" ]]; then
    cp "$PROJECT_ROOT/config.example.json" "$STAGE/claude-config.json"
elif [[ -f "$BIN_DIR/config.example.json" ]]; then
    cp "$BIN_DIR/config.example.json" "$STAGE/claude-config.json"
else
    fail "Base config not found (config.example.json)"
fi

# Rewrite log_level to a ${LOG_LEVEL} placeholder: same expansion mechanism
# as ${STATE_DIR} / ${IPC_ADDR} (process-start config.Load expands them from
# EnvironmentFile -- see below). Editing LOG_LEVEL in repo-root .env and
# re-deploying applies to every service, no JSON editing needed. The STAGE
# copy is modified; the repo base stays as-is. Asserted explicitly so a
# missing field in the base does not let sed fail silently.
# Single quotes prevent shell expansion of ${LOG_LEVEL} (left for Go config.Load);
# shellcheck SC2016 is the expected warning, disabled inline to document intent.
# shellcheck disable=SC2016
sed -i 's|"log_level"[[:space:]]*:[[:space:]]*"[^"]*"|"log_level":            "${LOG_LEVEL}"|' "$STAGE/claude-config.json"
# shellcheck disable=SC2016
# Verify the log_level placeholder took effect. Use a regex so the assertion
# is robust to whitespace alignment changes in the sed substitution above.
grep -Eq '"log_level"[[:space:]]*:[[:space:]]*"\$\{LOG_LEVEL\}"' "$STAGE/claude-config.json" \
    || fail "log_level placeholder injection failed: $STAGE/claude-config.json has no log_level field (injection anchor missing)"

# state_dir / ipc_addr / frontend_url are already ${STATE_DIR} / ${IPC_ADDR}
# placeholders in the config template, expanded by each process's config.Load
# at startup from environment variables (see internal/config's expandEnvVars).
# deploy.sh only has to make sure IPC_ADDR / STATE_DIR reach the
# EnvironmentFile (see step 3 below) -- no JSON sed needed. This avoids both
# the metacharacter-escape traps of literal substitution and the silent-fail
# risk of sed that would split state.

# Each backend (claude/opencode/miniagent) gets its own router_path injected.
# They share one state_dir, so defaulting to the same router.v5.json would
# overwrite each other's chat bindings. The deploy script explicitly splits
# them into claude/opencode/miniagent-router.json (filename convention of this
# script only; differs from the config default router.v5.json; the router_path
# field is configurable).
#
# Optional 3rd parameter backend_id: when non-empty, also rewrite backend_id
# (opencode/miniagent derived from claude-config need it); empty preserves the
# base (claude/feishu).
# router_path injection uses sed `/\"backend_id\"/a\...`: anchored on the
# backend_id line, appended after it. If a user-customised config lacks
# backend_id, sed silently skips and the unit would fall back to the same
# default router.v5.json -- overwriting bindings -- so we explicitly assert
# router_path landed.
inject_router_path() {
    local file="$1" path="$2" backend_id="${3:-}"
    # Delete any old router_path and append the new one after backend_id in a
    # single sed -i (one fsync); backend_id rewrite runs conditionally
    # (empty for claude/feishu derivation, skipped).
    sed -i -e '/"router_path"/d' \
           -e '/"backend_id"/a\  "router_path":  "'"$path"'",' "$file"
    [[ -n "$backend_id" ]] && sed -i 's|"backend_id"[[:space:]]*:.*|"backend_id":   "'"$backend_id"'",|' "$file"
    grep -q '"router_path"' "$file" \
        || fail "router_path injection failed: $file has no backend_id field (injection anchor missing); backends would share a default router file and overwrite each other"
}

inject_router_path "$STAGE/claude-config.json" "$STATE_DIR/claude-router.json"

# opencode-back: distinct backend_id + distinct router_path
cp "$STAGE/claude-config.json" "$STAGE/opencode-config.json"
inject_router_path "$STAGE/opencode-config.json" "$STATE_DIR/opencode-router.json" "opencode-1"

# miniagent-back: distinct backend_id + distinct router_path (same pattern as opencode)
cp "$STAGE/claude-config.json" "$STAGE/miniagent-config.json"
inject_router_path "$STAGE/miniagent-config.json" "$STATE_DIR/miniagent-router.json" "miniagent-1"

# feishu-front: derived from claude-config (same base). Note: every backend
# shares internal/config.Config struct + DisallowUnknownFields, so "extra
# fields are inert" is NOT true -- the struct must recognise every key in the
# config or parse fails. Today it is safe only because the struct is a
# superset of the example fields; a schema drift would break all of them.
cp "$STAGE/claude-config.json" "$STAGE/feishu-config.json"

info "Generated claude-config / opencode-config / miniagent-config / feishu-config"

# Legacy cleanup: opencode-serve-back was removed from the codebase (CLI mode
# replaces it). On every deploy we now detect and remove any leftover unit +
# state files, so the machine does not carry a "ghost service" that
# crash-loops. Forced even when --services does not list it -- the upgrade
# path must converge to "no such unit".
legacy_unit="lark-opencode-serve-back"
if sudo systemctl list-unit-files 2>/dev/null | grep -q "^${legacy_unit}\.service"; then
    warn "Detected legacy unit ${legacy_unit}.service (opencode-serve-back was removed); stopping and disabling..."
    sudo systemctl disable --now "$legacy_unit" 2>/dev/null || true
    sudo rm -f "/etc/systemd/system/${legacy_unit}.service"
    sudo systemctl daemon-reload
    info "Cleaned up ${legacy_unit}.service"
fi
# Also clean up legacy state files (router persistence + usage stats)
for legacy_state in \
    "$STATE_DIR/opencode-serve-router.json" \
    "$STATE_DIR/usage-opencode-serve.json"; do
    if [[ -e "$legacy_state" ]]; then
        sudo rm -f "$legacy_state"
        info "Removed legacy state file: $legacy_state"
    fi
done
# Legacy config template (derived file under CONFIG_DIR)
if [[ -e "$CONFIG_DIR/opencode-serve-config.json" ]]; then
    sudo rm -f "$CONFIG_DIR/opencode-serve-config.json"
    info "Removed legacy config: $CONFIG_DIR/opencode-serve-config.json"
fi

# CLI binary readiness is a hard precondition for each of
# claude/opencode/miniagent: a missing CLI -> backend crashes on startup
# (IsReady runs `<cli> --version`), systemd retrying every 5s. We probe up
# front, stop+disable+drop the service so the noise stays down. Runs before
# stop_services: stop+disable ahead of this run's service restarts.
# Mutating SELECTED/SERVICES during the loop does not affect this iteration
# (bash expands the array to positional params once, up front).
for s in "${SELECTED[@]}"; do
    cli="$(svc_cli "$s")"
    [[ -z "$cli" ]] && continue
    if probe_cli "$cli"; then
        info "$s-back included in deploy ($cli ready)"
        continue
    fi
    u="$(svc_unit "$s")"
    warn "$s-back not deploy-ready ($cli CLI not ready); stopping and disabling $u (skipped this run)"
    case "$s" in
        claude)    warn "  Install the Claude Code CLI and re-deploy to include it: https://github.com/anthropics/claude-code" ;;
        opencode)  warn "  Install the opencode CLI and re-deploy to include it: https://github.com/sst/opencode" ;;
        miniagent) warn "  Install the miniagent CLI and re-deploy to include it: https://github.com/justphantom/miniagent" ;;
    esac
    sudo systemctl disable --now "$u" 2>/dev/null || true
    drop_service "$s"
done

# -- Step 3: create directories + copy files + fix permissions -----------------
# STATE_DIR/{claude,opencode} are the two backends' default_directory;
# per-chat working dirs are auto-created under them at runtime via MkdirAll.
info "Creating system directories..."
sudo mkdir -p "$DEPLOY_DIR" "$CONFIG_DIR" "$STATE_DIR/claude" "$STATE_DIR/opencode"

# Services must be stopped before binary overwrite, otherwise ETXTBSY.
stop_services

info "Copying binaries and configs..."

# Binaries are updated via "write temp file + atomic rename" rather than a
# direct cp overwrite. rename(2) replacing the path does not trigger ETXTBSY
# -- a running process keeps the old inode until it is restarted. The temp
# file lives in the same $DEPLOY_DIR so mv is a same-filesystem rename
# (atomic, no cross-device copy). Business services are already stopped in
# stop_services, so the rename is also safe.
# Note: deploy-monitor's binary is not in this flow -- upgrade-monitor.sh
# manages it independently.
for s in "${SELECTED[@]}"; do
    u="$(svc_unit "$s")"
    [[ -f "$BIN_DIR/$u" ]] || fail "Build artifact missing: $u (--binaries input or make build output incomplete)"
    sudo cp "$BIN_DIR/$u" "$DEPLOY_DIR/.${u}.new"
    sudo mv -f "$DEPLOY_DIR/.${u}.new" "$DEPLOY_DIR/$u"
done
sudo chmod 755 "$DEPLOY_DIR"/*

# Configs are deploy artifacts; each run copies them from STAGE to CONFIG_DIR.
for s in "${SELECTED[@]}"; do
    sudo cp "$STAGE/$(svc_config "$s")" "$CONFIG_DIR/"
done
sudo chmod 600 "$CONFIG_DIR"/*.json

# The repo-root .env is the single source of truth: each deploy first syncs
# this run's parameters (IPC_ADDR / STATE_DIR) into repo-root .env, then
# wholesale-overwrites CONFIG_DIR/.env from it. Any operator edit to .env
# (credentials, models, workspace, ...) takes effect on re-deploy; the old
# CONFIG_DIR/.env is not preserved.
# update_env_key idempotently updates one key: in-place sed if present, append
# otherwise.
update_env_key() {
    local key="$1" val="$2" file="$3"
    # Inline sed-replacement escaping (& \ and the | separator), so paths like
    # PROJECT_ROOT containing metacharacters are not interpreted as
    # backreferences or truncating separators. Used only here; no helper
    # extracted.
    local esc_val="${val//\\/\\\\}"
    esc_val="${esc_val//&/\\&}"
    esc_val="${esc_val//|/\\|}"
    if sudo grep -q "^${key}=" "$file"; then
        sudo sed -i "s|^${key}=.*|${key}=${esc_val}|" "$file"
    else
        echo "${key}=${val}" | sudo tee -a "$file" > /dev/null
    fi
}
# Sync deploy params into repo-root .env first, otherwise the wholesale
# overwrite would drop them and config's ${IPC_ADDR} / ${STATE_DIR} expansion
# would fail.
update_env_key IPC_ADDR "$IPC_ADDR" "$PROJECT_ROOT/.env"
update_env_key STATE_DIR "$STATE_DIR" "$PROJECT_ROOT/.env"
update_env_key PROJECT_ROOT "$PROJECT_ROOT" "$PROJECT_ROOT/.env"
# LOG_LEVEL: default to info if absent (config's ${LOG_LEVEL} errors on
# unset/empty); existing value is preserved so operators toggling to debug
# survive a re-deploy.
if ! grep -q '^LOG_LEVEL=' "$PROJECT_ROOT/.env" 2>/dev/null; then
    update_env_key LOG_LEVEL info "$PROJECT_ROOT/.env"
    warn ".env missing LOG_LEVEL; appended LOG_LEVEL=info (change to debug and re-deploy to take effect)"
fi
# WORKSPACE_ROOT: if .env does not set it or still has the placeholder, default
# to PROJECT_ROOT's parent (typically the common root of all projects). The
# operator can override explicitly in .env.
if ! grep -q '^WORKSPACE_ROOT=' "$PROJECT_ROOT/.env" 2>/dev/null || \
   grep -q '^WORKSPACE_ROOT=$\|^WORKSPACE_ROOT=/home/user/your-project' "$PROJECT_ROOT/.env" 2>/dev/null; then
    WORKSPACE_ROOT_DEFAULT="$(dirname "$PROJECT_ROOT")"
    update_env_key WORKSPACE_ROOT "$WORKSPACE_ROOT_DEFAULT" "$PROJECT_ROOT/.env"
    info "WORKSPACE_ROOT auto-set to $WORKSPACE_ROOT_DEFAULT (parent of PROJECT_ROOT)"
fi
# FRONTEND_URL: defaults to http://$IPC_ADDR on a single host. Only derived
# when .env does not set it or it is empty; multi-host deploys have the
# operator set the front-end reachable address explicitly (not overridden).
if ! grep -q '^FRONTEND_URL=' "$PROJECT_ROOT/.env" 2>/dev/null || \
   grep -q '^FRONTEND_URL=$' "$PROJECT_ROOT/.env" 2>/dev/null; then
    update_env_key FRONTEND_URL "http://$IPC_ADDR" "$PROJECT_ROOT/.env"
fi
sudo cp "$PROJECT_ROOT/.env" "$CONFIG_DIR/.env"
sudo chmod 600 "$CONFIG_DIR/.env"
info "Overwrote $CONFIG_DIR/.env (repo-root .env is the source of truth)"

info "Fixing directory and file permissions -> owner=$RUN_USER"
sudo chown -R "$RUN_USER:$RUN_USER" "$DEPLOY_DIR" "$CONFIG_DIR" "$STATE_DIR"

# -- Step 4: generate systemd units (dynamic user) -----------------------------
info "Generating systemd unit files (User=$RUN_USER)..."

for s in "${SELECTED[@]}"; do
    u="$(svc_unit "$s")"
    write_unit "$u" "$(svc_config "$s")" "$(svc_depends "$s")" "" "$(svc_privileged "$s")"
done

# -- Step 5: start (serial: front-end listens first, then backends) ------------
info "Starting services..."
sudo systemctl daemon-reload
# enable every service for autostart, but NOT --now; we control order below.
# Stderr is not silenced: write_unit has produced the file, so a real enable
# failure means systemd itself is unwell (path conflict etc.) -- keep stderr
# so set -e surfaces the cause immediately. Still `|| true`: some systemctl
# versions return non-zero for already-enabled units, not a deploy failure.
sudo systemctl enable "${SERVICES[@]}" || true

# Start the front-end first (if part of this deploy) and wait for the IPC port
# to listen, so backends do not crash-loop trying to connect.
# When only deploying backends, the front-end is already running; skip the
# wait and start backends directly.
if [[ " ${SELECTED[*]} " == *" feishu "* ]]; then
    front_unit="$(svc_unit feishu)"
    sudo systemctl start "$front_unit"
    wait_active "$front_unit" || fail_after_stop "$front_unit" "$front_unit failed to start"
    wait_listen || fail_after_stop "$front_unit" "feishu-front IPC port $IPC_ADDR not listening; backends cannot connect"
fi

# Port is up; start the selected backends (excluding feishu, independent of each other)
backends=()
for s in "${SELECTED[@]}"; do
    [[ "$s" == "feishu" ]] && continue
    backends+=("$(svc_unit "$s")")
done
[[ ${#backends[@]} -eq 0 ]] || sudo systemctl start "${backends[@]}"

# -- Step 6: verify (poll is-active; replaces a fixed sleep) -------------------
info "Verifying..."
all_ok=true
for svc in "${SERVICES[@]}"; do
    if wait_active "$svc"; then
        echo -e "  ${GREEN}OK${NC}   $svc  $(systemctl show -p MainPID --value "$svc")"
    else
        echo -e "  ${RED}FAIL${NC} $svc"
        all_ok=false
    fi
done

# IPC ready + auth check: GET /v1/status with .env's IPC_SECRET, expecting 200.
# The legacy unauthenticated probe expecting 401 only proved "auth is enforced";
# it did not prove "the key matches .env -> the service is actually usable".
# A correct Bearer getting 200 means the auth chain end-to-end works.
ipc_secret="$(env_get IPC_SECRET)"
if [[ -z "$ipc_secret" ]]; then
    echo -e "  ${YELLOW}WARN${NC} repo-root .env has no IPC_SECRET; skipping IPC auth check"
else
    code="$(curl -s -o /dev/null -m "$HTTP_TIMEOUT" -w '%{http_code}' -H "Authorization: Bearer $ipc_secret" "http://$IPC_ADDR/v1/status" 2>/dev/null || echo 000)"
    if [[ "$code" == "200" ]]; then
        echo -e "  ${GREEN}OK${NC}   IPC ($IPC_ADDR) returned 200 with auth (ready)"
    elif [[ "$code" == "401" ]]; then
        echo -e "  ${YELLOW}WARN${NC} IPC ($IPC_ADDR) returned 401 (.env IPC_SECRET does not match the running service)"
    else
        echo -e "  ${YELLOW}WARN${NC} IPC ($IPC_ADDR) returned $code (expected 200)"
    fi
fi

if $all_ok; then
    info "Deploy complete"
else
    # Dump the last 10 log lines of failed units so the operator does not have
    # to invoke journalctl by hand; sudo because the journal is root-owned.
    for svc in "${SERVICES[@]}"; do
        systemctl is-active --quiet "$svc" 2>/dev/null && continue
        warn "Recent $svc logs (journalctl -u $svc -n 10):"
        sudo journalctl -u "$svc" -n 10 --no-pager 2>/dev/null | sed 's/^/    /' || true
    done
    fail "Some services failed to start (see journalctl summary above)"
fi
