#!/usr/bin/env bash
#
# protocol-level invariants.
# This is the single retained gateway/wire smoke used by ordinary and manually
# invoked release verification targets.
#     test/integration so `make release` works on a laptop without IBKR
#   CANARY_SMOKE_STRICT     — 1 = FAIL on no-gateway instead of SKIP (release path)
#   CANARY_SMOKE_FAST       — 1 = stop after boot + quote + account (~15s inner-loop
#   SPX_EXPECTED_REACHABLE  — 1 (default in `make smoke`) = the SPX daemon probe
#                             must return real SPX data; banner-seen FAILS the run.
#                             0 = banner-seen is a clean skip (CI / accounts without
#                             regression between releases (design §11.2).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib-daemon-control.sh"
. "$SCRIPT_DIR/lib-release-smoke-wait.sh"

BIN="${1:?usage: wire-smoke.sh <bin/canary> <bin/wire-assert>}"
ASSERT="${2:?usage: wire-smoke.sh <bin/canary> <bin/wire-assert>}"

if [[ ! -x "$BIN" ]]; then
    echo "wire-smoke: $BIN not executable" >&2
    exit 2
fi
if [[ ! -x "$ASSERT" ]]; then
    echo "wire-smoke: $ASSERT not executable (run 'make smoke-build')" >&2
    exit 2
fi

GATEWAY_HOST="${IBKR_TEST_HOST:-127.0.0.1}"
if [[ ! "$GATEWAY_HOST" =~ ^[A-Za-z0-9._:-]+$ ]]; then
    echo "wire-smoke: invalid IBKR_TEST_HOST: $GATEWAY_HOST" >&2
    exit 2
fi
SMOKE_CLIENT_ID="${CANARY_SMOKE_CLIENT_ID:-$((200 + ($$ % 600)))}"
if [[ ! "$SMOKE_CLIENT_ID" =~ ^[0-9]+$ ]] || (( SMOKE_CLIENT_ID < 1 || SMOKE_CLIENT_ID > 998 )); then
    echo "wire-smoke: invalid CANARY_SMOKE_CLIENT_ID: $SMOKE_CLIENT_ID" >&2
    exit 2
fi
BREADTH_CLIENT_ID=$((SMOKE_CLIENT_ID + 1))
# 60s default. The option-chain probe can take ~30s when 22 legs
# need contract resolution from a cold cache (observed 2026-05-18:
PER_CMD_TIMEOUT="${CANARY_SMOKE_TIMEOUT:-60}"

# 1. Gateway-presence probe. Default posture matches test/integration:
# a missing gateway is SKIP (exit 0), not FAIL — `make smoke` from a
# laptop without paper-account IBKR access must still pass. The release
# path overrides via CANARY_SMOKE_STRICT=1 to FAIL on no-gateway, so a
# release can't silently bypass the wire gate. The probe uses bash's
STRICT="${CANARY_SMOKE_STRICT:-0}"
GATEWAY_PORT="${IBKR_TEST_PORT:-}"
if [[ -n "$GATEWAY_PORT" ]]; then
    if [[ ! "$GATEWAY_PORT" =~ ^[0-9]+$ ]] || (( GATEWAY_PORT < 1 || GATEWAY_PORT > 65535 )); then
        echo "wire-smoke: invalid IBKR_TEST_PORT: $GATEWAY_PORT" >&2
        exit 2
    fi
    probe_ports=("$GATEWAY_PORT")
else
    # Preserve the old TWS-live preference, then accept TWS/Gateway paper
    # because the smoke is read-only and wire-equivalent for these checks.
    read -r -a probe_ports <<<"${IBKR_TEST_PORTS:-7496 7497 4001 4002}"
fi

GATEWAY_PORT=""
for port in "${probe_ports[@]}"; do
    if [[ ! "$port" =~ ^[0-9]+$ ]] || (( port < 1 || port > 65535 )); then
        echo "wire-smoke: invalid probe port: $port" >&2
        exit 2
    fi
    if timeout 2 bash -c "exec 3<>/dev/tcp/${GATEWAY_HOST}/${port}" 2>/dev/null; then
        GATEWAY_PORT="$port"
        break
    fi
