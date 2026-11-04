package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A covering statement clears a hold even when the first reading after it
// drifts near zero in the new broker day: the two P&L bases of such a
// reading differ by a previous figure inside the band, which cannot hide a
// flow. On 2026-11-03 06:57 UTC such drift moved the ambiguity past the
// 2026-11-02 statement that had just been reconciled, and every morning
// would have done the same to the statement of the day before.
func TestMandateHeldFlowNearZeroDriftKeepsTheCoveringStatement(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	clock := time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return clock }
	c := testMandateConstitution()
	reconcileNow(t, st)
	read := func(at time.Time, equity, pnl float64) {
		clock = at
		st.observeMandateLocked(c, equity, mandatePnLReading{DailyBase: new(pnl), Session: mandateSessionDate(at)}, at, 0, at)
	}
	read(time.Date(2026, 11, 2, 14, 0, 0, 0, time.UTC), 300000, 0)
	read(time.Date(2026, 11, 2, 14, 2, 0, 0, time.UTC), 320000, 0)
	if st.state.Mandate.FlowSuspect == nil {
		t.Fatal("setup: the move raised no hold")
	}
	read(time.Date(2026, 11, 3, 6, 55, 0, 0, time.UTC), 320000, 100) // the new broker day opens near zero
	st.state.StatementCoverageTo = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	read(time.Date(2026, 11, 3, 6, 57, 0, 0, time.UTC), 320050, 150) // drift near zero, figure changed
	if s := st.state.Mandate.FlowSuspect; s != nil {
		t.Fatalf("near-zero drift moved the hold past the covering statement: ambiguous at %s", s.PnLResetAmbiguousAt)
	}
}

