package daemon

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestOpenCoreStoreFreshCustomIsIsolatedAndRestartable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", privateTestDir(t))
	t.Setenv("XDG_CACHE_HOME", privateTestDir(t))
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))
	dbPath := filepath.Join(privateTestDir(t), "daemon.db")
	s := newCutoverTestServer(t, dbPath)
	if err := s.openCoreStore(t.Context()); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := s.coreStore.GetStateDocument(t.Context(), daemonStateScope, v3BootstrapKind)
	if err != nil || !ok {
		t.Fatalf("v3 marker: ok=%v err=%v", ok, err)
	}
	var marker v3Bootstrap
	if json.Unmarshal(doc.JSON, &marker) != nil || marker.Version != 1 || marker.CreatedAt.IsZero() {
		t.Fatalf("invalid v3 marker: %s", doc.JSON)
	}
	if err := s.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
	restarted := newCutoverTestServer(t, dbPath)
	if err := restarted.openCoreStore(t.Context()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if err := restarted.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
}

func TestProductionV3RequiresStableBridgeForFileAuthority(t *testing.T) {
	stateRoot := privateTestDir(t)
	t.Setenv("XDG_STATE_HOME", stateRoot)
	t.Setenv("XDG_CACHE_HOME", privateTestDir(t))
	t.Setenv("XDG_CONFIG_HOME", privateTestDir(t))
	legacy := filepath.Join(stateRoot, "ibkr", "order-journal.jsonl")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := newCutoverTestServer(t, "")
	err := s.openCoreStore(t.Context())
	if err == nil || !strings.Contains(err.Error(), "latest stable 2.x") {
		t.Fatalf("error=%v, want stable-bridge instruction", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "ibkr", "daemon.db")); !os.IsNotExist(err) {
		t.Fatalf("v3 published a database beside unbridged authority: %v", err)
	}
}

func TestMajorBridgeAcceptsCompletedV2AndRejectsInterruptedV2(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := &Server{productionStateDatabase: true}
	write := func(status string, revision int64) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"status": status, "created_at": time.Now().UTC()})
		if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: legacyCutoverManifestKind, ExpectedRevision: revision, JSON: raw,
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("pending_seal", 0)
	if err := s.validateMajorBridge(t.Context(), store); err == nil {
		t.Fatal("interrupted v2 cutover was accepted")
	}
	write("sealed", 1)
	if err := s.validateMajorBridge(t.Context(), store); err != nil {
		t.Fatalf("completed v2 bridge: %v", err)
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
	if err := newCutoverTestServer(t, dbPath).openCoreStore(t.Context()); err == nil {
		t.Fatal("existing authority started without anti-rollback watermark")
	}
}

func TestOpenCoreStoreAdvancesWatermarkAfterMutation(t *testing.T) {
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
	if err != nil || after == nil || after.HeadGeneration <= before.HeadGeneration {
		t.Fatalf("watermark before=%+v after=%+v err=%v", before, after, err)
	}
	if err := s.closeCoreStore(); err != nil {
		t.Fatal(err)
	}
}

func newCutoverTestServer(t *testing.T, databasePath string) *Server {
	t.Helper()
	return New(Options{Config: &config.Resolved{}, SocketPath: filepath.Join(privateTestDir(t), "daemon.sock"), Version: "test", Logger: NewLogger(&bytes.Buffer{}, "error"), StateDatabasePath: databasePath})
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}
