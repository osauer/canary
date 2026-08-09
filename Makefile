.DEFAULT_GOAL := help

# `--match='v*'` excludes the `canary--vX.Y.Z` plugin tags created by
# `claude plugin tag` so the binary always stamps itself with the
# nearest binary-release tag (e.g. v0.4.4) and not the lexicographically
# earlier plugin tag at the same commit.
VERSION  ?= $(shell git describe --tags --match='v*' --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse HEAD 2>/dev/null || echo none)
# Dev builds stamp the HEAD commit date, not wall-clock build time, so an
# unchanged tree rebuilds to a byte-identical binary — that identity is
# what lets install/restart-daemon skip the daemon bounce when nothing
# changed. Releases already stamp the tag's commit date (RELEASE_DATE in
# release-binaries), so this matches the release convention rather than
# diverging from it. Falls back to wall clock outside a git checkout.
DATE     ?= $(shell TZ=UTC git show -s --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD 2>/dev/null || TZ=UTC date +%Y-%m-%dT%H:%M:%SZ)

# `-s -w` strip the external symbol table and DWARF debug info. Cuts the
# binary by ~32% (9.6 MB → 6.5 MB on darwin/arm64). Go runtime keeps its
# own function metadata so panic stack traces remain readable; what's
# lost is delve symbolication, `go tool nm`/`objdump`, and external
# profilers that read external symbols. Startup time is unchanged —
# this is a size optimisation, not a speed one.
STRIP_LDFLAGS = -s -w

LDFLAGS = $(STRIP_LDFLAGS) -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
# Local builds are broker-write capable by default (2026-06-10 decision):
# the developer machine always gets the trading binary, so a plain
# `make install` can no longer silently downgrade a trading install.
# Public release artefacts build BOTH variants explicitly in
# scripts/build-release-target.sh; `make build GO_TAGS=""` still produces
# a read-only binary (and says so via the banner below).
GO_TAGS ?= trading
GO_BUILD_TAGS = $(if $(strip $(GO_TAGS)),-tags '$(GO_TAGS)',)

# Install location for `make install`. Defaults to ~/.local/bin (XDG
# user-local convention; usually already on PATH). Override for a system
# install: make install PREFIX=/usr/local (needs sudo). Note: $GOBIN is
# the wrong target here — that's a Go-developer convention for source
# tools, but Canary is an end-user CLI binary and shouldn't require Go to
# be installed at runtime.
PREFIX ?= $(HOME)/.local
RESTART_TIMEOUT ?= 15s

CLAUDE_DIR ?= $(HOME)/.claude
CLAUDE_PLUGIN_ID ?= canary@canary
CLAUDE_PLUGIN_MARKETPLACE ?= $(CURDIR)
SKILL_DIR  ?= $(CLAUDE_DIR)/skills/canary
CODEX_DIR  ?= $(HOME)/.codex
CODEX_SKILL_DIR ?= $(CODEX_DIR)/skills/canary
SKILL_SRC  ?= skills/canary

# v2 remains the maintained N-1 line while v3 develops and stabilizes. Every
# other major releases from main; callers cannot retarget this pinned variable.
MAIN_BRANCH ?= $(if $(filter v2.%,$(RELEASE_VERSION)),release/2.x,main)
RELEASE_SOURCE_MODE ?= controller
RELEASE_CI_POLL ?= 15s
RELEASE_CI_TIMEOUT ?= 30m
RELEASE_CONTROLLER_CONTRACT = release-controller-v1
override release_first_makeflag = $(firstword $(MAKEFLAGS))
override release_compact_makeflags = $(if $(filter --%,$(release_first_makeflag)),,$(if $(findstring =,$(release_first_makeflag)),,$(release_first_makeflag)))
override release_unsafe_makeflags = $(strip $(filter -i --ignore-errors -k --keep-going,$(MAKEFLAGS)) $(if $(findstring i,$(release_compact_makeflags)),i) $(if $(findstring k,$(release_compact_makeflags)),k) $(if $(findstring n,$(release_compact_makeflags)),n) $(if $(findstring t,$(release_compact_makeflags)),t))
# Variables that are constants of the release contract, not caller knobs. A
# command-line or environment assignment beats a makefile one and propagates
# into every sub-make, so each of these can narrow what a release proves while
# every downstream gate still reports success: RELEASE_TARGETS shrinks the
# artifact matrix that assembly then treats as complete, SPX_EXPECTED_REACHABLE
# and SMOKE_STRICT disarm the binding live smoke, MAIN_BRANCH retargets the ref
# the candidate lands on and that exact-SHA CI evidence is read for, GO_TAGS
# decides whether the smoked binary can reach the broker write path at all, and
# STRIP_LDFLAGS is injected verbatim into the published binaries' link flags.
# GO_BUILD_TAGS and LDFLAGS are pinned with them because a derived variable
# that can be overridden directly is a way around the source it derives from,
# and RELEASE_SOURCE_MODE because it selects which commit a publication helper
# proves it is running from. Guarded release targets therefore reject any origin
# but this makefile, the same expansion-time treatment MAKE and MAKEFLAGS
# already get.
override release_pinned_vars = RELEASE_TARGETS SPX_EXPECTED_REACHABLE SMOKE_STRICT MAIN_BRANCH GO_TAGS GO_BUILD_TAGS STRIP_LDFLAGS LDFLAGS RELEASE_SOURCE_MODE
override release_overridden_vars = $(strip $(foreach release_pinned_var,$(release_pinned_vars),$(if $(filter file,$(origin $(release_pinned_var))),,$(release_pinned_var))))
MCP_PUBLISHER ?= $(if $(wildcard bin/mcp-publisher),bin/mcp-publisher,mcp-publisher)
RELEASE_WORKTREE_ROOT ?= $(abspath $(CURDIR)/..)
MCP_REGISTRY_AUTO_LOGIN ?= 1
MCP_REGISTRY_LOGIN_METHOD ?= github

.PHONY: help reduction-metrics reduction-metrics-check build install restart-daemon uninstall test test-pkg test-support test-internal test-daemon test-daemon-default test-daemon-trading test-integration test-integration-live trading-package-scope-check clean install-plugin install-plugin-refresh install-skill uninstall-skill all check commit-check product-identity-check go-doc-check gofmt-check vet-check staticcheck-check govulncheck-check fmt app-check app-log-contract-check scheduled-monitor-check app-contract-check app-syntax-check app-browser-helper-check app-auth-check app-behavior-check app-active-alert-inbox-check app-alert-compat-check app-market-events-check app-service-worker-check app-render-check remote-relay-check release-packaging-check app-refresh app-refresh-smoke app-smoke release _release-run _release-publish release-resume _release-resume-run release-binaries release-mcpb release-checksums release-payload-inventory-check release-registry-server registry-login release-auth-preflight release-origin-check release-ci-wait _release-ci-wait-historical release-main-candidate-check release-source-candidate-check release-controller-source-check release-source-mode-check release-tag-candidate-check release-plugin-tag-candidate-check release-github-candidate-check release-github-assets registry-publish registry-publish-verify-first release-verify release-smoke release-site-check smoke smoke-build smoke-only smoke-fast version plugin-check parity-check modernize modernize-check refresh-spx-members hook-version-check registry-version-check changelog-check changelog-lint changelog-lint-historical docs-html-check pages-build account-data-check hook-behavior-check agent-config-check

help: ## List available targets
	@awk 'BEGIN {FS = ":.*##"; print "Available targets (default: help):\n"} \
		/^[a-zA-Z][a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)
	@echo
	@echo "Common flow:  make fmt && make test && make build   (test already runs check)"
	@echo "Daemon flow:  make install restart-daemon   (FORCE=1 adds canary restart --force; refreshes any running app)"
	@echo "Release flow: make release RELEASE_VERSION=vX.Y.Z   (clean committed HEAD; origin/$(MAIN_BRANCH) not ahead)"
	@echo "              tags + pushes + cross-compiles + creates GitHub Release with binaries attached"

reduction-metrics: ## Measure committed files and maintained LOC against the v3 baseline
	@./scripts/reduction-metrics.sh

reduction-metrics-check: ## Verify the locked v3 reduction baseline remains reproducible
	@./scripts/reduction-metrics.sh --verify-baseline c3ec1d81c8537c8b982791e6fdc78f3da4e2c28e >/dev/null

build: ## Compile the canonical bin/canary executable
	@mkdir -p bin
	@rm -f bin/ibkr
	go build $(GO_BUILD_TAGS) -ldflags '$(LDFLAGS)' -o bin/canary ./cmd/canary
	@case " $(GO_TAGS) " in (*" trading "*) ;; (*) \
		echo "NOTE: built WITHOUT broker-write capability (read-only daemon)."; \
		echo "      Installing this over a trading build silently downgrades it."; \
		echo "      For order placement build with: make install GO_TAGS=trading"; \
	;; esac

install: build ## Install only the canonical canary executable to $(PREFIX)/bin
	@install -d "$(PREFIX)/bin"
	@for path in "$(PREFIX)/bin/canary" "$(PREFIX)/bin/ibkr"; do \
		if [ -e "$$path" ] || [ -L "$$path" ]; then \
			if [ ! -f "$$path" ] || [ -L "$$path" ]; then \
				echo "install: refusing executable path $$path because it is not a regular file" >&2; \
				exit 1; \
			fi; \
		fi; \
	done
	@if cmp -s bin/canary "$(PREFIX)/bin/canary"; then \
		echo "canary unchanged at $(PREFIX)/bin/canary — skipping copy"; \
	else \
		stage="$(PREFIX)/bin/.canary-install.$$$$"; \
		trap 'rm -f "$$stage"' EXIT HUP INT TERM; \
		install -m 0755 bin/canary "$$stage" && \
		mv -f "$$stage" "$(PREFIX)/bin/canary" && \
		echo "Installed canary to $(PREFIX)/bin"; \
	fi
	@rm -f "$(PREFIX)/bin/ibkr" "$(PREFIX)/bin/canary.bak" "$(PREFIX)/bin/ibkr.bak"
	@echo "Restart the daemon and any running app with: $(PREFIX)/bin/canary restart"

# Skip the bounce when the freshly-built binary is byte-identical to the
# installed one AND the canonical daemon is already running — restarting
# then buys nothing, disturbs other sessions' in-flight CLI calls, and
# re-rolls the TWS client-slot retention race on the pinned client ID.
# Byte-identity is meaningful because DATE stamps the commit date (above),
# so unchanged source ⇒ unchanged binary. A daemon that is NOT running is
# left alone too (the next CLI call autospawns it — that is the design).
# FORCE=1 always installs and restarts.
restart-daemon: build ## Install + restart daemon, skipped when the binary is unchanged (FORCE=1 always bounces)
	@if [ -z "$(FORCE)" ] && cmp -s bin/canary "$(PREFIX)/bin/canary" && \
		pgrep -f "$(PREFIX)/bin/canary daemon" >/dev/null 2>&1; then \
		echo "binary unchanged and daemon running — skipping restart (FORCE=1 to bounce anyway)"; \
	else \
		$(MAKE) --no-print-directory install && \
		"$(PREFIX)/bin/canary" restart --timeout $(RESTART_TIMEOUT) $(if $(FORCE),--force,); \
	fi

APP_SMOKE_URL ?= http://127.0.0.1:8765
APP_SMOKE_BROWSER ?= chromium
app-check: app-log-contract-check app-contract-check app-syntax-check app-browser-helper-check app-auth-check app-behavior-check app-active-alert-inbox-check app-alert-compat-check app-market-events-check app-service-worker-check ## Fast app gate: production logging + SPA contracts

