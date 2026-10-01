package daemon

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestCashLedgerPlanningUsesConsumptionTimeForReceiptFreshness(t *testing.T) {
	started := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	consumed := started.Add(2 * time.Second)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, tc := range []struct {
		name      string
		webAt     time.Time
		wantState string
	}{
		{"receipt arrived during refresh", started.Add(time.Second), rpc.CashSweepStateInvest},
		{"receipt at maximum age", consumed.Add(-time.Minute), rpc.CashSweepStateInvest},
		{"genuinely future receipt", consumed.Add(time.Second), rpc.CashSweepStateSettlementUnknown},
		{"receipt became stale during refresh", started.Add(-59 * time.Second), rpc.CashSweepStateSettlementUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			current := started
			srv.now = func() time.Time { return current }
			srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				// Account and positions reads have completed; the final broker read
				// consumes more time before cash is used by the planner.
				current = consumed
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: current}, scope, nil
			}
			acct := &rpc.AccountResult{AccountID: scope.Account, BaseCurrency: "USD", AsOf: consumed,
				Authority: &rpc.AccountDataAuthority{Scope: accountDataScope(scope), Availability: rpc.AccountDataAvailable,
					Freshness: rpc.AccountDataFreshnessCurrent, AsOf: consumed, Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}},
				BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "USD", CashObserved: true, CashCcy: 60000, ExchangeRate: 1,
					WebCash: &rpc.WebCashObservation{Currency: "USD", Scope: accountDataScope(scope), CashBalance: 60000, SettledCash: 60000, AsOf: tc.webAt}}}
			engine := &proposalEngine{server: srv, queued: &queuedAuthStore{}}
			policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
			in := engine.cashSweepInput(context.Background(), policy, acct, &rpc.PositionsResult{}, scope, started)
			cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, started), "USD")
			if cp.status.State != tc.wantState || tc.wantState != rpc.CashSweepStateInvest && cp.side != "" {
				t.Fatalf("receipt freshness used refresh start instead of consumption: %+v, want %s", cp.status, tc.wantState)
			}
			if !current.Equal(consumed) || !acct.BaseCurrencyLedger.WebCash.AsOf.Equal(tc.webAt) {
				t.Fatal("fixture did not advance time or the original broker timestamp was changed")
			}
		})
	}
}

func TestCashLedgerPlanningRechecksFillsAfterAccountRead(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 2, 0, time.UTC)
	stamp, filledAt := now.Add(-2*time.Second), now.Add(-time.Second)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, tc := range []struct {
		name         string
		twsAt, webAt time.Time
		native       *float64
		uncertain    bool
		foreignFill  bool
		wantState    string
		wantCash     float64
	}{
		{"retained TWS and Web before terminal BUY", stamp, stamp, nil, false, false, rpc.CashSweepStateCashUnavailable, 0},
		{"fresh Web cannot repair pre-fill TWS", stamp, now, nil, false, false, rpc.CashSweepStateCashUnavailable, 0},
		{"post-fill TWS and pre-fill Web only", now, stamp, nil, false, false, rpc.CashSweepStateSettlementUnknown, 0},
		{"post-fill native survives pre-fill supplement", now, stamp, new(7000.0), false, false, rpc.CashSweepStateInvest, 7000},
		{"both receipts post-fill", now, now, nil, false, false, rpc.CashSweepStateInvest, 60000},
		{"unknown continuity cannot certify Web-only cash", now, now, nil, true, false, rpc.CashSweepStateSettlementUnknown, 0},
		{"independent native survives unknown supplement continuity", now, now, new(7000.0), true, false, rpc.CashSweepStateInvest, 7000},
		{"other-account fill does not retire selected-account cash", stamp, stamp, nil, false, true, rpc.CashSweepStateInvest, 60000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			srv.now = func() time.Time { return now }
			srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: now}, scope, nil
			}
			web := &rpc.WebCashObservation{Currency: "USD", Scope: accountDataScope(scope), CashBalance: 60000, SettledCash: 60000, AsOf: tc.webAt}
			acct := &rpc.AccountResult{AccountID: scope.Account, BaseCurrency: "USD", AsOf: tc.twsAt,
				Authority: &rpc.AccountDataAuthority{Scope: accountDataScope(scope), Availability: rpc.AccountDataAvailable,
					Freshness: rpc.AccountDataFreshnessCurrent, AsOf: tc.twsAt, Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}},
				BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "USD", CashObserved: true, CashCcy: 60000, ExchangeRate: 1, WebCash: web, SettledCashCcy: tc.native}}
			// Account returned first. A terminal BUY is then durably observed while
			// subsequent positions/open-order work completes. No buy remains working.
			fill := originTestOrder("filled-between-account-and-plan", 801, filledAt)
			fill.Type, fill.Status, fill.SendState = orderJournalEventStatusUpdated, "Filled", orderSendStateTerminal
			fill.Action, fill.Quantity, fill.Filled = rpc.OrderActionBuy, 10, 10
			if tc.foreignFill {
				fill.Account = "DU7654321"
			}
			if err := srv.orderJournal.Append(fill); err != nil {
				t.Fatal(err)
			}
			srv.orderLifecyclePersistenceUncertain.Store(tc.uncertain)
			engine := &proposalEngine{server: srv, queued: &queuedAuthStore{}}
			policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
			in := engine.cashSweepInput(context.Background(), policy, acct, &rpc.PositionsResult{}, scope, now)
			cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD")
			if cp.status.State != tc.wantState || tc.wantState != rpc.CashSweepStateInvest && cp.side != "" {
				t.Fatalf("post-fill input posture = %+v, want %s", cp.status, tc.wantState)
			}
			if tc.wantState == rpc.CashSweepStateInvest && (cp.side != rpc.CashSweepSideInvest || cp.cash != tc.wantCash) {
				t.Fatalf("independent current cash lost its bound: side %q cash %v, want %v", cp.side, cp.cash, tc.wantCash)
			}
			if acct.BaseCurrencyLedger.WebCash != web {
				t.Fatal("planner mutated the retained account source snapshot")
			}
		})
	}
}

