#!/bin/sh
# check-no-account-data.sh — fail the pre-commit gate when tracked/staged
# files carry real IBKR account data. Five checks, all over the git index
# (tracked + staged-for-add files):
#   1. No HTML at the repo root and 2. no scratch-page names (*lab*.html,
#      *scratch*) — scratch pages and screenshots are the historical leak.
#   3. No IBKR account IDs (U / DU followed by 6-9 digits) anywhere,
#      including Go files and binary blobs. Only conspicuous synthetic
#      placeholders are allowlisted.
#   4. No compiled executables.
#   5. No current holdings, matched against a denylist derived at run time
#      from private local data and never committed.
set -eu

# Byte-wise grep: the locale-aware path is ~5x slower over the docs tree.
LC_ALL=C
export LC_ALL

cd "$(dirname "$0")/.."

self=scripts/check-no-account-data.sh
status=0

# Index contents, minus files staged for deletion / missing on disk
files=$(git ls-files --cached | while IFS= read -r f; do
  [ -e "$f" ] && printf '%s\n' "$f"
done)

# 1) HTML at repo root.
root_html=$(printf '%s\n' "$files" | grep -E '^[^/]+\.html$' || true)
if [ -n "$root_html" ]; then
  echo "check-no-account-data: HTML file(s) at repo root — scratch pages stay untracked, real pages live under docs/ or web/:" >&2
  printf '  %s\n' $root_html >&2
  status=1
fi

# 2) Scratch-page names anywhere in the tree.
scratch=$(printf '%s\n' "$files" | grep -iE '(^|/)[^/]*lab[^/]*\.html$|scratch' || true)
if [ -n "$scratch" ]; then
  echo "check-no-account-data: scratch-page filename(s) tracked (*lab*.html / *scratch*):" >&2
  printf '  %s\n' $scratch >&2
  status=1
fi

# 3) Account IDs anywhere in the index. git grep scans staged blob
id_re='(^|[^[:alnum:]_])D?U[0-9]{6,9}([^[:alnum:]]|$)'
# Repdigit IDs join the sequence dummies: a real account never reads as
# several distinct accounts at once.
allow_re='D?U1234567|D?U7654321|DU123456|DU0000000|D?U1111111|D?U2222222|D?U6666666|D?U9999999'
candidates=$(git grep --cached -laEi "$id_re" -- ":!$self" || true)
for f in $candidates; do
	ids=$(git grep --cached -haoiE "$id_re" -- "$f" | grep -oiE 'D?U[0-9]{6,9}' |
		tr '[:lower:]' '[:upper:]' | grep -vxE "$allow_re" || true)
	if [ -n "$ids" ]; then
		count=$(printf '%s\n' "$ids" | wc -l | tr -d ' ')
		echo "check-no-account-data: $f contains $count non-placeholder IBKR account ID occurrence(s)" >&2
		echo "                       real IDs must never be committed; use the U1234567 / DU1234567 placeholders" >&2
		status=1
	fi
done

# 4) No compiled executables in the index (Mach-O / ELF magic). A stray
#    and other checked-in assets pass — only executable container magic
#    fails. Size-gated so the magic sniff touches a handful of files.
bins=$(printf '%s\n' "$files" | while IFS= read -r f; do
	[ -f "$f" ] || continue
	size=$(wc -c <"$f" | tr -d ' ')
	[ "$size" -gt 65536 ] || continue
	case $(od -An -N4 -tx1 "$f" | tr -d ' \n') in
	(cffaedfe | cefaedfe | feedface | feedfacf | cafebabe | bebafeca | 7f454c46) printf '%s\n' "$f" ;;
	esac
done)
if [ -n "$bins" ]; then
	echo "check-no-account-data: compiled executable(s) tracked — build outputs stay untracked:" >&2
	printf '  %s\n' $bins >&2
	status=1
fi


