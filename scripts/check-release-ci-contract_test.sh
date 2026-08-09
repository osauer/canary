#!/bin/sh

set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
checker="$repo_root/scripts/check-release-ci-contract.sh"
test_root=$(mktemp -d "${TMPDIR:-/tmp}/canary-release-ci-contract-test.XXXXXX")
fixture="$test_root/repo"

cleanup() {
	rm -rf "$test_root"
}
trap cleanup EXIT HUP INT TERM

write_makefile() {
	cat >"$fixture/Makefile" <<'EOF'
release-ci-wait:
	@GOFLAGS= go run ./scripts/release-ci-wait \
		-contract scripts/release-ci-contract.json \
		-sha "$$(git rev-parse HEAD)" -branch "$(MAIN_BRANCH)" -event push \
		-poll "$(RELEASE_CI_POLL)" -timeout "$(RELEASE_CI_TIMEOUT)"

_release-ci-wait-historical:
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

unrelated:
	@true
EOF
}

write_manifest() {
	mkdir -p "$fixture/scripts"
	cat >"$fixture/scripts/release-ci-contract.json" <<'EOF'
{
  "repository": "osauer/canary",
  "workflows": [
    {
      "file": "ci.yml",
      "name": "ci",
      "jobs": [
        "make check (lint + vet + vulncheck + parity)",
        "make test (ubuntu-latest)",
        "make test-daemon-default (sharded -race)",
        "make test-daemon-trading (sharded -race)",
        "isolated Canary app render",
        "cross-compile release matrix"
      ]
    },
    {
      "file": "pages-check.yml",
      "name": "pages check",
      "jobs": [
        "local page targets"
      ]
    }
  ]
}
EOF
	cat >"$fixture/scripts/release-ci-legacy-contracts.json" <<'EOF'
{
  "3b548f6d63286448ac132ca4ade66484952612f5": {
    "repository": "osauer/canary",
    "workflows": [
      {
        "file": "ci.yml",
        "name": "ci",
        "jobs": [
          "make check (lint + vet + vulncheck + parity)",
          "make test (ubuntu-latest)",
          "make test (macos-latest)",
          "cross-compile release matrix"
        ]
      },
      {
        "file": "pages-check.yml",
        "name": "pages check",
        "jobs": [
          "local page targets"
        ]
      }
    ]
  }
}
EOF
}

