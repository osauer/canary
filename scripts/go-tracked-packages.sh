#!/usr/bin/env bash
# Print the packages `go list "$@"` matches, one ./-relative path per line,
# keeping only those whose directory holds a tracked or untracked-but-not-
# ignored .go file. Run from the module root.
#
# The Go analysers in `make check` (vet, staticcheck, modernize, govulncheck)
# take package patterns, and `./...` also matches gitignored directories that
# don't start with `.` or `_`: on 2026-10-05 a July scratch probe under tmp/
# failed the gate for code that was never part of the tree. This keeps Go's
# own matching (build tags, testdata, nested modules) and drops only what
# .gitignore or .git/info/exclude hides; gofmt-check and go-doc-audit already
# scope to the same `git ls-files` set.

set -euo pipefail

module_dir=$(go list -m -f '{{.Dir}}')
tracked=$(git ls-files --cached --others --exclude-standard -- '*.go')
listed=$(go list -e -f '{{.Dir}}' "$@")

pkgs=$(awk -v root="$module_dir" '
	NR == FNR {
		if ($0 == "") next
		slash = match($0, /\/[^\/]*$/)
		keep[slash ? substr($0, 1, slash - 1) : "."] = 1
		next
	}
	$0 == "" { next }
	$0 == root { if ("." in keep) print "."; next }
	index($0, root "/") != 1 { print "go-tracked-packages: " $0 " is outside " root > "/dev/stderr"; exit 2 }
	{ rel = substr($0, length(root) + 2); if (rel in keep) print "./" rel }
' <(printf '%s\n' "$tracked") <(printf '%s\n' "$listed"))

if [ -z "$pkgs" ]; then
	echo "go-tracked-packages: no tracked Go package matches: $*" >&2
	exit 1
fi
printf '%s\n' "$pkgs"