func TestCashLedgerFillCutoffRejectsMissingOrUnreadableJournal(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, server := range []*Server{nil, {}, {orderJournal: newOrderJournalStore("")}} {
		if at, err := server.cashLedgerFillCutoff(scope); err == nil || !at.IsZero() {
			t.Fatal("unavailable journal certified a no-fill frontier", at, err)
		}
	}
	if _, err := (*Server)(nil).cashLedgerFillCutoff(scope); !errors.Is(err, ErrTradingDisabled) {
		t.Fatal("absence lost its build/journal diagnostic", err)
	}
}

func TestCashLedgerFillCutoffRechecksNewFillsAndPersistenceUncertainty(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	server := &Server{orderJournal: newTestOrderJournalStore(t, filepath.Join(t.TempDir(), "orders")), now: func() time.Time { return now }}
	if at, err := server.cashLedgerFillCutoff(scope); err != nil || !at.IsZero() {
		t.Fatal("available empty local journal was not readable", at, err)
	}
	appendFill := func(ref string, id int, at time.Time) {
		t.Helper()
		event := originTestOrder(ref, id, at)
		event.Type, event.Status = orderJournalEventStatusUpdated, "Filled"
		event.Filled, event.Quantity, event.SendState = 10, 10, orderSendStateTerminal
		if err := server.orderJournal.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	first := now.Add(-time.Minute)
	appendFill("first-known-fill", 801, first)
	cutoff, err := server.cashLedgerFillCutoff(scope)
	if err != nil || !cutoff.Equal(first) {
		t.Fatal("confirmed terminal fill was lost", cutoff, err)
	}
	// A cached Web receipt at first cannot survive a later confirmed fill after
	// that order disappears from working-order commitments.
	second := now.Add(-time.Second)
	appendFill("later-known-fill", 802, second)
	cutoff, err = server.cashLedgerFillCutoff(scope)
	if err != nil || !cutoff.Equal(second) || !first.Before(cutoff) {
		t.Fatal("cached pre-fill cash remained eligible", cutoff, err)
	}
	server.orderLifecyclePersistenceUncertain.Store(true)
	if _, err := server.cashLedgerFillCutoff(scope); err == nil {
		t.Fatal("known lost lifecycle receipts were ignored")
	}
	server.orderLifecyclePersistenceUncertain.Store(false)
	if err := server.orderJournal.authority.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := server.cashLedgerFillCutoff(scope); err == nil {
		t.Fatal("closed authoritative store became a no-fill frontier")
	}
}

func TestCashLedgerFillCutoffUsesConfirmedScopedIncreasesOnly(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	view := rpc.OrderView{OrderRef: "synthetic-fill", Account: scope.Account, Mode: scope.Mode, Filled: 3}
	first, latest := now.Add(-time.Minute), now.Add(-time.Second)
	event := func(at time.Time, filled float64) rpc.OrderEvent {
		return rpc.OrderEvent{Type: orderJournalEventStatusUpdated, Account: scope.Account, Mode: scope.Mode, At: at, Filled: filled, ExecTime: "untrusted broker time"}
	}
	events := []rpc.OrderEvent{event(first, 1), event(latest, 3), event(now, 3), {Type: orderJournalEventCancelRequested, Filled: 3, At: now}}
	foreign := view
	foreign.OrderRef, foreign.Account = "foreign-fill", "DU7654321"
	foreignEvent := event(now, 100)
	foreignEvent.Account = foreign.Account
	got, err := cashLedgerFillCutoffFrom([]rpc.OrderView{view, foreign}, map[string][]rpc.OrderEvent{orderViewKey(view): events, orderViewKey(foreign): {foreignEvent}}, scope, now)
	if err != nil || !got.Equal(latest) {
		t.Fatal("duplicate/bookkeeping/foreign receipt moved the fill frontier", got, err)
	}
	// A new execution/correction can affect cash without raising cumulative
	// quantity. Replayed execution IDs still cannot move the frontier.
	events = append(events, event(now, 3))
	events[len(events)-1].ExecID = "synthetic-execution-correction"
	if got, err := cashLedgerFillCutoffFrom([]rpc.OrderView{view}, map[string][]rpc.OrderEvent{orderViewKey(view): events}, scope, now); err != nil || !got.Equal(now) {
		t.Fatal("new execution correction did not invalidate earlier cash", got, err)
	}
	// Receipt clock regression cannot certify whether retained cash preceded
	// a later inserted fill. Hold instead of weakening the frontier.
	view.Filled = 4
	events = append(events, event(first.Add(time.Second), 4))
	if got, err := cashLedgerFillCutoffFrom([]rpc.OrderView{view}, map[string][]rpc.OrderEvent{orderViewKey(view): events}, scope, now); err == nil || !got.IsZero() {
		t.Fatal("clock regression certified old cash", got, err)
	}
}

func TestCashLedgerFillCutoffRefusesAmbiguousEvidence(t *testing.T) {
	now := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, scenario := range []string{"unbound-scope", "view-account-missing", "view-mode-missing", "view-mode-unknown", "view-account-aggregate", "event-account-missing", "event-mode-missing", "foreign-event", "unstamped", "future", "missing-events", "bookkeeping-only", "nonfinite", "negative", "cumulative-regression", "uncertain-send", "send-not-acknowledged", "unknown-reconciliation"} {
		t.Run(scenario, func(t *testing.T) {
			view := rpc.OrderView{OrderRef: "synthetic-fill", Account: scope.Account, Mode: scope.Mode, Filled: 1}
			ev := rpc.OrderEvent{Type: orderJournalEventStatusUpdated, Account: scope.Account, Mode: scope.Mode, At: now.Add(-time.Second), Filled: 1}
			selected := scope
			events := []rpc.OrderEvent{ev}
			switch scenario {
			case "unbound-scope":
				selected.Mode = "unknown"
			case "view-account-missing":
				view.Account = ""
			case "view-mode-missing":
				view.Mode = ""
			case "view-mode-unknown":
				view.Mode = rpc.AccountModeUnknown
			case "view-account-aggregate":
				view.Account = "All"
			case "event-account-missing":
				events[0].Account = ""
			case "event-mode-missing":
				events[0].Mode = ""
			case "foreign-event":
				events[0].Account = "DU7654321"
			case "unstamped":
				events[0].At = time.Time{}
			case "future":
				events[0].At = now.Add(time.Second)
			case "missing-events":
				events = nil
			case "bookkeeping-only":
				events[0].Type = orderJournalEventReconciledAbsent
			case "nonfinite":
				events[0].Filled = math.Inf(1)
			case "negative":
				events[0].Filled = -1
			case "cumulative-regression":
				events = append(events, ev)
				events[1].Filled = 0.5
			case "uncertain-send":
				view.SendState = orderSendStateUncertainSend
			case "send-not-acknowledged":
				view.SendState = orderSendStateSendAttempted
			case "unknown-reconciliation":
				view.LifecycleStatus = rpc.OrderLifecycleUnknownReconcileRequired
			}
			if at, err := cashLedgerFillCutoffFrom([]rpc.OrderView{view}, map[string][]rpc.OrderEvent{orderViewKey(view): events}, selected, now); err == nil || !at.IsZero() {
				t.Fatal("ambiguous evidence certified old cash", at, err)
			}
		})
	}
}
