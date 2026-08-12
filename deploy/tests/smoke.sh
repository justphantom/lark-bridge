#!/usr/bin/env bash
#
# smoke.sh — unit smoke tests for deploy/lib-common.sh and deploy.sh's
# source guard. Run from anywhere: ./deploy/tests/smoke.sh
set -euo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR_SRC="$(cd "$TEST_DIR/.." && pwd)"

pass=0 fail=0
ok()   { pass=$((pass+1)); echo "ok   $1"; }
bad()  { fail=$((fail+1)); echo "FAIL $1"; }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1: got '$2', want '$3'"; fi; }

# sudo stub: run the wrapped command directly.
sudo() { "$@"; }
export -f sudo

# shellcheck source=deploy/lib-common.sh
source "$DEPLOY_DIR_SRC/lib-common.sh"

# -- update_env_key: sed-metacharacter escaping --------------------------------
tmp="$(mktemp)"
trap 'rm -f "$tmp" "$_run_env" "$inj_tmp"' EXIT
echo "KEY=old" > "$tmp"
update_env_key KEY '/a&b|c\d' "$tmp"
check "update_env_key escapes & | \\" "$(cat "$tmp")" 'KEY=/a&b|c\d'
update_env_key NEWKEY "v" "$tmp"
check "update_env_key appends" "$(tail -1 "$tmp")" "NEWKEY=v"

# -- RUN_USER: env var > .env > invoking user; root forbidden -----------------
_run_env="$(mktemp)"
_saved_run_user="${RUN_USER:-}"
check "run_user defaults to invoker" "$(RUN_USER='' resolve_run_user "$_run_env")" "$(whoami)"
echo "RUN_USER=svc-user" > "$_run_env"
check "run_user reads .env" "$(RUN_USER='' resolve_run_user "$_run_env")" "svc-user"
check "run_user env overrides .env" "$(RUN_USER=env-user resolve_run_user "$_run_env")" "env-user"
if (RUN_USER=root bash -c 'source "$1/lib-common.sh" >/dev/null 2>&1' -- "$DEPLOY_DIR_SRC"); then
    bad "run_user root should fail"
else
    ok "run_user root fails"
fi
if [[ -n "$_saved_run_user" ]]; then RUN_USER="$_saved_run_user"; else unset RUN_USER 2>/dev/null || true; fi

# -- inject_config_dir: value lands as VALID JSON (double-layer escaping) ------
inj_tmp="$(mktemp)"
json_escape() { local v="$1"; v="${v//\\/\\\\}"; v="${v//\"/\\\"}"; printf '%s' "$v"; }
inject_run() {
    printf '{\n  "config_dir": ""\n}\n' > "$inj_tmp"
    ( cd "$DEPLOY_DIR_SRC/.." && RUN_USER="${RUN_USER:-$(whoami)}" bash -c '
        source deploy/deploy.sh >/dev/null 2>&1
        inject_config_dir "$1" "$2"
    ' -- "$inj_tmp" "$1" )
}
inject_check() {
    local name="$1" val="$2" esc
    inject_run "$val"
    esc="$(json_escape "$val")"
    if grep -qF "\"config_dir\": \"$esc\"" "$inj_tmp"; then ok "$name"
    else bad "$name: want \"config_dir\": \"$esc\"; got $(cat "$inj_tmp")"; fi
}
inject_check "inject normal path"   "/etc/lark-bridge"
inject_check "inject slashes"       "/a/b/c"
inject_check "inject spaces"        "/a b/c"
inject_check "inject & pipe bslash" '/a&b|c\d'
inject_check "inject double quote"  '/a"b'
printf '{\n  "config_dir": ""\n}\n' > "$inj_tmp"
( cd "$DEPLOY_DIR_SRC/.." && RUN_USER="${RUN_USER:-$(whoami)}" bash -c '
    source deploy/deploy.sh >/dev/null 2>&1
    if [[ -n "$1" ]]; then inject_config_dir "$2" "$1"; fi
' -- "" "$inj_tmp" )
if grep -qF '"config_dir": ""' "$inj_tmp"; then ok "inject empty skipped"; else bad "inject empty skipped"; fi

# -- source guard: sourcing deploy.sh defines functions without deploying -----
out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    type -t preflight_inflight_check
    type -t main
' 2>/dev/null)"
check "deploy.sh sourceable: preflight fn" "$(head -1 <<<"$out")" "function"
check "deploy.sh sourceable: main fn"      "$(tail -1 <<<"$out")" "function"

# -- parse_args: --binaries= equals form --------------------------------------
eq_out="$(cd "$DEPLOY_DIR_SRC/.." && bash -c '
    source deploy/deploy.sh
    parse_args --binaries=/tmp/bins
    echo "$BINARIES_SRC"
' 2>/dev/null)"
check "parse_args --binaries= equals form" "$eq_out" "/tmp/bins"

echo
echo "passed=$pass failed=$fail"
[[ "$fail" -eq 0 ]]
