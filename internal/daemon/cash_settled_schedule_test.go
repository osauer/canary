package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func settledScheduleFixture(ccy string, amounts ...float64) ibkrlib.SettledCashSchedule {
	s := ibkrlib.SettledCashSchedule{Currency: ccy, Status: "observed", ReceivedAt: time.Date(2026, 10, 2, 7, 38, 0, 0, time.UTC)}
	for i, amount := range amounts {
		s.Points = append(s.Points, ibkrlib.SettledCashPoint{Date: time.Date(2026, 10, 2+3*i, 0, 0, 0, 0, time.UTC), Amount: amount})
	}
	return s
}

func settledScheduleCapture(status string, schedules ...ibkrlib.SettledCashSchedule) *ibkrlib.SettledCashScheduleCapture {
	out := &ibkrlib.SettledCashScheduleCapture{StreamStatus: status, Schedules: map[string]ibkrlib.SettledCashSchedule{}}
	for _, s := range schedules {
		out.Schedules[s.Currency] = s
	}
	return out
}

func TestSettledCashScheduleAdmission(t *testing.T) {
	segment := settledScheduleFixture("USD", 5000, 60000)
	segment.SegmentPoints = []ibkrlib.SettledCashPoint{{Date: segment.Points[0].Date, Amount: 4000}, {Date: segment.Points[1].Date, Amount: 60000}}
	invalid := settledScheduleFixture("USD")
	invalid.Status, invalid.Reason = "invalid", "SettledCashByDate: dates do not strictly ascend"
	for _, tc := range []struct {
		name       string
		capture    *ibkrlib.SettledCashScheduleCapture
		observed   bool
		wantStatus string
		wantLow    float64
		wantReason string
	}{
		{"pending outflow: the final balance is the low", settledScheduleCapture("initial_complete", settledScheduleFixture("USD", 97000, 60000)), true, rpc.SettledCashScheduleAdmitted, 60000, ""},
		{"pending inflow: today's balance is the low", settledScheduleCapture("initial_complete", settledScheduleFixture("USD", 26000, 60000.4)), true, rpc.SettledCashScheduleAdmitted, 26000, ""},
		{"the securities segment lowers the bound", settledScheduleCapture("initial_complete", segment), true, rpc.SettledCashScheduleAdmitted, 4000, ""},
		{"a fill TWS has not re-sent", settledScheduleCapture("initial_complete", settledScheduleFixture("USD", 97000, 58000)), true, rpc.SettledCashScheduleHeld, 0, "does not end at current trade-date cash"},
		{"stream not downloaded", settledScheduleCapture("initial_pending", settledScheduleFixture("USD", 60000)), true, rpc.SettledCashScheduleHeld, 0, "stream is initial pending"},
		{"receipt truncated", &ibkrlib.SettledCashScheduleCapture{StreamStatus: "initial_complete", Truncated: true, Schedules: map[string]ibkrlib.SettledCashSchedule{"USD": settledScheduleFixture("USD", 60000)}}, true, rpc.SettledCashScheduleHeld, 0, "more settlement schedules"},
		{"invalid value", settledScheduleCapture("initial_complete", invalid), true, rpc.SettledCashScheduleHeld, 0, "strictly ascend"},
		{"no trade-date cash to reconcile with", settledScheduleCapture("initial_complete", settledScheduleFixture("USD", 60000)), false, rpc.SettledCashScheduleHeld, 0, "no current USD trade-date cash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &rpc.AccountResult{BaseCurrency: "EUR", CurrencyExposure: []rpc.CurrencyExposure{{Currency: "USD", CashCcy: 60000, CashObserved: tc.observed}, {Currency: "GBP", CashCcy: 5, CashObserved: true}},
				BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashCcy: 8000, CashObserved: true}}
			annotateSettledCashSchedules(res, tc.capture)
			got := res.CurrencyExposure[0].SettledCashSchedule
			if got == nil || got.Status != tc.wantStatus || !strings.Contains(got.Reason, tc.wantReason) || len(got.Points) != len(tc.capture.Schedules["USD"].Points) {
				t.Fatalf("schedule = %+v, want %s %q", got, tc.wantStatus, tc.wantReason)
			}
			if tc.wantStatus == rpc.SettledCashScheduleAdmitted && (got.Low == nil || *got.Low != tc.wantLow) || tc.wantStatus == rpc.SettledCashScheduleHeld && got.Low != nil {
				t.Fatalf("low = %v, want %v", got.Low, tc.wantLow)
			}
			if res.CurrencyExposure[1].SettledCashSchedule != nil || res.BaseCurrencyLedger.SettledCashSchedule != nil || res.CurrencyExposure[0].SettledCashCcy != nil {
				t.Fatal("a currency without a schedule gained one, or the ledger field was rewritten")
			}
		})
	}
	res := &rpc.AccountResult{CurrencyExposure: []rpc.CurrencyExposure{{Currency: "USD", CashCcy: 1, CashObserved: true}}}
	annotateSettledCashSchedules(res, nil)
	if res.CurrencyExposure[0].SettledCashSchedule != nil {
		t.Fatal("no capture invented a schedule")
	}
}

