#!/usr/bin/env bash
# Table-driven behavior test for canary-pre-tool-use.sh.
#
# The hook is a broker guardrail: false-allow lets an agent reach a write
# path, false-block breaks read-only workflows (the v1.14.0 cache blocked
# plain `canary orders --json` for weeks). Every row here is one payload and
# the exit code the hook must produce. Write-path rows stub the matching
# `canary trading status --json` executable on PATH; read-only rows must decide
# without invoking the binary (the stub records invocations).
set -u

hook_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
hook="${HOOK_UNDER_TEST:-$hook_dir/canary-pre-tool-use.sh}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

stub_dir="$work/bin"
mkdir -p "$stub_dir"
cat >"$stub_dir/canary" <<'STUB'
#!/usr/bin/env bash
echo "${0##*/} $*" >>"$CANARY_STUB_LOG"
if [[ "$1 $2 ${3:-}" == "trading status --json" ]]; then
  cat "$CANARY_STUB_STATUS"
  exit 0
fi
exit 1
STUB
chmod +x "$stub_dir/canary"

live_ready="$work/live-ready.json"
cat >"$live_ready" <<'JSON'
{"mode":"live","blocked":false,"live_override":"ready","can_write":true,"account":"U1234567","gateway_port":7496,"write_blockers":[]}
JSON

live_frozen="$work/live-frozen.json"
cat >"$live_frozen" <<'JSON'
{"mode":"live","blocked":false,"live_override":"ready","can_write":false,"account":"U1234567","gateway_port":7496,"write_blockers":[{"code":"trading_frozen"}]}
JSON

mode_disabled="$work/disabled.json"
cat >"$mode_disabled" <<'JSON'
{"mode":"disabled","blocked":true,"live_override":"blocked","can_write":false,"write_blockers":[{"code":"trading_disabled"}]}
JSON

fails=0
run_case() {
  local name="$1" want="$2" status_file="$3" want_stub="$4" cmd="$5"
  local stub_log="$work/stub-$name.log"
  : >"$stub_log"
  printf '{"tool_input":{"command":%s}}' "$(printf '%s' "$cmd" | jq -Rs .)" |
    PATH="$stub_dir:$PATH" CANARY_STUB_LOG="$stub_log" CANARY_STUB_STATUS="$status_file" \
      bash "$hook" >/dev/null 2>"$work/stderr-$name"
  local got=$?
  if [[ "$got" != "$want" ]]; then
    echo "FAIL $name: exit $got, want $want (cmd: $cmd)" >&2
    sed 's/^/  stderr: /' "$work/stderr-$name" >&2
    fails=$((fails + 1))
    return
  fi
  local stub_calls
  stub_calls="$(wc -l <"$stub_log" | tr -d ' ')"
  if [[ "$want_stub" == "none" && "$stub_calls" != "0" ]]; then
    echo "FAIL $name: read-only path invoked canary ($stub_calls calls)" >&2
    fails=$((fails + 1))
    return
  fi
  if [[ "$want_stub" == "status" && "$stub_calls" == "0" ]]; then
    echo "FAIL $name: write path decided without consulting trading status" >&2
    fails=$((fails + 1))
    return
  fi
  if [[ "$want_stub" == "status" ]]; then
    local expected_cli="${cmd%% *}"
    expected_cli="${expected_cli##*/}"
    if ! grep -qx "$expected_cli trading status --json" "$stub_log"; then
      echo "FAIL $name: status lookup did not use the invoked $expected_cli executable" >&2
      sed 's/^/  stub: /' "$stub_log" >&2
      fails=$((fails + 1))
      return
    fi
  fi
  echo "ok   $name"
}

run_cli_case() {
  local name="$1" want="$2" status_file="$3" want_stub="$4" template="$5"
  run_case "$name" "$want" "$status_file" "$want_stub" "${template//CLI/canary}"
}

# Read-only surfaces must pass without consulting the daemon.
run_case orders-bare 0 "$live_ready" none 'canary orders'
run_case orders-json 0 "$live_ready" none 'canary orders --json'
run_case orders-open 0 "$live_ready" none 'canary orders open --json'
run_case orders-history 0 "$live_ready" none 'canary orders history'
run_case orders-piped 0 "$live_ready" none 'canary orders --json | jq -c .orders'
run_case positions 0 "$live_ready" none 'canary positions --json'
run_case order-preview 0 "$live_ready" none 'canary order preview sell BB 20260821 C 12 100 --json'
run_case order-status 0 "$live_ready" none 'canary order status 42 --json'
run_case order-help 0 "$live_ready" none 'canary order --help'
run_case rules-future 0 "$live_ready" none 'canary rules --json'
run_case reduce-preview 0 "$live_ready" none 'canary proposals reduce BB --percent 25 --json'

# Human-only and destructive state writes stay blocked regardless of status.
run_case settings-set 2 "$live_ready" none 'canary settings set trading.freeze=true'
run_case watch-add 2 "$live_ready" none 'canary watch --add BB'
run_case daemon-wipe 2 "$live_ready" none 'canary daemon wipe'

# Broker writes consult trading status: route-ready allows, otherwise block.
run_case place-live-ready 0 "$live_ready" status 'canary order place --preview-token tok'
run_case place-disabled 2 "$mode_disabled" status 'canary order place --preview-token tok'
run_case cancel-frozen 0 "$live_frozen" status 'canary order cancel 42'
run_case place-frozen 2 "$live_frozen" status 'canary order place --preview-token tok'
run_case reduce-submit-live-ready 0 "$live_ready" status 'canary proposals reduce BB --percent 25 --submit --json'
run_case reduce-submit-disabled 2 "$mode_disabled" status 'canary proposals reduce BB --percent 25 --submit --json'
run_case reduce-submit-frozen 2 "$live_frozen" status 'canary proposals reduce BB --percent 25 --submit --json'
run_case close-frozen 2 "$live_frozen" status 'canary order close 42'