write_workflows() {
	mkdir -p "$fixture/.github/workflows"
	cat >"$fixture/.github/workflows/ci.yml" <<'EOF'
name: ci

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  check:
    name: make check (lint + vet + vulncheck + parity)
    runs-on: ubuntu-latest
    steps:
      - name: make check
        run: make check CHECK_DEPS=parity-check
  test:
    name: make test (${{ matrix.os }})
    runs-on: ${{ matrix.os }}
    strategy:
      fail-fast: false
      matrix:
        os: [ubuntu-latest]
    steps:
      - name: make test-pkg
        run: make test-pkg
      - name: make test-support (-race; command and CI/release helpers)
        run: make test-support
      - name: make test-internal (-race; internal minus daemon root)
        run: make test-internal
  test-daemon-default:
    name: make test-daemon-default (sharded -race)
    runs-on: ubuntu-latest
    steps:
      - name: make test-daemon-default (shards + hermetic integration)
        run: make test-daemon-default
  test-daemon-trading:
    name: make test-daemon-trading (sharded -race)
    runs-on: ubuntu-latest
    steps:
      - name: make test-daemon-trading (trading shards)
        run: make test-daemon-trading
  app-render:
    name: isolated Canary app render
    runs-on: ubuntu-latest
    steps:
      - name: npm ci
        working-directory: web/app
        run: npm ci
      - name: install Chromium
        working-directory: web/app
        run: npx playwright install --with-deps chromium
      - name: make app-render-check
        run: make app-render-check
  cross-compile:
    name: cross-compile release matrix
    runs-on: ubuntu-latest
EOF
	cat >"$fixture/.github/workflows/pages-check.yml" <<'EOF'
name: pages check

on:
  pull_request:
  push:
    branches:
      - main

jobs:
  links:
    name: local page targets
EOF
	cat >"$fixture/.github/workflows/registry-publish.yml" <<'EOF'
name: registry-publish

on:
  release:
    types: [published]
  workflow_dispatch:
    inputs:
      tag:
        description: "Release tag to publish"
        required: true

permissions:
  actions: read
  contents: read
  id-token: write

env:
  MCP_PUBLISHER_VERSION: v1.7.9
  # modelcontextprotocol/registry v1.7.9 registry_1.7.9_checksums.txt
  MCP_PUBLISHER_LINUX_AMD64_SHA256: ab128162b0616090b47cf245afe0a23f3ef08936fdce19074f5ba0a4469281ac

jobs:
  publish:
    runs-on: ubuntu-latest
    steps:
      - name: Resolve tag
        id: tag
        env:
          RELEASE_TAG: ${{ github.event.release.tag_name || inputs.tag }}
          RELEASE_EVENT: ${{ github.event_name }}
        run: |
          set -euo pipefail
          tag="$RELEASE_TAG"
          if ! [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then
            echo "unexpected tag '$tag'" >&2
            exit 1
          fi
          case "$RELEASE_EVENT" in
            release) source_mode=tag ;;
            workflow_dispatch) source_mode=controller ;;
            *) echo "unexpected event '$RELEASE_EVENT'" >&2; exit 1 ;;
          esac
          case "$tag" in
            v2.*) release_branch=release/2.x ;;
            *) release_branch=main ;;
          esac
          printf 'tag=%s\n' "$tag" >> "$GITHUB_OUTPUT"
          printf 'source_mode=%s\n' "$source_mode" >> "$GITHUB_OUTPUT"
          printf 'release_branch=%s\n' "$release_branch" >> "$GITHUB_OUTPUT"

      - name: Checkout release source
        uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262 # v4
        with:
          repository: osauer/canary
          ref: ${{ github.workflow_sha }}
          fetch-depth: 0
          fetch-tags: true

      - name: Set up Go
        uses: actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff # v5
        with:
          go-version-file: go.mod

      - name: Hydrate and verify exact release asset set
        env:
          GH_TOKEN: ${{ github.token }}
          RELEASE_TAG: ${{ steps.tag.outputs.tag }}
          RELEASE_SOURCE_MODE: ${{ steps.tag.outputs.source_mode }}
        run: |
          set -euo pipefail
          make release-github-assets RELEASE_VERSION="$RELEASE_TAG" RELEASE_SOURCE_MODE="$RELEASE_SOURCE_MODE"

      - name: Verify exact release authority
        env:
          GH_TOKEN: ${{ github.token }}
          RELEASE_TAG: ${{ steps.tag.outputs.tag }}
          RELEASE_SOURCE_MODE: ${{ steps.tag.outputs.source_mode }}
          RELEASE_BRANCH: ${{ steps.tag.outputs.release_branch }}
        run: |
          set -euo pipefail
          tag="$RELEASE_TAG"
          release_sha="$(git rev-parse "refs/tags/${tag}^{commit}")"
          contract="$(mktemp "${RUNNER_TEMP}/canary-release-ci-contract.XXXXXX")"
          trap 'rm -f "$contract"' EXIT HUP INT TERM
          ./scripts/check-release-source.sh --mode "$RELEASE_SOURCE_MODE" "$tag"
          ./scripts/check-release-origin.sh
          ./scripts/check-release-ci-contract.sh
          python3 ./scripts/materialize-release-ci-contract.py "$tag" "$contract"
          GOFLAGS= go run ./scripts/release-ci-wait \
            -contract "$contract" -historical \
            -sha "$release_sha" -branch "$RELEASE_BRANCH" -event push \
            -poll 15s -timeout 30m
          ./scripts/check-release-tag.sh "$tag"
          ./scripts/check-release-tag.sh --plugin "$tag"
          ./scripts/check-github-release.sh "$tag" dist

      - name: Install mcp-publisher
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          set -euo pipefail
          archive="${RUNNER_TEMP}/mcp-publisher_linux_amd64.tar.gz"
          mkdir -p bin
          gh release download "$MCP_PUBLISHER_VERSION" \
            --repo github.com/modelcontextprotocol/registry \
            --pattern 'mcp-publisher_linux_amd64.tar.gz' \
            --dir "$RUNNER_TEMP"
          printf '%s  %s\n' "$MCP_PUBLISHER_LINUX_AMD64_SHA256" "$archive" \
            | sha256sum --check --strict -
          tar -xzf "$archive" -C bin mcp-publisher
          bin/mcp-publisher --version

      - name: Generate and validate dist/server.json
        env:
          RELEASE_TAG: ${{ steps.tag.outputs.tag }}
        run: make release-registry-server RELEASE_VERSION="$RELEASE_TAG"

      - name: Publish via OIDC
        run: |
          set -euo pipefail
          bin/mcp-publisher login github-oidc
          MCP_REGISTRY_AUTO_LOGIN=0 \
            ./scripts/registry-publish-with-login.sh \
            bin/mcp-publisher dist/server.json
