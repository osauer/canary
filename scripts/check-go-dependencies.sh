#!/bin/sh

set -eu

repo=$(git rev-parse --show-toplevel)
cd "$repo"

fail() {
	printf 'go-dependencies: %s\n' "$1" >&2
	exit 1
}

expected_direct='github.com/BurntSushi/toml
github.com/osauer/hyperserve
github.com/skip2/go-qrcode
golang.org/x/mod
golang.org/x/sys
modernc.org/sqlite'
actual_direct=$(go list -m -f '{{if and (not .Main) (not .Indirect)}}{{.Path}}{{end}}' all | sed '/^$/d' | sort)
if [ "$actual_direct" != "$expected_direct" ]; then
	printf '%s\nexpected:\n%s\nactual:\n%s\n' \
		'go-dependencies: direct product dependency allowlist changed' \
		"$expected_direct" "$actual_direct" >&2
	exit 1
fi

product_graph=$(go list -m -f '{{if not .Main}}{{.Path}}{{end}}' all | sed '/^$/d' | sort -u)
for retired in \
	github.com/SherClockHolmes/webpush-go \
	github.com/coder/websocket \
	github.com/ProtonMail/go-crypto \
	github.com/cloudflare/circl \
	github.com/golang-jwt/jwt/v5 \
	github.com/yuin/goldmark
do
	if printf '%s\n' "$product_graph" | grep -Fqx "$retired"; then
		fail "retired or build-only module leaked into the product graph: $retired"
	fi
done

if grep -Eq '^[[:space:]]*tool[[:space:]]*\(' go.mod; then
	fail 'product go.mod contains a tool block; pin developer tools in tools/go.mod'
fi
[ -f tools/go.mod ] || fail 'tools/go.mod is missing'
[ -f tools/dependencies.go ] || fail 'tools/dependencies.go is missing; module-mode vulnerability scans need a package root'
[ -f scripts/docgen/docs-html/go.mod ] || fail 'docs-html module is missing'

go mod tidy -diff
go -C tools mod tidy -diff
go -C scripts/docgen/docs-html mod tidy -diff

printf '%s\n' 'go-dependencies: OK (6 direct product modules; OpenPGP verifier retired)'
