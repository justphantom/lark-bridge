#!/usr/bin/env bash
#
# smoke.sh — unit smoke tests for deploy/lib-common.sh and deploy.sh's
# source guard. Run from anywhere: ./deploy/tests/smoke.sh
#
# No systemd / sudo / network needed: sudo is stubbed to a pass-through so
# update_env_key exercises its real sed/tee logic against a temp file.
set -euo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR_SRC="$(cd "$TEST_DIR/.." && pwd)"

pass=0 fail=0
ok()   { pass=$((pass+1)); echo "ok   $1"; }
bad()  { fail=$((fail+1)); echo "FAIL $1"; }
check() { # check <name> <actual> <expected>
    if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi
}

# sudo stub: run the wrapped command directly (test env owns the temp files).
sudo() { "$@"; }
export -f sudo

# shellcheck source=deploy/lib-common.sh
source "$DEPLOY_DIR_SRC/lib-common.sh"

# -- service mapping table ----------------------------------------------------
check "svc_unit feishu"      "$(svc_unit feishu)"      "lark-feishu-front"
check "svc_unit claude"      "$(svc_unit claude)"      "lark-claude-back"
check "svc_unit miniagent"   "$(svc_unit miniagent)"   "lark-miniagent-back"
check "svc_config feishu"    "$(svc_config feishu)"    "feishu-config.json"
check "svc_depends feishu"   "$(svc_depends feishu)"   ""
check "svc_depends claude"   "$(svc_depends claude)"   "lark-feishu-front"
check "svc_privileged feishu" "$(svc_privileged feishu)" "false"
check "svc_privileged claude" "$(svc_privileged claude)" "true"
check "svc_cli feishu"       "$(svc_cli feishu)"       ""
check "svc_cli claude"       "$(svc_cli claude)"       "claude"
if svc_unit bogus >/dev/null 2>&1; then bad "svc_unit bogus should fail"; else ok "svc_unit bogus fails"; fi
# opencode/omp are no longer valid services: svc_unit must reject them.
if svc_unit opencode >/dev/null 2>&1; then bad "svc_unit opencode should fail"; else ok "svc_unit opencode fails"; fi
if svc_unit omp >/dev/null 2>&1; then bad "svc_unit omp should fail"; else ok "svc_unit omp fails"; fi

# -- SELECTED/SERVICES sync ----------------------------------------------------
SELECTED=(feishu claude miniagent)
rebuild_services
check "rebuild_services"     "${SERVICES[*]}"          "lark-feishu-front lark-claude-back lark-miniagent-back"
drop_service claude
check "drop_service SELECTED" "${SELECTED[*]}"         "feishu miniagent"
check "drop_service SERVICES" "${SERVICES[*]}"         "lark-feishu-front lark-miniagent-back"

# -- update_env_key: sed-metacharacter escaping --------------------------------
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
echo "KEY=old" > "$tmp"
update_env_key KEY '/a&b|c\d' "$tmp"
check "update_env_key escapes & | \\" "$(cat "$tmp")" 'KEY=/a&b|c\d'
update_env_key NEWKEY "v" "$tmp"
check "update_env_key appends" "$(tail -1 "$tmp")" "NEWKEY=v"

# -- run_mode: env var > .env > default dev ----------------------------------
# Save any pre-existing env value to restore after tests.
_saved_run_mode="${LARK_RUN_MODE:-}"
unset LARK_RUN_MODE 2>/dev/null || true
# No .env key, no env var.
check "run_mode defaults to dev" "$(run_mode)" "dev"
# .env value wins when env var is absent.
_proj_root_bak="$PROJECT_ROOT"
PROJECT_ROOT="$(mktemp -d)"
echo "LARK_RUN_MODE=pro" > "$PROJECT_ROOT/.env"
check "run_mode reads .env" "$(run_mode)" "pro"
# Env var wins over .env.
export LARK_RUN_MODE=dev
check "run_mode env overrides .env" "$(run_mode)" "dev"
# Restore.
rm -rf "$PROJECT_ROOT"
PROJECT_ROOT="$_proj_root_bak"
if [[ -n "$_saved_run_mode" ]]; then export LARK_RUN_MODE="$_saved_run_mode"; else unset LARK_RUN_MODE 2>/dev/null || true; fi

# -- guard_pro_mode: dev passes, pro skips, invalid fails ----------------------
# Source deploy-monitor.sh (source guard prevents auto-execution) and call the
# guard directly. Stub systemctl so the pro-mode disable branch is exercised.
systemctl() {
    if [[ "$1" == "is-enabled" && "$2" == "lark-deploy-monitor" ]]; then
        return 0
    fi
    return 0
}
export -f systemctl
# shellcheck source=deploy/deploy-monitor.sh
source "$DEPLOY_DIR_SRC/deploy-monitor.sh"
# dev: guard returns 0 and does not exit.
(
    LARK_RUN_MODE=dev
    guard_pro_mode
    echo "dev-passed"
) >/dev/null 2>&1
check "guard_pro_mode dev passes" "$?" "0"
# pro: guard calls exit 0 after disabling the unit.
_pro_out="$(
    LARK_RUN_MODE=pro
    guard_pro_mode 2>&1
    echo "should-not-reach"
)"
check "guard_pro_mode pro skips" "$(grep -c "跳过" <<<"$_pro_out" || true)" "1"
check "guard_pro_mode pro no build" "$(grep -c "构建 lark-deploy-monitor" <<<"$_pro_out" || true)" "0"
check "guard_pro_mode pro exits before main" "$(grep -c "should-not-reach" <<<"$_pro_out" || true)" "0"
if (
    LARK_RUN_MODE=bogus
    guard_pro_mode 2>/dev/null
); then
    bad "guard_pro_mode bogus should fail"
else
    ok "guard_pro_mode bogus fails"
fi

# -- source guard: sourcing deploy.sh must define functions without deploying --
out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    type -t preflight_inflight_check
    type -t main
' 2>/dev/null)"
check "deploy.sh sourceable: preflight fn" "$(head -1 <<<"$out")" "function"
check "deploy.sh sourceable: main fn"      "$(tail -1 <<<"$out")" "function"

# -- select_services: --services csv parsing (drives /deploy-some's ARGS) -----
# Validates the comma-split deploy.sh applies to ARGS=--services=feishu,claude
# arriving from /deploy-some. SERVICES_ARG must be set AFTER `source` because
# deploy.sh's top level resets SERVICES_ARG="" (deploy.sh:50).
csv_out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    SERVICES_ARG="feishu,claude"
    select_services >/dev/null
    echo "${SELECTED[*]}"
' 2>/dev/null)"
check "select_services csv split" "$csv_out" "feishu claude"

if (cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    SERVICES_ARG="bogus"
    select_services
' 2>/dev/null); then
    bad "select_services bogus should fail"
else
    ok "select_services bogus fails"
fi

# -- parse_args: --services=csv equals form (the exact shape /deploy-some emits)
eq_out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    parse_args --services=feishu,claude
    select_services >/dev/null
    echo "${SELECTED[*]}"
' 2>/dev/null)"
check "parse_args --services=csv" "$eq_out" "feishu claude"

eq_out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    parse_args --binaries=/tmp/bins --services=claude
    echo "$BINARIES_SRC $SERVICES_ARG"
' 2>/dev/null)"
check "parse_args --binaries= equals form" "$eq_out" "/tmp/bins claude"

echo
echo "passed=$pass failed=$fail"
[[ "$fail" -eq 0 ]]
