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
	expected_version=$3
	printf '%s\n' "$expected_version" | grep -Eq '^v2\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || return 1
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
	[ "$version" = "$expected_version" ] || return 1
	[ "$replacement" = "$expected_replace" ] || return 1
}

validate_replacement_records() {
	records=$1
	expected_witness=$2
	expected_version=$3
	if [ -z "$expected_witness" ]; then
		[ -z "$records" ]
		return
	fi
	expected="github.com/osauer/hyperserve/v2|$expected_version|$expected_witness||$expected_witness"
	[ "$records" = "$expected" ]
}

# Pure synthetic fixtures keep the authority rule strict without adding a
# second test-only parser that could drift from this gate.
old_record='github.com/osauer/hyperserve|v1.6.0|'
unrelated_replace_record='example.com/dependency|v1.0.0|/private/tmp/dependency||/private/tmp/dependency'
for fixture_version in v2.1.4 v2.12.0; do
	public_record="github.com/osauer/hyperserve/v2|$fixture_version|"
	local_record="github.com/osauer/hyperserve/v2|$fixture_version|/private/tmp/candidate"
	witness_replace_record="github.com/osauer/hyperserve/v2|$fixture_version|/private/tmp/candidate||/private/tmp/candidate"
	validate_hyperserve_records "$public_record" '' "$fixture_version" || fail 'internal HyperServe public-authority fixture failed'
	if validate_hyperserve_records "$local_record" '' "$fixture_version" || \
		validate_hyperserve_records "$old_record" '' "$fixture_version" || \
		validate_hyperserve_records "$public_record" '' 'v2.0.0' || \
		validate_hyperserve_records "$public_record
$public_record" '' "$fixture_version"; then
		fail 'internal HyperServe forbidden-authority fixture passed'
	fi
	validate_hyperserve_records "$local_record" '/private/tmp/candidate' "$fixture_version" || fail 'internal HyperServe witness fixture failed'
	validate_replacement_records '' '' "$fixture_version" || fail 'internal public replacement fixture failed'
	if validate_replacement_records "$witness_replace_record" '' "$fixture_version" || \
		validate_replacement_records "$unrelated_replace_record" '' "$fixture_version" || \
		validate_replacement_records "$witness_replace_record" '/private/tmp/other' "$fixture_version" || \
		validate_replacement_records "github.com/osauer/hyperserve/v2|$fixture_version|example.com/fork|v2.0.0|/private/tmp/candidate" '/private/tmp/candidate' "$fixture_version"; then
		fail 'internal forbidden replacement fixture passed'
	fi
	validate_replacement_records "$witness_replace_record" '/private/tmp/candidate' "$fixture_version" || fail 'internal witness replacement fixture failed'
	if validate_replacement_records "$witness_replace_record
$unrelated_replace_record" '/private/tmp/candidate' "$fixture_version"; then
		fail 'internal unrelated witness replacement fixture passed'
	fi
done
for invalid_version in v1.6.0 v2.2.0-rc.1 v2.2.0-20260913100000-abcdefabcdef v2.01.4; do
	if validate_hyperserve_records "github.com/osauer/hyperserve/v2|$invalid_version|" '' "$invalid_version"; then
		fail 'internal unstable HyperServe version fixture passed'
	fi
done

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

# go.mod owns the tested version. Latest-release discovery belongs to the
# explicit update workflow, never an ordinary build/check or a duplicate pin.
hyperserve_version=$(GOWORK=off go list -m -f '{{.Version}}' github.com/osauer/hyperserve/v2)
hyperserve_records=$(GOWORK=off go list -m -f '{{if or (eq .Path "github.com/osauer/hyperserve/v2") (eq .Path "github.com/osauer/hyperserve")}}{{.Path}}|{{.Version}}|{{with .Replace}}{{.Dir}}{{end}}{{end}}' all | sed '/^$/d')
replacement_records=$(GOWORK=off go list -m -f '{{if not .Main}}{{with .Replace}}{{$.Path}}|{{$.Version}}|{{.Path}}|{{.Version}}|{{.Dir}}{{end}}{{end}}' all | sed '/^$/d')
expected_replace=
if [ -n "${CANARY_HYPERSERVE_WITNESS_DIR:-}" ]; then
	expected_replace=$(cd "$CANARY_HYPERSERVE_WITNESS_DIR" 2>/dev/null && pwd -P) || \
		fail "HyperServe witness directory is not readable: $CANARY_HYPERSERVE_WITNESS_DIR"
fi
if ! validate_hyperserve_records "$hyperserve_records" "$expected_replace" "$hyperserve_version"; then
	if [ -n "$expected_replace" ]; then
		fail "HyperServe must be stable /v2@$hyperserve_version with the explicit witness replace $expected_replace"
	fi
	fail "HyperServe must be stable public /v2@$hyperserve_version with no v1 module or replacement"
fi
if ! validate_replacement_records "$replacement_records" "$expected_replace" "$hyperserve_version"; then
	if [ -n "$expected_replace" ]; then
		fail "the only permitted replacement is /v2@$hyperserve_version => $expected_replace"
	fi
	fail 'the public product graph must not contain any module replacement'
fi
hyperserve_authority="public HyperServe $hyperserve_version authority"
if [ -n "$expected_replace" ]; then
	hyperserve_authority="explicit HyperServe $hyperserve_version witness authority"
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