app-log-contract-check: ## Reject production app log emitters without an explicit severity
	go run ./scripts/app-log-audit -root .
	go test ./scripts/app-log-audit

scheduled-monitor-check: ## Verify bounded, redacted scheduled log classification
	go test ./scripts/log-monitor

# Go embedding accepts arbitrary bytes: a syntax error in app.js or
# service-worker.js still compiles, passes the substring-based contract
# tests, and ships a dead PWA (2026-07-12 review, F-01). This is the one
# non-Go binding dependency in `make check` — deliberate: a gate that
# self-skips when node is missing is not a gate. GitHub-hosted runners
# ship node, and local browser smokes already require it.
app-syntax-check: ## Embedded PWA assets parse: all web/app/*.js (node --check) + manifest JSON
	@command -v node >/dev/null 2>&1 || { echo "app-syntax-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	@found=0; for file in web/app/*.js; do \
		[ -f "$$file" ] || continue; \
		found=1; \
		node --check "$$file" || exit; \
	done; \
	[ "$$found" -eq 1 ] || { echo "app-syntax-check: no web/app/*.js files found" >&2; exit 1; }
	@node -e 'JSON.parse(require("fs").readFileSync("web/app/manifest.webmanifest","utf8"))'
	@node scripts/check-app-icons.mjs

app-browser-helper-check: ## Browser launcher fails safely inside the macOS Codex sandbox
	@command -v node >/dev/null 2>&1 || { echo "app-browser-helper-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	node --test scripts/lib-app-browser_test.mjs

app-auth-check: ## Execute browser credential-storage and crypto-less pairing contracts
	@command -v node >/dev/null 2>&1 || { echo "app-auth-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	node --test web/app/test/auth-credential.test.mjs

app-behavior-check: ## Execute production SPA modules for typed state, privacy, currency, protection, governance, and brief behavior
	@command -v node >/dev/null 2>&1 || { echo "app-behavior-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	node --test web/app/test/production-behavior.test.mjs

app-active-alert-inbox-check: ## Execute the sole active alert inbox and unread contract
	@command -v node >/dev/null 2>&1 || { echo "app-active-alert-inbox-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	node --test web/app/test/active-alert-inbox.test.mjs

app-alert-compat-check: ## Preserve notification settings and device-subscription contracts
	@command -v node >/dev/null 2>&1 || { echo "app-alert-compat-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	node --test web/app/test/alert-compat.test.mjs

app-market-events-check: ## Execute market-event exposure relevance contracts
	@command -v node >/dev/null 2>&1 || { echo "app-market-events-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	node --test web/app/test/market-events.test.mjs

app-service-worker-check: ## Execute service-worker payload and fixed-navigation contracts in a Node VM
	@command -v node >/dev/null 2>&1 || { echo "app-service-worker-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	node --test web/app/test/service-worker.test.mjs

# The hosted transport relay is a production component (architecture.md)
# whose test suite was previously invoked by no gate — repo-wide green
# said nothing about it. The tests import only node: builtins plus the
# local worker module, so bare `node --test` mirrors the package's
# `npm test` without adding npm to the binding dependency surface.
remote-relay-check: ## Cloudflare remote-relay unit tests (node --test, no npm needed)
	@command -v node >/dev/null 2>&1 || { echo "remote-relay-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	cd cloudflare/remote-relay && node --test test/*.test.js

release-packaging-check: ## Verify tag-isolated assembly, archive contents, release-pinned links, and release-gate fixtures
	./scripts/check-release-packaging.sh
	@# release-site-check itself is release-time only (it needs RELEASE_VERSION
	@# and reads the real tree), so its fixture runs here to stay inside `check`.
	@./scripts/check-release-site-sync_test.sh
	@./scripts/canary-mcp_test.sh

# Static drift gate between the Playwright app scripts and the SPA they
# assert against, plus the other web/app contract tests. Born of the
# 0574bd3 incident (2026-06-09): risk-plan ids were removed from
# index.html while app-browser-smoke.mjs kept asserting them — the
# browser smoke sat red for two days and v1.9.0 shipped anyway, because
# the browser smokes run outside check/test/release. Pure Go (no node,
# no Playwright, no running app), so it lives in `make check`; see
# TestBrowserScriptIDsMatchSPA in web/app/browser_script_ids_test.go.
app-contract-check: ## Browser-script ↔ SPA element-id drift gate + static app contracts (pure Go)
	go test ./web/app

app-refresh: install ## Install, restart the shared app host, and print a local pairing URL
	"$(PREFIX)/bin/canary" restart --app --timeout $(RESTART_TIMEOUT)
	@for i in $$(seq 1 60); do \
		if curl -fsS $(APP_SMOKE_URL)/manifest.webmanifest >/dev/null 2>&1; then break; fi; \
		sleep 0.5; \
	done
	"$(PREFIX)/bin/canary" app pair --public-url $(APP_SMOKE_URL) --json

app-refresh-smoke: app-refresh ## Refresh the shared app host, then run the browser app smoke
	$(MAKE) app-smoke APP_SMOKE_URL=$(APP_SMOKE_URL) APP_SMOKE_BROWSER=$(APP_SMOKE_BROWSER)

app-smoke: ## Browser-smoke a running Canary app without scanning a QR code
	node scripts/app-browser-smoke.mjs --base-url $(APP_SMOKE_URL) --browser $(APP_SMOKE_BROWSER) --no-notification

app-render-check: ## Hermetic production-app render with synthetic pairing/reload/auth recovery; never reads desk account data
	PLAYWRIGHT_NODE_MODULES="$(CURDIR)/web/app/node_modules" node scripts/app-browser-smoke.mjs \
		--browser $(APP_SMOKE_BROWSER) \
		--round4-synthetic=true

uninstall: ## Remove Canary and any pre-upgrade executable residue from $(PREFIX)/bin
	rm -f "$(PREFIX)/bin/canary" "$(PREFIX)/bin/ibkr" "$(PREFIX)/bin/canary.bak" "$(PREFIX)/bin/ibkr.bak"
	@echo "Removed Canary executables and pre-upgrade residue from $(PREFIX)/bin"

TEST_JOBS ?= 3
TEST_MAKEFLAGS = $(if $(filter 0,$(MAKELEVEL)),-j$(TEST_JOBS),)
test: ## Full gate: check + hermetic app render + pkg, command/support, and daemon/integration tests (-race), overlapped by default
	$(MAKE) $(TEST_MAKEFLAGS) check app-render-check test-pkg test-support test-daemon

# Compatibility spelling retained for older contributor and agent workflows.
# The reduced v3 tree has one canonical repository gate.
commit-check: check ## Compatibility alias for the canonical repository gate

# Binding pre-commit gate: agent config/hooks + formatting + go vet +
# staticcheck + govulncheck + plugin manifest validation. Fails on stdlib
# vulnerabilities too — keep Go patched.
# staticcheck and govulncheck are pinned as go.mod tool dependencies and
# invoked via `go tool`, so CI and local runs use the same versions.
#
# CHECK_DEPS gates the optional pieces of the check matrix. Default is the
# full strict gate (plugin-check + parity-check). CI without the `claude`
# CLI on PATH overrides with CHECK_DEPS=parity-check — the MCP↔CLI drift
# gate (parity-check) is what we cannot skip; plugin-manifest validation
# is recoverable because the schema is small and changes go through PR
# review anyway.
CHECK_DEPS ?= plugin-check parity-check
CHECK_JOBS ?= 8
CHECK_TARGETS = $(CHECK_DEPS) agent-config-check reduction-metrics-check modernize-check docs-check docs-html-check changelog-check account-data-check product-identity-check release-packaging-check app-log-contract-check scheduled-monitor-check app-contract-check app-syntax-check app-browser-helper-check app-auth-check app-behavior-check app-active-alert-inbox-check app-alert-compat-check app-market-events-check app-service-worker-check remote-relay-check go-doc-check gofmt-check vet-check staticcheck-check govulncheck-check
CHECK_MAKEFLAGS = $(if $(filter 0,$(MAKELEVEL)),-j$(CHECK_JOBS),)
check: ## agent config/hooks + Go docs/format/vet/staticcheck/vulns + modernize/plugin/parity/docs/changelog/account/app checks (binding pre-commit gate)
	$(MAKE) $(CHECK_MAKEFLAGS) $(CHECK_TARGETS)

product-identity-check: ## Reject retired product/module/site/CLI/MCP identities outside reviewed continuity exceptions
	@./scripts/check-product-identity.sh
	@./scripts/check-product-identity_test.sh

go-doc-check: ## Verify package and exported API documentation across all tracked Go build variants
	go run ./scripts/go-doc-audit -check

gofmt-check: ## Verify tracked / non-gitignored Go files are gofmt'd
	@# `gofmt -l .` walks every subdirectory and trips on gitignored paths
	@# (Claude Code agent worktrees, /dist, etc.). `git ls-files` respects
	@# .gitignore by listing tracked + untracked-but-not-ignored files —
	@# the right scope for a pre-commit format gate.
	@#
	@# Filter out paths git knows about but that don't exist on disk
	@# (staged-for-deletion mid-commit), otherwise gofmt prints
	@# `lstat …: no such file or directory` to stderr for each one.
	@unformatted=$$( \
		git ls-files --cached --others --exclude-standard '*.go' | \
		while IFS= read -r f; do [ -e "$$f" ] && printf '%s\n' "$$f"; done | \
		xargs gofmt -l \
	); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt: the following files need formatting:"; \
		echo "$$unformatted"; \
		echo "fix with: make fmt"; \
		exit 1; \
	fi

vet-check: ## Run go vet (both default and trading-tag builds)
	go vet ./...
	go vet -tags trading ./internal/... ./pkg/...

staticcheck-check: ## Run staticcheck
	go tool staticcheck ./...

# govulncheck's verdict is keyed on the dependency set + toolchain + the
# vulnerability DB — not on local code edits — so re-running it on every
# commit only pays cold-cache compile cost for the same answer. Skip when
# go.mod/go.sum/toolchain are unchanged AND a scan already passed today
# (the date bound keeps DB updates flowing in daily). The exact-SHA release
# authority runs on fresh CI runners with no stamp cache, so it always scans.
GOVULN_STAMP ?= $(HOME)/.cache/ibkr/govulncheck.stamp
govulncheck-check: ## Run govulncheck (skipped when deps unchanged and already scanned today; GOVULN_FORCE=1 forces)
	@depshash=$$( (cat go.mod go.sum 2>/dev/null; go version) | shasum -a 256 | cut -d' ' -f1); \
	today=$$(date +%Y-%m-%d); \
	if [ "$(GOVULN_FORCE)" != "1" ] && [ -r "$(GOVULN_STAMP)" ] && [ "$$(cat "$(GOVULN_STAMP)")" = "$$depshash $$today" ]; then \
		echo "govulncheck: deps/toolchain unchanged, already scanned today — skipping (GOVULN_FORCE=1 to force)"; \
	else \
		go tool govulncheck ./... && \
		mkdir -p "$$(dirname "$(GOVULN_STAMP)")" && \
		echo "$$depshash $$today" > "$(GOVULN_STAMP)"; \
	fi

# Validate the Claude Code plugin + marketplace manifests with the official
# `claude plugin validate` tool. The TestSkill* gates in internal/cli (run
# via parity-check) catch the prose-drift class `claude plugin validate`
# doesn't see (it checks the JSON, not SKILL.md).
plugin-check: ## Validate plugin/marketplace manifests with `claude plugin validate`
	@command -v claude >/dev/null 2>&1 || { echo "claude CLI not on PATH; install Claude Code or skip with: make check plugin-check= "; exit 1; }
	claude plugin validate .
	@$(MAKE) --no-print-directory hook-version-check
	@$(MAKE) --no-print-directory registry-version-check

# Root server.json is the MCP Registry template: release-registry-server
# reads its name/description and stamps version/packages from
# RELEASE_VERSION into dist/server.json. The checked-in version field is
# therefore never published — which is exactly how it drifted to 1.6.1
# while the plugin shipped 1.9.0. Pin it to plugin.json so the release
# version bump touches both files or fails the gate.
registry-version-check: ## Ensure server.json tracks plugin.json and stays registry-publishable
	@command -v jq >/dev/null 2>&1 || { echo "jq missing on PATH; install jq or skip"; exit 1; }
	@reg=$$(jq -r '.version // empty' server.json); \
	plg=$$(jq -r '.version // empty' .claude-plugin/plugin.json); \
	if [ -z "$$reg" ] || [ -z "$$plg" ] || [ "$$reg" != "$$plg" ]; then \
		echo "registry-version-check: server.json version ($$reg) != .claude-plugin/plugin.json version ($$plg); keep them in lockstep" >&2; \
		exit 1; \
	fi
	@# The MCP Registry caps the description at 100 characters. That limit used
	@# to be enforced only by release-registry-server, which runs from the TAG
	@# and therefore after tagging and after the GitHub release: v2.8.0 crossed
	@# the irreversible boundary with a 118-character description and could
	@# never be registered. Checking the working tree here puts the same limit
	@# in `make check`, so an over-long description fails before it is ever
	@# committed, let alone tagged.
	@desc=$$(jq -r '.description // empty' server.json); \
	len=$$(printf '%s' "$$desc" | wc -m | tr -d ' '); \
	if [ -z "$$desc" ]; then \
		echo "registry-version-check: server.json has no description; the MCP Registry requires one" >&2; \
		exit 1; \
	fi; \
	if [ "$$len" -gt 100 ]; then \
		echo "registry-version-check: server.json description is $$len characters; the MCP Registry caps it at 100" >&2; \
		echo "  $$desc" >&2; \
		exit 1; \
	fi

# The pre-tool-use hook is a broker guardrail with real routing logic
# (read-only allowlists, write gates, composition checks). It shipped for
# weeks blocking the read-only `canary orders` journal view, caught only by
# a human. Table-driven behavior cases keep both failure directions gated:
# false-allow (agent reaches a write) and false-block (read paths break).
hook-behavior-check: ## Run table-driven allow/block cases against the broker hooks
	@bash hooks/canary-pre-tool-use_test.sh
	@HOOK_UNDER_TEST=.codex/hooks/canary-pre-tool-use.sh bash hooks/canary-pre-tool-use_test.sh

agent-config-check: hook-behavior-check ## Validate project agent config, hooks, and read-only reviewer roles
	@bash -n hooks/canary-pre-tool-use.sh .codex/hooks/canary-pre-tool-use.sh
	@jq -e . .codex/hooks.json >/dev/null
	@jq -e . .claude/settings.json >/dev/null
	@jq -e . .claude/launch.json >/dev/null
	@sh -n .claude/preview-canary-app.sh
	@go test ./internal/agentconfig/
	@if command -v codex >/dev/null 2>&1; then \
		read_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- canary status --json | jq -r .decision); \
		write_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- canary order cancel ORDER_ID --json | jq -r .decision); \
		human_only_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- canary settings set trading.freeze=true | jq -r .decision); \
		commit_gate_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- make commit-check | jq -r .decision); \
		offline_gate_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- make check | jq -r .decision); \
		browser_gate_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- make app-render-check | jq -r .decision); \
		browser_script_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- node scripts/app-browser-smoke.mjs --browser chromium --round4-synthetic=true | jq -r .decision); \
		full_gate_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- make test | jq -r .decision); \
		live_gate_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- make restart-daemon | jq -r .decision); \
		smoke_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- make smoke | jq -r .decision); \
		release_decision=$$(codex execpolicy check --rules .codex/rules/canary.rules -- make release RELEASE_VERSION=v2.3.1 | jq -r .decision); \
		[ "$$read_decision" = allow ] && [ "$$write_decision" = prompt ] && [ "$$human_only_decision" = forbidden ] \
			&& [ "$$commit_gate_decision" = allow ] && [ "$$offline_gate_decision" = allow ] \
			&& [ "$$browser_gate_decision" = prompt ] && [ "$$browser_script_decision" = prompt ] && [ "$$full_gate_decision" = prompt ] \
			&& [ "$$live_gate_decision" = prompt ] && [ "$$smoke_decision" = prompt ] \
			&& [ "$$release_decision" = prompt ] || { \
			echo "execpolicy decisions: read=$$read_decision write=$$write_decision human-only=$$human_only_decision commit-gate=$$commit_gate_decision offline-gate=$$offline_gate_decision browser-gate=$$browser_gate_decision browser-script=$$browser_script_decision full-gate=$$full_gate_decision live-gate=$$live_gate_decision smoke=$$smoke_decision release=$$release_decision" >&2; exit 1; \
		}; \
	fi

# Drift gate for the session-start hook's fallback plugin version. When
# CLAUDE_PLUGIN_ROOT is unset the hook compares the binary against this
# hardcoded constant instead of plugin.json, and its skew warning keys on
# major.minor — so major.minor is what must stay in lockstep. The old
# "bump it manually at release time" convention drifted (constant 1.0.3
# vs plugin 1.8.0), hence a gate. Fails closed if the assignment line is
# missing or duplicated, so restructuring the hook can't silently skip it.
hook-version-check: ## Ensure session-start.sh fallback version tracks .claude-plugin/plugin.json (major.minor)
	@command -v jq >/dev/null 2>&1 || { echo "jq missing on PATH; install jq or skip"; exit 1; }
	@fallback=$$(sed -n 's/.*&& plugin_semver="\([0-9][0-9.]*\)".*/\1/p' hooks/session-start.sh); \
	count=$$(printf '%s\n' "$$fallback" | grep -c .); \
	if [ "$$count" -ne 1 ]; then \
		echo "hook-version-check: expected exactly one fallback plugin_semver=\"X.Y.Z\" assignment in hooks/session-start.sh, found $$count" >&2; \
		echo "update the extraction pattern in this target if the hook was restructured" >&2; \
		exit 1; \
	fi; \
	plugin=$$(jq -r '.version // empty' .claude-plugin/plugin.json); \
	fb_mm=$$(printf '%s' "$$fallback" | awk -F. 'NF>=2 {print $$1 "." $$2}'); \
	plg_mm=$$(printf '%s' "$$plugin" | awk -F. 'NF>=2 {print $$1 "." $$2}'); \
	if [ -z "$$fb_mm" ] || [ -z "$$plg_mm" ]; then \
		echo "hook-version-check: could not parse major.minor (fallback=$$fallback, plugin.json=$$plugin)" >&2; \
		exit 1; \
	fi; \
	if [ "$$fb_mm" != "$$plg_mm" ]; then \
		echo "hooks/session-start.sh fallback plugin_semver and .claude-plugin/plugin.json disagree on major.minor:" >&2; \
		echo "  fallback: $$fallback (major.minor $$fb_mm)" >&2; \
		echo "  plugin:   $$plugin (major.minor $$plg_mm)" >&2; \
		echo "bump the constant in hooks/session-start.sh to match plugin.json" >&2; \
		exit 1; \
	fi

# Drift gate for the MCP surface: TestParity in internal/mcp asserts that
# every cli.Commands() entry has a matching ibkr_<name> MCP tool (or is on
# the documented exclude list). TestStreamingParity is the streaming-
# resource counterpart — it pins the canary://… template inventory the
# server actually exposes. TestSkill* in internal/cli is the skill-layer
# counterpart: every CLI command documented in skills/canary/SKILL.md (or
# excluded with a reason), the allowed-tools list mirrored exactly in
# settings/canary.settings.json, and no broker/state write allowlisted.
# Cheap enough to live in the pre-commit gate.
parity-check: ## Verify MCP tool inventory matches the CLI surface
	go test -run 'TestParity|TestStreamingParity|TestNoTradingTools|TestSchemasAreValidJSON' ./internal/mcp/
	go test -run 'TestSkill|TestAgentPolicy' ./internal/cli/

# Idiom-drift gate. `go fix -diff` is the toolchain-native fixer (tracks the
# Go version pinned in go.mod); `go tool modernize` runs the broader gopls
# analyzer suite (range N, wg.Go, b.Loop, maps.Copy, SplitSeq, new(expr), …).
# Version of modernize is pinned via the `tool` directive in go.mod, so this
# gate is reproducible without an `@latest` install step.
#
# Stream discipline + chatter filter:
#   - `go fix -diff` writes the unified diff to stdout, download chatter to
#     stderr → capture stdout (no redirect needed; stderr stays visible).
#   - `go tool modernize` writes diagnostics AND `go: downloading …` lines to
#     stderr (the latter when go.mod's tool deps aren't cached — every fresh
#     CI run hits this). Same stream means we can't separate by redirection;
#     instead we capture stderr via stream-swap and grep the chatter out.
# A future kindness: `go: downloading` is the only chatter we've observed, so
# if the tool ever grows another routine stderr message, extend the filter
# explicitly instead of weakening it.
modernize-check: ## go fix -diff + modernize gate (Go idiom drift vs go.mod's go version)
	@out=$$(go fix -diff ./...); \
	if [ -n "$$out" ]; then \
		echo "go fix found pending changes:"; echo "$$out"; \
		echo "apply with: make modernize"; exit 1; \
	fi
	@out=$$(go tool modernize ./... 2>&1 1>/dev/null | grep -v '^go: downloading'); \
	if [ -n "$$out" ]; then \
		echo "modernize found pending changes:"; echo "$$out"; \
		echo "apply with: make modernize"; exit 1; \
	fi

modernize: ## Apply go fix + modernize rewrites in place
	go fix ./...
	go tool modernize -fix ./...

# Regenerate the docs/reference/*.md pages from their generators. The
# generators live under scripts/docgen/; each emits one markdown file
# from the canonical source (Go struct tags + `// docgen:env` comments
# for config-ref; the internal/mcp.Tools registry for mcp-tools; the
# internal/cli command registry for cli-ref). Run this after editing
# internal/config/config.go, internal/mcp/tools.go, internal/cli/cli.go,
# internal/cli/catalog.go, or adding/changing a // docgen:env comment, and
# commit the diff alongside the source change. `make docs-check` enforces
# no drift.
docs-regen: ## Regenerate checked-in documentation sources
	go run ./scripts/docgen/config-ref
	go run ./scripts/docgen/mcp-tools
	go run ./scripts/docgen/cli-ref
	go run ./scripts/check-mcp-server-card -write
	cp docs/mcp-server.json docs/.well-known/mcp/server.json

# docs-check is the CI gate: regenerate to a tempfile, diff against the
# checked-in copy, fail if they differ. Catches the "I changed a struct
# tag but forgot to regen" case. Wired into `make check` so it cannot
# be skipped. Uses POSIX tempfiles (not bash process substitution) so
# the recipe runs under /bin/sh on every host.
docs-check: ## Verify checked-in docs/reference/*.md match what the generators emit
	@go test ./scripts/docgen/config-ref ./scripts/docgen/cli-ref
	@go run ./scripts/check-mcp-server-card
	@cmp -s docs/mcp-server.json docs/.well-known/mcp/server.json || { \
		echo "docs-check: docs/.well-known/mcp/server.json differs from canonical docs/mcp-server.json" >&2; \
		echo "            run \`make docs-regen\` to refresh the public discovery copy" >&2; \
		exit 1; \
	}
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	fail=0; \
	for gen in config-ref mcp-tools cli-ref; do \
		case $$gen in \
			config-ref) ref=docs/docs/reference/config.md ;; \
			mcp-tools) ref=docs/docs/reference/mcp-tools.md ;; \
			cli-ref) ref=docs/docs/reference/cli.md ;; \
		esac; \
		go run ./scripts/docgen/$$gen -o "$$tmp/$$gen.md" || exit 1; \
		if ! diff -u "$$ref" "$$tmp/$$gen.md" > /dev/null 2>&1; then \
			echo "docs-check: $$ref out of date; run \`make docs-regen\`"; \
			diff -u "$$ref" "$$tmp/$$gen.md" || true; \
			fail=1; \
		fi; \
	done; \
	exit $$fail

