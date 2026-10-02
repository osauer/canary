package daemon

import (
	"errors"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"path/filepath"
	"testing"
)

func TestIndependentPriorityWatermarkFailureCannotConfirmReplay(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	fail := false
	core, err := corestore.Open(t.Context(), corestore.Options{Path: path, CommitObserver: func(corestore.AuthorityHead) error {
		if fail {
			return errors.New("synthetic external watermark failure")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	_, err = core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindPlatformSettings, JSON: []byte(oldCashPreferenceSettings)})
	if err != nil {
		t.Fatal(err)
	}
	store := &platformSettingsStore{}
	if err = store.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	s := &Server{platformSettings: store}
	value := "balanced"
	fail = true
	if _, err = saveCashPreference(t, s, &value, 1, "watermark-failed"); err == nil {
		t.Fatal("failed watermark reported save")
	}
	if core.Health().Ready {
		t.Fatal("fixture not latched")
	}
	got, err := s.handleCashSweepPreferences()
	if err == nil && got.Writable {
		t.Error("latched store advertised writable preference")
	}
	replay, err := saveCashPreference(t, s, &value, 1, "watermark-failed")
	if err == nil {
		t.Errorf("latched replay promoted unaccepted settings: %+v", replay)
	}
	if store.data.CashSweep.CurrencyPriority != nil {
		t.Error("latched replay changed accepted in-memory settings")
	}
	if err = core.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := corestore.Open(t.Context(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered := &platformSettingsStore{}
	if err = recovered.bindCore(t.Context(), reopened); err != nil {
		t.Fatal(err)
	}
	s = &Server{platformSettings: recovered}
	eur := "eur_first"
	if _, err = saveCashPreference(t, s, &eur, 2, "after-explicit-reopen"); err != nil {
		t.Fatal(err)
	}
	head, _ := reopened.AuthorityHead(t.Context())
	current, err := saveCashPreference(t, s, &value, 1, "watermark-failed")
	if err != nil || !current.Replay || current.Revision != 3 || current.SavedRevision != 2 || current.EffectivePriority != "eur_first" {
		t.Fatalf("ready reopened replay: %+v %v", current, err)
	}
	after, _ := reopened.AuthorityHead(t.Context())
	if head != after {
		t.Fatal("reopened replay rewrote audit")
	}
}
