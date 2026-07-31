package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStagedWorktreeExcludesUnstagedFixes(t *testing.T) {
	repo := t.TempDir()
	mustRun := func(args ...string) {
		t.Helper()
		if output, code, err := runCommand(repo, nil, "git", args...); err != nil {
			t.Fatalf("git %v: code=%d err=%v\n%s", args, code, err, output)
		}
	}
	mustRun("init", "-q")
	mustRun("config", "user.name", "Commit Check Test")
	mustRun("config", "user.email", "commit-check@example.invalid")

	valuePath := filepath.Join(repo, "value.txt")
	if err := os.WriteFile(valuePath, []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun("add", "value.txt")
	mustRun("commit", "-qm", "base")

	if err := os.WriteFile(valuePath, []byte("staged candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustRun("add", "value.txt")
	if err := os.WriteFile(valuePath, []byte("unstaged masking fix\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	head, err := gitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitOutput(repo, "write-tree")
	if err != nil {
		t.Fatal(err)
	}
	candidate, cleanup, err := stagedWorktree(repo, head, tree)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	got, err := os.ReadFile(filepath.Join(candidate, "value.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "staged candidate\n" {
		t.Fatalf("isolated candidate = %q, want staged bytes", got)
	}
}
