package daemon

import (
	"encoding/json"
	"testing"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func deskExecutionTestServer(t *testing.T) (*Server, brokerStateScope) {
	t.Helper()
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	s.cfg = &config.Resolved{Gateway: config.Gateway{Account: "DU1234567"}, Trading: config.Trading{Mode: "paper"}}
	scope, err := s.deskExecutionScope()
	if err != nil {
		t.Fatal(err)
	}
	return s, scope
}

func deskExecutionTestItem(request, episode, revision string) rpc.DeskExecutionItem {
	return rpc.DeskExecutionItem{RequestID: request, EpisodeID: episode, Intent: rpc.DeskExecutionIntent{Class: rpc.DeskClassReduce,
		Proposal: &rpc.DeskExecutionProposal{Key: "proposal-syn", Revision: revision}}}
}

func deskExecutionLookup(t *testing.T, s *Server, ids ...string) []rpc.DeskExecutionReceipt {
	t.Helper()
	raw, _ := json.Marshal(rpc.DeskExecutionLookupParams{RequestIDs: ids})
	got, err := s.handleDeskExecutionLookup(t.Context(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return got.Receipts
}

// The journal records a request before dispatch, answers retries with the
// same request, refuses a reused ID with another intent and a second request
// for a consumed episode, and lookup reads absent, unknown and final outcomes.
func TestDeskExecutionJournalDeduplicatesAndLooksUp(t *testing.T) {
	s, scope := deskExecutionTestServer(t)
	item := deskExecutionTestItem("req-1", "ep-1", "r1")

	if got := deskExecutionLookup(t, s, "req-1"); got[0].Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("before any request: %+v, want absent", got[0])
	}
	prior, refusal, err := s.recordDeskExecutionRequest(t.Context(), scope, item, "automatic")
	if err != nil || prior || refusal != nil {
		t.Fatalf("first record: prior=%v refusal=%+v err=%v", prior, refusal, err)
	}
	got := deskExecutionLookup(t, s, "req-1")[0]
	if got.Outcome != rpc.DeskOutcomeUnknown || got.Validate(item, true) != nil {
		t.Fatalf("recorded without an outcome: %+v, want a valid unknown", got)
	}
	if prior, refusal, err = s.recordDeskExecutionRequest(t.Context(), scope, item, "automatic"); err != nil || !prior || refusal != nil {
		t.Fatalf("retry: prior=%v refusal=%+v err=%v, want the prior request", prior, refusal, err)
	}
	changed := deskExecutionTestItem("req-1", "ep-1", "r2")
	if _, refusal, err = s.recordDeskExecutionRequest(t.Context(), scope, changed, "automatic"); err != nil || refusal == nil || refusal.Code != rpc.DeskBlockerRequestConflict {
		t.Fatalf("reused ID: %+v %v, want request_conflict", refusal, err)
	}
	second := deskExecutionTestItem("req-2", "ep-1", "r2")
	if _, refusal, err = s.recordDeskExecutionRequest(t.Context(), scope, second, "automatic"); err != nil || refusal == nil || refusal.Code != rpc.DeskBlockerEpisodeConsumed {
		t.Fatalf("consumed episode: %+v %v, want episode_consumed", refusal, err)
	}
	if got := deskExecutionLookup(t, s, "req-2")[0]; got.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("a refused-before-recording request: %+v, want absent", got)
	}

	if err := s.recordDeskExecutionOutcome(t.Context(), scope, "req-1", deskExecutionOutcomeRow{Outcome: rpc.DeskOutcomeUnknown, Message: "transport closed"}); err != nil {
		t.Fatal(err)
	}
	if err := s.recordDeskExecutionOutcome(t.Context(), scope, "req-1", deskExecutionOutcomeRow{Outcome: rpc.DeskOutcomeAccepted, OrderRef: "canary-syn-1"}); err != nil {
		t.Fatal(err)
	}
	got = deskExecutionLookup(t, s, "req-1")[0]
	if got.Outcome != rpc.DeskOutcomeAccepted || got.OrderRef != "canary-syn-1" || got.Validate(item, true) != nil {
		t.Fatalf("resolved: %+v, want accepted with the order", got)
	}
	if err := s.recordDeskExecutionOutcome(t.Context(), scope, "req-1", deskExecutionOutcomeRow{Outcome: rpc.DeskOutcomeAccepted, OrderRef: "canary-syn-2"}); err == nil {
		t.Fatal("a second accepted outcome was journaled")
	}

	// Another account sees none of it.
	s.cfg.Gateway.Account = "DU7654321"
	if got := deskExecutionLookup(t, s, "req-1")[0]; got.Outcome != rpc.DeskOutcomeAbsent {
		t.Fatalf("another account: %+v, want absent", got)
	}
}

// Without a journal, lookup fails; it never answers absent.
func TestDeskExecutionLookupFailsWithoutJournal(t *testing.T) {
	s, _ := deskExecutionTestServer(t)
	s.coreStore = nil
	raw, _ := json.Marshal(rpc.DeskExecutionLookupParams{RequestIDs: []string{"req-1"}})
	if got, err := s.handleDeskExecutionLookup(t.Context(), &rpc.Request{Params: raw}); err == nil {
		t.Fatalf("lookup without a journal: %+v, want an error", got)
	}
}

func TestDeskExecutionCapabilitiesListOnlyServedMethods(t *testing.T) {
	s, _ := deskExecutionTestServer(t)
	got := s.handleDeskExecutionCapabilities()
	if got.Version != rpc.DeskExecutionContractVersion || len(got.Methods) != 2 || len(got.Authorities) != 0 {
		t.Fatalf("capabilities %+v", got)
	}
}
