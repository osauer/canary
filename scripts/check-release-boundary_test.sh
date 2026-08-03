#!/usr/bin/env bash

set -euo pipefail

unset GIT_CONFIG_COUNT GIT_CONFIG_PARAMETERS GIT_CONFIG_SYSTEM
export GIT_CONFIG_NOSYSTEM=1
export GIT_CONFIG_GLOBAL=/dev/null
unset MAKEFILES GNUMAKEFLAGS

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
checker="$repo_root/scripts/check-release-boundary.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/canary-release-boundary-test.XXXXXX")"
cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

# Fixture makes run with a scrubbed environment and cwd pinned inside the
# fixture dir. Inside the release pipeline, MAKEFLAGS carries MAKELEVEL and
# RELEASE_PIPELINE_ENTRY=release into every descendant make — on 2026-07-29
# that armed the fixture guards and the fixture's then-real `gh release
# create` executed against the repository and published a bogus release.
# Publication recipes below are therefore inert (`false && <command>`), so
# even a broken guard cannot reach an external command.
fixture_make() {
	env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEFILES -u GNUMAKEFLAGS \
		RELEASE_PIPELINE_ENTRY= \
		PATH="$test_root/bin:$PATH" \
		make -s -C "$test_root" -f Makefile "$@"
}

fixture_make_with_makefiles() {
	makefiles_path=$1
	shift
	env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEFILES -u GNUMAKEFLAGS \
		MAKEFILES="$makefiles_path" RELEASE_PIPELINE_ENTRY= \
		PATH="$test_root/bin:$PATH" \
		make -s -C "$test_root" -f Makefile "$@"
}

# A release variable set in the environment beats a makefile `?=` assignment
# just as a command-line one does, so both origins get exercised.
fixture_make_with_env() {
	assignment=$1
	shift
	env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEFILES -u GNUMAKEFLAGS \
		RELEASE_PIPELINE_ENTRY= "$assignment" \
		PATH="$test_root/bin:$PATH" \
		make -s -C "$test_root" -f Makefile "$@"
}

mkdir -p "$test_root/bin" "$test_root/scripts" "$test_root/.github/workflows"
for command in git gh claude; do
	cat > "$test_root/bin/$command" <<'EOF'
#!/bin/sh
touch "$PWD/guard-leaked"
exit 97
EOF
	chmod 0755 "$test_root/bin/$command"
done
cat > "$test_root/Makefile" <<'EOF'
.PHONY: release release-resume _release-run _release-resume-run _release-publish release-origin-check release-ci-wait _release-ci-wait-historical release-main-candidate-check release-source-candidate-check release-controller-source-check release-tag-candidate-check release-plugin-tag-candidate-check release-github-candidate-check release-github-assets release-payload-inventory-check release-registry-server registry-publish registry-publish-verify-first
override release_first_makeflag = $(firstword $(MAKEFLAGS))
override release_compact_makeflags = $(if $(filter --%,$(release_first_makeflag)),,$(if $(findstring =,$(release_first_makeflag)),,$(release_first_makeflag)))
override release_unsafe_makeflags = $(strip $(filter -i --ignore-errors -k --keep-going,$(MAKEFLAGS)) $(if $(findstring i,$(release_compact_makeflags)),i) $(if $(findstring k,$(release_compact_makeflags)),k) $(if $(findstring n,$(release_compact_makeflags)),n) $(if $(findstring t,$(release_compact_makeflags)),t))
override release_pinned_vars = RELEASE_TARGETS SPX_EXPECTED_REACHABLE SMOKE_STRICT MAIN_BRANCH GO_TAGS GO_BUILD_TAGS STRIP_LDFLAGS LDFLAGS
override release_overridden_vars = $(strip $(foreach release_pinned_var,$(release_pinned_vars),$(if $(filter file,$(origin $(release_pinned_var))),,$(release_pinned_var))))
RELEASE_TARGETS = darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
SPX_EXPECTED_REACHABLE ?= 1
SMOKE_STRICT ?= 0
MAIN_BRANCH ?= main
GO_TAGS ?= trading
GO_BUILD_TAGS = $(if $(strip $(GO_TAGS)),-tags '$(GO_TAGS)',)
STRIP_LDFLAGS = -s -w
LDFLAGS = $(STRIP_LDFLAGS)
RELEASE_CONTROLLER_CONTRACT = release-controller-v1
release-payload-inventory-check:
	@./scripts/check-release-payload-inventory.sh "$(RELEASE_VERSION)" "$(abspath $(DIST_DIR))"
release-origin-check:
	@./scripts/check-release-origin.sh
release-source-candidate-check:
	@./scripts/check-release-source.sh "$(RELEASE_VERSION)"
release-controller-source-check:
	$(MAKE) release-origin-check
	@./scripts/check-release-source.sh --controller "$(RELEASE_VERSION)"
release-tag-candidate-check:
	$(MAKE) release-origin-check
	@./scripts/check-release-tag.sh "$(RELEASE_VERSION)"
release-plugin-tag-candidate-check:
	$(MAKE) release-origin-check
	@./scripts/check-release-tag.sh --plugin "$(RELEASE_VERSION)"
