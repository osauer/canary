package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func repoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", ".."}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestCommitCheckRemainsIntermediateOnly(t *testing.T) {
	makefile := repoFile(t, "Makefile")
	if !regexp.MustCompile(`(?m)^commit-check:.*\n\tgo run \./scripts/commit-check\s*$`).MatchString(makefile) {
		t.Fatal("Makefile commit-check target no longer invokes the staged-tree helper directly")
	}
	if !regexp.MustCompile(`(?m)^test:.*\n\t\$\(MAKE\) \$\(TEST_MAKEFLAGS\) check test-pkg test-support test-daemon\s*$`).MatchString(makefile) {
		t.Fatal("full test target no longer includes the binding check and complete test families")
	}
	releaseStart := strings.Index(makefile, "\n_release-run:")
	if releaseStart < 0 {
		t.Fatal("could not isolate _release-run recipe")
	}
	if strings.Contains(makefile[releaseStart:], "commit-check") {
		t.Fatal("_release-run must never treat commit-check as release evidence")
	}

	workflow := repoFile(t, ".github", "workflows", "ci.yml")
	if !strings.Contains(workflow, "run: make check CHECK_DEPS=parity-check") {
		t.Fatal("CI check job no longer runs the exact full check command")
	}
	if strings.Contains(workflow, "run: make commit-check") {
		t.Fatal("CI must not substitute the staged checkpoint gate for make check")
	}
}
