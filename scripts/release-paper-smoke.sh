#!/usr/bin/env bash
#
# release-paper-smoke.sh — binding release gate: run the daemon-observed
# paper order round-trip (`ibkr trading paper-smoke`: place 1-share
# far-off-market SPY LMT → broker ack → cancel → cancel confirm) against
# an isolated daemon pinned to the local *paper* TWS/Gateway session.
#
# Wired into `make release` at version bump (2026-06-10 decision): the
# order pipeline is verified automatically per release instead of by a
# human-certified runtime gate. The gate is BINDING — there is no SKIP:
#   - no paper session reachable  → release aborts (log the paper account
#     in first; a live session is never used for the smoke)
#   - non-DU account on the paper port → release aborts
#   - smoke result != passed      → release aborts
#
# Usage:
#   scripts/release-paper-smoke.sh [--preview-only] <bin/ibkr>
#
# --preview-only runs a production order preview through account-currency,
# notional/FX, and broker WhatIf authority, then exits without placing an
# order. The release target runs this inexpensive read-only mode before its
# full test gate; the binding round-trip remains after every test.
#
# Environment hooks:
#   IBKR_TEST_HOST        — gateway host (default 127.0.0.1)
#   IBKR_PAPER_PORTS      — space-separated paper probe ports (default "4002 7497")
#   IBKR_SMOKE_CLIENT_ID  — client ID for the isolated daemon (default derived)
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$SCRIPT_DIR/lib-daemon-control.sh"

MODE="smoke"
if [[ "${1:-}" == "--preview-only" ]]; then
    MODE="preview"
    shift
fi

BIN="${1:?usage: release-paper-smoke.sh [--preview-only] <bin/ibkr>}"
if [[ ! -x "$BIN" ]]; then
    echo "release-paper-smoke: $BIN not executable" >&2
    exit 2
fi

HOST="${IBKR_TEST_HOST:-127.0.0.1}"
# Paper ports ONLY (Gateway 4002, TWS 7497). The smoke transmits a real
# order; the live ports are deliberately not probe candidates.
read -r -a probe_ports <<<"${IBKR_PAPER_PORTS:-4002 7497}"

PORT=""
for port in "${probe_ports[@]}"; do
    if timeout 2 bash -c "exec 3<>/dev/tcp/${HOST}/${port}" 2>/dev/null; then
        PORT="$port"
        break
    fi
done
if [[ -z "$PORT" ]]; then
    echo "release-paper-smoke: FAIL — no paper TWS/Gateway reachable at ${HOST} ports ${probe_ports[*]}." >&2
    if [[ "$MODE" == "preview" ]]; then
        echo "  The read-only release preflight requires paper TWS/Gateway and refuses to inspect a live route." >&2
    else
        echo "  The release gate transmits a 1-share paper round-trip and refuses to run against live." >&2
    fi
    echo "  Log TWS (7497) or IB Gateway (4002) into the paper account, then re-run \`make release\`." >&2
    exit 1
fi
echo "release-paper-smoke: paper gateway present at ${HOST}:${PORT}"

CLIENT_ID="${IBKR_SMOKE_CLIENT_ID:-$((300 + ($$ % 600)))}"

TMPDIR_BASE="${TMPDIR:-/tmp}"
SMOKE_DIR="$(mktemp -d "$TMPDIR_BASE/ibkr-paper-smoke-XXXXXX")"
SOCKET="$SMOKE_DIR/ibkr.sock"
LOG="$SMOKE_DIR/ibkr-daemon.log"
LOCK="$SMOKE_DIR/ibkr.lock"
CONFIG="$SMOKE_DIR/config.toml"

export IBKR_SOCKET="$SOCKET"
export IBKR_LOG="$LOG"
export IBKR_CONFIG="$CONFIG"
# Isolated trading state: evidence, journal, and tokens must not touch the
# user's canonical daemon state.
export XDG_STATE_HOME="$SMOKE_DIR/state"
export XDG_CACHE_HOME="$SMOKE_DIR/cache"

cleanup() {
    local code=$?
    kill_daemon_from_lockfile "$LOCK"
    if [[ $code -ne 0 && -r "$LOG" ]]; then
        echo "" >&2
        echo "release-paper-smoke: isolated daemon log withheld because it may contain private broker identifiers" >&2
    fi
    rm -rf "$SMOKE_DIR" 2>/dev/null || true
    return $code
}
trap cleanup EXIT INT TERM

# Phase 1 — data-only daemon pinned to the paper port: discover the
# concrete paper account (order gates need a pinned non-aggregate account).
cat > "$CONFIG" <<EOF
[gateway]
host = "$HOST"
port = $PORT
client_id = $CLIENT_ID
tls = false
EOF

# `account --json` reports the aggregate "All" summary on an unpinned
# daemon; `status --json` carries the concrete session account. The field
# stays empty until the gateway handshake completes, so poll like
# release-smoke.sh does for status.connected.
ACCOUNT=""
for _ in $(seq 1 100); do
    ACCOUNT="$(timeout 30 "$BIN" status --json | python3 -c 'import json,sys; print(json.load(sys.stdin).get("connected_account",""))')"
    [[ -n "$ACCOUNT" ]] && break
    sleep 0.25
done
if [[ -z "$ACCOUNT" ]]; then
    echo "release-paper-smoke: FAIL — could not resolve the connected account on ${HOST}:${PORT} after 25s" >&2
    exit 1
fi
case "$ACCOUNT" in
    DU*|du*) ;;
    *)
        echo "release-paper-smoke: FAIL — connected account on ${HOST}:${PORT} is not classified as paper; refusing to transmit" >&2
        exit 1
        ;;
