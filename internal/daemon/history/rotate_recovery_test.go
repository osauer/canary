package history

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRotationRecoverySurvivesDatabaseDeletion pins the file-side intent:
// history.db is derived and may disappear before or after the live-file
// commit without making archive+live reconstruction ambiguous.
func TestRotationRecoverySurvivesDatabaseDeletion(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, stage string
	}{
		{"pre-swap", "renamed"},
		{"post-swap", "swapped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := testOptions(t)
			original := buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 3)
			s := openTestStore(t, opts)
			s.ingestAll(context.Background())
			s.rotateFailpoint = func(stage string) error {
				if stage == tc.stage {
					return errors.New("injected crash")
				}
				return nil
			}
			s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
			if _, err := os.Stat(s.rotationManifestPath(sourceRegime)); err != nil {
				t.Fatalf("durable rotation manifest missing at %s: %v", tc.stage, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			removeDBFiles(opts.DBPath)

			s2 := openTestStore(t, opts)
			s2.RecoverRotations(regimeRotationSource())
			if _, err := os.Stat(s2.rotationManifestPath(sourceRegime)); !os.IsNotExist(err) {
				t.Fatalf("manifest survived successful recovery: %v", err)
			}
			if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original {
				t.Fatalf("stream differs after %s recovery with database deletion", tc.name)
			}
			s2.ingestAll(context.Background())
			rows := dumpRows(t, s2, "regime_decisions")
			if len(rows) != strings.Count(original, "\n") {
				t.Fatalf("rebuilt rows = %d, want %d", len(rows), strings.Count(original, "\n"))
			}
		})
	}
}

// TestAdoptLegacyCanaryRotationManifest pins the on-disk half of the
// stress rename: an intent a pre-rename build left pending is carried to
// the current name at Open, so recovery still resolves it. Left unadopted
// it would be unresolvable, and the cutover path fails startup on exactly
// that. The fixture is a REAL pending manifest produced by a crashed
// rotation, then renamed back to the pre-rename shape.
func TestAdoptLegacyCanaryRotationManifest(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	original := buildMonthlyJournal(t, opts.CanaryJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 3)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.rotateFailpoint = func(stage string) error {
		if stage == "renamed" {
			return errors.New("injected crash")
		}
		return nil
	}
	s.RotateAll(context.Background(), stressRotationSource(), 2, rotationNow)
	pending, err := s.readRotationManifest(s.rotationManifestPath(sourceStress))
	if err != nil {
		t.Fatalf("no durable rotation manifest to age back: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Age the artifact back to what a pre-rename build wrote.
	legacyPath := s.rotationManifestPath(legacyCanaryManifestSource)
	legacy := pending
	legacy.Source = legacyCanaryManifestSource
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.rotationManifestPath(sourceStress)); err != nil {
		t.Fatal(err)
	}

	s2 := openTestStore(t, opts)
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy rotation intent survived adoption: %v", err)
	}
	adopted, err := s2.readRotationManifest(s2.rotationManifestPath(sourceStress))
	if err != nil {
		t.Fatalf("adopted rotation intent unreadable: %v", err)
	}
	if adopted.Source != sourceStress || !sameRotationCut(adopted, pending) {
		t.Fatalf("adopted intent = %+v, want the same cut under source %q", adopted, sourceStress)
	}

	// The adopted intent is recoverable, and the evidence stream is intact.
	s2.RecoverRotations(stressRotationSource())
	if _, err := os.Stat(s2.rotationManifestPath(sourceStress)); !os.IsNotExist(err) {
		t.Fatalf("adopted intent survived successful recovery: %v", err)
	}
	if got := reconstructStream(t, opts, opts.CanaryJournalPath); got != original {
		t.Fatal("stream differs after adopting and recovering the legacy intent")
	}
}