EOF
	cat >"$fixture/.github/workflows/cla.yml" <<'EOF'
name: cla

on:
  issue_comment:
    types: [created]
  pull_request_target:
    types: [opened, synchronize]

jobs:
  cla:
    runs-on: ubuntu-latest
EOF
}

reset_fixture() {
	rm -rf "$fixture"
	mkdir -p "$fixture"
	write_makefile
	write_manifest
	write_workflows
}

expect_pass() {
	label=$1
	if ! "$checker" "$fixture" >"$test_root/output" 2>&1; then
		echo "check-release-ci-contract test: $label unexpectedly failed" >&2
		sed 's/^/  /' "$test_root/output" >&2
		exit 1
	fi
}

expect_fail() {
	label=$1
	if "$checker" "$fixture" >"$test_root/output" 2>&1; then
		echo "check-release-ci-contract test: $label unexpectedly passed" >&2
		exit 1
	fi
}

reset_fixture
expect_pass "canonical manifest contract"

# The only pre-contract recovery release is pinned by its exact commit and
# exact workflow/job manifest. Deleting or editing that production authority
# must fail the normal repository gate without needing historical tags locally.
for legacy_mutation in missing wrong_sha wrong_job; do
	reset_fixture
	case "$legacy_mutation" in
	missing)
		rm "$fixture/scripts/release-ci-legacy-contracts.json"
		;;
	wrong_sha)
		sed 's/3b548f6d63286448ac132ca4ade66484952612f5/3b548f6d63286448ac132ca4ade66484952612f4/' \
			"$fixture/scripts/release-ci-legacy-contracts.json" \
			>"$fixture/scripts/release-ci-legacy-contracts.json.new"
		mv "$fixture/scripts/release-ci-legacy-contracts.json.new" \
			"$fixture/scripts/release-ci-legacy-contracts.json"
		;;
	wrong_job)
		sed 's/local page targets/local page target/' \
			"$fixture/scripts/release-ci-legacy-contracts.json" \
			>"$fixture/scripts/release-ci-legacy-contracts.json.new"
		mv "$fixture/scripts/release-ci-legacy-contracts.json.new" \
			"$fixture/scripts/release-ci-legacy-contracts.json"
		;;
	esac
	expect_fail "$legacy_mutation v2.5.4 legacy contract"
done

# The release/manual Registry workflow is an independent publication sink.
# Its canonical checkout, token/repository pins, exact-SHA authority chain,
# typed wrapper, and fail-closed order are all static release contracts.
for registry_mutation in \
	missing_actions_read \
	mutable_checkout_action \
	mutable_setup_action \
	fork_checkout \
	release_tag_checkout \
	event_sha_controller \
	ambient_github_token \
	unqualified_publisher_repo \
	wrong_publisher_digest \
	missing_publisher_digest_check \
	missing_tag_contract_materialization \
	current_contract_historical \
	missing_historical \
	wrong_historical_event \
	wrong_v2_release_branch \
	hardcoded_registry_branch \
	auto_login_enabled \
	early_authority_exit \
	direct_registry_query \
	direct_run_expression \
	publish_env_injection \
	source_without_mode \
	source_pinned_mode \
	assets_without_mode \
	swapped_event_modes \
	open_event_mode \
	head_release_sha_resolution \
	head_historical_waiter
