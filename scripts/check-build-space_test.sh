#!/bin/sh
# All capacity and Go responses are synthetic. No real cache or broker state.
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture=$(mktemp -d "${TMPDIR:-/tmp}/canary-build-space-test.XXXXXX")
fixture=$(CDPATH= cd -- "$fixture" && pwd)
trap 'rm -rf "$fixture"' EXIT HUP INT TERM
mkdir -p "$fixture/repo/scripts" "$fixture/repo/internal/daemon" "$fixture/bin" "$fixture/cache parent" "$fixture/temporary parent"
cp "$root/scripts/check-build-space.sh" "$fixture/repo/scripts/check-build-space.sh"
cp "$root/Makefile" "$fixture/repo/Makefile"
cat > "$fixture/bin/go" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$TEST_GO_CALLS"
[ "$*" = 'env GOCACHE GOTMPDIR' ] || exit 91
printf '%s\n' "$TEST_CACHE" "$TEST_GO_TMP"
EOF
cat > "$fixture/bin/df" <<'EOF'
#!/bin/sh
set -eu
[ "$1" = '-Pk' ] || exit 92
printf '%s\n' "$2" >> "$TEST_DF_CALLS"
[ "${TEST_DF_MODE:-}" != error ] || exit 93
if [ "${TEST_DF_MODE:-}" = malformed ]; then printf 'unreadable capacity\n'; exit; fi
free=20971520
case "$2" in
    "$TEST_REPO") free=$TEST_REPO_FREE ;;
    "$TEST_CACHE_PARENT") free=$TEST_CACHE_FREE ;;
    "$TEST_TMP_PARENT") free=$TEST_TMP_FREE ;;
esac
# A synthetic filesystem AND mount name contain spaces, as APFS names can.
printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\n'
printf '/dev/fixture disk 99999999 12345 %s 99%% /fixture mount with spaces\n' "$free"
EOF
chmod 755 "$fixture/bin/go" "$fixture/bin/df"
export PATH="$fixture/bin:$PATH"
export TEST_GO_CALLS="$fixture/go-calls" TEST_DF_CALLS="$fixture/df-calls"
export TEST_REPO="$fixture/repo" TEST_REPO_FREE=20971520
export TEST_CACHE_PARENT="$fixture/cache parent" TEST_CACHE="$fixture/cache parent/new/cache" TEST_CACHE_FREE=20971520
export TEST_TMP_PARENT="$fixture/temporary parent" TEST_GO_TMP="$fixture/temporary parent/new/tmp" TEST_TMP_FREE=20971520
export GOTMPDIR="$TEST_GO_TMP"
run() { : > "$TEST_GO_CALLS"; : > "$TEST_DF_CALLS"; "$fixture/repo/scripts/check-build-space.sh" > "$fixture/out" 2>&1; }
refuse() {
    if run; then echo "check-build-space test: expected refusal $1" >&2; exit 1; fi
    grep -F "$1" "$fixture/out" >/dev/null
}
run
grep -Fx "$TEST_CACHE_PARENT" "$TEST_DF_CALLS" >/dev/null
grep -Fx "$TEST_TMP_PARENT" "$TEST_DF_CALLS" >/dev/null
[ ! -e "$TEST_CACHE" ] && [ ! -e "$TEST_GO_TMP" ]
# Exact admission boundary, one KiB below it, independent filesystem failure.
(export TEST_CACHE_FREE=8388608; run)
(export TEST_CACHE_FREE=8388607; refuse 'Go build cache has')
(export TEST_REPO_FREE=1024; refuse 'repository has'
 [ ! -s "$TEST_GO_CALLS" ] || { echo 'low repository space invoked Go' >&2; exit 1; })
(export TEST_TMP_FREE=1024; refuse 'temporary has'
 [ ! -s "$TEST_GO_CALLS" ] || { echo 'low temporary space invoked Go' >&2; exit 1; })
(export TEST_DF_MODE=malformed; refuse 'cannot parse repository')
(export TEST_DF_MODE=error; refuse 'cannot inspect repository')
(export TEST_CACHE=relative/cache; refuse 'expected an absolute path')
# User-configured Go temporary directory may be absent; its parent is checked.
(export TEST_CACHE=off; refuse 'expected an absolute path')
[ ! -e "$TEST_CACHE" ] && [ ! -e "$TEST_GO_TMP" ]
# Prove the actual Make entrypoints fail before invoking a compiler or nested
# gate. The fixture has no source or service scripts and cannot touch runtime.
for target in build check test test-pkg test-daemon-default test-daemon-trading; do
    : > "$TEST_GO_CALLS"
    if (cd "$TEST_REPO" && TEST_REPO_FREE=1024 make "$target") > "$fixture/make-out" 2>&1; then
        echo "check-build-space test: Make $target ignored capacity refusal" >&2; exit 1
    fi
    grep -F 'repository has' "$fixture/make-out" >/dev/null
    [ ! -s "$TEST_GO_CALLS" ] || { echo "Make $target invoked Go after refusal" >&2; exit 1; }
done
[ ! -e "$TEST_REPO/bin" ]
printf '%s\n' 'check-build-space test: PASS'
