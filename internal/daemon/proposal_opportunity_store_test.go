package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestProposalOpportunityCleanCutoverIgnoresLegacyFiles(t *testing.T) {
	stateHome := t.TempDir()
	if err := os.Chmod(stateHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", stateHome)
	legacyDir := filepath.Join(stateHome, "ibkr")
	if err := os.MkdirAll(legacyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]string{
		"trade-proposals-current.json": `{"revision":"legacy-proposal"}`,
		"trade-proposals.jsonl":        `{"type":"ignored","key":"legacy-proposal"}` + "\n",
		"opportunities-current.json":   `{"revision":"legacy-opportunity"}`,
		"opportunities.jsonl":          `{"type":"ignored","key":"legacy-opportunity"}` + "\n",
	}
	for name, body := range legacy {
		if err := os.WriteFile(filepath.Join(legacyDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	srv.installProposalEngine()
	srv.installOpportunityEngine()
	if srv.tradeProposals.snapshot.Kind != "" || srv.opportunities.snapshot.Kind != "" {
		t.Fatal("constructors read proposal/opportunity state before SQLite attach")
	}

	core := openProposalOpportunityTestCore(t, filepath.Join(stateHome, "authority", "daemon.db"))
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
		t.Fatalf("initialize clean proposal/opportunity authority: %v", err)
	}
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
		t.Fatalf("validate retried clean proposal/opportunity authority: %v", err)
	}
	if err := srv.attachProposalOpportunityAuthority(t.Context(), core); err != nil {
		t.Fatalf("attach proposal/opportunity authority: %v", err)
	}
	proposal := srv.tradeProposals.Snapshot(false)
	if proposal.Revision != "empty" || len(proposal.Proposals) != 0 || !proposal.LoadedFromState {
		t.Fatalf("proposal cutover snapshot = %+v, want loaded empty epoch", proposal)
	}
	opportunity := srv.opportunities.Snapshot(false)
	if opportunity.Revision != "empty" || len(opportunity.Opportunities) != 0 || !opportunity.LoadedFromState {
		t.Fatalf("opportunity cutover snapshot = %+v, want loaded empty epoch", opportunity)
	}
	if events, err := loadProposalEvents(t.Context(), core); err != nil || len(events) != 0 {
		t.Fatalf("proposal cutover events = %+v, err=%v; want empty", events, err)
	}
	if events, err := loadOpportunityEvents(t.Context(), core); err != nil || len(events) != 0 {
		t.Fatalf("opportunity cutover events = %+v, err=%v; want empty", events, err)
	}
	proposalCurrent := scopedTestSnapshot("DU1234567", rpc.AccountModePaper, now.Add(time.Minute))
	if err := srv.tradeProposals.store.SaveCurrentWithEvents(t.Context(), proposalCurrent, []proposalEvent{{At: now.Add(time.Minute), Type: "generated", Key: proposalCurrent.Proposals[0].Key, Revision: proposalCurrent.Revision, AccountID: proposalCurrent.AccountID}}); err != nil {
		t.Fatalf("persist proposal without legacy mirror: %v", err)
	}
	opportunityCurrent := persistedOpportunitySnapshot(now.Add(time.Minute), "DU1234567", rpc.AccountModePaper, "sha256:opportunity-cutover")
	if err := srv.opportunities.store.SaveCurrentWithEvents(t.Context(), opportunityCurrent, []opportunityEvent{{At: now.Add(time.Minute), Type: "shown", Key: opportunityCurrent.Opportunities[0].Key, Revision: opportunityCurrent.Revision, AccountID: opportunityCurrent.AccountID}}); err != nil {
		t.Fatalf("persist opportunity without legacy mirror: %v", err)
	}
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err == nil {
		t.Fatal("clean initializer accepted an already-live proposal/opportunity epoch")
	}
	for name, want := range legacy {
		got, err := os.ReadFile(filepath.Join(legacyDir, name))
		if err != nil {
			t.Fatalf("read legacy sentinel %s: %v", name, err)
		}
		if string(got) != want {
			t.Fatalf("legacy sentinel %s was mirrored or rewritten: got %q want %q", name, got, want)
		}
	}
}

func TestProposalOpportunityRestartContinuityUsesSQLiteOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "authority", "daemon.db")
	core := openProposalOpportunityTestCore(t, path)
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
		t.Fatal(err)
	}

	proposalStore := &proposalStore{}
	if _, _, err := proposalStore.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	opportunityStore := &opportunityStore{}
	if _, _, err := opportunityStore.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC)
	proposal := scopedTestSnapshot("DU1234567", rpc.AccountModePaper, now)
	ignoredProposalEvent := proposalEvent{At: now, Type: "ignored", Key: proposal.Proposals[0].Key, Revision: proposal.Revision, AccountID: proposal.AccountID, Message: "proposal ignored"}
	if err := proposalStore.SaveCurrentWithEvents(t.Context(), proposal, []proposalEvent{ignoredProposalEvent}); err != nil {
		t.Fatalf("persist proposal current and event: %v", err)
	}
	opportunity := persistedOpportunitySnapshot(now, "DU1234567", rpc.AccountModePaper, "sha256:opportunity-1")
	ignoredOpportunityEvent := opportunityEvent{At: now, Type: "ignored", Key: opportunity.Opportunities[0].Key, Revision: opportunity.Revision, AccountID: opportunity.AccountID, Message: "opportunity ignored"}
	if err := opportunityStore.SaveCurrentWithEvents(t.Context(), opportunity, []opportunityEvent{ignoredOpportunityEvent}); err != nil {
		t.Fatalf("persist opportunity current and event: %v", err)
	}
	if err := proposalStore.AppendEvent(proposalEvent{At: now.Add(time.Second), Type: "submitted", Key: proposal.Proposals[0].Key, Revision: proposal.Revision, AccountID: proposal.AccountID, OrderRef: "redacted-order", PreviewTokenID: "redacted-token"}); err != nil {
		t.Fatalf("persist submitted proposal event: %v", err)
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := openProposalOpportunityTestCore(t, path)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now.Add(time.Minute))
	srv.installProposalEngine()
	srv.installOpportunityEngine()
	if err := srv.attachProposalOpportunityAuthority(t.Context(), reopened); err != nil {
		t.Fatalf("attach reopened authority: %v", err)
	}
	srv.tradeProposals.scope = func() brokerStateScope {
		return brokerStateScope{Account: proposal.AccountID, Mode: proposal.AccountMode}
	}
	loadedProposal := srv.tradeProposals.Snapshot(false)
	if !loadedProposal.LoadedFromState || loadedProposal.Revision != proposal.Revision || len(loadedProposal.Proposals) != 1 {
		t.Fatalf("restarted proposal snapshot = %+v", loadedProposal)
	}
	if !srv.tradeProposals.isIgnored(brokerStateScope{Account: proposal.AccountID, Mode: proposal.AccountMode}, proposal.Proposals[0].Key) {
		t.Fatal("restarted proposal engine did not restore SQLite ignored event")
	}
	if _, ok, err := srv.tradeProposals.store.FindSubmittedEvent("redacted-order", ""); err != nil || !ok {
		t.Fatalf("find restarted submitted event: ok=%v err=%v", ok, err)
	}
	loadedOpportunity := srv.opportunities.Snapshot(false)
	if !loadedOpportunity.LoadedFromState || loadedOpportunity.Revision != opportunity.Revision || len(loadedOpportunity.Opportunities) != 1 {
		t.Fatalf("restarted opportunity snapshot = %+v", loadedOpportunity)
	}
	if !srv.opportunities.isIgnored(brokerStateScope{Account: opportunity.AccountID, Mode: opportunity.AccountMode}, opportunity.Opportunities[0].Key) {
		t.Fatal("restarted opportunity engine did not restore SQLite ignored event")
	}
}

