#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ibkr-account-data-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

# Private holdings and money data come only from the synthetic fixtures
# below, never from this machine's daemon store or cache.
export XDG_STATE_HOME="$test_root/state" XDG_CACHE_HOME="$test_root/cache"
out="$test_root/out"
gate() { (cd "$repo" && ./scripts/check-no-account-data.sh) >"$out" 2>&1; }
fail() {
	echo "check-no-account-data test: $*" >&2
	sed 's/^/  | /' "$out" >&2
	exit 1
}
expect() { grep -Fq -- "$1" "$out" || fail "output lacks: $1"; }
withheld() { ! grep -qw -- "$1" "$out" || fail "failure output disclosed $1"; }
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

# A tracked executable over the 64 KiB size gate fails.
{ printf '\317\372\355\376'; head -c 70000 /dev/zero; } >"$repo/tool"
git -C "$repo" add tool
! gate || fail "a tracked Mach-O executable was accepted"
expect "compiled executable(s) tracked"
git -C "$repo" rm --quiet --cached tool

# Holdings. No store and no cache is a contributor machine: skip, loudly;
# the money check skips silently.
gate || fail "tree without private holdings data was rejected"
expect "holdings check SKIPPED"
! grep -q money "$out" || fail "the money check spoke without private data"

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

# The pre-commit hook runs the gate on exactly what is committed.
cp "$repo_root/scripts/pre-commit" "$repo/scripts/"
git -C "$repo" config core.hooksPath .git/hooks
mkdir -p "$repo/.git/hooks"
ln -s ../../scripts/pre-commit "$repo/.git/hooks/pre-commit"
stage fixture.txt "commit $probe_upper"
! git -C "$repo" commit --quiet -m leak >"$out" 2>&1 || fail "the pre-commit hook let a non-placeholder ID through"
expect "the account-data gate blocked this commit"
withheld "$probe_upper"
stage fixture.txt 'safe fixture DU1234567'
git -C "$repo" commit --quiet -m clean >"$out" 2>&1 || fail "the pre-commit hook blocked a clean commit"
rm "$repo/.git/hooks/pre-commit"

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
		CREATE TABLE statement_records (record_kind TEXT, account_key TEXT, raw_json TEXT);
		CREATE TABLE state_documents (kind TEXT, document_json TEXT); $1"
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

# Money figures from the daemon store: the latest live risk-capital NLV per
# account and the live proposal snapshot's cash rows. The synthetic book:
# NLV 618,034.27; cash 314,159.26 (settled 298,765.43), -271,828.18
# (borrowed) and 7,153.86; two balances below four significant digits. A
# paper book and an older document of the same account are not the live
# book; a store without a live book says so.
doc() { printf "INSERT INTO state_documents VALUES ('%s', '%s');" "$1" "$2"; }
nlv() { printf '{"state":{"account_id":"%s","account_mode":"%s","last_equity_base":%s,"last_equity_as_of":"%s"}}' "$@"; }
book="$(doc risk_capital "$(nlv A live 618034.27 2026-02-02T10:00:00Z)")
	$(doc risk_capital "$(nlv A live 123987.65 2026-01-15T10:00:00Z)")
	$(doc risk_capital "$(nlv P paper 864197.53 2026-02-02T10:00:00Z)")"
cash='{"account_mode":"live","cash_sweep":{"currencies":[{"currency":"EUR","cash":314159.26,"settled_cash":298765.43},
	{"currency":"USD","cash":-271828.18},{"currency":"GBP","cash":7153.86},{"currency":"CHF","cash":950.25},{"currency":"HKD","cash":40000}]}}'
gap='{"account_mode":"live","cash_sweep":{"currencies":[{"currency":"EUR","state":"cash_unavailable"}]}}'
no_nlv='{"state":{"account_id":"A","account_mode":"live","last_equity_as_of":"2026-02-02T10:00:00Z"}}'
store "$(doc risk_capital "$(nlv P paper 864197.53 2026-02-02T10:00:00Z)")"
stage fixture.txt 'safe fixture DU1234567'
gate || fail "a store without a live book was rejected"
expect "money check SKIPPED — the daemon store holds no live net liquidation value"
store "$book $(doc trade_proposals_current "$cash")"
gate || fail "the live book was rejected on a clean tree"
expect "money figures checked against the daemon store, NLV as of 2026-02-02"

