package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeFlexConfigPreservesUnrelatedContent(t *testing.T) {
	t.Parallel()

	in := []byte("# operator note\n[gateway]\nhost = \"127.0.0.1\"\n\n[flex]\n# keep this comment\nenabled = false\nquery_id = \"old\"\ntoken_path = \"old-token\"\n\n[daemon]\nlog_level = \"warn\"\n")
	out, err := MergeFlexConfig(in, "123456", "/private/token")
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{"# operator note", "host = \"127.0.0.1\"", "# keep this comment", "enabled = true", "query_id = \"123456\"", "token_path = \"/private/token\"", "log_level = \"warn\""} {
		if !strings.Contains(text, want) {
			t.Fatalf("merged config lacks %q:\n%s", want, text)
		}
	}
}

func TestUpdateFlexConfigAtomicCreatesPrivateBackup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	original := []byte("[flex]\nenabled = true\nquery_id = \"old\"\ntoken_path = \"old-token\"\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	backup, err := UpdateFlexConfigAtomic(path, "654321", "/private/new-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, backup} {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode = %04o", filepath.Base(file), got)
		}
	}
	backedUp, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if string(backedUp) != string(original) {
		t.Fatal("backup does not match the prior config")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Flex.Enabled || cfg.Flex.QueryID != "654321" || cfg.Flex.TokenPath != "/private/new-token" {
		t.Fatalf("updated flex config = %+v", cfg.Flex)
	}
}