release-github-candidate-check:
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/check-github-release.sh "$(RELEASE_VERSION)" "$(DIST_DIR)"
release-github-assets:
	$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/hydrate-github-release-assets.sh "$(RELEASE_VERSION)" "$(abspath $(DIST_DIR))"
	$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
release-registry-server:
	@set -eu; \
	template=$$(mktemp "$${TMPDIR:-/tmp}/canary-registry-template.XXXXXX") || exit 1; \
	trap 'rm -f "$$template"' EXIT HUP INT TERM; \
	python3 ./scripts/materialize-release-tag-file.py \
		"$(RELEASE_VERSION)" server.json "$$template"; \
	go run ./scripts/release-registry-server "$(RELEASE_VERSION)" "$$template" \
		"$(DIST_DIR)/canary-$(RELEASE_VERSION).mcpb" "$(DIST_DIR)/server.json"; \
	$(MCP_PUBLISHER) validate "$(DIST_DIR)/server.json"
registry-publish:
	$(if $(filter default,$(origin MAKE)),,$(error registry-publish: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error registry-publish: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error registry-publish: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error registry-publish: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error registry-publish: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error registry-publish: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error registry-publish: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error registry-publish: release variables must not be overridden: $(release_overridden_vars)),)
	$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/check-release-ci-contract.sh
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-registry-server RELEASE_VERSION=$(RELEASE_VERSION)
	./scripts/registry-publish-with-login.sh "$(MCP_PUBLISHER)" "$(DIST_DIR)/server.json"
registry-publish-verify-first:
	$(if $(filter default,$(origin MAKE)),,$(error registry-publish-verify-first: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error registry-publish-verify-first: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error registry-publish-verify-first: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error registry-publish-verify-first: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error registry-publish-verify-first: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error registry-publish-verify-first: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error registry-publish-verify-first: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error registry-publish-verify-first: release variables must not be overridden: $(release_overridden_vars)),)
	$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/check-release-ci-contract.sh
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION),$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION))
	$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-registry-server RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/registry-publish-verify-first.sh "$(RELEASE_VERSION)" \
		"$(DIST_DIR)/server.json" \
		make --no-print-directory registry-publish \
		RELEASE_VERSION="$(RELEASE_VERSION)"
release-ci-wait:
	@false
_release-publish:
	$(if $(filter default,$(origin MAKE)),,$(error _release-publish: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error _release-publish: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error _release-publish: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error _release-publish: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error _release-publish: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error _release-publish: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error _release-publish: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error _release-publish: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ "$(MAKELEVEL)" -lt 1 ]; then exit 1; fi
	@case "$(RELEASE_PIPELINE_ENTRY)" in release|release-resume) ;; *) exit 1 ;; esac
	$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) release-ci-wait,$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume)
	$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) changelog-lint RELEASE_VERSION=$(RELEASE_VERSION),$(MAKE) changelog-lint-historical RELEASE_VERSION=$(RELEASE_VERSION) RELEASE_SOURCE_DIR="$(RELEASE_SOURCE_DIR)")
	@notes=$$(mktemp -t canary-release-notes.XXXXXX) && \
	changelog=$$(mktemp -t canary-release-changelog.XXXXXX) && \
	template=$$(mktemp -t canary-release-notes-template.XXXXXX) && \
	python3 ./scripts/materialize-release-tag-file.py \
		"$(RELEASE_VERSION)" CHANGELOG.md "$$changelog" && \
	python3 ./scripts/materialize-release-tag-file.py \
		"$(RELEASE_VERSION)" .github/release-notes-template.md "$$template" && \
	./scripts/render-release-notes.sh "$(RELEASE_VERSION)" "$$changelog" "$$template" "$$notes" && \
	true
	./scripts/check-release-origin.sh && \
	./scripts/check-release-tag.sh "$(RELEASE_VERSION)" && \
	gh release create v1.2.3 --repo github.com/osauer/canary --verify-tag
_release-run:
	$(if $(filter default,$(origin MAKE)),,$(error _release-run: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error _release-run: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error _release-run: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error _release-run: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error _release-run: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error _release-run: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error _release-run: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error _release-run: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	$(MAKE) release-origin-check
	git push --no-follow-tags origin HEAD:$(MAIN_BRANCH)
	$(MAKE) plugin-check
	$(MAKE) release-smoke RELEASE_VERSION=$(RELEASE_VERSION) SMOKE_STRICT=1 SPX_EXPECTED_REACHABLE=1
	$(MAKE) release-ci-wait
	$(MAKE) release-main-candidate-check
	@msg=fixture; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	@$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION) || { \
		git tag -d $(RELEASE_VERSION) >/dev/null 2>&1; \
		exit 1; \
	}
	@$(MAKE) release-ci-wait || { \
		git tag -d $(RELEASE_VERSION) >/dev/null 2>&1; \
		exit 1; \
	}
	@$(MAKE) release-main-candidate-check || { \
		git tag -d $(RELEASE_VERSION) >/dev/null 2>&1; \
		exit 1; \
	}
	$(MAKE) release-origin-check
	git push --no-follow-tags --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@plugin_ref=$$(./scripts/check-release-tag.sh --plugin-ref "$(RELEASE_VERSION)") || exit 1; \
	claude plugin tag . && \
	./scripts/check-release-tag.sh --plugin-local "$(RELEASE_VERSION)" && \
	git push --no-follow-tags origin "$$plugin_ref"
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) registry-publish-verify-first RELEASE_PIPELINE_ENTRY=release RELEASE_VERSION=$(RELEASE_VERSION)
release:
	$(if $(filter default,$(origin MAKE)),,$(error release: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error release: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error release: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error release: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error release: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error release: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error release: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error release: release variables must not be overridden: $(release_overridden_vars)),)
	$(MAKE) -C . _release-run RELEASE_PIPELINE_ENTRY=release