func TestProposalCurrentAndEventsRollbackTogether(t *testing.T) {
	core := openProposalOpportunityTestCore(t, filepath.Join(t.TempDir(), "authority", "daemon.db"))
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	first, stale := &proposalStore{}, &proposalStore{}
	if _, _, err := first.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stale.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC)
	one := scopedTestSnapshot("DU1234567", rpc.AccountModePaper, now)
	one.Revision = "sha256:first"
	one.Proposals[0].Revision = one.Revision
	if err := first.SaveCurrentWithEvents(t.Context(), one, []proposalEvent{{At: now, Type: "generated", Key: one.Proposals[0].Key, Revision: one.Revision, AccountID: one.AccountID}}); err != nil {
		t.Fatal(err)
	}
	two := cloneProposalSnapshot(one)
	two.AsOf = now.Add(time.Minute)
	two.Revision = "sha256:stale"
	two.Proposals[0].Revision = two.Revision
	err := stale.SaveCurrentWithEvents(t.Context(), two, []proposalEvent{{At: two.AsOf, Type: "generated", Key: two.Proposals[0].Key, Revision: two.Revision, AccountID: two.AccountID}})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale current+event write error = %v, want revision conflict", err)
	}
	doc, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, proposalStateKind)
	if err != nil || !ok {
		t.Fatalf("load proposal current: ok=%v err=%v", ok, err)
	}
	var persisted rpc.TradeProposalSnapshot
	if err := json.Unmarshal(doc.JSON, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != one.Revision {
		t.Fatalf("current revision = %q, want %q after rolled-back conflict", persisted.Revision, one.Revision)
	}
	events, err := loadProposalEvents(t.Context(), core)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Revision != one.Revision {
		t.Fatalf("events after rolled-back conflict = %+v", events)
	}

	firstOpportunity, staleOpportunity := &opportunityStore{}, &opportunityStore{}
	if _, _, err := firstOpportunity.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if _, _, err := staleOpportunity.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	opportunityOne := persistedOpportunitySnapshot(now, "DU1234567", rpc.AccountModePaper, "sha256:opportunity-first")
	if err := firstOpportunity.SaveCurrentWithEvents(t.Context(), opportunityOne, []opportunityEvent{{At: now, Type: "shown", Key: opportunityOne.Opportunities[0].Key, Revision: opportunityOne.Revision, AccountID: opportunityOne.AccountID}}); err != nil {
		t.Fatal(err)
	}
	opportunityTwo := cloneOpportunitySnapshot(opportunityOne)
	opportunityTwo.AsOf = now.Add(time.Minute)
	opportunityTwo.Revision = "sha256:opportunity-stale"
	opportunityTwo.Opportunities[0].Revision = opportunityTwo.Revision
	err = staleOpportunity.SaveCurrentWithEvents(t.Context(), opportunityTwo, []opportunityEvent{{At: opportunityTwo.AsOf, Type: "shown", Key: opportunityTwo.Opportunities[0].Key, Revision: opportunityTwo.Revision, AccountID: opportunityTwo.AccountID}})
	if !errors.Is(err, corestore.ErrRevisionConflict) {
		t.Fatalf("stale opportunity current+event write error = %v, want revision conflict", err)
	}
	opportunityDoc, ok, err := core.GetStateDocument(t.Context(), daemonStateScope, opportunityStateKind)
	if err != nil || !ok {
		t.Fatalf("load opportunity current: ok=%v err=%v", ok, err)
	}
	var persistedOpportunity rpc.OpportunitySnapshot
	if err := json.Unmarshal(opportunityDoc.JSON, &persistedOpportunity); err != nil {
		t.Fatal(err)
	}
	if persistedOpportunity.Revision != opportunityOne.Revision {
		t.Fatalf("opportunity current revision = %q, want %q after rolled-back conflict", persistedOpportunity.Revision, opportunityOne.Revision)
	}
	opportunityEvents, err := loadOpportunityEvents(t.Context(), core)
	if err != nil {
		t.Fatal(err)
	}
	if len(opportunityEvents) != 1 || opportunityEvents[0].Revision != opportunityOne.Revision {
		t.Fatalf("opportunity events after rolled-back conflict = %+v", opportunityEvents)
	}
}

