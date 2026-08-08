package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryProductionAppLoggingPasses(t *testing.T) {
	violations, err := scanRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("production app log violations: %+v", violations)
	}
}

func TestAuditRejectsRawProductionEmitters(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "cmd/canary/app.go", `package main
import (
    "fmt"
    "os"
)
func runAppServeWithIO() { fmt.Fprintln(os.Stderr, "raw") }
`)
	writeFixture(t, root, "internal/app/example.go", `package app
import "log"
func emit() { log.Printf("raw") }
`)

	violations, err := scanRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	got := violationMessages(violations)
	for _, want := range []string{"runAppServeWithIO writes directly", "imports standard log", "standard log call"} {
		if !strings.Contains(got, want) {
			t.Fatalf("violations %q do not contain %q", got, want)
		}
	}
}

func TestAuditRejectsMultilineSlogMessage(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "cmd/canary/app.go", "package main\nfunc runAppServeWithIO() {}\n")
	writeFixture(t, root, "internal/app/example.go", `package app
import "log/slog"
func emit() { slog.Error("first\nsecond") }
`)

	violations, err := scanRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := violationMessages(violations); !strings.Contains(got, "unlevelled continuation line") {
		t.Fatalf("violations %q do not reject multiline slog", got)
	}
}

func writeFixture(t *testing.T, root, name, contents string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func violationMessages(violations []violation) string {
	var messages []string
	for _, item := range violations {
		messages = append(messages, item.message)
	}
	return strings.Join(messages, " | ")
}