# Each row is staged alone; a flagged row fails without echoing a figure.
# Without origin/main every file is unpublished, so rounded spellings
# within 2% apply (605,700 to 630,300 for the NLV). Round numbers, a figure
# inside a longer number, colours and hashes pass.
while IFS= read -r row; do
	want=${row%%: *} text=${row#*: }
	stage money.md "$text"
	if gate </dev/null; then got=pass; else got=flag; fi
	[ "$got" = "$want" ] || fail "money row wanted $want, got $got: $text"
	[ "$want" = flag ] || continue
	expect "money.md names a money figure of the live book on line(s) 1"
	for digits in $(printf '%s\n' "$text" | grep -oE '[0-9]{3,}'); do withheld "$digits"; done
done <<'EOF'
flag: NLV 618,034.27 EUR
flag: 618,034.27 EUR is the book
flag: 618k book
flag: NLV 618.034,27 EUR
flag: "net_liquidation": 618034.27,
flag: {"equity":[0,618034.27]}
flag: 2025-12-31,618034.27,EUR
flag: nlv := 618_034.27
flag: NetLiquidation: 618_000,
flag: a book of €618,034
flag: NLV 618034 EUR
flag: about 618,000 EUR
flag: about 618.000 EUR
flag: roughly 618K
flag: func TestSweepBookOf618k(t *testing.T) {
flag: after a 2% fall, 605,700
flag: after a 2% rise, 630,300
flag: a 629.8k book
flag: EUR cash 314,159.26
flag: | EUR | 314.159,26 |
flag: Cash: new(314159.3),
flag: EUR cash of about 314,200
flag: EUR 314.2k
flag: settled EUR 298,765.43
flag: USD is borrowed: −271,828.18 USD
flag: "USD": -271800,
flag: about 272k USD
flag: GBP 7,153.86
flag: GBP 7.154
pass: an older 123,987 and a paper 864,197 are not the live book
pass: 1,618,034 is another number
pass: a ratio of 0.618034
pass: 618,034,270 shares
pass: colour #618034
pass: digest a618034f
pass: 618km away
pass: after a 2% fall, 605,600
pass: after a 2% rise, 630,400
pass: about 620,000 or 630k
pass: build 7153 passed
pass: a constant of 7.1534
pass: CHF 950.25 and HKD 40,000
EOF

# Published files are judged on whole units only: a rounded figure already
# on origin/main passes until its file changes, a whole one never does.
stage sized.md 'sized for 630,300'
git -C "$repo" commit --quiet -m sized
git -C "$repo" update-ref refs/remotes/origin/main HEAD
gate || fail "a published rounded figure in an unchanged file was flagged"
stage sized.md 'sized for 630,300 again'
! gate || fail "a rounded figure in a changed file was accepted"
expect "sized.md names a money figure of the live book on line(s) 1"
git -C "$repo" checkout --quiet HEAD -- sized.md
stage whole.md 'NLV 618,034'
git -C "$repo" commit --quiet -m whole
git -C "$repo" update-ref refs/remotes/origin/main HEAD
! gate || fail "a published whole figure was accepted"
expect "whole.md names a money figure of the live book on line(s) 1"
git -C "$repo" rm --quiet sized.md whole.md
git -C "$repo" commit --quiet -m unpublish
git -C "$repo" update-ref -d refs/remotes/origin/main

# Released CHANGELOG sections are out of scope here too.
stage CHANGELOG.md '# Changelog

## v1.1.0 — 2026-02-01 00:00 CET
- Sized against a 618k book.

## v1.0.0 — 2026-01-01 00:00 CET
- Held ZQXW at NLV 618,034.'
! gate || fail "an unreleased changelog section naming a money figure was accepted"
grep -qx 'check-no-account-data: CHANGELOG.md names a money figure of the live book on line(s) 4' "$out" ||
	fail "only the unreleased changelog line should be named"
git -C "$repo" checkout --quiet HEAD -- CHANGELOG.md

# A failed money grep fails the check; a live account without an NLV means
# the query no longer matches the store: fail rather than skip. A snapshot
# without cash (sweep off, ledger catching up after a fill) leaves cash
# unchecked and says so.
! (cd "$repo" && PATH="$test_root/bin:$PATH" ./scripts/check-no-account-data.sh) >"$out" 2>&1 || fail "a failed money grep passed"
expect "git grep failed; the money check did not run"
store "$(doc risk_capital "$no_nlv")"
! gate || fail "a live book without a readable NLV was accepted"
expect "fix the money query here"
store "$book $(doc trade_proposals_current "$gap")"
gate || fail "a cash ledger gap failed the gate"
expect "cash balances not checked"

# An unreadable store falls back to the holdings cache; the money check has
# no cache and fails, as does the holdings check without its cache.
printf 'not a database\n' >"$db"
! gate || fail "an unreadable store passed the money check"
expect "holdings checked against the private cache (cannot read the daemon store at"
expect "cannot read money figures from the daemon store"
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