func TestSettledCashScheduleReachesTheSweep(t *testing.T) {
	res := &rpc.AccountResult{AccountID: "DU1234567", BaseCurrency: "EUR",
		CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "USD", CashCcy: 60000, CashObserved: true, ExchangeRate: 0.9}},
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashCcy: 8000, CashObserved: true, ExchangeRate: 1},
		Authority: &rpc.AccountDataAuthority{Scope: rpc.AccountDataScope{AccountID: "DU1234567", AccountMode: "paper"}, Availability: rpc.AccountDataAvailable,
			Freshness: rpc.AccountDataFreshnessCurrent, Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}}}
	annotateSettledCashSchedules(res, settledScheduleCapture("initial_complete", settledScheduleFixture("USD", 97000, 60000), settledScheduleFixture("EUR", 3500, 8000)))
	_, ledger, reason := cashSweepLedger(res)
	if reason != "" || ledger["USD"].Settled == nil || *ledger["USD"].Settled != 60000 || ledger["USD"].SettledSourceKind != "native_tws_settlement_schedule" ||
		ledger["EUR"].Settled == nil || *ledger["EUR"].Settled != 3500 {
		t.Fatalf("sweep ledger = %+v %q", ledger, reason)
	}
	res.CurrencyExposure[0].SettledCashCcy = new(59000.0)
	if row := cashSweepCashObservation(res.CurrencyExposure[0], res, time.Now()); *row.Settled != 59000 || row.SettledSourceKind != "minimum_of_native_tws_ledger_and_schedule" {
		t.Fatalf("ledger and schedule = %+v", row)
	}

	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	in := cashSweepTestInput(map[string]float64{"USD": 60000, "EUR": 8000})
	in.Settlement = cashSweepSettlement{Reason: "the order journal cannot certify settlement"}
	in.Ledger["USD"], in.Ledger["EUR"] = ledger["USD"], ledger["EUR"]
	plan := cashSweepPlanFor(policy, in, now)
	usd, eur := cashSweepCurrencyOf(t, plan, "USD").status, cashSweepCurrencyOf(t, plan, "EUR").status
	if usd.State == rpc.CashSweepStateSettlementUnknown || usd.SettledCashSource != rpc.CashSweepSettledSourceBroker || usd.SettledSourceKind != "native_tws_settlement_schedule" ||
		*usd.SettledCash != 60000 || *usd.Cash != 60000 {
		t.Fatalf("USD with an admitted schedule = %+v", usd)
	}
	if eur.State == rpc.CashSweepStateSettlementUnknown || *eur.SettledCash != 3500 || *eur.Cash != 3500 {
		t.Fatalf("EUR spends only today's settled balance before Monday's inflow = %+v", eur)
	}

	// A held schedule keeps settlement unknown and says why.
	held := *res.CurrencyExposure[0].SettledCashSchedule
	held.Status, held.Low, held.Reason = rpc.SettledCashScheduleHeld, nil, "TWS's USD settlement schedule does not end at current trade-date cash; waiting for TWS's next account update"
	row := res.CurrencyExposure[0]
	row.SettledCashCcy, row.SettledCashSchedule = nil, &held
	in.Ledger["USD"] = cashSweepCashObservation(row, res, time.Now())
	if st := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD").status; st.State != rpc.CashSweepStateSettlementUnknown || st.Reason != held.Reason || st.Cash != nil {
		t.Fatalf("held schedule = %+v", st)
	}
}

func TestCashLedgerPlanningRechecksSettledScheduleAgainstFills(t *testing.T) {
	now := time.Date(2026, 10, 2, 10, 0, 2, 0, time.UTC)
	stamp, filledAt := now.Add(-2*time.Second), now.Add(-time.Second)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, tc := range []struct {
		name        string
		twsAt       time.Time
		uncertain   bool
		foreignFill bool
		wantState   string
		wantReason  string
	}{
		{"post-fill snapshot keeps its schedule", now, false, false, rpc.CashSweepStateInvest, ""},
		{"pre-fill snapshot cannot fund a sweep", stamp, false, false, rpc.CashSweepStateCashUnavailable, "predates a confirmed fill"},
		{"unknown fill continuity drops the schedule", now, true, false, rpc.CashSweepStateSettlementUnknown, "cannot be checked against cash-affecting fills"},
		{"other-account fill does not retire the schedule", stamp, false, true, rpc.CashSweepStateInvest, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			srv.now = func() time.Time { return now }
			srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: now}, scope, nil
			}
			schedule := &rpc.SettledCashSchedule{Status: rpc.SettledCashScheduleAdmitted, Low: new(52000.0),
				Points: []rpc.SettledCashPoint{{Date: "2026-10-02", Amount: 52000}, {Date: "2026-10-05", Amount: 60000}}}
			acct := &rpc.AccountResult{AccountID: scope.Account, BaseCurrency: "USD", AsOf: tc.twsAt,
				Authority: &rpc.AccountDataAuthority{Scope: accountDataScope(scope), Availability: rpc.AccountDataAvailable,
					Freshness: rpc.AccountDataFreshnessCurrent, AsOf: tc.twsAt, Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}},
				BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "USD", CashObserved: true, CashCcy: 60000, ExchangeRate: 1, SettledCashSchedule: schedule}}
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
			if cp.status.State != tc.wantState || !strings.Contains(cp.status.Reason, tc.wantReason) || tc.wantState != rpc.CashSweepStateInvest && cp.side != "" {
				t.Fatalf("schedule posture = %+v, want %s %q", cp.status, tc.wantState, tc.wantReason)
			}
			if tc.wantState == rpc.CashSweepStateInvest && (cp.side != rpc.CashSweepSideInvest || cp.cash != 52000) {
				t.Fatalf("schedule low lost its bound: side %q cash %v", cp.side, cp.cash)
			}
			if acct.BaseCurrencyLedger.SettledCashSchedule != schedule || schedule.Status != rpc.SettledCashScheduleAdmitted {
				t.Fatal("planner mutated the retained account source snapshot")
			}
		})
	}
}
