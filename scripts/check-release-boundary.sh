#!/usr/bin/env bash

# Verify that release publication authority stays in the guarded internal
# verify artifacts, but they must not grow an independent tag/push/release
# path. Canonical shape: `release` (worktree orchestrator, owns no
# publication command) → `_release-run` (pipeline body: tag, tag push,
# plugin tag) → `_release-publish` (gh release create), plus the post-tag
# recovery lane `release-resume` (worktree orchestrator, owns no
# publication command) → `_release-resume-run` (exact-SHA Actions
# re-verification, then idempotent re-entry: plugin tag, may re-invoke
# `_release-publish`). Every internal target must reject top-level invocation

set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
failure=0

count_literal() {
	local haystack="$1" needle="$2" count=0
	while [[ "$haystack" == *"$needle"* ]]; do
		haystack="${haystack#*"$needle"}"
		count=$((count + 1))
	done
	printf '%s\n' "$count"
}

require_line_count() {
	[ "$(grep -Fxc "$2" "$1")" -eq "$3" ] && return
	echo "check-release-boundary: $4" >&2
	failure=1
}

inspect_fail_closed_final_gate() {
	local gate="$1"
	awk -v gate="$gate" '
		$0 == "\t@$(MAKE) " gate " || { \\" {
			start = NR
			if ((getline cleanup) > 0 &&
				cleanup == "\t\tgit tag -d $(RELEASE_VERSION) >/dev/null 2>&1; \\" &&
				(getline fail) > 0 &&
				fail == "\t\texit 1; \\" &&
				(getline closing) > 0 &&
				closing == "\t}") {
				count++
				if (first == 0) {
					first = start
				}
			}
		}
		END {
			printf "%d:%d\n", count + 0, first + 0
		}
	' "$root/Makefile"
}

validate_make_context_guards() {
	local guarded_target="$1" data make_count make_position makeflags_count
	local makeflags_position unsafe_count unsafe_position makefile_list_count
	local makefile_list_position makefiles_count makefiles_position
	local singleton_count singleton_position canonical_count canonical_position
	local pinned_count pinned_position
	data="$(
		awk -v target="$guarded_target" '
			$0 ~ ("^" target "[[:space:]]*:") {
				in_target = 1
				next
			}
			in_target && $0 ~ /^[^[:space:]#][^:]*:/ {
				exit
			}
			in_target {
				line = $0
				sub(/^[[:space:]]+/, "", line)
				if (line == "" || line ~ /^#/) {
					next
				}
				position++
				if (line == "$(if $(filter default,$(origin MAKE)),,$(error " target ": MAKE must not be overridden))") {
					make_count++
					make_position = position
				}
				if (line == "$(if $(filter file,$(origin MAKEFLAGS)),,$(error " target ": MAKEFLAGS must not be overridden))") {
					makeflags_count++
					makeflags_position = position
				}
				if (line == "$(if $(release_unsafe_makeflags),$(error " target ": unsafe Make flags are forbidden),)") {
					unsafe_count++
					unsafe_position = position
				}
				if (line == "$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error " target ": MAKEFILE_LIST must not be overridden))") {
					makefile_list_count++
					makefile_list_position = position
				}
				if (line == "$(if $(strip $(MAKEFILES)),$(error " target ": MAKEFILES must be empty),)") {
					makefiles_count++
					makefiles_position = position
				}
				if (line == "$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error " target ": exactly one makefile is required))") {
					singleton_count++
					singleton_position = position
				}
				if (line == "$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error " target ": only the canonical Makefile is allowed))") {
					canonical_count++
					canonical_position = position
				}
				if (line == "$(if $(release_overridden_vars),$(error " target ": release variables must not be overridden: $(release_overridden_vars)),)") {
					pinned_count++
					pinned_position = position
				}
			}
			END {
				printf "%d:%d:%d:%d:%d:%d:%d:%d:%d:%d:%d:%d:%d:%d:%d:%d\n",
					make_count + 0, make_position + 0,
					makeflags_count + 0, makeflags_position + 0,
					unsafe_count + 0, unsafe_position + 0,
					makefile_list_count + 0, makefile_list_position + 0,
					makefiles_count + 0, makefiles_position + 0,
					singleton_count + 0, singleton_position + 0,
					canonical_count + 0, canonical_position + 0,
					pinned_count + 0, pinned_position + 0
			}
		' "$root/Makefile"
	)"
	IFS=: read -r make_count make_position makeflags_count makeflags_position \
		unsafe_count unsafe_position makefile_list_count makefile_list_position \
		makefiles_count makefiles_position singleton_count singleton_position \
		canonical_count canonical_position pinned_count pinned_position <<< "$data"
	if [ "$make_count" -ne 1 ] || [ "$make_position" -ne 1 ] \
		|| [ "$makeflags_count" -ne 1 ] || [ "$makeflags_position" -ne 2 ] \
		|| [ "$unsafe_count" -ne 1 ] || [ "$unsafe_position" -ne 3 ] \
		|| [ "$makefile_list_count" -ne 1 ] || [ "$makefile_list_position" -ne 4 ] \
		|| [ "$makefiles_count" -ne 1 ] || [ "$makefiles_position" -ne 5 ] \
		|| [ "$singleton_count" -ne 1 ] || [ "$singleton_position" -ne 6 ] \
		|| [ "$canonical_count" -ne 1 ] || [ "$canonical_position" -ne 7 ] \
		|| [ "$pinned_count" -ne 1 ] || [ "$pinned_position" -ne 8 ]; then
		printf 'check-release-boundary: %s Make-context guards must be its first eight executable recipe lines\n' \
			"$guarded_target" >&2
		failure=1
	fi
}

if grep -Eq '^\.PHONY:.*(^|[[:space:]])release-publish([[:space:]]|$)' "$root/Makefile"; then
	echo "check-release-boundary: public release-publish target must not appear in .PHONY" >&2
	failure=1
fi

for required_phony in \
	release release-resume _release-run _release-resume-run _release-publish \
	release-origin-check release-ci-wait _release-ci-wait-historical \
	release-main-candidate-check release-source-candidate-check release-controller-source-check release-source-mode-check release-tag-candidate-check \
	release-plugin-tag-candidate-check release-github-candidate-check release-github-assets \
	release-payload-inventory-check release-registry-server registry-publish registry-publish-verify-first
do
	if ! grep -Eq "^\\.PHONY:.*(^|[[:space:]])${required_phony}([[:space:]]|$)" "$root/Makefile"; then
		printf 'check-release-boundary: release authority target %s must be literal .PHONY\n' \
			"$required_phony" >&2
		failure=1
	fi
done

if grep -Eq '^[[:space:]]*\.ONESHELL[[:space:]]*:' "$root/Makefile"; then
	echo "check-release-boundary: .ONESHELL is forbidden because release authority requires per-line failure propagation" >&2
	failure=1
fi
if grep -Eq '^[[:space:]]*\.IGNORE[[:space:]]*:' "$root/Makefile"; then
	echo "check-release-boundary: .IGNORE is forbidden because release failures must remain binding" >&2
	failure=1
fi

for required_make_context in \
	'override release_first_makeflag = $(firstword $(MAKEFLAGS))' \
	'override release_compact_makeflags = $(if $(filter --%,$(release_first_makeflag)),,$(if $(findstring =,$(release_first_makeflag)),,$(release_first_makeflag)))' \
	'override release_unsafe_makeflags = $(strip $(filter -i --ignore-errors -k --keep-going,$(MAKEFLAGS)) $(if $(findstring i,$(release_compact_makeflags)),i) $(if $(findstring k,$(release_compact_makeflags)),k) $(if $(findstring n,$(release_compact_makeflags)),n) $(if $(findstring t,$(release_compact_makeflags)),t))' \
	'override release_pinned_vars = RELEASE_TARGETS SPX_EXPECTED_REACHABLE SMOKE_STRICT MAIN_BRANCH GO_TAGS GO_BUILD_TAGS STRIP_LDFLAGS LDFLAGS RELEASE_SOURCE_MODE' \
	'override release_overridden_vars = $(strip $(foreach release_pinned_var,$(release_pinned_vars),$(if $(filter file,$(origin $(release_pinned_var))),,$(release_pinned_var))))'
do
	if ! grep -Fqx "$required_make_context" "$root/Makefile"; then
		printf 'check-release-boundary: missing exact release Make-context authority: %s\n' \
			"$required_make_context" >&2
		failure=1
	fi
done
if ! grep -Fqx \
	'RELEASE_TARGETS = darwin-arm64 darwin-amd64 linux-amd64 linux-arm64' \
	"$root/Makefile"; then
	echo "check-release-boundary: release asset target inventory must stay exact" >&2
	failure=1
fi

# The published-inventory gate must not be able to drift away from the matrix
# it is proving. Both literals are exact, and this is where they are compared.
inventory_checker="$root/scripts/check-release-payload-inventory.sh"
if [ ! -x "$inventory_checker" ]; then
	echo "check-release-boundary: scripts/check-release-payload-inventory.sh must exist and be executable" >&2
	failure=1
else
	makefile_matrix="$(
		sed -n 's/^RELEASE_TARGETS = \(.*\)$/\1/p' "$root/Makefile"
	)"
	inventory_matrix="$(
		sed -n 's/^CANONICAL_RELEASE_TARGETS="\(.*\)"$/\1/p' "$inventory_checker"
	)"
	if [ -z "$inventory_matrix" ] || [ "$makefile_matrix" != "$inventory_matrix" ]; then
		printf 'check-release-boundary: published-inventory matrix [%s] must equal RELEASE_TARGETS [%s]\n' \
			"$inventory_matrix" "$makefile_matrix" >&2
		failure=1
	fi
fi
if ! grep -Fqx \
	'RELEASE_CONTROLLER_CONTRACT = release-controller-v1' \
	"$root/Makefile"; then
	echo "check-release-boundary: release recovery controller contract marker is missing" >&2
	failure=1
fi
if ! grep -Fqx \
	'MAIN_BRANCH ?= $(if $(filter v2.%,$(RELEASE_VERSION)),release/2.x,main)' \
	"$root/Makefile"; then
	echo "check-release-boundary: release line must map v2 to release/2.x and later majors to main" >&2
	failure=1
fi
for workflow in ci.yml pages-check.yml; do
	if ! grep -Eq 'branches: \[[^]]*release/2\.x' "$root/.github/workflows/$workflow"; then
		printf 'check-release-boundary: %s must run on the maintained release/2.x branch\n' "$workflow" >&2
		failure=1
	fi
done