_release-ci-wait-historical:
	$(if $(filter default,$(origin MAKE)),,$(error _release-ci-wait-historical: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error _release-ci-wait-historical: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error _release-ci-wait-historical: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error _release-ci-wait-historical: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error _release-ci-wait-historical: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error _release-ci-wait-historical: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error _release-ci-wait-historical: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error _release-ci-wait-historical: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release-resume" ]; then exit 1; fi
	@release_sha=$$(git rev-parse --verify "refs/tags/$(RELEASE_VERSION)^{commit}") || { \
		exit 1; \
	}; \
	GOFLAGS= go run ./scripts/release-ci-wait \
		-contract scripts/release-ci-contract.json -historical \
		-sha "$$release_sha" -branch "$(MAIN_BRANCH)" -event push \
		-poll 1s -timeout 1s
_release-resume-run:
	$(if $(filter default,$(origin MAKE)),,$(error _release-resume-run: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error _release-resume-run: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error _release-resume-run: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error _release-resume-run: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error _release-resume-run: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error _release-resume-run: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error _release-resume-run: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error _release-resume-run: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release-resume" ]; then exit 1; fi
	$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/check-release-ci-contract.sh
	$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@release_state=$$(./scripts/github-release-state.sh "$(RELEASE_VERSION)") || exit 1; \
	if [ "$$release_state" = existing ]; then \
		echo present; \
		$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION); \
		printf '%s\n' existing >"$(DIST_DIR)/.canary-resume-github-state"; \
	elif [ "$$release_state" = absent ]; then \
		echo absent; \
		$(MAKE) release-binaries RELEASE_VERSION=$(RELEASE_VERSION); \
		printf '%s\n' absent >"$(DIST_DIR)/.canary-resume-github-state"; \
	else \
		exit 1; \
	fi
	@plugin_ref=$$(./scripts/check-release-tag.sh --plugin-ref "$(RELEASE_VERSION)") || exit 1; \
	if git ls-remote --exit-code origin "$$plugin_ref" >/dev/null 2>&1; then \
		$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION); \
	else \
		if git show-ref --verify --quiet "$$plugin_ref"; then \
			./scripts/check-release-tag.sh --plugin-local "$(RELEASE_VERSION)" || exit 1; \
		else \
			claude plugin tag "$(RELEASE_SOURCE_DIR)" && \
			./scripts/check-release-tag.sh --plugin-local "$(RELEASE_VERSION)" || exit 1; \
		fi; \
	fi
	$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@plugin_ref=$$(./scripts/check-release-tag.sh --plugin-ref "$(RELEASE_VERSION)") || exit 1; \
	if git ls-remote --exit-code origin "$$plugin_ref" >/dev/null 2>&1; then \
		echo present; \
	else \
		git push --no-follow-tags origin "$$plugin_ref"; \
	fi
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@resume_state=$$(cat "$(DIST_DIR)/.canary-resume-github-state" 2>/dev/null || true); \
	case "$$resume_state" in \
	existing) echo present ;; \
	absent) \
		$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION) RELEASE_SOURCE_DIR="$(RELEASE_SOURCE_DIR)"; \
	;; \
	*) exit 1 ;; \
	esac
	$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) registry-publish-verify-first RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)
release-resume:
	$(if $(filter default,$(origin MAKE)),,$(error release-resume: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error release-resume: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error release-resume: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error release-resume: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error release-resume: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error release-resume: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error release-resume: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error release-resume: release variables must not be overridden: $(release_overridden_vars)),)
	@release_sha=$$(git rev-parse --verify "refs/tags/$(RELEASE_VERSION)^{commit}") || exit 1; \
	controller_sha=$$(git rev-parse --verify "HEAD^{commit}") || exit 1; \
	if ! git grep -Fqx 'RELEASE_CONTROLLER_CONTRACT = release-controller-v1' "$$controller_sha" -- Makefile; then \
		exit 1; \
	fi; \
	controller_wt="controller"; \
	source_wt="source"; \
	git worktree add --detach "$$controller_wt" "$$controller_sha" || exit 1; \
	if ! git worktree add --detach "$$source_wt" "$$release_sha"; then \
		exit 1; \
	fi; \
	if MESSAGE=fixture $(MAKE) -C "$$controller_wt" _release-resume-run RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION) RELEASE_SOURCE_DIR="$$source_wt"; then \
		true; \
	else \
		exit 1; \
	fi