done

if [[ -z "$GATEWAY_PORT" ]]; then
    candidates="${probe_ports[*]}"
    if [[ "$STRICT" == "1" ]]; then
        echo "wire-smoke: FAIL — no gateway reachable at ${GATEWAY_HOST} ports ${candidates} (STRICT mode; release path must exercise TWS/Gateway)" >&2
        exit 1
    fi
    echo "wire-smoke: SKIP — no gateway reachable at ${GATEWAY_HOST} ports ${candidates}"
    exit 0
fi
echo "wire-smoke: gateway present at ${GATEWAY_HOST}:${GATEWAY_PORT}"

# 2. Isolated daemon under /tmp so the smoke never touches the user's canonical
# daemon. The wire interceptor is the test surface designed for this use case.
TMPDIR_BASE="${TMPDIR:-/tmp}"
SMOKE_DIR="$(mktemp -d "$TMPDIR_BASE/ibkr-wire-smoke-XXXXXX")"
SOCKET="$SMOKE_DIR/ibkr.sock"
LOG="$SMOKE_DIR/ibkr-daemon.log"
LOCK="$SMOKE_DIR/ibkr.lock"
WIRE_LOG="$SMOKE_DIR/wire.jsonl"
CONFIG="$SMOKE_DIR/config.toml"

cat > "$CONFIG" <<EOF
[gateway]
host = "$GATEWAY_HOST"
port = $GATEWAY_PORT
client_id = $SMOKE_CLIENT_ID
breadth_client_id = $BREADTH_CLIENT_ID
tls = false
EOF

export CANARY_SOCKET="$SOCKET"
export CANARY_LOG="$LOG"
export CANARY_CONFIG="$CONFIG"
# Isolated trading state: marks, journals, and tokens must not touch the
# in operator state failed the v1.15.0 release smoke, 2026-07-08).
export XDG_STATE_HOME="$SMOKE_DIR/state"
export XDG_CACHE_HOME="$SMOKE_DIR/cache"
export IBKR_WIRE_INTERCEPTOR=1
export IBKR_WIRE_LOG_PATH="$WIRE_LOG"
export IBKR_WIRE_RING_SIZE=4096

cleanup() {
    local code=$?
    kill_daemon_from_lockfile "$LOCK"
    # On failure, surface the daemon log tail and the last few wire
    # frames — the failure-mode is in the wire data, not in the CLI's
    if [[ $code -ne 0 ]]; then
        if [[ -r "$LOG" ]]; then
            echo ""
            echo "wire-smoke: daemon log tail ($LOG):" >&2
            tail -30 "$LOG" >&2 || true
        fi
        if [[ -r "$WIRE_LOG" ]]; then
            echo ""
            echo "wire-smoke: last 5 wire frames ($WIRE_LOG):" >&2
            tail -5 "$WIRE_LOG" >&2 || true
        fi
    fi
    rm -rf "$SMOKE_DIR" 2>/dev/null || true
    return $code
}
trap cleanup EXIT INT TERM

# Normal local smoke runs use a unique client ID and leave the user's daemon
if [[ "${CANARY_SMOKE_STOP_EXISTING:-0}" == "1" ]]; then
    stop_existing_daemons wire-smoke
fi

# Run a CLI command with a deadline; on failure, print the command +
run_cli() {
    local label="$1"
    shift
    LAST_CMD_EXIT=0
    LAST_CMD_OUTPUT="$(timeout "$PER_CMD_TIMEOUT" "$BIN" "$@" 2>&1)" || LAST_CMD_EXIT=$?
}

# Reach retained read-only daemon methods through the internal smoke helper.
# These probes replace retired user-facing CLI adapters without expanding the
# product surface or granting any broker-write authority.
run_probe() {
    local label="$1"
    local probe="$2"
    shift 2
    LAST_CMD_EXIT=0
    LAST_CMD_OUTPUT="$(timeout "$PER_CMD_TIMEOUT" "$ASSERT" --probe "$probe" --socket "$SOCKET" "$@" 2>&1)" || LAST_CMD_EXIT=$?
}

