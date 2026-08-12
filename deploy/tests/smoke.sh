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
check "svc_unit miniagent"   "$(svc_unit miniagent)"   "lark-miniagent-back"
check "svc_config feishu"    "$(svc_config feishu)"    "feishu-config.json"
check "svc_depends feishu"   "$(svc_depends feishu)"   ""
check "svc_depends miniagent" "$(svc_depends miniagent)" "lark-feishu-front"
check "svc_privileged feishu" "$(svc_privileged feishu)" "false"
check "svc_privileged miniagent" "$(svc_privileged miniagent)" "true"
check "svc_cli feishu"       "$(svc_cli feishu)"       ""
check "svc_cli miniagent"    "$(svc_cli miniagent)"    "miniagent"
if svc_unit bogus >/dev/null 2>&1; then bad "svc_unit bogus should fail"; else ok "svc_unit bogus fails"; fi
# opencode/omp/claude are no longer valid services: svc_unit must reject them.
if svc_unit opencode >/dev/null 2>&1; then bad "svc_unit opencode should fail"; else ok "svc_unit opencode fails"; fi
if svc_unit omp >/dev/null 2>&1; then bad "svc_unit omp should fail"; else ok "svc_unit omp fails"; fi
if svc_unit claude >/dev/null 2>&1; then bad "svc_unit claude should fail"; else ok "svc_unit claude fails"; fi

# -- SELECTED/drop_service ----------------------------------------------------
SELECTED=(feishu miniagent)
check "SELECTED initial"      "${SELECTED[*]}"          "feishu miniagent"
drop_service miniagent
check "drop_service SELECTED" "${SELECTED[*]}"          "feishu"

# -- update_env_key: sed-metacharacter escaping --------------------------------
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
echo "KEY=old" > "$tmp"
update_env_key KEY '/a&b|c\d' "$tmp"
check "update_env_key escapes & | \\" "$(cat "$tmp")" 'KEY=/a&b|c\d'
update_env_key NEWKEY "v" "$tmp"
check "update_env_key appends" "$(tail -1 "$tmp")" "NEWKEY=v"

# -- RUN_USER: env var > .env > invoking user; root forbidden -----------------
# resolve_run_user takes an optional env file so the .env precedence is tested
# against a temp file (the repo-root .env is not touched).
_run_env="$(mktemp)"
trap 'rm -f "$tmp" "$_run_env"' EXIT
_saved_run_user="${RUN_USER:-}"
check "run_user defaults to invoker" "$(RUN_USER='' resolve_run_user "$_run_env")" "$(whoami)"
echo "RUN_USER=svc-user" > "$_run_env"
check "run_user reads .env" "$(RUN_USER='' resolve_run_user "$_run_env")" "svc-user"
check "run_user env overrides .env" "$(RUN_USER=env-user resolve_run_user "$_run_env")" "env-user"
# Root guard: sourcing lib-common.sh with RUN_USER=root must fail.
if (RUN_USER=root bash -c 'source "$1/lib-common.sh" >/dev/null 2>&1' -- "$DEPLOY_DIR_SRC"); then
    bad "run_user root should fail"
else
    ok "run_user root fails"
fi
if [[ -n "$_saved_run_user" ]]; then RUN_USER="$_saved_run_user"; else unset RUN_USER 2>/dev/null || true; fi

# -- inject_config_dir: value lands as VALID JSON (double-layer escaping) ------
# inject_config_dir lives in deploy.sh; source it like the select_services test.
# The value traverses sed-replacement THEN lands as JSON, so \ " & | each need
# escaping at both layers -- the " and \ cases are the ones that silently produce
# invalid JSON if the escape chain is wrong.
inj_tmp="$(mktemp)"
trap 'rm -f "$tmp" "$_run_env" "$inj_tmp"' EXIT
# json_escape: val -> the exact bytes that must appear inside the JSON string
# (\ -> \\, " -> \"). Used to build the grep -F expectation.
json_escape() { local v="$1"; v="${v//\\/\\\\}"; v="${v//\"/\\\"}"; printf '%s' "$v"; }
inject_run() {   # <val>: write a template config, source deploy.sh, call inject_config_dir
    printf '{\n  "config_dir": ""\n}\n' > "$inj_tmp"
    ( cd "$DEPLOY_DIR_SRC/.." && RUN_USER="${RUN_USER:-$(whoami)}" bash -c '
        source deploy/deploy.sh >/dev/null 2>&1
        inject_config_dir "$1" "$2"
    ' -- "$inj_tmp" "$1" )
}
inject_check() { # <name> <val>: assert the file now holds config_dir == val as legal JSON
    local name="$1" val="$2" esc
    inject_run "$val"
    esc="$(json_escape "$val")"
    if grep -qF "\"config_dir\": \"$esc\"" "$inj_tmp"; then
        ok "$name"
    else
        bad "$name: want \"config_dir\": \"$esc\"; got $(cat "$inj_tmp")"
    fi
}
inject_check "inject normal path"   "/etc/lark-bridge"
inject_check "inject slashes"       "/a/b/c"
inject_check "inject spaces"        "/a b/c"
inject_check "inject & pipe bslash" '/a&b|c\d'
inject_check "inject double quote"  '/a"b'
# Empty value: caller's [[ -n ]] guard skips inject_config_dir -> field stays "".
printf '{\n  "config_dir": ""\n}\n' > "$inj_tmp"
_empty_val=""
( cd "$DEPLOY_DIR_SRC/.." && RUN_USER="${RUN_USER:-$(whoami)}" bash -c '
    source deploy/deploy.sh >/dev/null 2>&1
    if [[ -n "$1" ]]; then inject_config_dir "$2" "$1"; fi
' -- "$_empty_val" "$inj_tmp" )
if grep -qF '"config_dir": ""' "$inj_tmp"; then ok "inject empty skipped (caller guard)"; else bad "inject empty skipped"; fi

# -- source guard: sourcing deploy.sh must define functions without deploying --
out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    type -t preflight_inflight_check
    type -t main
' 2>/dev/null)"
check "deploy.sh sourceable: preflight fn" "$(head -1 <<<"$out")" "function"
check "deploy.sh sourceable: main fn"      "$(tail -1 <<<"$out")" "function"

# -- select_services: --services csv parsing ----------------------------------
# Validates the comma-split deploy.sh applies to ARGS=--services=feishu,miniagent.
# SERVICES_ARG must be set AFTER `source` because deploy.sh's top level resets
# SERVICES_ARG="" (deploy.sh:50).
csv_out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    SERVICES_ARG="feishu,miniagent"
    select_services >/dev/null
    echo "${SELECTED[*]}"
' 2>/dev/null)"
check "select_services csv split" "$csv_out" "feishu miniagent"

if (cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    SERVICES_ARG="bogus"
    select_services
' 2>/dev/null); then
    bad "select_services bogus should fail"
else
    ok "select_services bogus fails"
fi

# -- parse_args: --services=csv equals form
eq_out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    parse_args --services=feishu,miniagent
    select_services >/dev/null
    echo "${SELECTED[*]}"
' 2>/dev/null)"
check "parse_args --services=csv" "$eq_out" "feishu miniagent"

eq_out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    parse_args --binaries=/tmp/bins --services=miniagent
    echo "$BINARIES_SRC $SERVICES_ARG"
' 2>/dev/null)"
check "parse_args --binaries= equals form" "$eq_out" "/tmp/bins miniagent"

echo
echo "passed=$pass failed=$fail"
[[ "$fail" -eq 0 ]]
