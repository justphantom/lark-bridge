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
check "svc_unit opencode"    "$(svc_unit opencode)"    "lark-opencode-back"
check "svc_unit miniagent"   "$(svc_unit miniagent)"   "lark-miniagent-back"
check "svc_config feishu"    "$(svc_config feishu)"    "feishu-config.json"
check "svc_depends feishu"   "$(svc_depends feishu)"   ""
check "svc_depends claude"   "$(svc_depends claude)"   "lark-feishu-front"
check "svc_privileged feishu" "$(svc_privileged feishu)" "false"
check "svc_privileged claude" "$(svc_privileged claude)" "true"
check "svc_cli feishu"       "$(svc_cli feishu)"       ""
check "svc_cli opencode"     "$(svc_cli opencode)"     "opencode"
if svc_unit bogus >/dev/null 2>&1; then bad "svc_unit bogus should fail"; else ok "svc_unit bogus fails"; fi

# -- SELECTED/SERVICES sync ----------------------------------------------------
SELECTED=(feishu claude opencode miniagent)
rebuild_services
check "rebuild_services"     "${SERVICES[*]}"          "lark-feishu-front lark-claude-back lark-opencode-back lark-miniagent-back"
drop_service opencode
check "drop_service SELECTED" "${SELECTED[*]}"         "feishu claude miniagent"
check "drop_service SERVICES" "${SERVICES[*]}"         "lark-feishu-front lark-claude-back lark-miniagent-back"

# -- update_env_key: sed-metacharacter escaping --------------------------------
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
echo "KEY=old" > "$tmp"
update_env_key KEY '/a&b|c\d' "$tmp"
check "update_env_key escapes & | \\" "$(cat "$tmp")" 'KEY=/a&b|c\d'
update_env_key NEWKEY "v" "$tmp"
check "update_env_key appends" "$(tail -1 "$tmp")" "NEWKEY=v"

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