do
	reset_fixture
	python3 - "$fixture/.github/workflows/registry-publish.yml" \
		"$registry_mutation" <<'PY'
import sys

path, mutation = sys.argv[1:]
replacements = {
    "missing_actions_read": ("  actions: read\n", ""),
    "mutable_checkout_action": (
        "actions/checkout@11d5960a326750d5838078e36cf38b85af677262",
        "actions/checkout@v4",
    ),
    "mutable_setup_action": (
        "actions/setup-go@40f1582b2485089dde7abd97c1529aa768e1baff",
        "actions/setup-go@v5",
    ),
    "fork_checkout": (
        "          repository: osauer/canary",
        "          repository: attacker/canary",
    ),
    "release_tag_checkout": (
        "          ref: ${{ github.workflow_sha }}",
        "          ref: ${{ steps.tag.outputs.tag }}",
    ),
    "event_sha_controller": (
        "          ref: ${{ github.workflow_sha }}",
        "          ref: ${{ github.sha }}",
    ),
    "ambient_github_token": (
        "          GH_TOKEN: ${{ github.token }}",
        "          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}",
    ),
    "unqualified_publisher_repo": (
        "--repo github.com/modelcontextprotocol/registry",
        "--repo modelcontextprotocol/registry",
    ),
    "wrong_publisher_digest": (
        "ab128162b0616090b47cf245afe0a23f3ef08936fdce19074f5ba0a4469281ac",
        "0" * 64,
    ),
    "missing_publisher_digest_check": (
        "            | sha256sum --check --strict -",
        "            | true",
    ),
    "missing_tag_contract_materialization": (
        '          python3 ./scripts/materialize-release-ci-contract.py "$tag" "$contract"\n',
        "",
    ),
    "current_contract_historical": (
        '            -contract "$contract" -historical',
        "            -contract scripts/release-ci-contract.json -historical",
    ),
    "missing_historical": (" -historical \\\n", " \\\n"),
    "wrong_historical_event": ('-branch "$RELEASE_BRANCH" -event push', '-branch "$RELEASE_BRANCH" -event release'),
    "wrong_v2_release_branch": ("            v2.*) release_branch=release/2.x ;;", "            v2.*) release_branch=main ;;"),
    "hardcoded_registry_branch": ('-branch "$RELEASE_BRANCH" -event push', '-branch main -event push'),
    "auto_login_enabled": ("MCP_REGISTRY_AUTO_LOGIN=0", "MCP_REGISTRY_AUTO_LOGIN=1"),
    "early_authority_exit": (
        '          release_sha="$(git rev-parse "refs/tags/${tag}^{commit}")"\n',
        '          release_sha="$(git rev-parse "refs/tags/${tag}^{commit}")"\n'
        '          exit 0\n',
    ),
    "direct_registry_query": (
        "          bin/mcp-publisher login github-oidc\n",
        "          curl -fsS https://registry.modelcontextprotocol.io/v0/servers\n"
        "          bin/mcp-publisher login github-oidc\n",
    ),
    "direct_run_expression": (
        'run: make release-registry-server RELEASE_VERSION="$RELEASE_TAG"',
        "run: make release-registry-server "
        "RELEASE_VERSION=${{ steps.tag.outputs.tag }}",
    ),
    "publish_env_injection": (
        "      - name: Publish via OIDC\n"
        "        run: |\n",
        "      - name: Publish via OIDC\n"
        "        env:\n"
        "          BASH_ENV: ./untrusted.sh\n"
        "        run: |\n",
    ),
    "source_without_mode": (
        './scripts/check-release-source.sh --mode "$RELEASE_SOURCE_MODE" "$tag"',
        './scripts/check-release-source.sh "$tag"',
    ),
    "source_pinned_mode": (
        './scripts/check-release-source.sh --mode "$RELEASE_SOURCE_MODE" "$tag"',
        './scripts/check-release-source.sh --mode tag "$tag"',
    ),
    "assets_without_mode": (
        'make release-github-assets RELEASE_VERSION="$RELEASE_TAG" '
        'RELEASE_SOURCE_MODE="$RELEASE_SOURCE_MODE"',
        'make release-github-assets RELEASE_VERSION="$RELEASE_TAG"',
    ),
    "swapped_event_modes": (
        "            release) source_mode=tag ;;\n"
        "            workflow_dispatch) source_mode=controller ;;\n",
        "            release) source_mode=controller ;;\n"
        "            workflow_dispatch) source_mode=tag ;;\n",
    ),
    "open_event_mode": (
        '            *) echo "unexpected event \'$RELEASE_EVENT\'" >&2; exit 1 ;;\n',
        "",
    ),
    "head_release_sha_resolution": (
        'release_sha="$(git rev-parse "refs/tags/${tag}^{commit}")"',
        'release_sha="$(git rev-parse HEAD)"',
    ),
    "head_historical_waiter": (
        '-sha "$release_sha" -branch "$RELEASE_BRANCH" -event push',
        '-sha "$(git rev-parse HEAD)" -branch "$RELEASE_BRANCH" -event push',
    ),
}
old, new = replacements[mutation]
with open(path, encoding="utf-8") as source:
    text = source.read()