// Trades move cash into positions and back. When the broker's executions
// carry the whole cash and quantity change between two readings, the
// equity move is the marks' and the trades', never money, whatever the
// daily P&L figure says: on 2026-11-02 the owner's hand trades in TWS while
// the daemon was away held every mandate reduction from 15:44 UTC on. The
// executions arrived a poll after the raising reading, so the hold clears
// on them then. A deposit beside a purchase and a transfer in kind stay
// held.
func TestMandateFlowCheckReadsTradesTheExecutionsCarryAsTrades(t *testing.T) {
	utc := func(hour, minute, second int) time.Time {
		return time.Date(2026, 11, 2, hour, minute, second, 0, time.UTC)
	}
	suspected := func(got mandate.Capital) bool {
		return !got.Known && len(got.Reasons) > 0 && strings.Contains(got.Reasons[0], "a cash flow is suspected")
	}
	fx := new(0.9) // base per USD
	fill := func(id string, conID, multiplier int, side string, qty, price float64, at time.Time) rpc.FillQuoteRecord {
		secType := "OPT"
		if multiplier == 1 {
			secType = "STK"
		}
		return rpc.FillQuoteRecord{ExecID: id, Contract: rpc.ContractParams{ConID: conID, SecType: secType, Multiplier: multiplier}, Side: side, Quantity: qty, Price: price, ExecTime: at, FXRate: fx}
	}
	type book map[int]float64
	fresh := func(t *testing.T) (*riskCapitalStore, *risk.Constitution, func(at time.Time, equity, cash, pnl float64, b book, fills []rpc.FillQuoteRecord, complete bool), func() mandate.Capital) {
		clock := utc(14, 23, 0)
		st := newTestRiskCapitalStore(t)
		st.now = func() time.Time { return clock }
		c := testMandateConstitution()
		reconcileNow(t, st)
		read := func(at time.Time, equity, cash, pnl float64, b book, fills []rpc.FillQuoteRecord, complete bool) {
			clock = at.Add(time.Second)
			reading := mandatePnLReading{DailyBase: new(pnl), Session: mandateSessionDate(at), CashBase: new(cash), PositionsKey: mandateQuantitiesKey(b), Quantities: b,
				Fills: func(from, to time.Time) ([]rpc.FillQuoteRecord, bool) {
					var out []rpc.FillQuoteRecord
					for _, f := range fills {
						if f.ExecTime.After(from) && !f.ExecTime.After(to) {
							out = append(out, f)
						}
					}
					return out, complete
				}}
			st.ObserveWithPnL(equity, reading, at, c, testLiveObserveScope, true)
		}
		capital := func() mandate.Capital { got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock); return got }
		return st, c, read, capital
	}
	before := book{101: 200, 202: 10}
	// Sold 100 shares at 180 USD and bought 5 calls at 4.00 (multiplier
	// 100): +16,200 and −1,800 base at 0.9, a commission of 12 beside.
	trades := []rpc.FillQuoteRecord{
		fill("e1", 101, 1, rpc.OrderActionSell, 100, 180, utc(15, 37, 6)),
		fill("e2", 202, 100, rpc.OrderActionBuy, 5, 4, utc(15, 40, 1)),
	}
	after := book{101: 100, 202: 15}
	t.Run("hand trades held at the reconnect, cleared once the executions arrive", func(t *testing.T) {
		st, _, read, capital := fresh(t)
		read(utc(14, 23, 21), 300000, 100000, 1000, before, nil, true)
		// The daemon is back: the equity fell further than the figure
		// says, the executions not yet polled.
		read(utc(15, 44, 50), 285000, 114388, -6000, after, trades, false)
		if got := capital(); !suspected(got) {
			t.Fatalf("setup: the reconnect reading raised no hold: %+v", got)
		}
		read(utc(15, 46, 50), 285100, 114388, -5900, after, trades, true)
		if got := capital(); !got.Known || st.state.Mandate.FlowSuspect != nil {
			t.Fatalf("executions that carry the whole cash and quantity change left the hold: %+v", got)
		}
	})
	t.Run("executions known at the raising reading", func(t *testing.T) {
		_, _, read, capital := fresh(t)
		read(utc(14, 23, 21), 300000, 100000, 1000, before, nil, true)
		read(utc(15, 44, 50), 285000, 114388, -6000, after, trades, true)
		if got := capital(); !got.Known {
			t.Fatalf("trades the executions carry raised a hold: %v", got.Reasons)
		}
	})
	t.Run("deposit beside a purchase", func(t *testing.T) {
		_, _, read, capital := fresh(t)
		read(utc(14, 23, 21), 300000, 100000, 1000, before, nil, true)
		// 20,000 in, 50 shares bought at 200 USD (9,000 base) of it.
		buy := []rpc.FillQuoteRecord{fill("e3", 101, 1, rpc.OrderActionBuy, 50, 200, utc(15, 37, 6))}
		read(utc(15, 44, 50), 320000, 111000, 1000, book{101: 250, 202: 10}, buy, true)
		if got := capital(); !suspected(got) {
			t.Fatalf("a deposit beside a purchase passed as trades: %+v", got)
		}
		read(utc(15, 46, 50), 320000, 111000, 1000, book{101: 250, 202: 10}, buy, true)
		if got := capital(); !suspected(got) {
			t.Fatalf("a deposit beside a purchase cleared as trades: %+v", got)
		}
	})
	t.Run("transfer in kind beside a trade", func(t *testing.T) {
		_, _, read, capital := fresh(t)
		read(utc(14, 23, 21), 300000, 100000, 1000, before, nil, true)
		// The sale and shares of another name arriving that no execution carries.
		read(utc(15, 44, 50), 330000, 116200, 1000, book{101: 100, 202: 10, 303: 500}, trades[:1], true)
		if got := capital(); !suspected(got) {
			t.Fatalf("a transfer in kind passed as trades: %+v", got)
		}
		read(utc(15, 46, 50), 330000, 116200, 1000, book{101: 100, 202: 10, 303: 500}, trades[:1], true)
		if got := capital(); !suspected(got) {
			t.Fatalf("a transfer in kind cleared as trades: %+v", got)
		}
	})
}

