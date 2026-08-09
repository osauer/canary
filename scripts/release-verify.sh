#!/usr/bin/env bash
#
# release-verify.sh — run the freshly-built Canary binary against a live
# Wired into `make release` between the rebuild and the cross-compile,
# so a binary that cannot talk to the gateway never reaches GitHub
# Releases. Designed to be binding (non-zero exit aborts the release)
#   - Runs against an isolated daemon under /tmp; never touches the
#   - The matrix uses only commands that work regardless of market
#     hours: version (offline), status, account, positions, and a
#     request can take ≥25 s on a cold cache — that's an off-hours
#     cannot hang the release.
#   scripts/release-verify.sh <bin-path> <expected-version>
#   scripts/release-verify.sh bin/canary v0.15.1

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib-daemon-control.sh"

BIN="${1:?usage: release-verify.sh <bin/canary> <expected-version> (e.g. v0.15.1)}"
EXPECTED="${2:?expected version required, e.g. v0.15.1}"

if [[ ! -x "$BIN" ]]; then
    echo "release-verify: $BIN not executable" >&2
    exit 2
fi

# Tight per-command budgets. SPY snapshots typically return in <1s when
# market-data farm without letting a wedged daemon hang the release.
PER_CMD_TIMEOUT="${CANARY_RELEASE_VERIFY_TIMEOUT:-15}"

# Isolated environment under /tmp (macOS Unix-socket path limit is 104
TMPDIR_BASE="${TMPDIR:-/tmp}"
SMOKE_DIR="$(mktemp -d "$TMPDIR_BASE/ibkr-release-verify-XXXXXX")"
SOCKET="$SMOKE_DIR/ibkr.sock"
LOG="$SMOKE_DIR/ibkr-daemon.log"
LOCK="$SMOKE_DIR/ibkr.lock"

export CANARY_SOCKET="$SOCKET"
export CANARY_LOG="$LOG"
# Isolated trading state: marks, journals, and tokens must not touch the
# in operator state failed the v1.15.0 release smoke, 2026-07-08).
export XDG_STATE_HOME="$SMOKE_DIR/state"
export XDG_CACHE_HOME="$SMOKE_DIR/cache"

cleanup() {
    local code=$?
    kill_daemon_from_lockfile "$LOCK"
    # On non-zero exit, surface the daemon log tail so the failure mode
    if [[ $code -ne 0 && -r "$LOG" ]]; then
        echo ""
        echo "release-verify: daemon log tail ($LOG):" >&2
        tail -40 "$LOG" >&2 || true
    fi
    rm -rf "$SMOKE_DIR" 2>/dev/null || true
    return $code
}
trap cleanup EXIT INT TERM

echo "release-verify: smoke matrix against $BIN expecting $EXPECTED"
echo "release-verify: isolated daemon → $SOCKET"

# See scripts/lib-daemon-control.sh for the rationale (client-ID slot
# race against the user's canonical daemon — aborted the v0.16.0 release
stop_existing_daemons release-verify

# Helper: run a CLI command with a deadline; on failure, print the
run_cli() {
    local label="$1"
    shift
    if ! out="$(timeout "$PER_CMD_TIMEOUT" "$BIN" "$@" 2>&1)"; then
        echo "release-verify: FAIL [$label]: '$BIN $*' exited non-zero (or timed out at ${PER_CMD_TIMEOUT}s)" >&2
        echo "$out" >&2
        exit 1
    fi
    printf '%s' "$out"
}

# Helper: print the value at a dotted JSON path, or empty string when any
# become Python's True/False (the caller compares against those exact
# tokens).
python_extract='
import json, sys
d = json.load(sys.stdin)
for k in sys.argv[1].split("."):
    if not isinstance(d, dict):
        print(""); sys.exit(0)
    v = d.get(k)
    if v is None:
        print(""); sys.exit(0)
    d = v
print(d)
'

# Helper: exit 0 if the JSON (stdin) has the given top-level key,
python_haskey='
import json, sys
d = json.load(sys.stdin)
sys.exit(0 if sys.argv[1] in d else 1)
'

json_field() {
    local path="$1"
    local input="$2"
    printf '%s' "$input" | python3 -c "$python_extract" "$path"
}

# 1 — version stamp on the binary matches what we're shipping. Offline.
echo "  [1/7] version stamp..."
version_json="$(run_cli version version --json)"
actual_version="$(json_field version "$version_json")"
if [[ "$actual_version" != "$EXPECTED" ]]; then
    echo "release-verify: FAIL: binary stamps version=$actual_version, expected=$EXPECTED" >&2
    echo "(the ldflags in 'make build' must agree with the release tag)" >&2
    exit 1
fi

# 2 — status: daemon spawned, gateway connected, daemon_version matches.
echo "  [2/7] status (autospawn daemon at isolated socket)..."
status_json=""
connected=""
daemon_version=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
    status_json="$(run_cli status status --json)"
    connected="$(json_field connected "$status_json")"
    daemon_version="$(json_field daemon_version "$status_json")"
    if [[ "$connected" == "True" ]]; then
        break
    fi
    sleep 0.5