EOF
cat > "$test_root/scripts/package.sh" <<'EOF'
#!/bin/sh
printf '%s\n' package
EOF
chmod 0755 "$test_root/scripts/package.sh"
cat > "$test_root/scripts/check-release-payload-inventory.sh" <<'EOF'
#!/bin/sh
CANONICAL_RELEASE_TARGETS="darwin-arm64 darwin-amd64 linux-amd64 linux-arm64"
printf '%s\n' "$CANONICAL_RELEASE_TARGETS"
EOF
chmod 0755 "$test_root/scripts/check-release-payload-inventory.sh"

"$checker" "$test_root" >/dev/null

rm -f "$test_root/guard-leaked"
if fixture_make _release-publish >/dev/null 2>&1; then
	echo "check-release-boundary test: direct internal publication helper invocation passed" >&2
	exit 1
fi
if [ -e "$test_root/guard-leaked" ]; then
	echo "check-release-boundary test: _release-publish guard leaked; publication recipe executed" >&2
	exit 1
fi
if fixture_make _release-run >/dev/null 2>&1; then
	echo "check-release-boundary test: direct internal pipeline body invocation passed" >&2
	exit 1
fi
if [ -e "$test_root/guard-leaked" ]; then
	echo "check-release-boundary test: _release-run guard leaked; publication recipe executed" >&2
	exit 1
fi
if fixture_make _release-resume-run >/dev/null 2>&1; then
	echo "check-release-boundary test: direct internal resume body invocation passed" >&2
	exit 1
fi
if fixture_make _release-ci-wait-historical >/dev/null 2>&1; then
	echo "check-release-boundary test: direct historical CI helper invocation passed" >&2
	exit 1
fi
if [ -e "$test_root/guard-leaked" ]; then
	echo "check-release-boundary test: internal release guard leaked; external recipe executed" >&2
	exit 1
fi

# Make-level ignore/keep-going state and recursive-Make overrides can bypass
# shell failures unless rejected by expansion-time $(error) guards.
for context_case in \
	ignore_errors keep_going dry_run dry_run_long just_print recon touch \
	touch_long make_override makeflags_override
do
	rm -f "$test_root/guard-leaked"
	case "$context_case" in
		ignore_errors) args=(-i release) ;;
		keep_going) args=(-k release) ;;
		dry_run) args=(-n release) ;;
		dry_run_long) args=(--dry-run release) ;;
		just_print) args=(--just-print release) ;;
		recon) args=(--recon release) ;;
		touch) args=(-t release) ;;
		touch_long) args=(--touch release) ;;
		make_override) args=(MAKE=true release) ;;
		makeflags_override) args=(MAKEFLAGS= release) ;;
	esac
	if fixture_make "${args[@]}" >/dev/null 2>&1; then
		echo "check-release-boundary test: $context_case release context passed" >&2
		exit 1
	fi
	if [ -e "$test_root/guard-leaked" ]; then
		echo "check-release-boundary test: $context_case reached an external command" >&2
		exit 1
	fi
done

# Release variables that are constants of the contract, not caller knobs. A
# narrowed RELEASE_TARGETS used to assemble a partial artifact set that every
# later step then reported as complete, and SPX_EXPECTED_REACHABLE=0 /
# SMOKE_STRICT=0 disarm the binding live smoke. Each attempt must be refused by
# the expansion-time guard — proved by its exact diagnostic, not merely by the
# fixture failing later for some unrelated reason — and must reach no external
# command on the way.
assert_pinned_var_refused() {
	description=$1
	shift
	rm -f "$test_root/guard-leaked"
	if output="$("$@" 2>&1)"; then
		echo "check-release-boundary test: $description was accepted" >&2
		exit 1
	fi
	case "$output" in
		*"release variables must not be overridden"*) ;;
		*)
			echo "check-release-boundary test: $description was not refused by the pinned-variable guard" >&2
			exit 1
			;;
	esac
	if [ -e "$test_root/guard-leaked" ]; then
		echo "check-release-boundary test: $description reached an external command" >&2
		exit 1
	fi
}

for pinned_target in release release-resume registry-publish registry-publish-verify-first; do
	for pinned_case in \
		RELEASE_TARGETS=linux-amd64 \
		RELEASE_TARGETS= \
		SPX_EXPECTED_REACHABLE=0 \
		SMOKE_STRICT=0 \
		MAIN_BRANCH=scratch \
		GO_TAGS= \
		GO_BUILD_TAGS= \
		STRIP_LDFLAGS= \
		LDFLAGS=-X=main.version=spoofed
	do
		assert_pinned_var_refused "command-line $pinned_case on $pinned_target" \
			fixture_make "$pinned_case" "$pinned_target" RELEASE_VERSION=v1.2.3
	done
	# `?=` defaults also lose to the environment, so that origin is release
	# authority too. `=` assignments (RELEASE_TARGETS, STRIP_LDFLAGS) are
	# immune to the environment and are covered by the command-line cases.
	for pinned_env_case in \
		SPX_EXPECTED_REACHABLE=0 \
		SMOKE_STRICT=0 \
		MAIN_BRANCH=scratch \
		GO_TAGS=
	do
		assert_pinned_var_refused "environment $pinned_env_case on $pinned_target" \
			fixture_make_with_env "$pinned_env_case" "$pinned_target" RELEASE_VERSION=v1.2.3
	done
done
rm -f "$test_root/guard-leaked"

