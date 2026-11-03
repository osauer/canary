package daemon

import (
	"encoding/json"
	"github.com/osauer/canary/v2/internal/rpc"
	"strings"
	"testing"
	"time"
)

// Synthetic, continuous same-session P&L. A suspicion already exists before
// the near-zero reading, so admission's net-move filter is not involved.
func TestMandateHeldFlowKeepsSecondDepositAfterPartialDeclaration(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return mandateTestNow }
	c := testMandateConstitution()
	reconcileNow(t, st)
	observeWithPnL(st, c, 300000, 20000, mandateTestNow.Add(-5*time.Minute))
	// First real deposit; unchanged same-session P&L.
	observeWithPnL(st, c, 320000, 20000, mandateTestNow.Add(-4*time.Minute))
	if st.state.Mandate.FlowSuspect == nil {
		t.Fatal("setup: initial deposit did not raise suspicion")
	}
	t.Logf("first deposit: suspect=%+v", st.state.Mandate.FlowSuspect)
	// Second real +20000 deposit and -19500 return: equity rises only 500;
	// continuous session P&L falls from +20000 to +500. No reset occurred.
	observeWithPnL(st, c, 320500, 500, mandateTestNow.Add(-3*time.Minute))
	if st.state.Mandate.FlowSuspect == nil {
		t.Fatal("setup: suspicion vanished before declaration")
	}
	t.Logf("second deposit plus loss: suspect=%+v; actual cumulative undeclared flow=40000", st.state.Mandate.FlowSuspect)
	// Explain only the FIRST +20000 deposit. The second +20000 remains unknown.
	if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "deposit", AmountBase: 20000}, rpc.OrderOriginHumanTTY, c); err != nil {
		t.Fatal(err)
	}
	observeWithPnL(st, c, 320500, 500, mandateTestNow.Add(-time.Minute))
	capital, _ := st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow)
	t.Logf("after declaring first deposit only: capital=%+v suspect=%+v; actual unresolved flow=20000", capital, st.state.Mandate.FlowSuspect)
	if capital.Known || st.state.Mandate.FlowSuspect == nil {
		t.Fatal("near-zero reset inference cleared a real second deposit after only the first was declared")
	}
	if st.state.Mandate.FlowSuspect.Unexplained != 20000 {
		t.Fatalf("expected remaining +20000 deposit, got %+v", st.state.Mandate.FlowSuspect)
	}
	// Neither interpretation is proven: even declaring both deposits cannot
	// resolve the P&L basis. Persist the ambiguity through a state round trip.
	if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "deposit", AmountBase: 20000}, rpc.OrderOriginHumanTTY, c); err != nil {
		t.Fatal(err)
	}
	observeWithPnL(st, c, 320500, 500, mandateTestNow)
	raw, err := json.Marshal(st.state.Mandate)
	if err != nil {
		t.Fatal(err)
	}
	var restored mandateCapitalStateV1
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	st.state.Mandate = &restored
	observeWithPnL(st, c, 320500, 500, mandateTestNow)
	if capital, _ := st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow); capital.Known || st.state.Mandate.FlowSuspect == nil || !strings.Contains(strings.Join(capital.Reasons, " "), "flow declarations alone cannot settle") {
		t.Fatalf("ambiguity lost or not explained: %+v", capital)
	}
	st.state.StatementCoverageTo = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	observeWithPnL(st, c, 320500, 500, mandateTestNow)
	if capital, _ := st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow); !capital.Known || st.state.Mandate.FlowSuspect != nil {
		t.Fatalf("covering statement did not resolve ambiguity: %+v", capital)
	}
}

// The owner retained this admission tradeoff on 2026-10-19: an offsetting
// market move does not create a new hold when net equity stays flat (#109).
func TestMandateOffsettingFlowRetainsOwnerAdmissionDecision(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return mandateTestNow }
	c := testMandateConstitution()
	reconcileNow(t, st)
	observeWithPnL(st, c, 300000, 0, mandateTestNow.Add(-5*time.Minute))
	observeWithPnL(st, c, 300000, -20000, mandateTestNow.Add(-time.Minute))
	got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow)
	if !got.Known || st.state.Mandate.FlowSuspect != nil {
		t.Fatal("restored the declined residual-only admission rule")
	}
}