func TestProposalOpportunityWritesFailClosedWhenAuthorityFails(t *testing.T) {
	core := openProposalOpportunityTestCore(t, filepath.Join(t.TempDir(), "authority", "daemon.db"))
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	proposalStore, opportunityStore := &proposalStore{}, &opportunityStore{}
	if _, _, err := proposalStore.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if _, _, err := opportunityStore.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 20, 11, 0, 0, 0, time.UTC)
	proposalOld := scopedTestSnapshot("DU1234567", rpc.AccountModePaper, now)
	proposalEngine := &proposalEngine{
		store: proposalStore, now: func() time.Time { return now }, ignored: map[string]struct{}{}, snapshot: proposalOld,
		scope: func() brokerStateScope {
			return brokerStateScope{Account: proposalOld.AccountID, Mode: proposalOld.AccountMode}
		},
	}
	opportunityOld := persistedOpportunitySnapshot(now, "DU1234567", rpc.AccountModePaper, "sha256:opportunity-old")
	opportunityEngine := &opportunityEngine{
		server: &Server{endpoint: discover.Endpoint{Port: 7497, Account: opportunityOld.AccountID}},
		store:  opportunityStore, now: func() time.Time { return now }, ignored: map[string]struct{}{}, snapshot: opportunityOld,
	}
	if err := core.Close(); err != nil {
		t.Fatal(err)
	}
	proposalNew := cloneProposalSnapshot(proposalOld)
	proposalNew.Revision = "sha256:proposal-new"
	proposalNew.Proposals[0].Revision = proposalNew.Revision
	if err := proposalEngine.installSnapshot(proposalNew, false); err == nil {
		t.Fatal("proposal install succeeded against closed SQLite authority")
	}
	if got := proposalEngine.Snapshot(false).Revision; got != proposalOld.Revision {
		t.Fatalf("proposal cache changed after failed write: got %q want %q", got, proposalOld.Revision)
	}
	if result := proposalEngine.Ignore(rpc.TradeProposalIgnoreParams{Key: "proposal-failed-ignore"}); result.Accepted {
		t.Fatalf("proposal ignore accepted after failed persistence: %+v", result)
	}
	if proposalEngine.isIgnored(brokerStateScope{Account: proposalOld.AccountID, Mode: proposalOld.AccountMode}, "proposal-failed-ignore") {
		t.Fatal("proposal ignore mutated memory after failed persistence")
	}
	opportunityNew := cloneOpportunitySnapshot(opportunityOld)
	opportunityNew.Revision = "sha256:opportunity-new"
	opportunityNew.Opportunities[0].Revision = opportunityNew.Revision
	if err := opportunityEngine.installSnapshot(opportunityNew, false); err == nil {
		t.Fatal("opportunity install succeeded against closed SQLite authority")
	}
	if got := opportunityEngine.Snapshot(false).Revision; got != opportunityOld.Revision {
		t.Fatalf("opportunity cache changed after failed write: got %q want %q", got, opportunityOld.Revision)
	}
	if result := opportunityEngine.Ignore(rpc.OpportunityIgnoreParams{Key: "opportunity-failed-ignore"}); result.Accepted {
		t.Fatalf("opportunity ignore accepted after failed persistence: %+v", result)
	}
	if opportunityEngine.isIgnored(brokerStateScope{Account: opportunityOld.AccountID, Mode: opportunityOld.AccountMode}, "opportunity-failed-ignore") {
		t.Fatal("opportunity ignore mutated memory after failed persistence")
	}
}

