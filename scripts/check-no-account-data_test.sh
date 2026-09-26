#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ibkr-account-data-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

# Private holdings data comes only from the synthetic fixtures below, never
# from this machine's daemon store or cache.
export XDG_STATE_HOME="$test_root/state" XDG_CACHE_HOME="$test_root/cache"
out="$test_root/out"
gate() { (cd "$repo" && ./scripts/check-no-account-data.sh) >"$out" 2>&1; }
fail() {
	echo "check-no-account-data test: $*" >&2
	sed 's/^/  | /' "$out" >&2
	exit 1
}
expect() { grep -Fq -- "$1" "$out" || fail "output lacks: $1"; }
withheld() { ! grep -qw -- "$1" "$out" || fail "failure output disclosed held ticker $1"; }
stage() {
	printf '%s\n' "$2" >"$repo/$1"
	git -C "$repo" add "$1"
}

repo="$test_root/repo"
mkdir -p "$repo/scripts"
cp "$repo_root/scripts/check-no-account-data.sh" "$repo/scripts/"
git init --quiet "$repo"
git -C "$repo" config user.name "Account Data Test"
git -C "$repo" config user.email "account-data-test@example.invalid"
printf '%s\n' 'safe fixture DU1234567' > "$repo/fixture.txt"
git -C "$repo" add .
gate || fail "safe fixture was rejected"

probe_lower="du987""6543"
printf '%s\n' "$probe_lower" > "$repo/fixture.txt"
git -C "$repo" add fixture.txt
if output="$(cd "$repo" && ./scripts/check-no-account-data.sh 2>&1)"; then
	echo "check-no-account-data test: lowercase non-placeholder ID was accepted" >&2
	exit 1
fi
if printf '%s\n' "$output" | grep -Fqi "$probe_lower"; then
	echo "check-no-account-data test: failure output disclosed the matched ID" >&2
	exit 1
fi

probe_upper="$(printf '%s' "$probe_lower" | tr '[:lower:]' '[:upper:]')"
printf 'fixture\000%s\000data\n' "$probe_upper" > "$repo/fixture.bin"
git -C "$repo" add fixture.bin
stage fixture.txt 'safe fixture DU1234567'
! gate || fail "binary non-placeholder ID was accepted"
expect "fixture.bin contains 1 non-placeholder IBKR account ID occurrence(s)"
withheld "$probe_upper"
git -C "$repo" rm --quiet --cached fixture.bin

# Holdings. No store and no cache is a contributor machine: skip, loudly.
gate || fail "tree without private holdings data was rejected"
expect "holdings check SKIPPED"

# A synthetic denylist injected through the private cache. Matching is
# whole-token and case-sensitive; a single letter and Canary's own market
# instruments are counted, not checked.
mkdir -p "$XDG_CACHE_HOME/ibkr"
printf '%s\n' '# as of 2026-01-02' ZQXW QZ Q SPY > "$XDG_CACHE_HOME/ibkr/holdings-denylist"
stage fixture.txt 'zqxw ZQXWV AZQXW ZQXW_2 qz QZX; Q and A; SPY puts'
gate || fail "a non-token, lowercase, single-letter or instrument use was flagged"
expect "holdings checked against the private cache (no daemon store at"
expect "2 held ticker(s) not checked"

stage a.go 'Symbol: "ZQXW"'
stage b.sh 'canary proposals reduce QZ --percent 25 --json'
! gate || fail "synthetic holdings in code were accepted"
expect "a.go names a current holding on line(s) 1"
expect "b.sh names a current holding on line(s) 1"
withheld ZQXW
withheld QZ
git -C "$repo" rm --quiet --cached a.go b.sh

# Public reference lists, binary blobs and released CHANGELOG sections are
# out of scope; the section being prepared above them is not.
mkdir -p "$repo/internal/breadth/spx"
stage internal/breadth/spx/members_data.go '{"ZQXW", "QZ"}'
printf 'blob\000ZQXW\000\n' > "$repo/holding.bin"
git -C "$repo" add holding.bin
stage CHANGELOG.md '# Changelog

## v1.0.0 — 2026-01-01 00:00 CET
- Held ZQXW.'
git -C "$repo" commit --quiet -m base
git -C "$repo" tag v1.0.0
gate || fail "a public list, binary blob or released changelog section was flagged"
stage CHANGELOG.md '# Changelog

