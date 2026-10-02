package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestCashSweepDefaultRouteFollowsTheMarketCycle(t *testing.T) {
	day := func(v string) time.Time {
		d, err := time.Parse(time.DateOnly, v)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	for _, tc := range []struct {
		ccy, day string
		days     int
	}{
		{"USD", "2026-10-02", 1},
		{"USD", "2028-06-30", 1},
		{"EUR", "2026-10-02", 2},
		{"EUR", "2027-10-08", 2}, // last Friday under CSDR T+2
		{"EUR", "2027-10-11", 1}, // Regulation (EU) 2025/2075
	} {
		exchange, days, ok := cashSweepDefaultRoute(tc.ccy, day(tc.day))
		if !ok || exchange != "SMART" || days != tc.days {
			t.Fatalf("%s %s: %q T+%d %v, want SMART T+%d", tc.ccy, tc.day, exchange, days, ok, tc.days)
		}
	}
	for _, ccy := range []string{"GBP", "CAD", "CHF", ""} {
		if _, _, ok := cashSweepDefaultRoute(ccy, day("2026-10-02")); ok {
			t.Fatalf("%q gained a route without a payment calendar", ccy)
		}
	}
}

func TestCashSweepRouteForPrefersThePolicyAndNeedsNoDate(t *testing.T) {
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name         string
		ccy          string
		cfg          protectionCashSweepCurrency
		at           time.Time
		wantExchange string
		wantDays     int
		wantSource   string
		wantReason   string
	}{
		{"fresh install or upgraded file", "USD", protectionCashSweepCurrency{}, at, "SMART", 1, rpc.CashSweepSettlementSourceDefault, ""},
		{"EUR default", "EUR", protectionCashSweepCurrency{}, at, "SMART", 2, rpc.CashSweepSettlementSourceDefault, ""},
		{"EUR trade day is Berlin's", "EUR", protectionCashSweepCurrency{}, time.Date(2027, 10, 10, 23, 30, 0, 0, time.UTC), "SMART", 1, rpc.CashSweepSettlementSourceDefault, ""},
		{"full override", "USD", protectionCashSweepCurrency{SettlementExchange: "SMART", SettlementDays: new(3)}, at, "SMART", 3, rpc.CashSweepSettlementSourcePolicy, ""},
		{"lag override keeps the default exchange", "USD", protectionCashSweepCurrency{SettlementDays: new(2)}, at, "SMART", 2, rpc.CashSweepSettlementSourcePolicy, ""},
		{"exchange override keeps the default lag", "EUR", protectionCashSweepCurrency{SettlementExchange: "IBIS"}, at, "IBIS", 2, rpc.CashSweepSettlementSourceDefault, ""},
		{"an owner end date still ends the route", "USD", protectionCashSweepCurrency{SettlementValidThrough: "2026-10-01"}, at, "SMART", 1, rpc.CashSweepSettlementSourceDefault, "settlement_valid_through has passed"},
		{"an owner end date in the future", "USD", protectionCashSweepCurrency{SettlementValidThrough: "2026-10-02"}, at, "SMART", 1, rpc.CashSweepSettlementSourceDefault, ""},
		{"no calendar, no route", "GBP", protectionCashSweepCurrency{}, at, "", 0, rpc.CashSweepSettlementSourcePolicy, "no verified payment calendar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := cashSweepRouteFor(tc.ccy, tc.cfg, tc.at)
			if r.exchange != tc.wantExchange || r.source != tc.wantSource || (tc.wantDays == 0) != (r.days == nil) || r.days != nil && *r.days != tc.wantDays ||
				r.validThrough != string(tc.cfg.SettlementValidThrough) {
				t.Fatalf("route = %+v (days %v), want %s T+%d %s", r, r.days, tc.wantExchange, tc.wantDays, tc.wantSource)
			}
			_, reason := cashSweepSettlementDate(tc.ccy, r.exchange, r.days, r.validThrough, tc.at)
			if tc.wantReason == "" && reason != "" || tc.wantReason != "" && !strings.Contains(reason, tc.wantReason) && !strings.Contains(reason, "lag or exchange") {
				t.Fatalf("settlement date reason %q, want %q", reason, tc.wantReason)
			}
		})
	}
	if r := cashSweepRouteFor("USD", protectionCashSweepCurrency{SettlementDays: new(1)}, time.Time{}); r.exchange != "" || r.days == nil {
		t.Fatalf("an unknown clock invented a default exchange: %+v", r)
	}
}

// A policy file written before routes had defaults, or by a fresh install,
// carries no settlement lines: its bill rows take Canary's route and no
// calendar blocker; an exchange the route does not name, or an owner end date
// that has passed, still holds.
func TestCashSweepRowsWithoutSettlementLinesUseTheDefaultRoute(t *testing.T) {
	now := cashSweepTestNow()
	row := func(mutate func(*protectionCashSweepCurrency)) rpc.TradeProposal {
		policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
		usd := policy.Buckets.CashSweep.Currency["USD"]
		usd.SettlementDays, usd.SettlementExchange, usd.SettlementValidThrough = nil, "", ""
		if mutate != nil {
			mutate(&usd)
		}
		policy.Buckets.CashSweep.Currency["USD"] = usd
		plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": 60000}), now)
		cashSweepResolveBills(context.Background(), usBillSource(now), policy.Buckets.CashSweep, &plan, now)
		return cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cashSweepCurrencyOf(t, plan, "USD"))
	}
	got := row(nil)
	s := got.CashSweep
	if s == nil || s.SettlementExchange != "SMART" || s.SettlementDays == nil || *s.SettlementDays != 1 || s.SettlementSource != rpc.CashSweepSettlementSourceDefault || s.SettlementValidThrough != "" {
		t.Fatalf("row route = %+v", s)
	}
	if got.Contract.Exchange != "SMART" || hasTradingBlocker(got.Blockers, "cash_sweep_settlement_calendar_unknown") ||
		hasTradingBlocker(cashSweepCurrentEvidenceBlockers(got, now), "cash_sweep_settlement_calendar_unknown") {
		t.Fatalf("the default route held the row: %+v / %+v", got.Contract, got.Blockers)
	}
	if b := cashSweepCurrentEvidenceBlockers(row(func(c *protectionCashSweepCurrency) { c.SettlementExchange = "IBIS" }), now); !hasTradingBlocker(b, "cash_sweep_settlement_calendar_unknown") {
		t.Fatalf("an override naming another exchange was accepted: %v", b)
	}
	if b := cashSweepCurrentEvidenceBlockers(row(func(c *protectionCashSweepCurrency) { c.SettlementValidThrough = "2026-09-29" }), now); !hasTradingBlocker(b, "cash_sweep_settlement_calendar_unknown") {
		t.Fatalf("a passed owner end date was ignored: %v", b)
	}
}