// TestAdoptLegacyCanaryRotationManifestKeepsConflictingIntent pins the
// fail-closed branch: two intents describing different cuts are never
// resolved by guess, because the legacy file's archives may be the only
// copy of a journal prefix.
func TestAdoptLegacyCanaryRotationManifestKeepsConflictingIntent(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildMonthlyJournal(t, opts.CanaryJournalPath, []string{"2026-04", "2026-05", "2026-06"}, 3)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.rotateFailpoint = func(stage string) error {
		if stage == "renamed" {
			return errors.New("injected crash")
		}
		return nil
	}
	s.RotateAll(context.Background(), stressRotationSource(), 2, rotationNow)
	pending, err := s.readRotationManifest(s.rotationManifestPath(sourceStress))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	legacy := pending
	legacy.Source = legacyCanaryManifestSource
	legacy.CutBytes-- // a different cut than the current-name intent
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacyPath := s.rotationManifestPath(legacyCanaryManifestSource)
	if err := os.WriteFile(legacyPath, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	s2 := openTestStore(t, opts)
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("conflicting legacy intent was removed: %v", err)
	}
	kept, err := s2.readRotationManifest(s2.rotationManifestPath(sourceStress))
	if err != nil {
		t.Fatalf("current-name intent disturbed: %v", err)
	}
	if !sameRotationCut(kept, pending) {
		t.Fatalf("current-name intent overwritten: %+v", kept)
	}
}

func TestFinalizeRotationIsIdempotent(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-06"}, 2)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)

	var id, cut, baseBefore int64
	var postGenesis, archivesJSON string
	if err := s.db.QueryRow(`SELECT id, cut_bytes, base_before, COALESCE(post_genesis, ''), archives_json FROM rotation_log ORDER BY id DESC LIMIT 1`).
		Scan(&id, &cut, &baseBefore, &postGenesis, &archivesJSON); err != nil {
		t.Fatal(err)
	}
	var archives []rotationArchive
	if err := json.Unmarshal([]byte(archivesJSON), &archives); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := s.db.QueryRow(`SELECT base FROM ingest_sources WHERE source = 'regime'`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.finalizeRotation(id, sourceRegime, baseBefore, cut, postGenesis, archives); err != nil {
		t.Fatalf("repeat finalize: %v", err)
	}
	var after int64
	if err := s.db.QueryRow(`SELECT base FROM ingest_sources WHERE source = 'regime'`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("repeat finalize advanced base from %d to %d", before, after)
	}
}

func TestSyncDirReportsFailure(t *testing.T) {
	t.Parallel()
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDir silently accepted an unreadable directory")
	}
}

func TestRollbackFailureRemainsPending(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-06"}, 2)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.rotateFailpoint = func(stage string) error {
		if stage == "renamed" {
			return errors.New("injected crash")
		}
		return nil
	}
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	s.rotateFailpoint = nil

	var id int64
	if err := s.db.QueryRow(`SELECT id FROM rotation_log WHERE state = 'pending'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(opts.RotatedDir, "regime-decisions-2026-04.jsonl.gz")
	f, err := os.OpenFile(archive, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Force quarantine rename failure without changing the live journal.
	if err := os.Mkdir(archive+fmt.Sprintf(".quarantine-%d", id), 0o700); err != nil {
		t.Fatal(err)
	}

	s.RecoverRotations(regimeRotationSource())
	var state string
	if err := s.db.QueryRow(`SELECT state FROM rotation_log WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("failed rollback state = %q, want pending", state)
	}
	if _, err := os.Stat(s.rotationManifestPath(sourceRegime)); err != nil {
		t.Fatalf("failed rollback removed its durable manifest: %v", err)
	}
}

