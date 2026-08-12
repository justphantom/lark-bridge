# shellcheck shell=bash
# shellcheck disable=SC2034  # vars are consumed by the sourcing entry scripts
#
# lib-common.sh — shared helpers for deploy.sh. Source, do not execute.

# -- Paths ----------------------------------------------------------------------
LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$LIB_DIR/.." && pwd)"
BIN_DIR="$PROJECT_ROOT/bin"

DEPLOY_DIR="/opt/lark-bridge/bin"
CONFIG_DIR="/etc/lark-bridge"
STATE_DIR="${STATE_DIR:-/var/lib/lark-bridge}"

# -- Timeouts -------------------------------------------------------------------
HTTP_TIMEOUT=3    # curl IPC probes
WAIT_RETRIES=15   # systemctl cold-start poll (1s each, ~15s cap)
STOP_TIMEOUT=15   # systemctl stop deadline (default 90s is too long)

# -- Colors + logging -----------------------------------------------------------
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; NC='\033[0m'
info()  { echo -e "${GREEN}[INFO]${NC}  $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()  { echo -e "${RED}[FAIL]${NC}  $*" >&2; exit 1; }

# -- Invoker user (needs passwordless sudo) -------------------------------------
if [[ "$EUID" -eq 0 ]]; then
    INVOKER_USER="${SUDO_USER:-}"
    [[ -n "$INVOKER_USER" ]] || fail "Do not run as root directly; use 'sudo -E' so SUDO_USER is preserved."
else
    INVOKER_USER="$(whoami)"
fi

# -- Run user (the account the systemd services run as) -------------------------
# Precedence: env var RUN_USER > repo-root .env RUN_USER > INVOKER_USER.
# Decoupled so an admin can deploy on behalf of a dedicated service account.
# shellcheck disable=SC2120  # $1 env file is optional (used by smoke tests)
resolve_run_user() {
    local envf="${1:-$PROJECT_ROOT/.env}"
    local u="${RUN_USER:-$(env_get RUN_USER "$envf")}"
    echo "${u:-$INVOKER_USER}"
}

# env_get reads KEY=VALUE from .env (or $2); empty if file/key missing.
env_get() {
    local key="$1" file="${2:-$PROJECT_ROOT/.env}"
    grep -E "^${key}=" "$file" 2>/dev/null | head -1 | cut -d= -f2- || true
}

RUN_USER="$(resolve_run_user)"
[[ -n "$RUN_USER" ]] || fail "RUN_USER could not be resolved (set RUN_USER in .env)"
[[ "$RUN_USER" == "root" ]] && fail "RUN_USER must not be 'root'"

# update_env_key idempotently updates one key: in-place sed if present, append otherwise.
update_env_key() {
    local key="$1" val="$2" file="$3"
    local esc_val="${val//\\/\\\\}"
    esc_val="${esc_val//&/\\&}"
    esc_val="${esc_val//|/\\|}"
    if sudo grep -q "^${key}=" "$file"; then
        sudo sed -i "s|^${key}=.*|${key}=${esc_val}|" "$file"
    else
        echo "${key}=${val}" | sudo tee -a "$file" > /dev/null
    fi
}

# Poll up to ~15s for the service to become active.
wait_active() {
    local svc="$1" i
    for ((i=0; i<WAIT_RETRIES; i++)); do
        systemctl is-active --quiet "$svc" && return 0
        sleep 1
    done
    return 1
}

# Poll up to ~15s for the feishu-front IPC port to listen (non-000 = listening).
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