## v1.1.0 — 2026-02-01 00:00 CET
- Sold QZ.

## v1.0.0 — 2026-01-01 00:00 CET
- Held ZQXW.'
! gate || fail "an unreleased changelog section naming a holding was accepted"
expect "CHANGELOG.md names a current holding on line(s) 4"
git -C "$repo" checkout --quiet HEAD -- CHANGELOG.md

# The daemon store's latest Flex position report per account is the
# denylist, adjusted option roots and their underlyings included; closed
# positions and trades are not holdings.
if ! command -v sqlite3 >/dev/null 2>&1; then
	echo "check-no-account-data test: OK (daemon-store cases skipped: sqlite3 not installed)"
	exit 0
fi
db="$XDG_STATE_HOME/ibkr/daemon.db"
cache="$XDG_CACHE_HOME/ibkr/holdings-denylist"
store() {
	rm -f "$db"
	mkdir -p "${db%/*}"
	sqlite3 "$db" "CREATE TABLE statement_files (file_key TEXT);
		CREATE TABLE statement_records (record_kind TEXT, account_key TEXT, raw_json TEXT); $1"
}
pos() { printf "INSERT INTO statement_records VALUES ('%s', '%s', '%s');" "$1" "$2" "$3"; }
store "INSERT INTO statement_files VALUES ('flex');
	$(pos position a '{"Symbol":"ZQXOLD","ReportDate":"2026-01-01T00:00:00Z"}')
	$(pos position a '{"Symbol":"ZQXNEW","ReportDate":"2026-02-02T00:00:00Z"}')
	$(pos position a '{"Symbol":"ZQXOPT1 260918C00005000","UnderlyingSymbol":"ZQXOPT","ReportDate":"2026-02-02T00:00:00Z"}')
	$(pos position b '{"Symbol":"ZQXB","ReportDate":"2026-01-15T00:00:00Z"}')
	$(pos trade a '{"Symbol":"ZQXTRD","ReportDate":"2026-02-02T00:00:00Z"}')"
rm -f "$cache"
stage fixture.txt 'closed ZQXOLD, traded ZQXTRD'
gate || fail "a closed position or a trade was treated as a current holding"
expect "holdings checked against the daemon store, positions as of 2026-02-02"
[ "$(grep -v '^#' "$cache" | tr '\n' ' ')" = "ZQXB ZQXNEW ZQXOPT ZQXOPT1 " ] || fail "cache does not hold the latest report per account"
[ -n "$(find "$cache" -perm 600)" ] || fail "cache is not private (0600)"
stage fixture.txt 'close the ZQXOPT calls'
! gate || fail "an option underlying in the latest report was accepted"
withheld ZQXOPT

# A failed holdings grep fails the check rather than passing it.
mkdir -p "$test_root/bin"
printf '#!/bin/sh\ncase " $* " in *" grep --cached -I "*) exit 128 ;; esac\nexec %s "$@"\n' "$(command -v git)" >"$test_root/bin/git"
chmod +x "$test_root/bin/git"
stage fixture.txt 'safe fixture DU1234567'
! (cd "$repo" && PATH="$test_root/bin:$PATH" ./scripts/check-no-account-data.sh) >"$out" 2>&1 || fail "a failed holdings grep passed"
expect "git grep failed"

# An unreadable store falls back to the cache; without one it fails.
stage fixture.txt 'safe fixture DU1234567'
printf 'not a database\n' >"$db"
gate || fail "an unreadable store with a cache was rejected"
expect "holdings checked against the private cache (cannot read the daemon store at"
rm -f "$cache"
! gate || fail "an unreadable store without a cache skipped the check"
expect "never skips the holdings check"

# Statements without a readable ticker mean the projection changed: fail,
# even with a cache, rather than skip.
printf '%s\n' '# as of 2026-01-02' ZQXW > "$cache"
store "INSERT INTO statement_files VALUES ('flex'); $(pos position a '{"Ticker":"ZQXNEW","ReportDate":"2026-02-02T00:00:00Z"}')"
! gate || fail "a store whose position report yields no ticker was accepted"
expect "no ticker was read"

# A store without Flex statements holds no private holdings data.
rm -f "$cache"
store ""
gate || fail "a store without Flex statements was rejected"
expect "holdings check SKIPPED — no private holdings data (the daemon store holds no Flex statements"

echo "check-no-account-data test: OK"
