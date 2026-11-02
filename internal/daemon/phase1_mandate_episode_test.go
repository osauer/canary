package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestMandateHoldRemembersBreachWithoutAuthorityPolling(t *testing.T) {
	s := phase1MandateEpisodeServer(t)
	s.mandateBreachForTest = nil
	e := &proposalEngine{server: s, store: &proposalStore{core: s.coreStore}, scope: func() brokerStateScope { return testLiveObserveScope }}
	s.tradeProposals = e
	snapshot := func(revision, state, episode string) rpc.TradeProposalSnapshot {
		return rpc.TradeProposalSnapshot{
			Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion,
			AsOf: cashPolicyTestNow, Revision: revision, AccountID: testLiveObserveScope.Account, AccountMode: testLiveObserveScope.Mode,
			MandateReduction: &rpc.TradeProposalMandateStatus{State: state, Episode: episode},
		}
	}
	// The producer opens and closes an episode while Desk is offline. No
	// authority status read occurs between these committed observations.
	if err := e.installSnapshot(snapshot("breach", rpc.MandateReductionNothingReducible, "synthetic-breach"), false); err != nil {
		t.Fatal(err)
	}
	if err := e.installSnapshot(snapshot("within", rpc.MandateReductionWithin, ""), false); err != nil {
		t.Fatal(err)
	}
	s.mandateEpisode = mandateEpisodeClock{}
	phase1RequireMandateHold(t, s)
}

func TestMandateHoldRetriesUncommittedEpisode(t *testing.T) {
	s := phase1MandateEpisodeServer(t)
	state, episode := rpc.MandateReductionWithin, ""
	s.mandateBreachForTest = func() (string, string) { return state, episode }
	if _, _, known := s.deskAuthorityHold(t.Context()); !known {
		t.Fatal("initial mandate should be known")
	}
	state, episode = rpc.MandateReductionNothingReducible, "synthetic-breach"
	interrupted, cancel := context.WithCancel(t.Context())
	cancel()
	_, engaged, known := s.deskAuthorityHold(interrupted)
	if known || !engaged {
		t.Errorf("an uncommitted breach must remain engaged and unconfirmed: engaged=%v known=%v", engaged, known)
	}
	// Persistence becomes available after the episode ends. A healthy read
	// must finish the pending receipt, not treat in-memory state as durable.
	state, episode = rpc.MandateReductionWithin, ""
	phase1RequireMandateHold(t, s)
	s.mandateEpisode = mandateEpisodeClock{}
	phase1RequireMandateHold(t, s)
}

func phase1MandateEpisodeServer(t *testing.T) *Server {
	t.Helper()
	s, _, _ := cashPolicyServer(t, cashPolicyTestFile)
	s.startedAt = cashPolicyTestNow
	head, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("synthetic private controller capability"))
	terms := rpc.DeskAuthorityTerms{
		AuthorityEpoch: head.AuthorityEpoch, Version: 1, Kind: "desk_automatic_authority", ID: "synthetic-episode-authority",
		AccountID: testLiveObserveScope.Account, AccountMode: testLiveObserveScope.Mode, MaximumScope: "full",
		ControllerHash: hex.EncodeToString(hash[:]), ConfirmBefore: cashPolicyTestNow.Add(time.Minute),
		Pricing: "fixed-midpoint", Algorithm: "Adaptive", TIF: "DAY", Protection: "existing-policy",
		Capacity: "existing-policy-reuse-confirmed-funds", Selection: "desk-deterministic-controller",
	}
	row := deskAuthorityRecord{Terms: &terms, Scope: "full", Running: true, PreferencesHash: strings.Repeat("a", 64), AdaptivePriority: "Normal", ControlProcess: s.startedAt}
	if _, err := s.saveDeskAuthority(t.Context(), 0, row, "synthetic-confirmed"); err != nil {
		t.Fatal(err)
	}
	return s
}

func phase1RequireMandateHold(t *testing.T, s *Server) {
	t.Helper()
	got, err := s.handleDeskAuthorityStatus(t.Context())
	if err != nil || got.Scope != "protect" || got.HeldBy != rpc.DeskAuthorityHeldByMandate {
		t.Fatalf("a completed breach restored full authority without owner confirmation: scope=%q held_by=%q error=%v", got.Scope, got.HeldBy, err)
	}
	fence := deskAuthorityFence{ID: "synthetic-episode-authority", Generation: 1, PreferencesHash: strings.Repeat("a", 64), AdaptivePriority: "Normal", Action: "entry", ValidUntil: cashPolicyTestNow.Add(time.Minute)}
	release, err := s.lockDeskAuthorityForWire(t.Context(), &fence, rpc.TradingStatus{Account: testLiveObserveScope.Account, Mode: testLiveObserveScope.Mode})
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("entry passed the final authority fence after an unconfirmed breach")
	}
}