# Markdown is the only prose authority for the generator-declared pages. The
# Pages artifact is built from tracked sources; generated HTML never returns to
# the Git tree. Structural and link checks inspect the exact artifact deployed.
docs-html-check: ## Build and verify the complete Pages artifact
	@go test ./scripts/docgen/docs-html
	@node scripts/render-architecture.mjs --check
	@$(MAKE) pages-build

pages-build: ## Build the deployable Pages artifact from tracked sources
	@rm -rf dist/pages
	@mkdir -p dist/pages
	@cp -R docs/. dist/pages/
	@go run ./scripts/docgen/docs-html -output-root dist/pages
	@node scripts/check-docs-html-structure.mjs dist/pages
	@node scripts/check-pages-links.mjs dist/pages

# Pull the current S&P-500 membership list from Wikipedia and rewrite
# internal/breadth/spx/members_data.go. Invoked by `make release` so a
# freshly-tagged binary always carries a current list; a release that
# would change the file fails-closed with a "commit and re-run" message
# so the tag and binary stay coherent (see the refresh-spx-members
# block inside `release:` for the dirty-tree guard).
refresh-spx-members: ## Refresh internal/breadth/spx/members_data.go from Wikipedia
	go run ./scripts/refresh-spx-members

fmt: ## Apply gofmt -w to every tracked / non-gitignored .go file
	@# Same scope as `make check` so `make fmt && make check` is idempotent.
	git ls-files --cached --others --exclude-standard '*.go' | xargs gofmt -w

