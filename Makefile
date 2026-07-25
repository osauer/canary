.DEFAULT_GOAL := help

# `--match='v*'` excludes the `ibkr--vX.Y.Z` plugin tags created by
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
# tools, but ibkr is an end-user CLI binary and shouldn't require Go to
# be installed at runtime.
PREFIX ?= $(HOME)/.local
RESTART_TIMEOUT ?= 15s

CLAUDE_DIR ?= $(HOME)/.claude
CLAUDE_PLUGIN_ID ?= ibkr@ibkr
CLAUDE_PLUGIN_MARKETPLACE ?= $(CURDIR)
SKILL_DIR  ?= $(CLAUDE_DIR)/skills/ibkr
CODEX_DIR  ?= $(HOME)/.codex
CODEX_SKILL_DIR ?= $(CODEX_DIR)/skills/ibkr
SKILL_SRC  ?= skills/ibkr

MAIN_BRANCH ?= main
RELEASE_TEST_JOBS ?= 3
MCP_PUBLISHER ?= $(if $(wildcard bin/mcp-publisher),bin/mcp-publisher,mcp-publisher)
MCP_REGISTRY_AUTO_LOGIN ?= 1
MCP_REGISTRY_LOGIN_METHOD ?= github

.PHONY: help build install restart-daemon uninstall test test-pkg test-daemon clean install-plugin install-plugin-refresh install-skill uninstall-skill all check go-doc-check gofmt-check vet-check staticcheck-check govulncheck-check govuln-prewarm-install fmt app-check app-contract-check app-syntax-check app-governance-check app-active-alert-inbox-check app-alert-compat-check app-market-events-check app-service-worker-check remote-relay-check release-packaging-check app-refresh app-refresh-smoke app-smoke app-screenshots cli-screenshots app-lifecycle-smoke release release-binaries release-mcpb release-checksums release-registry-server registry-login release-auth-preflight registry-publish registry-publish-verify-first release-publish release-verify release-smoke release-paper-preflight release-site-check smoke smoke-build smoke-only smoke-fast version plugin-check parity-check modernize modernize-check refresh-spx-members hook-version-check registry-version-check changelog-check changelog-lint changelog-stub docs-html-check docs-html-regen account-data-check hook-behavior-check agent-config-check

help: ## List available targets
	@awk 'BEGIN {FS = ":.*##"; print "Available targets (default: help):\n"} \
		/^[a-zA-Z][a-zA-Z0-9_-]+:.*##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)
	@echo
	@echo "Common flow:  make fmt && make test && make build   (test already runs check)"
	@echo "Daemon flow:  make install restart-daemon   (FORCE=1 adds ibkr restart --force; refreshes any running app)"
	@echo "Release flow: make release RELEASE_VERSION=vX.Y.Z   (clean tree + HEAD == origin/$(MAIN_BRANCH))"
	@echo "              tags + pushes + cross-compiles + creates GitHub Release with binaries attached"

build: ## Compile bin/ibkr with version stamped via ldflags
	@mkdir -p bin
	go build $(GO_BUILD_TAGS) -ldflags '$(LDFLAGS)' -o bin/ibkr ./cmd/ibkr
	@case " $(GO_TAGS) " in (*" trading "*) ;; (*) \
		echo "NOTE: built WITHOUT broker-write capability (read-only daemon)."; \
		echo "      Installing this over a trading build silently downgrades it."; \
		echo "      For order placement build with: make install GO_TAGS=trading"; \
	;; esac

install: build ## Install ibkr to $(PREFIX)/bin (default ~/.local/bin); skips the copy when byte-identical
	@if cmp -s bin/ibkr $(PREFIX)/bin/ibkr; then \
		echo "ibkr unchanged at $(PREFIX)/bin/ibkr — skipping copy"; \
	else \
		install -d $(PREFIX)/bin && \
		install -m 0755 bin/ibkr $(PREFIX)/bin/ibkr && \
		echo "Installed ibkr to $(PREFIX)/bin" && \
		echo "Restart the daemon and any running app with: $(PREFIX)/bin/ibkr restart"; \
	fi

