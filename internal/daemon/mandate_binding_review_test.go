package daemon

import (
	"context"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"

	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/rpc"
)

func TestMandateKeptInputRejectsChangedBrokerIdentity(t *testing.T) {
	for _, change := range []string{"connector epoch", "account"} {
		t.Run(change, func(t *testing.T) {
			s := indexLossServer(t, mandateReductionBook(mandateReductionStock("SPY", 9101, 100, 50)))
			if in, _, why := s.mandateCurrentInput(); in == nil {
				t.Fatal(why)
			}
			if change == "connector epoch" {
				// The real connector teardown advances publication authority even
				// when there is no live socket to stop in this synthetic fixture.
				s.stopConnector()
			} else {
				s.mu.Lock()
				s.endpoint.Account = "another-synthetic-account"
				s.mu.Unlock()
			}
			if in, _, why := s.mandateCurrentInput(); in != nil || why == "" {
				t.Fatalf("changed %s still serves old book: input=%t reason=%q", change, in != nil, why)
			}
		})
	}
}

func TestMandateEvaluationCannotPublishAcrossConnectorChange(t *testing.T) {
	s, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, s, 0)
	s.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		s.stopConnector()
		return mandateTestPositions(100), nil
	}
	res := s.evaluateMandate(t.Context(), false)
	if res.Verdict == nil || res.Verdict.Status != mandate.StatusUnknown || res.Verdict.Stresses != nil {
		t.Fatalf("evaluation crossing connector publication returned measurable verdict: %+v", res.Verdict)
	}
	if in, _, why := s.mandateCurrentInput(); in != nil || why == "" {
		t.Fatalf("old-session evaluation relabelled current: input=%t reason=%q", in != nil, why)
	}
}

func TestMandateBreachStreakBelongsToBrokerIdentity(t *testing.T) {
	for _, change := range []string{"account", "connector epoch", "portfolio generation"} {
		t.Run(change, func(t *testing.T) {
			var cache mandateVerdictCache
			binding := daemonBrokerEvidenceBinding{scope: brokerStateScope{Account: "synthetic", Mode: rpc.AccountModePaper}}
			res := &rpc.MandateRiskResult{Verdict: &mandate.Verdict{Status: mandate.StatusOver}}
			cache.set(res, &mandate.Input{}, mandateTestNow, "same-policy", true, binding)
			want := 1
			switch change {
			case "account":
				binding.scope.Account = "another-synthetic-account"
			case "connector epoch":
				binding.connectorEpoch++
			case "portfolio generation":
				binding.broker.PortfolioProjectionGeneration++
				want = 2
			}
			cache.set(res, &mandate.Input{}, mandateTestNow.Add(mandateStreakSpacing), "same-policy", true, binding)
			if got := cache.kept().overStreak; got != want {
				t.Fatalf("streak=%d after %s; want %d", got, change, want)
			}
		})
	}
}

func TestMandateOldEvaluationCannotReplaceSuccessorBook(t *testing.T) {
	s, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, s, 0)
	var successor *mandate.Input
	s.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		s.stopConnector()
		fresh := mandateReductionFixture(t, mandateReductionBook(mandateReductionStock("SPY", 9101, 100, 50)), 0.5, 1000000, 0)
		successor = fresh.Input
		s.mandateVerdicts.set(fresh.Result, successor, s.nowUTC(), s.riskPolicies.snapshot().policy.FingerprintKey(), false, s.captureMandateBookBinding())
		return mandateTestPositions(100), nil
	}
	res := s.evaluateMandate(t.Context(), false)
	if res.Verdict == nil || res.Verdict.Status != mandate.StatusUnknown {
		t.Fatal("old evaluation did not report changed identity")
	}
	if kept, _, why := s.mandateCurrentInput(); kept != successor {
		t.Fatalf("old evaluation displaced the successor book: %s", why)
	}
}

// The broker's first-byte guard already owns the publication and evidence
// barriers. A cache read must use the lock-safe identity accessors there.
func TestMandateConsumerReadDoesNotRelockBrokerEvidence(t *testing.T) {
	connector := &ibkrlib.Connector{}
	s := &Server{connector: connector}
	finished := make(chan struct{})
	go func() {
		connector.WithBrokerEvidenceMutation(func() { s.currentMandateVerdict() })
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("mandate consumer read deadlocked under the broker evidence barrier")
	}
}

func TestMandateConnectedBookBindingRejectsDifferentFrontier(t *testing.T) {
	broker := startFakeTWS(t)
	s, _ := sessionLossServer(t, broker, "2026-10-14T15:00:00Z")
	binding := s.captureMandateBookBinding()
	if binding.connector == nil || !binding.connector.SessionCurrent(binding.broker.Session) || !s.mandateBookBindingCurrent(binding) {
		t.Fatal("precondition: connected book must validate its captured identity")
	}
	changed := binding
	changed.broker.PortfolioProjectionGeneration++
	if s.mandateBookBindingCurrent(changed) {
		t.Fatal("different structural portfolio generation remained current")
	}
	changed = binding
	changed.broker.Session = ibkrlib.ConnectorSessionBinding{}
	if s.mandateBookBindingCurrent(changed) {
		t.Fatal("different socket session remained current")
	}
	// The same consumer validation must complete inside the real connected
	// Connector's evidence barrier, as it does at the first-byte guard.
	finished := make(chan bool, 1)
	go func() {
		binding.connector.WithBrokerEvidenceMutation(func() { finished <- s.mandateBookBindingCurrent(binding) })
	}()
	select {
	case current := <-finished:
		if !current {
			t.Fatal("barrier-owned current identity was rejected")
		}
	case <-time.After(time.Second):
		t.Fatal("connected binding read deadlocked under broker evidence barrier")
	}
	s.stopConnector()
	if s.mandateBookBindingCurrent(binding) {
		t.Fatal("retired physical connector still owns the kept book")
	}
}