for recursive_mode in -n -t; do
	rm -f "$test_root/guard-leaked"
	if PATH="$test_root/bin:$PATH" MAKELEVEL=4 RELEASE_PIPELINE_ENTRY=release \
		make -s "$recursive_mode" -C "$test_root" -f Makefile \
		_release-publish RELEASE_PIPELINE_ENTRY=release RELEASE_VERSION=v1.2.3 \
		>/dev/null 2>&1; then
		echo "check-release-boundary test: $recursive_mode direct publication context passed" >&2
		exit 1
	fi
	if [ -e "$test_root/guard-leaked" ]; then
		echo "check-release-boundary test: $recursive_mode direct publication reached an external command" >&2
		exit 1
	fi
done

cat >"$test_root/injected.mk" <<'EOF'
.IGNORE:
EOF
for injected_case in direct_makefiles overridden_makefile_list; do
	rm -f "$test_root/guard-leaked"
	case "$injected_case" in
		direct_makefiles)
			injected_args=(release)
			if fixture_make_with_makefiles "$test_root/injected.mk" \
				"${injected_args[@]}" >/dev/null 2>&1; then
				echo "check-release-boundary test: $injected_case release context passed" >&2
				exit 1
			fi
			;;
		overridden_makefile_list)
			injected_args=(MAKEFILE_LIST=Makefile release)
			if fixture_make "${injected_args[@]}" >/dev/null 2>&1; then
				echo "check-release-boundary test: $injected_case release context passed" >&2
				exit 1
			fi
			;;
	esac
	if [ -e "$test_root/guard-leaked" ]; then
		echo "check-release-boundary test: $injected_case reached an external command" >&2
		exit 1
	fi
done
rm -f "$test_root/injected.mk"

# An attacker-controlled first makefile can set global failure semantics before
# the canonical file is parsed. The canonical target must reject a multi-file
# MAKEFILE_LIST before any recipe is expanded.
cat >"$test_root/extra.mk" <<'EOF'
.IGNORE:
EOF
rm -f "$test_root/guard-leaked"
if env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL -u MAKEFILES -u GNUMAKEFLAGS \
	RELEASE_PIPELINE_ENTRY= \
	PATH="$test_root/bin:$PATH" \
	make -s -C "$test_root" -f extra.mk -f Makefile release >/dev/null 2>&1; then
	echo "check-release-boundary test: two-file Make invocation passed" >&2
	exit 1
fi
if [ -e "$test_root/guard-leaked" ]; then
	echo "check-release-boundary test: two-file Make invocation reached an external command" >&2
	exit 1
fi
rm -f "$test_root/extra.mk"

for registry_target in registry-publish registry-publish-verify-first; do
	for unsafe_mode in -i -k -n -t; do
		rm -f "$test_root/guard-leaked"
		if fixture_make "$unsafe_mode" "$registry_target" RELEASE_VERSION=v1.2.3 \
			>/dev/null 2>&1; then
			echo "check-release-boundary test: $unsafe_mode $registry_target passed" >&2
			exit 1
		fi
		if [ -e "$test_root/guard-leaked" ]; then
			echo "check-release-boundary test: $unsafe_mode $registry_target reached an external command" >&2
			exit 1
		fi
	done
done

# Regression for the 2026-07-29 incident: under a pipeline-shaped
# environment the guards legitimately pass, so the inert recipes are the
# last line of defense — the invocation must still fail rather than publish.
if PATH="$test_root/bin:$PATH" MAKEFLAGS="RELEASE_PIPELINE_ENTRY=release" MAKELEVEL=4 RELEASE_PIPELINE_ENTRY=release \
	make -s -C "$test_root" -f Makefile _release-publish >/dev/null 2>&1; then
	echo "check-release-boundary test: pipeline-shaped env let the publication recipe succeed" >&2
	exit 1
fi
if [ -e "$test_root/guard-leaked" ]; then
	echo "check-release-boundary test: pipeline-shaped direct publication reached an external command" >&2
	exit 1
fi
rm -f "$test_root/guard-leaked"

# Origin and GitHub repository pins are structural release authority, not
# best-effort helpers or overrideable CLI context.
cp "$test_root/Makefile" "$test_root/Makefile.canonical"

for local_gate_mutation in missing_plugin partial_commit duplicate_test; do
	case "$local_gate_mutation" in
	missing_plugin)
		sed 's#	$(MAKE) plugin-check#	@true#' \
			"$test_root/Makefile.canonical" >"$test_root/Makefile"
		;;
	partial_commit)
		sed 's#	$(MAKE) plugin-check#	$(MAKE) commit-check#' \
			"$test_root/Makefile.canonical" >"$test_root/Makefile"
		;;
	duplicate_test)
		sed 's#	$(MAKE) plugin-check#	$(MAKE) plugin-check\
	$(MAKE) test#' \
			"$test_root/Makefile.canonical" >"$test_root/Makefile"
		;;
	esac
	if "$checker" "$test_root" >/dev/null 2>&1; then
		echo "check-release-boundary test: $local_gate_mutation local release gate passed" >&2
		exit 1
	fi
done

