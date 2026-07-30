# shellcheck shell=bash
# shellcheck disable=SC2034  # vars are consumed by the sourcing entry scripts
#
# lib-common.sh — shared helpers for the lark-bridge deploy scripts
# (deploy.sh / upgrade-monitor.sh / upgrade-status.sh). Source, do not
# execute:
#
#   source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/lib-common.sh"
#
# Owns: path constants, polling timeouts, colors + info/warn/fail, RUN_USER
# resolution, .env helpers (env_get / update_env_key), systemd wait helpers
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

# -- Run user (scripts use embedded sudo; running as root is forbidden) ---------
# Direct sudo would make whoami return root and the services would run as root.
# Restore the real caller from SUDO_USER; bail out if absent.
if [[ "$EUID" -eq 0 ]]; then
    RUN_USER="${SUDO_USER:-}"
    [[ -n "$RUN_USER" ]] || fail "Do not run this script as root directly; it sudo's internally when needed. If you must, use 'sudo -E' so SUDO_USER is preserved."
else
    RUN_USER="$(whoami)"
fi

# env_get reads KEY=VALUE from the repo-root .env; returns empty if the file is
# missing or the key absent. First-time deploy (.env not generated yet) returns
# empty and the caller's default kicks in.
# `|| true` guards: under pipefail a grep with no match returns 1 and would
# otherwise abort the command substitution under set -e.
env_get() {
    local key="$1"
    grep -E "^${key}=" "$PROJECT_ROOT/.env" 2>/dev/null | head -1 | cut -d= -f2- || true
}

# run_mode returns the effective LARK_RUN_MODE value: env var > repo-root .env >
# default "dev". Used by upgrade-monitor.sh to decide whether deploy-monitor
# should be installed/started. Invalid values are rejected by the caller.
run_mode() {
    local mode="${LARK_RUN_MODE:-$(env_get LARK_RUN_MODE)}"
    echo "${mode:-dev}"
}

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
svc_unit()  { case "$1" in feishu) echo lark-feishu-front;; claude) echo lark-claude-back;; opencode) echo lark-opencode-back;; omp) echo lark-omp-back;; miniagent) echo lark-miniagent-back;; *) return 1;; esac; }
svc_config(){ case "$1" in feishu) echo feishu-config.json;; claude) echo claude-config.json;; opencode) echo opencode-config.json;; omp) echo omp-config.json;; miniagent) echo miniagent-config.json;; esac; }
# Backends depend on the front-end listening and need privileged mode
# (passthrough of external CLIs); feishu-front has neither.
svc_depends(){ [[ "$1" == "feishu" ]] && echo "" || echo "lark-feishu-front"; }
svc_privileged(){ [[ "$1" == "feishu" ]] && echo "false" || echo "true"; }
# CLI binary name (for probe_cli); feishu has no CLI.
svc_cli(){ case "$1" in claude) echo "claude";; opencode) echo "opencode";; omp) echo "omp";; miniagent) echo "miniagent";; *) echo "";; esac; }

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