pages_workflow="$root/.github/workflows/pages-check.yml"
pages_deploy="$root/.github/workflows/pages-deploy.yml"
pages_message='Pages must check both maintained branches and deploy only from the main-only workflow'
require_line_count "$pages_workflow" '    branches: [main, release/2.x]' 1 "$pages_message"
require_line_count "$pages_deploy" '    branches: [main]' 1 "$pages_message"
require_line_count "$pages_deploy" '    environment:' 1 "$pages_message"
require_line_count "$pages_deploy" '        uses: actions/deploy-pages@d6db90164ac5ed86f2b6aed7e0febac5b3c0c03e # v4' 1 "$pages_message"
if grep -Eq 'environment:|actions/(configure|upload|deploy)-pages' "$pages_workflow"; then
	echo "check-release-boundary: release/2.x Pages check must not own deployment authority" >&2
	failure=1
fi

registry_workflow="$root/.github/workflows/registry-publish.yml"
registry_message="registry exact-SHA proof must follow the tag's major branch"
require_line_count "$registry_workflow" '            v2.*) release_branch=release/2.x ;;' 1 "$registry_message"
require_line_count "$registry_workflow" '            *) release_branch=main ;;' 1 "$registry_message"
require_line_count "$registry_workflow" '          printf '\''release_branch=%s\n'\'' "$release_branch" >> "$GITHUB_OUTPUT"' 1 "$registry_message"
require_line_count "$registry_workflow" '          RELEASE_BRANCH: ${{ steps.tag.outputs.release_branch }}' 1 "$registry_message"
require_line_count "$registry_workflow" '            -sha "$release_sha" -branch "$RELEASE_BRANCH" -event push \' 1 "$registry_message"

# Every check below this point runs once per line of the Makefile and of every
# string here is byte-identical to the one grep -E was handed, so only the
# THE RIGHT-HAND SIDE OF `=~` MUST STAY AN UNQUOTED VARIABLE REFERENCE. Writing
# fails, nothing warns: every check below silently stops matching and the
# release boundary reports OK while verifying nothing.
re_publication_command='(^|[;&|[:space:]])(git[[:space:]]+(tag|push)|gh[[:space:]]+release[[:space:]]+(create|edit|upload)|claude[[:space:]]+plugin[[:space:]]+tag)([;&|[:space:]]|$)'
# The sole sanctioned asset upload. It lives in a script because the uploads
# run in parallel, and it is safe there only because it refuses any release
# that is not a staged draft — see scripts/upload-release-assets.sh.
upload_asset_command='gh release upload "$version" "$asset" --repo github.com/osauer/canary &'
upload_asset_script='upload-release-assets.sh'
re_local_full_gate='^@?\$\(MAKE\)([[:space:]]+-[^[:space:]]+)*[[:space:]]+(test|check|commit-check)([[:space:]]|$)'
re_release_smoke='^@?\$\(MAKE\)[[:space:]]+release-smoke([[:space:]]|$)'
re_main_push='^@?git[[:space:]]+push[[:space:]]+--no-follow-tags[[:space:]]+origin[[:space:]]+HEAD:\$\(MAIN_BRANCH\)[[:space:]]*$'
re_ci_wait='^@?\$\(MAKE\)[[:space:]]+release-ci-wait[[:space:]]*$'
re_main_candidate_check='^@?\$\(MAKE\)[[:space:]]+release-main-candidate-check[[:space:]]*$'
re_annotated_tag='^@?git[[:space:]]+tag[[:space:]]+-a[[:space:]]+\$\(RELEASE_VERSION\)[[:space:]]+-m[[:space:]]+"\$\$msg"[[:space:]]*$'
re_atomic_tag_push='^@?git[[:space:]]+push[[:space:]]+--no-follow-tags[[:space:]]+--atomic[[:space:]]+origin[[:space:]]+HEAD:\$\(MAIN_BRANCH\)[[:space:]]+\$\(RELEASE_VERSION\)[[:space:]]*$'
re_origin_check='^@?\$\(MAKE\)[[:space:]]+release-origin-check[[:space:]]*$'
re_tag_candidate_check='^@?\$\(MAKE\)[[:space:]]+release-tag-candidate-check[[:space:]]+RELEASE_VERSION=\$\(RELEASE_VERSION\)[[:space:]]*$'
re_plugin_tag_candidate_check='^@?\$\(MAKE\)[[:space:]]+release-plugin-tag-candidate-check[[:space:]]+RELEASE_VERSION=\$\(RELEASE_VERSION\)[[:space:]]*$'
re_plugin_push='^git[[:space:]]+push[[:space:]]+--no-follow-tags[[:space:]]+origin[[:space:]]+"\$\$plugin_ref"[[:space:]]*$'
re_github_candidate_check='^@?\$\(MAKE\)[[:space:]]+release-github-candidate-check[[:space:]]+RELEASE_VERSION=\$\(RELEASE_VERSION\)[[:space:]]*$'
re_registry_verify_fresh='^@?\$\(MAKE\)[[:space:]]+registry-publish-verify-first[[:space:]]+RELEASE_PIPELINE_ENTRY=release[[:space:]]+RELEASE_VERSION=\$\(RELEASE_VERSION\)[[:space:]]*$'
re_ci_wait_historical_call='^@?\$\(MAKE\)[[:space:]]+_release-ci-wait-historical[[:space:]]+RELEASE_PIPELINE_ENTRY=release-resume[[:space:]]*$'
re_registry_verify_resume='^@?\$\(MAKE\)[[:space:]]+registry-publish-verify-first[[:space:]]+RELEASE_PIPELINE_ENTRY=release-resume[[:space:]]+RELEASE_VERSION=\$\(RELEASE_VERSION\)[[:space:]]*$'
re_origin_script='^@?\./scripts/check-release-origin\.sh[[:space:]]*$'
re_invokes_release_publish='(^|[;&|[:space:]])(\$\(MAKE\)|make)([[:space:]][^[:space:]]+)*[[:space:]]_release-publish([;&|[:space:]]|$)'
re_invokes_release_run='(^|[;&|[:space:]])(\$\(MAKE\)|make)([[:space:]][^[:space:]]+)*[[:space:]]_release-run([;&|[:space:]]|$)'
re_invokes_release_resume_run='(^|[;&|[:space:]])(\$\(MAKE\)|make)([[:space:]][^[:space:]]+)*[[:space:]]_release-resume-run([;&|[:space:]]|$)'
re_invokes_ci_wait_historical='(^|[;&|[:space:]])(\$\(MAKE\)|make)([[:space:]][^[:space:]]+)*[[:space:]]_release-ci-wait-historical([;&|[:space:]]|$)'