# Library tests. The pkg/ibkr suite is fully hermetic — wire-level
# captured fixtures (wire_fixtures_test.go, scanner_test.go) plus net.Pipe-
# driven handshake tests; no live gateway is required. The end-to-end
# gateway path is covered by test/integration. Timeout sized for CI's
# slower runners — local runs typically finish in <30s.
#
# -race is on: this layer carries the wire-path goroutines (rate-limiter
# dispatch, msg-204 notice recovery, slot accounting) and was the last
# package family without race coverage.
#
# Hermetic suites run WITHOUT -count=1 so Go's content-addressed test
# cache applies: unchanged packages report cached passes in ~0s and only
# edited packages re-run. The cache only ever serves passes for identical
# inputs, so nothing green is taken on faith — a flake reruns on any
# input change. test/integration keeps -count=1 below because gateway
# state is invisible to the cache key.
test-pkg: ## Run pkg/ibkr/... tests under -race (TWS protocol library; cached when unchanged)
	go test -race -timeout=180s ./pkg/ibkr/...

# Command entrypoints and the hermetic registry metadata helper are shipped
# surfaces too. Keep them in one explicit race-enabled leg so both local
# `make test` and CI exercise them without depending on package discovery
# elsewhere in the matrix.
test-support: ## Run command and CI/release support tests under -race
	go test -race -timeout=180s ./cmd/...
	go test -race -timeout=60s ./scripts/release-registry-server
	go test -race -timeout=60s ./scripts/release-ci-wait

test-integration: ## Run hermetic CLI/daemon lifecycle integration tests; never probes a live Gateway
	INTEGRATION_TEST_MODE=hermetic go test -race -count=1 -timeout=420s -run '^TestLifecycle_' ./test/integration/...

test-integration-live: ## Require and exercise a live Gateway; absence or failed handshake is an error
	./scripts/with-gateway-lock.sh env INTEGRATION_TEST_MODE=live go test -v -race -count=1 -timeout=420s -skip '^TestLifecycle_' ./test/integration/...

# Daemon + CLI integration tests. -race is on for the daemon path because
# this layer carries the goroutines (subscriptions, idle timer, signal
# handlers); race detector earns its slot here. Binding targets separate
# hermetic lifecycle coverage from strict live-Gateway evidence.
#
# The integration leg is serialized across sessions via with-gateway-lock:
# its client IDs and daemon spawns hit the shared TWS gateway, and two
# overlapping runs used to flake with error 326 and force a full re-run.
trading-package-scope-check:
	@set -eu; \
		unexpected="$$(find internal/daemon -mindepth 2 -type f -name '*.go' -exec sh -c \
			'for file do if grep -q "^//go:build .*trading" "$$file"; then printf "%s\n" "$$file"; fi; done; exit 0' sh {} +)"; \
		if [ -n "$$unexpected" ]; then \
			echo "trading-package-scope-check: a daemon subpackage now has trading-tagged source:" >&2; \
			printf '%s\n' "$$unexpected" >&2; \
			echo "add that exact package to the trading test matrix before proceeding" >&2; \
			exit 1; \
		fi

test-internal: ## Run internal/... under -race excluding the daemon root
	@set -eu; \
		daemon_pkg="$$(go list ./internal/daemon)"; \
		all_internal="$$(go list ./internal/...)"; \
		internal_pkgs="$$(printf '%s\n' "$$all_internal" | awk -v omit="$$daemon_pkg" '$$0 != omit')"; \
		if [ -n "$$internal_pkgs" ]; then \
			go test -race -timeout=240s $$internal_pkgs; \
		fi

# The daemon root's two build modes are separate CI jobs since 2026-08-04:
# together they were the ubuntu test job's 7.5-minute tail, and they share
# nothing but the scope check, so they parallelize cleanly.
test-daemon-default: trading-package-scope-check ## Daemon root default build + hermetic lifecycle integration
	go test -race -timeout=420s ./internal/daemon
	$(MAKE) test-integration

test-daemon-trading: trading-package-scope-check ## Daemon root trading build (write path)
	go test -race -timeout=420s -tags trading ./internal/daemon

test-daemon: trading-package-scope-check ## Run internal/... and hermetic integration under -race in both build modes
	$(MAKE) test-internal
	$(MAKE) test-daemon-default
	$(MAKE) test-daemon-trading

# Install the standalone skill bundle directly under global agent skill roots.
# Dogfood path only — end users get the skill via `/plugin install canary`.
# Idempotent: re-running updates files in place.
install-skill: build ## Install SKILL.md to global Claude/Codex skill dirs (dogfood path)
	install -d $(SKILL_DIR)
	install -m 0644 $(SKILL_SRC)/SKILL.md $(SKILL_DIR)/SKILL.md
	install -m 0644 $(SKILL_SRC)/schemas.md $(SKILL_DIR)/schemas.md
	install -d $(CODEX_SKILL_DIR)
	install -m 0644 $(SKILL_SRC)/SKILL.md $(CODEX_SKILL_DIR)/SKILL.md
	install -m 0644 $(SKILL_SRC)/schemas.md $(CODEX_SKILL_DIR)/schemas.md
	@echo "Installed skill to $(SKILL_DIR)"
	@echo "Installed skill to $(CODEX_SKILL_DIR)"
	@echo
	@echo "Prefer the plugin install path for end users:"
	@echo "  /plugin marketplace add osauer/canary"
	@echo "  /plugin install canary"
	@echo
	@echo "For a global Bash(canary ...) allowlist, copy settings/canary.settings.json"
	@echo "into your ~/.claude/settings.json by hand (the SKILL frontmatter already"
	@echo "grants the read patterns when the skill is active)."
	@if command -v claude >/dev/null 2>&1; then \
		echo; \
		echo "Refreshing the Claude Code plugin from this checkout so MCP tools/hooks update too..."; \
		$(MAKE) --no-print-directory install-plugin-refresh; \
	else \
		echo; \
		echo "Claude CLI not on PATH; skipped Claude Code plugin refresh."; \
	fi

install-plugin: build install-plugin-refresh ## Install/update the Claude Code plugin from this checkout (dogfood path)

install-plugin-refresh:
	@command -v claude >/dev/null 2>&1 || { echo "claude CLI not on PATH; install Claude Code first" >&2; exit 1; }
	claude plugin validate .
	claude plugin marketplace add "$(CLAUDE_PLUGIN_MARKETPLACE)"
	@if claude plugin list --json 2>/dev/null | grep -q '"id": "$(CLAUDE_PLUGIN_ID)"'; then \
		claude plugin uninstall "$(CLAUDE_PLUGIN_ID)"; \
	fi
	claude plugin install "$(CLAUDE_PLUGIN_ID)"
	@echo "Installed Claude Code plugin $(CLAUDE_PLUGIN_ID) from $(CLAUDE_PLUGIN_MARKETPLACE)"
	@echo "Restart Claude Code or run /reload-plugins to load plugin MCP servers."

uninstall-skill: ## Remove the dogfood skill install from global Claude/Codex skill dirs
	rm -rf $(SKILL_DIR)
	rm -rf $(CODEX_SKILL_DIR)

clean: ## Remove bin/ and dist/
	rm -rf bin dist

# Cross-compile release tarballs for the OS/arch matrix this project actually
# supports. The daemon uses Unix-only primitives (Setsid, flock, AF_UNIX
# sockets); Windows is intentionally out of scope and would require a port.
# Each tarball contains the stamped binary plus LICENSE + README.md so a
# colleague can extract, drop into ~/.local/bin, and run.
RELEASE_TARGETS = darwin-arm64 darwin-amd64 linux-amd64 linux-arm64
DIST_DIR = dist
RELEASE_BUILD_JOBS ?= 4

# Release builds resolve the commit hash from the *tag*, not from HEAD,
# so the binary's stamped commit matches the git tag a colleague would
# `git checkout`. -buildvcs=false suppresses runtime/debug.BuildInfo's
# vcs.modified flag — the -ldflags vars are authoritative for releases,
# and the dirty/clean signal is only useful for in-tree dev builds.
release-verify: ## Smoke-test the local bin/canary against a live gateway (called by `make release`)
	@# Standalone so a release-flow failure can be diagnosed in isolation:
	@#   make release-verify RELEASE_VERSION=v0.15.1
	@# The script spawns an isolated daemon under /tmp, runs a fixed
	@# matrix (version, status, account, positions, quote SPY), asserts
	@# the v0.15+ data_type contract on each surface, and tears the
	@# daemon down. Requires a reachable IBKR Gateway — the gate is
	@# binding by design (see release-verify.sh).
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-verify: RELEASE_VERSION is required, e.g. make release-verify RELEASE_VERSION=v0.15.1" >&2; \
		exit 1; \
	fi
	@if [ ! -x bin/canary ]; then \
		echo "release-verify: bin/canary missing — run 'make build' first" >&2; \
		exit 1; \
	fi
	./scripts/with-gateway-lock.sh ./scripts/release-verify.sh bin/canary $(RELEASE_VERSION)

release-smoke: smoke-build ## Release gate: JSON contract + wire smoke in one reachable TWS/Gateway daemon session
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-smoke: RELEASE_VERSION is required, e.g. make release-smoke RELEASE_VERSION=v0.15.1" >&2; \
		exit 1; \
	fi
	@if [ ! -x bin/canary ]; then \
		echo "release-smoke: bin/canary missing — run 'make build VERSION=$(RELEASE_VERSION)' first" >&2; \
		exit 1; \
	fi
	CANARY_SMOKE_STRICT=$(SMOKE_STRICT) SPX_EXPECTED_REACHABLE=$(SPX_EXPECTED_REACHABLE) ./scripts/with-gateway-lock.sh ./scripts/release-smoke.sh bin/canary $(RELEASE_VERSION) bin/wire-assert