// The flow check counts executions only when the read is whole through the
// reading: a complete, error-free poll of the same account started after
// it, with nothing it found still waiting for its capture.
func TestFillsBetweenIsWholeOnlyAfterACompletePollAndCapture(t *testing.T) {
	scope := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}
	at := time.Date(2026, 11, 2, 15, 44, 50, 0, time.UTC)
	rt := &fillQuoteRuntime{records: map[string]fillQuoteStored{
		"in":    {Account: "U1234567", Record: rpc.FillQuoteRecord{ExecID: "in", ExecTime: at.Add(-7 * time.Minute)}},
		"early": {Account: "U1234567", Record: rpc.FillQuoteRecord{ExecID: "early", ExecTime: at.Add(-2 * time.Hour)}},
		"other": {Account: "U7654321", Record: rpc.FillQuoteRecord{ExecID: "other", ExecTime: at.Add(-7 * time.Minute)}},
	}, claimed: map[string]bool{}}
	from := at.Add(-80 * time.Minute)
	authority := &orderPreviewBrokerAuthority{}
	poll := fillQuotePoll{scope: scope, at: at.Add(-time.Second), complete: true, coverageFrom: from.Add(-time.Minute), allClients: true, authority: authority}
	rt.recordPoll(poll)
	if _, ok := rt.fillsBetween(scope, from, at, authority); ok {
		t.Fatal("a poll before the reading read as whole")
	}
	poll.at, poll.complete = at.Add(time.Minute), false
	rt.recordPoll(poll)
	if _, ok := rt.fillsBetween(scope, from, at, authority); ok {
		t.Fatal("an incomplete poll read as whole")
	}
	poll.complete, poll.err = true, "timeout"
	rt.recordPoll(poll)
	if _, ok := rt.fillsBetween(scope, from, at, authority); ok {
		t.Fatal("a poll with a broker error read as whole")
	}
	poll.err = ""
	rt.recordPoll(poll)
	rt.queue = []fillQuoteFill{{account: "U1234567"}}
	if _, ok := rt.fillsBetween(scope, from, at, authority); ok {
		t.Fatal("an execution waiting for its capture read as whole")
	}
	rt.queue = nil
	rt.pending = map[string]fillQuoteStored{"waiting": {Account: scope.Account}}
	if _, ok := rt.fillsBetween(scope, from, at, authority); ok {
		t.Fatal("an execution awaiting durable storage read as whole")
	}
	rt.pending = nil
	got, ok := rt.fillsBetween(scope, from, at, authority)
	if !ok || len(got) != 1 || got[0].ExecID != "in" {
		t.Fatalf("fills = %+v, %v; want only the account's execution in the window", got, ok)
	}
	if _, ok := rt.fillsBetween(brokerStateScope{Account: "U7654321", Mode: rpc.AccountModeLive}, from, at, authority); ok {
		t.Fatal("another account's poll read as whole")
	}
	if _, ok := rt.fillsBetween(scope, from, at, &orderPreviewBrokerAuthority{}); ok {
		t.Fatal("a replacement broker authority inherited the old poll's proof")
	}
	s := &Server{fillQuotes: rt}
	if _, ok := s.mandateFillsBetween(scope, from, at); ok {
		t.Fatal("a disconnected broker session certified execution coverage")
	}
}

