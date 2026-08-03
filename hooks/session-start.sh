#!/usr/bin/env bash
# SessionStart hook for the Canary Claude Code plugin.
#
# Two warnings, both written to stderr, both non-fatal:
#   1) The canonical `canary` binary is not on PATH. A retired `ibkr` entry is
#      detected for diagnosis but never invoked.
#   2) Binary and plugin disagree on major.minor (skill may reference
#      subcommands the binary doesn't expose, or vice versa).
#
# Never exit nonzero — a SessionStart hook must not block CC startup.

set +e

if ! command -v canary >/dev/null 2>&1; then
	if command -v ibkr >/dev/null 2>&1; then
		printf '\n[Canary plugin] A retired `ibkr` executable is on PATH, but hooks will not invoke it. Install the canonical `canary` executable, remove the retired entry, then restart your Claude Code session.\n\n' >&2
	else
		printf '\n[Canary plugin] The `canary` binary is not on your PATH.\nInstall Canary with one of:\n  curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh\n  make install   (from a checkout of github.com/osauer/canary)\nThen restart your Claude Code session.\n\n' >&2
	fi
	exit 0
fi
cli_bin="canary"

# The version-skew check needs jq. Fail closed (silently skip the check)
# if jq is missing — the PreToolUse hook is the one that hard-blocks on it.
command -v jq >/dev/null 2>&1 || exit 0

bin_raw=$("$cli_bin" version --json 2>/dev/null | jq -r '.version // empty')
[ -z "$bin_raw" ] && exit 0

# `canary version --json` reports `git describe`, e.g.
# `v0.27.9-4-g4317cfd[-dirty]`.
# Strip the leading `v` and the development suffix to land on a clean semver.
bin_semver=$(printf '%s' "$bin_raw" | sed -E 's/^v//; s/-[0-9]+-g[a-f0-9]+(-dirty)?$//')

# Plugin version lives at $CLAUDE_PLUGIN_ROOT/.claude-plugin/plugin.json. CC sets
# CLAUDE_PLUGIN_ROOT on the hook process automatically when the hook fires from
# an installed plugin (per https://docs.claude.com/en/docs/claude-code/hooks).
# Fall back to a hardcoded constant for development / unexpected unset cases;
# `make hook-version-check` (chained from plugin-check, so part of every
# `make check` and release) fails when the constant's major.minor drifts
# from .claude-plugin/plugin.json.
plugin_semver=""
if [ -n "${CLAUDE_PLUGIN_ROOT:-}" ] && [ -f "$CLAUDE_PLUGIN_ROOT/.claude-plugin/plugin.json" ]; then
	plugin_semver=$(jq -r '.version // empty' "$CLAUDE_PLUGIN_ROOT/.claude-plugin/plugin.json" 2>/dev/null)
fi
[ -z "$plugin_semver" ] && plugin_semver="2.7.0"

bin_mm=$(printf '%s' "$bin_semver" | awk -F. 'NF>=2 {print $1 "." $2}')
plg_mm=$(printf '%s' "$plugin_semver" | awk -F. 'NF>=2 {print $1 "." $2}')

# Bail if either side didn't yield a parseable major.minor (e.g. binary built
# without a tag in scope and reports `dev`). Better to stay silent than warn
# on a malformed comparison.
{ [ -z "$bin_mm" ] || [ -z "$plg_mm" ]; } && exit 0
[ "$bin_mm" = "$plg_mm" ] && exit 0

older=$(printf '%s\n%s\n' "$bin_mm" "$plg_mm" | sort -V | head -n1)
if [ "$older" = "$bin_mm" ]; then
	printf '\n[Canary plugin] Version skew detected.\n  binary: %s (major.minor %s)\n  plugin: %s (major.minor %s)\nThe binary is older than the plugin. The skill may reference subcommands the\nbinary does not expose yet (e.g. `canary regime`) and you would see\n`unknown subcommand` errors.\nUpdate the binary:\n  curl -fsSL https://raw.githubusercontent.com/osauer/canary/main/install.sh | sh\nSee the "Claude Code" section in README.md for both release channels.\n\n' \
		"$bin_semver" "$bin_mm" "$plugin_semver" "$plg_mm" >&2
else
	printf '\n[Canary plugin] Version skew detected.\n  binary: %s (major.minor %s)\n  plugin: %s (major.minor %s)\nThe plugin is older than the binary. The skill may not describe subcommands\nthe binary now exposes.\nUpdate the plugin:\n  claude plugin update canary@canary\nSee the "Claude Code" section in README.md for both release channels.\n\n' \
		"$bin_semver" "$bin_mm" "$plugin_semver" "$plg_mm" >&2
fi

exit 0