if old not in text:
    raise SystemExit(f"fixture mutation target missing for {mutation}")
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(old, new, 1))
PY
	expect_fail "$registry_mutation registry authority"
done

reset_fixture
python3 - "$fixture/.github/workflows/registry-publish.yml" <<'PY'
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    text = source.read()
old = """          ./scripts/check-release-origin.sh
          ./scripts/check-release-ci-contract.sh
"""
new = """          ./scripts/check-release-ci-contract.sh
          ./scripts/check-release-origin.sh
"""
if old not in text:
    raise SystemExit("fixture authority-order target missing")
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(old, new, 1))
PY
expect_fail "reordered registry authority"

reset_fixture
python3 - "$fixture/.github/workflows/registry-publish.yml" <<'PY'
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    text = source.read()
needle = "          bin/mcp-publisher login github-oidc\n"
if needle not in text:
    raise SystemExit("fixture raw-publish target missing")
raw_publish = "          bin/mcp-publisher publish dist/server.json\n"
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(needle, needle + raw_publish, 1))
PY
expect_fail "raw mcp-publisher publication"

reset_fixture
python3 - "$fixture/.github/workflows/registry-publish.yml" <<'PY'
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    text = source.read()
asset = text.index("      - name: Hydrate and verify exact release asset set\n")
authority = text.index("      - name: Verify exact release authority\n")
install = text.index("      - name: Install mcp-publisher\n")
asset_block = text[asset:authority]
authority_block = text[authority:install]
with open(path, "w", encoding="utf-8") as destination:
    destination.write(
        text[:asset] + authority_block + asset_block + text[install:]
    )
PY
expect_fail "registry proof-step reorder"

# A newly repo-owned push-to-main workflow would otherwise run outside the
# release waiter's single exact-SHA snapshot.
reset_fixture
cat >"$fixture/.github/workflows/rogue.yml" <<'EOF'
name: rogue

on:
  push:
    branches: [main]

jobs:
  rogue:
    runs-on: ubuntu-latest
EOF
expect_fail "third push-to-main workflow"

# Updating the manifest alongside an unauthorized workflow does not expand the
# hard-coded release authority silently.
reset_fixture
cat >"$fixture/.github/workflows/rogue.yml" <<'EOF'
name: rogue

on:
  push:
    branches: [main]

jobs:
  rogue:
    runs-on: ubuntu-latest
EOF
python3 - "$fixture/scripts/release-ci-contract.json" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    contract = json.load(source)
contract["workflows"].append(
    {"file": "rogue.yml", "name": "rogue", "jobs": ["rogue"]}
)
with open(path, "w", encoding="utf-8") as destination:
    json.dump(contract, destination)
PY
expect_fail "third workflow added to manifest"

# A broad push trigger also includes main and must not escape the manifest.
reset_fixture
cat >"$fixture/.github/workflows/rogue.yaml" <<'EOF'
name: rogue

on:
  push:

jobs:
  rogue:
    runs-on: ubuntu-latest