func TestMandateHeldFlowUsesConfirmedSessionReset(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return mandateTestNow }
	c := testMandateConstitution()
	reconcileNow(t, st)
	observeWithPnL(st, c, 300000, 20000, mandateTestNow.Add(-25*time.Hour))
	observeWithPnL(st, c, 320000, 20000, mandateTestNow.Add(-24*time.Hour))
	observeWithPnL(st, c, 320500, 500, mandateTestNow.Add(-time.Minute))
	if st.state.Mandate.FlowSuspect == nil || st.state.Mandate.FlowSuspect.Unexplained != 20000 {
		t.Fatalf("actual session reset changed flow: %+v", st.state.Mandate.FlowSuspect)
	}
	if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "deposit", AmountBase: 20000}, rpc.OrderOriginHumanTTY, c); err != nil {
		t.Fatal(err)
	}
	observeWithPnL(st, c, 320500, 500, mandateTestNow)
	if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow); !got.Known {
		t.Fatalf("declared flow after actual reset held: %+v", got)
	}
}

func TestMandateHeldDepositSurvivesActualBrokerReset(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 16, 23, 59, 59, 0, ny)
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return now }
	c := testMandateConstitution()
	reconcileNow(t, st)
	observeWithPnL(st, c, 300000, -20000, now.Add(-5*time.Minute))
	observeWithPnL(st, c, 320000, -20000, now.Add(-4*time.Minute))
	if st.state.Mandate.FlowSuspect == nil {
		t.Fatal("setup: missing real deposit hold")
	}
	// Actual broker P&L reset can precede session-date rollover. There is no
	// second flow or market gain, and no declaration or covering statement.
	observeWithPnL(st, c, 320000, 0, now)
	got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, now)
	t.Logf("actual reset while held: capital=%+v suspect=%+v", got, st.state.Mandate.FlowSuspect)
	if got.Known || st.state.Mandate.FlowSuspect == nil {
		t.Fatal("real held deposit cleared by actual same-session broker P&L reset")
	}
}

func TestMandateHeldAmbiguityPersistsAndRequiresLatestStatement(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	now := time.Date(2026, 10, 19, 15, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	c := testMandateConstitution()
	reconcileNow(t, st)
	friday := time.Date(2026, 10, 16, 15, 0, 0, 0, time.UTC)
	observeWithPnL(st, c, 300000, 0, friday.Add(-time.Minute))
	observeWithPnL(st, c, 320000, 0, friday)
	// Monday's real loss does not explain Friday's deposit.
	observeWithPnL(st, c, 300000, -20000, now.Add(-time.Minute))
	// Force persistence even inside the ordinary observation throttle.
	pnl := mandatePnLReading{DailyBase: new(500.0), Session: mandateSessionDate(now)}
	force := st.observeMandateLocked(c, 300500, pnl, now, 0, now)
	if !force || st.state.Mandate.FlowSuspect == nil || !st.state.Mandate.FlowSuspect.PnLResetAmbiguousAt.Equal(now) {
		t.Fatalf("new ambiguity not durably marked: force=%v suspect=%+v", force, st.state.Mandate.FlowSuspect)
	}
	// Repeated unchanged readings are not new ambiguity events.
	later := now.Add(time.Minute)
	if force := st.observeMandateLocked(c, 300500, pnl, later, 0, later); force || !st.state.Mandate.FlowSuspect.PnLResetAmbiguousAt.Equal(now) {
		t.Fatal("unchanged figure advanced ambiguity or forced persistence")
	}
	// Drift from a figure already within the band changes neither basis by
	// enough to hide a flow: no new ambiguity.
	later = now.Add(2 * time.Minute)
	pnl.DailyBase = new(0.0)
	if force := st.observeMandateLocked(c, 300500, pnl, later, 0, later); force || !st.state.Mandate.FlowSuspect.PnLResetAmbiguousAt.Equal(now) {
		t.Fatal("near-zero drift advanced ambiguity")
	}
	// A second reset from a figure beyond the band advances the required
	// evidence time.
	pnl.DailyBase = new(-5000.0)
	st.observeMandateLocked(c, 295500, pnl, later.Add(30*time.Second), 0, later.Add(30*time.Second))
	pnl.DailyBase = new(0.0)
	later = later.Add(time.Minute)
	if force := st.observeMandateLocked(c, 295500, pnl, later, 0, later); !force || !st.state.Mandate.FlowSuspect.PnLResetAmbiguousAt.Equal(later) {
		t.Fatal("advanced ambiguity was not forced to persist")
	}
	// Friday's statement covers the first flow, but not Monday's ambiguity.
	st.state.StatementCoverageTo = time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	st.observeMandateLocked(c, 300500, pnl, later.Add(time.Minute), 0, later.Add(time.Minute))
	if st.state.Mandate.FlowSuspect == nil {
		t.Fatal("old statement cleared a later ambiguous reading")
	}
	st.state.StatementCoverageTo = time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	if force := st.observeMandateLocked(c, 300500, pnl, later.Add(2*time.Minute), 0, later.Add(2*time.Minute)); !force || st.state.Mandate.FlowSuspect != nil {
		t.Fatal("covering statement did not clear and persist")
	}
}