# Skip the bounce when the freshly-built binary is byte-identical to the
# installed one AND the canonical daemon is already running — restarting
# then buys nothing, disturbs other sessions' in-flight CLI calls, and
# re-rolls the TWS client-slot retention race on the pinned client ID.
# Byte-identity is meaningful because DATE stamps the commit date (above),
# so unchanged source ⇒ unchanged binary. A daemon that is NOT running is
# left alone too (the next CLI call autospawns it — that is the design).
# FORCE=1 always installs and restarts.
restart-daemon: build ## Install + restart daemon, skipped when the binary is unchanged (FORCE=1 always bounces)
	@if [ -z "$(FORCE)" ] && cmp -s bin/ibkr $(PREFIX)/bin/ibkr && pgrep -f "$(PREFIX)/bin/ibkr daemon" >/dev/null 2>&1; then \
		echo "binary unchanged and daemon running — skipping restart (FORCE=1 to bounce anyway)"; \
	else \
		$(MAKE) --no-print-directory install && \
		$(PREFIX)/bin/ibkr restart --timeout $(RESTART_TIMEOUT) $(if $(FORCE),--force,); \
	fi

APP_SMOKE_URL ?= http://127.0.0.1:8765
APP_SMOKE_BROWSER ?= chromium
app-check: app-contract-check app-syntax-check app-governance-check app-active-alert-inbox-check app-alert-compat-check app-market-events-check app-service-worker-check ## Fast SPA gate: JS syntax + static app contracts

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

app-governance-check: ## Execute governance refresh, cutover, and attempt-redaction contracts in a Node VM
	@command -v node >/dev/null 2>&1 || { echo "app-governance-check: node not found — this gate is binding, install Node.js" >&2; exit 1; }
	node --test web/app/test/governance-ui.test.mjs

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
	$(PREFIX)/bin/ibkr restart --app --timeout $(RESTART_TIMEOUT)
	@for i in $$(seq 1 60); do \
		if curl -fsS $(APP_SMOKE_URL)/manifest.webmanifest >/dev/null 2>&1; then break; fi; \
		sleep 0.5; \
	done
	$(PREFIX)/bin/ibkr app pair --public-url $(APP_SMOKE_URL) --json

app-refresh-smoke: app-refresh ## Refresh the shared app host, then run the browser app smoke
	$(MAKE) app-smoke APP_SMOKE_URL=$(APP_SMOKE_URL) APP_SMOKE_BROWSER=$(APP_SMOKE_BROWSER)

app-smoke: ## Browser-smoke a running ibkr app without scanning a QR code
	node scripts/app-browser-smoke.mjs --base-url $(APP_SMOKE_URL) --browser $(APP_SMOKE_BROWSER) --no-notification

# The complete monitor snapshot is synthetic before any published image is
# written, covering positions, symbols, orders, and proposals as well as the
# account id and balances. See docs/social/canary-app-{mobile,desktop}.png.
app-screenshots: ## Regenerate the published app screenshots from a running ibkr app (fully synthetic data)
	node scripts/app-screenshots.mjs --base-url $(APP_SMOKE_URL) --browser $(APP_SMOKE_BROWSER) --synthetic

cli-screenshots: ## Regenerate the published CLI screenshots from cmd/_preview fixtures
	node scripts/cli-screenshots.mjs

APP_LIFECYCLE_ADDR ?= 127.0.0.1:18765
APP_LIFECYCLE_URL ?= http://$(APP_LIFECYCLE_ADDR)
app-lifecycle-smoke: build ## Start an isolated app, pair, restart it, and verify browser SSE/auth recovery
	@tmpdir=$$(mktemp -d /tmp/ibkr-app-lifecycle-smoke.XXXXXX); \
	app="$$(pwd)/bin/ibkr"; \
	log="$$tmpdir/app.log"; \
	cleanup() { \
		kill -TERM "$$app_pid" >/dev/null 2>&1 || true; \
		wait "$$app_pid" >/dev/null 2>&1 || true; \
		rm -rf "$$tmpdir"; \
	}; \
	trap cleanup EXIT; \
	"$$app" app --addr $(APP_LIFECYCLE_ADDR) --public-url $(APP_LIFECYCLE_URL) --state-dir "$$tmpdir" >"$$log" 2>&1 & \
	app_pid=$$!; \
	for i in $$(seq 1 80); do \
		if curl -fsS "$(APP_LIFECYCLE_URL)/manifest.webmanifest" >/dev/null 2>&1; then break; fi; \
		sleep 0.1; \
	done; \
	curl -fsS "$(APP_LIFECYCLE_URL)/manifest.webmanifest" >/dev/null; \
	node scripts/app-browser-smoke.mjs \
		--base-url $(APP_LIFECYCLE_URL) \
		--browser $(APP_SMOKE_BROWSER) \
		--no-notification \
		--no-webcrypto=true \
		--lifecycle=true \
		--restart-command "$$app restart --app --json --timeout $(RESTART_TIMEOUT)" \
		--stop-restarted-app=true

