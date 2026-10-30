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
expect "holdings checked against the daemon's holdings file, positions as of 2026-01-02"
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

# Commit messages are publication content too. Use a separate synthetic
# repository so published-message controls cannot affect the file fixtures.
index_repo="$repo"
message_repo="$test_root/messages"
mkdir -p "$message_repo/scripts"
cp "$repo_root/scripts/check-no-account-data.sh" "$message_repo/scripts/"
git init --quiet "$message_repo"
git -C "$message_repo" config user.name "Account Data Test"
git -C "$message_repo" config user.email "account-data-test@example.invalid"
git -C "$message_repo" config core.hooksPath .git/hooks
printf 'safe fixture\n' > "$message_repo/fixture.txt"
git -C "$message_repo" add .
git -C "$message_repo" commit --quiet -m 'Synthetic published base'
git -C "$message_repo" update-ref refs/remotes/origin/main HEAD
repo="$message_repo"
message_commit() {
	git -C "$message_repo" commit --quiet --allow-empty -m "$1"
	message_sha=$(git -C "$message_repo" rev-parse HEAD)
}
message_commit 'Synthetic subject

The current holding is ZQXW.'
holding_sha="$message_sha"
message_commit 'Later safe subject'
! gate || fail "an older unpublished commit body naming a holding was accepted"
expect "commit $holding_sha names a current holding on line(s) 3"
withheld ZQXW
# Already published commits are not newly disclosed by this push.
git -C "$message_repo" update-ref refs/remotes/origin/main HEAD
gate || fail "a published commit message was rescanned as outgoing"
message_commit "Synthetic subject

Account $probe_lower"
! gate || fail "an unpublished commit body containing an account ID was accepted"
expect "commit $message_sha contains a non-placeholder IBKR account ID on line 3"
withheld "$probe_lower"
git -C "$message_repo" update-ref refs/remotes/origin/main HEAD
message_commit 'SPY, DU1234567, zqxw and ZQXWV are allowed examples'
gate || fail "commit messages did not preserve the existing ID and holdings allowlists"
# Enumeration or message reads failing cannot silently drop this scope.
mkdir -p "$test_root/message-bin"
for failed_command in rev-list show; do
	printf '#!/bin/sh\ncase "$1" in %s) exit 128 ;; esac\nexec "%s" "$@"\n' "$failed_command" "$(command -v git)" > "$test_root/message-bin/git"
	chmod +x "$test_root/message-bin/git"
	! (cd "$repo" && PATH="$test_root/message-bin:$PATH" ./scripts/check-no-account-data.sh) > "$out" 2>&1 ||
		fail "failed outgoing $failed_command read passed the gate"
	expect "the message check did not run"
	withheld ZQXW
	withheld "$probe_lower"
done
repo="$index_repo"

# The gate never opens daemon.db: only the daemon may, because an outside
# SQLite opener resets a live daemon's shared memory if the daemon's lock on
# it is lost. A sqlite3 first on PATH records any call.
db="$XDG_STATE_HOME/ibkr/daemon.db"
cache="$XDG_CACHE_HOME/ibkr/holdings-denylist"
money="$XDG_CACHE_HOME/ibkr/money-denylist"
mkdir -p "${db%/*}" "$test_root/sqlite-bin"
printf 'not a database\n' >"$db"
printf '#!/bin/sh\necho "$*" >>"%s"\nexit 1\n' "$test_root/sqlite-calls" >"$test_root/sqlite-bin/sqlite3"
chmod +x "$test_root/sqlite-bin/sqlite3"
export PATH="$test_root/sqlite-bin:$PATH"

# A machine with a daemon store but no holdings file never skips; without
# the money file the money check says so loudly until the daemon writes it.
rm -f "$cache" "$money"
stage fixture.txt 'safe fixture DU1234567'
! gate || fail "a daemon store without the holdings file skipped the check"
expect "the daemon has not written"
expect "never skips the holdings check"
expect "money check SKIPPED — the daemon has not written"

# The daemon's holdings file is the latest Flex position report per account,
# adjusted option roots and their underlyings included
# (internal/daemon/account_data_files_test.go builds it).
printf '%s\n' '# as of 2026-02-02' ZQXB ZQXNEW ZQXOPT ZQXOPT1 >"$cache"
stage fixture.txt 'closed ZQXOLD, traded ZQXTRD'
gate || fail "a ticker outside the holdings file was flagged"
expect "holdings checked against the daemon's holdings file, positions as of 2026-02-02"
stage fixture.txt 'close the ZQXOPT calls'
! gate || fail "an option underlying in the holdings file was accepted"
withheld ZQXOPT