# Run one named wire-assert check against the whole JSONL. Per-command
# at boot (SPY for the regime path, ARCA contract lookups, etc.), so
assert_wire() {
    local check="$1"
    local envelope="${2:-}"
    local args=(--jsonl "$WIRE_LOG" --check "$check")
    if [[ "${LOOSE:-0}" -eq 1 ]]; then
        args+=(--loose)
    fi
    if [[ -n "$envelope" ]]; then
        args+=(--envelope-path "$envelope")
    fi
    if ! "$ASSERT" "${args[@]}"; then
        echo "" >&2
        echo "wire-smoke: aborting on first failure" >&2
        exit 1
    fi
}

echo "wire-smoke: isolated daemon → $SOCKET"
echo "wire-smoke: wire log → $WIRE_LOG"
echo "wire-smoke: client IDs → primary=$SMOKE_CLIENT_ID breadth=$BREADTH_CLIENT_ID"

# 4. Boot the daemon by issuing a status call (which autospawns one at
# the isolated socket). Wait for the gateway to be connected — give it
# 25s, same budget as the integration suite.
echo "  [boot] autospawning daemon..."

# 0.25s poll granularity: the daemon typically connects in 2-4s, and a 1s
for attempt in $(seq 1 100); do
    if "$BIN" status --json 2>/dev/null | grep -q '"connected": *true'; then
        break
    fi
    sleep 0.25
    if [[ $attempt -eq 100 ]]; then
        echo "wire-smoke: FAIL: daemon never reached connected=true within 25s" >&2
        exit 1
    fi
done
assert_wire status-handshake
echo "  [boot] ok"

# 5. Detect frozen/off-hours mode by querying SPY's quote state. IBKR can
# engine may be idle. In loose mode the chain-iv-source check warns
# instead of failing.

run_probe quote-spy quote --symbol SPY
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: quote SPY exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
data_type="$(echo "$LAST_CMD_OUTPUT" | grep -o '"data_type": *"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/')"
quote_quality="$(echo "$LAST_CMD_OUTPUT" | grep -o '"quote_quality": *"[^"]*"' | head -1 | sed 's/.*"\(.*\)"/\1/')"
off_hours=0
if grep -q '"code": *"off_hours_quote"' <<<"$LAST_CMD_OUTPUT"; then
    off_hours=1
fi
case "$data_type" in
    live)
        if [[ "$off_hours" -eq 0 && "$quote_quality" == "firm" ]]; then
            LOOSE=0
            echo "  [mode] live"
        else
            LOOSE=1
            echo "  [mode] live/${quote_quality:-unknown} off-hours — loose (model engine may be idle)"
        fi
        ;;
    frozen|delayed|delayed-frozen|"")
        LOOSE=1
        echo "  [mode] $data_type — loose (model engine may be idle)"
        ;;
    *)
        LOOSE=1
        echo "  [mode] unknown ($data_type) — loose"
        ;;
esac
assert_wire quote-spy

# 6. account.summary — pins account-level reqAccountSummary path.
echo "  [account]..."

run_cli account account --json
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: account exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
assert_wire account-summary

# Fast tier exits here: handshake, quote, and account-summary wire paths
# (`make smoke`) and the release gates.
if [[ "${CANARY_SMOKE_FAST:-0}" == "1" ]]; then
    echo ""
    echo "wire-smoke: PASS (fast tier) — boot + quote + account wire contract checks passed"
    echo "wire-smoke: run the full \`make smoke\` for daemon/CLI wire-path changes"
    exit 0
fi

# Settle the primary-client startup computes before the heavy interactive
# reads — the posture release-smoke adopted after the 2026-08-03 v2.7.0
# release fire burned 480s+60s waiting on it and every read then passed
wire_smoke_status_provider() {
    local output_var="$1"
    local status_json=""
    if ! status_json="$(timeout 5 "$BIN" status --json 2>/dev/null)"; then
        return 1
    fi
    printf -v "$output_var" '%s' "$status_json"
}