done
if [[ "$connected" != "True" ]]; then
    echo "release-verify: FAIL: gateway not reachable after 5s of polling (status.connected=$connected)" >&2
    echo "$status_json" >&2
    exit 1
fi
if [[ "$daemon_version" != "$EXPECTED" ]]; then
    echo "release-verify: FAIL: daemon stamped version=$daemon_version, expected=$EXPECTED" >&2
    echo "(autospawn picked up an unexpected binary — check \$PATH and bin/canary)" >&2
    exit 1
fi
# v0.27.4 wire contract: background_tasks is always emitted (possibly
# without nil-checking. Each entry, when present, must have a string
# whether the autospawned daemon is past postConnectSetup yet — only
bg_check="$(printf '%s' "$status_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
tasks = d.get("background_tasks", None)
if tasks is None:
    print("background_tasks missing — must be emitted (even when empty)")
    sys.exit(0)
if not isinstance(tasks, list):
    print("background_tasks=" + repr(tasks) + ", want list")
    sys.exit(0)
for t in tasks:
    if not isinstance(t, dict) or not isinstance(t.get("name"), str):
        print("background_tasks entry malformed: " + repr(t))
        sys.exit(0)
print("ok (" + str(len(tasks)) + " task(s))")
')"
if [[ "$bg_check" != ok* ]]; then
    echo "release-verify: FAIL: status.background_tasks: $bg_check" >&2
    echo "$status_json" >&2
    exit 1
fi

# 3 — account: pins the account-summary handler and the v0.15+ omitempty
# behaviour on data_type. Account financials are always available
echo "  [3/7] account.summary..."
account_json="$(run_cli account account --json)"
account_id="$(json_field account_id "$account_json")"
if [[ -z "$account_id" ]]; then
    echo "release-verify: FAIL: account_id missing from account.summary response" >&2
    echo "$account_json" >&2
    exit 1
fi
# data_type must be absent (v0.15 contract: account has no live/delayed
if printf '%s' "$account_json" | python3 -c "$python_haskey" data_type; then
    echo "release-verify: FAIL: account.summary leaked data_type field (v0.15 must omit it)" >&2
    echo "$account_json" >&2
    exit 1
fi

# 4 — positions: shape only. Empty positions array is valid; this gates
echo "  [4/7] positions.list..."
positions_json="$(run_cli positions positions --json)"
positions_shape_ok="$(printf '%s' "$positions_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("ok" if isinstance(d.get("stocks"), list) and isinstance(d.get("options"), list) else "bad")
')"
if [[ "$positions_shape_ok" != "ok" ]]; then
    echo "release-verify: FAIL: positions.list missing stocks/options arrays" >&2
    echo "$positions_json" >&2
    exit 1
fi
if printf '%s' "$positions_json" | python3 -c "$python_haskey" data_type; then
    echo "release-verify: FAIL: positions.list leaked data_type field (v0.15 must omit it)" >&2
    echo "$positions_json" >&2
    exit 1
fi

# 5 — quote SPY: pins the quote.snapshot handler and the data_type
# — that's an acceptable degraded state; we only fail on a hard error
echo "  [5/7] quote SPY..."
quote_json="$(run_cli quote_SPY quote SPY --json)"
quote_check="$(printf '%s' "$quote_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
sym = d.get("symbol")
if sym != "SPY":
    print("symbol=" + repr(sym) + ", want SPY")
    sys.exit(0)
dt = d.get("data_type", "")
valid = {"live", "delayed", "frozen", "delayed-frozen"}
if dt not in valid:
    print("data_type=" + repr(dt) + ", want one of " + str(sorted(valid)))
    sys.exit(0)
if not any(d.get(k) is not None for k in ("bid", "ask", "last", "mark")):
    print("quote has no bid/ask/last/mark price; refusing all-empty successful snapshot")
    sys.exit(0)
vol = d.get("volume")
if vol is not None and vol > 1_000_000_000:
    print("volume=" + repr(vol) + " exceeds 1B shares; likely IBKR Decimal size not normalised")
    sys.exit(0)
print("ok")
')"
if [[ "$quote_check" != "ok" ]]; then
    echo "release-verify: FAIL: quote SPY: $quote_check" >&2
    echo "$quote_json" >&2
    exit 1
fi

# 6 — breadth: pins the v0.27.3 BreadthSPXResult.State contract. Three
# bugs in the v0.27.x cycle (bootstrap race, poison-cache, idle kill)
#   - if state == "ready", pct_above_50dma MUST be > 0 (a successful
#     50DMA; v0.27.0's poison-cache produced 0 with what would have
#     been state=ready — the exact regression this gates against)
#   - if state == "cold", pct_above_50dma MUST be 0 (engine hasn't run;
# cold-start fan-out takes ~60 min (IBKR pacing) — release-verify
# every release a CI ordeal. The invariant we CAN check in seconds
echo "  [6/7] breadth.spx state..."
breadth_json="$(run_cli breadth breadth --json)"
breadth_check="$(printf '%s' "$breadth_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
state = d.get("state")
value = d.get("pct_above_50dma", 0)
valid_states = {"cold", "computing", "ready", "degraded"}
if state not in valid_states:
    print("state=" + repr(state) + ", want one of " + str(sorted(valid_states)))
    sys.exit(0)
