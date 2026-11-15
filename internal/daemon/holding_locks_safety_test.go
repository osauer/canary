package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestHoldingLocksAcknowledgementWaitsForBrokerTransaction(t *testing.T) {
	s, _, _ := watchlistServer(t)
	s.brokerWriteMu.Lock()
	released := false
	defer func() {
		if !released {
			s.brokerWriteMu.Unlock()
		}
	}()
	started, done := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		_, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{ConID: 9001, Symbol: "SYNA", Role: rpc.HoldingLockRoleCore}}, 0, "lock-during-basket")
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		t.Fatalf("lock was acknowledged during an in-flight broker transaction: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	s.brokerWriteMu.Unlock()
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock did not complete after the broker transaction ended")
	}
	locks, err := s.holdingLocks(t.Context())
	if _, locked := locks.Match("SYNA", 9001); err != nil || !locked {
		t.Fatalf("accepted lock missing: %v", err)
	}
}

func TestMandateReductionHoldsWhenHoldingLocksCannotBeRead(t *testing.T) {
	s, core, _ := watchlistServer(t)
	s.riskPolicies = newRiskPolicyManager("", time.Minute, nil)
	s.riskPolicies.active = testMandateConstitution()
	pos := mandateReductionBook(mandateReductionStock("SYNA", 9001, 3000, 100))
	fixture := mandateReductionFixture(t, pos, 1.5, 1000000, mandateReductionOverReadings)
	e := &proposalEngine{server: s}
	run := func(previous []rpc.TradeProposal) ([]rpc.TradeProposal, *rpc.TradeProposalMandateStatus) {
		in := e.mandateReductionInput(t.Context(), defaultProtectionPolicy(), pos, brokerStateScope{})
		in.Result, in.Input, in.EvaluatedAt, in.OverStreak = fixture.Result, fixture.Input, fixture.EvaluatedAt, fixture.OverStreak
		in.Previous = previous
		return e.mandateReductionProposals(defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, in, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, mandateTestNow)
	}
	prior, _ := run(nil)
	if len(prior) == 0 {
		t.Fatal("precondition: healthy empty lock list must permit a reduction")
	}
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindHoldingLocks, JSON: []byte(`{"version":1,"locks":null}`)}); err != nil {
		t.Fatal(err)
	}
	if !core.Health().Ready {
		t.Fatal("fixture must preserve store health; only the lock document is invalid")
	}
	fresh, status := run(nil)
	if len(fresh) != 0 || status == nil || !strings.Contains(status.Reason, "locked holdings") {
		t.Fatalf("unreadable locks selected %d new reductions: %+v", len(fresh), status)
	}
	held, status := run(prior)
	if len(held) != len(prior) || status == nil || !status.Held {
		t.Fatalf("standing batch was not held: rows=%d status=%+v", len(held), status)
	}
	for _, p := range held {
		if p.Mandate == nil || !p.Mandate.Held {
			t.Fatal("standing row remains executable")
		}
	}
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindHoldingLocks, ExpectedRevision: 1, JSON: []byte(`{"version":1,"locks":[]}`)}); err != nil {
		t.Fatal(err)
	}
	recovered, status := run(prior)
	if len(recovered) == 0 || status.Held {
		t.Fatalf("valid empty list did not restore evaluation: %+v", status)
	}
}