# The pinned-variable guard is authority only while it is present on every
# guarded target and the machinery behind it is exact.
for pinned_guard_mutation in \
	'$(if $(release_overridden_vars),$(error release: release variables must not be overridden: $(release_overridden_vars)),)' \
	'$(if $(release_overridden_vars),$(error _release-run: release variables must not be overridden: $(release_overridden_vars)),)'
do
	grep -Fvx "	$pinned_guard_mutation" "$test_root/Makefile.canonical" >"$test_root/Makefile"
	if "$checker" "$test_root" >/dev/null 2>&1; then
		echo "check-release-boundary test: dropped pinned-variable guard passed: $pinned_guard_mutation" >&2
		exit 1
	fi
done

for pinned_machinery_mutation in release_pinned_vars release_overridden_vars; do
	grep -v "^override $pinned_machinery_mutation = " \
		"$test_root/Makefile.canonical" >"$test_root/Makefile"
	if "$checker" "$test_root" >/dev/null 2>&1; then
		echo "check-release-boundary test: missing $pinned_machinery_mutation authority passed" >&2
		exit 1
	fi
done

sed 's#^override release_pinned_vars = .*#override release_pinned_vars = RELEASE_TARGETS#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: a narrowed pinned-variable list passed" >&2
	exit 1
fi

# The live smoke's strictness must be passed explicitly, never inherited.
for smoke_pin_mutation in \
	's# SPX_EXPECTED_REACHABLE=1##' \
	's#SPX_EXPECTED_REACHABLE=1#SPX_EXPECTED_REACHABLE=$(SPX_EXPECTED_REACHABLE)#'
do
	sed "$smoke_pin_mutation" "$test_root/Makefile.canonical" >"$test_root/Makefile"
	if "$checker" "$test_root" >/dev/null 2>&1; then
		echo "check-release-boundary test: unpinned release smoke passed: $smoke_pin_mutation" >&2
		exit 1
	fi
done

# The published inventory must be proved before the tag is public and again
# before the GitHub release is created.
awk '
	$0 == "\t@$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION) || { \\" {
		for (i = 0; i < 3; i++) {
			getline discarded
		}
		next
	}
	{ print }
' "$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: release body without a pre-tag inventory proof passed" >&2
	exit 1
fi

awk '
	$0 == "\t@$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION) || { \\" {
		in_inventory = 1
	}
	in_inventory && $0 == "\t\texit 1; \\" {
		print "\t\ttrue; \\"
		in_inventory = 0
		next
	}
	{ print }
' "$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: best-effort inventory cleanup block passed" >&2
	exit 1
fi

# Moving the proof after the atomic push restores exactly the defect: the tag
# is public before the artifact set is known to be complete.
awk '
	$0 == "\t@$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION) || { \\" {
		held = $0
		for (i = 0; i < 3; i++) {
			getline extra
			held = held "\n" extra
		}
		next
	}
	{ print }
	$0 == "\tgit push --no-follow-tags --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)" && held != "" {
		print held
		held = ""
	}
' "$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: inventory proof after the atomic tag push passed" >&2
	exit 1
fi

grep -Fvx '	$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION)' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: publication without an inventory proof passed" >&2
	exit 1
fi

# The gate's matrix and the Makefile's must be one decision, not two.
cp "$test_root/Makefile.canonical" "$test_root/Makefile"
cp "$test_root/scripts/check-release-payload-inventory.sh" "$test_root/inventory.canonical"
sed 's#^CANONICAL_RELEASE_TARGETS=.*#CANONICAL_RELEASE_TARGETS="linux-amd64"#' \
	"$test_root/inventory.canonical" >"$test_root/scripts/check-release-payload-inventory.sh"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: inventory matrix drifted from RELEASE_TARGETS and passed" >&2
	exit 1
fi
rm -f "$test_root/scripts/check-release-payload-inventory.sh"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: missing published-inventory gate passed" >&2
	exit 1
fi
cp "$test_root/inventory.canonical" "$test_root/scripts/check-release-payload-inventory.sh"
chmod 0755 "$test_root/scripts/check-release-payload-inventory.sh"
rm -f "$test_root/inventory.canonical"

# Recovery must execute the current committed controller while keeping the
# historical release commit as immutable source and CI authority.
sed 's#git worktree add --detach "$$controller_wt" "$$controller_sha"#git worktree add --detach "$$controller_wt" "$$release_sha"#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: release tag became the recovery controller" >&2
	exit 1
fi

sed 's#-sha "$$release_sha" -branch#-sha "$$(git rev-parse HEAD)" -branch#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: controller HEAD became historical CI authority" >&2
	exit 1
fi

sed 's#claude plugin tag "$(RELEASE_SOURCE_DIR)"#claude plugin tag .#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: resume plugin tag used controller source" >&2
	exit 1
fi

sed 's# RELEASE_SOURCE_DIR="$(RELEASE_SOURCE_DIR)"; \\#; \\#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: resume publication omitted immutable source" >&2
	exit 1
fi

sed 's#"$(RELEASE_VERSION)" CHANGELOG.md "$$changelog"#"$(RELEASE_VERSION)" "$$RELEASE_SOURCE_DIR/CHANGELOG.md" "$$changelog"#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: release notes bypassed immutable tag blobs" >&2
	exit 1
fi

sed 's#"$(RELEASE_VERSION)" server.json "$$template"#"$(RELEASE_VERSION)" ./server.json "$$template"#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: registry metadata bypassed immutable tag blob" >&2
	exit 1