SETTLE_TASKS="gamma-zero regime-prewarm"
echo "  [settle] waiting up to 180s for the primary-client startup tasks ($SETTLE_TASKS) to drain..."
if ! release_smoke_settle_or_fail wire_smoke_status_provider "$SETTLE_TASKS" 180 chain; then
    exit 1
fi

# 7. chain with a near expiry — pins the IV-source path that the
# v0.24.x bug broke. In loose mode this check warns instead of failing.
echo "  [chain SPY 1-wide]..."

# Pick the third near expiry from the retained typed daemon response.
expiries="$("$ASSERT" --probe chain-expiries --socket "$SOCKET" --symbol SPY 2>/dev/null | grep -o '"date":[[:space:]]*"[0-9-]*"' | head -3 | tail -1 | cut -d'"' -f4)"
if [[ -z "$expiries" ]]; then
    echo "wire-smoke: FAIL: could not list SPY expiries through the daemon probe" >&2
    exit 1
fi
run_probe chain-iv chain --symbol SPY --expiry "$expiries" --width 1 --side both
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: chain exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
CHAIN_ENV="$SMOKE_DIR/chain-iv-envelope.json"
printf '%s' "$LAST_CMD_OUTPUT" > "$CHAIN_ENV"
assert_wire chain-iv-source "$CHAIN_ENV"

# 8. regime — the dashboard's fan-out. Asserts all 5 indicator
echo "  [regime]..."

run_probe regime regime
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: regime exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
assert_wire regime-subs

# 9. gamma --no-wait — proves the non-blocking path returns a typed lifecycle
echo "  [gamma --no-wait]..."

run_probe gamma gamma
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: gamma --no-wait exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
GAMMA_ENV="$SMOKE_DIR/gamma-no-wait-envelope.json"
printf '%s' "$LAST_CMD_OUTPUT" > "$GAMMA_ENV"
assert_wire gamma-no-wait-envelope "$GAMMA_ENV"

# 10. SPX coverage check — exercises the `--only=spx` path landed in
# the gamma-spx-coverage arc. Per design §11.2: on this dev machine
# `SPX_EXPECTED_REACHABLE=1` flips banner-seen from clean-skip to
# loud-fail, preventing silent SPX regression. CI accounts without
# CBOE OPRA can disable via the env var.
#
# The check is non-blocking on the SPX compute itself — `--no-wait`
# returns immediately with the current cache state. We only assert
# the daemon ACCEPTED `--only=spx` (didn't reject the scope) and that
# the result envelope doesn't carry the entitlement-skipped banner
# when SPX_EXPECTED_REACHABLE is set.
echo "  [gamma --only=spx --no-wait]..."
run_probe gamma-spx gamma --scope spx
if [[ $LAST_CMD_EXIT -ne 0 ]]; then
    echo "wire-smoke: FAIL: gamma --only=spx exit=$LAST_CMD_EXIT" >&2
    echo "$LAST_CMD_OUTPUT" >&2
    exit 1
fi
if [[ "${SPX_EXPECTED_REACHABLE:-0}" -eq 1 ]]; then
    # Check the result for SPX-skipped warnings. The envelope's
    # `warnings` array carries "spx_unavailable:<reason>" tokens when
    # the combined-mode prewarm degraded. Note: when --only=spx is
    # used, the daemon runs the SPX path directly, so a real
    # entitlement issue surfaces as Status=error here.
    if echo "$LAST_CMD_OUTPUT" | grep -q '"status": *"error"'; then
        echo "wire-smoke: FAIL: SPX_EXPECTED_REACHABLE=1 but the SPX gamma probe returned error" >&2
        echo "$LAST_CMD_OUTPUT" >&2
        exit 1
    fi
    echo "    [spx ok — daemon accepted --only=spx scope, no entitlement error]"
fi

echo ""
mode_label="strict"
if [[ "${LOOSE:-0}" -eq 1 ]]; then mode_label="loose"; fi
echo "wire-smoke: PASS — ${BIN} wire contract checks passed (mode=${mode_label})"