EOF
expect_fail "unfiltered third push workflow"

# Tag-only push workflows do not run for a branch push and are not part of the
# release candidate's push-to-main inventory.
reset_fixture
cat >"$fixture/.github/workflows/tag-only.yml" <<'EOF'
name: tag-only

on:
  push:
    tags: [v*]

jobs:
  tag:
    runs-on: ubuntu-latest
EOF
expect_pass "tag-only push workflow"

# The manifest is the only workflow/name/job authority and must stay exact.
reset_fixture
sed '/"make test (ubuntu-latest)",/d' \
	"$fixture/scripts/release-ci-contract.json" \
	>"$fixture/scripts/release-ci-contract.json.new"
mv "$fixture/scripts/release-ci-contract.json.new" \
	"$fixture/scripts/release-ci-contract.json"
expect_fail "missing expected manifest job"

# Workflow display names and GitHub-rendered job names (including matrix
# expansion) must drift with the manifest during ordinary make check, not late
# in a release after live gates.
reset_fixture
sed 's/^name: ci$/name: renamed ci/' "$fixture/.github/workflows/ci.yml" \
	>"$fixture/.github/workflows/ci.yml.new"
mv "$fixture/.github/workflows/ci.yml.new" \
	"$fixture/.github/workflows/ci.yml"
expect_fail "renamed workflow display name"

reset_fixture
sed 's/name: cross-compile release matrix/name: renamed cross-compile/' \
	"$fixture/.github/workflows/ci.yml" \
	>"$fixture/.github/workflows/ci.yml.new"
mv "$fixture/.github/workflows/ci.yml.new" \
	"$fixture/.github/workflows/ci.yml"
expect_fail "renamed rendered job"

reset_fixture
sed 's/os: \[ubuntu-latest\]/os: [ubuntu-latest, macos-latest]/' \
	"$fixture/.github/workflows/ci.yml" \
	>"$fixture/.github/workflows/ci.yml.new"
mv "$fixture/.github/workflows/ci.yml.new" \
	"$fixture/.github/workflows/ci.yml"
expect_fail "changed job matrix expansion"

reset_fixture
sed 's/run: make check CHECK_DEPS=parity-check/run: make commit-check/' \
	"$fixture/.github/workflows/ci.yml" \
	>"$fixture/.github/workflows/ci.yml.new"
mv "$fixture/.github/workflows/ci.yml.new" \
	"$fixture/.github/workflows/ci.yml"
expect_fail "partial gate substituted for exact CI check"

reset_fixture
sed '/run: make check CHECK_DEPS=parity-check/a\
        continue-on-error: true' \
	"$fixture/.github/workflows/ci.yml" \
	>"$fixture/.github/workflows/ci.yml.new"
mv "$fixture/.github/workflows/ci.yml.new" \
	"$fixture/.github/workflows/ci.yml"
expect_fail "CI check step made non-binding"

for test_mutation in missing_pkg partial_support best_effort_daemon extra_run; do
	reset_fixture
	python3 - "$fixture/.github/workflows/ci.yml" "$test_mutation" <<'PY'
import sys

path, mutation = sys.argv[1:]
replacements = {
    "missing_pkg": (
        "      - name: make test-pkg\n"
        "        run: make test-pkg\n",
        "",
    ),
    "partial_support": (
        "        run: make test-support",
        "        run: go test ./scripts/...",
    ),
    "best_effort_daemon": (
        "      - name: make test-daemon-default (shards + hermetic integration)\n"
        "        run: make test-daemon-default",
        "      - name: make test-daemon-default (shards + hermetic integration)\n"
        "        continue-on-error: true\n"
        "        run: make test-daemon-default",
    ),
    "extra_run": (
        "      - name: make test-pkg\n",
        "      - name: partial shortcut\n"
        "        run: make commit-check\n"
        "      - name: make test-pkg\n",
    ),
}
old, new = replacements[mutation]
with open(path, encoding="utf-8") as source:
    text = source.read()
if old not in text:
    raise SystemExit(f"fixture mutation target missing for {mutation}")
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(old, new, 1))
PY
	expect_fail "$test_mutation exact CI test matrix"
done

