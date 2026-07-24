#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ibkr-paper-preflight-test.XXXXXX")"
cleanup() {
    rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

mkdir -p "$test_root/bin"
call_log="$test_root/calls.log"

cat >"$test_root/bin/timeout" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
shift
if [[ "${1:-}" == "bash" && "${2:-}" == "-c" && "${3:-}" == exec\ 3* ]]; then
    exit 0
fi
exec "$@"
EOF
chmod +x "$test_root/bin/timeout"

cat >"$test_root/fake-ibkr" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$PAPER_PREFLIGHT_CALL_LOG"
case "$*" in
    "status --json")
        printf '%s\n' '{"connected":true,"connected_account":"DU_TEST"}'
        ;;
    "quote SPY --json")
        printf '%s\n' '{"symbol":"SPY","bid":500.00,"ask":500.02,"last":500.01}'
        ;;
    "order preview buy SPY 1 --strategy explicit-limit --order-type LMT --limit 490.00 --tif DAY --json")
        if [[ "${PAPER_PREFLIGHT_ELIGIBLE:-1}" == "1" ]]; then
            if [[ "${PAPER_PREFLIGHT_FX:-valid}" == "valid" ]]; then
                printf '%s\n' '{"token_minted":true,"submit_eligible":true,"executable":true,"mode":"paper","draft":{"contract":{"symbol":"SPY"}},"notional_currency":"USD","base_currency":"EUR","fx_rate":0.92,"fx_evidence_at":"2026-07-24T08:00:00Z","fx_data_type":"live","fx_source":"ibkr.tws.exact_fx_quote","what_if":{"status":"accepted"}}'
            else
                printf '%s\n' '{"token_minted":true,"submit_eligible":true,"executable":true,"mode":"paper","draft":{"contract":{"symbol":"SPY"}},"notional_currency":"USD","base_currency":"USD","fx_rate":1,"fx_source":"currency_identity","what_if":{"status":"accepted"}}'
            fi
        else
            printf '%s\n' '{"token_minted":true,"submit_eligible":false,"executable":false,"mode":"paper","draft":{"contract":{"symbol":"SPY"}},"notional_currency":"USD","base_currency":"EUR","fx_rate":0.92,"fx_evidence_at":"2026-07-24T08:00:00Z","fx_data_type":"live","fx_source":"ibkr.tws.exact_fx_quote","what_if":{"status":"rejected"}}'
        fi
        ;;
    "trading paper-smoke --json")
        if [[ "${PAPER_SMOKE_FAKE_RESULT:-}" == "passed" ]]; then
            printf '%s\n' '{"passed":true,"result":"passed","mode":"paper","account":"DU_TEST","symbol":"SPY","quantity":1,"order_ref":"sensitive-order-ref","ack_lifecycle_status":"acknowledged","cancel_lifecycle_status":"cancelled","evidence_saved":true}'
        elif [[ "${PAPER_SMOKE_FAKE_RESULT:-}" == "failed" ]]; then
            printf '%s\n' '{"passed":false,"result":"failed","mode":"paper","account":"DU_TEST","symbol":"SPY","quantity":1,"order_ref":"sensitive-order-ref","ack_lifecycle_status":"acknowledged","cancel_lifecycle_status":"cancel_pending","evidence_saved":true,"message":"inspect sensitive-order-ref on DU_TEST"}'
        else
            echo "preview-only test reached the transmitting command" >&2
            exit 99
        fi
        ;;
    *)
        echo "unexpected fake ibkr command: $*" >&2
        exit 98
        ;;
esac
EOF
chmod +x "$test_root/fake-ibkr"

export PATH="$test_root/bin:$PATH"
export PAPER_PREFLIGHT_CALL_LOG="$call_log"
export IBKR_PAPER_PORTS="7497"

"$repo_root/scripts/release-paper-smoke.sh" --preview-only "$test_root/fake-ibkr" >/dev/null
grep -Fxq "quote SPY --json" "$call_log"
grep -Fxq "order preview buy SPY 1 --strategy explicit-limit --order-type LMT --limit 490.00 --tif DAY --json" "$call_log"
if grep -Fq "trading paper-smoke" "$call_log"; then
    echo "release-paper-smoke test: preview-only mode transmitted an order" >&2
    exit 1
fi

: >"$call_log"
if PAPER_PREFLIGHT_ELIGIBLE=0 "$repo_root/scripts/release-paper-smoke.sh" --preview-only "$test_root/fake-ibkr" >/dev/null 2>&1; then
    echo "release-paper-smoke test: ineligible preview unexpectedly passed" >&2
    exit 1
fi
grep -Fxq "quote SPY --json" "$call_log"
grep -Fxq "order preview buy SPY 1 --strategy explicit-limit --order-type LMT --limit 490.00 --tif DAY --json" "$call_log"
if grep -Fq "trading paper-smoke" "$call_log"; then
    echo "release-paper-smoke test: failed preflight reached transmitting command" >&2
    exit 1
fi

: >"$call_log"
if PAPER_PREFLIGHT_FX=identity "$repo_root/scripts/release-paper-smoke.sh" --preview-only "$test_root/fake-ibkr" >/dev/null 2>&1; then
    echo "release-paper-smoke test: same-currency preview unexpectedly claimed cross-currency FX coverage" >&2
    exit 1
fi
if grep -Fq "trading paper-smoke" "$call_log"; then
    echo "release-paper-smoke test: invalid FX preflight reached transmitting command" >&2
    exit 1
fi

smoke_output="$test_root/smoke-output.txt"
PAPER_SMOKE_FAKE_RESULT=passed "$repo_root/scripts/release-paper-smoke.sh" "$test_root/fake-ibkr" >"$smoke_output" 2>&1
grep -Fq "private identifiers withheld" "$smoke_output"
if grep -Eq 'DU_TEST|sensitive-order-ref' "$smoke_output"; then
    echo "release-paper-smoke test: successful smoke exposed private identifiers" >&2
    exit 1
fi

if PAPER_SMOKE_FAKE_RESULT=failed "$repo_root/scripts/release-paper-smoke.sh" "$test_root/fake-ibkr" >"$smoke_output" 2>&1; then
    echo "release-paper-smoke test: failed smoke unexpectedly passed" >&2
    exit 1
fi
grep -Fq "acknowledged=acknowledged cancelled=cancel_pending evidence_saved=True" "$smoke_output"
if grep -Eq 'DU_TEST|sensitive-order-ref' "$smoke_output"; then
    echo "release-paper-smoke test: failed smoke exposed private identifiers" >&2
    exit 1
fi

echo "release-paper-smoke test: OK"
