package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	edgecore "github.com/osauer/canary/v3/internal/edge"
	"github.com/osauer/canary/v3/internal/flexstmt"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestEdgeProtectionRequiresUniquePriorSubmittedEvidence(t *testing.T) {
	at := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "U", Mode: "paper"}
	statements := []flexstmt.Statement{{AccountID: "U", Trades: []flexstmt.Trade{{RecordID: "trade", AccountID: "U", ConID: 123, ExecutionID: "exec", ExecutedAt: at}}}}
	event := orderJournalEvent{Account: "U", Mode: "paper", ConID: 123, ExecID: "exec", OrderRef: "order", PreviewTokenID: "token", At: at}
	proposal := proposalEvent{Type: "submitted", AccountID: "U", AccountMode: "paper", OrderRef: "order", PreviewTokenID: "token", Bucket: "risk_reduction", Key: "proposal", Revision: "revision", PolicyID: "policy", PolicyVersion: 1, PolicyFingerprint: rpc.Fingerprint{Version: "1", Key: "fingerprint"}, At: at.Add(-time.Second)}
	for _, kind := range []string{"exact", "future", "conflicting_policy", "conflicting_token", "wrong_account", "wrong_contract", "source_only", "missing_fingerprint"} {
		t.Run(kind, func(t *testing.T) {
			events := []orderJournalEvent{event}
			proposals := []proposalEvent{proposal}
			switch kind {
			case "future":
				proposals[0].At = at.Add(time.Second)
			case "conflicting_policy":
				p := proposal
				p.PolicyID = "other"
				proposals = append(proposals, p)
			case "conflicting_token":
				p := proposal
				p.PreviewTokenID = "other"
				proposals = append(proposals, p)
			case "wrong_account":
				events[0].Account = "OTHER"
			case "wrong_contract":
				events[0].ConID = 999
			case "source_only":
				proposals = nil
				events[0].Source = "risk_reduction"
			case "missing_fingerprint":
				proposals[0].PolicyFingerprint = rpc.Fingerprint{}
			}
			got := matchEdgeProtectionRecords(scope, statements, events, proposals)
			if (len(got) == 1) != (kind == "exact") {
				t.Fatalf("%s linked=%d", kind, len(got))
			}
		})
	}
}

func TestEdgePublicationWithholdsChangedProtectionWithoutChangingPrices(t *testing.T) {
	const account = "UEDGEFIXTURE"
	now := time.Date(2026, 8, 24, 23, 0, 0, 0, time.UTC)
	store, statements, evidence := projectEdgeAcceptanceFixture(t, "edge-acceptance-365.xml", account, now)
	records := map[string]edgecore.ProtectionRecord{}
	for _, st := range statements {
		for _, tr := range st.Trades {
			records[tr.RecordID] = edgecore.ProtectionRecord{Identity: "same", Bucket: "risk_reduction", At: now.AddDate(0, 0, -365)}
		}
	}
	core, err := edgecore.Analyze(edgecore.Input{AsOf: now, WindowDays: 365, BaseCurrency: "USD", Statements: statements, Bars: edgeAcceptanceBars(), ProtectionRecords: records})
	if err != nil {
		t.Fatal(err)
	}
	// A completed position is injected only to exercise publication transport;
	// its reconstruction is separately proved by the option-cycle core tests.
	core.Options.Cycles = edgecore.OptionCycles{CompletedCount: 1, CompletePNLCount: 1, KnownPNLBase: new(float64(90)), Cycles: []edgecore.OptionCycle{{ID: "option-cycle_fixture", Symbol: "SYN", Direction: "long", OpenedAt: now.AddDate(0, 0, -2), ClosedAt: now.AddDate(0, 0, -1), ClosedBy: "trade", ExecutionCount: 2, RealizedPNLBase: new(float64(90)), PNLStatus: "complete", LinkedProtectionCount: 2}}}
	port := 4001
	srv := &Server{cfg: &config.Resolved{Gateway: config.Gateway{Account: account, Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "424242"}}, coreStore: store, now: func() time.Time { return now }}
	scope := srv.currentBrokerStateScope()
	_, _, fp := srv.edgeProtectionEvidence(t.Context(), scope)
	pub := edgePublication{ScopeFingerprint: edgeScopeFingerprint(scope), EvidenceFingerprint: evidence, State: rpc.EdgeStateCurrent, Windows: map[string]edgecore.Result{"365d": core}, LocalContextFingerprint: fp, LocalContextAsOf: now, UpdatedAt: now}
	change := acceptanceChange(t, core.Changes, "APEX", edgecore.ActionAdd)
	read := func(want string) *rpc.EdgeResult {
		t.Helper()
		got, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: []byte(`{"window":"365d","horizon_sessions":20,"change_id":"` + change.ID + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		if got.ProtectionState != want {
			t.Fatalf("state=%s want %s", got.ProtectionState, want)
		}
		linked := 0
		for _, p := range got.Patterns {
			for _, h := range p.Horizons {
				linked += h.LinkedProtectionCount + h.PartialProtectionCount
			}
		}
		if want == "current" {
			if linked == 0 || got.Change.ProtectionContext.Status != "linked" || got.Options.Cycles.Cycles[0].LinkedProtectionCount != 2 {
				t.Fatal("current provenance lost")
			}
		} else if linked != 0 || got.Change.ProtectionContext.Status != "unavailable" || got.Options.Cycles.Cycles[0].LinkedProtectionCount != 0 {
			t.Fatal("stale provenance escaped")
		}
		return got
	}
	if err := srv.saveEdgePublication(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	before := read("current")
	ps := &proposalStore{core: store}
	if err := ps.appendEvent(t.Context(), proposalEvent{Version: proposalEventFileVersion, Type: "submitted", At: now, AccountID: account, AccountMode: scope.Mode, Key: "changed", OrderRef: "synthetic", PreviewTokenID: "different"}); err != nil {
		t.Fatal(err)
	}
	after := read("changed")
	if after.Fingerprint != before.Fingerprint || after.Headline != before.Headline || after.Account.ProfitLossBase != before.Account.ProfitLossBase || *after.Options.Cycles.KnownPNLBase != 90 {
		t.Fatal("context change altered broker financial evidence")
	}
	pub.LocalContextFingerprint = ""
	if err := srv.saveEdgePublication(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	read("unavailable")
	_, _, pub.LocalContextFingerprint = srv.edgeProtectionEvidence(t.Context(), scope)
	if err := srv.saveEdgePublication(t.Context(), pub); err != nil {
		t.Fatal(err)
	}
	read("current")
}
