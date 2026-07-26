package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func openStatementProjectionTestServer(t *testing.T) (*Server, *corestore.Store, time.Time) {
	t.Helper()
	stateHome := t.TempDir()
	if err := os.Chmod(stateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(stateHome, "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	return &Server{coreStore: core, now: func() time.Time { return now }}, core, now
}

func TestStatementProjectionCutoverRestatementAndRemoval(t *testing.T) {
	s, core, now := openStatementProjectionTestServer(t)
	const older = "flex-20260713-063000.xml"
	const newer = "flex-20260714-063000.xml"
	writeFlexFixture(t, older, "20260713;063000", "20260708", "20260709",
		equityRow("20260708", 100)+"\n"+equityRow("20260709", 110))
	writeFlexFixture(t, newer, "20260714;063000", "20260709", "20260710",
		equityRow("20260709", 200)+"\n"+equityRow("20260710", 210))

	report, err := rebuildStatementProjectionForCutover(t.Context(), core, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.FileCount != 2 || report.StatementCount != 2 || report.EquityInputRows != 4 || report.EquityWinnerRows != 3 {
		t.Fatalf("statement cutover report = %+v", report)
	}
	if len(report.SourceSetSHA256) != 64 || len(report.ProjectionSHA256) != 64 || len(report.Sources) != 2 {
		t.Fatalf("statement cutover hashes/sources = %+v", report)
	}
	for _, source := range report.Sources {
		if source.Status != "imported" || source.Statements != 1 || source.EquityRows != 2 || len(source.SHA256) != 64 {
			t.Fatalf("statement source = %+v", source)
		}
	}

	entries, total, err := s.sqliteStatementEquityDays(t.Context(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(entries) != 3 || entries[0].Day != "2026-07-10" || entries[1].EquityBase != 200 || entries[1].SourceStmt != newer {
		t.Fatalf("initial statement winners total=%d entries=%+v", total, entries)
	}
	head, err := core.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	afterNoop, err := core.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if afterNoop.HeadGeneration != head.HeadGeneration {
		t.Fatalf("unchanged statement inventory advanced authority head: before=%d after=%d", head.HeadGeneration, afterNoop.HeadGeneration)
	}

	dir, _ := flexStatementsDirPath()
	newerPath := filepath.Join(dir, newer)
	raw, err := os.ReadFile(newerPath)
	if err != nil {
		t.Fatal(err)
	}
	corrected := strings.Replace(string(raw), `total="200.000000"`, `total="222.000000"`, 1)
	if len(corrected) != len(raw) || corrected == string(raw) {
		t.Fatal("same-size restatement fixture was not constructed")
	}
	if err := os.WriteFile(newerPath, []byte(corrected), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, total, err = s.sqliteStatementEquityDays(t.Context(),
		time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC), 10)
	if err != nil || total != 1 || entries[0].EquityBase != 222 {
		t.Fatalf("same-size restatement total=%d entries=%+v err=%v", total, entries, err)
	}

	if err := os.Remove(newerPath); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, total, err = s.sqliteStatementEquityDays(t.Context(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil || total != 2 || entries[0].Day != "2026-07-09" || entries[0].EquityBase != 110 || entries[0].SourceStmt != older {
		t.Fatalf("removed-file fallback total=%d entries=%+v err=%v", total, entries, err)
	}
	health, err := s.sqliteStatementsHealth(t.Context())
	if err != nil || health.IngestedBytes != health.JournalBytes || health.LastIngestAt.IsZero() {
		t.Fatalf("statement health=%+v err=%v", health, err)
	}
}

func TestStatementProjectionFailurePreservesLastCompleteGeneration(t *testing.T) {
	s, core, now := openStatementProjectionTestServer(t)
	const name = "flex-20260713-063000.xml"
	writeFlexFixture(t, name, "20260713;063000", "20260709", "20260709", equityRow("20260709", 110))
	if _, err := rebuildStatementProjectionForCutover(t.Context(), core, now); err != nil {
		t.Fatal(err)
	}
	beforeFiles, err := core.LoadStatementFiles(t.Context(), statementProjectionScope)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := flexStatementsDirPath()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("<FlexQueryResponse><broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshStatementProjection(t.Context()); err == nil {
		t.Fatal("malformed restatement replaced the complete projection")
	}
	afterFiles, err := core.LoadStatementFiles(t.Context(), statementProjectionScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFiles) != 1 || afterFiles[0].SHA256 != beforeFiles[0].SHA256 {
		t.Fatalf("malformed restatement changed current inventory: before=%+v after=%+v", beforeFiles, afterFiles)
	}
	entries, total, err := s.sqliteStatementEquityDays(t.Context(),
		time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil || total != 1 || entries[0].EquityBase != 110 {
		t.Fatalf("projection after parse failure total=%d entries=%+v err=%v", total, entries, err)
	}
}

func TestStatementProjectionCutoverRejectsSymlinkedEvidence(t *testing.T) {
	_, core, now := openStatementProjectionTestServer(t)
	dir, err := flexStatementsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.xml")
	if err := os.WriteFile(target, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "linked.xml")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildStatementProjectionForCutover(t.Context(), core, now); err == nil {
		t.Fatal("symlinked statement evidence was accepted")
	}
	files, err := core.LoadStatementFiles(t.Context(), statementProjectionScope)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("symlink failure published inventory: %+v", files)
	}
}