uninstall: ## Remove ibkr from $(PREFIX)/bin
	rm -f $(PREFIX)/bin/ibkr
	@echo "Removed ibkr from $(PREFIX)/bin"

TEST_JOBS ?= 3
TEST_MAKEFLAGS = $(if $(filter 0,$(MAKELEVEL)),-j$(TEST_JOBS),)
test: ## Full gate: check + pkg tests + daemon/integration tests (-race), overlapped by default
	$(MAKE) $(TEST_MAKEFLAGS) check test-pkg test-daemon

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
CHECK_TARGETS = $(CHECK_DEPS) agent-config-check modernize-check docs-check docs-html-check changelog-check account-data-check release-packaging-check app-contract-check app-syntax-check app-governance-check app-active-alert-inbox-check app-alert-compat-check app-market-events-check app-service-worker-check remote-relay-check go-doc-check gofmt-check vet-check staticcheck-check govulncheck-check
CHECK_MAKEFLAGS = $(if $(filter 0,$(MAKELEVEL)),-j$(CHECK_JOBS),)
check: ## agent config/hooks + Go docs/format/vet/staticcheck/vulns + modernize/plugin/parity/docs/changelog/account/app checks (binding pre-commit gate)
	$(MAKE) $(CHECK_MAKEFLAGS) $(CHECK_TARGETS)

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
# (the date bound keeps DB updates flowing in daily). The release path
# forces a full run via GOVULN_FORCE=1; CI runners have no stamp cache
# and always run.
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

govuln-prewarm-install: ## Install a 06:00 LaunchAgent that pre-warms the daily govulncheck stamp (uninstall: scripts/install-govuln-prewarm.sh --uninstall)
	@./scripts/install-govuln-prewarm.sh

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
registry-version-check: ## Ensure server.json version tracks .claude-plugin/plugin.json
	@command -v jq >/dev/null 2>&1 || { echo "jq missing on PATH; install jq or skip"; exit 1; }
	@reg=$$(jq -r '.version // empty' server.json); \
	plg=$$(jq -r '.version // empty' .claude-plugin/plugin.json); \
	if [ -z "$$reg" ] || [ -z "$$plg" ] || [ "$$reg" != "$$plg" ]; then \
		echo "registry-version-check: server.json version ($$reg) != .claude-plugin/plugin.json version ($$plg); keep them in lockstep" >&2; \
		exit 1; \
	fi

# The pre-tool-use hook is a broker guardrail with real routing logic
# (read-only allowlists, write gates, composition checks). It shipped for
# weeks blocking the read-only `ibkr orders` journal view, caught only by
# a human. Table-driven behavior cases keep both failure directions gated:
# false-allow (agent reaches a write) and false-block (read paths break).
hook-behavior-check: ## Run table-driven allow/block cases against the broker hooks
	@bash hooks/ibkr-pre-tool-use_test.sh
	@HOOK_UNDER_TEST=.codex/hooks/ibkr-pre-tool-use.sh bash hooks/ibkr-pre-tool-use_test.sh