fi

# The final pre-publication gates must not merely be present: their
# continuation body must delete the recoverable local tag and return failure.
awk '
	$0 == "\t@$(MAKE) release-ci-wait || { \\" {
		in_final_ci = 1
	}
	in_final_ci && $0 == "\t\texit 1; \\" {
		print "\t\ttrue; \\"
		in_final_ci = 0
		next
	}
	{ print }
' "$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: best-effort final CI cleanup block passed" >&2
	exit 1
fi

# Expansion-time Make guards are authority only while they precede all
# executable work in the public release and recovery targets.
for reordered_target in \
	release release-resume _release-run _release-resume-run \
	_release-publish _release-ci-wait-historical \
	registry-publish registry-publish-verify-first
do
	awk -v target="$reordered_target" '
		index($0, target ":") == 1 {
			print
			print "\t@true"
			next
		}
		{ print }
	' "$test_root/Makefile.canonical" >"$test_root/Makefile"
	if "$checker" "$test_root" >/dev/null 2>&1; then
		echo "check-release-boundary test: reordered $reordered_target Make-context guards passed" >&2
		exit 1
	fi
done

for make_failure_mask in '.ONESHELL:' '.IGNORE:' '.IGNORE: _release-run'; do
	{
		printf '%s\n' "$make_failure_mask"
		sed -n '1,$p' "$test_root/Makefile.canonical"
	} >"$test_root/Makefile"
	if "$checker" "$test_root" >/dev/null 2>&1; then
		echo "check-release-boundary test: $make_failure_mask failure masking passed" >&2
		exit 1
	fi
done

sed 's#git push --no-follow-tags origin HEAD:#git push origin HEAD:#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: candidate push inherited push.followTags" >&2
	exit 1
fi

sed 's#git push --no-follow-tags --atomic origin#git push --atomic origin#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: atomic release push inherited push.followTags" >&2
	exit 1
fi

sed 's#git push --no-follow-tags origin "$$plugin_ref"#git push origin "$$plugin_ref"#g' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: plugin push inherited push.followTags" >&2
	exit 1
fi

sed 's#claude plugin tag \.#claude plugin tag . --push#g' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: claude plugin tag --push bypass passed" >&2
	exit 1
fi

sed 's#@./scripts/check-release-tag.sh --plugin "$(RELEASE_VERSION)"#@true#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: no-op plugin-tag candidate check passed" >&2
	exit 1
fi

sed 's#@./scripts/check-github-release.sh "$(RELEASE_VERSION)" "$(DIST_DIR)"#@true#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: no-op GitHub asset candidate check passed" >&2
	exit 1
fi

sed 's#./scripts/registry-publish-with-login.sh "$(MCP_PUBLISHER)" "$(DIST_DIR)/server.json"#true#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: unbound registry fallback publisher passed" >&2
	exit 1
fi

sed 's#$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)#@true#g' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: registry publication without exact-SHA authority passed" >&2
	exit 1
fi

# Git itself must honor the pinned option even when user config requests
# reachable annotated tags. Only main and the explicitly named release tag
# may appear on the bare remote.
follow_remote="$test_root/follow-remote.git"
follow_work="$test_root/follow-work"
git init --bare -q "$follow_remote"
git init -q "$follow_work"
git -C "$follow_work" config user.name "Canary Boundary Test"
git -C "$follow_work" config user.email "boundary-test@example.invalid"
git -C "$follow_work" remote add origin "$follow_remote"
printf '%s\n' base >"$follow_work/evidence.txt"
git -C "$follow_work" add evidence.txt
git -C "$follow_work" commit -q -m base
git -C "$follow_work" tag -a unrelated-v0 -m unrelated-v0
printf '%s\n' candidate >>"$follow_work/evidence.txt"
git -C "$follow_work" commit -q -am candidate
git -C "$follow_work" -c push.followTags=true \
	push --no-follow-tags origin HEAD:main >/dev/null
if git --git-dir="$follow_remote" show-ref --verify --quiet refs/tags/unrelated-v0; then
	echo "check-release-boundary test: candidate push leaked unrelated annotated tag" >&2
	exit 1
fi
git -C "$follow_work" tag -a v1.2.3 -m v1.2.3
git -C "$follow_work" -c push.followTags=true \
	push --no-follow-tags --atomic origin HEAD:main v1.2.3 >/dev/null
if ! git --git-dir="$follow_remote" show-ref --verify --quiet refs/tags/v1.2.3; then
	echo "check-release-boundary test: atomic release push omitted named tag" >&2
	exit 1
fi
if git --git-dir="$follow_remote" show-ref --verify --quiet refs/tags/unrelated-v0; then
	echo "check-release-boundary test: atomic release push leaked unrelated annotated tag" >&2
	exit 1
fi

sed 's#@./scripts/check-release-origin.sh#@true#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: no-op release-origin-check target passed" >&2
	exit 1
fi

sed 's#--repo github.com/osauer/canary#--repo github.com/osauer/canary --repo other/canary#' \
	"$test_root/Makefile.canonical" >"$test_root/Makefile"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: duplicated gh --repo override passed" >&2
	exit 1