if state == "ready" and value <= 0:
    print("state=ready but pct_above_50dma=" + repr(value) + " — successful finalise must produce non-zero S5FI (regression of v0.27.0 poison-cache)")
    sys.exit(0)
if state == "cold" and value != 0:
    print("state=cold but pct_above_50dma=" + repr(value) + " — engine that never ran cannot have a value")
    sys.exit(0)
print("ok (state=" + state + " pct_above_50dma=" + repr(value) + ")")
')"
if [[ "$breadth_check" != ok* ]]; then
    echo "release-verify: FAIL: breadth.spx: $breadth_check" >&2
    echo "$breadth_json" >&2
    exit 1
fi
echo "    $breadth_check"

# 7 — regime call-sequence drop check. Pins the user-facing contract that
# must not silently flip to error/unavailable on call N+1. Two calls 30 s
# between fetches and the release blocks.
echo "  [7/7] regime call-sequence (call N+1 doesn't lose what call N had)..."
# Wait for breadth's cold-start fan-out to be in flight before firing
# smoke daemon enters this exact state on its own — postConnectSetup
# Max 20 s wait; if breadth never enters refreshing (e.g. persisted
echo "    waiting up to 20s for breadth mid-fan-out (reproduces v0.27.5 contention)..."
saw_breadth=""
for _ in $(seq 1 20); do
    status_check="$(timeout 5 "$BIN" status --json 2>/dev/null || true)"
    if printf '%s' "$status_check" | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
    tasks = d.get("background_tasks") or []
    for t in tasks:
        if isinstance(t, dict) and t.get("name") == "breadth-spx":
            sys.exit(0)
    sys.exit(1)
except Exception:
    sys.exit(1)
' 2>/dev/null; then
        saw_breadth="yes"
        break
    fi
    sleep 1
done
if [[ -n "$saw_breadth" ]]; then
    echo "    breadth fan-out detected; letting it warm up for 8s before regime"
    sleep 8
else
    echo "    breadth not refreshing within window (snapshot fresh or bootstrap skipped); proceeding"
fi

# Regime fans out five fetchers; the slowest leg is a 20 s history budget,
saved_per_cmd_timeout="$PER_CMD_TIMEOUT"
PER_CMD_TIMEOUT=30
regime_json_1="$(run_cli regime_1 regime --json)"
sleep 30
regime_json_2="$(run_cli regime_2 regime --json)"
PER_CMD_TIMEOUT="$saved_per_cmd_timeout"
regime_check="$(python3 -c '
import json, sys
a = json.loads(sys.argv[1])
b = json.loads(sys.argv[2])
rows = ["vix_term_structure", "hyg_spy_divergence", "usd_jpy", "gamma_zero"]
drops = []
for r in rows:
    s1 = a.get(r, {}).get("status", "")
    s2 = b.get(r, {}).get("status", "")
    if s1 in ("ok", "stale") and s2 in ("error", "unavailable"):
        drops.append(r + ": " + s1 + " -> " + s2)
if drops:
    print("DROP " + "; ".join(drops))
    sys.exit(0)
print("ok (no rows downgraded between calls)")
' "$regime_json_1" "$regime_json_2")"
if [[ "$regime_check" != ok* ]]; then
    echo "release-verify: FAIL: regime sequence: $regime_check" >&2
    echo "" >&2
    echo "call 1:" >&2
    echo "$regime_json_1" >&2
    echo "call 2:" >&2
    echo "$regime_json_2" >&2
    exit 1
fi
echo "    $regime_check"

# Shape-coherence: under the contention reproduced above (breadth
# 45 s orchestrator deadline is the safety net for genuinely-stuck
# trigger the safety net instead). A row's ErrorMessage containing
# per-fetcher fix didn't fire — exactly the 2026-05-19 production
# bug shape, and a release-blocker.
shape_check="$(python3 -c '
import json, sys
findings = []
for label, payload in (("call 1", sys.argv[1]), ("call 2", sys.argv[2])):
    d = json.loads(payload)
    for r in ("vix_term_structure", "hyg_spy_divergence", "usd_jpy"):
        msg = d.get(r, {}).get("error_message", "") or ""
        if "fan-out exceeded handler deadline" in msg:
            findings.append(label + " " + r + ": orchestrator safety net triggered (per-fetcher budget did not bail in time); message=" + repr(msg))
if findings:
    print("FALLBACK " + " | ".join(findings))
    sys.exit(0)
print("ok (no row hit the orchestrator-deadline fallback on either call)")
' "$regime_json_1" "$regime_json_2")"
if [[ "$shape_check" != ok* ]]; then
    echo "release-verify: FAIL: regime shape: $shape_check" >&2
    echo "" >&2
    echo "call 1:" >&2
    echo "$regime_json_1" >&2
    echo "call 2:" >&2
    echo "$regime_json_2" >&2
    exit 1
fi
echo "    $shape_check"

echo ""
echo "release-verify: PASS — $BIN ($EXPECTED) talks to the gateway and ships honest JSON"