agent-config-check: hook-behavior-check ## Validate project agent config, hooks, and read-only reviewer roles
	@bash -n hooks/ibkr-pre-tool-use.sh .codex/hooks/ibkr-pre-tool-use.sh
	@jq -e . .codex/hooks.json >/dev/null
	@jq -e . .claude/settings.json >/dev/null
	@go test ./internal/agentconfig/
	@if command -v codex >/dev/null 2>&1; then \
		read_decision=$$(codex execpolicy check --rules .codex/rules/ibkr.rules -- ibkr status --json | jq -r .decision); \
		write_decision=$$(codex execpolicy check --rules .codex/rules/ibkr.rules -- ibkr order place --preview-token TOKEN --json | jq -r .decision); \
		human_only_decision=$$(codex execpolicy check --rules .codex/rules/ibkr.rules -- ibkr settings set trading.freeze=true | jq -r .decision); \
		offline_gate_decision=$$(codex execpolicy check --rules .codex/rules/ibkr.rules -- make check | jq -r .decision); \
		live_gate_decision=$$(codex execpolicy check --rules .codex/rules/ibkr.rules -- make restart-daemon | jq -r .decision); \
		smoke_decision=$$(codex execpolicy check --rules .codex/rules/ibkr.rules -- make smoke | jq -r .decision); \
		release_decision=$$(codex execpolicy check --rules .codex/rules/ibkr.rules -- make release RELEASE_VERSION=v2.3.1 | jq -r .decision); \
		release_preflight_decision=$$(codex execpolicy check --rules .codex/rules/ibkr.rules -- make release-paper-preflight VERSION=v2.3.1 | jq -r .decision); \
		[ "$$read_decision" = allow ] && [ "$$write_decision" = prompt ] && [ "$$human_only_decision" = forbidden ] \
			&& [ "$$offline_gate_decision" = allow ] && [ "$$live_gate_decision" = prompt ] && [ "$$smoke_decision" = prompt ] \
			&& [ "$$release_decision" = prompt ] && [ "$$release_preflight_decision" = prompt ] || { \
			echo "execpolicy decisions: read=$$read_decision write=$$write_decision human-only=$$human_only_decision offline-gate=$$offline_gate_decision live-gate=$$live_gate_decision smoke=$$smoke_decision release=$$release_decision release-preflight=$$release_preflight_decision" >&2; exit 1; \
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
# resource counterpart — it pins the ibkr://… template inventory the
# server actually exposes. TestSkill* in internal/cli is the skill-layer
# counterpart: every CLI command documented in skills/ibkr/SKILL.md (or
# excluded with a reason), the allowed-tools list mirrored exactly in
# settings/ibkr.settings.json, and no broker/state write allowlisted.
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
# for config-ref; the internal/mcp.Tools registry for mcp-tools). Run
# this after editing internal/config/config.go, internal/mcp/tools.go,
# or adding/changing a // docgen:env comment, and commit the diff
# alongside the source change. `make docs-check` enforces no drift.
docs-regen: ## Regenerate docs/reference/*.md and their public HTML derivatives
	go run ./scripts/docgen/config-ref
	go run ./scripts/docgen/mcp-tools
	go run ./scripts/check-mcp-server-card -write
	cp docs/mcp-server.json docs/.well-known/mcp/server.json
	go run ./scripts/docgen/docs-html

# docs-check is the CI gate: regenerate to a tempfile, diff against the
# checked-in copy, fail if they differ. Catches the "I changed a struct
# tag but forgot to regen" case. Wired into `make check` so it cannot
# be skipped. Uses POSIX tempfiles (not bash process substitution) so
# the recipe runs under /bin/sh on every host.
docs-check: ## Verify checked-in docs/reference/*.md match what the generators emit
	@go test ./scripts/docgen/config-ref
	@go run ./scripts/check-mcp-server-card
	@cmp -s docs/mcp-server.json docs/.well-known/mcp/server.json || { \
		echo "docs-check: docs/.well-known/mcp/server.json differs from canonical docs/mcp-server.json" >&2; \
		echo "            run \`make docs-regen\` to refresh the public discovery copy" >&2; \
		exit 1; \
	}
	@tmp=$$(mktemp -d); trap 'rm -rf "$$tmp"' EXIT; \
	fail=0; \
	for gen in config-ref mcp-tools; do \
		case $$gen in \
			config-ref) ref=docs/docs/reference/config.md ;; \
			mcp-tools) ref=docs/docs/reference/mcp-tools.md ;; \
		esac; \
		go run ./scripts/docgen/$$gen -o "$$tmp/$$gen.md" || exit 1; \
		if ! diff -u "$$ref" "$$tmp/$$gen.md" > /dev/null 2>&1; then \
			echo "docs-check: $$ref out of date; run \`make docs-regen\`"; \
			diff -u "$$ref" "$$tmp/$$gen.md" || true; \
			fail=1; \
		fi; \
	done; \
	exit $$fail

# Markdown is the only prose authority for the 14 public documentation pages
# declared by scripts/docgen/docs-html. GitHub Pages currently publishes docs/
# verbatim, so deterministic HTML derivatives stay checked in. The check
# re-renders every declared page and compares exact bytes; it never trusts a
# marker stored in the derivative.
docs-html-check: ## Verify generated docs/ HTML exactly matches Markdown sources
	@go test ./scripts/docgen/docs-html
	@go run ./scripts/docgen/docs-html -check
	@node scripts/render-architecture.mjs --check

docs-html-regen: ## Regenerate public docs/ HTML from Markdown sources
	@go run ./scripts/docgen/docs-html

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