reset_fixture
cat >>"$fixture/.github/workflows/ci.yml" <<'EOF'
  unexpected:
    name: unexpected job
    runs-on: ubuntu-latest
EOF
expect_fail "added rendered job"

reset_fixture
sed 's#"repository": "osauer/canary"#"repository": "fork/canary"#' \
	"$fixture/scripts/release-ci-contract.json" \
	>"$fixture/scripts/release-ci-contract.json.new"
mv "$fixture/scripts/release-ci-contract.json.new" \
	"$fixture/scripts/release-ci-contract.json"
expect_fail "wrong manifest repository"

reset_fixture
cat >"$fixture/scripts/release-ci-contract.json" <<'EOF'
{
  "repository": "osauer/canary",
  "repository": "osauer/canary",
  "workflows": []
}
EOF
expect_fail "duplicate manifest key"

reset_fixture
sed 's/"file": "pages-check.yml"/"file": "ci.yml"/' \
	"$fixture/scripts/release-ci-contract.json" \
	>"$fixture/scripts/release-ci-contract.json.new"
mv "$fixture/scripts/release-ci-contract.json.new" \
	"$fixture/scripts/release-ci-contract.json"
expect_fail "duplicate manifest workflow"

# One process owns the snapshot. Missing, duplicated, or legacy per-workflow
# invocation shapes must fail statically.
reset_fixture
cat >"$fixture/Makefile" <<'EOF'
release-ci-wait:
	@true
_release-ci-wait-historical:
	@GOFLAGS= go run ./scripts/release-ci-wait \
		-contract scripts/release-ci-contract.json -historical \
		-sha "$$(git rev-parse HEAD)" -branch "$(MAIN_BRANCH)" -event push \
		-poll "$(RELEASE_CI_POLL)" -timeout "$(RELEASE_CI_TIMEOUT)"
EOF
expect_fail "missing Go waiter invocation"

reset_fixture
cat >"$fixture/Makefile" <<'EOF'
release-ci-wait:
	GOFLAGS= go run ./scripts/release-ci-wait -contract scripts/release-ci-contract.json
	GOFLAGS= go run ./scripts/release-ci-wait -contract scripts/release-ci-contract.json
_release-ci-wait-historical:
	@GOFLAGS= go run ./scripts/release-ci-wait \
		-contract scripts/release-ci-contract.json -historical \
		-sha "$$(git rev-parse HEAD)" -branch "$(MAIN_BRANCH)" -event push \
		-poll "$(RELEASE_CI_POLL)" -timeout "$(RELEASE_CI_TIMEOUT)"
EOF
expect_fail "duplicated Go waiter invocation"

reset_fixture
sed 's#scripts/release-ci-contract.json#scripts/other-contract.json#' \
	"$fixture/Makefile" >"$fixture/Makefile.new"
mv "$fixture/Makefile.new" "$fixture/Makefile"
expect_fail "wrong contract path"

reset_fixture
sed 's#-contract scripts/release-ci-contract.json#-workflow ci.yml -job check#' \
	"$fixture/Makefile" >"$fixture/Makefile.new"
mv "$fixture/Makefile.new" "$fixture/Makefile"
expect_fail "legacy per-workflow invocation"

# Historical resume must use the same manifest while explicitly bypassing only
# the mutable current-workflow catalog.
reset_fixture
sed 's/ -historical//' "$fixture/Makefile" >"$fixture/Makefile.new"
mv "$fixture/Makefile.new" "$fixture/Makefile"
expect_fail "historical helper without historical mode"

# Historical resume must resolve the immutable release-tag commit, never the
# recovery controller's HEAD.
reset_fixture
python3 - "$fixture/Makefile" <<'PY'
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    text = source.read()
old = '-sha "$$release_sha" -branch "$(MAIN_BRANCH)"'
new = '-sha "$$(git rev-parse HEAD)" -branch "$(MAIN_BRANCH)"'
if old not in text:
    raise SystemExit("fixture historical-SHA target missing")
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(old, new, 1))
PY
expect_fail "historical helper using controller HEAD"