func TestProposalOpportunityAttachRejectsMalformedRows(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	t.Run("missing current document", func(t *testing.T) {
		core := openProposalOpportunityTestCore(t, filepath.Join(t.TempDir(), "authority", "daemon.db"))
		store := &proposalStore{}
		if _, _, err := store.bindCore(t.Context(), core); err == nil {
			t.Fatal("missing proposal current row was accepted")
		}
	})
	t.Run("current document", func(t *testing.T) {
		core := openProposalOpportunityTestCore(t, filepath.Join(t.TempDir(), "authority", "daemon.db"))
		if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
			t.Fatal(err)
		}
		if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: proposalStateKind, ExpectedRevision: 1,
			JSON: []byte(`{"kind":"wrong","schema_version":"wrong","as_of":"2026-07-20T12:00:00Z","revision":"sha256:x"}`),
		}); err != nil {
			t.Fatal(err)
		}
		store := &proposalStore{}
		if _, _, err := store.bindCore(t.Context(), core); err == nil {
			t.Fatal("malformed proposal current row was accepted")
		}
	})
	t.Run("proposal event", func(t *testing.T) {
		core := openProposalOpportunityTestCore(t, filepath.Join(t.TempDir(), "authority", "daemon.db"))
		if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
			t.Fatal(err)
		}
		raw := []byte(`{"version":99,"at":"2026-07-20T12:00:00Z","type":"ignored","key":"bad"}`)
		if _, err := core.AppendEvents(t.Context(), []corestore.EventInput{{
			ScopeKey: daemonStateScope, EventKey: coreEventKey("bad-proposal", now, raw, 0), Type: proposalCoreEventType,
			Action: coreEventActionRecord, Origin: coreEventOriginDaemon, OccurredAt: now, PayloadJSON: raw,
		}}); err != nil {
			t.Fatal(err)
		}
		store := &proposalStore{}
		if _, _, err := store.bindCore(t.Context(), core); err == nil {
			t.Fatal("malformed proposal event row was accepted")
		}
	})
	t.Run("opportunity event", func(t *testing.T) {
		core := openProposalOpportunityTestCore(t, filepath.Join(t.TempDir(), "authority", "daemon.db"))
		if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
			t.Fatal(err)
		}
		raw := []byte(`{"version":1,"at":"0001-01-01T00:00:00Z","type":"ignored","key":"bad"}`)
		if _, err := core.AppendEvents(t.Context(), []corestore.EventInput{{
			ScopeKey: daemonStateScope, EventKey: coreEventKey("bad-opportunity", now, raw, 0), Type: opportunityCoreEventType,
			Action: coreEventActionRecord, Origin: coreEventOriginDaemon, OccurredAt: now, PayloadJSON: raw,
		}}); err != nil {
			t.Fatal(err)
		}
		store := &opportunityStore{}
		if _, _, err := store.bindCore(t.Context(), core); err == nil {
			t.Fatal("malformed opportunity event row was accepted")
		}
	})
}

func openProposalOpportunityTestCore(t *testing.T, path string) *corestore.Store {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatalf("open proposal/opportunity test authority: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func persistedOpportunitySnapshot(at time.Time, account, mode, revision string) rpc.OpportunitySnapshot {
	return rpc.OpportunitySnapshot{
		Kind:          rpc.OpportunitySnapshotKind,
		SchemaVersion: rpc.OpportunitySnapshotSchemaVersion,
		AsOf:          at,
		Revision:      revision,
		AccountID:     account,
		AccountMode:   mode,
		Opportunities: []rpc.Opportunity{{Key: "option_exercise:test", Revision: revision}},
	}
}
