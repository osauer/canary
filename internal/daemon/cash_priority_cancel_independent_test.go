package daemon

import (
	"context"
	"encoding/json"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	"path/filepath"
	"testing"
)

func TestIndependentPriorityCancelledAfterCommitReadHeals(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cancelAfterCommit := false
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db"), CommitObserver: func(corestore.AuthorityHead) error {
		if cancelAfterCommit {
			cancel()
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
	p := &platformSettingsStore{}
	if err = p.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	s := &Server{platformSettings: p}
	value := "balanced"
	raw, _ := json.Marshal(rpc.SetCashSweepPriorityRequest{CurrencyPriority: &value, ExpectedRevision: 1, RequestID: "committed-lost-page"})
	cancelAfterCommit = true
	_, err = s.handleCashSweepPrioritySet(ctx, &rpc.Request{Params: raw})
	if err == nil {
		t.Fatal("fixture expected unconfirmed caller response")
	}
	if !core.Health().Ready {
		t.Fatal("fixture incorrectly latched a successful commit")
	}
	got, err := s.handleCashSweepPreferences()
	if err != nil || got.Revision != 2 || got.CurrencyPriority == nil || *got.CurrencyPriority != value {
		t.Fatalf("fresh page retained obsolete revision after successful commit: %+v %v", got, err)
	}
}
