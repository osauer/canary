package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRunKeepsRegistryIdentityAndUsesCanonicalCanaryBundle(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	if err := os.WriteFile("server.json", []byte(`{
  "name": "io.github.attacker/controller",
  "description": "Mutable controller metadata must not be used."
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	template := `{
  "name": "io.github.osauer/canary",
  "title": "Canary MCP",
  "description": "Canary tools for Interactive Brokers.",
  "version": "0.0.0",
  "repository": {
    "url": "https://github.com/osauer/canary",
    "source": "github",
    "id": "1234071553"
  }
}`
	templatePath := filepath.Join(root, "tagged-server.json")
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "canary-v2.4.0.mcpb")
	payload := []byte("canonical bundle")
	if err := os.WriteFile(bundle, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "dist", "server.json")
	if err := run([]string{"v2.4.0", templatePath, bundle, output}); err != nil {
		t.Fatalf("run: %v", err)
	}

	var got registryServer
	raw, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "io.github.osauer/canary" {
		t.Fatalf("registry machine name = %q", got.Name)
	}
	if got.Title != "Canary MCP" || got.Repository.URL != "https://github.com/osauer/canary" {
		t.Fatalf("human/repository identity = %q / %q", got.Title, got.Repository.URL)
	}
	if got.Version != "2.4.0" || len(got.Packages) != 1 {
		t.Fatalf("version/packages = %q / %d", got.Version, len(got.Packages))
	}
	wantURL := "https://github.com/osauer/canary/releases/download/v2.4.0/canary-v2.4.0.mcpb"
	if got.Packages[0].Identifier != wantURL {
		t.Fatalf("package identifier = %q, want %q", got.Packages[0].Identifier, wantURL)
	}
	sum := sha256.Sum256(payload)
	if got.Packages[0].FileSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("package digest = %q", got.Packages[0].FileSHA256)
	}
}

func TestRunRejectsSymlinkTemplate(t *testing.T) {
	root := t.TempDir()
	templatePath := filepath.Join(root, "server.json")
	realTemplatePath := filepath.Join(root, "real-server.json")
	if err := os.WriteFile(realTemplatePath, []byte(`{
  "name": "io.github.osauer/canary",
  "description": "Canary tools for Interactive Brokers."
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTemplatePath, templatePath); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(root, "canary-v2.4.0.mcpb")
	if err := os.WriteFile(bundle, []byte("bundle"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := run([]string{"v2.4.0", templatePath, bundle, filepath.Join(root, "dist", "server.json")})
	if err == nil {
		t.Fatal("run accepted a symlink release server template")
	}
}
