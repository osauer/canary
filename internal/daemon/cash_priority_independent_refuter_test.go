package daemon

import (
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"path/filepath"
	"testing"
)

func TestIndependentPriorityRestartReplayAndFailedSave(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	core, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	_, err = core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindPlatformSettings, JSON: []byte(oldCashPreferenceSettings)})
	if err != nil {
		t.Fatal(err)
	}
	bind := func() *Server {
		t.Helper()
		p := &platformSettingsStore{}
		if err := p.bindCore(t.Context(), core); err != nil {
			t.Fatal(err)
		}
		return &Server{platformSettings: p}
	}
	s := bind()
	first, latest := "balanced", "eur_first"
	if _, err := saveCashPreference(t, s, &first, 1, "lost-ack-before-restart"); err != nil {
		t.Fatal(err)
	}
	if _, err := saveCashPreference(t, s, &latest, 2, "new-choice"); err != nil {
		t.Fatal(err)
	}
	if err = core.Close(); err != nil {
		t.Fatal(err)
	}
	core, err = corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	s = bind()
	head, err := core.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := saveCashPreference(t, s, &first, 1, "lost-ack-before-restart")
	if err != nil || !receipt.Replay || receipt.Revision != 3 || receipt.SavedRevision != 2 || receipt.CurrencyPriority == nil || *receipt.CurrencyPriority != latest {
		t.Fatalf("restart replay rolled choice back: %+v %v", receipt, err)
	}
	after, err := core.AuthorityHead(t.Context())
	if err != nil || after != head {
		t.Fatalf("replay rewrote DB: %v", err)
	}
	if err = core.Close(); err != nil {
		t.Fatal(err)
	}
	// An unreachable durable store must not publish a different preference.
	if _, err = saveCashPreference(t, s, &first, 3, "fail-durable-write"); err == nil {
		t.Fatal("failed DB write reported save")
	}
	got, err := s.handleCashSweepPreferences()
	if err != nil || got.CurrencyPriority == nil || *got.CurrencyPriority != latest || got.Revision != 3 {
		t.Fatalf("failed save changed in-memory preference: %+v %v", got, err)
	}
}