release-site-check: ## Require osauer.dev/canary static site sync for non-patch releases
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-site-check: RELEASE_VERSION is required, e.g. make release-site-check RELEASE_VERSION=v1.8.0" >&2; \
		exit 1; \
	fi
	./scripts/check-release-site-sync.sh $(RELEASE_VERSION)

smoke-build: ## Compile the bin/wire-assert helper used by `make smoke`
	@mkdir -p bin
	go build -o bin/wire-assert ./cmd/wire-assert

# Run wire-smoke against the *existing* bin/canary without rebuilding it.
# The release flow uses this so it can exercise the version-stamped
# binary produced by `make build VERSION=$(RELEASE_VERSION)`, instead
# of clobbering that stamp with a `git describe` rebuild via the smoke
# dep chain.
#
# Drives bin/canary against a reachable TWS/Gateway session with the wire interceptor
# enabled and asserts per-command protocol-level invariants — catches
# bugs the unit suite can't see (e.g. the v0.24.x productionLegFetcher
# bug where the gateway sent the right ticks but the daemon read the
# wrong field).
#
# SMOKE_STRICT controls the no-gateway posture (forwarded to the script
# as CANARY_SMOKE_STRICT):
#   SMOKE_STRICT=0 (default) → SKIP cleanly when no gateway is up; lets
#                              user-invoked `make smoke` work on a laptop
#                              without paper-account IBKR access.
#   SMOKE_STRICT=1 → FAIL when no gateway is up; the release path passes
#                    this so a vanished gateway can't silently bypass
#                    the wire gate. Paper TWS/Gateway is accepted because
#                    the smoke is read-only.
SMOKE_STRICT ?= 0

# SPX_EXPECTED_REACHABLE — default ON in this repo because this is the
# dev machine with CBOE OPRA entitlement; the user's standing guardrail
# (per internal-docs/design/gamma-spx-coverage.md §11.2): "no SPX data would be
# a bug on my setup." If `canary gamma --only=spx` returns the
# entitlement-skipped banner, fail loudly rather than silently passing
# the smoke. Override with `make smoke SPX_EXPECTED_REACHABLE=0` on
# accounts that legitimately lack SPX entitlement.
SPX_EXPECTED_REACHABLE ?= 1

smoke-only: smoke-build ## Run wire smoke against existing bin/canary (no rebuild); SMOKE_STRICT=1 makes no-gateway a failure
	@if [ ! -x bin/canary ]; then \
		echo "smoke-only: bin/canary missing — run 'make build' first" >&2; \
		exit 1; \
	fi
	CANARY_SMOKE_STRICT=$(SMOKE_STRICT) SPX_EXPECTED_REACHABLE=$(SPX_EXPECTED_REACHABLE) ./scripts/with-gateway-lock.sh ./scripts/wire-smoke.sh bin/canary bin/wire-assert

smoke: build smoke-only ## Wire-level smoke vs. reachable TWS/Gateway (rebuilds bin/canary; SKIP if no gateway)

# The per-commit inner-loop gate: boot + handshake + quote + account
# against a real gateway (~15s) instead of the full wire matrix. The full
# `make smoke` stays binding for daemon/CLI wire-path changes and for
# releases — this tier exists so a docs/proposal/SPA change doesn't pay
# the chain/regime/gamma fan-out every commit.
smoke-fast: build smoke-build ## Fast wire smoke: boot + quote + account only (~15s; full matrix stays in `make smoke`)
	CANARY_SMOKE_FAST=1 CANARY_SMOKE_STRICT=$(SMOKE_STRICT) ./scripts/with-gateway-lock.sh ./scripts/wire-smoke.sh bin/canary bin/wire-assert

release-binaries: ## Cross-compile canonical read-only/trading tarballs and the read-only MCPB
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-binaries: RELEASE_VERSION is required, e.g. make release-binaries RELEASE_VERSION=v0.6.0" >&2; \
		exit 1; \
	fi
	@if ! git rev-parse --verify --quiet "refs/tags/$(RELEASE_VERSION)^{commit}" >/dev/null; then \
		echo "release-binaries: tag $(RELEASE_VERSION) does not exist; run \`make release RELEASE_VERSION=$(RELEASE_VERSION)\` first" >&2; \
		exit 1; \
	fi
	./scripts/with-release-tag-checkout.sh "$(RELEASE_VERSION)" \
		"$(CURDIR)/scripts/build-release-artifacts.sh" all "$(RELEASE_VERSION)" "$(abspath $(DIST_DIR))" \
		"$(RELEASE_TARGETS)" "$(RELEASE_BUILD_JOBS)" "$(STRIP_LDFLAGS)"
	@echo
	@echo "Built artefacts in $(DIST_DIR)/:"
	@ls -la $(DIST_DIR)

release-mcpb: ## Build the cross-platform MCP Bundle from release tarballs
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-mcpb: RELEASE_VERSION is required, e.g. make release-mcpb RELEASE_VERSION=v1.2.1" >&2; \
		exit 1; \
	fi
	./scripts/with-release-tag-checkout.sh "$(RELEASE_VERSION)" \
		"$(CURDIR)/scripts/build-release-artifacts.sh" mcpb "$(RELEASE_VERSION)" "$(abspath $(DIST_DIR))" \
		"$(RELEASE_TARGETS)" "$(RELEASE_BUILD_JOBS)" "$(STRIP_LDFLAGS)"

release-checksums: ## Sign SHA256SUMS for tarballs and MCPB assets
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-checksums: RELEASE_VERSION is required, e.g. make release-checksums RELEASE_VERSION=v1.2.1" >&2; \
		exit 1; \
	fi
	./scripts/with-release-tag-checkout.sh "$(RELEASE_VERSION)" \
		"$(CURDIR)/scripts/build-release-artifacts.sh" checksums "$(RELEASE_VERSION)" "$(abspath $(DIST_DIR))" \
		"$(RELEASE_TARGETS)" "$(RELEASE_BUILD_JOBS)" "$(STRIP_LDFLAGS)"

# The published payload set is fixed by the release contract — four targets
# x read-only/trading tarballs plus the versioned and floating MCPB — and is
# never a function of what matrix the caller asked for. Counting assets is not
# enough: six correct files plus four duplicates also counts to ten, so the
# gate compares exact names. Both the pre-tag proof in _release-run and the
# pre-publication proof in _release-publish run this one authority.
release-payload-inventory-check:
	@./scripts/check-release-payload-inventory.sh "$(RELEASE_VERSION)" "$(abspath $(DIST_DIR))"

release-registry-server: ## Generate and validate dist/server.json for MCP Registry publishing
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-registry-server: RELEASE_VERSION is required, e.g. make release-registry-server RELEASE_VERSION=v1.2.1" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/canary-$(RELEASE_VERSION).mcpb" ]; then \
		echo "release-registry-server: missing $(DIST_DIR)/canary-$(RELEASE_VERSION).mcpb; run make release-mcpb" >&2; \
		exit 1; \
	fi
	@set -eu; \
	template=$$(mktemp "$${TMPDIR:-/tmp}/canary-registry-template.XXXXXX") || exit 1; \
	trap 'rm -f "$$template"' EXIT HUP INT TERM; \
	python3 ./scripts/materialize-release-tag-file.py \
		"$(RELEASE_VERSION)" server.json "$$template"; \
	go run ./scripts/release-registry-server "$(RELEASE_VERSION)" "$$template" \
		"$(DIST_DIR)/canary-$(RELEASE_VERSION).mcpb" "$(DIST_DIR)/server.json"; \
	$(MCP_PUBLISHER) validate "$(DIST_DIR)/server.json"

registry-login: ## Refresh MCP Registry auth token (default: GitHub device flow)
	$(MCP_PUBLISHER) login $(MCP_REGISTRY_LOGIN_METHOD)

release-auth-preflight: ## Fail-fast gh auth + registry fallback preconditions (device code only if Actions OIDC fails)
	MCP_REGISTRY_AUTO_LOGIN=$(MCP_REGISTRY_AUTO_LOGIN) \
		./scripts/release-auth-preflight.sh "$(MCP_PUBLISHER)" "$(MCP_REGISTRY_LOGIN_METHOD)"

release-origin-check:
	@./scripts/check-release-origin.sh

release-ci-wait: ## Require exact-HEAD success from every source-controlled push-to-main workflow
	$(MAKE) release-origin-check
	@command -v gh >/dev/null 2>&1 || { echo "release-ci-wait: gh CLI not on PATH" >&2; exit 1; }
	@GOFLAGS= go run ./scripts/release-ci-wait \
		-contract scripts/release-ci-contract.json \
		-sha "$$(git rev-parse HEAD)" -branch "$(MAIN_BRANCH)" -event push \
		-poll "$(RELEASE_CI_POLL)" -timeout "$(RELEASE_CI_TIMEOUT)"

