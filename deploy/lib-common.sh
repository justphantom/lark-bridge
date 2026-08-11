# shellcheck shell=bash
# shellcheck disable=SC2034  # vars are consumed by the sourcing entry scripts
#
# lib-common.sh — shared helpers for the lark-bridge deploy scripts
# (deploy.sh / deploy-status.sh). Source, do not
# execute:
#
#   source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-common.sh"
#
# Owns: path constants, polling timeouts, colors + info/warn/fail,
# INVOKER_USER/RUN_USER resolution (RUN_USER injected from .env), .env helpers
# (env_get / update_env_key), systemd wait helpers
# (wait_active / wait_listen), and the service short-name mapping table
# (svc_unit / svc_config / svc_depends / svc_privileged / svc_cli) with its
# SELECTED/SERVICES sync helpers. Entry scripts keep their own arg parsing,
# ERR trap, and unit-file writers (unit content differs per service).

# -- Paths ----------------------------------------------------------------------
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$LIB_DIR/.." && pwd)"
BIN_DIR="$PROJECT_ROOT/bin"

DEPLOY_DIR="/opt/lark-bridge/bin"
CONFIG_DIR="/etc/lark-bridge"
STATE_DIR="${STATE_DIR:-/var/lib/lark-bridge}"

# -- Timeouts / polling constants (centralised to avoid magic numbers) ----------
# HTTP_TIMEOUT       upper bound for curl IPC probes (local/LAN)
# WAIT_RETRIES       systemctl cold-start poll retries (1s each, ~15s cap)
# STOP_TIMEOUT       systemctl stop deadline; SIGKILL fallback beyond it (default TimeoutStopSec=90s is too long)
HTTP_TIMEOUT=3
WAIT_RETRIES=15
STOP_TIMEOUT=15

# -- Colors ---------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; exit 1; }

# -- Invoker user (the account running this script; needs passwordless sudo) ---
# Direct sudo would make whoami return root and the services would run as root.
# Restore the real caller from SUDO_USER; bail out if absent. INVOKER_USER is
# who actually executes the embedded sudo commands in this flow; it may differ
# from the service run user RUN_USER (see below).
if [[ "$EUID" -eq 0 ]]; then
    INVOKER_USER="${SUDO_USER:-}"
    [[ -n "$INVOKER_USER" ]] || fail "Do not run this script as root directly; it sudo's internally when needed. If you must, use 'sudo -E' so SUDO_USER is preserved."
else
    INVOKER_USER="$(whoami)"
fi

# -- Run user (the account the systemd services run as) -------------------------
# Injected from .env: precedence env var RUN_USER > repo-root .env RUN_USER >
# the invoking user (INVOKER_USER). Decoupled from INVOKER_USER so an admin can
# deploy on behalf of a dedicated service account; the deploy scripts chown
# deploy/config/state dirs and write systemd User= with this value. root is
# forbidden -- the services must never run as root. deploy.sh syncs the
# effective value back into repo-root .env so it stays the single source of
# truth across re-deploys.
# resolve_run_user is defined before env_get below, but the assignment is placed
# AFTER env_get's definition: top-level statements execute as they are read, so
# calling a not-yet-defined env_get here would fail at source time.
# shellcheck disable=SC2120  # $1 env file is optional (used by smoke tests)
resolve_run_user() {
    local envf="${1:-$PROJECT_ROOT/.env}"
    local u="${RUN_USER:-$(env_get RUN_USER "$envf")}"
    echo "${u:-$INVOKER_USER}"
}

# env_get reads KEY=VALUE from the repo-root .env (or $2 if given); returns
# empty if the file is missing or the key absent. First-time deploy (.env not
# generated yet) returns empty and the caller's default kicks in.
# `|| true` guards: under pipefail a grep with no match returns 1 and would
# otherwise abort the command substitution under set -e.
env_get() {
    local key="$1" file="${2:-$PROJECT_ROOT/.env}"
    grep -E "^${key}=" "$file" 2>/dev/null | head -1 | cut -d= -f2- || true
}

RUN_USER="$(resolve_run_user)"
[[ -n "$RUN_USER" ]] || fail "RUN_USER could not be resolved (set RUN_USER in .env)"
[[ "$RUN_USER" == "root" ]] && fail "RUN_USER must not be 'root' (services must not run as root)"

# update_env_key idempotently updates one key: in-place sed if present, append
# otherwise.
update_env_key() {
    local key="$1" val="$2" file="$3"
    # Inline sed-replacement escaping (& \ and the | separator), so paths like
    # PROJECT_ROOT containing metacharacters are not interpreted as
    # backreferences or truncating separators.
    local esc_val="${val//\\/\\\\}"
    esc_val="${esc_val//&/\\&}"
    esc_val="${esc_val//|/\\|}"
    if sudo grep -q "^${key}=" "$file"; then
        sudo sed -i "s|^${key}=.*|${key}=${esc_val}|" "$file"
    else
        echo "${key}=${val}" | sudo tee -a "$file" > /dev/null
    fi
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
# Reads $IPC_ADDR at call time; the entry script sets it after sourcing.
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

# -- Service short-name mapping table -------------------------------------------
# Add a new backend here in four spots and --services picks it up; no other
# deploy-flow touch-points need changing.
svc_unit()  { case "$1" in feishu) echo lark-feishu-front;; miniagent) echo lark-miniagent-back;; *) return 1;; esac; }
svc_config(){ case "$1" in feishu) echo feishu-config.json;; miniagent) echo miniagent-config.json;; esac; }
# Backends depend on the front-end listening and need privileged mode
# (passthrough of external CLIs); feishu-front has neither.
svc_depends(){ [[ "$1" == "feishu" ]] && echo "" || echo "lark-feishu-front"; }
svc_privileged(){ [[ "$1" == "feishu" ]] && echo "false" || echo "true"; }
# CLI binary name (for probe_cli); feishu has no CLI.
svc_cli(){ case "$1" in miniagent) echo "miniagent";; *) echo "";; esac; }

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