# A failed holdings grep fails the check rather than passing it.
mkdir -p "$test_root/bin"
printf '#!/bin/sh\ncase " $* " in *" grep --cached -I "*) exit 128 ;; esac\nexec %s "$@"\n' "$(command -v git)" >"$test_root/bin/git"
chmod +x "$test_root/bin/git"
stage fixture.txt 'safe fixture DU1234567'
! (cd "$repo" && PATH="$test_root/bin:$PATH" ./scripts/check-no-account-data.sh) >"$out" 2>&1 || fail "a failed holdings grep passed"
expect "git grep failed"

# A dated report without a ticker means the daemon's file changed shape:
# fail rather than skip. A daemon without a Flex position report has no
# private holdings data.
printf '%s\n' '# as of 2026-02-02' >"$cache"
! gate || fail "a holdings file naming no ticker was accepted"
expect "names no ticker"
printf '%s\n' '# no Flex position report' >"$cache"
gate || fail "a daemon without a Flex position report was rejected"
expect "holdings check SKIPPED — no private holdings data (the daemon holds no Flex position report)"
printf '%s\n' '# as of 2026-02-02' ZQXB ZQXNEW ZQXOPT ZQXOPT1 >"$cache"

# Money figures from the daemon's money file: the live account's NLV and the
# live proposal snapshot's cash rows (a paper book or an older document never
# reaches it). The synthetic book: NLV 618,034.27; cash 314,159.26 (settled
# 298,765.43), -271,828.18 (borrowed) and 7,153.86; two balances below four
# significant digits.
book='nlv|618034.27|2026-02-02'
cash='cash|314159.26|
cash|298765.43|
cash|-271828.18|
cash|7153.86|
cash|950.25|
cash|40000|'
printf '%s\n' 'cash|314159.26|' >"$money"
gate || fail "a money file without a live book was rejected"
expect "money check SKIPPED — the daemon's money file holds no live net liquidation value"
printf '%s\n%s\n' "$book" "$cash" >"$money"
gate || fail "the live book was rejected on a clean tree"
expect "money figures checked against the daemon's money file, NLV as of 2026-02-02"

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
# the daemon's file changed shape: fail rather than skip. A snapshot without
# cash (sweep off, ledger catching up after a fill) leaves cash unchecked
# and says so. A money file the gate cannot read fails it.
! (cd "$repo" && PATH="$test_root/bin:$PATH" ./scripts/check-no-account-data.sh) >"$out" 2>&1 || fail "a failed money grep passed"
expect "git grep failed; the money check did not run"
printf '%s\n' 'nlv|missing|2026-02-02' >"$money"
! gate || fail "a live book without a readable NLV was accepted"
expect "has no net liquidation value"
printf '%s\n' "$book" >"$money"
gate || fail "a cash ledger gap failed the gate"
expect "cash balances not checked"
chmod 000 "$money"
! gate || fail "an unreadable money file passed the money check"
expect "cannot read money figures"
chmod 600 "$money"

# Outgoing messages use the same live-money spellings and boundary rules
# as unpublished files, including rounded figures. Only SHA and line escape.
printf '%s\n%s\n' "$book" "$cash" >"$money"
repo="$message_repo"
git -C "$message_repo" update-ref refs/remotes/origin/main HEAD
message_commit 'Synthetic subject

A 618k book and cash of 314,159.26.'
! gate || fail "unpublished commit-body money figures were accepted"
expect "commit $message_sha names a money figure of the live book on line(s) 3"
withheld 618k
withheld 314
git -C "$message_repo" update-ref refs/remotes/origin/main HEAD
message_commit 'An older 123,987, a paper 864,197 and digest a618034f are not live figures'
gate || fail "message money matching diverged from the file boundary rules"
repo="$index_repo"

[ ! -e "$test_root/sqlite-calls" ] || { cat "$test_root/sqlite-calls" >"$out"; fail "the gate ran sqlite3 against the daemon store"; }

# The canonical release and its internal entry must both run the privacy
# gate before their first push; later CI is already past disclosure.
python3 - "$repo_root/Makefile" <<'PY_RELEASE_ORDER'
from pathlib import Path
import sys
text = Path(sys.argv[1]).read_text().splitlines()
for target in ("release:", "_release-run:"):
    start = next(i for i, line in enumerate(text) if line.startswith(target))
    gated = False
    found_push = False
    for line in text[start + 1:]:
        if line and not line.startswith(("\t", "#", " ")):
            break
        if line.startswith("\t") and "$(MAKE) account-data-check" in line:
            gated = True
        if line.startswith("\t") and "git push " in line:
            found_push = True
            if not gated:
                raise SystemExit(f"{target} first push precedes the account-data gate")
            break
    if not found_push:
        raise SystemExit(f"{target} publication boundary missing")
PY_RELEASE_ORDER

echo "check-no-account-data test: OK"