_release-ci-wait-historical:
	$(if $(filter default,$(origin MAKE)),,$(error _release-ci-wait-historical: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error _release-ci-wait-historical: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error _release-ci-wait-historical: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error _release-ci-wait-historical: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error _release-ci-wait-historical: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error _release-ci-wait-historical: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error _release-ci-wait-historical: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error _release-ci-wait-historical: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release-resume" ]; then \
		echo "_release-ci-wait-historical: internal resume helper; invoke 'make release-resume RELEASE_VERSION=vX.Y.Z'" >&2; \
		exit 1; \
	fi
	$(MAKE) release-origin-check
	@command -v gh >/dev/null 2>&1 || { echo "_release-ci-wait-historical: gh CLI not on PATH" >&2; exit 1; }
	@release_sha=$$(git rev-parse --verify "refs/tags/$(RELEASE_VERSION)^{commit}") || { \
		echo "_release-ci-wait-historical: cannot resolve release tag $(RELEASE_VERSION)" >&2; \
		exit 1; \
	}; \
	contract=$$(mktemp "$${TMPDIR:-/tmp}/canary-release-ci-contract.XXXXXX") || exit 1; \
	trap 'rm -f "$$contract"' EXIT HUP INT TERM; \
	python3 ./scripts/materialize-release-ci-contract.py \
		"$(RELEASE_VERSION)" "$$contract"; \
	GOFLAGS= go run ./scripts/release-ci-wait \
		-contract "$$contract" -historical \
		-sha "$$release_sha" -branch "$(MAIN_BRANCH)" -event push \
		-poll "$(RELEASE_CI_POLL)" -timeout "$(RELEASE_CI_TIMEOUT)"

# Keep the mutable main-ref assertion separate from exact-SHA Actions evidence:
# release-resume must verify the tagged SHA even after origin/main advances.
release-main-candidate-check:
	$(MAKE) release-origin-check
	@sha=$$(git rev-parse HEAD) || exit 1; \
	remote_line=$$(git ls-remote --exit-code --refs origin "refs/heads/$(MAIN_BRANCH)") || { \
		echo "release-main-candidate-check: cannot resolve origin/$(MAIN_BRANCH)" >&2; \
		exit 1; \
	}; \
	set -- $$remote_line; \
	if [ "$$#" -ne 2 ] || [ "$$2" != "refs/heads/$(MAIN_BRANCH)" ]; then \
		echo "release-main-candidate-check: malformed origin/$(MAIN_BRANCH) response" >&2; \
		exit 1; \
	fi; \
	if [ "$$1" != "$$sha" ]; then \
		echo "release-main-candidate-check: origin/$(MAIN_BRANCH) is $$1, expected release candidate $$sha" >&2; \
		exit 1; \
	fi

release-source-candidate-check:
	@./scripts/check-release-source.sh --mode tag "$(RELEASE_VERSION)"

release-controller-source-check:
	$(MAKE) release-origin-check
	@./scripts/check-release-source.sh --mode controller "$(RELEASE_VERSION)"

# The registry workflow checks out github.workflow_sha, which is the tag commit
# on a release publication and the dispatch ref head on a manual heal. Its
# anchor therefore follows the trigger, and only this target takes it from the
# caller; every other source proof names one fixed mode.
release-source-mode-check:
	$(MAKE) release-origin-check
	@./scripts/check-release-source.sh --mode "$(RELEASE_SOURCE_MODE)" "$(RELEASE_VERSION)"

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

release-github-assets: ## Hydrate and verify the exact signed asset set from an existing GitHub release
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "release-github-assets: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	$(MAKE) release-source-mode-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/hydrate-github-release-assets.sh "$(RELEASE_VERSION)" "$(abspath $(DIST_DIR))"
	$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)

registry-publish: ## Recover registry publication from an exact tagged release worktree
	$(if $(filter default,$(origin MAKE)),,$(error registry-publish: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error registry-publish: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error registry-publish: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error registry-publish: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error registry-publish: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error registry-publish: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error registry-publish: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error registry-publish: release variables must not be overridden: $(release_overridden_vars)),)
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "registry-publish: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/check-release-ci-contract.sh
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-registry-server RELEASE_VERSION=$(RELEASE_VERSION)
	MCP_REGISTRY_AUTO_LOGIN=$(MCP_REGISTRY_AUTO_LOGIN) MCP_REGISTRY_LOGIN_METHOD=$(MCP_REGISTRY_LOGIN_METHOD) \
		./scripts/registry-publish-with-login.sh "$(MCP_PUBLISHER)" "$(DIST_DIR)/server.json"

registry-publish-verify-first: ## Release-only: wait for Actions OIDC, then fall back to direct login + publish
	$(if $(filter default,$(origin MAKE)),,$(error registry-publish-verify-first: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error registry-publish-verify-first: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error registry-publish-verify-first: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error registry-publish-verify-first: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error registry-publish-verify-first: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error registry-publish-verify-first: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error registry-publish-verify-first: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error registry-publish-verify-first: release variables must not be overridden: $(release_overridden_vars)),)
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "registry-publish-verify-first: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/check-release-ci-contract.sh
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@# Primary path (entry=release): dist/ holds the set this pipeline just
	@# built, checksummed, signed, and uploaded; release-github-candidate-check
	@# proves GitHub's asset inventory, per-asset sha256 digests, signed
	@# SHA256SUMS bytes, and release body all equal that local set, so byte
	@# re-hydration of ~130MB adds no evidence and ~3 min (operator decision
	@# 2026-08-03). Resume/recovery entries keep full hydration because their
	@# local dist/ cannot be trusted to match the published release.
	$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION),$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION))
	$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-registry-server RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/registry-publish-verify-first.sh "$(RELEASE_VERSION)" \
		"$(DIST_DIR)/server.json" \
		make --no-print-directory registry-publish \
		RELEASE_VERSION="$(RELEASE_VERSION)" DIST_DIR="$(DIST_DIR)" \
		MCP_PUBLISHER="$(MCP_PUBLISHER)" MCP_REGISTRY_AUTO_LOGIN="$(MCP_REGISTRY_AUTO_LOGIN)" \
		MCP_REGISTRY_LOGIN_METHOD="$(MCP_REGISTRY_LOGIN_METHOD)"

