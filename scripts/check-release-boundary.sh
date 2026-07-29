#!/usr/bin/env bash

# Verify that release publication authority stays in the guarded internal
# Makefile targets. Packaging helpers and Actions workflows may assemble or
# verify artifacts, but they must not grow an independent tag/push/release
# path. Canonical shape: `release` (worktree orchestrator, owns no
# publication command) → `_release-run` (pipeline body: tag, tag push,
# plugin tag) → `_release-publish` (gh release create). Both internal
# targets must reject top-level invocation and stay out of `make help`.

set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
failure=0

if grep -Eq '^\.PHONY:.*(^|[[:space:]])release-publish([[:space:]]|$)' "$root/Makefile"; then
	echo "check-release-boundary: public release-publish target must not appear in .PHONY" >&2
	failure=1
fi

check_file() {
	local file="$1" line number text
	while IFS=: read -r number text; do
		trimmed="${text#"${text%%[![:space:]]*}"}"
		case "$trimmed" in
			""|\#*) continue ;;
		esac
		printf 'check-release-boundary: forbidden publication command in %s:%s: %s\n' \
			"$file" "$number" "$trimmed" >&2
		failure=1
	done < <(
		grep -En \
			'(^|[;&|[:space:]])(git[[:space:]]+(tag|push)|gh[[:space:]]+release[[:space:]]+create|claude[[:space:]]+plugin[[:space:]]+tag)([;&|[:space:]]|$)' \
			"$file" || true
	)
}

while IFS= read -r file; do
	check_file "$file"
done < <(
	find "$root/scripts" "$root/.github/workflows" -type f \
		\( -name '*.sh' -o -name '*.yml' -o -name '*.yaml' \) \
		! -name '*_test.sh' \
		! -name 'check-release-boundary.sh' \
		-print
)

target=""
run_seen=0
publish_seen=0
run_guard_makelevel=0
run_guard_entry=0
publish_guard_makelevel=0
publish_guard_entry=0
run_called_from_release=0
publish_called_from_run=0
while IFS= read -r line; do
	if [[ "$line" =~ ^([^[:space:]#][^:]*)\: ]]; then
		target="${BASH_REMATCH[1]}"
		if [ "$target" = "release-publish" ]; then
			printf 'check-release-boundary: public release-publish target is forbidden; GitHub publication must stay internal to release\n' >&2
			failure=1
		fi
		if { [ "$target" = "_release-publish" ] || [ "$target" = "_release-run" ]; } && [[ "$line" == *"##"* ]]; then
			printf 'check-release-boundary: %s must not be advertised by make help\n' "$target" >&2
			failure=1
		fi
	fi
	trimmed="${line#"${line%%[![:space:]]*}"}"
	case "$trimmed" in
		""|\#*|@\#*) continue ;;
	esac

	if [ "$target" = "_release-publish" ]; then
		[[ "$trimmed" == *'$(MAKELEVEL)'* ]] && publish_guard_makelevel=1
		[[ "$trimmed" == *'$(RELEASE_PIPELINE_ENTRY)'* ]] && publish_guard_entry=1
	fi
	if [ "$target" = "_release-run" ]; then
		[[ "$trimmed" == *'$(MAKELEVEL)'* ]] && run_guard_makelevel=1
		[[ "$trimmed" == *'$(RELEASE_PIPELINE_ENTRY)'* ]] && run_guard_entry=1
	fi
	if printf '%s\n' "$trimmed" | grep -Eq '(^|[;&|[:space:]])(\$\(MAKE\)|make)([[:space:]][^[:space:]]+)*[[:space:]]_release-publish([;&|[:space:]]|$)'; then
		if [ "$target" = "_release-run" ]; then
			publish_called_from_run=1
		else
			printf 'check-release-boundary: target %q may not invoke _release-publish\n' "$target" >&2
			failure=1
		fi
	fi
	if printf '%s\n' "$trimmed" | grep -Eq '(^|[;&|[:space:]])(\$\(MAKE\)|make)([[:space:]][^[:space:]]+)*[[:space:]]_release-run([;&|[:space:]]|$)'; then
		if [ "$target" = "release" ]; then
			run_called_from_release=1
		else
			printf 'check-release-boundary: target %q may not invoke _release-run\n' "$target" >&2
			failure=1
		fi
	fi
	if printf '%s\n' "$trimmed" | grep -Eq '(^|[;&|[:space:]])(git[[:space:]]+(tag|push)|gh[[:space:]]+release[[:space:]]+create|claude[[:space:]]+plugin[[:space:]]+tag)([;&|[:space:]]|$)'; then
		case "$target" in
			_release-run)
				run_seen=1
				;;
			_release-publish)
				publish_seen=1
				;;
			*)
				printf 'check-release-boundary: Makefile target %q owns a forbidden publication command: %s\n' \
					"$target" "$trimmed" >&2
				failure=1
				;;
		esac
	fi
done < "$root/Makefile"

if [ "$run_seen" -eq 1 ]; then
	if [ "$run_guard_makelevel" -ne 1 ] || [ "$run_guard_entry" -ne 1 ]; then
		printf 'check-release-boundary: _release-run must reject top-level calls using MAKELEVEL and RELEASE_PIPELINE_ENTRY guards\n' >&2
		failure=1
	fi
	if [ "$run_called_from_release" -ne 1 ]; then
		printf 'check-release-boundary: _release-run must be reachable from the canonical release target\n' >&2
		failure=1
	fi
fi
if [ "$publish_seen" -eq 1 ]; then
	if [ "$publish_guard_makelevel" -ne 1 ] || [ "$publish_guard_entry" -ne 1 ]; then
		printf 'check-release-boundary: _release-publish must reject top-level calls using MAKELEVEL and RELEASE_PIPELINE_ENTRY guards\n' >&2
		failure=1
	fi
	if [ "$publish_called_from_run" -ne 1 ]; then
		printf 'check-release-boundary: _release-publish must be reachable from _release-run\n' >&2
		failure=1
	fi
fi

if [ "$failure" -ne 0 ]; then
	exit 1
fi
echo "check-release-boundary: OK"