check_file() {
	local file="$1" line_number=0 text trimmed code
	local is_uploader=0 uploader_command_count=0
	if [ "${file##*/}" = "$upload_asset_script" ]; then
		is_uploader=1
	fi
	while IFS= read -r text || [ -n "$text" ]; do
		line_number=$((line_number + 1))
		trimmed="${text#"${text%%[![:space:]]*}"}"
		code="${trimmed%%#*}"
		code="${code%"${code##*[![:space:]]}"}"
		[ -n "$code" ] || continue
		if ! [[ "$code" =~ $re_publication_command ]]; then
			continue
		fi
		if [ "$is_uploader" -eq 1 ] && [ "$code" = "$upload_asset_command" ]; then
			uploader_command_count=$((uploader_command_count + 1))
			continue
		fi
		printf 'check-release-boundary: forbidden publication command in %s:%s: %s\n' \
			"$file" "$line_number" "$code" >&2
		failure=1
	done < "$file"
	if [ "$is_uploader" -eq 1 ]; then
		if [ "$uploader_command_count" -ne 1 ]; then
			printf 'check-release-boundary: %s must contain exactly one sanctioned asset upload (found %s)\n' \
				"$file" "$uploader_command_count" >&2
			failure=1
		fi
		# The uploader's whole safety case is that it cannot touch a
		# published release. Pin the draft resolution and its exactly-one
		if ! grep -Fq 'select(.draft == true and .tag_name == \"$version\")' "$file" \
			|| ! grep -Fq 'expected exactly one staged draft' "$file"; then
			printf 'check-release-boundary: %s must resolve exactly one staged draft before uploading\n' \
				"$file" >&2
			failure=1
		fi
	fi
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
line_number=0
run_seen=0
publish_seen=0
resume_seen=0
run_target_seen=0
run_guard_makelevel=0
run_guard_entry=0
publish_guard_makelevel=0
publish_guard_entry=0
resume_guard_makelevel=0
resume_guard_entry=0
historical_guard_makelevel=0
historical_guard_entry=0
historical_release_sha_count=0
historical_waiter_sha_count=0
historical_head_sha_count=0
run_called_from_release=0
publish_called_from_run=0
resume_called_from_release_resume=0
historical_called_from_resume=0
historical_called_from_publish=0
historical_called_from_registry=0
historical_target_seen=0
origin_target_seen=0
origin_target_recipe_count=0
origin_target_command_count=0
tag_target_seen=0
tag_target_recipe_count=0
tag_target_origin_count=0
tag_target_command_count=0
source_target_seen=0
source_target_recipe_count=0
source_target_command_count=0
controller_source_target_seen=0
controller_source_target_recipe_count=0
controller_source_target_origin_count=0
controller_source_target_command_count=0
mode_source_target_seen=0
mode_source_target_recipe_count=0
mode_source_target_origin_count=0
mode_source_target_command_count=0
plugin_tag_target_seen=0
plugin_tag_target_recipe_count=0
plugin_tag_target_origin_count=0
plugin_tag_target_command_count=0
github_target_seen=0
github_target_recipe_count=0
github_target_origin_count=0
github_target_tag_count=0
github_target_command_count=0
github_assets_target_seen=0
github_assets_source_count=0
github_assets_hydrator_count=0
github_assets_candidate_count=0
registry_server_target_seen=0
registry_server_materializer_count=0
registry_server_template_count=0
registry_server_generator_count=0
registry_server_payload_count=0
registry_server_validate_count=0
registry_server_materializer_line=0
registry_server_template_line=0
registry_server_generator_line=0
registry_server_payload_line=0
registry_server_validate_line=0
registry_publish_target_seen=0
registry_verify_target_seen=0
release_early_push_count=0
publish_upload_count=0
publish_upload_line=0
publish_draft_verify_count=0
publish_draft_verify_line=0
publish_flip_count=0
publish_flip_line=0
run_main_push_count=0
run_ci_wait_count=0
run_main_check_count=0
run_tag_count=0
run_atomic_tag_push_count=0
run_origin_check_count=0
run_main_push_line=0
run_ci_wait_line1=0
run_ci_wait_line2=0
run_main_check_line1=0
run_main_check_line2=0
run_tag_line=0
run_atomic_tag_push_line=0
run_origin_check_line1=0
run_origin_check_line2=0
run_tag_candidate_count=0
run_tag_candidate_line1=0
run_tag_candidate_line2=0
run_plugin_tag_candidate_count=0
run_plugin_tag_candidate_line1=0
run_plugin_tag_candidate_line2=0
run_plugin_push_count=0
run_plugin_push_line=0
run_plugin_local_check_count=0
run_plugin_local_check_line=0
run_claude_plugin_tag_count=0
run_first_publication_line=0
run_github_publish_line=0
run_github_check_count=0
run_github_check_line=0
run_registry_line=0
run_plugin_check_count=0
run_plugin_check_line=0
run_release_smoke_line=0
run_release_smoke_count=0
run_release_smoke_pinned_count=0
run_payload_inventory_count=0
run_payload_inventory_line=0
run_local_full_gate_count=0
resume_ci_wait_count=0
resume_ci_wait_line1=0
resume_ci_wait_line2=0
resume_origin_check_count=0
resume_origin_check_line1=0
resume_origin_check_line2=0
resume_tag_candidate_count=0
resume_tag_candidate_line1=0
resume_tag_candidate_line2=0
resume_tag_candidate_line3=0
resume_plugin_tag_candidate_count=0
resume_plugin_tag_candidate_line1=0
resume_plugin_tag_candidate_line2=0
resume_plugin_push_count=0
resume_plugin_push_line=0
resume_plugin_local_check_count=0
resume_claude_plugin_tag_count=0
resume_controller_source_count=0
resume_static_contract_count=0
resume_github_view_count=0
resume_github_line=0
resume_github_publish_line=0
resume_github_assets_count=0
resume_github_assets_line=0
resume_binaries_count=0
resume_binaries_line=0
resume_existing_state_count=0
resume_absent_state_count=0
resume_state_dispatch_count=0
resume_github_check_count=0
resume_github_check_line=0
resume_registry_line=0
publish_payload_inventory_count=0
publish_payload_inventory_line=0
publish_ci_authority_count=0
publish_ci_authority_line=0
publish_tag_check_count=0
publish_tag_check_line=0
publish_direct_origin_count=0
publish_direct_origin_line=0
publish_create_line=0
publish_fresh_call_count=0
publish_resume_call_count=0
publish_resume_source_count=0
publish_changelog_gate_count=0
publish_tag_materializer_count=0
publish_changelog_path_count=0
publish_template_path_count=0
publish_renderer_count=0
publish_changelog_path_line=0
publish_template_path_line=0
publish_renderer_line=0
registry_publish_authority_count=0
registry_publish_source_count=0
registry_publish_static_count=0
registry_publish_origin_count=0
registry_publish_tag_count=0
registry_publish_plugin_tag_count=0
registry_publish_github_count=0
registry_publish_server_count=0
registry_publish_command_count=0
registry_publish_source_line=0
registry_publish_static_line=0
registry_publish_origin_line=0
registry_publish_authority_line=0
registry_publish_tag_line=0
registry_publish_plugin_tag_line=0
registry_publish_github_line=0
registry_publish_server_line=0
registry_publish_command_line=0
registry_verify_authority_count=0
registry_verify_source_count=0
registry_verify_static_count=0
registry_verify_origin_count=0
registry_verify_tag_count=0
registry_verify_plugin_tag_count=0
registry_verify_github_count=0
registry_verify_fallback_count=0
registry_verify_fallback_make_count=0
registry_verify_entry_count=0
registry_verify_server_count=0
registry_verify_expected_json_count=0
registry_verify_source_line=0
registry_verify_static_line=0
registry_verify_origin_line=0
registry_verify_authority_line=0
registry_verify_tag_line=0
registry_verify_plugin_tag_line=0
registry_verify_github_line=0
registry_verify_server_line=0
registry_verify_fallback_line=0
release_make_guard_count=0
release_makeflags_guard_count=0
release_unsafe_flags_guard_count=0
release_recipe_code_index=0
release_make_guard_position=0
release_makeflags_guard_position=0
release_unsafe_flags_guard_position=0
release_dispatch_line=0
resume_make_guard_count=0
resume_makeflags_guard_count=0
resume_unsafe_flags_guard_count=0
resume_recipe_code_index=0
resume_make_guard_position=0
resume_makeflags_guard_position=0
resume_unsafe_flags_guard_position=0
resume_dispatch_line=0
resume_controller_marker_count=0
resume_controller_worktree_count=0
resume_source_worktree_count=0
resume_source_dispatch_count=0
while IFS= read -r line; do
	line_number=$((line_number + 1))
	if [[ "$line" =~ ^([^[:space:]#][^:]*)\: ]]; then
		target="${BASH_REMATCH[1]}"
		if [ "$target" = "_release-run" ]; then
			run_target_seen=1
		fi
		if [ "$target" = "_release-ci-wait-historical" ]; then
			historical_target_seen=1
		fi
		if [ "$target" = "release-origin-check" ]; then
			origin_target_seen=$((origin_target_seen + 1))
		fi
		if [ "$target" = "release-tag-candidate-check" ]; then
			tag_target_seen=$((tag_target_seen + 1))
		fi
		if [ "$target" = "release-source-candidate-check" ]; then
			source_target_seen=$((source_target_seen + 1))
		fi
		if [ "$target" = "release-controller-source-check" ]; then
			controller_source_target_seen=$((controller_source_target_seen + 1))
		fi
		if [ "$target" = "release-source-mode-check" ]; then
			mode_source_target_seen=$((mode_source_target_seen + 1))
		fi
		if [ "$target" = "release-plugin-tag-candidate-check" ]; then
			plugin_tag_target_seen=$((plugin_tag_target_seen + 1))
		fi
		if [ "$target" = "release-github-candidate-check" ]; then
			github_target_seen=$((github_target_seen + 1))
		fi
		if [ "$target" = "release-github-assets" ]; then
			github_assets_target_seen=$((github_assets_target_seen + 1))
		fi
		if [ "$target" = "release-registry-server" ]; then
			registry_server_target_seen=$((registry_server_target_seen + 1))
		fi
		if [ "$target" = "registry-publish" ]; then
			registry_publish_target_seen=$((registry_publish_target_seen + 1))
		fi
		if [ "$target" = "registry-publish-verify-first" ]; then
			registry_verify_target_seen=$((registry_verify_target_seen + 1))
		fi
		if [ "$target" = "release-publish" ]; then
			printf 'check-release-boundary: public release-publish target is forbidden; GitHub publication must stay internal to release\n' >&2
			failure=1
		fi
		if { [ "$target" = "_release-publish" ] || [ "$target" = "_release-run" ] || [ "$target" = "_release-resume-run" ] || [ "$target" = "_release-ci-wait-historical" ]; } && [[ "$line" == *"##"* ]]; then
			printf 'check-release-boundary: %s must not be advertised by make help\n' "$target" >&2
			failure=1
		fi
		continue
	fi
	trimmed="${line#"${line%%[![:space:]]*}"}"
	case "$trimmed" in
		""|\#*|@\#*) continue ;;
	esac
	code="${trimmed%%#*}"
	code="${code%"${code##*[![:space:]]}"}"
	[ -n "$code" ] || continue

	if [ "$target" = "_release-publish" ]; then
		[[ "$code" == *'$(MAKELEVEL)'* ]] && publish_guard_makelevel=1
		[[ "$code" == *'$(RELEASE_PIPELINE_ENTRY)'* ]] && publish_guard_entry=1
		if [[ "$code" == *"gh release create"* ]]; then
			repo_count=$(count_literal "$code" "--repo")
			verify_tag_count=$(count_literal "$code" "--verify-tag")
			draft_count=$(count_literal "$code" "--draft")
			if [ "$repo_count" -ne 1 ] \
				|| ! [[ "$code" =~ --repo[[:space:]]+github\.com/osauer/canary([[:space:]]|$) ]]; then
				printf 'check-release-boundary: _release-publish must use exactly one canonical --repo github.com/osauer/canary\n' >&2
				failure=1
			fi
			if [ "$verify_tag_count" -ne 1 ]; then
				printf 'check-release-boundary: _release-publish must use exactly one gh --verify-tag pin\n' >&2
				failure=1
			fi
			# Draft-then-publish (2026-08-04): the create must stage a
			# draft so the release is never public with a partial asset
			if [ "$draft_count" -ne 1 ]; then
				printf 'check-release-boundary: _release-publish must create the release as a staged --draft\n' >&2
				failure=1
			fi
		fi
		if [[ "$code" == *"gh release create"* ]]; then
			publish_create_line="$line_number"
		fi
		if [ "$code" = './scripts/upload-release-assets.sh "$(RELEASE_VERSION)" $$assets $(DIST_DIR)/SHA256SUMS $(DIST_DIR)/SHA256SUMS.asc && \' ]; then
			publish_upload_count=$((publish_upload_count + 1))
			publish_upload_line="$line_number"
		fi
		if [ "$code" = 'CHECK_GITHUB_RELEASE_STAGE=draft ./scripts/check-github-release.sh "$(RELEASE_VERSION)" "$(DIST_DIR)" && \' ]; then
			publish_draft_verify_count=$((publish_draft_verify_count + 1))
			publish_draft_verify_line="$line_number"
		fi
		if [ "$code" = 'gh release edit $(RELEASE_VERSION) --repo github.com/osauer/canary --draft=false --latest' ]; then
			publish_flip_count=$((publish_flip_count + 1))
			publish_flip_line="$line_number"
		fi
		if [ "$code" = './scripts/check-release-origin.sh && \' ]; then
			publish_direct_origin_count=$((publish_direct_origin_count + 1))
			publish_direct_origin_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			publish_payload_inventory_count=$((publish_payload_inventory_count + 1))
			publish_payload_inventory_line="$line_number"
		fi
		if [ "$code" = '$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) release-ci-wait,$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume)' ]; then
			publish_ci_authority_count=$((publish_ci_authority_count + 1))
			publish_ci_authority_line="$line_number"
			historical_called_from_publish=1
		fi
		if [ "$code" = './scripts/check-release-tag.sh "$(RELEASE_VERSION)" && \' ]; then
			publish_tag_check_count=$((publish_tag_check_count + 1))
			publish_tag_check_line="$line_number"
		fi
		[ "$code" = '$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) changelog-lint RELEASE_VERSION=$(RELEASE_VERSION),$(MAKE) changelog-lint-historical RELEASE_VERSION=$(RELEASE_VERSION) RELEASE_SOURCE_DIR="$(RELEASE_SOURCE_DIR)")' ] \
			&& publish_changelog_gate_count=$((publish_changelog_gate_count + 1))
		[ "$code" = 'python3 ./scripts/materialize-release-tag-file.py \' ] \
			&& publish_tag_materializer_count=$((publish_tag_materializer_count + 1))
		if [ "$code" = '"$(RELEASE_VERSION)" CHANGELOG.md "$$changelog" && \' ]; then
			publish_changelog_path_count=$((publish_changelog_path_count + 1))
			publish_changelog_path_line="$line_number"
		fi
		if [ "$code" = '"$(RELEASE_VERSION)" .github/release-notes-template.md "$$template" && \' ]; then
			publish_template_path_count=$((publish_template_path_count + 1))
			publish_template_path_line="$line_number"
		fi
		if [ "$code" = './scripts/render-release-notes.sh "$(RELEASE_VERSION)" "$$changelog" "$$template" "$$notes" && \' ]; then
			publish_renderer_count=$((publish_renderer_count + 1))
			publish_renderer_line="$line_number"
		fi
	fi
	if [ "$target" = "release" ]; then
		release_recipe_code_index=$((release_recipe_code_index + 1))
		[ "$code" = '$(if $(filter default,$(origin MAKE)),,$(error release: MAKE must not be overridden))' ] \
			&& {
				release_make_guard_count=$((release_make_guard_count + 1))
				release_make_guard_position="$release_recipe_code_index"
			}
		[ "$code" = '$(if $(filter file,$(origin MAKEFLAGS)),,$(error release: MAKEFLAGS must not be overridden))' ] \
			&& {
				release_makeflags_guard_count=$((release_makeflags_guard_count + 1))
				release_makeflags_guard_position="$release_recipe_code_index"
			}
		[ "$code" = '$(if $(release_unsafe_makeflags),$(error release: unsafe Make flags are forbidden),)' ] \
			&& {
				release_unsafe_flags_guard_count=$((release_unsafe_flags_guard_count + 1))
				release_unsafe_flags_guard_position="$release_recipe_code_index"
			}
	fi
	if [ "$target" = "release-resume" ]; then
		resume_recipe_code_index=$((resume_recipe_code_index + 1))
		[ "$code" = '$(if $(filter default,$(origin MAKE)),,$(error release-resume: MAKE must not be overridden))' ] \
			&& {
				resume_make_guard_count=$((resume_make_guard_count + 1))
				resume_make_guard_position="$resume_recipe_code_index"
			}
		[ "$code" = '$(if $(filter file,$(origin MAKEFLAGS)),,$(error release-resume: MAKEFLAGS must not be overridden))' ] \
			&& {
				resume_makeflags_guard_count=$((resume_makeflags_guard_count + 1))
				resume_makeflags_guard_position="$resume_recipe_code_index"
			}
		[ "$code" = '$(if $(release_unsafe_makeflags),$(error release-resume: unsafe Make flags are forbidden),)' ] \
			&& {
				resume_unsafe_flags_guard_count=$((resume_unsafe_flags_guard_count + 1))
				resume_unsafe_flags_guard_position="$resume_recipe_code_index"
			}
		[[ "$code" == *'git cat-file blob "$$controller_sha:Makefile"'* ]] \
			&& [[ "$code" == *"grep -Fqx 'RELEASE_CONTROLLER_CONTRACT = release-controller-v1'"* ]] \
			&& resume_controller_marker_count=$((resume_controller_marker_count + 1))
		[ "$code" = 'git worktree add --detach "$$controller_wt" "$$controller_sha" || exit 1; \' ] \
			&& resume_controller_worktree_count=$((resume_controller_worktree_count + 1))
		[ "$code" = 'if ! git worktree add --detach "$$source_wt" "$$release_sha"; then \' ] \
			&& resume_source_worktree_count=$((resume_source_worktree_count + 1))
		if [[ "$code" == *'$(MAKE) -C "$$controller_wt" _release-resume-run'* ]] \
			&& [[ "$code" == *'RELEASE_SOURCE_DIR="$$source_wt"'* ]]; then
			resume_source_dispatch_count=$((resume_source_dispatch_count + 1))
		fi
	fi
	if [ "$target" = "_release-run" ]; then
		[[ "$code" == *'$(MAKELEVEL)'* ]] && run_guard_makelevel=1
		[[ "$code" == *'$(RELEASE_PIPELINE_ENTRY)'* ]] && run_guard_entry=1
		if [ "$code" = '$(MAKE) plugin-check' ]; then
			run_plugin_check_count=$((run_plugin_check_count + 1))
			run_plugin_check_line="$line_number"
		fi
		if [[ "$code" =~ $re_local_full_gate ]]; then
			run_local_full_gate_count=$((run_local_full_gate_count + 1))
		fi
		if [[ "$code" =~ $re_release_smoke ]]; then
			run_release_smoke_line="$line_number"
			run_release_smoke_count=$((run_release_smoke_count + 1))
		fi
		if [ "$code" = '$(MAKE) release-smoke RELEASE_VERSION=$(RELEASE_VERSION) SMOKE_STRICT=1 SPX_EXPECTED_REACHABLE=1' ]; then
			run_release_smoke_pinned_count=$((run_release_smoke_pinned_count + 1))
		fi
		if [ "$code" = '@$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION) || { \' ]; then
			run_payload_inventory_count=$((run_payload_inventory_count + 1))
			run_payload_inventory_line="$line_number"
		fi
		if [[ "$code" =~ $re_main_push ]]; then
			run_main_push_count=$((run_main_push_count + 1))
			run_main_push_line="$line_number"
		fi
		if [[ "$code" =~ $re_ci_wait ]] \
			|| [ "$code" = '@$(MAKE) release-ci-wait || { \' ]; then
			run_ci_wait_count=$((run_ci_wait_count + 1))
			if [ "$run_ci_wait_count" -eq 1 ]; then
				run_ci_wait_line1="$line_number"
			elif [ "$run_ci_wait_count" -eq 2 ]; then
				run_ci_wait_line2="$line_number"
			fi
		fi
		if [[ "$code" =~ $re_main_candidate_check ]] \
			|| [ "$code" = '@$(MAKE) release-main-candidate-check || { \' ]; then
			run_main_check_count=$((run_main_check_count + 1))
			if [ "$run_main_check_count" -eq 1 ]; then
				run_main_check_line1="$line_number"
			elif [ "$run_main_check_count" -eq 2 ]; then
				run_main_check_line2="$line_number"
			fi
		fi
		if [[ "$code" =~ $re_annotated_tag ]]; then
			run_tag_count=$((run_tag_count + 1))
			run_tag_line="$line_number"
		fi
		if [[ "$code" =~ $re_atomic_tag_push ]]; then
			run_atomic_tag_push_count=$((run_atomic_tag_push_count + 1))
			run_atomic_tag_push_line="$line_number"
		fi
		if [[ "$code" =~ $re_origin_check ]]; then
			run_origin_check_count=$((run_origin_check_count + 1))
			if [ "$run_origin_check_count" -eq 1 ]; then
				run_origin_check_line1="$line_number"
			elif [ "$run_origin_check_count" -eq 2 ]; then
				run_origin_check_line2="$line_number"
			fi
		fi
		if [[ "$code" =~ $re_tag_candidate_check ]]; then
			run_tag_candidate_count=$((run_tag_candidate_count + 1))
			if [ "$run_tag_candidate_count" -eq 1 ]; then
				run_tag_candidate_line1="$line_number"
			elif [ "$run_tag_candidate_count" -eq 2 ]; then
				run_tag_candidate_line2="$line_number"
			fi
		fi
		if [[ "$code" =~ $re_plugin_tag_candidate_check ]]; then
			run_plugin_tag_candidate_count=$((run_plugin_tag_candidate_count + 1))
			if [ "$run_plugin_tag_candidate_count" -eq 1 ]; then
				run_plugin_tag_candidate_line1="$line_number"
			elif [ "$run_plugin_tag_candidate_count" -eq 2 ]; then
				run_plugin_tag_candidate_line2="$line_number"
			fi
		fi
		if [[ "$code" =~ $re_plugin_push ]]; then
			run_plugin_push_count=$((run_plugin_push_count + 1))
			run_plugin_push_line="$line_number"
		fi
		if [ "$code" = './scripts/check-release-tag.sh --plugin-local "$(RELEASE_VERSION)" && \' ]; then
			run_plugin_local_check_count=$((run_plugin_local_check_count + 1))
			run_plugin_local_check_line="$line_number"
		fi
		if [[ "$code" == "claude plugin tag ."* ]]; then
			run_claude_plugin_tag_count=$((run_claude_plugin_tag_count + 1))
		fi
		if [[ "$code" =~ $re_github_candidate_check ]]; then
			run_github_check_count=$((run_github_check_count + 1))
			run_github_check_line="$line_number"
		fi
		if [[ "$code" =~ $re_registry_verify_fresh ]]; then
			run_registry_line="$line_number"
		fi
		if [[ "$code" == *"claude plugin tag"* ]] && [ "$run_first_publication_line" -eq 0 ]; then
			run_first_publication_line="$line_number"
		fi
	fi
	if [ "$target" = "_release-resume-run" ]; then
		[[ "$code" == *'$(MAKELEVEL)'* ]] && resume_guard_makelevel=1
		[[ "$code" == *'$(RELEASE_PIPELINE_ENTRY)'* ]] && resume_guard_entry=1
		if [[ "$code" =~ $re_ci_wait_historical_call ]]; then
			resume_ci_wait_count=$((resume_ci_wait_count + 1))
			if [ "$resume_ci_wait_count" -eq 1 ]; then
				resume_ci_wait_line1="$line_number"
			elif [ "$resume_ci_wait_count" -eq 2 ]; then
				resume_ci_wait_line2="$line_number"
			fi
			historical_called_from_resume=1
		fi
		if [ "$code" = '@release_state=$$(./scripts/github-release-state.sh "$(RELEASE_VERSION)") || exit 1; \' ]; then
			resume_github_view_count=$((resume_github_view_count + 1))
			resume_github_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION); \' ]; then
			resume_github_assets_count=$((resume_github_assets_count + 1))
			resume_github_assets_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-binaries RELEASE_VERSION=$(RELEASE_VERSION); \' ]; then
			resume_binaries_count=$((resume_binaries_count + 1))
			resume_binaries_line="$line_number"
		fi
		[ "$code" = 'printf '\''%s\n'\'' existing >"$(DIST_DIR)/.canary-resume-github-state"; \' ] \
			&& resume_existing_state_count=$((resume_existing_state_count + 1))
		[ "$code" = 'printf '\''%s\n'\'' absent >"$(DIST_DIR)/.canary-resume-github-state"; \' ] \
			&& resume_absent_state_count=$((resume_absent_state_count + 1))
		[ "$code" = '@resume_state=$$(cat "$(DIST_DIR)/.canary-resume-github-state" 2>/dev/null || true); \' ] \
			&& resume_state_dispatch_count=$((resume_state_dispatch_count + 1))
		if [[ "$code" == 'claude plugin tag "$(RELEASE_SOURCE_DIR)"'* ]]; then
			resume_claude_plugin_tag_count=$((resume_claude_plugin_tag_count + 1))
		fi
		[ "$code" = '$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)' ] \
			&& resume_controller_source_count=$((resume_controller_source_count + 1))
		[ "$code" = '@./scripts/check-release-ci-contract.sh' ] \
			&& resume_static_contract_count=$((resume_static_contract_count + 1))
		if [ "$code" = './scripts/check-release-tag.sh --plugin-local "$(RELEASE_VERSION)" || exit 1; \' ]; then
			resume_plugin_local_check_count=$((resume_plugin_local_check_count + 1))
		fi
		if [ "$code" = 'git push --no-follow-tags origin "$$plugin_ref"; \' ]; then
			resume_plugin_push_count=$((resume_plugin_push_count + 1))
			resume_plugin_push_line="$line_number"
		fi
		if [[ "$code" =~ $re_origin_check ]]; then
			resume_origin_check_count=$((resume_origin_check_count + 1))
			if [ "$resume_origin_check_count" -eq 1 ]; then
				resume_origin_check_line1="$line_number"
			elif [ "$resume_origin_check_count" -eq 2 ]; then
				resume_origin_check_line2="$line_number"
			fi
		fi
		if [[ "$code" =~ $re_tag_candidate_check ]]; then
			resume_tag_candidate_count=$((resume_tag_candidate_count + 1))
			if [ "$resume_tag_candidate_count" -eq 1 ]; then
				resume_tag_candidate_line1="$line_number"
			elif [ "$resume_tag_candidate_count" -eq 2 ]; then
				resume_tag_candidate_line2="$line_number"
			elif [ "$resume_tag_candidate_count" -eq 3 ]; then
				resume_tag_candidate_line3="$line_number"
			fi
		fi
		if [[ "$code" =~ $re_plugin_tag_candidate_check ]]; then
			resume_plugin_tag_candidate_count=$((resume_plugin_tag_candidate_count + 1))
			if [ "$resume_plugin_tag_candidate_count" -eq 1 ]; then
				resume_plugin_tag_candidate_line1="$line_number"
			elif [ "$resume_plugin_tag_candidate_count" -eq 2 ]; then
				resume_plugin_tag_candidate_line2="$line_number"
			fi
		fi
		if [[ "$code" =~ $re_github_candidate_check ]]; then
			resume_github_check_count=$((resume_github_check_count + 1))
			resume_github_check_line="$line_number"
		fi
		if [[ "$code" =~ $re_registry_verify_resume ]]; then
			resume_registry_line="$line_number"
		fi
	fi
	if [ "$target" = "_release-ci-wait-historical" ]; then
		[[ "$code" == *'$(MAKELEVEL)'* ]] && historical_guard_makelevel=1
		[[ "$code" == *'$(RELEASE_PIPELINE_ENTRY)'* ]] && historical_guard_entry=1
		[ "$code" = '@release_sha=$$(git rev-parse --verify "refs/tags/$(RELEASE_VERSION)^{commit}") || { \' ] \
			&& historical_release_sha_count=$((historical_release_sha_count + 1))
		[ "$code" = '-sha "$$release_sha" -branch "$(MAIN_BRANCH)" -event push \' ] \
			&& historical_waiter_sha_count=$((historical_waiter_sha_count + 1))
		[[ "$code" == *'git rev-parse HEAD'* ]] \
			&& historical_head_sha_count=$((historical_head_sha_count + 1))
	fi
	if [ "$target" = "release-origin-check" ]; then
		origin_target_recipe_count=$((origin_target_recipe_count + 1))
		if [[ "$code" =~ $re_origin_script ]]; then
			origin_target_command_count=$((origin_target_command_count + 1))
		fi
	fi
	if [ "$target" = "release-tag-candidate-check" ]; then
		tag_target_recipe_count=$((tag_target_recipe_count + 1))
		if [[ "$code" =~ $re_origin_check ]]; then
			tag_target_origin_count=$((tag_target_origin_count + 1))
		fi
		if [ "$code" = '@./scripts/check-release-tag.sh "$(RELEASE_VERSION)"' ] \
			|| [ "$code" = './scripts/check-release-tag.sh "$(RELEASE_VERSION)"' ]; then
			tag_target_command_count=$((tag_target_command_count + 1))
		fi
	fi
	if [ "$target" = "release-source-candidate-check" ]; then
		source_target_recipe_count=$((source_target_recipe_count + 1))
		if [ "$code" = '@./scripts/check-release-source.sh --mode tag "$(RELEASE_VERSION)"' ] \
			|| [ "$code" = './scripts/check-release-source.sh --mode tag "$(RELEASE_VERSION)"' ]; then
			source_target_command_count=$((source_target_command_count + 1))
		fi
	fi
	if [ "$target" = "release-controller-source-check" ]; then
		controller_source_target_recipe_count=$((controller_source_target_recipe_count + 1))
		if [ "$code" = '$(MAKE) release-origin-check' ]; then
			controller_source_target_origin_count=$((controller_source_target_origin_count + 1))
		fi
		if [ "$code" = '@./scripts/check-release-source.sh --mode controller "$(RELEASE_VERSION)"' ] \
			|| [ "$code" = './scripts/check-release-source.sh --mode controller "$(RELEASE_VERSION)"' ]; then
			controller_source_target_command_count=$((controller_source_target_command_count + 1))
		fi
	fi
	if [ "$target" = "release-source-mode-check" ]; then
		mode_source_target_recipe_count=$((mode_source_target_recipe_count + 1))
		if [ "$code" = '$(MAKE) release-origin-check' ]; then
			mode_source_target_origin_count=$((mode_source_target_origin_count + 1))
		fi
		if [ "$code" = '@./scripts/check-release-source.sh --mode "$(RELEASE_SOURCE_MODE)" "$(RELEASE_VERSION)"' ] \
			|| [ "$code" = './scripts/check-release-source.sh --mode "$(RELEASE_SOURCE_MODE)" "$(RELEASE_VERSION)"' ]; then
			mode_source_target_command_count=$((mode_source_target_command_count + 1))
		fi
	fi
	if [ "$target" = "release-plugin-tag-candidate-check" ]; then
		plugin_tag_target_recipe_count=$((plugin_tag_target_recipe_count + 1))
		if [[ "$code" =~ $re_origin_check ]]; then
			plugin_tag_target_origin_count=$((plugin_tag_target_origin_count + 1))
		fi
		if [ "$code" = '@./scripts/check-release-tag.sh --plugin "$(RELEASE_VERSION)"' ] \
			|| [ "$code" = './scripts/check-release-tag.sh --plugin "$(RELEASE_VERSION)"' ]; then
			plugin_tag_target_command_count=$((plugin_tag_target_command_count + 1))
		fi
	fi
	if [ "$target" = "release-github-candidate-check" ]; then
		github_target_recipe_count=$((github_target_recipe_count + 1))
		if [[ "$code" =~ $re_origin_check ]]; then
			github_target_origin_count=$((github_target_origin_count + 1))
		fi
		if [ "$code" = '$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			github_target_tag_count=$((github_target_tag_count + 1))
		fi
		if [ "$code" = '@./scripts/check-github-release.sh "$(RELEASE_VERSION)" "$(DIST_DIR)"' ] \
			|| [ "$code" = './scripts/check-github-release.sh "$(RELEASE_VERSION)" "$(DIST_DIR)"' ]; then
			github_target_command_count=$((github_target_command_count + 1))
		fi
	fi
	if [ "$target" = "release-github-assets" ]; then
		[ "$code" = '$(MAKE) release-source-mode-check RELEASE_VERSION=$(RELEASE_VERSION)' ] \
			&& github_assets_source_count=$((github_assets_source_count + 1))
		[ "$code" = '@./scripts/hydrate-github-release-assets.sh "$(RELEASE_VERSION)" "$(abspath $(DIST_DIR))"' ] \
			&& github_assets_hydrator_count=$((github_assets_hydrator_count + 1))
		[ "$code" = '$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)' ] \
			&& github_assets_candidate_count=$((github_assets_candidate_count + 1))
	fi
	if [ "$target" = "release-registry-server" ]; then
		if [ "$code" = 'python3 ./scripts/materialize-release-tag-file.py \' ]; then
			registry_server_materializer_count=$((registry_server_materializer_count + 1))
			registry_server_materializer_line="$line_number"
		fi
		if [ "$code" = '"$(RELEASE_VERSION)" server.json "$$template"; \' ]; then
			registry_server_template_count=$((registry_server_template_count + 1))
			registry_server_template_line="$line_number"
		fi
		if [ "$code" = 'go run ./scripts/release-registry-server "$(RELEASE_VERSION)" "$$template" \' ]; then
			registry_server_generator_count=$((registry_server_generator_count + 1))
			registry_server_generator_line="$line_number"
		fi
		if [ "$code" = '"$(DIST_DIR)/canary-$(RELEASE_VERSION).mcpb" "$(DIST_DIR)/server.json"; \' ]; then
			registry_server_payload_count=$((registry_server_payload_count + 1))
			registry_server_payload_line="$line_number"
		fi
		if [ "$code" = '$(MCP_PUBLISHER) validate "$(DIST_DIR)/server.json"' ]; then
			registry_server_validate_count=$((registry_server_validate_count + 1))
			registry_server_validate_line="$line_number"
		fi
	fi
	if [ "$target" = "registry-publish" ]; then
		if [ "$code" = '$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_publish_source_count=$((registry_publish_source_count + 1))
			registry_publish_source_line="$line_number"
		fi
		if [ "$code" = '@./scripts/check-release-ci-contract.sh' ]; then
			registry_publish_static_count=$((registry_publish_static_count + 1))
			registry_publish_static_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-origin-check' ]; then
			registry_publish_origin_count=$((registry_publish_origin_count + 1))
			registry_publish_origin_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_publish_authority_count=$((registry_publish_authority_count + 1))
			registry_publish_authority_line="$line_number"
			historical_called_from_registry=$((historical_called_from_registry + 1))
		fi
		if [ "$code" = '$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_publish_tag_count=$((registry_publish_tag_count + 1))
			registry_publish_tag_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_publish_plugin_tag_count=$((registry_publish_plugin_tag_count + 1))
			registry_publish_plugin_tag_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_publish_github_count=$((registry_publish_github_count + 1))
			registry_publish_github_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-registry-server RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_publish_server_count=$((registry_publish_server_count + 1))
			registry_publish_server_line="$line_number"
		fi
		if [ "$code" = './scripts/registry-publish-with-login.sh "$(MCP_PUBLISHER)" "$(DIST_DIR)/server.json"' ]; then
			registry_publish_command_count=$((registry_publish_command_count + 1))
			registry_publish_command_line="$line_number"
		fi
	fi
	if [ "$target" = "registry-publish-verify-first" ]; then
		if [ "$code" = '$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_verify_source_count=$((registry_verify_source_count + 1))
			registry_verify_source_line="$line_number"
		fi
		if [ "$code" = '@./scripts/check-release-ci-contract.sh' ]; then
			registry_verify_static_count=$((registry_verify_static_count + 1))
			registry_verify_static_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-origin-check' ]; then
			registry_verify_origin_count=$((registry_verify_origin_count + 1))
			registry_verify_origin_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_verify_authority_count=$((registry_verify_authority_count + 1))
			registry_verify_authority_line="$line_number"
			historical_called_from_registry=$((historical_called_from_registry + 1))
		fi
		if [ "$code" = '$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_verify_tag_count=$((registry_verify_tag_count + 1))
			registry_verify_tag_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_verify_plugin_tag_count=$((registry_verify_plugin_tag_count + 1))
			registry_verify_plugin_tag_line="$line_number"
		fi
		# The GitHub-asset proof is entry-conditional by design (2026-08-03):
		# the primary release path digest-verifies the just-uploaded local set
		# via release-github-candidate-check, while resume/recovery entries
		# must byte-hydrate because their local dist/ is untrusted. Only this
		# exact conditional line satisfies the proof slot.
		if [ "$code" = '$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION),$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION))' ]; then
			registry_verify_github_count=$((registry_verify_github_count + 1))
			registry_verify_github_line="$line_number"
		fi
		if [ "$code" = '$(MAKE) release-registry-server RELEASE_VERSION=$(RELEASE_VERSION)' ]; then
			registry_verify_server_count=$((registry_verify_server_count + 1))
			registry_verify_server_line="$line_number"
		fi
		if [ "$code" = '@./scripts/registry-publish-verify-first.sh "$(RELEASE_VERSION)" \' ]; then
			registry_verify_fallback_count=$((registry_verify_fallback_count + 1))
			registry_verify_fallback_line="$line_number"
		fi
		[ "$code" = '"$(DIST_DIR)/server.json" \' ] \
			&& registry_verify_expected_json_count=$((registry_verify_expected_json_count + 1))
		[ "$code" = 'make --no-print-directory registry-publish \' ] \
			&& registry_verify_fallback_make_count=$((registry_verify_fallback_make_count + 1))
		[[ "$code" == RELEASE_PIPELINE_ENTRY=* ]] \
			&& registry_verify_entry_count=$((registry_verify_entry_count + 1))
	fi
	if [[ "$code" == *"claude plugin tag"* ]] && [[ "$code" == *"--push"* ]]; then
		printf 'check-release-boundary: claude plugin tag --push is forbidden; create locally and push the exact ref with --no-follow-tags\n' >&2
		failure=1
	fi
	if [[ "$code" == *"registry-publish-with-login.sh"* ]] \
		&& ! { [ "$target" = "registry-publish" ] \
			&& [ "$code" = './scripts/registry-publish-with-login.sh "$(MCP_PUBLISHER)" "$(DIST_DIR)/server.json"' ]; }; then
		printf 'check-release-boundary: registry fallback publisher must be called only by guarded registry-publish\n' >&2
		failure=1
	fi
	if [[ "$code" =~ $re_invokes_release_publish ]]; then
		if [ "$target" = "_release-run" ] || [ "$target" = "_release-resume-run" ]; then
			publish_called_from_run=1
			if [ "$target" = "_release-run" ] \
				&& [[ "$code" == *'_release-publish RELEASE_PIPELINE_ENTRY=release RELEASE_VERSION=$(RELEASE_VERSION)'* ]]; then
				publish_fresh_call_count=$((publish_fresh_call_count + 1))
				run_github_publish_line="$line_number"
				if [ "$run_first_publication_line" -eq 0 ]; then
					run_first_publication_line="$line_number"
				fi
			fi
			if [ "$target" = "_release-resume-run" ] \
				&& [[ "$code" == *'_release-publish RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)'* ]]; then
				publish_resume_call_count=$((publish_resume_call_count + 1))
				resume_github_publish_line="$line_number"
				[[ "$code" == *'RELEASE_SOURCE_DIR="$(RELEASE_SOURCE_DIR)"'* ]] \
					&& publish_resume_source_count=$((publish_resume_source_count + 1))
			fi
		else
			printf 'check-release-boundary: target %q may not invoke _release-publish\n' "$target" >&2
			failure=1
		fi
	fi
	if [[ "$code" =~ $re_invokes_release_run ]]; then
		if [ "$target" = "release" ]; then
			run_called_from_release=1
			release_dispatch_line="$line_number"
		else
			printf 'check-release-boundary: target %q may not invoke _release-run\n' "$target" >&2
			failure=1
		fi
	fi
	if [[ "$code" =~ $re_invokes_release_resume_run ]]; then
		if [ "$target" = "release-resume" ]; then
			resume_called_from_release_resume=1
			resume_dispatch_line="$line_number"
		else
			printf 'check-release-boundary: target %q may not invoke _release-resume-run\n' "$target" >&2
			failure=1
		fi
	fi
	if [[ "$code" =~ $re_invokes_ci_wait_historical ]] \
		&& [ "$target" != "_release-resume-run" ] \
		&& ! { [ "$target" = "_release-publish" ] \
			&& [ "$code" = '$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) release-ci-wait,$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume)' ]; } \
		&& ! { { [ "$target" = "registry-publish" ] || [ "$target" = "registry-publish-verify-first" ]; } \
			&& [ "$code" = '$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)' ]; }; then
		printf 'check-release-boundary: target %q may not invoke _release-ci-wait-historical\n' "$target" >&2
		failure=1
	fi
	if [[ "$code" =~ $re_publication_command ]]; then
		case "$target" in
			release)
				# The release front door may land the candidate on
				# starts hosted CI ~90s earlier) — exactly that one push
				# shape and nothing else. Tags, releases, and plugin tags
				if [[ "$code" =~ $re_main_push ]]; then
					release_early_push_count=$((release_early_push_count + 1))
				else
					printf 'check-release-boundary: Makefile target %q owns a forbidden publication command: %s\n' \
						"$target" "$code" >&2
					failure=1
				fi
				;;
			_release-run)
				run_seen=1
				;;
			_release-publish)
				publish_seen=1
				;;
			_release-resume-run)
				resume_seen=1
				;;
			*)
				printf 'check-release-boundary: Makefile target %q owns a forbidden publication command: %s\n' \
					"$target" "$code" >&2
				failure=1
				;;
		esac
	fi
done < "$root/Makefile"

for guarded_target in \
	release release-resume _release-run _release-resume-run _release-publish \
	_release-ci-wait-historical registry-publish registry-publish-verify-first
do
	validate_make_context_guards "$guarded_target"
done

final_ci_block_data="$(inspect_fail_closed_final_gate release-ci-wait)"
final_ci_block_count="${final_ci_block_data%%:*}"
final_ci_block_line="${final_ci_block_data#*:}"
final_main_block_data="$(inspect_fail_closed_final_gate release-main-candidate-check)"
final_main_block_count="${final_main_block_data%%:*}"
final_main_block_line="${final_main_block_data#*:}"
inventory_block_data="$(inspect_fail_closed_final_gate 'release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION)')"
inventory_block_count="${inventory_block_data%%:*}"
inventory_block_line="${inventory_block_data#*:}"

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
if [ "$run_target_seen" -eq 1 ]; then
	if [ "$release_make_guard_count" -ne 1 ] \
		|| [ "$release_makeflags_guard_count" -ne 1 ] \
		|| [ "$release_unsafe_flags_guard_count" -ne 1 ]; then
		printf 'check-release-boundary: release must reject overridden MAKE/MAKEFLAGS and ignore-errors/keep-going flags at expansion time\n' >&2
		failure=1
	fi
	if [ "$release_make_guard_position" -ne 1 ] \
		|| [ "$release_makeflags_guard_position" -ne 2 ] \
		|| [ "$release_unsafe_flags_guard_position" -ne 3 ] \
		|| [ "$release_dispatch_line" -eq 0 ]; then
		printf 'check-release-boundary: release Make-context guards must be its first three executable recipe lines, before pipeline dispatch\n' >&2
		failure=1
	fi
	if [ "$resume_make_guard_count" -ne 1 ] \
		|| [ "$resume_makeflags_guard_count" -ne 1 ] \
		|| [ "$resume_unsafe_flags_guard_count" -ne 1 ]; then
		printf 'check-release-boundary: release-resume must reject overridden MAKE/MAKEFLAGS and ignore-errors/keep-going flags at expansion time\n' >&2
		failure=1
	fi
	if [ "$resume_make_guard_position" -ne 1 ] \
		|| [ "$resume_makeflags_guard_position" -ne 2 ] \
		|| [ "$resume_unsafe_flags_guard_position" -ne 3 ] \
		|| [ "$resume_dispatch_line" -eq 0 ]; then
		printf 'check-release-boundary: release-resume Make-context guards must be its first three executable recipe lines, before pipeline dispatch\n' >&2
		failure=1
	fi
	if [ "$resume_controller_marker_count" -ne 1 ] \
		|| [ "$resume_controller_worktree_count" -ne 1 ] \
		|| [ "$resume_source_worktree_count" -ne 1 ] \
		|| [ "$resume_source_dispatch_count" -ne 1 ]; then
		printf 'check-release-boundary: release-resume must run the committed current controller with a separate immutable tag-source worktree\n' >&2
		failure=1
	fi
	if [ "$origin_target_seen" -ne 1 ] || [ "$origin_target_recipe_count" -ne 1 ] \
		|| [ "$origin_target_command_count" -ne 1 ]; then
		printf 'check-release-boundary: release-origin-check must be exactly one direct ./scripts/check-release-origin.sh recipe\n' >&2
		failure=1
	fi
	if [ "$tag_target_seen" -ne 1 ] || [ "$tag_target_recipe_count" -ne 2 ] \
		|| [ "$tag_target_origin_count" -ne 1 ] || [ "$tag_target_command_count" -ne 1 ]; then
		printf 'check-release-boundary: release-tag-candidate-check must be one origin pin plus one direct check-release-tag invocation\n' >&2
		failure=1
	fi
	if [ "$source_target_seen" -ne 1 ] || [ "$source_target_recipe_count" -ne 1 ] \
		|| [ "$source_target_command_count" -ne 1 ]; then
		printf 'check-release-boundary: release-source-candidate-check must be one direct clean tagged-source check\n' >&2
		failure=1
	fi
	if [ "$controller_source_target_seen" -ne 1 ] \
		|| [ "$controller_source_target_recipe_count" -ne 2 ] \
		|| [ "$controller_source_target_origin_count" -ne 1 ] \
		|| [ "$controller_source_target_command_count" -ne 1 ]; then
		printf 'check-release-boundary: release-controller-source-check must pin origin and exact clean origin/main controller state\n' >&2
		failure=1
	fi
	if [ "$mode_source_target_seen" -ne 1 ] \
		|| [ "$mode_source_target_recipe_count" -ne 2 ] \
		|| [ "$mode_source_target_origin_count" -ne 1 ] \
		|| [ "$mode_source_target_command_count" -ne 1 ]; then
		printf 'check-release-boundary: release-source-mode-check must pin origin and prove the caller-named exact source anchor\n' >&2
		failure=1
	fi
	if [ "$plugin_tag_target_seen" -ne 1 ] || [ "$plugin_tag_target_recipe_count" -ne 2 ] \
		|| [ "$plugin_tag_target_origin_count" -ne 1 ] || [ "$plugin_tag_target_command_count" -ne 1 ]; then
		printf 'check-release-boundary: release-plugin-tag-candidate-check must be one origin pin plus one direct plugin-tag check\n' >&2
		failure=1
	fi
	if [ "$github_target_seen" -ne 1 ] || [ "$github_target_recipe_count" -ne 3 ] \
		|| [ "$github_target_origin_count" -ne 1 ] || [ "$github_target_tag_count" -ne 1 ] \
		|| [ "$github_target_command_count" -ne 1 ]; then
		printf 'check-release-boundary: release-github-candidate-check must pin origin, the release tag, and exact GitHub assets\n' >&2
		failure=1
	fi
	if [ "$github_assets_target_seen" -ne 1 ] \
		|| [ "$github_assets_source_count" -ne 1 ] \
		|| [ "$github_assets_hydrator_count" -ne 1 ] \
		|| [ "$github_assets_candidate_count" -ne 1 ]; then
		printf 'check-release-boundary: release-github-assets must hydrate through the staged installer between exact source and GitHub verification\n' >&2
		failure=1
	fi
	if [ "$registry_server_target_seen" -ne 1 ] \
		|| [ "$registry_server_materializer_count" -ne 1 ] \
		|| [ "$registry_server_template_count" -ne 1 ] \
		|| [ "$registry_server_generator_count" -ne 1 ] \
		|| [ "$registry_server_payload_count" -ne 1 ] \
		|| [ "$registry_server_validate_count" -ne 1 ] \
		|| [ "$registry_server_materializer_line" -ge "$registry_server_template_line" ] \
		|| [ "$registry_server_template_line" -ge "$registry_server_generator_line" ] \
		|| [ "$registry_server_generator_line" -ge "$registry_server_payload_line" ] \
		|| [ "$registry_server_payload_line" -ge "$registry_server_validate_line" ]; then
		printf 'check-release-boundary: release-registry-server must derive metadata from the immutable tag template and verified MCPB\n' >&2
		failure=1
	fi
	if [ "$run_main_push_count" -ne 1 ]; then
		printf 'check-release-boundary: _release-run must contain exactly one candidate push to origin before CI verification (found %s)\n' \
			"$run_main_push_count" >&2
		failure=1
	fi
	if [ "$release_early_push_count" -ne 1 ]; then
		printf 'check-release-boundary: release must land the candidate on origin exactly once before worktree prep (found %s)\n' \
			"$release_early_push_count" >&2
		failure=1
	fi
	if [ "$run_ci_wait_count" -ne 2 ]; then
		printf 'check-release-boundary: _release-run must contain pre-tag and final release-ci-wait gates (found %s)\n' \
			"$run_ci_wait_count" >&2
		failure=1
	fi
	if [ "$run_main_check_count" -ne 2 ]; then
		printf 'check-release-boundary: _release-run must contain pre-tag and final release-main-candidate-check gates (found %s)\n' \
			"$run_main_check_count" >&2
		failure=1
	fi
	if [ "$final_ci_block_count" -ne 1 ] \
		|| [ "$final_ci_block_line" -ne "$run_ci_wait_line2" ]; then
		printf 'check-release-boundary: final release-ci-wait must be one exact fail-closed local-tag cleanup block\n' >&2
		failure=1
	fi
	if [ "$final_main_block_count" -ne 1 ] \
		|| [ "$final_main_block_line" -ne "$run_main_check_line2" ]; then
		printf 'check-release-boundary: final release-main-candidate-check must be one exact fail-closed local-tag cleanup block\n' >&2
		failure=1
	fi
	if [ "$run_tag_count" -ne 1 ]; then
		printf 'check-release-boundary: _release-run must contain exactly one annotated release tag boundary (found %s)\n' \
			"$run_tag_count" >&2
		failure=1
	fi
	if [ "$run_atomic_tag_push_count" -ne 1 ]; then
		printf 'check-release-boundary: _release-run must contain exactly one atomic main-plus-tag push (found %s)\n' \
			"$run_atomic_tag_push_count" >&2
		failure=1
	fi
	if [ "$run_origin_check_count" -ne 2 ]; then
		printf 'check-release-boundary: _release-run must contain exactly two canonical origin checks (found %s)\n' \
			"$run_origin_check_count" >&2
		failure=1
	fi
	# plugin-check reaches the version stamps hosted CI drops, so it has to
	# release-smoke; with that leg gone the anchor is the pre-tag CI wait.
	if [ "$run_plugin_check_count" -ne 1 ] \
		|| [ "$run_ci_wait_line1" -eq 0 ] \
		|| [ "$run_plugin_check_line" -ge "$run_ci_wait_line1" ]; then
		printf 'check-release-boundary: _release-run must run one exact local plugin-check before the pre-tag CI wait\n' >&2
		failure=1
	fi
	# The release is hermetic as of 2026-08-06 by operator decision: it must
	# depend on no broker session and no external service. This asserted the
	# opposite until then — that release-smoke ran exactly once with pinned
	# are retired; release-smoke remains invocable by hand but may not run
	if [ "$run_release_smoke_count" -ne 0 ]; then
		printf 'check-release-boundary: _release-run must not invoke release-smoke; the release is hermetic and takes no broker dependency\n' >&2
		failure=1
	fi
	# The fixed published inventory must be proved while the release tag is
	# still a deletable local ref, not only after it is public.
	if [ "$run_payload_inventory_count" -ne 1 ] \
		|| [ "$inventory_block_count" -ne 1 ] \
		|| [ "$inventory_block_line" -ne "$run_payload_inventory_line" ] \
		|| [ "$run_tag_line" -eq 0 ] \
		|| [ "$run_atomic_tag_push_line" -eq 0 ] \
		|| [ "$run_tag_line" -ge "$run_payload_inventory_line" ] \
		|| [ "$run_payload_inventory_line" -ge "$run_atomic_tag_push_line" ]; then
		printf 'check-release-boundary: _release-run must prove the published payload inventory in one fail-closed local-tag cleanup block between the annotated tag and the atomic tag push\n' >&2
		failure=1
	fi
	if [ "$run_local_full_gate_count" -ne 0 ]; then
		printf 'check-release-boundary: _release-run must rely on pinned exact-SHA CI rather than repeat test, check, or commit-check locally\n' >&2
		failure=1
	fi
	if [ "$run_tag_candidate_count" -ne 2 ] \
		|| [ "$run_plugin_tag_candidate_count" -ne 2 ] \
		|| [ "$run_plugin_push_count" -ne 1 ] \
		|| [ "$run_plugin_local_check_count" -ne 1 ] \
		|| [ "$run_claude_plugin_tag_count" -ne 1 ] \
		|| [ "$run_github_check_count" -ne 1 ] \
		|| [ "$run_registry_line" -eq 0 ] \
		|| [ "$run_first_publication_line" -eq 0 ] \
		|| [ "$run_github_publish_line" -eq 0 ] \
		|| [ "$run_atomic_tag_push_line" -ge "$run_tag_candidate_line1" ] \
		|| [ "$run_tag_candidate_line1" -ge "$run_first_publication_line" ] \
		|| [ "$run_first_publication_line" -ge "$run_plugin_local_check_line" ] \
		|| [ "$run_plugin_local_check_line" -ge "$run_plugin_push_line" ] \
		|| [ "$run_plugin_push_line" -ge "$run_plugin_tag_candidate_line1" ] \
		|| [ "$run_plugin_tag_candidate_line1" -ge "$run_github_publish_line" ] \
		|| [ "$run_github_publish_line" -ge "$run_github_check_line" ] \
		|| [ "$run_github_check_line" -ge "$run_tag_candidate_line2" ] \
		|| [ "$run_tag_candidate_line2" -ge "$run_plugin_tag_candidate_line2" ] \
		|| [ "$run_plugin_tag_candidate_line2" -ge "$run_registry_line" ]; then
		printf 'check-release-boundary: fresh publication order must prove release tag, push only the plugin ref, verify GitHub assets, then re-prove both tags before registry publication\n' >&2
		failure=1
	fi
	if [ "$run_main_push_count" -eq 1 ] && [ "$run_ci_wait_count" -eq 2 ] \
		&& [ "$run_main_check_count" -eq 2 ] && [ "$run_tag_count" -eq 1 ] \
		&& [ "$run_atomic_tag_push_count" -eq 1 ] && [ "$run_origin_check_count" -eq 2 ] \
		&& { [ "$run_origin_check_line1" -ge "$run_main_push_line" ] \
			|| [ "$run_main_push_line" -ge "$run_ci_wait_line1" ] \
			|| [ "$run_ci_wait_line1" -ge "$run_main_check_line1" ] \
			|| [ "$run_main_check_line1" -ge "$run_tag_line" ] \
			|| [ "$run_tag_line" -ge "$run_ci_wait_line2" ] \
			|| [ "$run_ci_wait_line2" -ge "$run_main_check_line2" ] \
			|| [ "$run_main_check_line2" -ge "$run_origin_check_line2" ] \
			|| [ "$run_origin_check_line2" -ge "$run_atomic_tag_push_line" ]; }; then
		printf 'check-release-boundary: required order is origin check < candidate push < pre-tag CI/main gates < annotated tag < final CI/main gates < origin recheck < atomic main-plus-tag push\n' >&2
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
	if [ "$publish_fresh_call_count" -ne 1 ] || [ "$publish_resume_call_count" -ne 1 ] \
		|| [ "$publish_resume_source_count" -ne 1 ]; then
		printf 'check-release-boundary: fresh and resume lanes must each invoke _release-publish with their exact pipeline entry\n' >&2
		failure=1
	fi
	if [ "$publish_changelog_gate_count" -ne 1 ] \
		|| [ "$publish_tag_materializer_count" -ne 2 ] \
		|| [ "$publish_changelog_path_count" -ne 1 ] \
		|| [ "$publish_template_path_count" -ne 1 ] \
		|| [ "$publish_renderer_count" -ne 1 ] \
		|| [ "$publish_changelog_path_line" -ge "$publish_template_path_line" ] \
		|| [ "$publish_template_path_line" -ge "$publish_renderer_line" ] \
		|| [ "$publish_renderer_line" -ge "$publish_create_line" ]; then
		printf 'check-release-boundary: _release-publish must render notes only from immutable tag blobs\n' >&2
		failure=1
	fi
	if [ "$publish_direct_origin_count" -ne 1 ] || [ "$publish_create_line" -eq 0 ] \
		|| [ "$publish_direct_origin_line" -ge "$publish_create_line" ]; then
		printf 'check-release-boundary: _release-publish must directly check canonical origin before gh release create\n' >&2
		failure=1
	fi
	if [ "$publish_payload_inventory_count" -ne 1 ] \
		|| [ "$publish_create_line" -eq 0 ] \
		|| [ "$publish_payload_inventory_line" -ge "$publish_create_line" ]; then
		printf 'check-release-boundary: _release-publish must prove the published payload inventory before gh release create\n' >&2
		failure=1
	fi
	if [ "$publish_ci_authority_count" -ne 1 ] \
		|| [ "$publish_tag_check_count" -ne 1 ] \
		|| [ "$publish_ci_authority_line" -ge "$publish_direct_origin_line" ] \
		|| [ $((publish_direct_origin_line + 1)) -ne "$publish_tag_check_line" ] \
		|| [ $((publish_tag_check_line + 1)) -ne "$publish_create_line" ]; then
		printf 'check-release-boundary: _release-publish must re-prove exact-SHA CI, then directly pin origin and the remote tag immediately before gh release create\n' >&2
		failure=1
	fi
	if [ "$publish_upload_count" -ne 1 ] \
		|| [ "$publish_draft_verify_count" -ne 1 ] \
		|| [ "$publish_flip_count" -ne 1 ] \
		|| [ $((publish_create_line + 1)) -ne "$publish_upload_line" ] \
		|| [ $((publish_upload_line + 1)) -ne "$publish_draft_verify_line" ] \
		|| [ $((publish_draft_verify_line + 1)) -ne "$publish_flip_line" ]; then
		printf 'check-release-boundary: _release-publish must upload assets to the staged draft, verify it, and publish it only via the pinned flip, in that order\n' >&2
		failure=1
	fi
fi
if [ "$resume_seen" -eq 1 ]; then
	if [ "$resume_guard_makelevel" -ne 1 ] || [ "$resume_guard_entry" -ne 1 ]; then
		printf 'check-release-boundary: _release-resume-run must reject top-level calls using MAKELEVEL and RELEASE_PIPELINE_ENTRY guards\n' >&2
		failure=1
	fi
	if [ "$resume_called_from_release_resume" -ne 1 ]; then
		printf 'check-release-boundary: _release-resume-run must be reachable from the canonical release-resume target\n' >&2
		failure=1
	fi
	if [ "$resume_ci_wait_count" -ne 2 ]; then
		printf 'check-release-boundary: _release-resume-run must contain early and final historical CI gates (found %s)\n' \
			"$resume_ci_wait_count" >&2
		failure=1
	fi
	if [ "$resume_ci_wait_count" -eq 2 ] && [ "$resume_plugin_push_line" -gt 0 ] \
		&& { [ "$resume_ci_wait_line1" -ge "$resume_ci_wait_line2" ] \
			|| [ "$resume_ci_wait_line2" -ge "$resume_origin_check_line2" ] \
			|| [ "$resume_origin_check_line2" -ge "$resume_tag_candidate_line2" ] \
			|| [ "$resume_tag_candidate_line2" -ge "$resume_plugin_push_line" ]; }; then
		printf 'check-release-boundary: release-resume must re-verify exact-SHA Actions and origin immediately before publication\n' >&2
		failure=1
	fi
	if [ "$resume_origin_check_count" -ne 2 ] || [ "$resume_plugin_push_line" -eq 0 ] \
		|| [ "$resume_origin_check_line1" -ge "$resume_github_line" ] \
		|| [ "$resume_origin_check_line2" -ge "$resume_plugin_push_line" ]; then
		printf 'check-release-boundary: release-resume must recheck canonical origin before publication\n' >&2
		failure=1
	fi
	if [ "$resume_tag_candidate_count" -ne 3 ] \
		|| [ "$resume_plugin_tag_candidate_count" -ne 2 ] \
		|| [ "$resume_plugin_push_count" -ne 1 ] \
		|| [ "$resume_plugin_local_check_count" -ne 2 ] \
		|| [ "$resume_claude_plugin_tag_count" -ne 1 ] \
		|| [ "$resume_controller_source_count" -ne 1 ] \
		|| [ "$resume_static_contract_count" -ne 1 ] \
		|| [ "$resume_github_view_count" -ne 1 ] \
		|| [ "$resume_github_assets_count" -ne 1 ] \
		|| [ "$resume_binaries_count" -ne 1 ] \
		|| [ "$resume_existing_state_count" -ne 1 ] \
		|| [ "$resume_absent_state_count" -ne 1 ] \
		|| [ "$resume_state_dispatch_count" -ne 1 ] \
		|| [ "$resume_github_check_count" -ne 1 ] \
		|| [ "$resume_github_publish_line" -eq 0 ] \
		|| [ "$resume_registry_line" -eq 0 ] \
		|| [ "$resume_ci_wait_line1" -ge "$resume_origin_check_line1" ] \
		|| [ "$resume_origin_check_line1" -ge "$resume_tag_candidate_line1" ] \
		|| [ "$resume_tag_candidate_line1" -ge "$resume_github_line" ] \
		|| [ "$resume_github_line" -ge "$resume_github_assets_line" ] \
		|| [ "$resume_github_assets_line" -ge "$resume_binaries_line" ] \
		|| [ "$resume_binaries_line" -ge "$resume_ci_wait_line2" ] \
		|| [ "$resume_ci_wait_line2" -ge "$resume_origin_check_line2" ] \
		|| [ "$resume_origin_check_line2" -ge "$resume_tag_candidate_line2" ] \
		|| [ "$resume_tag_candidate_line2" -ge "$resume_plugin_push_line" ] \
		|| [ "$resume_plugin_push_line" -ge "$resume_plugin_tag_candidate_line1" ] \
		|| [ "$resume_plugin_tag_candidate_line1" -ge "$resume_github_publish_line" ] \
		|| [ "$resume_github_publish_line" -ge "$resume_github_check_line" ] \
		|| [ "$resume_github_check_line" -ge "$resume_tag_candidate_line3" ] \
		|| [ "$resume_tag_candidate_line3" -ge "$resume_plugin_tag_candidate_line2" ] \
		|| [ "$resume_plugin_tag_candidate_line2" -ge "$resume_registry_line" ]; then
		printf 'check-release-boundary: release-resume must verify or publish each exact tag and GitHub asset set before guarded registry recovery\n' >&2
		failure=1
	fi
fi
if [ "$registry_publish_target_seen" -ne 1 ] \
	|| [ "$registry_publish_authority_count" -ne 1 ] \
	|| [ "$registry_publish_source_count" -ne 1 ] \
	|| [ "$registry_publish_static_count" -ne 1 ] \
	|| [ "$registry_publish_origin_count" -ne 1 ] \
	|| [ "$registry_publish_tag_count" -ne 1 ] \
	|| [ "$registry_publish_plugin_tag_count" -ne 1 ] \
	|| [ "$registry_publish_github_count" -ne 1 ] \
	|| [ "$registry_publish_server_count" -ne 1 ] \
	|| [ "$registry_publish_command_count" -ne 1 ] \
	|| [ "$registry_publish_source_line" -ge "$registry_publish_static_line" ] \
	|| [ "$registry_publish_static_line" -ge "$registry_publish_origin_line" ] \
	|| [ "$registry_publish_origin_line" -ge "$registry_publish_tag_line" ] \
	|| [ "$registry_publish_tag_line" -ge "$registry_publish_plugin_tag_line" ] \
	|| [ "$registry_publish_plugin_tag_line" -ge "$registry_publish_github_line" ] \
	|| [ "$registry_publish_github_line" -ge "$registry_publish_authority_line" ] \
	|| [ "$registry_publish_authority_line" -ge "$registry_publish_server_line" ] \
	|| [ "$registry_publish_server_line" -ge "$registry_publish_command_line" ]; then
	printf 'check-release-boundary: registry-publish must re-prove exact-SHA CI, origin, both tags, and GitHub assets before its sole fallback publication command\n' >&2
	failure=1
fi
if [ "$registry_verify_target_seen" -ne 1 ] \
	|| [ "$registry_verify_authority_count" -ne 1 ] \
	|| [ "$registry_verify_source_count" -ne 1 ] \
	|| [ "$registry_verify_static_count" -ne 1 ] \
	|| [ "$registry_verify_origin_count" -ne 1 ] \
	|| [ "$registry_verify_tag_count" -ne 1 ] \
	|| [ "$registry_verify_plugin_tag_count" -ne 1 ] \
	|| [ "$registry_verify_github_count" -ne 1 ] \
	|| [ "$registry_verify_server_count" -ne 1 ] \
	|| [ "$registry_verify_expected_json_count" -ne 1 ] \
	|| [ "$registry_verify_fallback_count" -ne 1 ] \
	|| [ "$registry_verify_fallback_make_count" -ne 1 ] \
	|| [ "$registry_verify_entry_count" -ne 0 ] \
	|| [ "$registry_verify_source_line" -ge "$registry_verify_static_line" ] \
	|| [ "$registry_verify_static_line" -ge "$registry_verify_origin_line" ] \
	|| [ "$registry_verify_origin_line" -ge "$registry_verify_tag_line" ] \
	|| [ "$registry_verify_tag_line" -ge "$registry_verify_plugin_tag_line" ] \
	|| [ "$registry_verify_plugin_tag_line" -ge "$registry_verify_github_line" ] \
	|| [ "$registry_verify_github_line" -ge "$registry_verify_authority_line" ] \
	|| [ "$registry_verify_authority_line" -ge "$registry_verify_server_line" ] \
	|| [ "$registry_verify_server_line" -ge "$registry_verify_fallback_line" ]; then
	printf 'check-release-boundary: registry-publish-verify-first must re-prove publication authority and propagate the exact pipeline entry to its guarded fallback\n' >&2
	failure=1
fi
if [ "$historical_target_seen" -eq 1 ]; then
	if [ "$historical_guard_makelevel" -ne 1 ] || [ "$historical_guard_entry" -ne 1 ]; then
		printf 'check-release-boundary: _release-ci-wait-historical must reject top-level calls using MAKELEVEL and RELEASE_PIPELINE_ENTRY guards\n' >&2
		failure=1
	fi
	if [ "$historical_release_sha_count" -ne 1 ] \
		|| [ "$historical_waiter_sha_count" -ne 1 ] \
		|| [ "$historical_head_sha_count" -ne 0 ]; then
		printf 'check-release-boundary: historical CI authority must use the exact release tag commit, never controller HEAD\n' >&2
		failure=1
	fi
	if [ "$historical_called_from_resume" -ne 1 ] \
		|| [ "$historical_called_from_publish" -ne 1 ] \
		|| [ "$historical_called_from_registry" -ne 2 ]; then
		printf 'check-release-boundary: historical exact-SHA authority must be reachable only from resume and guarded tag-publication/registry sinks\n' >&2
		failure=1
	fi
fi

if [ "$failure" -ne 0 ]; then
	exit 1
fi
echo "check-release-boundary: OK"