# Daemon + CLI integration tests. -race is on for the daemon path because
# this layer carries the goroutines (subscriptions, idle timer, signal
# handlers); race detector earns its slot here. Integration tests skip
# cleanly when no IBKR gateway is reachable.
#
# The integration leg is serialized across sessions via with-gateway-lock:
# its client IDs and daemon spawns hit the shared TWS gateway, and two
# overlapping runs used to flake with error 326 and force a full re-run.
test-daemon: ## Run internal/... and test/integration/... under -race (incl. trading-tag write path)
	go test -race -timeout=240s ./internal/...
	./scripts/with-gateway-lock.sh go test -race -count=1 -timeout=420s ./test/integration/...
	go test -race -timeout=240s -tags trading ./internal/daemon/...

# Install the standalone skill bundle directly under global agent skill roots.
# Dogfood path only — end users get the skill via `/plugin install ibkr`.
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
	@echo "  /plugin marketplace add osauer/ibkr"
	@echo "  /plugin install ibkr"
	@echo
	@echo "For a global Bash(ibkr ...) allowlist, copy settings/ibkr.settings.json"
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
release-verify: ## Smoke-test the local bin/ibkr against a live gateway (called by `make release`)
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
	@if [ ! -x bin/ibkr ]; then \
		echo "release-verify: bin/ibkr missing — run 'make build' first" >&2; \
		exit 1; \
	fi
	./scripts/with-gateway-lock.sh ./scripts/release-verify.sh bin/ibkr $(RELEASE_VERSION)

release-smoke: smoke-build ## Release gate: JSON contract + wire smoke in one reachable TWS/Gateway daemon session
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-smoke: RELEASE_VERSION is required, e.g. make release-smoke RELEASE_VERSION=v0.15.1" >&2; \
		exit 1; \
	fi
	@if [ ! -x bin/ibkr ]; then \
		echo "release-smoke: bin/ibkr missing — run 'make build VERSION=$(RELEASE_VERSION)' first" >&2; \
		exit 1; \
	fi
	IBKR_SMOKE_STRICT=$(SMOKE_STRICT) SPX_EXPECTED_REACHABLE=$(SPX_EXPECTED_REACHABLE) ./scripts/with-gateway-lock.sh ./scripts/release-smoke.sh bin/ibkr $(RELEASE_VERSION) bin/wire-assert

release-paper-preflight: build ## Early read-only paper account/FX/WhatIf gate; never submits an order
	./scripts/with-gateway-lock.sh ./scripts/release-paper-smoke.sh --preview-only bin/ibkr

release-site-check: ## Require osauer.dev/ibkr static site sync for non-patch releases
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-site-check: RELEASE_VERSION is required, e.g. make release-site-check RELEASE_VERSION=v1.8.0" >&2; \
		exit 1; \
	fi
	./scripts/check-release-site-sync.sh $(RELEASE_VERSION)

smoke-build: ## Compile the bin/wire-assert helper used by `make smoke`
	@mkdir -p bin
	go build -o bin/wire-assert ./cmd/wire-assert

# Run wire-smoke against the *existing* bin/ibkr without rebuilding it.
# The release flow uses this so it can exercise the version-stamped
# binary produced by `make build VERSION=$(RELEASE_VERSION)`, instead
# of clobbering that stamp with a `git describe` rebuild via the smoke
# dep chain.
#
# Drives bin/ibkr against a reachable TWS/Gateway session with the wire interceptor
# enabled and asserts per-command protocol-level invariants — catches
# bugs the unit suite can't see (e.g. the v0.24.x productionLegFetcher
# bug where the gateway sent the right ticks but the daemon read the
# wrong field).
#
# SMOKE_STRICT controls the no-gateway posture (forwarded to the script
# as IBKR_SMOKE_STRICT):
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
# a bug on my setup." If `ibkr gamma --only=spx` returns the
# entitlement-skipped banner, fail loudly rather than silently passing
# the smoke. Override with `make smoke SPX_EXPECTED_REACHABLE=0` on
# accounts that legitimately lack SPX entitlement.
SPX_EXPECTED_REACHABLE ?= 1

smoke-only: smoke-build ## Run wire smoke against existing bin/ibkr (no rebuild); SMOKE_STRICT=1 makes no-gateway a failure
	@if [ ! -x bin/ibkr ]; then \
		echo "smoke-only: bin/ibkr missing — run 'make build' first" >&2; \
		exit 1; \
	fi
	IBKR_SMOKE_STRICT=$(SMOKE_STRICT) SPX_EXPECTED_REACHABLE=$(SPX_EXPECTED_REACHABLE) ./scripts/with-gateway-lock.sh ./scripts/wire-smoke.sh bin/ibkr bin/wire-assert

