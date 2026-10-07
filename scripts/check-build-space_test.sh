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
printf '%s|GOFLAGS=%s\n' "$*" "${GOFLAGS:-}" >> "$TEST_GO_CALLS"
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
    "$TEST_CACHE_PARENT"|"$TEST_CACHE"*) free=$TEST_CACHE_FREE ;;
    "$TEST_TMP_PARENT") free=$TEST_TMP_FREE ;;
esac
# Trimming frees space: once the old cache entry is gone, the volume is roomy.
if [ -n "${TEST_TRIM_FREES:-}" ] && [ ! -e "$TEST_TRIM_FREES" ]; then free=20971520; fi
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
pass() {
    run || { echo "check-build-space test: expected a pass $1" >&2; cat "$fixture/out" >&2; exit 1; }
    grep -F "$1" "$fixture/out" >/dev/null || { echo "check-build-space test: output lacks: $1" >&2; cat "$fixture/out" >&2; exit 1; }
}
refuse() {
    if run; then echo "check-build-space test: expected refusal $1" >&2; exit 1; fi
    grep -F "$1" "$fixture/out" >/dev/null
}
run
[ ! -s "$fixture/out" ] || { echo 'a roomy volume printed output' >&2; exit 1; }
grep -Fx "$TEST_CACHE_PARENT" "$TEST_DF_CALLS" >/dev/null
grep -Fx "$TEST_TMP_PARENT" "$TEST_DF_CALLS" >/dev/null
[ ! -e "$TEST_CACHE" ] && [ ! -e "$TEST_GO_TMP" ]
# Exact floor: at it passes silently; one KiB below with no cache to trim it
# warns and proceeds, never refuses.
(export TEST_CACHE_FREE=8388608; run; [ ! -s "$fixture/out" ])
(export TEST_CACHE_FREE=8388607; pass 'no Go build cache to trim'; pass 'proceeding anyway')
(export TEST_REPO_FREE=1024; pass 'repository has 0.0 GiB available')
(export TEST_TMP_FREE=1024; pass 'temporary has 0.0 GiB available')
# An unreadable filesystem or a malformed cache path is a fault, not space.
(export TEST_DF_MODE=malformed; refuse 'cannot parse repository')
(export TEST_DF_MODE=error; refuse 'cannot inspect repository')
(export TEST_CACHE=relative/cache; refuse 'expected an absolute path')
(export TEST_CACHE=off; refuse 'expected an absolute path')
[ ! -e "$TEST_GO_TMP" ]

# A real-shaped cache: Go's README, an entry from 2000 and one used now.
cache="$fixture/cache parent/go-build"
mkdir -p "$cache/ab" "$cache/cd"
printf 'This directory holds cached build artifacts from the Go build system.\n' > "$cache/README"
: > "$cache/ab/old-a"; : > "$cache/cd/new-d"; : > "$cache/trim.txt"
touch -t 200001010000 "$cache/ab/old-a" "$cache/trim.txt"
export TEST_CACHE="$cache"
# Trimming the oldest step frees enough: the old entry goes, the fresh one and
# Go's own files stay, and the gate passes.
(export TEST_CACHE_FREE=1024 TEST_TRIM_FREES="$cache/ab/old-a"; pass 'untouched for 24 hours'
 [ ! -e "$cache/ab/old-a" ] && [ -e "$cache/cd/new-d" ] && [ -e "$cache/README" ] && [ -e "$cache/trim.txt" ] ||
     { echo 'trim removed the wrong files' >&2; exit 1; })
# Nothing frees enough: every step runs, entries used in the last two hours
# survive, and the gate still proceeds with a warning.
: > "$cache/ab/old-b"; touch -t 200001010000 "$cache/ab/old-b"
(export TEST_CACHE_FREE=1024; pass 'proceeding anyway'
 [ ! -e "$cache/ab/old-b" ] && [ -e "$cache/cd/new-d" ] || { echo 'the full ladder removed a fresh entry' >&2; exit 1; })
# A directory Go did not mark as its cache is never touched.
rm "$cache/README"; : > "$cache/ab/old-c"; touch -t 200001010000 "$cache/ab/old-c"
(export TEST_CACHE_FREE=1024; pass 'no Go build cache to trim'
 [ -e "$cache/ab/old-c" ] || { echo 'trimmed a directory without the Go cache README' >&2; exit 1; })

# The Make entrypoints run the check first, with -trimpath in GOFLAGS, and a
# low volume no longer stops them before Go. The fixture has no sources, so
# the synthetic go fails right after; nothing touches runtime state.
export TEST_CACHE="$fixture/cache parent/new/cache"
for target in build check test test-pkg test-daemon-default test-daemon-trading; do
    : > "$TEST_GO_CALLS"
    (cd "$TEST_REPO" && TEST_REPO_FREE=1024 make "$target") > "$fixture/make-out" 2>&1 || true
    grep -F 'proceeding anyway' "$fixture/make-out" >/dev/null || { echo "Make $target skipped the space check" >&2; exit 1; }
    head -1 "$TEST_GO_CALLS" | grep -Fx 'env GOCACHE GOTMPDIR|GOFLAGS=-trimpath' >/dev/null ||
        { echo "Make $target did not run the check first with GOFLAGS=-trimpath" >&2; cat "$TEST_GO_CALLS" >&2; exit 1; }
done
printf '%s\n' 'check-build-space test: PASS'