// A request end marker says the current-day inventory finished; it cannot
// certify an older flow base or executions hidden by the API client scope.
// A missing round trip can cancel a real deposit in cash while leaving the
// same quantities, so cash/quantity reconstruction alone cannot prove it.
func TestMandateTradeRecoveryRejectsUnprovenExecutionCoverage(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	at := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		from       time.Time
		allClients bool
	}{
		{"previous day gap", at.Add(-25 * time.Hour), true},
		{"other clients not proven", at.Add(-time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fill := rpc.FillQuoteRecord{ExecID: "current-day-buy", Contract: rpc.ContractParams{ConID: 101, SecType: "STK", Multiplier: 1},
				Side: rpc.OrderActionBuy, Quantity: 10, Price: 100, FXRate: new(1.0), ExecTime: at.Add(-time.Minute)}
			rt := &fillQuoteRuntime{records: map[string]fillQuoteStored{fill.ExecID: {Account: scope.Account, Record: fill}}}
			rt.recordPoll(fillQuotePoll{scope: scope, at: at.Add(time.Minute), complete: true, otherClientsSeen: true, coverageFrom: at.Add(-2 * time.Hour), allClients: tc.allClients})
			base := mandateFlowBase{At: tc.from, CashBase: new(100000.0), PositionsKey: mandateQuantitiesKey(map[int]float64{101: 100})}
			pnl := mandatePnLReading{CashBase: new(99000.0), Quantities: map[int]float64{101: 110},
				PositionsKey: mandateQuantitiesKey(map[int]float64{101: 110}),
				Fills:        func(from, to time.Time) ([]rpc.FillQuoteRecord, bool) { return rt.fillsBetween(scope, from, to, nil) }}
			if _, _, explained := mandateTradesExplain(base, pnl, at, 0, 1); explained {
				t.Fatal("partial execution history can clear a real deposit hidden by an omitted losing round trip")
			}
		})
	}
}

func TestExecutionCoverageUsesOnlyDatedFillsInTheCurrentAccountAnswer(t *testing.T) {
	at := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	fill := func(account, stamp string) ibkrlib.OrderLifecycleEvent {
		return ibkrlib.OrderLifecycleEvent{Account: account, ExecTime: stamp}
	}
	for _, tc := range []struct {
		name string
		rows []ibkrlib.OrderLifecycleEvent
		want time.Time
	}{
		{"empty", nil, time.Time{}},
		{"oldest current answer", []ibkrlib.OrderLifecycleEvent{fill("DU1234567", "20261103-14:00:00"), fill("DU1234567", "20261103-13:00:00")}, at.Add(-2 * time.Hour)},
		{"another account cannot extend coverage", []ibkrlib.OrderLifecycleEvent{fill("DU7654321", "20261102-13:00:00"), fill("DU1234567", "20261103-14:00:00")}, at.Add(-time.Hour)},
		{"receipt fallback not proof", []ibkrlib.OrderLifecycleEvent{fill("DU1234567", "20261103 14:00:00")}, time.Time{}},
		{"one ambiguous timestamp", []ibkrlib.OrderLifecycleEvent{fill("DU1234567", "20261103-14:00:00"), fill("DU1234567", "")}, time.Time{}},
		{"unknown account", []ibkrlib.OrderLifecycleEvent{fill("", "20261103-14:00:00")}, time.Time{}},
		{"future execution", []ibkrlib.OrderLifecycleEvent{fill("DU1234567", "20261103-16:00:00")}, time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := executionCoverageFrom(tc.rows, "DU1234567", at); !got.Equal(tc.want) {
				t.Fatalf("coverage starts at %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExecutionRecoveryRequiresCurrentSessionMasterClientEvidence(t *testing.T) {
	s := &Server{}
	authority := &orderPreviewBrokerAuthority{connectorEpoch: 2}
	diagnostic := ibkrlib.APIConfigDiagnostic{State: "observed", Settings: &ibkrlib.APISettings{MasterClientID: new(15)}}
	s.gatewaySetup.set(1, diagnostic)
	if s.fillQuoteAllClients(authority, 15) {
		t.Fatal("previous session's Master API setting granted coverage")
	}
	s.gatewaySetup.set(2, diagnostic)
	if !s.fillQuoteAllClients(authority, 15) || s.fillQuoteAllClients(authority, 16) {
		t.Fatal("Master API evidence did not bind the exact client")
	}
	diagnostic.Settings.MasterClientID = nil
	s.gatewaySetup.set(2, diagnostic)
	if s.fillQuoteAllClients(authority, 15) {
		t.Fatal("missing Master API setting granted coverage")
	}
}