# 5) No current holdings. The denylist never enters the repo, not even
#    hashed: the ticker space is small enough to brute-force. It is the
#    latest Flex position report per account in the daemon's own store,
#    opened read-only, and a private copy cached outside the repo stands in
#    when the store cannot be opened. With neither (CI, contributor machines)
#    the check skips with a notice. A store that cannot be read and has no
#    cache, or that holds statements but yields no ticker, fails instead:
#    the owner's machine never skips silently.
home=${HOME:-/nonexistent}
state=${XDG_STATE_HOME:-$home/.local/state}/ibkr/daemon.db
cache=${XDG_CACHE_HOME:-$home/.cache}/ibkr/holdings-denylist
ticker_re='[A-Z][A-Z0-9]*([.][A-Z0-9]+)?'
tilde() { case $1 in "$home"/*) printf '~/%s' "${1#"$home"/}" ;; *) printf '%s' "$1" ;; esac; }
store() { sqlite3 -readonly -bail -cmd '.timeout 2000' "$state" "$1" 2>/dev/null; }
held= asof= source= why= unreadable=
if [ ! -e "$state" ]; then
	why="no daemon store at $(tilde "$state")"
elif ! command -v sqlite3 >/dev/null 2>&1; then
	why="no sqlite3 to read the daemon store at $(tilde "$state")" unreadable=1
elif ! flex=$(store 'SELECT count(*) FROM statement_files'); then
	why="cannot read the daemon store at $(tilde "$state")" unreadable=1
elif [ "$flex" -eq 0 ]; then
	why="the daemon store holds no Flex statements"
else
	# Rows read day|symbol|underlying: an option contributes its underlying,
	# and an IBKR class suffix ("BRK B") keeps its root.
	out=$(store "WITH pos AS (SELECT account_key AS a, substr(json_extract(raw_json, '\$.ReportDate'), 1, 10) AS d,
		json_extract(raw_json, '\$.Symbol') AS s, json_extract(raw_json, '\$.UnderlyingSymbol') AS u
		FROM statement_records WHERE record_kind = 'position')
		SELECT d, s, u FROM pos WHERE d = (SELECT max(d) FROM pos AS p WHERE p.a = pos.a)") || out=
	asof=$(printf '%s\n' "$out" | cut -d'|' -f1 | sort -r | sed -n 1p)
	held=$(printf '%s\n' "$out" | cut -d'|' -f2- | tr '|' '\n' | sed 's/ .*//' | grep -xE "$ticker_re" | sort -u || true)
	if [ -n "$held" ]; then
		source="the daemon store"
		(umask 077 && mkdir -p "${cache%/*}" && printf '# as of %s\n%s\n' "$asof" "$held" >"$cache.$$" &&
			mv -f "$cache.$$" "$cache") 2>/dev/null || rm -f "$cache.$$"
	else
		echo "check-no-account-data: the daemon store holds Flex statements but no ticker was read from its" >&2
		echo "                       position report; fix the holdings query here rather than skip the check" >&2
		status=1
	fi
fi
if [ -z "$source" ] && [ -n "$why" ] && [ -f "$cache" ]; then
	held=$(grep -xE "$ticker_re" "$cache" | sort -u || true)
	asof=$(sed -n 's/^# as of //p' "$cache")
	[ -z "$held" ] || source="the private cache ($why)"
fi

# Single letters, Canary's own market instruments (pkg/ibkr/symbols.go) and
# tickers that are also its vocabulary (broker and venues, contract types,
# order terms, date layouts, acronyms) match the product's own text; a
# holding among them is counted, not checked. Public reference lists name
# every S&P 500 member by construction and stay out of scope by path.
unchecked='VIX VVIX VIX3M SPX NDX RUT DJI DJX DXY SPY QQQ IWM DIA GLD TLT HYG SMH XLK XLF XLI XLE XLV
XLY XLP XLU XLB XLRE IBIT IBKR CBOE CME ICE OPT CASH LMT DAY DTE DD ET PATH KEY CI ON AI UI'
public_lists=':!internal/breadth/spx/members_data.go :!internal/breadth/spx/sectors_data.go
:!internal/daemon/market_tape_leaders.go'
if [ -n "$source" ]; then
	checked=$(printf '%s\n' "$held" | grep -vxF "$(printf '%s\n' $unchecked)" | grep -xE '..+' || true)
	skipped=$(($(printf '%s\n' "$held" | grep -c .) - $(printf '%s\n' "$checked" | grep -c . || true)))
	# Released CHANGELOG sections are published record (tags and GitHub
	# releases); only the section being prepared above them is checked.
	cut=$(git show :CHANGELOG.md 2>/dev/null | awk '/^## v[0-9]/ { print NR, $2 }' | while read -r n v; do
		if git rev-parse -q --verify "refs/tags/$v" >/dev/null; then echo "$n" && break; fi
	done)
	raw=
	if [ -n "$checked" ]; then
		raw=$(printf '%s\n' "$checked" | git grep --cached -I -n -w -F -f - -- . $public_lists) || [ $? -eq 1 ] || {
			echo "check-no-account-data: git grep failed; the holdings check did not run" >&2
			status=1
		}
	fi
	# Only path and line leave the script: gate output travels into commit
	# messages, reports and CI logs, so the ticker is withheld.
	hits=$(printf '%s\n' "$raw" | awk -F: -v cut="${cut:-0}" \
		'NF > 2 && !($1 == "CHANGELOG.md" && cut > 0 && $2 >= cut) { l[$1] = l[$1] (l[$1] == "" ? "" : ",") $2 }
		END { for (f in l) print "check-no-account-data: " f " names a current holding on line(s) " l[f] }' | sort)
	if [ -n "$hits" ]; then
		printf '%s\n' "$hits" >&2
		echo "                       real holdings must never be committed; use a synthetic ticker such as SYNTH" >&2
		status=1
	fi
	echo "check-no-account-data: holdings checked against $source, positions as of ${asof:-unknown}"
	[ "$skipped" -eq 0 ] || echo "check-no-account-data: $skipped held ticker(s) not checked: single letters, Canary instruments or vocabulary"
elif [ -n "$unreadable" ]; then
	echo "check-no-account-data: $why and no private cache at $(tilde "$cache");" >&2
	echo "                       a machine with a daemon store never skips the holdings check" >&2
	status=1
elif [ -n "$why" ]; then
	echo "check-no-account-data: holdings check SKIPPED — no private holdings data ($why, no cache at $(tilde "$cache"));" >&2
	echo "                       expected on CI and contributor machines" >&2
fi

[ "$status" -eq 0 ] && echo "check-no-account-data: OK"
exit "$status"
