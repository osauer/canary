package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParityCanonicalMCPIdentityAcrossSurfaces(t *testing.T) {
	for _, tool := range Tools {
		if !strings.HasPrefix(tool.Name, "canary_") {
			t.Errorf("runtime tool %q is outside the canary_* namespace", tool.Name)
		}
		if !strings.HasPrefix(tool.Title, "Canary ") {
			t.Errorf("runtime tool %q title %q is not Canary-branded", tool.Name, tool.Title)
		}
	}
	for _, resource := range ResourceTemplates {
		if !strings.HasPrefix(resource.URITemplate, "canary://") {
			t.Errorf("runtime resource %q is outside the canary:// namespace", resource.URITemplate)
		}
	}

	root := filepath.Clean(filepath.Join("..", ".."))
	assertJSONField(t, filepath.Join(root, "server.json"), "name", "io.github.osauer/canary")
	assertJSONField(t, filepath.Join(root, "docs", "mcp-server.json"), "name", "canary")
	assertJSONField(t, filepath.Join(root, ".claude-plugin", "plugin.json"), "name", "canary")
	assertJSONField(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), "name", "canary")

	for _, path := range []string{
		"server.json",
		"docs/mcp-server.json",
		"docs/.well-known/mcp/server-card.json",
		".claude-plugin/plugin.json",
		".claude-plugin/marketplace.json",
		"claude-plugin/.mcp.json",
		".codex/config.toml",
		".claude/settings.json",
		"scripts/build-mcpb.sh",
		"scripts/canary-mcp.sh",
	} {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, forbidden := range []string{
			"ibkr_",
			"ibkr://",
			"io.github.osauer/ibkr",
			"github.com/osauer/ibkr",
			"osauer.dev/ibkr",
			"ibkr.mcpb",
			"server/ibkr",
			"IBKR_BIN",
			"scripts/ibkr-mcp.sh",
		} {
			if strings.Contains(string(body), forbidden) {
				t.Errorf("%s retains legacy MCP identity %q", path, forbidden)
			}
		}
	}
}

func assertJSONField(t *testing.T, path, field, want string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var document map[string]any
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if got, _ := document[field].(string); got != want {
		t.Errorf("%s %s = %q, want %q", path, field, got, want)
	}
}
