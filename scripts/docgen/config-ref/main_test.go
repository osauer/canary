package main

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// docs-check runs this generator from the repository root while
// pages-build deletes and rebuilds dist/ in the same make -j run, and a
// walk into dist/ failed on a page removed under it. Build and scratch
// output holds no hand-written source, so the scan never enters it.
func TestScanSkipsGitignoredBuildOutput(t *testing.T) {
	root := t.TempDir()
	write := func(rel, env string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		src := "package x\n\n// docgen:env " + env + " | Synthetic.\nfunc read() string { return os.Getenv(\"" + env + "\") }\n"
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/app/app.go", "CANARY_KEPT")
	write("test/integration/runner/runner.go", "CANARY_RUNNER")
	for i, rel := range []string{"dist/pages/page.go", "bin/tool.go", "build/backtest/case.go", "docs-html/page.go", "reports/report.go", "tmp/scratch.go", "internal/tmp/scratch.go", "test/integration/run-7/case.go", "quick-fox-1a2b3c/copy.go", ".claude/worktrees/copy.go", "web/app/node_modules/dep.go", "scripts/__pycache__/cache.go"} {
		write(rel, "CANARY_BUILD_OUTPUT_"+string(rune('A'+i)))
	}
	envs, err := scanEnvVars(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, env := range envs {
		names = append(names, env.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"CANARY_KEPT", "CANARY_RUNNER"}) {
		t.Fatalf("the scan must read source and skip build output: %v", names)
	}
	if err := validateDocumentedEnvReads(root, envs); err != nil {
		t.Fatalf("undocumented reads inside build output must not be validated: %v", err)
	}
}

// Every directory .gitignore excludes is outside the scan, so a new build
// output added there cannot reintroduce the race unnoticed.
func TestScanSkipsEveryGitignoredDirectory(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	root := t.TempDir()
	excluded := func(rel string) bool {
		for dir := ""; ; {
			part, rest, more := strings.Cut(rel, "/")
			dir = strings.TrimPrefix(dir+"/"+part, "/")
			if skipScanDir(root, filepath.Join(root, filepath.FromSlash(dir))) {
				return true
			}
			if !more {
				return false
			}
			rel = rest
		}
	}
	checked := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		dir := strings.TrimSuffix(line, "/")
		if !strings.HasSuffix(line, "/") && (strings.Contains(dir, ".") || strings.Contains(dir, "*")) {
			continue // a file pattern
		}
		sample := strings.TrimPrefix(dir, "**/")
		sample = strings.TrimPrefix(sample, "/")
		sample = strings.ReplaceAll(sample, "[0-9a-f]", "a")
		sample = strings.ReplaceAll(sample, "*", "x")
		if !excluded(sample) {
			t.Errorf(".gitignore excludes %q but the scan would enter %q", line, sample)
		}
		checked++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if checked < 5 {
		t.Fatalf("read only %d ignored directories from .gitignore", checked)
	}
}
