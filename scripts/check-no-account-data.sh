#!/bin/sh
# check-no-account-data.sh — fail the pre-commit gate when tracked/staged
# files carry real IBKR account data. Scratch pages and screenshots are the
# historical leak vector here, so the gate is deliberately paranoid about
# root-level HTML, scratch-style filenames, and account-id shapes.
#
# Three checks, all over the git index (tracked + staged-for-add files):
#   1. No HTML files at the repo root — real pages live under docs/ or
#      web/; root HTML is a scratch page by definition here.
#   2. No scratch-page names anywhere (*lab*.html, *scratch*).
#   3. No IBKR account IDs (U / DU followed by 6-9 digits) anywhere,
#      including Go files and binary blobs. Only conspicuous synthetic
#      documentation/test placeholders (U1234567-style dummies) are
#      allowlisted.
set -eu

# Byte-wise grep: the locale-aware path is ~5x slower over the docs tree.
LC_ALL=C
export LC_ALL

cd "$(dirname "$0")/.."

self=scripts/check-no-account-data.sh
status=0

# Index contents, minus files staged for deletion / missing on disk
# (same scope rationale as gofmt-check in the Makefile).
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
#    contents (multithreaded, ~3x faster than xargs grep over the worktree
#    here). Boundary classes instead of \b for BSD/GNU regex portability;
#    the trailing class rejects longer digit runs.
id_re='(^|[^[:alnum:]_])D?U[0-9]{6,9}([^[:alnum:]]|$)'
# Repdigit IDs join the sequence dummies: a real account never reads as
# seven identical digits, so they stay safe fixtures for tests that need
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
#    `go build` output at the repo root has been committed before; images
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

[ "$status" -eq 0 ] && echo "check-no-account-data: OK"
exit "$status"