func TestRotationDoesNotDropAppendBeforeSwap(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	original := buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-06"}, 2)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	appended := `{"v":1,"ts":"2026-07-20T10:00:00Z","stage":"calm","fingerprint":"late"}` + "\n"
	s.rotateFailpoint = func(stage string) error {
		if stage != "renamed" {
			return nil
		}
		f, err := os.OpenFile(opts.RegimeJournalPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(appended); err != nil {
			_ = f.Close()
			return err
		}
		return f.Close()
	}
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	s.rotateFailpoint = nil

	live, err := os.ReadFile(opts.RegimeJournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(live) != original+appended {
		t.Fatal("rotation rewrote or dropped a journal append made before swap")
	}
	s.RecoverRotations(regimeRotationSource())
	if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original+appended {
		t.Fatal("recovery did not preserve the append made before swap")
	}
}

func TestManifestRetainedUntilFinalizeCheckpoint(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	original := buildMonthlyJournal(t, opts.RegimeJournalPath, []string{"2026-04", "2026-05"}, 2)
	s := openTestStore(t, opts)
	s.ingestAll(context.Background())
	s.rotateFailpoint = func(stage string) error {
		if stage == "finalized" {
			return errors.New("power loss before checkpoint")
		}
		return nil
	}
	s.RotateAll(context.Background(), regimeRotationSource(), 2, rotationNow)
	s.rotateFailpoint = nil
	if _, err := os.Stat(s.rotationManifestPath(sourceRegime)); err != nil {
		t.Fatalf("manifest removed before finalize checkpoint: %v", err)
	}

	var id, cut, baseBefore int64
	if err := s.db.QueryRow(`SELECT id, cut_bytes, base_before FROM rotation_log ORDER BY id DESC LIMIT 1`).Scan(&id, &cut, &baseBefore); err != nil {
		t.Fatal(err)
	}
	// Model loss of the uncheckpointed NORMAL-synchronous finalize commit.
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE ingest_sources SET base = ? WHERE source = 'regime'`, baseBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`DELETE FROM archive_files WHERE source = 'regime'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE rotation_log SET state = 'pending', finished_at = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	appended := `{"v":1,"ts":"2026-07-20T11:00:00Z","stage":"calm","fingerprint":"after-empty-tail"}` + "\n"
	appendJournal(t, opts.RegimeJournalPath, appended)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openTestStore(t, opts)
	s2.RecoverRotations(regimeRotationSource())
	if _, err := os.Stat(s2.rotationManifestPath(sourceRegime)); !os.IsNotExist(err) {
		t.Fatalf("manifest survived checkpointed recovery: %v", err)
	}
	var state string
	var base int64
	if err := s2.db.QueryRow(`SELECT state FROM rotation_log WHERE id = ?`, id).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := s2.db.QueryRow(`SELECT base FROM ingest_sources WHERE source = 'regime'`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if state != "done" || base != baseBefore+cut {
		t.Fatalf("recovered finalize = state %q base %d, want done/%d", state, base, baseBefore+cut)
	}
	s2.ingestAll(context.Background())
	if got := reconstructStream(t, opts, opts.RegimeJournalPath); got != original+appended {
		t.Fatal("checkpoint recovery changed the evidence stream")
	}
}

func TestSwapLiveTailSyncsRestoredMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "rules-decisions.jsonl")
	if err := os.WriteFile(path, []byte("old\nkeep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := swapLiveTail(path, 4, 9, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep\n" || st.Mode().Perm() != 0o644 {
		t.Fatalf("swapped live tail = %q mode %o, want keep\\n/0644", got, st.Mode().Perm())
	}
}

func TestRotationManifestRejectsUnsafeArchives(t *testing.T) {
	t.Parallel()
	opts := testOptions(t)
	s := openTestStore(t, opts)
	def, ok := s.sourceByName(sourceRegime)
	if !ok {
		t.Fatal("regime source missing")
	}
	valid := rotationArchive{
		Name: "regime-decisions-2026-04.jsonl.gz", Months: "2026-04",
		RawBytes: 10, GzBytes: 20, SHA256: strings.Repeat("0", sha256.Size*2),
	}
	unsafe := valid
	unsafe.Name = "../regime-decisions-2026-04.jsonl.gz"
	if err := validateRotationArchives(def, 10, []rotationArchive{unsafe}); err == nil {
		t.Fatal("archive path traversal was accepted")
	}
	if err := validateRotationArchives(def, 11, []rotationArchive{valid}); err == nil {
		t.Fatal("archive raw-byte total differing from cut was accepted")
	}
}

// stressRotationSource binds the stress journal to a standalone lock, the
// rotation-test counterpart of regimeRotationSource.
func stressRotationSource() []RotationSource {
	return []RotationSource{{Name: sourceStress, Locker: &sync.Mutex{}}}
}