smoke: build smoke-only ## Wire-level smoke vs. reachable TWS/Gateway (rebuilds bin/ibkr; SKIP if no gateway)

# The per-commit inner-loop gate: boot + handshake + quote + account
# against a real gateway (~15s) instead of the full wire matrix. The full
# `make smoke` stays binding for daemon/CLI wire-path changes and for
# releases — this tier exists so a docs/proposal/SPA change doesn't pay
# the chain/regime/gamma fan-out every commit.
smoke-fast: build smoke-build ## Fast wire smoke: boot + quote + account only (~15s; full matrix stays in `make smoke`)
	IBKR_SMOKE_FAST=1 IBKR_SMOKE_STRICT=$(SMOKE_STRICT) ./scripts/with-gateway-lock.sh ./scripts/wire-smoke.sh bin/ibkr bin/wire-assert

release-binaries: ## Cross-compile read-only + trading tarballs and the (read-only) MCPB into dist/ — needs RELEASE_VERSION=vX.Y.Z
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

release-registry-server: ## Generate and validate dist/server.json for MCP Registry publishing
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-registry-server: RELEASE_VERSION is required, e.g. make release-registry-server RELEASE_VERSION=v1.2.1" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb" ]; then \
		echo "release-registry-server: missing $(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb; run make release-mcpb" >&2; \
		exit 1; \
	fi
	go run ./scripts/release-registry-server $(RELEASE_VERSION) "$(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb" "$(DIST_DIR)/server.json"
	$(MCP_PUBLISHER) validate "$(DIST_DIR)/server.json"

registry-login: ## Refresh MCP Registry auth token (default: GitHub device flow)
	$(MCP_PUBLISHER) login $(MCP_REGISTRY_LOGIN_METHOD)

release-auth-preflight: ## Fail-fast gh auth + registry fallback preconditions (device code only if Actions OIDC fails)
	MCP_REGISTRY_AUTO_LOGIN=$(MCP_REGISTRY_AUTO_LOGIN) \
		./scripts/release-auth-preflight.sh "$(MCP_PUBLISHER)" "$(MCP_REGISTRY_LOGIN_METHOD)"

registry-publish: release-registry-server ## Publish dist/server.json, refreshing expired Registry auth when needed
	MCP_REGISTRY_AUTO_LOGIN=$(MCP_REGISTRY_AUTO_LOGIN) MCP_REGISTRY_LOGIN_METHOD=$(MCP_REGISTRY_LOGIN_METHOD) \
		./scripts/registry-publish-with-login.sh "$(MCP_PUBLISHER)" "$(DIST_DIR)/server.json"

registry-publish-verify-first: ## Release-only: wait for Actions OIDC, then fall back to direct login + publish
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "registry-publish-verify-first: RELEASE_VERSION is required, e.g. make registry-publish-verify-first RELEASE_VERSION=v1.2.1" >&2; \
		exit 1; \
	fi
	@./scripts/registry-publish-verify-first.sh "$(RELEASE_VERSION)" \
		make --no-print-directory registry-publish \
		RELEASE_VERSION="$(RELEASE_VERSION)" DIST_DIR="$(DIST_DIR)" \
		MCP_PUBLISHER="$(MCP_PUBLISHER)" MCP_REGISTRY_AUTO_LOGIN="$(MCP_REGISTRY_AUTO_LOGIN)" \
		MCP_REGISTRY_LOGIN_METHOD="$(MCP_REGISTRY_LOGIN_METHOD)"

