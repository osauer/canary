#!/bin/sh
# Keep development builds from exhausting the volume that holds live audit data.
# Below the 8 GiB floor the Go build cache is trimmed, least recently used
# entries first, until every build filesystem is back above it; the gate never
# refuses for space (owner instruction 2026-10-07 09:49 CEST: "trim
# automatically, do not refuse"). Go refreshes an entry's mtime at most once an
# hour while it is used, so the last step keeps everything used in the last two
# hours and a build running in another worktree loses nothing it needs; a
# trimmed entry is recompiled on its next use.
#
# What still bounds the risk: the cache is the volume's dominant consumer and
# -trimpath (Makefile GOFLAGS) shares its entries across worktrees. If trimming
# cannot reach the floor, the shortfall is not the cache's: the gate proceeds
# with a warning naming it, and a build that then fills the volume can make the
# daemon's state writes fail until space is freed.
set -eu
export LC_ALL=C

[ "$#" -eq 0 ] || { echo 'check-build-space: no arguments accepted' >&2; exit 2; }
minimum_kib=8388608 # 8 GiB
trim_hours='24 12 6 2'
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

fail() { printf 'check-build-space: %s\n' "$*" >&2; exit 1; }
note() { printf 'check-build-space: %s\n' "$*" >&2; }

# available_kib prints the KiB available on the filesystem holding path.
available_kib() {
    label=$1
    path=$2
    case "$path" in /*) ;; *) fail "cannot inspect $label: expected an absolute path" ;; esac
    # Go may not have created its cache or temporary directory yet. Inspect its
    # closest existing parent without creating anything or following a bad link.
    existing=$path
    while [ ! -e "$existing" ] && [ ! -L "$existing" ]; do
        existing=$(dirname -- "$existing")
    done
    report=$(df -Pk "$existing" 2>/dev/null) || fail "cannot inspect $label filesystem"
    # POSIX -P fixes the numeric columns. Locate the capacity column rather
    # than counting fields: filesystem and mount names can contain spaces.
    available=$(printf '%s\n' "$report" | awk '
        NR > 1 { for (i=4; i<=NF; i++)
            if ($i ~ /^[0-9]+%$/ && $(i-1) ~ /^[0-9]+$/ &&
                $(i-2) ~ /^[0-9]+$/ && $(i-3) ~ /^[0-9]+$/) {
                print $(i-1); exit
            }
        }')
    case "$available" in ''|*[!0-9]*) fail "cannot parse $label filesystem capacity" ;; esac
    printf '%s\n' "$available"
}

gib() { awk -v free="$1" 'BEGIN { printf "%.1f", free/1048576 }'; }

# short names the first build filesystem below the floor, with its free GiB,
# or prints nothing when every one is above it.
short() {
    while IFS='|' read -r label path; do
        [ -n "$label" ] || continue
        free=$(available_kib "$label" "$path")
        if ! awk -v free="$free" -v floor="$minimum_kib" 'BEGIN { exit !(free >= floor) }'; then
            printf '%s has %s GiB available\n' "$label" "$(gib "$free")"
            return
        fi
    done <<EOF
repository|$root
temporary|${GOTMPDIR:-${TMPDIR:-/tmp}}
${build_cache:+Go build cache|$build_cache}
${go_temporary:+Go temporary|$go_temporary}
EOF
}

# trim deletes the Go build-cache entries untouched for more than hours.
# Only a directory Go itself marked as its cache is touched, and only the
# entry files inside its two-hex-digit subdirectories.
trim() {
    hours=$1
    [ -n "$build_cache" ] && [ -f "$build_cache/README" ] &&
        grep -q 'cached build artifacts from the Go build system' "$build_cache/README" || return 1
    find "$build_cache" -mindepth 2 -maxdepth 2 -type f -path "$build_cache/[0-9a-f][0-9a-f]/*" \
        -mmin "+$((hours * 60))" -exec rm -f {} + 2>/dev/null || true
}

build_cache=''
go_temporary=''
# Known volumes first: the common case needs no Go invocation.
lack=$(short)
[ -n "$lack" ] || {
    environment=$(go env GOCACHE GOTMPDIR) || fail 'cannot resolve Go cache and temporary directories'
    build_cache=$(printf '%s\n' "$environment" | sed -n '1p')
    go_temporary=$(printf '%s\n' "$environment" | sed -n '2p')
    go_temporary=${go_temporary:-${TMPDIR:-/tmp}}
    lack=$(short)
    [ -n "$lack" ] || exit 0
}
if [ -z "$build_cache" ]; then
    environment=$(go env GOCACHE GOTMPDIR) || fail 'cannot resolve Go cache and temporary directories'
    build_cache=$(printf '%s\n' "$environment" | sed -n '1p')
    go_temporary=$(printf '%s\n' "$environment" | sed -n '2p')
    go_temporary=${go_temporary:-${TMPDIR:-/tmp}}
fi
case "$build_cache" in /*) ;; *) fail "cannot inspect Go build cache: expected an absolute path" ;; esac

note "$lack, below the 8 GiB floor; trimming the Go build cache"
for hours in $trim_hours; do
    trim "$hours" || { note "no Go build cache to trim at $build_cache"; break; }
    lack=$(short)
    if [ -z "$lack" ]; then
        note "trimmed Go build-cache entries untouched for ${hours} hours; every build filesystem is above 8 GiB again"
        exit 0
    fi
done
note "$lack after trimming; proceeding anyway, so this gate may fill the volume and the daemon's state writes can fail until space is freed"
