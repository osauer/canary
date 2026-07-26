package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestOpenCoreStoreFreshCustomIsIsolatedAndRestartable(t *testing.T) {
	liveState := privateTestDir(t)
	liveCache := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", liveState)
	t.Setenv("XDG_CACHE_HOME", liveCache)
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))

	custom := privateTestDir(t)
	dbPath := filepath.Join(custom, "daemon.db")
	orderSentinel := filepath.Join(custom, "order-journal.jsonl")
	purgeSentinel := filepath.Join(custom, "purge-ledger.json")
	if err := os.WriteFile(orderSentinel, []byte("must-not-be-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(purgeSentinel, []byte("must-not-be-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := newCutoverTestServer(t, dbPath)
	if err := s.openCoreStore(t.Context()); err != nil {
		t.Fatalf("open fresh custom authority: %v", err)
	}
	manifest, _, err := loadCoreCutoverManifest(t.Context(), s.coreStore)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != coreCutoverStatusFresh || manifest.ImportedLegacy || len(manifest.Sources) != 0 {
		t.Fatalf("fresh manifest = %+v", manifest)
	}
	events, err := s.coreStore.LoadOrderEvents(t.Context(), corestore.OrderQuery{Limit: 10})
	if err != nil || len(events) != 0 {
		t.Fatalf("fresh order authority events=%d err=%v", len(events), err)
	}
	for _, path := range []string{orderSentinel, purgeSentinel} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "must-not-be-read\n" {
			t.Fatalf("custom sibling %s changed: data=%q err=%v", filepath.Base(path), got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(liveState, "ibkr", "order-preview-key-v2")); !os.IsNotExist(err) {
		t.Fatalf("custom authority touched live signer path: %v", err)
	}
	if info, err := os.Stat(filepath.Join(custom, "order-preview-key-v2")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("custom signer missing or invalid: info=%v err=%v", info, err)
	}
	if err := s.closeCoreStore(); err != nil {
		t.Fatal(err)
	}

	restarted := newCutoverTestServer(t, dbPath)
	if err := restarted.openCoreStore(t.Context()); err != nil {
		t.Fatalf("reopen fresh custom authority: %v", err)
	}
	if err := restarted.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCoreStoreProductionEmptyCutoverSealsAndRestarts(t *testing.T) {
	stateRoot := privateTestDir(t)
	cacheRoot := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	t.Setenv("XDG_CACHE_HOME", cacheRoot)
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))
	contractPath := filepath.Join(cacheRoot, "ibkr", "contracts.json")
	if err := os.MkdirAll(filepath.Dir(contractPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const contractSentinel = "legacy contract acceleration cache"
	if err := os.WriteFile(contractPath, []byte(contractSentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	briefPath := filepath.Join(stateRoot, "ibkr", briefStateFile)
	writeJSONFixture(t, briefPath, briefStateFileV1{
		Version: briefStateVersion,
		Stamps: map[string]briefStampState{
			"daily": {Fingerprint: "legacy-derived-baseline", At: time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)},
		},
	})
	legacyBrief, err := os.ReadFile(briefPath)
	if err != nil {
		t.Fatal(err)
	}

	s := newCutoverTestServer(t, "")
	if err := s.openCoreStore(t.Context()); err != nil {
		t.Fatalf("open production cutover authority: %v", err)
	}
	manifest, _, err := loadCoreCutoverManifest(t.Context(), s.coreStore)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Status != coreCutoverStatusSealed || !manifest.ImportedLegacy || manifest.CompletedAt.IsZero() {
		t.Fatalf("sealed manifest = %+v", manifest)
	}
	var contractSource, briefSource *coreCutoverSource
	for i := range manifest.Sources {
		switch manifest.Sources[i].Kind {
		case "contract_cache":
			contractSource = &manifest.Sources[i]
		case "brief_baselines":
			briefSource = &manifest.Sources[i]
		}
	}
	if contractSource == nil || contractSource.Path != contractPath || contractSource.Status != "sealed" {
		t.Fatalf("contract cache source = %+v, want sealed %s", contractSource, contractPath)
	}
	if got, err := os.ReadFile(contractSource.Destination); err != nil || string(got) != contractSentinel {
		t.Fatalf("sealed contract cache data=%q err=%v", got, err)
	}
	if _, err := os.Lstat(contractPath); !os.IsNotExist(err) {
		t.Fatalf("legacy contract cache remains live: %v", err)
	}
	if briefSource == nil || briefSource.Path != briefPath || briefSource.Status != "sealed" || briefSource.SkippedClass != "discarded_presentation_baseline" {
		t.Fatalf("brief baseline source = %+v, want explicitly discarded and sealed", briefSource)
	}
	if got, err := os.ReadFile(briefSource.Destination); err != nil || !bytes.Equal(got, legacyBrief) {
		t.Fatalf("sealed brief baseline data=%q err=%v", got, err)
	}
	if _, err := os.Lstat(briefPath); !os.IsNotExist(err) {
		t.Fatalf("legacy brief baseline remains live: %v", err)
	}
	if _, ok := s.briefState.latestBaseline(); ok {
		t.Fatal("legacy derived brief baseline seeded the clean SQLite epoch")
	}
	for _, name := range []string{"order-preview-key", "order-journal.jsonl"} {
		info, err := os.Stat(filepath.Join(stateRoot, "ibkr", name))
		if err != nil || !info.IsDir() {
			t.Fatalf("legacy blocker %s: info=%v err=%v", name, info, err)
		}
	}
	for _, path := range []string{manifest.PrepublishBackupPath, manifest.FinalBackupPath} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("verified cutover backup %s missing: info=%v err=%v", filepath.Base(path), info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "ibkr", "history.db")); !os.IsNotExist(err) {
		t.Fatalf("production cutover recreated history.db: %v", err)
	}
	if err := s.closeCoreStore(); err != nil {
		t.Fatal(err)
	}

	restarted := newCutoverTestServer(t, "")
	if err := restarted.openCoreStore(t.Context()); err != nil {
		t.Fatalf("reopen sealed production authority: %v", err)
	}
	if err := restarted.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCoreStoreRejectsExistingAuthorityWithoutWatermark(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", privateTestDir(t))
	t.Setenv("XDG_CACHE_HOME", privateTestDir(t))
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))

	dbPath := filepath.Join(privateTestDir(t), "daemon.db")
	s := newCutoverTestServer(t, dbPath)
	if err := s.openCoreStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dbPath + ".head"); err != nil {
		t.Fatal(err)
	}

	restarted := newCutoverTestServer(t, dbPath)
	if err := restarted.openCoreStore(t.Context()); err == nil {
		t.Fatal("existing authority started without its anti-rollback watermark")
	}
}

func TestOpenCoreStoreRejectsStaleBackupRestore(t *testing.T) {
	tests := []struct {
		name       string
		production bool
		dropHead   bool
	}{
		{name: "prepublish_with_newer_head", production: false},
		{name: "prepublish_without_head", production: false, dropHead: true},
		{name: "final_with_newer_head", production: true},
		{name: "final_without_head", production: true, dropHead: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stateRoot := privateTestDir(t)
			t.Setenv("XDG_STATE_HOME", stateRoot)
			t.Setenv("XDG_CACHE_HOME", privateTestDir(t))
			t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))

			dbPath := filepath.Join(privateTestDir(t), "daemon.db")
			configuredPath := dbPath
			if tt.production {
				configuredPath = ""
			}
			s := newCutoverTestServer(t, configuredPath)
			if err := s.openCoreStore(t.Context()); err != nil {
				t.Fatal(err)
			}
			dbPath = s.coreStorePath
			manifest, _, err := loadCoreCutoverManifest(t.Context(), s.coreStore)
			if err != nil {
				t.Fatal(err)
			}
			backupPath := manifest.PrepublishBackupPath
			if tt.production {
				backupPath = manifest.FinalBackupPath
			}
			if _, err := s.coreStore.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
				ScopeKey: "test", Kind: "post_backup_tombstone", JSON: []byte(`{"consumed":true}`),
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.closeCoreStore(); err != nil {
				t.Fatal(err)
			}

			stale, err := os.ReadFile(backupPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(dbPath); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{"-wal", "-shm"} {
				if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(dbPath, stale, 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.dropHead {
				if err := os.Remove(dbPath + ".head"); err != nil {
					t.Fatal(err)
				}
			}

			restarted := newCutoverTestServer(t, configuredPath)
			if err := restarted.openCoreStore(t.Context()); err == nil {
				t.Fatal("rolled-back authority started after restoring an older verified backup")
			}
		})
	}
}

func TestOpenCoreStoreResumesFinalBackupBeforeSealedStatus(t *testing.T) {
	stateRoot := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	t.Setenv("XDG_CACHE_HOME", privateTestDir(t))
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))

	s := newCutoverTestServer(t, "")
	if err := s.openCoreStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	manifest, doc, err := loadCoreCutoverManifest(t.Context(), s.coreStore)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Status = coreCutoverStatusBackup
	if _, err := writeCoreCutoverManifest(t.Context(), s.coreStore, manifest, doc.Revision); err != nil {
		t.Fatal(err)
	}
	if err := s.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifest.FinalBackupPath); err != nil {
		t.Fatal(err)
	}
	restarted := newCutoverTestServer(t, "")
	if err := restarted.openCoreStore(t.Context()); err != nil {
		t.Fatalf("resume final-backup cutover phase: %v", err)
	}
	got, _, err := loadCoreCutoverManifest(t.Context(), restarted.coreStore)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != coreCutoverStatusSealed {
		t.Fatalf("resumed status=%q, want %q", got.Status, coreCutoverStatusSealed)
	}
	if _, err := corestore.VerifyBackup(t.Context(), got.FinalBackupPath, got.MinimumFinalBackupHead); err != nil {
		t.Fatalf("resumed final backup: %v", err)
	}
	if err := restarted.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCoreStoreAdvancesWatermarkAfterEveryCommittedMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", privateTestDir(t))
	t.Setenv("XDG_CACHE_HOME", privateTestDir(t))
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))

	dbPath := filepath.Join(privateTestDir(t), "daemon.db")
	s := newCutoverTestServer(t, dbPath)
	if err := s.openCoreStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := loadAuthorityWatermark(dbPath + ".head")
	if err != nil || before == nil {
		t.Fatalf("initial watermark=%+v err=%v", before, err)
	}
	if _, err := s.coreStore.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
		ScopeKey: "test", Kind: "watermark_probe", JSON: []byte(`{"v":1}`),
	}); err != nil {
		t.Fatal(err)
	}
	after, err := loadAuthorityWatermark(dbPath + ".head")
	if err != nil || after == nil {
		t.Fatalf("advanced watermark=%+v err=%v", after, err)
	}
	live, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if *after != live || after.HeadGeneration <= before.HeadGeneration {
		t.Fatalf("watermark before=%+v after=%+v live=%+v", *before, *after, live)
	}
	if err := s.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
}

func newCutoverTestServer(t *testing.T, databasePath string) *Server {
	t.Helper()
	return New(Options{
		Config:            &config.Resolved{},
		SocketPath:        filepath.Join(privateTestDir(t), "daemon.sock"),
		Version:           "test",
		Logger:            NewLogger(&bytes.Buffer{}, "error"),
		StateDatabasePath: databasePath,
	})
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
