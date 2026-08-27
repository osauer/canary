#!/bin/sh

set -eu

repo=$(git rev-parse --show-toplevel)
cd "$repo"

fail() {
	printf 'go-dependencies: %s\n' "$1" >&2
	exit 1
}

validate_hyperserve_records() {
	records=$1
	expected_replace=$2
	v2_record=
	while IFS= read -r record; do
		[ -n "$record" ] || continue
		case "$record" in
			github.com/osauer/hyperserve\|*) return 1 ;;
			github.com/osauer/hyperserve/v2\|*)
				[ -z "$v2_record" ] || return 1
				v2_record=$record
				;;
			*) return 1 ;;
		esac
	done <<EOF
$records
EOF

	[ -n "$v2_record" ] || return 1
	rest=${v2_record#*|}
	version=${rest%%|*}
	replacement=${rest#*|}
	[ "$version" = "v2.1.0" ] || return 1
	[ "$replacement" = "$expected_replace" ] || return 1
}

validate_replacement_records() {
	records=$1
	expected_witness=$2
	if [ -z "$expected_witness" ]; then
		[ -z "$records" ]
		return
	fi
	expected="github.com/osauer/hyperserve/v2|v2.1.0|$expected_witness||$expected_witness"
	[ "$records" = "$expected" ]
}

# Pure synthetic fixtures keep the production rule pinned without adding a
# second test-only parser that could drift from this gate.
public_record='github.com/osauer/hyperserve/v2|v2.1.0|'
local_record='github.com/osauer/hyperserve/v2|v2.1.0|/private/tmp/candidate'
old_record='github.com/osauer/hyperserve|v1.6.0|'
witness_replace_record='github.com/osauer/hyperserve/v2|v2.1.0|/private/tmp/candidate||/private/tmp/candidate'
unrelated_replace_record='example.com/dependency|v1.0.0|/private/tmp/dependency||/private/tmp/dependency'
validate_hyperserve_records "$public_record" '' || fail 'internal HyperServe public-authority fixture failed'
if validate_hyperserve_records "$local_record" '' || validate_hyperserve_records "$old_record" ''; then
	fail 'internal HyperServe forbidden-authority fixture passed'
fi
validate_hyperserve_records "$local_record" '/private/tmp/candidate' || fail 'internal HyperServe witness fixture failed'
validate_replacement_records '' '' || fail 'internal public replacement fixture failed'
if validate_replacement_records "$witness_replace_record" '' || \
	validate_replacement_records "$unrelated_replace_record" ''; then
	fail 'internal forbidden replacement fixture passed'
fi
validate_replacement_records "$witness_replace_record" '/private/tmp/candidate' || \
	fail 'internal witness replacement fixture failed'
if validate_replacement_records "$witness_replace_record
$unrelated_replace_record" '/private/tmp/candidate'; then
	fail 'internal unrelated witness replacement fixture passed'
fi

expected_direct='github.com/BurntSushi/toml
github.com/osauer/hyperserve/v2
github.com/skip2/go-qrcode
golang.org/x/mod
golang.org/x/sys
modernc.org/sqlite'
actual_direct=$(GOWORK=off go list -m -f '{{if and (not .Main) (not .Indirect)}}{{.Path}}{{end}}' all | sed '/^$/d' | sort)
if [ "$actual_direct" != "$expected_direct" ]; then
	printf '%s\nexpected:\n%s\nactual:\n%s\n' \
		'go-dependencies: direct product dependency allowlist changed' \
		"$expected_direct" "$actual_direct" >&2
	exit 1
fi

hyperserve_records=$(GOWORK=off go list -m -f '{{if or (eq .Path "github.com/osauer/hyperserve/v2") (eq .Path "github.com/osauer/hyperserve")}}{{.Path}}|{{.Version}}|{{with .Replace}}{{.Dir}}{{end}}{{end}}' all | sed '/^$/d')
replacement_records=$(GOWORK=off go list -m -f '{{if not .Main}}{{with .Replace}}{{$.Path}}|{{$.Version}}|{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}' all | sed '/^$/d')
expected_replace=
if [ -n "${CANARY_HYPERSERVE_WITNESS_DIR:-}" ]; then
	expected_replace=$(cd "$CANARY_HYPERSERVE_WITNESS_DIR" 2>/dev/null && pwd -P) || \
		fail "HyperServe witness directory is not readable: $CANARY_HYPERSERVE_WITNESS_DIR"
fi
if ! validate_hyperserve_records "$hyperserve_records" "$expected_replace"; then
	if [ -n "$expected_replace" ]; then
		fail "HyperServe must be exactly /v2@v2.1.0 with the explicit witness replace $expected_replace"
	fi
	fail 'HyperServe must be exactly public /v2@v2.1.0 with no v1 module or replacement'
fi
if ! validate_replacement_records "$replacement_records" "$expected_replace"; then
	if [ -n "$expected_replace" ]; then
		fail "the only permitted replacement is /v2@v2.1.0 => $expected_replace"
	fi
	fail 'the public product graph must not contain any module replacement'
fi
hyperserve_authority='public HyperServe v2.1.0 authority'
if [ -n "$expected_replace" ]; then
	hyperserve_authority='explicit HyperServe v2.1.0 witness authority'
fi

product_graph=$(GOWORK=off go list -m -f '{{if not .Main}}{{.Path}}{{end}}' all | sed '/^$/d' | sort -u)
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

GOWORK=off go mod tidy -diff
GOWORK=off go -C tools mod tidy -diff
GOWORK=off go -C scripts/docgen/docs-html mod tidy -diff

printf 'go-dependencies: OK (6 direct product modules; %s; OpenPGP verifier retired)\n' "$hyperserve_authority"
