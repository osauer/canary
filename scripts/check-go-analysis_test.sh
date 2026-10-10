#!/usr/bin/env bash
# Exercise the real Make recipes with synthetic analyzers, including silent
# failures and both build variants. No private state or network is used.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/canary-analysis-test.XXXXXX")
trap 'rm -rf "$fixture"' EXIT HUP INT TERM
cp "$root/Makefile" "$fixture/Makefile"
mkdir -p "$fixture/bin" "$fixture/scripts/docgen/docs-html" "$fixture/tools"
export ANALYSIS_FIXTURE="$fixture"
export PATH="$fixture/bin:$PATH"
export MAKEFLAGS= MFLAGS=
cat > "$fixture/scripts/go-tracked-packages.sh" <<'STUB'
#!/bin/sh
printf 'packages %s\n' "$*" >> "$ANALYSIS_FIXTURE/calls"
[ "${FAIL_STEP:-}" != packages ] || exit 73
printf './fixture\n'
STUB
for script in go-tracked-packages_test.sh check-go-analysis_test.sh; do
    printf '#!/bin/sh\nexit 0\n' > "$fixture/scripts/$script"
done
cat > "$fixture/bin/go" <<'STUB'
#!/bin/sh
printf 'go %s\n' "$*" >> "$ANALYSIS_FIXTURE/calls"
case "$*" in
    version) echo 'go version fixture';;
    '-C tools tool -n '*)
        [ "${FAIL_STEP:-}" != resolve ] || exit 73
        printf '%s/bin/%s\n' "$ANALYSIS_FIXTURE" "$5";;
    'fix -diff '*)
        [ "${FAIL_STEP:-}" != fix ] || exit 73
        [ "${DIAGNOSTIC:-}" != fix ] || echo 'synthetic rewrite';;
    'vet '*)
        [ "${FAIL_STEP:-}" != vet ] || exit 73;;
esac
exit 0
STUB
cat > "$fixture/bin/analyzer" <<'STUB'
#!/bin/sh
name=${0##*/}
if [ "$name" = staticcheck ]; then printf '%s\n' "${STATICCHECK_CACHE:-unset}" >> "$ANALYSIS_FIXTURE/staticcheck-caches"; fi
if [ "$name" = staticcheck ] && [ "${1:-}" = -merge ] && [ "${FAIL_STEP:-}" = merge ]; then exit 73; fi
printf '%s %s\n' "$name" "$*" >> "$ANALYSIS_FIXTURE/calls"
if [ "${FAIL_STEP:-}" = "$name" ]; then
    case " ${*} " in
        *' -tags trading '*) variant=trading;;
        *) variant=default;;
    esac
    [ "${FAIL_VARIANT:-$variant}" != "$variant" ] || exit 73
fi
if [ "$name" = modernize ]; then
    [ "${DIAGNOSTIC:-}" != modernize ] || echo 'synthetic diagnostic' >&2
    [ "${DIAGNOSTIC:-}" != download ] || echo 'go: downloading example.invalid/fixture v1.0.0' >&2
fi
exit 0
STUB
chmod +x "$fixture/bin/"* "$fixture/scripts/"*.sh
for name in modernize staticcheck govulncheck; do
    cp "$fixture/bin/analyzer" "$fixture/bin/$name"
done
run() {
    make -s -C "$fixture" "$1" GOVULN_STAMP="$fixture/stamp" > "$fixture/output" 2>&1
}
fail() { echo "go-analysis test: $*" >&2; cat "$fixture/output" >&2; exit 1; }
reject() { if run "$1"; then fail "$1 accepted ${FAIL_STEP:-${DIAGNOSTIC:-unknown}}"; fi; }
export FAIL_STEP= DIAGNOSTIC= FAIL_VARIANT=
# A silent nonzero exit must fail even when there is no diff or diagnostic.
for step in fix modernize resolve packages; do
    export FAIL_STEP=$step
    reject modernize-check
done
export FAIL_STEP=
for diagnostic in fix modernize; do
    export DIAGNOSTIC=$diagnostic
    reject modernize-check
done
export DIAGNOSTIC=download
run modernize-check || fail 'successful download chatter was rejected'
export DIAGNOSTIC=
run modernize-check || fail 'clean modernize check failed'

: > "$fixture/calls"
for target in vet-check staticcheck-check govulncheck-check; do
    run "$target" || fail "$target rejected a clean fixture"
done
for call in 'go vet ./fixture' 'go vet -tags trading ./fixture' \
    'staticcheck -f binary ./fixture' 'staticcheck -f binary -tags trading ./fixture' \
    'govulncheck ./fixture' 'govulncheck -tags trading ./fixture' \
    'govulncheck -C tools -tags=tools -scan=module' 'govulncheck ./...'; do
    grep -Fxq "$call" "$fixture/calls" || fail "missing build coverage: $call"
done
[ "$(grep -Fxc 'packages -tags trading ./...' "$fixture/calls")" -eq 3 ] || fail 'trading package discovery omitted part of the product'
grep -q '^staticcheck -merge ' "$fixture/calls" || fail 'staticcheck results were not merged'
export FAIL_STEP=merge
reject staticcheck-check
export FAIL_STEP=
# An earlier clean scan cannot skip checks of later source/build variants.
run govulncheck-check || fail 'repeat scan failed'
[ "$(grep -Fxc 'govulncheck -tags trading ./fixture' "$fixture/calls")" -eq 2 ] || fail 'repeat trading scan was skipped'
for step in staticcheck govulncheck; do
    for variant in default trading; do
        export FAIL_STEP=$step FAIL_VARIANT=$variant
        reject "$step-check"
    done
done
export FAIL_VARIANT=
for target in vet-check staticcheck-check govulncheck-check; do
    export FAIL_STEP=packages
    reject "$target"
done
for target in staticcheck-check govulncheck-check; do
    export FAIL_STEP=resolve
    reject "$target"
done
# Staticcheck caches contain source paths. Shared cross-worktree entries must
# not prevent the two product variants from merging their used-symbol sets.
export FAIL_STEP= STATICCHECK_CACHE="$fixture/shared-cache"
: > "$fixture/staticcheck-caches"
run staticcheck-check || fail 'isolated staticcheck run failed'
run staticcheck-check || fail 'second isolated staticcheck run failed'
caches=()
while IFS= read -r cache; do caches+=("$cache"); done < "$fixture/staticcheck-caches"
[ "${#caches[@]}" -eq 6 ] || fail 'missing staticcheck cache witnesses'
[ "${caches[0]}" != "$STATICCHECK_CACHE" ] || fail 'staticcheck reused the ambient cross-worktree cache'
[ "${caches[0]}" = "${caches[1]}" ] && [ "${caches[1]}" = "${caches[2]}" ] || fail 'build variants did not share the isolated cache'
[ "${caches[3]}" = "${caches[4]}" ] && [ "${caches[4]}" = "${caches[5]}" ] || fail 'second build variants did not share the isolated cache'
[ "${caches[0]}" != "${caches[3]}" ] || fail 'separate runs reused a source-path cache'
[ ! -e "${caches[0]}" ] && [ ! -e "${caches[3]}" ] || fail 'temporary analyzer caches were retained'
export FAIL_STEP=vet
reject vet-check
printf 'go-analysis test: OK (silent failures, diagnostics, both builds, repeat scans, isolated source-path caches)\n'
