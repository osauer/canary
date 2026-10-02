#!/bin/sh
# Keep development builds from exhausting the volume that holds live audit data.
# This is an admission floor, not a build-size prediction or cache manager.
set -eu
export LC_ALL=C

[ "$#" -eq 0 ] || { echo 'check-build-space: no arguments accepted' >&2; exit 2; }
minimum_kib=8388608 # 8 GiB; no bypass switch.
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

fail() { printf 'check-build-space: %s\n' "$*" >&2; exit 1; }
check_path() {
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
    if ! awk -v free="$available" -v floor="$minimum_kib" 'BEGIN { exit !(free >= floor) }'; then
        gib=$(awk -v free="$available" 'BEGIN { printf "%.1f", free/1048576 }')
        fail "$label has ${gib} GiB available; at least 8 GiB is required. Stop build workers, free build-cache space deliberately, then retry. No files were removed."
    fi
}

# Inspect known volumes before asking Go, which can bootstrap a toolchain.
check_path repository "$root"
check_path temporary "${GOTMPDIR:-${TMPDIR:-/tmp}}"
environment=$(go env GOCACHE GOTMPDIR) || fail 'cannot resolve Go cache and temporary directories'
build_cache=$(printf '%s\n' "$environment" | sed -n '1p')
go_temporary=$(printf '%s\n' "$environment" | sed -n '2p')
check_path 'Go build cache' "$build_cache"
check_path 'Go temporary' "${go_temporary:-${TMPDIR:-/tmp}}"
