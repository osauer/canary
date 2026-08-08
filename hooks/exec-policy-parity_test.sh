#!/usr/bin/env bash
# Verify canonical Canary decisions and retired ibkr anti-bypass boundaries.
set -u

hook_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$hook_dir/.." && pwd)"
rules="$repo_root/.codex/rules/canary.rules"

if ! command -v codex >/dev/null 2>&1; then
  echo "execpolicy parity: skipped (codex unavailable)"
  exit 0
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "execpolicy parity: skipped (jq unavailable)"
  exit 0
fi

fails=0

decision() {
  codex execpolicy check --rules "$rules" -- "$@" 2>/dev/null | jq -r '.decision // "unmatched"'
}

run_cli_case() {
  local name="$1" cli="$2" want="$3"
  shift 3

  local got
  got="$(decision "$cli" "$@")"
  if [[ "$got" != "$want" ]]; then
    echo "FAIL $name: $cli=$got want=$want" >&2
    fails=$((fails + 1))
    return
  fi
  echo "ok   $name ($cli $want)"
}

run_shell_case() {
  local name="$1" want="$2" template="$3"
  local canary_command="${template//CLI/canary}"
  local canary_decision
  canary_decision="$(decision /bin/bash -lc "$canary_command")"
  if [[ "$canary_decision" != "$want" ]]; then
    echo "FAIL $name: canary=$canary_decision want=$want" >&2
    fails=$((fails + 1))
    return
  fi
  echo "ok   $name ($want)"
}

# Retained read-only surfaces; free-form preview was retired in v3.
run_cli_case read-status canary allow status --json
run_cli_case order-preview canary unmatched order preview --symbol AAPL --action BUY --quantity 1 --json
run_cli_case order-status canary allow order status ORDER_ID --json
run_cli_case orders-open canary allow orders open --json
run_cli_case retired-read-status ibkr unmatched status --json
run_cli_case retired-order-preview ibkr unmatched order preview --symbol AAPL --action BUY --quantity 1 --json

# Broker writes retain the current-turn prompt boundary.
run_cli_case order-cancel canary prompt order cancel ORDER_ID --json
run_cli_case opportunity-exercise canary prompt opportunities exercise KEY REVISION --json

# Runtime settings, freeze, and limit writes remain human-only.
run_cli_case settings-freeze canary forbidden settings set trading.freeze=true
run_cli_case retired-settings-freeze ibkr forbidden settings set trading.freeze=true
run_cli_case settings-limit canary forbidden settings set trading.max_order_notional=1000

# Unknown/malformed and composed shell forms get no executable-specific gap.
# The canonical release target retains its current-turn prompt boundary.
commit_check_decision="$(decision make commit-check)"
release_decision="$(decision make release RELEASE_VERSION=v9.9.9)"
if [[ "$commit_check_decision" != "allow" || "$release_decision" != "prompt" ]]; then
	echo "FAIL release-boundary: commit-check=$commit_check_decision release=$release_decision want=allow/prompt" >&2
  fails=$((fails + 1))
else
  echo "ok   verification-release-boundary (allow/prompt)"
fi

if [[ "$fails" -gt 0 ]]; then
  echo "$fails execpolicy parity case(s) failed" >&2
  exit 1
fi
echo "execpolicy safety: all cases passed"