# Shell composition around a write is blocked before any status lookup.
run_case compound-write 2 "$live_ready" none 'canary orders --json; canary order place --preview-token tok'
run_case subshell-write 2 "$live_ready" none 'canary order place --preview-token $(cat tok)'
run_case compound-reduce-submit 2 "$live_ready" none 'canary proposals reduce BB --percent 25 --submit --json; echo done'

# Canonical adversarial shapes retain their authority boundaries.
run_cli_case place 0 "$live_ready" status 'CLI order place --preview-token tok'
run_cli_case modify-disabled 2 "$mode_disabled" status 'CLI order modify 42 --quantity 2'
run_cli_case cancel-frozen 0 "$live_frozen" status 'CLI order cancel 42'
run_cli_case exercise-disabled 2 "$mode_disabled" status 'CLI opportunities exercise option_exercise:a sha256:rev --json'
run_cli_case purge-execute-disabled 2 "$mode_disabled" status 'CLI purge execute --all --json'
run_cli_case restore-preview 0 "$live_ready" none 'CLI purge restore --all --json'
run_cli_case restore-execute-disabled 2 "$mode_disabled" status 'CLI purge restore --all --execute --json'
run_cli_case settings-freeze 2 "$live_ready" none 'CLI settings set trading.freeze=true'
run_cli_case settings-limit 2 "$live_ready" none 'CLI settings set trading.max_order_notional=1000'
run_cli_case preview 0 "$live_ready" none 'CLI order preview sell BB 20260821 C 12 100 --json'
run_cli_case status 0 "$live_ready" none 'CLI order status 42 --json'
run_cli_case malformed-name 0 "$live_ready" none 'CLIevil order place --preview-token tok'
run_cli_case composed-pipe 2 "$live_ready" none 'CLI order place --preview-token tok | cat'
run_cli_case composed-chain 2 "$live_ready" none 'CLI order status 42; CLI order place --preview-token tok'
run_cli_case command-substitution 2 "$live_ready" none 'CLI order place --preview-token $(cat tok)'
run_cli_case paper-smoke-direct 0 "$live_ready" status 'CLI trading paper-smoke'

# Every retired executable spelling is rejected before read/write
# classification or trading-status lookup.
run_case retired-read 2 "$live_ready" none 'ibkr status --json'
run_case retired-help 2 "$live_ready" none 'ibkr order --help'
run_case retired-write 2 "$live_ready" none 'ibkr order place --preview-token tok'
run_case retired-mixed 2 "$live_ready" none 'canary order status 42; ibkr order place --preview-token tok'

# The retired name is rejected at command position, however it is reached.
run_case retired-abs-path 2 "$live_ready" none '/opt/legacy/ibkr status'
run_case retired-rel-path 2 "$live_ready" none './tools/ibkr status'
run_case retired-env-prefix 2 "$live_ready" none 'CANARY_LOG=/tmp/x ibkr status'
run_case retired-wrapper 2 "$live_ready" none 'env ibkr status'
run_case retired-sh-c 2 "$live_ready" none 'bash -c "ibkr order place --preview-token tok"'
run_case retired-piped 2 "$live_ready" none 'echo x | ibkr status'
run_case retired-subshell 2 "$live_ready" none 'echo $(ibkr status)'

# A path or argument that merely ends in a CLI name is not an invocation. The
# repo itself lives under a directory named ibkr, so substring matching here
# blocked ordinary read-only tooling.
run_case path-arg-git 0 "$live_ready" none 'git -C /Users/osauer/dev/ibkr status --short'
run_case path-arg-nested 0 "$live_ready" none 'ls /Users/osauer/dev/ibkr/.claude/worktrees'
run_case path-arg-grep 0 "$live_ready" none 'grep -n ibkr hooks/canary-pre-tool-use.sh'
# Quote normalization strips the pattern's quotes before tokenizing, so a
# retired spelling inside a search pattern must still read as an argument.
run_case pattern-arg-quoted 0 "$live_ready" none "grep -cE 'legacy/ibkr' Makefile"
run_case path-arg-canary 0 "$live_ready" none 'git -C /srv/canary log --oneline -3'
run_case path-arg-then-write 2 "$live_ready" none 'git -C /Users/osauer/dev/ibkr status; canary settings set trading.freeze=true'

# A bare `cd` into the primary working directory must pass. Substring matching
# blocked it, `cd <path>/` slipped through on the trailing slash, and a blocked
# cd is worse than a retry: the shell silently stays in the wrong tree, so the
# next command reads another worktree's state.
run_case cd-primary-tree 0 "$live_ready" none 'cd /Users/osauer/dev/ibkr'
run_case cd-trailing-slash 0 "$live_ready" none 'cd /Users/osauer/dev/ibkr/'
run_case cd-then-read 0 "$live_ready" none 'cd /Users/osauer/dev/ibkr && canary status --json'
run_case cd-then-write 2 "$live_ready" none 'cd /Users/osauer/dev/ibkr && canary settings set trading.freeze=true'

# Only the canonical release target carries the fixed paper round-trip
# exception. The project hook leaves that make invocation to execpolicy and
# the release target's own gates; direct paper-smoke remains a broker write.
run_case release-target 0 "$live_ready" none 'make release RELEASE_VERSION=v9.9.9'

if [[ "$fails" -gt 0 ]]; then
  echo "$fails hook behavior case(s) failed" >&2
  exit 1
fi
echo "hook behavior: all cases passed"
