package main

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

// TestMergeCanaryMCPEntryFreshConfig covers the case where no existing config
// is present — the resulting JSON should contain only mcpServers.canary.
func TestMergeCanaryMCPEntryFreshConfig(t *testing.T) {
	t.Parallel()
	out, err := mergeCanaryMCPEntry(map[string]any{}, "/usr/local/bin/canary")
	if err != nil {
		t.Fatalf("mergeCanaryMCPEntry: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	servers, ok := got["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing or wrong shape: %#v", got)
	}
	canary, ok := servers["canary"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers.canary missing or wrong shape: %#v", servers)
	}
	if canary["command"] != "/usr/local/bin/canary" {
		t.Errorf("command: got %v, want /usr/local/bin/canary", canary["command"])
	}
	args, ok := canary["args"].([]any)
	if !ok || len(args) != 1 || args[0] != "mcp" {
		t.Errorf("args: got %v, want [\"mcp\"]", canary["args"])
	}
	if !strings.HasSuffix(string(out), "\n") {
		t.Errorf("output should end in newline")
	}
}

// TestMergeCanaryMCPEntryPreservesUnrelatedKeys is the load-bearing invariant
// — a user's other settings (theme, telemetry, unrelated mcpServers) must
// survive the merge unchanged.
func TestMergeCanaryMCPEntryPreservesUnrelatedKeys(t *testing.T) {
	t.Parallel()
	existing := map[string]any{
		"theme":     "dark",
		"telemetry": false,
		"mcpServers": map[string]any{
			"ibkr": map[string]any{"command": "/retired/bin/ibkr"},
			"filesystem": map[string]any{
				"command": "npx",
				"args":    []any{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
			},
		},
	}
	out, err := mergeCanaryMCPEntry(existing, "/opt/bin/canary")
	if err != nil {
		t.Fatalf("mergeCanaryMCPEntry: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if got["theme"] != "dark" {
		t.Errorf("theme lost or corrupted: %v", got["theme"])
	}
	if got["telemetry"] != false {
		t.Errorf("telemetry lost or corrupted: %v", got["telemetry"])
	}
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["filesystem"]; !ok {
		t.Errorf("filesystem mcpServer was clobbered: %#v", servers)
	}
	if _, retired := servers["ibkr"]; retired {
		t.Fatalf("retired mcpServers.ibkr entry survived rewrite: %#v", servers)
	}
	canary := servers["canary"].(map[string]any)
	if canary["command"] != "/opt/bin/canary" {
		t.Errorf("canary.command: got %v", canary["command"])
	}
}

// TestMergeCanaryMCPEntryOverwritesPriorEntry — re-running `canary setup` after
// reinstalling to a new path must update the command, not duplicate.
func TestMergeCanaryMCPEntryOverwritesPriorEntry(t *testing.T) {
	t.Parallel()
	existing := map[string]any{
		"mcpServers": map[string]any{
			"canary": map[string]any{
				"command": "/old/path/canary",
				"args":    []any{"mcp"},
			},
		},
	}
	out, err := mergeCanaryMCPEntry(existing, "/new/path/canary")
	if err != nil {
		t.Fatalf("mergeCanaryMCPEntry: %v", err)
	}

	var got map[string]any
	_ = json.Unmarshal(out, &got)
	canary := got["mcpServers"].(map[string]any)["canary"].(map[string]any)
	if canary["command"] != "/new/path/canary" {
		t.Errorf("expected command to be overwritten to /new/path/canary, got %v", canary["command"])
	}
	if servers := got["mcpServers"].(map[string]any); len(servers) != 1 {
		t.Errorf("expected exactly one mcpServer entry after overwrite, got %d: %#v", len(servers), servers)
	}
}

// TestClaudeDesktopConfigPathPlatforms covers the path-resolution edges. We
// can't easily set runtime.GOOS, so this test asserts the current platform
// returns a sensible path on darwin and an explanatory error elsewhere.
func TestClaudeDesktopConfigPathPlatforms(t *testing.T) {
	t.Parallel()
	path, err := claudeDesktopConfigPath()
	switch runtime.GOOS {
	case "darwin":
		if err != nil {
			t.Fatalf("darwin: unexpected error: %v", err)
		}
		if !strings.HasSuffix(path, "Library/Application Support/Claude/claude_desktop_config.json") {
			t.Errorf("darwin: path suffix wrong: %s", path)
		}
	case "windows":
		// Don't actively ship Windows, but the code branch exists. Skip
		// rather than assert APPDATA shape — CI doesn't run on Windows.
		t.Skip("not exercised by CI")
	default:
		if err == nil {
			t.Errorf("expected error on %s, got path %q", runtime.GOOS, path)
		}
		if !strings.Contains(err.Error(), "not available on") {
			t.Errorf("error message should explain platform unavailability, got: %v", err)
		}
	}
}

func TestAppLaunchAgentPlistRemoteArgs(t *testing.T) {
	t.Parallel()

	plist := string(appLaunchAgentPlist(
		"/usr/local/bin/canary",
		"/tmp/app.log",
		"/tmp/app.err.log",
		appLaunchAgentOptions{Remote: true, RemoteURL: "https://remote.example.test"},
	))
	for _, want := range []string{
		"<string>/usr/local/bin/canary</string>",
		"<string>app</string>",
		"<string>--remote</string>",
		"<string>--remote-url</string>",
		"<string>https://remote.example.test</string>",
		"<string>/tmp/app.log</string>",
		"<string>/tmp/app.err.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}
