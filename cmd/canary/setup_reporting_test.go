package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/flexstmt"
)

func TestWriteCandidateReportingTokenIsPrivate(t *testing.T) {
	t.Parallel()

	secret := []byte("private-token")
	path, err := writeCandidateReportingToken(t.TempDir(), secret)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %04o", got)
	}
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != "private-token\n" {
		t.Fatal("stored token does not match input")
	}
}

func TestValidSetupReportingQueryID(t *testing.T) {
	t.Parallel()

	if !validSetupReportingQueryID("123456") {
		t.Fatal("rejected numeric Query ID")
	}
	for _, value := range []string{"", "12 34", "12a4", "１２３４"} {
		if validSetupReportingQueryID(value) {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestReadSetupSecretLineAcceptsLongPastedToken(t *testing.T) {
	t.Parallel()

	want := strings.Repeat("9", 256)
	got, err := readSetupSecretLine(bufio.NewReader(strings.NewReader(want + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatal("pasted token did not round trip")
	}
}

func TestSetupReportingPromptIsConciseAndExplainsHiddenPaste(t *testing.T) {
	t.Parallel()

	var checklist bytes.Buffer
	printReportingChecklist(&checklist)
	for _, section := range flexstmt.CanonicalQueryManifest() {
		if !strings.Contains(checklist.String(), section.Label) {
			t.Fatalf("checklist missing section %q", section.Label)
		}
	}
	if strings.Contains(checklist.String(), "IBCommissionCurrency") {
		t.Fatal("checklist printed the verbose field manifest")
	}

	var prompt bytes.Buffer
	printSetupReportingTokenPrompt(&prompt)
	for _, phrase := range []string{"Command-V on macOS", "press Return", "Nothing will appear"} {
		if !strings.Contains(prompt.String(), phrase) {
			t.Fatalf("token prompt missing %q", phrase)
		}
	}
}

func TestPruneSupersededReportingTokensKeepsActiveAndRollbackGeneration(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	active := filepath.Join(dir, "flex-token-active")
	rollback := filepath.Join(dir, "flex-token-rollback")
	relativeRollback := filepath.Join(dir, "flex-token-relative")
	old := filepath.Join(dir, "flex-token-old")
	unrelated := filepath.Join(dir, "operator-note")
	for _, path := range []string{active, rollback, relativeRollback, old, unrelated} {
		if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneSupersededReportingTokens(dir, active, "~/flex-token-rollback", "flex-token-relative"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{active, rollback, relativeRollback, unrelated} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("kept file %s: %v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("superseded token still exists: %v", err)
	}
}

func TestPruneSupersededReportingTokensRejectsFilesystemRoot(t *testing.T) {
	t.Parallel()
	root := filepath.VolumeName(t.TempDir()) + string(os.PathSeparator)
	if err := pruneSupersededReportingTokens(root); err == nil {
		t.Fatal("filesystem-root token cleanup was accepted")
	}
}