esac
if [[ "$MODE" == "preview" ]]; then
    echo "release-paper-preflight: paper account verified (redacted)"
else
    echo "release-paper-smoke: paper account verified (redacted)"
fi

# Phase 2 — restart the isolated daemon with the trading gate pinned to
# that account, then run the smoke through the production order path.
kill_daemon_from_lockfile "$LOCK"
cat > "$CONFIG" <<EOF
[gateway]
host = "$HOST"
port = $PORT
client_id = $CLIENT_ID
account = "$ACCOUNT"
tls = false

[trading]
mode = "paper"
EOF

# Fresh autospawned daemon: wait for the gateway handshake before the
# smoke, or its reference-quote leg fails fast with gateway_unavailable.
CONNECTED=""
for _ in $(seq 1 100); do
    CONNECTED="$(timeout 30 "$BIN" status --json | python3 -c 'import json,sys; print(json.load(sys.stdin).get("connected",False))')"
    [[ "$CONNECTED" == "True" ]] && break
    sleep 0.25
done
if [[ "$CONNECTED" != "True" ]]; then
    echo "release-paper-smoke: FAIL — daemon did not connect to ${HOST}:${PORT} within 25s" >&2
    exit 1
fi

if [[ "$MODE" == "preview" ]]; then
    QUOTE_OUT="$(timeout 30 "$BIN" quote SPY --json)" || {
        echo "release-paper-preflight: FAIL — SPY reference quote errored" >&2
        exit 1
    }
    if ! LIMIT_PRICE="$(printf '%s' "$QUOTE_OUT" | python3 -c '
import json
import math
import sys

quote = json.load(sys.stdin)
reference = next(
    (float(quote[name]) for name in ("bid", "last", "mark", "price", "ask")
     if quote.get(name) is not None and float(quote[name]) > 0),
    0,
)
if reference <= 0:
    sys.exit(1)
limit = math.floor((reference * 0.98 + 1e-9) * 100) / 100
if limit <= 0:
    sys.exit(1)
print(f"{limit:.2f}")
')"; then
        echo "release-paper-preflight: FAIL — SPY reference quote had no usable price" >&2
        exit 1
    fi
    PREVIEW_OUT="$(timeout 90 "$BIN" order preview buy SPY 1 \
        --strategy explicit-limit --order-type LMT --limit "$LIMIT_PRICE" --tif DAY --json)" || {
        echo "release-paper-preflight: FAIL — read-only SPY preview errored" >&2
        exit 1
    }
    if ! PREVIEW_SUMMARY="$(printf '%s' "$PREVIEW_OUT" | python3 -c '
import json
import math
import sys

data = json.load(sys.stdin)
what_if = data.get("what_if") or {}
notional_currency = str(data.get("notional_currency") or "").upper()
base_currency = str(data.get("base_currency") or "").upper()
fx_rate = data.get("fx_rate")
fx_data_type = data.get("fx_data_type")
fx_source = data.get("fx_source")
fx_evidence_at = data.get("fx_evidence_at")
fx_ok = (
    len(notional_currency) == 3
    and len(base_currency) == 3
    and notional_currency != base_currency
    and isinstance(fx_rate, (int, float))
    and math.isfinite(fx_rate)
    and fx_rate > 0
    and isinstance(fx_evidence_at, str)
    and bool(fx_evidence_at.strip())
    and fx_data_type in {"live", "frozen", "delayed", "delayed-frozen"}
    and fx_source == "ibkr.tws.exact_fx_quote"
)
summary = "mode={} token_minted={} submit_eligible={} what_if={} cross_currency_fx={}".format(
    data.get("mode", "unknown"),
    bool(data.get("token_minted")),
    bool(data.get("submit_eligible")),
    what_if.get("status", "unknown"),
    fx_ok,
)
print(summary)
ok = (
    data.get("mode") == "paper"
    and data.get("token_minted") is True
    and data.get("submit_eligible") is True
    and data.get("executable") is True
    and (data.get("draft") or {}).get("contract", {}).get("symbol") == "SPY"
    and fx_ok
)
sys.exit(0 if ok else 1)
')"; then
        echo "release-paper-preflight: FAIL — preview did not clear the paper submit-eligibility contract ($PREVIEW_SUMMARY)" >&2
        exit 1
    fi
    echo "release-paper-preflight: PASS — account/cross-currency FX/WhatIf preview is eligible; no order submitted ($PREVIEW_SUMMARY)"
    exit 0
fi

OUT="$(timeout 150 "$BIN" trading paper-smoke --json)" || {
    echo "release-paper-smoke: FAIL — paper-smoke command errored; raw broker response withheld" >&2
    exit 1
}
RESULT="$(printf '%s' "$OUT" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("result",""))')" || {
    echo "release-paper-smoke: FAIL — paper-smoke returned invalid JSON; raw broker response withheld" >&2
    exit 1
}
if [[ "$RESULT" != "passed" ]]; then
    SAFE_SUMMARY="$(printf '%s' "$OUT" | python3 -c '
import json
import sys

data = json.load(sys.stdin)
print(
    "mode={} symbol={} quantity={} acknowledged={} cancelled={} evidence_saved={}".format(
        data.get("mode", "unknown"),
        data.get("symbol", "unknown"),
        data.get("quantity", "unknown"),
        data.get("ack_lifecycle_status", "unknown"),
        data.get("cancel_lifecycle_status", "unknown"),
        bool(data.get("evidence_saved")),
    )
)
')"
    echo "release-paper-smoke: FAIL — smoke result '$RESULT' (want passed; $SAFE_SUMMARY; private identifiers withheld)" >&2
    exit 1
fi
echo "release-paper-smoke: PASS — order pipeline round-trip confirmed on verified paper account (private identifiers withheld)"
