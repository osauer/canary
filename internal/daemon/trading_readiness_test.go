package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestLockedOrderInitializerKeepsCustomDatabaseArtifactsIsolated(t *testing.T) {
	liveRoot := filepath.Join(t.TempDir(), "must-not-touch")
	t.Setenv("XDG_STATE_HOME", liveRoot)
	dbPath := filepath.Join(t.TempDir(), "offline-authority", "daemon.db")
	authority, err := corestore.Open(t.Context(), corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open custom authority: %v", err)
	}
	defer authority.Close()
	srv := &Server{coreStorePath: dbPath, now: time.Now}
	if err := srv.initializeLockedOrderSignerAndReadiness(t.Context(), authority); err != nil {
		t.Fatalf("initialize locked order authority: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "order-preview-key-v2")); err != nil {
		t.Fatalf("custom authority signer key missing: %v", err)
	}
	if _, ok, err := authority.GetStateDocument(t.Context(), tradingReadinessStateScope, tradingReadinessStateKind); err != nil || !ok {
		t.Fatalf("empty readiness document found=%v err=%v", ok, err)
	}
	if _, err := os.Stat(liveRoot); !os.IsNotExist(err) {
		t.Fatalf("custom authority touched production-style state root: %v", err)
	}
}

// newTestPaperSmokeSigner builds an orderTokenSigner with a fresh key in a
// temp dir, the evidence-MAC dependency of the readiness store.
func newTestPaperSmokeSigner(t *testing.T) *orderTokenSigner {
	t.Helper()
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), nil)
	if err != nil {
		t.Fatalf("newOrderTokenSigner: %v", err)
	}
	return signer
}

func newTestTradingReadinessStore(t *testing.T, path string, signer *orderTokenSigner) *tradingReadinessStore {
	t.Helper()
	authorityPath := filepath.Join(filepath.Dir(path), "sqlite-authority", "daemon.db")
	authority, err := corestore.Open(t.Context(), corestore.Options{Path: authorityPath})
	if err != nil {
		t.Fatalf("open readiness authority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	store := newTradingReadinessStore(path, signer)
	if err := store.UseCoreStore(t.Context(), authority); err != nil {
		t.Fatalf("attach readiness authority: %v", err)
	}
	return store
}

func TestTradingReadinessSavePaperSmokeWritesPrivateSQLiteState(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state", "trading-readiness.json")
	store := newTestTradingReadinessStore(t, path, newTestPaperSmokeSigner(t))
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)

	err := store.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            now,
	})
	if err != nil {
		t.Fatalf("SavePaperSmoke: %v", err)
	}

	info, err := os.Stat(filepath.Join(filepath.Dir(path), "sqlite-authority", "daemon.db"))
	if err != nil {
		t.Fatalf("stat readiness database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o, want 600", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy readiness path was written: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PaperSmoke == nil || got.PaperSmoke.Account != "DU1234567" || !got.PaperSmoke.At.Equal(now) {
		t.Fatalf("loaded paper smoke = %+v", got.PaperSmoke)
	}
}

func TestTradingReadinessCheckPaperSmoke(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	store := newTestTradingReadinessStore(t, filepath.Join(t.TempDir(), "trading-readiness.json"), newTestPaperSmokeSigner(t))
	if err := store.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SavePaperSmoke: %v", err)
	}

	check := store.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusValid {
		t.Fatalf("status = %q, want valid: %+v", check.Status, check)
	}

	check = store.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 32, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusMismatch {
		t.Fatalf("status = %q, want mismatch", check.Status)
	}

	check = store.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", time.Minute, now)
	if check.Status != tradingPaperSmokeStatusStale {
		t.Fatalf("status = %q, want stale", check.Status)
	}
}

func TestTradingReadinessSurvivesAuthorityRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 13, 45, 0, 0, time.UTC)
	dbPath := filepath.Join(t.TempDir(), "authority", "daemon.db")
	signer := newTestPaperSmokeSigner(t)
	authority, err := corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("open readiness authority: %v", err)
	}
	store := newTradingReadinessStore("", signer)
	if err := store.UseCoreStore(ctx, authority); err != nil {
		t.Fatalf("attach readiness authority: %v", err)
	}
	if err := store.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account: "DU1234567", Endpoint: "127.0.0.1:4002", EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID: 31, Version: "test-version", Result: tradingPaperSmokeResultPassed, At: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("save readiness: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("close readiness authority: %v", err)
	}
	authority, err = corestore.Open(ctx, corestore.Options{Path: dbPath})
	if err != nil {
		t.Fatalf("reopen readiness authority: %v", err)
	}
	defer authority.Close()
	restarted := newTradingReadinessStore("", signer)
	if err := restarted.UseCoreStore(ctx, authority); err != nil {
		t.Fatalf("reattach readiness authority: %v", err)
	}
	if check := restarted.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now); check.Status != tradingPaperSmokeStatusValid {
		t.Fatalf("restarted readiness = %+v", check)
	}
}

func TestTradingReadinessRejectsTamperedEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "trading-readiness.json")
	store := newTestTradingReadinessStore(t, path, newTestPaperSmokeSigner(t))
	if err := store.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SavePaperSmoke: %v", err)
	}

	// Hand-edit the authoritative document without recomputing its MAC.
	store.mu.RLock()
	authority := store.authority
	store.mu.RUnlock()
	doc, ok, err := authority.GetStateDocument(context.Background(), tradingReadinessStateScope, tradingReadinessStateKind)
	if err != nil {
		t.Fatalf("read authority: %v", err)
	}
	if !ok {
		t.Fatal("readiness authority is missing")
	}
	f, err := decodeTradingReadinessDocument(doc.JSON)
	if err != nil {
		t.Fatalf("decode authority: %v", err)
	}
	f.PaperSmoke.Account = "DU7654321"
	tampered, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal tampered authority: %v", err)
	}
	if _, err := authority.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
		ScopeKey: tradingReadinessStateScope, Kind: tradingReadinessStateKind,
		ExpectedRevision: doc.Revision, JSON: tampered,
	}); err != nil {
		t.Fatalf("write tampered authority: %v", err)
	}
	check := store.CheckPaperSmoke("U7654321", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusUnsigned {
		t.Fatalf("status = %q, want unsigned for tampered evidence", check.Status)
	}
}

func TestTradingReadinessCutoverIgnoresLegacyHandWrittenEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "trading-readiness.json")
	// The pre-MAC era de-facto live switch: a hand-written file with no MAC.
	handWritten := `{"version":1,"paper_smoke":{"account":"DU1234567","endpoint":"127.0.0.1:4002","endpoint_class":"paper","client_id":31,"version":"test-version","result":"passed","at":"2026-05-28T06:00:00Z"}}`
	if err := os.WriteFile(path, []byte(handWritten), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := newTestTradingReadinessStore(t, path, newTestPaperSmokeSigner(t))
	check := store.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusMissing {
		t.Fatalf("status = %q, want missing because legacy evidence is not imported", check.Status)
	}
	if check.Action == "" || check.Message == "" {
		t.Fatalf("unsigned check must carry message+action: %+v", check)
	}
}

func TestTradingReadinessNilSignerFailsClosed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 28, 7, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "trading-readiness.json")
	signed := newTestTradingReadinessStore(t, path, newTestPaperSmokeSigner(t))
	if err := signed.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SavePaperSmoke: %v", err)
	}

	unsignedStore := newTradingReadinessStore(path, nil)
	if err := unsignedStore.UseCoreStore(t.Context(), signed.authority); err == nil {
		t.Fatal("UseCoreStore with nil signer must error")
	}
	if err := unsignedStore.SavePaperSmoke(tradingPaperSmokeEvidence{}); err == nil {
		t.Fatal("SavePaperSmoke with nil signer must error")
	}
	check := unsignedStore.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, now)
	if check.Status != tradingPaperSmokeStatusUnreadable {
		t.Fatalf("status = %q, want unreadable with unattached nil signer", check.Status)
	}
}

func TestPaperSmokeMACSignVerifyRoundTrip(t *testing.T) {
	t.Parallel()
	signer := newTestPaperSmokeSigner(t)
	ev := tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            time.Date(2026, 5, 28, 6, 0, 0, 123456789, time.UTC),
	}
	mac, err := signer.signPaperSmoke(ev)
	if err != nil || mac == "" {
		t.Fatalf("signPaperSmoke: %q, %v", mac, err)
	}
	if !signer.verifyPaperSmoke(ev, mac) {
		t.Fatal("verifyPaperSmoke must accept its own MAC")
	}
	if signer.verifyPaperSmoke(ev, "") {
		t.Fatal("empty MAC must not verify")
	}
	other := ev
	other.Result = tradingPaperSmokeResultFailed
	if signer.verifyPaperSmoke(other, mac) {
		t.Fatal("MAC must not transfer to altered evidence")
	}
	otherKey := newTestPaperSmokeSigner(t)
	if otherKey.verifyPaperSmoke(ev, mac) {
		t.Fatal("MAC must not verify under a different key")
	}
}
