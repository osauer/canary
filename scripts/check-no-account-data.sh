#!/bin/sh
# check-no-account-data.sh — fail the pre-commit gate when tracked/staged
# files carry real IBKR account data. Six checks, all over the git index
# (tracked + staged-for-add files):
#   1. No HTML at the repo root and 2. no scratch-page names (*lab*.html,
#      *scratch*) — scratch pages and screenshots are the historical leak.
#   3. No IBKR account IDs (U / DU followed by 6-9 digits) anywhere,
#      including Go files and binary blobs. Only conspicuous synthetic
#      placeholders are allowlisted.
#   4. No compiled executables.
#   5. No current holdings and 6. no money figures of the live book (net
#      liquidation value, cash balances), matched against denylists derived
#      at run time from private local data and never committed.
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

# 3) Account IDs anywhere in the index. Keep both grep stages in text mode:
# binary match boundaries can include NUL bytes, which GNU grep otherwise hides.
id_re='(^|[^[:alnum:]_])D?U[0-9]{6,9}([^[:alnum:]]|$)'
# Repdigit IDs join the sequence dummies: a real account never reads as
# several distinct accounts at once.
allow_re='D?U1234567|D?U7654321|DU123456|DU0000000|D?U1111111|D?U2222222|D?U6666666|D?U9999999'
candidates=$(git grep --cached -laEi "$id_re" -- ":!$self" || true)
for f in $candidates; do
	ids=$(git grep --cached -haoiE "$id_re" -- "$f" | grep -aoiE 'D?U[0-9]{6,9}' |
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
#    fails. Size-gated so the magic sniff touches a handful of files; one
#    wc sizes them all (a summary "total" line is no file and drops out).
bins=$(printf '%s\n' "$files" | tr '\n' '\0' | xargs -0 wc -c 2>/dev/null |
	awk '$1 > 65536 { sub(/^ *[0-9]+ /, ""); print }' | while IFS= read -r f; do
	[ -f "$f" ] || continue
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
# -init /dev/null: a personal ~/.sqliterc must not change the output format.
store() { sqlite3 -init /dev/null -readonly -bail -cmd '.timeout 2000' "$state" "$1" 2>/dev/null; }
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
	out=$(store "WITH pos AS MATERIALIZED (SELECT account_key AS a, substr(json_extract(raw_json, '\$.ReportDate'), 1, 10) AS d,
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
# Released CHANGELOG sections are published record (tags and GitHub
# releases); only the section being prepared above them is checked.
cut=$(git show :CHANGELOG.md 2>/dev/null | awk '/^## v[0-9]/ { print NR, $2 }' | while read -r n v; do
	if git rev-parse -q --verify "refs/tags/$v" >/dev/null; then echo "$n" && break; fi
done)
if [ -n "$source" ]; then
	checked=$(printf '%s\n' "$held" | grep -vxF "$(printf '%s\n' $unchecked)" | grep -xE '..+' || true)
	skipped=$(($(printf '%s\n' "$held" | grep -c .) - $(printf '%s\n' "$checked" | grep -c . || true)))
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

# 6) No money figures of the live book: the net liquidation value of each
#    live account (its latest risk-capital document) and the cash balances
#    the live proposal snapshot reports per currency, read from the daemon
#    store like the holdings and kept nowhere else. Without a store (CI) the
#    check is silent; a store it cannot read, or a live account without an
#    NLV, fails it. A figure of four or more significant digits is matched
#    in whole units, grouped by ",", ".", "_" or not at all, cents optional.
#    Authors round, and the book moves before they commit, so in files not
#    yet on origin/main every multiple of 100 within 2% of a figure counts
#    too, also as 12.3k and 123k. Published files are judged on whole units
#    only, so a market move never fails an unchanged tree. A spelling needs
#    three significant digits, an ungrouped one five digits, and must stand
#    as a whole number. Only path and line leave the script.
if [ -e "$state" ]; then
	if ! mout=$(store "WITH rc AS (SELECT json_extract(document_json, '\$.state.account_id') AS a,
			json_extract(document_json, '\$.state.last_equity_as_of') AS t, json_extract(document_json, '\$.state.last_equity_base') AS v
			FROM state_documents WHERE kind = 'risk_capital' AND json_extract(document_json, '\$.state.account_mode') = 'live')
		SELECT 'nlv', coalesce(v, 'missing'), substr(t, 1, 10) FROM rc WHERE t = (SELECT max(t) FROM rc AS o WHERE o.a IS rc.a)
		UNION ALL SELECT 'cash', j.value, NULL FROM state_documents AS d, json_each(d.document_json, '\$.cash_sweep.currencies') AS c,
			json_each(c.value) AS j WHERE d.kind = 'trade_proposals_current' AND json_extract(d.document_json, '\$.account_mode') = 'live'
			AND j.key IN ('cash', 'trade_date_cash', 'settled_cash')"); then
		echo "check-no-account-data: cannot read money figures from the daemon store at $(tilde "$state"); the money check did not run" >&2
		status=1
	elif ! printf '%s\n' "$mout" | grep -q '^nlv|'; then
		echo "check-no-account-data: money check SKIPPED — the daemon store holds no live net liquidation value" >&2
	elif printf '%s\n' "$mout" | grep -q '^nlv|missing|'; then
		echo "check-no-account-data: a live risk-capital document has no net liquidation value; fix the money query here" >&2
		echo "                       rather than skip the check" >&2
		status=1
	else
		# Whole units of every figure first ("all": the whole index), then
		# the rounded spellings within 2% ("new": files changed since the
		# merge base with origin/main, or every file without one).
		spellings=$(printf '%s\n' "$mout" | awk -F'|' '
			function sig(s) { gsub(/[^0-9]/, "", s); sub(/^0+/, "", s); sub(/0+$/, "", s); return length(s) }
			function grp(n, sep,   s, o) {
				for (s = sprintf("%d", n); length(s) > 3; s = substr(s, 1, length(s) - 3)) o = sep substr(s, length(s) - 2) o
				return s o
			}
			function put(s) { if (sig(s) >= 3 && !(s in seen)) { seen[s]; print tier, s } }
			function units(n) { if (n >= 10000) put(sprintf("%d", n)); put(grp(n, ",")); put(grp(n, ".")); put(grp(n, "_")) }
			$2 ~ /^-?[0-9]+([.][0-9]+)?$/ { v = $2 < 0 ? -$2 : $2; if (sig(sprintf("%d", v + 0.5)) >= 4) F[++nf] = v }
			END {
				tier = "all"; for (i = 1; i <= nf; i++) { units(int(F[i])); units(int(F[i] + 0.5)) }
				tier = "new"
				for (i = 1; i <= nf; i++) for (m = int(F[i] * 0.98 / 100) * 100 + 100; m <= F[i] * 1.02; m += 100) {
					units(m); put(m % 1000 ? sprintf("%d.%dk", m / 1000, m % 1000 / 100) : sprintf("%dk", m / 1000))
				}
			}')
		changed= all=1
		base=$(git merge-base HEAD origin/main 2>/dev/null) && changed=$(git diff --cached --name-only "$base" --) && all=0
		raw=
		if [ -n "$spellings" ]; then
			raw=$(printf '%s\n' "$spellings" | cut -d' ' -f2 |
				git grep --cached -I -n -i -F -f - -- . ':!scripts/check-no-account-data_test.sh') || [ $? -eq 1 ] || {
				echo "check-no-account-data: git grep failed; the money check did not run" >&2
				status=1
			}
		fi
		# A spelling counts only as a whole number: not the tail of a longer
		# one (an ungrouped spelling may follow a comma: CSV, JSON), a hex
		# colour or an identifier, nor the head of a longer one, though cents
		# may follow.
		hits=$(printf '%s\n' "$raw" | MONEY_SPELLINGS="$spellings" MONEY_CHANGED="$changed" awk -v cut="${cut:-0}" -v all="$all" '
			function digit(c) { return c ~ /[0-9]/ }
			BEGIN {
				n = split(ENVIRON["MONEY_SPELLINGS"], P, "\n")
				for (k = 1; k <= n; k++) { T[k] = substr(P[k], 1, 3); P[k] = substr(P[k], 5) }
				m = split(ENVIRON["MONEY_CHANGED"], x, "\n"); for (i = 1; i <= m; i++) C[x[i]]
			}
			{
				if (!(i = index($0, ":"))) next
				f = substr($0, 1, i - 1); r = substr($0, i + 1); i = index(r, ":")
				l = substr(r, 1, i - 1); t = tolower(substr(r, i + 1))
				if (f == "CHANGELOG.md" && cut > 0 && l + 0 >= cut) next
				for (k = 1; k <= n; k++) {
					if (T[k] == "new" && !all && !(f in C)) continue
					p = P[k]; kf = p ~ /k$/; g = substr(p, length(p) - 3, 1); sep = !kf && g ~ /[.,_]/ ? g : ""
					for (o = 0; (j = index(substr(t, o + 1), p)) > 0; o += j) {
						s = o + j; e = s + length(p); a = substr(t, e, 1)
						b = s > 1 ? substr(t, s - 1, 1) : ""; bb = s > 2 ? substr(t, s - 2, 1) : ""
						if (digit(b) || b == "#" || (!kf && b ~ /[a-z_]/) || (digit(bb) && (b == "." || sep != "" && b ~ /[.,_]/))) continue
						if (digit(a) || a ~ (kf ? "[a-z]" : "[a-z_]") || (a == sep && digit(substr(t, e + 1, 1)))) continue
						L[f] = L[f] (L[f] == "" ? "" : ",") l
						next
					}
				}
			}
			END { for (f in L) print "check-no-account-data: " f " names a money figure of the live book on line(s) " L[f] }' | sort)
		if [ -n "$hits" ]; then
			printf '%s\n' "$hits" >&2
			echo "                       the live book's NLV and cash balances must never be committed; use figures far from them" >&2
			status=1
		fi
		echo "check-no-account-data: money figures checked against the daemon store, NLV as of $(printf '%s\n' "$mout" |
			awk -F'|' '$1 == "nlv" { print $3 }' | sort -r | sed -n 1p)"
		printf '%s\n' "$mout" | grep -qE '^cash\|-?[0-9]' ||
			echo "check-no-account-data: cash balances not checked: the live proposal snapshot reports none (sweep off or ledger unavailable)"
	fi
fi

[ "$status" -eq 0 ] && echo "check-no-account-data: OK"
exit "$status"