# Compose the GitHub Release notes by substituting __VERSION__ and
# __HIGHLIGHTS__ in the install-header template, then appending the
# matching CHANGELOG.md entry. __HIGHLIGHTS__ is pulled from the entry's
# `### What's new` section, so the release body's top stanza is mechanically
# derived from CHANGELOG — no second place to drift. Marks the new release
# as latest.
release-publish: ## Create the GitHub Release page (notes + binaries) — RELEASE_VERSION required
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release-publish: RELEASE_VERSION is required, e.g. make release-publish RELEASE_VERSION=v0.6.0" >&2; \
		exit 1; \
	fi
	@if [ ! -d "$(DIST_DIR)" ] || [ ! -f "$(DIST_DIR)/SHA256SUMS" ]; then \
		echo "release-publish: $(DIST_DIR)/ missing or empty; run \`make release-binaries RELEASE_VERSION=$(RELEASE_VERSION)\` first" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/SHA256SUMS.asc" ]; then \
		echo "release-publish: $(DIST_DIR)/SHA256SUMS.asc missing — `ibkr update` from v1.0+ requires the signature; re-run release-binaries" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb" ]; then \
		echo "release-publish: $(DIST_DIR)/ibkr-$(RELEASE_VERSION).mcpb missing — re-run release-binaries" >&2; \
		exit 1; \
	fi
	@if [ ! -f "$(DIST_DIR)/ibkr.mcpb" ]; then \
		echo "release-publish: $(DIST_DIR)/ibkr.mcpb missing — re-run release-binaries" >&2; \
		exit 1; \
	fi
	@command -v gh >/dev/null 2>&1 || { echo "release-publish: gh CLI not on PATH; brew install gh" >&2; exit 1; }
	$(MAKE) changelog-lint RELEASE_VERSION=$(RELEASE_VERSION)
	@notes=$$(mktemp -t ibkr-release-notes.XXXXXX) && \
	highlights=$$(mktemp -t ibkr-release-highlights.XXXXXX) && \
	trap 'rm -f $$notes $$highlights' EXIT && \
	awk -v ver='$(RELEASE_VERSION)' '/^## v[0-9]/{ if(in_ver) exit; in_ver = ($$0 ~ "^## "ver" "); next } in_ver && /^### What.s new$$/{ in_new=1; next } in_ver && in_new && /^###/{ exit } in_new' CHANGELOG.md > $$highlights && \
	awk -v ver='$(RELEASE_VERSION)' -v hf="$$highlights" '{ gsub(/__VERSION__/, ver) } /__HIGHLIGHTS__/{ while ((getline line < hf) > 0) print line; close(hf); next } { print }' .github/release-notes-template.md > $$notes && \
	awk -v ver='$(RELEASE_VERSION)' '/^## v[0-9]/{ in_section = ($$0 ~ "^## " ver " "); skip=0; if(in_section){ next } } in_section && /^### What.s new$$/{ skip=1; next } in_section && skip && /^### /{ skip=0 } in_section && !skip' CHANGELOG.md >> $$notes && \
	title="$${MESSAGE:-$(RELEASE_VERSION)}" && \
	gh release create $(RELEASE_VERSION) --notes-file $$notes --title "$$title" --latest $(DIST_DIR)/*.tar.gz $(DIST_DIR)/*.mcpb $(DIST_DIR)/SHA256SUMS $(DIST_DIR)/SHA256SUMS.asc

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
	@RELEASE_VERSION=$(RELEASE_VERSION) ./scripts/check-changelog-entry.sh

changelog-stub: ## Prepend a CHANGELOG.md entry skeleton for RELEASE_VERSION
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "changelog-stub: RELEASE_VERSION is required, e.g. make changelog-stub RELEASE_VERSION=v0.27.12" >&2; \
		exit 1; \
	fi
	@RELEASE_VERSION=$(RELEASE_VERSION) ./scripts/changelog-stub.sh

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
# - HEAD not synced with origin/<MAIN_BRANCH> (tag would point at a commit
#   GitHub doesn't have)
# - tag already exists locally or on origin
# Sequence: cheap gates → read-only paper preview → full tests → stamped build
# → live/paper smoke → annotated tag → publish. The early preview exercises the
# account-currency, FX, and broker WhatIf path before the expensive test suite.
# The transmitting smoke still runs only after every test and before tagging.
# Does NOT push commits — push those first.
release: ## Tag and push a release: make release RELEASE_VERSION=vX.Y.Z [MESSAGE="..."]
	@if [ -z "$(RELEASE_VERSION)" ]; then \
		echo "release: RELEASE_VERSION is required, e.g. make release RELEASE_VERSION=v0.3.1" >&2; \
		exit 1; \
	fi
	@if ! echo "$(RELEASE_VERSION)" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$$'; then \
		echo "release: RELEASE_VERSION must look like vX.Y.Z (got $(RELEASE_VERSION))" >&2; \
		exit 1; \
	fi
	@expected=$$(echo "$(RELEASE_VERSION)" | sed 's/^v//'); \
	if ! grep -q "\"version\": \"$$expected\"" .claude-plugin/plugin.json; then \
		echo "release: .claude-plugin/plugin.json version != $$expected — bump it before releasing so plugin tag agrees with binary tag" >&2; \
		grep '"version"' .claude-plugin/plugin.json >&2; \
		exit 1; \
	fi
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "release: working tree is dirty; commit or stash first" >&2; \
		git status --short >&2; \
		exit 1; \
	fi
	@head=$$(git rev-parse HEAD); \
	main=$$(git rev-parse origin/$(MAIN_BRANCH) 2>/dev/null) || { \
		echo "release: origin/$(MAIN_BRANCH) ref missing locally; run 'git fetch origin $(MAIN_BRANCH)' first" >&2; \
		exit 1; \
	}; \
	if [ "$$head" != "$$main" ]; then \
		echo "release: HEAD ($$head) does not match origin/$(MAIN_BRANCH) ($$main); push your commits first" >&2; \
		exit 1; \
	fi
	@if git rev-parse --verify --quiet $(RELEASE_VERSION) >/dev/null; then \
		echo "release: tag $(RELEASE_VERSION) already exists locally" >&2; \
		exit 1; \
	fi
	@if git ls-remote --tags --exit-code origin $(RELEASE_VERSION) >/dev/null 2>&1; then \
		echo "release: tag $(RELEASE_VERSION) already exists on origin" >&2; \
		exit 1; \
	fi
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
	@# static osauer.dev/ibkr pages must be synced and pushed before tagging.
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
		echo "release: refresh-spx-members produced uncommitted changes." >&2; \
		echo "        commit the S&P-500 membership update and re-run \`make release\`." >&2; \
		git status --short >&2; \
		exit 1; \
	fi
	@# Exercise the paper account, cross-currency notional, and broker WhatIf
	@# path before the ~10-minute full gate. This mints an isolated preview
	@# token but never places an order; the binding place/ack/cancel smoke stays
	@# below, after all tests. Late v2.3.0 preview failures motivated this gate.
	$(MAKE) release-paper-preflight VERSION=$(RELEASE_VERSION)
	@# Run the existing full test gate with parallel prerequisites. This
	@# keeps the same checks/tests but overlaps check, pkg, and daemon work.
	@# GOVULN_FORCE=1: the release gate never takes the daily-stamp skip.
	$(MAKE) -j$(RELEASE_TEST_JOBS) test GOVULN_FORCE=1
	@# Build the release binary with the target version stamped BEFORE
	@# tagging — pass VERSION explicitly so the build doesn't fall back
	@# to `git describe` (which wouldn't see the tag yet). The smoke
	@# script asserts `bin/ibkr version == $(RELEASE_VERSION)`, so the
	@# stamp has to match.
	$(MAKE) build VERSION=$(RELEASE_VERSION)
	@# Binding TWS/Gateway JSON + wire smoke against the freshly-stamped
	@# binary. Runs one isolated daemon session and fails on no gateway.
	$(MAKE) release-smoke RELEASE_VERSION=$(RELEASE_VERSION) SMOKE_STRICT=1
	@# Binding paper-trading smoke (2026-06-10 decision): the order
	@# pipeline is verified automatically per release — place/ack/cancel
	@# a 1-share paper round-trip via an isolated daemon pinned to the
	@# local PAPER session. No SKIP: a missing paper login aborts the
	@# release. This replaces the human-certified runtime live gate.
	./scripts/with-gateway-lock.sh ./scripts/release-paper-smoke.sh bin/ibkr
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	git tag -a $(RELEASE_VERSION) -m "$$msg"
	@$(MAKE) release-binaries RELEASE_VERSION=$(RELEASE_VERSION) || { \
		git tag -d $(RELEASE_VERSION) >/dev/null 2>&1; \
		exit 1; \
	}
	git push origin $(RELEASE_VERSION)
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	claude plugin tag . --push --message "$$msg"
	@msg="$${MESSAGE:-$(RELEASE_VERSION)}"; \
	$(MAKE) release-publish RELEASE_VERSION=$(RELEASE_VERSION) MESSAGE="$$msg"
	$(MAKE) registry-publish-verify-first RELEASE_VERSION=$(RELEASE_VERSION)
	@echo
	@echo "Released $(RELEASE_VERSION):"
	@echo "  https://github.com/osauer/ibkr/releases/tag/$(RELEASE_VERSION)"
	@echo
	@echo "Verify:"
	@echo "  bin/ibkr version"
	@echo "  gh release view $(RELEASE_VERSION) --json assets -q '.assets[].name'"
	@echo "  gh api repos/osauer/ibkr/git/refs/tags/ibkr--$(RELEASE_VERSION) --jq '.object.sha'"