fi
mv "$test_root/Makefile.canonical" "$test_root/Makefile"

cat > "$test_root/scripts/rogue.sh" <<'EOF'
#!/bin/sh
git push origin v1.2.3
EOF
chmod 0755 "$test_root/scripts/rogue.sh"
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: rogue script publication path passed" >&2
	exit 1
fi
rm "$test_root/scripts/rogue.sh"

cat >> "$test_root/Makefile" <<'EOF'
release-helper:
	gh release create v1.2.3
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: rogue Makefile publication target passed" >&2
	exit 1
fi

cat > "$test_root/Makefile" <<'EOF'
.PHONY: release release-publish
release-publish:
	gh release create v1.2.3
release:
	$(MAKE) release-publish
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: public release-publish authority passed" >&2
	exit 1
fi

cat > "$test_root/Makefile" <<'EOF'
_release-run:
	git tag -a v1.2.3 -m fixture
release:
	$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: unguarded internal pipeline body passed" >&2
	exit 1
fi

# The pre-worktree shape — publication commands directly in `release` —
# must now fail: authority lives in the guarded worktree body.
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
release:
	git tag -a v1.2.3 -m fixture
	git push origin v1.2.3
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: publication commands in release target passed" >&2
	exit 1
fi

# A release body may not omit the exact-SHA Actions gate.
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
_release-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	git push origin HEAD:$(MAIN_BRANCH)
	$(MAKE) release-main-candidate-check
	@msg=fixture; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	git push --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: release body without release-ci-wait passed" >&2
	exit 1
fi

# The gate cannot run before the candidate push that creates the authoritative
# push-to-main workflow runs.
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
_release-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	$(MAKE) release-ci-wait
	git push origin HEAD:$(MAIN_BRANCH)
	$(MAKE) release-main-candidate-check
	@msg=fixture; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	git push --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: release-ci-wait before candidate push passed" >&2
	exit 1
fi

# The gate must run before the first annotated tag exists, not merely somewhere
# inside the canonical release target.
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
_release-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	git push origin HEAD:$(MAIN_BRANCH)
	$(MAKE) release-main-candidate-check
	@msg=fixture; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	$(MAKE) release-ci-wait
	git push --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: release-ci-wait after tag passed" >&2
	exit 1
fi

# A lexically present gate that is unreachable or invoked with make's
# ignore-errors flag is not release authority.
for bypass in unreachable ignore_errors; do
	case "$bypass" in
		unreachable) gate_line='@true || $(MAKE) release-ci-wait' ;;
		ignore_errors) gate_line='@$(MAKE) -i release-ci-wait' ;;
	esac
	cat > "$test_root/Makefile" <<EOF
_release-publish:
	@if [ "\$(MAKELEVEL)" -lt 1 ] || [ "\$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
_release-run:
	@if [ "\$(MAKELEVEL)" -lt 1 ] || [ "\$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	git push origin HEAD:\$(MAIN_BRANCH)
	$gate_line
	\$(MAKE) release-main-candidate-check
	@msg=fixture; \\
	git tag -a \$(RELEASE_VERSION) -m "\$\$msg"
	git push --atomic origin HEAD:\$(MAIN_BRANCH) \$(RELEASE_VERSION)
	\$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	\$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
EOF
	if "$checker" "$test_root" >/dev/null 2>&1; then
		echo "check-release-boundary test: $bypass release-ci-wait counted as authority" >&2
		exit 1
	fi
done

# Trailing comments are documentation, never executable release authority.
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
_release-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	@true # git push origin HEAD:$(MAIN_BRANCH)
	$(MAKE) release-ci-wait
	$(MAKE) release-main-candidate-check
	@msg=fixture; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	git push --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: trailing-comment candidate push counted as authority" >&2
	exit 1
fi

# The recovery lane must independently re-prove the tagged SHA's Actions
# evidence before any post-tag publication.
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
_release-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	git push origin HEAD:$(MAIN_BRANCH)
	$(MAKE) release-ci-wait
	$(MAKE) release-main-candidate-check
	@msg=fixture; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	git push --atomic origin HEAD:$(MAIN_BRANCH) $(RELEASE_VERSION)
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
_release-resume-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release-resume" ]; then exit 1; fi
	claude plugin tag . --push
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release-resume:
	$(MAKE) _release-resume-run RELEASE_PIPELINE_ENTRY=release-resume
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: resume without exact-SHA Actions gate passed" >&2
	exit 1
fi

# The release tag must be pushed atomically with a non-force reassertion of
# the candidate main ref, closing the final concurrent-main race.
cat > "$test_root/Makefile" <<'EOF'
_release-publish:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	gh release create v1.2.3
_release-run:
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then exit 1; fi
	git push origin HEAD:$(MAIN_BRANCH)
	$(MAKE) release-ci-wait
	$(MAKE) release-main-candidate-check
	@msg=fixture; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	git push origin $(RELEASE_VERSION)
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release
release:
	$(MAKE) _release-run RELEASE_PIPELINE_ENTRY=release
EOF
if "$checker" "$test_root" >/dev/null 2>&1; then
	echo "check-release-boundary test: non-atomic release tag push passed" >&2
	exit 1
fi

echo "check-release-boundary test: OK"