# Compose the GitHub Release notes by substituting __VERSION__ and
# __HIGHLIGHTS__ in the install-header template, then appending the
# matching CHANGELOG.md entry. __HIGHLIGHTS__ is pulled from the entry's
# `### What's new` section, so the release body's top stanza is mechanically
# derived from CHANGELOG — no second place to drift. The release is created
# as an empty staged draft, its assets uploaded in parallel, the complete set
# verified in place, and only then flipped to published+latest (2026-08-04):
# the publication event — and the registry OIDC workflow it triggers — never
# sees a partial upload, which is also what makes the parallel upload safe.
# Stale drafts from an interrupted attempt are pruned first; published
# releases are never touched by the prune.
_release-publish:
	$(if $(filter default,$(origin MAKE)),,$(error _release-publish: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error _release-publish: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error _release-publish: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error _release-publish: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error _release-publish: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error _release-publish: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error _release-publish: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error _release-publish: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ "$(MAKELEVEL)" -lt 1 ]; then \
		echo "_release-publish: internal pipeline helper; invoke 'make release RELEASE_VERSION=vX.Y.Z'" >&2; \
		exit 1; \
	fi
	@case "$(RELEASE_PIPELINE_ENTRY)" in \
		release|release-resume) ;; \
		*) echo "_release-publish: invalid internal pipeline entry" >&2; exit 1 ;; \
	esac
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "_release-publish: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) release-ci-wait,$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume)
	$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION)
	@cd "$(DIST_DIR)" && shasum -a 256 -c SHA256SUMS
	@command -v gh >/dev/null 2>&1 || { echo "_release-publish: gh CLI not on PATH; brew install gh" >&2; exit 1; }
	$(if $(filter release,$(RELEASE_PIPELINE_ENTRY)),$(MAKE) changelog-lint RELEASE_VERSION=$(RELEASE_VERSION),$(MAKE) changelog-lint-historical RELEASE_VERSION=$(RELEASE_VERSION) RELEASE_SOURCE_DIR="$(RELEASE_SOURCE_DIR)")
	@notes=$$(mktemp -t canary-release-notes.XXXXXX) && \
	changelog=$$(mktemp -t canary-release-changelog.XXXXXX) && \
	template=$$(mktemp -t canary-release-notes-template.XXXXXX) && \
	trap 'rm -f $$notes $$changelog $$template' EXIT && \
	python3 ./scripts/materialize-release-tag-file.py \
		"$(RELEASE_VERSION)" CHANGELOG.md "$$changelog" && \
	python3 ./scripts/materialize-release-tag-file.py \
		"$(RELEASE_VERSION)" .github/release-notes-template.md "$$template" && \
	./scripts/render-release-notes.sh "$(RELEASE_VERSION)" "$$changelog" "$$template" "$$notes" && \
	assets=""; asset_count=0; \
	while read -r digest asset; do \
		case "$$asset" in ""|*/*|*[!A-Za-z0-9._-]*) echo "_release-publish: unsafe SHA256SUMS asset name: $$asset" >&2; exit 1 ;; esac; \
		[ -n "$$digest" ] && [ -f "$(DIST_DIR)/$$asset" ] || { echo "_release-publish: missing checksummed asset $$asset" >&2; exit 1; }; \
		assets="$$assets $(DIST_DIR)/$$asset"; asset_count=$$((asset_count + 1)); \
	done < "$(DIST_DIR)/SHA256SUMS"; \
	[ "$$asset_count" -eq 10 ] || { echo "_release-publish: expected 10 checksummed payloads, got $$asset_count" >&2; exit 1; }; \
	title="$${MESSAGE:-$(RELEASE_VERSION)}" && \
	./scripts/prune-github-release-drafts.sh "$(RELEASE_VERSION)" && \
	./scripts/check-release-origin.sh && \
	./scripts/check-release-tag.sh "$(RELEASE_VERSION)" && \
	gh release create $(RELEASE_VERSION) --repo github.com/osauer/canary --verify-tag --draft --notes-file $$notes --title "$$title" && \
	./scripts/upload-release-assets.sh "$(RELEASE_VERSION)" $$assets $(DIST_DIR)/SHA256SUMS $(DIST_DIR)/SHA256SUMS.asc && \
	CHECK_GITHUB_RELEASE_STAGE=draft ./scripts/check-github-release.sh "$(RELEASE_VERSION)" "$(DIST_DIR)" && \
	gh release edit $(RELEASE_VERSION) --repo github.com/osauer/canary --draft=false --latest

changelog-check: ## Verify CHANGELOG.md has no template or maintainer-process leakage
	@./scripts/check-changelog-public.sh

# Born of the 2026-06-11 incident: a root-level scratch page with real
# margin/net-liq figures shipped in the v1.9.0 tag and needed a history
# rewrite. Fails on root HTML, *lab*.html / *scratch* names, and IBKR
# account IDs (U/DU + 6-9 digits) in every tracked file, including tests
# and binary blobs.
account-data-check: ## No IBKR account data or scratch pages in tracked files
	@./scripts/check-no-account-data.sh
	@./scripts/check-no-account-data_test.sh

changelog-lint: ## Validate the topmost CHANGELOG.md entry matches RELEASE_VERSION and has required shape
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "changelog-lint: RELEASE_VERSION is required, e.g. make changelog-lint RELEASE_VERSION=v0.27.12" >&2; \
		exit 1; \
	fi
	@RELEASE_VERSION=$(RELEASE_VERSION) \
		CHANGELOG_PATH=CHANGELOG.md \
		CHANGELOG_HISTORICAL=0 \
		./scripts/check-changelog-entry.sh
	@# Local only: reads git and the changelog, never GitHub. Kept out of
	@# changelog-lint-historical, whose immutable source has no release range.
	@RELEASE_VERSION=$(RELEASE_VERSION) \
		CHANGELOG_PATH=CHANGELOG.md \
		./scripts/check-changelog-issue-refs.sh
	@./scripts/check-changelog-issue-refs_test.sh

changelog-lint-historical:
	@if [ -z "$(RELEASE_VERSION)" ] || [ -z "$(RELEASE_SOURCE_DIR)" ]; then \
		echo "changelog-lint-historical: release version and immutable source are required" >&2; \
		exit 1; \
	fi
	@RELEASE_VERSION=$(RELEASE_VERSION) \
		CHANGELOG_PATH="$(RELEASE_SOURCE_DIR)/CHANGELOG.md" \
		CHANGELOG_HISTORICAL=1 \
		./scripts/check-changelog-entry.sh

all: build test ## build + test

version: ## Print the version string the next build would embed
	@echo "VERSION=$(VERSION)"
	@echo "COMMIT=$(COMMIT)"
	@echo "DATE=$(DATE)"

# Tag and push a new release. RELEASE_VERSION is a separate variable from
# build-time VERSION (which auto-derives from git describe and is always
# populated) so the "missing arg" guard actually fires.
#
# Guards against the foot-guns:
# - missing RELEASE_VERSION arg
# - dirty working tree (would bake "-dirty" into the binary)
# - origin/<MAIN_BRANCH> contains commits HEAD lacks (release would omit
#   already-landed work)
# - tag already exists locally or on origin
# Sequence: candidate push (starts the exact full Actions matrix) → cheap
# local/plugin gates → read-only paper preview → stamped build → live/paper
# smoke → exact-SHA Actions wait → recoverable local tag/artifact assembly →
# final Actions/main recheck → atomic tag publish. The early preview exercises
# the account-currency, FX, and broker WhatIf path before the expensive local
# smoke; the transmitting smoke and CI authority both stay before tagging.
# The pipeline body (_release-run) executes in a detached worktree checked out
# at the operator's committed HEAD, so this checkout stays free for concurrent
# work and local edits can never leak into release artifacts. Its first step
# fast-forward-pushes that candidate to origin/MAIN_BRANCH.
release: ## Cut a release from an isolated worktree of committed HEAD: make release RELEASE_VERSION=vX.Y.Z [MESSAGE="..."]
	$(if $(filter default,$(origin MAKE)),,$(error release: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error release: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error release: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error release: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error release: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error release: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error release: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error release: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release: RELEASE_VERSION is required, e.g. make release RELEASE_VERSION=v0.3.1" >&2; \
		exit 1; \
	fi
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "release: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	@branch=$$(git symbolic-ref --quiet --short HEAD) || { echo "release: checkout must be on $(MAIN_BRANCH)" >&2; exit 1; }; \
	if [ "$$branch" != "$(MAIN_BRANCH)" ]; then \
		echo "release: $(RELEASE_VERSION) belongs to $(MAIN_BRANCH), not $$branch" >&2; \
		exit 1; \
	fi
	$(MAKE) release-origin-check
	@# Fetch so origin/MAIN_BRANCH means GitHub's state, not a stale local
	@# remote-tracking ref. Releasing needs the network anyway.
	@git fetch origin $(MAIN_BRANCH) --quiet || { \
		echo "release: git fetch origin $(MAIN_BRANCH) failed; releasing requires the network" >&2; \
		exit 1; \
	}
	@# The release ships exactly the operator's committed HEAD. If origin has
	@# commits this checkout lacks, releasing HEAD would drop landed work from
	@# the release — that is a merge decision for a human, so refuse.
	@git merge-base --is-ancestor origin/$(MAIN_BRANCH) HEAD || { \
		echo "release: origin/$(MAIN_BRANCH) has commits this checkout lacks; pull/rebase first:" >&2; \
		git log --oneline HEAD..origin/$(MAIN_BRANCH) >&2; \
		exit 1; \
	}
	@if git rev-parse --verify --quiet $(RELEASE_VERSION) >/dev/null; then \
		echo "release: tag $(RELEASE_VERSION) already exists locally" >&2; \
		exit 1; \
	fi
	@if git ls-remote --tags --exit-code origin $(RELEASE_VERSION) >/dev/null 2>&1; then \
		echo "release: tag $(RELEASE_VERSION) already exists on origin" >&2; \
		exit 1; \
	fi
	@# Land the candidate BEFORE worktree prep: the push starts hosted CI,
	@# which is the pre-tag critical path, and worktree creation costs ~1
	@# min that the exact-SHA CI wait would otherwise absorb at the end.
	@# Plain push refuses non-fast-forward, and _release-run re-asserts the
	@# same ref from the worktree (a no-op here), so ordering loses nothing.
	git push --no-follow-tags origin HEAD:$(MAIN_BRANCH)
	@wt="$(RELEASE_WORKTREE_ROOT)/canary-release-$(RELEASE_VERSION)"; \
	sha=$$(git rev-parse HEAD); \
	if [ -e "$$wt" ]; then \
		echo "release: $$wt already exists (previous failed run?)." >&2; \
		echo "        inspect it, then remove with: git worktree remove --force $$wt" >&2; \
		exit 1; \
	fi; \
	echo "==> release worktree: $$wt (HEAD @ $$sha)"; \
	git worktree add --detach "$$wt" "$$sha" || exit 1; \
	msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	if MESSAGE="$$msg" $(MAKE) -C "$$wt" _release-run RELEASE_PIPELINE_ENTRY=release RELEASE_VERSION=$(RELEASE_VERSION) $(if $(wildcard bin/mcp-publisher),MCP_PUBLISHER=$(CURDIR)/bin/mcp-publisher); then \
		git worktree remove --force "$$wt"; \
	else \
		echo "release: pipeline failed; worktree kept for inspection: $$wt" >&2; \
		echo "        when done: git worktree remove --force $$wt" >&2; \
		exit 1; \
	fi

# The primary tag push is the pipeline's irreversible boundary: plugin tag,
# GitHub release, and registry publication follow it, and a failure in any of
# them used to strand a pushed tag that `make release` then refuses. Resume
# re-enters exactly at that boundary. Local and broker gates do not re-run,
# but the tagged SHA's immutable, tag-era Actions evidence is re-verified
# before any publication leg; this rejects tags that were not produced from a
# fully green candidate. Recovery executes the current committed origin/main
# controller while keeping a second clean worktree as immutable tag source.
# An existing GitHub release is staged from its published signed assets and
# verified byte-for-byte; only an absent release gets a fresh local assembly.
# Notes and Registry metadata are rendered from tag blobs. A partial or
# mismatched GitHub release fails loudly instead of being clobbered.
release-resume: ## Resume a release interrupted after its tag was pushed: make release-resume RELEASE_VERSION=vX.Y.Z
	$(if $(filter default,$(origin MAKE)),,$(error release-resume: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error release-resume: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error release-resume: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error release-resume: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error release-resume: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error release-resume: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error release-resume: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error release-resume: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-resume: RELEASE_VERSION is required, e.g. make release-resume RELEASE_VERSION=v0.3.1" >&2; \
		exit 1; \
	fi
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "release-resume: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	@branch=$$(git symbolic-ref --quiet --short HEAD) || { echo "release-resume: checkout must be on $(MAIN_BRANCH)" >&2; exit 1; }; \
	if [ "$$branch" != "$(MAIN_BRANCH)" ]; then \
		echo "release-resume: $(RELEASE_VERSION) belongs to $(MAIN_BRANCH), not $$branch" >&2; \
		exit 1; \
	fi
	$(MAKE) release-origin-check
	@git fetch origin --tags --quiet || { \
		echo "release-resume: git fetch origin failed; resuming requires the network" >&2; \
		exit 1; \
	}
	@release_sha=$$(git rev-parse --verify --quiet "refs/tags/$(RELEASE_VERSION)^{commit}") || { \
		echo "release-resume: tag $(RELEASE_VERSION) does not exist locally; nothing to resume — run make release" >&2; \
		exit 1; \
	}; \
	peeled=$$(git ls-remote origin "refs/tags/$(RELEASE_VERSION)^{}" | awk '{print $$1}'); \
	plain=$$(git ls-remote origin "refs/tags/$(RELEASE_VERSION)" | awk '{print $$1}'); \
	remote_commit=$${peeled:-$$plain}; \
	if [ -z "$$remote_commit" ]; then \
		echo "release-resume: tag $(RELEASE_VERSION) is not on origin; the failure predates the irreversible boundary." >&2; \
		echo "        delete the local tag (git tag -d $(RELEASE_VERSION)) and re-run make release" >&2; \
		exit 1; \
	fi; \
	if [ "$$remote_commit" != "$$release_sha" ]; then \
		echo "release-resume: origin tag $(RELEASE_VERSION) points at $$remote_commit but the local tag at $$release_sha; refusing to resume a diverged tag" >&2; \
		exit 1; \
	fi; \
	controller_sha=$$(git rev-parse --verify "HEAD^{commit}") || exit 1; \
	if ! git cat-file blob "$$controller_sha:Makefile" | grep -Fqx 'RELEASE_CONTROLLER_CONTRACT = release-controller-v1'; then \
		echo "release-resume: committed HEAD lacks the current recovery-controller contract; update and commit main first" >&2; \
		exit 1; \
	fi; \
	controller_wt="$(RELEASE_WORKTREE_ROOT)/canary-resume-$(RELEASE_VERSION)-controller"; \
	source_wt="$(RELEASE_WORKTREE_ROOT)/canary-resume-$(RELEASE_VERSION)-source"; \
	for wt in "$$controller_wt" "$$source_wt"; do \
		if [ -e "$$wt" ]; then \
			echo "release-resume: $$wt already exists (previous failed resume?)." >&2; \
			echo "        inspect it, then remove with: git worktree remove --force $$wt" >&2; \
			exit 1; \
		fi; \
	done; \
	echo "==> resume controller: $$controller_wt (controller @ $$controller_sha)"; \
	git worktree add --detach "$$controller_wt" "$$controller_sha" || exit 1; \
	echo "==> immutable release source: $$source_wt (tag $(RELEASE_VERSION) @ $$release_sha)"; \
	if ! git worktree add --detach "$$source_wt" "$$release_sha"; then \
		git worktree remove --force "$$controller_wt" >/dev/null 2>&1 || true; \
		exit 1; \
	fi; \
	msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	if MESSAGE="$$msg" $(MAKE) -C "$$controller_wt" _release-resume-run RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION) RELEASE_SOURCE_DIR="$$source_wt" $(if $(wildcard bin/mcp-publisher),MCP_PUBLISHER=$(CURDIR)/bin/mcp-publisher); then \
		git worktree remove --force "$$source_wt" || exit 1; \
		git worktree remove --force "$$controller_wt"; \
	else \
		echo "release-resume: resume failed; worktrees kept for inspection:" >&2; \
		echo "        controller: $$controller_wt" >&2; \
		echo "        source:     $$source_wt" >&2; \
		echo "        when done, remove each with: git worktree remove --force <path>" >&2; \
		exit 1; \
	fi

_release-resume-run:
	$(if $(filter default,$(origin MAKE)),,$(error _release-resume-run: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error _release-resume-run: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error _release-resume-run: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error _release-resume-run: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error _release-resume-run: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error _release-resume-run: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error _release-resume-run: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error _release-resume-run: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release-resume" ]; then \
		echo "_release-resume-run: internal pipeline body; invoke 'make release-resume RELEASE_VERSION=vX.Y.Z'" >&2; \
		exit 1; \
	fi
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "_release-resume-run: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	@if [ -z "$(RELEASE_SOURCE_DIR)" ] || [ ! -d "$(RELEASE_SOURCE_DIR)" ] || [ -L "$(RELEASE_SOURCE_DIR)" ]; then \
		echo "_release-resume-run: RELEASE_SOURCE_DIR must be the immutable tag worktree" >&2; \
		exit 1; \
	fi
	@source_root=$$(git -C "$(RELEASE_SOURCE_DIR)" rev-parse --show-toplevel) || exit 1; \
	source_root=$$(cd "$$source_root" && pwd -P) || exit 1; \
	source_dir=$$(cd "$(RELEASE_SOURCE_DIR)" && pwd -P) || exit 1; \
	release_sha=$$(git rev-parse --verify "refs/tags/$(RELEASE_VERSION)^{commit}") || exit 1; \
	source_sha=$$(git -C "$$source_dir" rev-parse --verify "HEAD^{commit}") || exit 1; \
	if [ "$$source_root" != "$$source_dir" ] || [ "$$source_sha" != "$$release_sha" ] \
		|| [ -n "$$(git -C "$$source_dir" status --porcelain --untracked-files=normal)" ]; then \
		echo "_release-resume-run: release source is not the clean exact tag worktree" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(RELEASE_SOURCE_DIR)/.claude-plugin/plugin.json" ] \
		|| [ -L "$(RELEASE_SOURCE_DIR)/.claude-plugin/plugin.json" ]; then \
		echo "_release-resume-run: tagged plugin manifest is missing or unsafe" >&2; \
		exit 1; \
	fi
	@expected=$$(echo "$(RELEASE_VERSION)" | sed 's/^v//'); \
	python3 -c 'import json, sys; from pathlib import Path; document = json.loads(Path(sys.argv[1]).read_text(encoding="utf-8")); sys.exit(f"_release-resume-run: tagged plugin version is not exact {sys.argv[2]}") if type(document) is not dict or document.get("version") != sys.argv[2] else None' \
		"$(RELEASE_SOURCE_DIR)/.claude-plugin/plugin.json" "$$expected"
	$(MAKE) release-controller-source-check RELEASE_VERSION=$(RELEASE_VERSION)
	@./scripts/check-release-ci-contract.sh
	$(MAKE) release-auth-preflight
	@# A matching version stamp is necessary but not release authority. Prove
	@# that this exact tagged SHA completed every source-controlled
	@# push-to-main workflow;
	@# origin/main and the current workflow catalog may legitimately have
	@# advanced since the tag was pushed.
	$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@release_state=$$(./scripts/github-release-state.sh "$(RELEASE_VERSION)") || exit 1; \
	if [ "$$release_state" = existing ]; then \
		echo "release-resume: GitHub release exists; hydrating and verifying its signed asset set"; \
		$(MAKE) release-github-assets RELEASE_VERSION=$(RELEASE_VERSION); \
		printf '%s\n' existing >"$(DIST_DIR)/.canary-resume-github-state"; \
	elif [ "$$release_state" = absent ]; then \
		echo "release-resume: GitHub release absent; assembling a fresh signed asset set"; \
		$(MAKE) release-binaries RELEASE_VERSION=$(RELEASE_VERSION); \
		printf '%s\n' absent >"$(DIST_DIR)/.canary-resume-github-state"; \
	else \
		echo "release-resume: internal GitHub release state is invalid" >&2; \
		exit 1; \
	fi
	@plugin_ref=$$(./scripts/check-release-tag.sh --plugin-ref "$(RELEASE_VERSION)") || exit 1; \
	if git ls-remote --exit-code origin "$$plugin_ref" >/dev/null 2>&1; then \
		$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION); \
	else \
		if git show-ref --verify --quiet "$$plugin_ref"; then \
			./scripts/check-release-tag.sh --plugin-local "$(RELEASE_VERSION)" || exit 1; \
		else \
			msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
			claude plugin tag "$(RELEASE_SOURCE_DIR)" --message "$$msg" && \
			./scripts/check-release-tag.sh --plugin-local "$(RELEASE_VERSION)" || exit 1; \
		fi; \
	fi
	@# Re-read the tagged SHA's latest attempts after all expensive recovery
	@# work and immediately before a missing plugin tag can be published.
	$(MAKE) _release-ci-wait-historical RELEASE_PIPELINE_ENTRY=release-resume
	$(MAKE) release-origin-check
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@plugin_ref=$$(./scripts/check-release-tag.sh --plugin-ref "$(RELEASE_VERSION)") || exit 1; \
	if git ls-remote --exit-code origin "$$plugin_ref" >/dev/null 2>&1; then \
		echo "release-resume: plugin tag $$plugin_ref already on origin; verifying"; \
	else \
		git push --no-follow-tags origin "$$plugin_ref"; \
	fi
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@resume_state=$$(cat "$(DIST_DIR)/.canary-resume-github-state" 2>/dev/null || true); \
	case "$$resume_state" in \
	existing) echo "release-resume: existing GitHub release already verified" ;; \
	absent) \
		msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
		$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION) RELEASE_SOURCE_DIR="$(RELEASE_SOURCE_DIR)" MESSAGE="$$msg" ;; \
	*) \
		echo "release-resume: internal GitHub release state is missing or invalid" >&2; \
		exit 1 ;; \
	esac
	$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) registry-publish-verify-first RELEASE_PIPELINE_ENTRY=release-resume RELEASE_VERSION=$(RELEASE_VERSION)
	@echo
	@echo "Resumed $(RELEASE_VERSION):"
	@echo "  https://github.com/osauer/canary/releases/tag/$(RELEASE_VERSION)"

# Internal: release pipeline body; runs inside the worktree created by
# `make release`. Deliberately not advertised in `make help`.
_release-run:
	$(if $(filter default,$(origin MAKE)),,$(error _release-run: MAKE must not be overridden))
	$(if $(filter file,$(origin MAKEFLAGS)),,$(error _release-run: MAKEFLAGS must not be overridden))
	$(if $(release_unsafe_makeflags),$(error _release-run: unsafe Make flags are forbidden),)
	$(if $(filter file,$(origin MAKEFILE_LIST)),,$(error _release-run: MAKEFILE_LIST must not be overridden))
	$(if $(strip $(MAKEFILES)),$(error _release-run: MAKEFILES must be empty),)
	$(if $(filter 1,$(words $(MAKEFILE_LIST))),,$(error _release-run: exactly one makefile is required))
	$(if $(filter Makefile,$(MAKEFILE_LIST)),,$(error _release-run: only the canonical Makefile is allowed))
	$(if $(release_overridden_vars),$(error _release-run: release variables must not be overridden: $(release_overridden_vars)),)
	@if [ "$(MAKELEVEL)" -lt 1 ] || [ "$(RELEASE_PIPELINE_ENTRY)" != "release" ]; then \
		echo "_release-run: internal pipeline body; invoke 'make release RELEASE_VERSION=vX.Y.Z'" >&2; \
		exit 1; \
	fi
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "_release-run: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	@expected=$$(echo "$(RELEASE_VERSION)" | sed 's/^v//'); \
	if ! grep -q "\"version\": \"$$expected\"" .claude-plugin/plugin.json; then \
		echo "_release-run: .claude-plugin/plugin.json version != $$expected in the release commit — commit the stamp first" >&2; \
		grep '"version"' .claude-plugin/plugin.json >&2; \
		exit 1; \
	fi
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "_release-run: release worktree is unexpectedly dirty" >&2; \
		git status --short >&2; \
		exit 1; \
	fi
	@# Land the release commit before anything expensive: the release ships
	@# exactly this commit, and origin/$(MAIN_BRANCH) must carry it. Plain
	@# push refuses non-fast-forward, so origin moving since fire aborts here.
	$(MAKE) release-origin-check
	git push --no-follow-tags origin HEAD:$(MAIN_BRANCH)
	@# Auth preflight before any expensive step: gh auth goes stale
	@# between releases and used to surface only at the LAST pipeline
	@# legs (v2.0.0 stranded twice on registry-publish). Actions OIDC is
	@# the normal registry path; this checks gh plus the local device-code
	@# fallback in case the workflow does not deliver the released version.
	$(MAKE) release-auth-preflight
	@# Validate the CHANGELOG entry shape before any expensive step. A
	@# malformed entry (wrong version heading, missing ### What's new, or
	@# no Keep-a-Changelog subsection) fails here, not after refresh-spx /
	@# test / build / smoke have already run.
	$(MAKE) changelog-lint RELEASE_VERSION=$(RELEASE_VERSION)
	@# Non-patch releases change the public product surface enough that the
	@# static osauer.dev/canary pages must be synced and pushed before tagging.
	$(MAKE) release-site-check RELEASE_VERSION=$(RELEASE_VERSION)
	@# Refresh the S&P-500 membership list from Wikipedia. The release
	@# flow runs this on every cut so every tagged binary carries a
	@# current list; a same-day refresh that produces no diff is a no-op.
	@# A real diff fails the release here: the maintainer commits the
	@# membership update separately and re-runs `make release`, so the
	@# git tag and the binary's checked-in list stay in lockstep. This
	@# guard runs AFTER the dirty-tree check above so we can attribute a
	@# dirty tree to the refresh, not to stray edits.
	$(MAKE) refresh-spx-members
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "_release-run: refresh-spx-members produced uncommitted changes." >&2; \
		echo "        in your main checkout: make refresh-spx-members, commit, push, then re-run \`make release\`." >&2; \
		git status --short >&2; \
		exit 1; \
	fi
	@# Hosted exact-SHA CI owns the complete check and test matrix. Its static
	@# contract pins every required command, so do not repeat that full work
	@# locally here. Plugin validation remains local because hosted CI
	@# intentionally has no Claude CLI.
	$(MAKE) plugin-check
	@# No paper-session gate runs here. Both the place/ack/cancel round-trip
	@# and the read-only WhatIf preflight were removed on 2026-08-06 by
	@# operator decision: each demanded a paper TWS login and a human dialog
	@# click on every cut. The release no longer touches the order path at
	@# all — neither transmit nor preview — and performs no broker write.
	@# Build the release binary with the target version stamped BEFORE
	@# tagging — pass VERSION explicitly so the build doesn't fall back
	@# to `git describe` (which wouldn't see the tag yet). The smoke
	@# script asserts `bin/canary version == $(RELEASE_VERSION)`, so the
	@# stamp has to match.
	$(MAKE) build VERSION=$(RELEASE_VERSION)
	@# No broker gate runs here. release-smoke was removed from the pipeline
	@# on 2026-08-06 by operator decision, completing the same day's removal
	@# of the paper round-trip and the WhatIf preflight: the release is now
	@# hermetic and depends on no external service.
	@#
	@# What it asserted is not deleted — `make smoke` and `make smoke-fast`
	@# run the identical wire invariants (quote-spy, chain-iv-source,
	@# regime-subs, account-summary, gamma-no-wait-envelope,
	@# status-handshake) and stay available. The reasoning: four of the six
	@# check failures a daily operator hits within minutes, and gating a
	@# mechanical publishing step on a live market session made releases
	@# non-deterministic. The two that catch silently-wrong data
	@# (chain-iv-source, regime-subs) are better run on a cadence than once
	@# per cut, which is what the manual targets are for.
	@#
	@# What is no longer proven before a publish: that the shipped binary
	@# talks to IBKR correctly at all. Run `make smoke-fast` after a release
	@# that touched the wire path.
	@# The push-triggered workflows have run in parallel with the local gates.
	@# Before crossing the tag boundary, require the exact candidate SHA,
	@# workflow identity, latest rerun state, and the complete source-controlled
	@# CI + pages job inventory to be completed/success. Missing or unavailable
	@# evidence fails closed. Then pin the mutable main ref immediately before
	@# tagging; the later atomic tag push reasserts it at publication time.
	$(MAKE) release-ci-wait
	$(MAKE) release-main-candidate-check
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	@$(MAKE) release-binaries RELEASE_VERSION=$(RELEASE_VERSION) || { \
		git tag -d $(RELEASE_VERSION) >/dev/null 2>&1; \
		exit 1; \
	}
	@# Assembly proves only that it built what it was asked for. Prove the
	@# fixed published inventory BEFORE the irreversible tag push, so a
	@# narrowed matrix fails while the tag is still a deletable local ref
	@# rather than stranding a public tag in the resume lane.
	@$(MAKE) release-payload-inventory-check RELEASE_VERSION=$(RELEASE_VERSION) || { \
		git tag -d $(RELEASE_VERSION) >/dev/null 2>&1; \
		exit 1; \
	}
	@# Artifact assembly can take long enough for a manual Actions rerun to
	@# start. Re-prove the latest attempts and mutable main ref immediately
	@# before the atomic remote tag push; remove the recoverable local tag if
	@# either final authority check fails.
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
	msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	claude plugin tag . --message "$$msg" && \
	./scripts/check-release-tag.sh --plugin-local "$(RELEASE_VERSION)" && \
	git push --no-follow-tags origin "$$plugin_ref"
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	$(MAKE) _release-publish RELEASE_PIPELINE_ENTRY=release RELEASE_VERSION=$(RELEASE_VERSION) MESSAGE="$$msg"
	$(MAKE) release-github-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) release-plugin-tag-candidate-check RELEASE_VERSION=$(RELEASE_VERSION)
	$(MAKE) registry-publish-verify-first RELEASE_PIPELINE_ENTRY=release RELEASE_VERSION=$(RELEASE_VERSION)
	@echo
	@echo "Released $(RELEASE_VERSION):"
	@echo "  https://github.com/osauer/canary/releases/tag/$(RELEASE_VERSION)"
	@echo
	@echo "Verify:"
	@echo "  bin/canary version"
	@echo "  test ! -e bin/ibkr && test ! -L bin/ibkr"
	@echo "  gh release view $(RELEASE_VERSION) --repo github.com/osauer/canary --json assets -q '.assets[].name'"
	@plugin_name=$$(sed -n 's/^[[:space:]]*\"name\":[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p' .claude-plugin/plugin.json | head -n1); \
	echo "  gh api repos/osauer/canary/git/refs/tags/$$plugin_name--$(RELEASE_VERSION) --jq '.object.sha'"