# Exact candidate identity and wait bounds are part of the authority, not
# caller-selectable hints.
for mutation in wrong_sha wrong_branch wrong_event missing_timeout; do
	reset_fixture
	python3 - "$fixture/Makefile" "$mutation" <<'PY'
import sys

path, mutation = sys.argv[1:]
replacements = {
    "wrong_sha": ('"$$(git rev-parse HEAD)"', "deadbeef"),
    "wrong_branch": ('"$(MAIN_BRANCH)"', "main"),
    "wrong_event": ("-event push", "-event workflow_dispatch"),
    "missing_timeout": (' -timeout "$(RELEASE_CI_TIMEOUT)"', ""),
}
old, new = replacements[mutation]
with open(path, encoding="utf-8") as source:
    text = source.read()
if old not in text:
    raise SystemExit(f"fixture mutation target missing for {mutation}")
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(old, new, 1))
PY
	expect_fail "$mutation waiter authority"
done

# Neither shell masking nor Make's ignore-errors prefix may turn the binding
# waiter into best-effort evidence.
reset_fixture
python3 - "$fixture/Makefile" <<'PY'
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    text = source.read()
needle = "-event push"
if needle not in text:
    raise SystemExit("fixture mutation target missing")
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(needle, needle + " || true", 1))
PY
expect_fail "shell-masked waiter invocation"

reset_fixture
python3 - "$fixture/Makefile" <<'PY'
import sys

path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    text = source.read()
needle = "\t@GOFLAGS= go run"
if needle not in text:
    raise SystemExit("fixture mutation target missing")
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(needle, "\t-GOFLAGS= go run", 1))
PY
expect_fail "Make ignore-errors waiter invocation"

# Ambient or persistent GOFLAGS may not replace the waiter process via
# -exec. The command shape pins an explicit empty value.
for goflags_mutation in missing_go_flags executable_override; do
	reset_fixture
	python3 - "$fixture/Makefile" "$goflags_mutation" <<'PY'
import sys

path, mutation = sys.argv[1:]
replacements = {
    "missing_go_flags": ("@GOFLAGS= go run", "@go run"),
    "executable_override": (
        "@GOFLAGS= go run",
        "@GOFLAGS=-exec=true go run",
    ),
}
old, new = replacements[mutation]
with open(path, encoding="utf-8") as source:
    text = source.read()
if old not in text:
    raise SystemExit(f"fixture mutation target missing for {mutation}")
with open(path, "w", encoding="utf-8") as destination:
    destination.write(text.replace(old, new, 1))
PY
	expect_fail "$goflags_mutation waiter environment"
done

# The checker must never guess through malformed or unsupported trigger YAML.
reset_fixture
sed 's/branches: \[main\]/branches: [main/' \
	"$fixture/.github/workflows/ci.yml" \
	>"$fixture/.github/workflows/ci.yml.new"
mv "$fixture/.github/workflows/ci.yml.new" \
	"$fixture/.github/workflows/ci.yml"
expect_fail "malformed workflow YAML"

reset_fixture
cat >"$fixture/.github/workflows/ci.yml" <<'EOF'
name: ci

on: [push]

jobs:
  check:
    runs-on: ubuntu-latest
EOF
expect_fail "unsupported inline trigger syntax"

# Path filters make trigger applicability candidate-dependent; the static
# release contract rejects them instead of waiting for a run that may not exist.
reset_fixture
sed '/    branches: \[main\]/a\
    paths: ["**"]' "$fixture/.github/workflows/ci.yml" \
	>"$fixture/.github/workflows/ci.yml.new"
mv "$fixture/.github/workflows/ci.yml.new" \
	"$fixture/.github/workflows/ci.yml"
expect_fail "path-filtered push workflow"

cat >"$test_root/goflags.mk" <<'EOF'
probe:
	@GOFLAGS= go run ./scripts/release-ci-wait -definitely-invalid-flag
EOF
if GOFLAGS='-exec=true' make -s -C "$repo_root" -f "$test_root/goflags.mk" probe \
	>"$test_root/output" 2>&1; then
	echo "check-release-ci-contract test: ambient GOFLAGS bypassed explicit empty pin" >&2
	exit 1
fi

echo "check-release-ci-contract test: OK"
