#!/usr/bin/env bash
# Fixture test for go-tracked-packages.sh: gitignored packages leave the list,
# everything Go's own `./...` matching keeps stays, and a gitignored package
# that does not compile no longer fails `go vet` over the list.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
lister="$repo_root/scripts/go-tracked-packages.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-go-tracked-packages-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

export GOWORK=off GOTOOLCHAIN=local GOFLAGS=

fail() {
	echo "go-tracked-packages test: $*" >&2
	exit 1
}

write() {
	mkdir -p "$(dirname "$test_root/$1")"
	printf '%s\n' "$2" > "$test_root/$1"
}

write go.mod $'module example.test/fixture\n\ngo 1.22'
write root.go 'package fixture'
write a/a.go 'package a'
write a/testdata/t.go 'package t'
write onlytests/x_test.go 'package onlytests'
write tagged/t.go $'//go:build trading\n\npackage tagged'
write tools/go.mod $'module example.test/tools\n\ngo 1.22'
write tools/t.go 'package tools'
write .gitignore 'tmp/'
write tmp/probe/main.go $'package main\n\nfunc main() { removedFunction() }'
write excluded/e.go $'package excluded\n\nvar _ = removedFunction()'
git -C "$test_root" init -q
printf 'excluded/\n' >> "$test_root/.git/info/exclude"
git -C "$test_root" add .
write fresh/f.go 'package fresh'

cd "$test_root"

got=$("$lister" ./...)
want=$'.\n./a\n./fresh\n./onlytests'
[ "$got" = "$want" ] || fail "default build listed:"$'\n'"$got"$'\nwant:\n'"$want"

got=$("$lister" -tags trading ./...)
want=$'.\n./a\n./fresh\n./onlytests\n./tagged'
[ "$got" = "$want" ] || fail "trading build listed:"$'\n'"$got"$'\nwant:\n'"$want"

if "$lister" ./tmp/... >/dev/null 2>&1; then
	fail "a pattern matching only gitignored packages produced a list"
fi

if go vet ./... >/dev/null 2>&1; then
	fail "fixture no longer reproduces the failure: go vet ./... passed over the broken gitignored packages"
fi
# shellcheck disable=SC2046 # one package path per word
go vet $("$lister" ./...) || fail "go vet over the listed packages failed"
