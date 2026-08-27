#!/usr/bin/env bash

# Prove that the compact current test spine still detects selected historical
# production regressions. Each case archives committed HEAD, overlays only the
# focused spine, applies the current equivalent of one historical bug, and
# requires the named test to fail for the expected reason. No live Gateway is
# involved.

set -euo pipefail

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
manifest="$repo_root/scripts/regression-spine.tsv"
contract_only=false
case "${1:-}" in
"") ;;
--contract-only) contract_only=true ;;
*) echo "usage: scripts/test-regression-spine.sh [--contract-only]" >&2; exit 2 ;;
esac

if [ ! -f "$manifest" ] || [ -L "$manifest" ]; then
	echo "regression-spine: manifest is missing or unsafe" >&2
	exit 1
fi

candidate=$(git -C "$repo_root" rev-parse --verify HEAD)
cases=0
while IFS=$'\t' read -r id fix mutation_patch package test_name expected_failure extra; do
	case "$id" in "" | \#*) continue ;; esac
	if [ -n "${extra:-}" ] \
		|| ! [[ "$id" =~ ^[a-z0-9][a-z0-9-]{0,63}$ ]] \
		|| ! [[ "$fix" =~ ^[0-9a-f]{40}$ ]] \
		|| ! [[ "$mutation_patch" =~ ^scripts/regression-spine/[a-z0-9-]+\.patch$ ]] \
		|| ! [[ "$package" =~ ^\./[A-Za-z0-9_./-]+$ ]] \
		|| ! [[ "$test_name" =~ ^Test[A-Za-z0-9_]+$ ]] \
		|| ! [[ "$expected_failure" =~ ^Test[A-Za-z0-9_/]+$ ]]; then
		echo "regression-spine: malformed manifest row for ${id:-unknown}" >&2
		exit 1
	fi
	git -C "$repo_root" cat-file -e "$fix^{commit}" 2>/dev/null || {
		echo "regression-spine: missing fix commit for $id" >&2
		exit 1
	}
	git -C "$repo_root" merge-base --is-ancestor "$fix" "$candidate" || {
		echo "regression-spine: fix $id is not an ancestor of candidate HEAD" >&2
		exit 1
	}
	if [ ! -f "$repo_root/$mutation_patch" ] || [ -L "$repo_root/$mutation_patch" ]; then
		echo "regression-spine: mutation patch is missing or unsafe for $id" >&2
		exit 1
	fi
	if grep -Eq '^diff --git a/.*_test\.go b/' "$repo_root/$mutation_patch" \
		|| ! grep -Eq '^diff --git a/.*\.go b/.*\.go$' "$repo_root/$mutation_patch"; then
		echo "regression-spine: $id mutation must touch production Go only" >&2
		exit 1
	fi
	if ! grep -Rqs --include='*_test.go' "func $test_name(" "$repo_root/${package#./}"; then
		echo "regression-spine: current suite is missing $test_name" >&2
		exit 1
	fi
	cases=$((cases + 1))
done <"$manifest"
if [ "$cases" -eq 0 ]; then
	echo "regression-spine: manifest has no cases" >&2
	exit 1
fi
if [ "$contract_only" = true ]; then
	echo "regression-spine contract: OK ($cases cases)"
	exit 0
fi

test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-regression-spine.XXXXXX")"
trap 'rm -rf "$test_root"' EXIT HUP INT TERM
killed=0
while IFS=$'\t' read -r id fix mutation_patch package test_name expected_failure extra; do
	case "$id" in "" | \#*) continue ;; esac
	case_root="$test_root/$id"
	log_file="$test_root/$id.log"
	mkdir -p "$case_root"
	git -C "$repo_root" archive "$candidate" | tar -xf - -C "$case_root"

	# The focused spine can be uncommitted while it is first introduced. Overlay
	# only that file; all other tests and production stay at the same committed
	# candidate so unrelated work in a shared checkout cannot create false kills.
	spine_test=internal/daemon/regression_spine_test.go
	if [ -f "$repo_root/$spine_test" ]; then
		cp "$repo_root/$spine_test" "$case_root/$spine_test"
	fi

	(cd "$case_root" && git apply "$repo_root/$mutation_patch") || {
		echo "regression-spine: $id historical mutation no longer applies" >&2
		exit 1
	}

	set +e
	(cd "$case_root" && go test -count=1 -run "^${test_name}$" "$package") >"$log_file" 2>&1
	status=$?
	set -e
	if [ "$status" -eq 0 ]; then
		echo "regression-spine: SURVIVED $id ($test_name passed historical regression)" >&2
		exit 1
	fi
	if ! grep -Fq -- "--- FAIL: $expected_failure" "$log_file"; then
		echo "regression-spine: $id failed for an unexpected reason" >&2
		sed -n '1,120p' "$log_file" >&2
		exit 1
	fi
	echo "regression-spine: KILLED $id via $expected_failure"
	killed=$((killed + 1))
done <"$manifest"

echo "regression-spine: OK ($killed/$cases historical regressions killed)"
