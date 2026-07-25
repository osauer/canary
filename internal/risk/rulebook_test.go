package risk

import (
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func etDate(y int, m time.Month, d int) time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// healthyInputs models a compact version of a real book: an oversized NOW
// complex into earnings, a BB option line over the premium cap, a SPY hedge,
// and an MSFT covered call across its print.
func healthyInputs() RuleInputs {
	now := etDate(2026, 7, 7)
	nowEarnings := EarningsInput{Known: true, Date: etDate(2026, 7, 22), TimeOfDay: "amc", SessionsUntil: new(11), Source: "fetched"}
	bbEarnings := EarningsInput{Known: true, Date: etDate(2026, 7, 30), TimeOfDay: "amc", SessionsUntil: new(17), Source: "fetched"}
	msftEarnings := EarningsInput{Known: true, Date: etDate(2026, 7, 29), TimeOfDay: "amc", SessionsUntil: new(16), Source: "fetched"}
	return RuleInputs{
		AsOf:            now,
		BaseCurrency:    "EUR",
		Positions:       SourceState{Healthy: true},
		Account:         SourceState{Healthy: true},
		NLVBase:         new(245000.0),
		CashBase:        new(-62000.0),
		DailyPnLBase:    new(9700.0),
		SessionOpen:     true,
		SPYDayChangePct: new(1.0),
		Names: []NameInput{
			{
				Symbol: "NOW", ExposureBase: 380000, MarketValueBase: 120000, HasStockLeg: true, ExposureBaseComplete: true,
				StockDayChangePct: new(1.6),
				Legs: []LegInput{
					{Desc: "NOW 20260717 C 130", Right: "C", Strike: 130, Expiry: etDate(2026, 7, 17), DTE: 10,
						Quantity: 35, Multiplier: 100, Mark: 0.44, Underlying: new(108.0), Delta: new(0.08),
						MarketValueBase: 1400, ExtrinsicBase: new(1400.0), CostBasisBase: new(2000.0), FXToBase: new(0.9)},
					{Desc: "NOW 20260821 C 115", Right: "C", Strike: 115, Expiry: etDate(2026, 8, 21), DTE: 45,
						Quantity: 50, Multiplier: 100, Mark: 7.86, Underlying: new(108.0), Delta: new(0.46),
						MarketValueBase: 36000, ExtrinsicBase: new(36000.0), CostBasisBase: new(40000.0), FXToBase: new(0.9)},
				},
			},
			{
				Symbol: "BB", ExposureBase: 45000, MarketValueBase: 45000, HasStockLeg: true, ExposureBaseComplete: true,
				StockDayChangePct: new(-1.7),
				Legs: []LegInput{
					{Desc: "BB 20260821 C 12", Right: "C", Strike: 12, Expiry: etDate(2026, 8, 21), DTE: 45,
						Quantity: 300, Multiplier: 100, Mark: 1.28, Underlying: new(11.3), Delta: new(0.50),
						MarketValueBase: 34000, ExtrinsicBase: new(34000.0), CostBasisBase: new(40000.0), FXToBase: new(0.9)},
				},
			},
			{
				Symbol: "MSFT", ExposureBase: 30000, MarketValueBase: 12000, HasStockLeg: true, ExposureBaseComplete: true,
				StockDayChangePct: new(0.3),
				Legs: []LegInput{
					{Desc: "MSFT 20260821 C 400", Right: "C", Strike: 400, Expiry: etDate(2026, 8, 21), DTE: 45,
						Quantity: -3, Multiplier: 100, Mark: 5, Underlying: new(386.0), Delta: new(-0.3),
						MarketValueBase: -1400},
				},
			},
			{
				Symbol: "SPY", ExposureBase: -80000, MarketValueBase: 38000, HasStockLeg: false,
				Legs: []LegInput{
					{Desc: "SPY 20261016 P 710", Right: "P", Strike: 710, Expiry: etDate(2026, 10, 16), DTE: 101,
						Quantity: 40, Multiplier: 100, Mark: 10.4, Underlying: new(752.0), Delta: new(-0.24),
						MarketValueBase: 38000, ExtrinsicBase: new(38000.0), CostBasisBase: new(45000.0), FXToBase: new(0.9), HedgeListed: true},
				},
			},
		},
		Earnings:          map[string]EarningsInput{"NOW": nowEarnings, "MSFT": msftEarnings, "BB": bbEarnings},
		NonBaseNLVBase:    new(230000.0),
		NonBaseCurrencies: []string{"USD"},
	}
}

func rowByID(t *testing.T, ev Evaluation, id string) RuleRow {
	t.Helper()
	for _, r := range ev.Rows {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("row %s missing", id)
	return RuleRow{}
}

func TestEvaluateRulebookHealthyBook(t *testing.T) {
	ev := EvaluateRulebook(healthyInputs(), DefaultRulebookPolicy())
	if len(ev.Rows) != 14 {
		t.Fatalf("rows = %d, want 14", len(ev.Rows))
	}
	cases := map[string]string{
		RuleSingleNameExposure: RuleStatusAct,     // NOW 155% of NLV
		RuleOptionLinePremium:  RuleStatusAct,     // NOW 115C 14.7%, BB 13.9% (normal tier)
		RuleCashSellOnly:       RuleStatusAct,     // -25.3% vs calm -25 floor
		RuleExtrinsicBudget:    RuleStatusAct,     // ex-hedge ~29.1% of NLV
		RuleExpiryRunway:       RuleStatusWatch,   // NOW Jul 130C at 10 DTE
		RuleCatalystCoverage:   RuleStatusWatch,   // NOW 130C dies before Jul 22
		RuleOverwriteEarnings:  RuleStatusAct,     // MSFT short call through Jul 29
		RuleEarningsSizeFreeze: RuleStatusUnknown, // hedge-list membership is not evidence that an option underlying is a nonissuer
		RuleRedOnGreen:         RuleStatusWatch,   // BB -1.7% on SPY +1.0%
		RuleWinnerTrim:         RuleStatusPass,    // nothing +4%
		RuleGreenDayAction:     RuleStatusInfo,    // green day, act rules open
		RuleHedgeIntegrity:     RuleStatusAct,     // hedge ~159% of gross long > 2× calm band top
		RuleExitDiscipline:     RuleStatusPass,    // worst line -30%, under the -40% fence
		RuleFXExposure:         RuleStatusWatch,   // 93.9% non-base vs 60% watch-only bar
	}
	for id, want := range cases {
		if got := rowByID(t, ev, id).Status; got != want {
			t.Errorf("%s = %s, want %s (evidence: %s)", id, got, want, rowByID(t, ev, id).Evidence)
		}
	}
}

func TestGreenDayActionRequiresFiniteDailyPnL(t *testing.T) {
	tests := []struct {
		name string
		pnl  *float64
	}{
		{name: "missing", pnl: nil},
		{name: "nan", pnl: new(math.NaN())},
		{name: "positive infinity", pnl: new(math.Inf(1))},
		{name: "negative infinity", pnl: new(math.Inf(-1))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := healthyInputs()
			in.DailyPnLBase = test.pnl
			row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleGreenDayAction)
			if row.Status != RuleStatusNotEvaluated || row.Reason != RuleReasonPnLUnavailable {
				t.Fatalf("green day row = %s/%s, want not_evaluated/%s", row.Status, row.Reason, RuleReasonPnLUnavailable)
			}
		})
	}
}

// TestNeverFalsePass is the acceptance test for the design's safety
// invariant: strip each input dimension and assert the affected rows report
// unknown/not_evaluated — never pass (docs/design/trading-rulebook.md).
func TestNeverFalsePass(t *testing.T) {
	pol := DefaultRulebookPolicy()
	portfolioRules := []string{
		RuleSingleNameExposure, RuleOptionLinePremium, RuleExtrinsicBudget,
		RuleExpiryRunway, RuleCatalystCoverage, RuleOverwriteEarnings,
		RuleEarningsSizeFreeze, RuleHedgeIntegrity, RuleExitDiscipline,
	}
	assertNoPass := func(t *testing.T, ev Evaluation, ids ...string) {
		t.Helper()
		for _, id := range ids {
			r := rowByID(t, ev, id)
			if r.Status == RuleStatusPass {
				t.Errorf("%s = pass on degraded inputs (evidence: %s)", id, r.Evidence)
			}
		}
	}

	t.Run("positions pending", func(t *testing.T) {
		in := healthyInputs()
		in.Positions = SourceState{Healthy: false, Reason: "positions_pending"}
		in.Names = nil // a boot race serves an empty book
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, append(portfolioRules, RuleRedOnGreen, RuleWinnerTrim)...)
	})

	t.Run("account absent", func(t *testing.T) {
		in := healthyInputs()
		in.Account = SourceState{Healthy: false, Reason: "account_unavailable"}
		in.NLVBase, in.CashBase, in.DailyPnLBase = nil, nil, nil
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, append(portfolioRules, RuleCashSellOnly, RuleGreenDayAction, RuleFXExposure)...)
	})

	t.Run("underlying stripped", func(t *testing.T) {
		// Positions healthy, deltas present, but no leg has an underlying
		// spot (and extrinsic, which needs it, is gone with it). Rules 4 and
		// 6 must degrade — rule 6 silently skipping these legs produced a
		// live false pass on 2026-07-08.
		in := healthyInputs()
		for i := range in.Names {
			for j := range in.Names[i].Legs {
				in.Names[i].Legs[j].Underlying = nil
				in.Names[i].Legs[j].ExtrinsicBase = nil
			}
		}
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, RuleCatalystCoverage, RuleExtrinsicBudget)
		if got := rowByID(t, ev, RuleCatalystCoverage).Status; got != RuleStatusUnknown {
			t.Errorf("catalyst_coverage = %s with all underlyings stripped, want unknown", got)
		}
	})

	t.Run("fx report absent", func(t *testing.T) {
		in := healthyInputs()
		in.NonBaseNLVBase = nil
		ev := EvaluateRulebook(in, pol)
		r := rowByID(t, ev, RuleFXExposure)
		if r.Status != RuleStatusUnknown || r.Reason != "fx_unavailable" {
			t.Errorf("fx_exposure = %s/%s without a currency report, want unknown/fx_unavailable", r.Status, r.Reason)
		}
		// A corroborated zero is a legitimate pass — the unknown above is
		// about absence, not about small numbers.
		in.NonBaseNLVBase = new(0.0)
		ev = EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleFXExposure).Status; got != RuleStatusPass {
			t.Errorf("corroborated zero non-base exposure = %s, want pass", got)
		}
	})

	t.Run("cost basis missing", func(t *testing.T) {
		in := healthyInputs()
		for i := range in.Names {
			for j := range in.Names[i].Legs {
				in.Names[i].Legs[j].CostBasisBase = nil
			}
		}
		ev := EvaluateRulebook(in, pol)
		r := rowByID(t, ev, RuleExitDiscipline)
		if r.Status != RuleStatusUnknown || r.Reason != "cost_basis_unavailable" {
			t.Errorf("exit_discipline = %s/%s without cost bases, want unknown/cost_basis_unavailable", r.Status, r.Reason)
		}
	})

	t.Run("greeks stripped", func(t *testing.T) {
		in := healthyInputs()
		for i := range in.Names {
			gap := 0.0
			for j := range in.Names[i].Legs {
				in.Names[i].Legs[j].Delta = nil
				in.Names[i].Legs[j].ExtrinsicBase = nil
				gap += abs(in.Names[i].Legs[j].MarketValueBase)
			}
			in.Names[i].GreeksGapNotionalBase = gap
		}
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, RuleSingleNameExposure, RuleExtrinsicBudget, RuleHedgeIntegrity)
	})

	t.Run("earnings unknown", func(t *testing.T) {
		in := healthyInputs()
		in.Earnings = map[string]EarningsInput{}
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, RuleCatalystCoverage, RuleOverwriteEarnings)
	})

	t.Run("earnings stale", func(t *testing.T) {
		in := healthyInputs()
		for k, e := range in.Earnings {
			e.Stale = true
			in.Earnings[k] = e
		}
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, RuleCatalystCoverage, RuleOverwriteEarnings)
	})

	t.Run("off session", func(t *testing.T) {
		in := healthyInputs()
		in.SessionOpen = false
		ev := EvaluateRulebook(in, pol)
		for _, id := range []string{RuleRedOnGreen, RuleWinnerTrim} {
			if got := rowByID(t, ev, id).Status; got != RuleStatusNotEvaluated {
				t.Errorf("%s = %s off-session, want not_evaluated", id, got)
			}
		}
	})

	t.Run("no spy tape", func(t *testing.T) {
		in := healthyInputs()
		in.SPYDayChangePct = nil
		ev := EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleRedOnGreen).Status; got != RuleStatusUnknown {
			t.Errorf("red_on_green = %s without SPY tape, want unknown", got)
		}
	})
}

func TestEarningsGapEdges(t *testing.T) {
	expiry := etDate(2026, 7, 17)
	now := etDate(2026, 7, 7)
	cases := []struct {
		name      string
		e         EarningsInput
		wantSpans bool
		wantAmb   bool
	}{
		{"amc on expiry day dies before gap", EarningsInput{Known: true, Date: etDate(2026, 7, 17), TimeOfDay: "amc"}, false, false},
		{"bmo on expiry day spans", EarningsInput{Known: true, Date: etDate(2026, 7, 17), TimeOfDay: "bmo"}, true, false},
		{"unknown tod on expiry day conservative", EarningsInput{Known: true, Date: etDate(2026, 7, 17)}, true, true},
		{"earnings after expiry", EarningsInput{Known: true, Date: etDate(2026, 7, 24), TimeOfDay: "amc"}, false, false},
		{"earnings before today", EarningsInput{Known: true, Date: etDate(2026, 7, 1), TimeOfDay: "amc"}, false, false},
		{"earnings inside life", EarningsInput{Known: true, Date: etDate(2026, 7, 15), TimeOfDay: "bmo"}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spans, amb := spansEarningsGap(now, expiry, tc.e)
			if spans != tc.wantSpans || amb != tc.wantAmb {
				t.Fatalf("spansEarningsGap = (%v,%v), want (%v,%v)", spans, amb, tc.wantSpans, tc.wantAmb)
			}
		})
	}

	if expiresBeforeCatalyst(etDate(2026, 7, 17), EarningsInput{Known: true, Date: etDate(2026, 7, 17), TimeOfDay: "amc"}) != true {
		t.Error("amc on expiry day: option dies before the gap, rule 6 should flag")
	}
	if expiresBeforeCatalyst(etDate(2026, 7, 17), EarningsInput{Known: true, Date: etDate(2026, 7, 17), TimeOfDay: "bmo"}) != false {
		t.Error("bmo on expiry day: option lives through the gap, rule 6 should not flag")
	}
}

func TestEarningsUnknownReasonIsDisclosed(t *testing.T) {
	if got := earningsGapWord(EarningsInput{Reason: "conflicting_sources"}); got != "conflicting across providers" {
		t.Fatalf("conflict label = %q", got)
	}
	if got := earningsGapWord(EarningsInput{Reason: "no_date_published"}); got != "not published by the provider" {
		t.Fatalf("no-date label = %q", got)
	}
}

func TestTerminalNonReportingIsExplicitlyNotApplicable(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{{
		Symbol: "ACMEQ", ExposureBase: 150000, ExposureBaseComplete: true,
		Legs: []LegInput{
			{Desc: "ACMEQ long", Right: "C", Strike: 12, Expiry: etDate(2026, 8, 21), DTE: 45,
				Quantity: 1, Multiplier: 100, Underlying: new(10.0), MarketValueBase: 100},
			{Desc: "ACMEQ short", Right: "C", Strike: 15, Expiry: etDate(2026, 8, 21), DTE: 45,
				Quantity: -1, Multiplier: 100, Underlying: new(10.0), MarketValueBase: -50},
		},
	}}
	in.Earnings = map[string]EarningsInput{"ACMEQ": {
		TerminalNonReporting: true, Source: "verified_terminal", Reason: earningsTerminalClassForTest,
	}}
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings, RuleEarningsSizeFreeze} {
		row := rowByID(t, ev, id)
		if row.Status != RuleStatusNotEvaluated || row.Reason != EarningsReasonTerminalNonReporting {
			t.Errorf("%s = %s/%s, want not_evaluated/terminal_non_reporting", id, row.Status, row.Reason)
		}
		if len(row.Exempt) != 1 || row.Exempt[0].Symbol != "ACMEQ" || !strings.Contains(row.Exempt[0].Note, "exact contract") {
			t.Errorf("%s exemptions = %+v", id, row.Exempt)
		}
	}
}

func TestBrokerNonIssuerIsExplicitlyNotEvaluatedNeverPass(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{{
		Symbol: "SYNTH1", ExposureBase: 150000, ExposureBaseComplete: true,
	}}
	in.Earnings = map[string]EarningsInput{"SYNTH1": {
		NotApplicable: true, Source: "broker_identity", Reason: EarningsReasonBrokerNonIssuer,
	}}
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings, RuleEarningsSizeFreeze} {
		row := rowByID(t, ev, id)
		if row.Status != RuleStatusNotEvaluated || row.Reason != EarningsReasonBrokerNonIssuer || len(row.Exempt) != 1 {
			t.Errorf("%s did not disclose the broker nonissuer exemption", id)
		}
	}
}

func TestBrokerNonIssuerStockProofCannotExemptMixedOptionGroup(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{{
		Symbol: "SYNTH1", ExposureBase: 150000, ExposureBaseComplete: true,
		Legs: []LegInput{
			{Desc: "synthetic long", Right: "C", Strike: 12, Expiry: etDate(2026, 8, 21), DTE: 45,
				Quantity: 1, Multiplier: 100, Underlying: new(10.0), MarketValueBase: 100},
			{Desc: "synthetic short", Right: "C", Strike: 15, Expiry: etDate(2026, 8, 21), DTE: 45,
				Quantity: -1, Multiplier: 100, Underlying: new(10.0), MarketValueBase: -50},
		},
	}}
	in.Earnings = map[string]EarningsInput{"SYNTH1": {
		NotApplicable: true, Source: "broker_identity", Reason: EarningsReasonBrokerNonIssuer,
	}}
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings, RuleEarningsSizeFreeze} {
		row := rowByID(t, ev, id)
		if row.Status != RuleStatusUnknown || len(row.Exempt) != 0 {
			t.Errorf("%s let a stock identity proof exempt option legs: %+v", id, row)
		}
	}
}

// A COMMON broker identity remains an ordinary issuer in the daemon; with a
// typed no-date provider outcome it reaches risk as unknown, never exempt.
func TestCommonIssuerWithNoDateNeverPassesRelevantRules(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{{
		Symbol: "SYNTH1", ExposureBase: 150000, ExposureBaseComplete: true,
		Legs: []LegInput{
			{Desc: "synthetic long", Right: "C", Strike: 12, Expiry: etDate(2026, 8, 21), DTE: 45,
				Quantity: 1, Multiplier: 100, Underlying: new(10.0), MarketValueBase: 100},
			{Desc: "synthetic short", Right: "C", Strike: 15, Expiry: etDate(2026, 8, 21), DTE: 45,
				Quantity: -1, Multiplier: 100, Underlying: new(10.0), MarketValueBase: -50},
		},
	}}
	in.Earnings = map[string]EarningsInput{"SYNTH1": {Source: "fetched", Reason: "no_date_published"}}
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings, RuleEarningsSizeFreeze} {
		row := rowByID(t, ev, id)
		if row.Status == RuleStatusPass || row.Status == RuleStatusNotEvaluated {
			t.Errorf("%s treated an issuer with unknown earnings as resolved", id)
		}
	}
}

func TestOrdinaryIssuerStockOnlyUnknownOrStaleFailsClosed(t *testing.T) {
	cases := []struct {
		name     string
		earnings EarningsInput
	}{
		{name: "not published", earnings: EarningsInput{Source: "fetched", Reason: "no_date_published"}},
		{name: "stale date", earnings: EarningsInput{Known: true, Stale: true, Date: etDate(2026, 7, 30), Source: "fetched"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := healthyInputs()
			in.Names = []NameInput{{Symbol: "COMMON1", ExposureBase: 150000, ExposureBaseComplete: true}}
			in.Earnings = map[string]EarningsInput{"COMMON1": tc.earnings}
			ev := EvaluateRulebook(in, DefaultRulebookPolicy())
			for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings, RuleEarningsSizeFreeze} {
				row := rowByID(t, ev, id)
				if row.Status != RuleStatusUnknown || row.Reason != "earnings_unknown" || len(row.Offenders) != 1 {
					t.Errorf("%s = %+v, want one earnings_unknown offender", id, row)
				}
			}
		})
	}
}

func TestOrdinaryIssuerBelowSizeWithoutDateFailsClosed(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{{Symbol: "COMMON1", ExposureBase: 1000, ExposureBaseComplete: true}}
	in.Earnings = map[string]EarningsInput{"COMMON1": {Source: "fetched", Reason: "no_date_published"}}
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings, RuleEarningsSizeFreeze} {
		row := rowByID(t, ev, id)
		if row.Status != RuleStatusUnknown || row.Reason != "earnings_unknown" {
			t.Errorf("%s = %s/%s, want unknown/earnings_unknown below the size threshold", id, row.Status, row.Reason)
		}
	}
}

func TestTerminalNonReportingStockOnlyIsExplicitlyNotApplicableForRulesSixAndSeven(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{{
		Symbol: "ACMEQ", ExposureBase: 150000, ExposureBaseComplete: true,
	}}
	in.Earnings = map[string]EarningsInput{"ACMEQ": {
		TerminalNonReporting: true, Source: "verified_terminal", Reason: earningsTerminalClassForTest,
	}}
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings} {
		row := rowByID(t, ev, id)
		if row.Status != RuleStatusNotEvaluated || row.Reason != EarningsReasonTerminalNonReporting {
			t.Errorf("%s = %s/%s, want not_evaluated/terminal_non_reporting", id, row.Status, row.Reason)
		}
		if len(row.Exempt) != 1 || row.Exempt[0].Symbol != "ACMEQ" {
			t.Errorf("%s exemptions = %+v", id, row.Exempt)
		}
	}
}

func TestUnresolvedTerminalAuthorityStockOnlyFailsClosedForEarningsRules(t *testing.T) {
	cases := []struct {
		name  string
		input EarningsInput
	}{
		{name: "expired", input: EarningsInput{Source: "verified_terminal", Reason: "terminal_evidence_expired"}},
		{name: "identity conflict", input: EarningsInput{Source: "verified_terminal", Reason: "terminal_identity_conflict"}},
		{name: "date source conflict", input: EarningsInput{Source: "verified_terminal", Reason: "terminal_evidence_conflict"}},
		{name: "stale accepted record", input: EarningsInput{TerminalNonReporting: true, Stale: true, Source: "verified_terminal", Reason: earningsTerminalClassForTest}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := healthyInputs()
			in.Names = []NameInput{{Symbol: "ACMEQ", ExposureBase: 1000, ExposureBaseComplete: true}}
			in.Earnings = map[string]EarningsInput{"ACMEQ": tc.input}
			ev := EvaluateRulebook(in, DefaultRulebookPolicy())
			for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings, RuleEarningsSizeFreeze} {
				row := rowByID(t, ev, id)
				if row.Status != RuleStatusUnknown || row.Reason != "earnings_unknown" {
					t.Errorf("%s = %s/%s, want unknown/earnings_unknown", id, row.Status, row.Reason)
				}
				if len(row.Exempt) != 0 || len(row.Offenders) != 1 || row.Offenders[0].Symbol != "ACMEQ" || !strings.Contains(row.Offenders[0].Note, "authority is unresolved") {
					t.Errorf("%s unresolved evidence = offenders:%+v exempt:%+v", id, row.Offenders, row.Exempt)
				}
			}
		})
	}
}

func TestTerminalNonReportingStockOnlyDoesNotHideAssessedNames(t *testing.T) {
	in := healthyInputs()
	in.Names = []NameInput{
		{Symbol: "ACMEQ", ExposureBase: 1000, ExposureBaseComplete: true},
		{
			Symbol: "OTHER", ExposureBase: 2000, ExposureBaseComplete: true,
			Legs: []LegInput{
				{Desc: "OTHER long", Right: "C", Strike: 12, Expiry: etDate(2026, 8, 21), DTE: 45,
					Quantity: 1, Multiplier: 100, Underlying: new(10.0), MarketValueBase: 100},
				{Desc: "OTHER short", Right: "C", Strike: 15, Expiry: etDate(2026, 7, 10), DTE: 3,
					Quantity: -1, Multiplier: 100, Underlying: new(10.0), MarketValueBase: -50},
			},
		},
	}
	in.Earnings = map[string]EarningsInput{
		"ACMEQ": {TerminalNonReporting: true, Source: "verified_terminal", Reason: earningsTerminalClassForTest},
		"OTHER": {Known: true, Date: etDate(2026, 7, 22), TimeOfDay: "amc", SessionsUntil: new(11), Source: "fetched"},
	}
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	for _, id := range []string{RuleCatalystCoverage, RuleOverwriteEarnings} {
		row := rowByID(t, ev, id)
		if row.Status != RuleStatusPass {
			t.Errorf("%s = %s/%s, want assessed pass", id, row.Status, row.Reason)
		}
		if len(row.Exempt) != 1 || row.Exempt[0].Symbol != "ACMEQ" {
			t.Errorf("%s exemptions = %+v", id, row.Exempt)
		}
	}
}

const earningsTerminalClassForTest = "equity_interests_cancelled"

func TestHedgeExemptionSuppressedWhenOverHedged(t *testing.T) {
	in := healthyInputs()
	// Inflate the hedge so the band breaches high: short delta ~194k vs
	// gross long ~455k ≈ 43%.
	spy := &in.Names[3].Legs[0]
	spy.Delta = new(-0.60)
	spy.DTE = 10 // inside runway window so the exemption question is live
	spy.Expiry = etDate(2026, 7, 17)
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())

	hedge := rowByID(t, ev, RuleHedgeIntegrity)
	// ~397% of gross long is past twice the calm band top: act, not watch —
	// and the rule-5 exemption suppression must survive the escalation.
	if hedge.Status != RuleStatusAct || !strings.Contains(hedge.Evidence, "twice") {
		t.Fatalf("hedge row = %s (%s), want act past 2× band top", hedge.Status, hedge.Evidence)
	}
	runway := rowByID(t, ev, RuleExpiryRunway)
	found := false
	for _, o := range runway.Offenders {
		if o.Symbol == "SPY" && strings.Contains(o.Note, "suppressed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("over-hedged SPY leg should lose its runway exemption; offenders = %+v, exempt = %+v", runway.Offenders, runway.Exempt)
	}
}

// A policy-hedge index name with net-short delta belongs to rule 12, not the
// concentration cap — but it must surface in rule 1's Exempt list, never
// silently vanish (2026-07-07 live finding: 40 SPY puts ≈ 234% of NLV in
// absolute delta-dollars outranked every real concentration offender).
func TestSingleNameExposureExemptsShortHedge(t *testing.T) {
	in := healthyInputs()
	in.Names[3].ExposureBase = -640000 // oversized SPY hedge, short delta
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	row := rowByID(t, ev, RuleSingleNameExposure)
	for _, o := range row.Offenders {
		if o.Symbol == "SPY" {
			t.Fatalf("short hedge SPY must not be a concentration offender: %+v", row.Offenders)
		}
	}
	found := false
	for _, e := range row.Exempt {
		if e.Symbol == "SPY" && strings.Contains(e.Note, "rule 12") {
			found = true
		}
	}
	if !found {
		t.Fatalf("short hedge must be disclosed in Exempt, got %+v", row.Exempt)
	}
	if row.Status != RuleStatusAct || row.Offenders[0].Symbol != "NOW" {
		t.Fatalf("real concentration offender should lead: status=%s offenders=%+v", row.Status, row.Offenders)
	}
	// A LONG position in a hedge-listed index is ordinary concentration.
	in.Names[3].ExposureBase = 640000
	ev = EvaluateRulebook(in, DefaultRulebookPolicy())
	row = rowByID(t, ev, RuleSingleNameExposure)
	if len(row.Offenders) == 0 || row.Offenders[0].Symbol != "SPY" {
		t.Fatalf("long index exposure must still count as concentration, got %+v", row.Offenders)
	}
}

// The exemption covers only what rule 12 can size (long puts with delta).
// Short stock or short calls in a hedge symbol are directional shorts: the
// residual beyond the sized legs stays a concentration offender.
func TestSingleNameExposureResidualBeyondSizedHedge(t *testing.T) {
	in := healthyInputs()
	// Fixture SPY puts size to 0.24*40*100*752 = 721,920 short delta-dollars.
	// Net short 900k leaves a 178,080 residual = 72.7% of the 245k NLV.
	in.Names[3].ExposureBase = -900000
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())
	row := rowByID(t, ev, RuleSingleNameExposure)
	var spy *RuleOffender
	for i := range row.Offenders {
		if row.Offenders[i].Symbol == "SPY" {
			spy = &row.Offenders[i]
		}
	}
	if spy == nil || spy.Observed < 72 || spy.Observed > 73 {
		t.Fatalf("residual short beyond sized hedge legs must be an offender near 72.7%%, got %+v", row.Offenders)
	}
	if len(row.Exempt) == 0 || row.Exempt[0].Symbol != "SPY" {
		t.Fatalf("sized portion must still be disclosed in Exempt, got %+v", row.Exempt)
	}

	// A hedge-symbol short with NO rule-12-sizeable legs (e.g. short stock,
	// puts stripped) is pure concentration: no Exempt row at all.
	in = healthyInputs()
	in.Names[3].ExposureBase = -640000
	in.Names[3].Legs = nil
	ev = EvaluateRulebook(in, DefaultRulebookPolicy())
	row = rowByID(t, ev, RuleSingleNameExposure)
	if len(row.Exempt) != 0 {
		t.Fatalf("unsized hedge-symbol short must not be exempted, got %+v", row.Exempt)
	}
	found := false
	for _, o := range row.Offenders {
		if o.Symbol == "SPY" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unsized hedge-symbol short must be a concentration offender, got %+v", row.Offenders)
	}
}

func TestRankingHardestFirst(t *testing.T) {
	ev := EvaluateRulebook(healthyInputs(), DefaultRulebookPolicy())
	if len(ev.Ranked) != 14 {
		t.Fatalf("ranked = %d, want 14", len(ev.Ranked))
	}
	weight := map[string]int{RuleStatusAct: 5, RuleStatusWatch: 4, RuleStatusUnknown: 3, RuleStatusInfo: 2, RuleStatusNotEvaluated: 1, RuleStatusPass: 0}
	prev := 6
	prevImpact := 0.0
	for i, ix := range ev.Ranked {
		r := ev.Rows[ix]
		w := weight[r.Status]
		if w > prev {
			t.Fatalf("ranked[%d] %s (%s) outranks a lighter status", i, r.ID, r.Status)
		}
		if w == prev && r.ImpactBase > prevImpact {
			t.Fatalf("ranked[%d] %s impact %.0f should precede %.0f", i, r.ID, r.ImpactBase, prevImpact)
		}
		prev, prevImpact = w, r.ImpactBase
	}
	first := ev.Rows[ev.Ranked[0]]
	if first.Status != RuleStatusAct {
		t.Fatalf("hardest-first head = %s, want an act row", first.Status)
	}
}

// Regime-conditional thresholds (rules 3, 4, 12): a fresh stage selects its
// set; a carried or never-seen stage evaluates worse-of(carried, calm) so
// stale regime data can hold or tighten a verdict but never relax it — in
// either band direction.
func TestRegimeConditionalThresholds(t *testing.T) {
	pol := DefaultRulebookPolicy()

	t.Run("fresh confirmed tightens the cash floor", func(t *testing.T) {
		in := healthyInputs()
		in.CashBase = new(-12250.0) // -5% of NLV: fine in calm, breach in confirmed
		in.RegimeStage = RegimeBucketConfirmed
		in.RegimeStageAsOf = in.AsOf
		ev := EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleCashSellOnly).Status; got != RuleStatusAct {
			t.Errorf("cash at -5%% under fresh confirmed (+10 floor) = %s, want act", got)
		}
		in.RegimeStage = RegimeBucketCalm
		ev = EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleCashSellOnly).Status; got != RuleStatusPass {
			t.Errorf("cash at -5%% under fresh calm (-25 floor) = %s, want pass", got)
		}
	})

	t.Run("carried confirmed cannot loosen the hedge band", func(t *testing.T) {
		// Ratio ~60%: inside the confirmed band (40-70) but over calm (25-35).
		// A carried confirmed stage must NOT acquit — worse-of keeps watch.
		in := healthyInputs()
		in.Names[3].Legs[0].Delta = new(-0.0908) // 0.0908*40*100*752 ≈ 273k ≈ 60% of 455k
		in.RegimeStage = RegimeBucketConfirmed
		in.RegimeStageAsOf = in.AsOf.Add(-6 * time.Hour)
		in.RegimeStageCarried = true
		ev := EvaluateRulebook(in, pol)
		r := rowByID(t, ev, RuleHedgeIntegrity)
		if r.Status != RuleStatusWatch {
			t.Errorf("hedge 60%% under carried confirmed = %s, want watch (worse-of calm)", r.Status)
		}
		found := false
		for _, n := range r.Notes {
			if strings.Contains(n, "carried") {
				found = true
			}
		}
		if !found {
			t.Errorf("carried-stage verdict must disclose provenance, notes = %v", r.Notes)
		}
		// The same ratio under a FRESH confirmed stage passes: 60 ∈ [40,70].
		in.RegimeStageCarried = false
		in.RegimeStageAsOf = in.AsOf
		ev = EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleHedgeIntegrity).Status; got != RuleStatusPass {
			t.Errorf("hedge 60%% under fresh confirmed = %s, want pass", got)
		}
	})

	t.Run("carried confirmed still tightens cash", func(t *testing.T) {
		in := healthyInputs()
		in.CashBase = new(-12250.0)
		in.RegimeStage = RegimeBucketConfirmed
		in.RegimeStageCarried = true
		ev := EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleCashSellOnly).Status; got != RuleStatusAct {
			t.Errorf("cash -5%% under carried confirmed = %s, want act (worse-of)", got)
		}
	})

	t.Run("never-seen stage uses calm with disclosure", func(t *testing.T) {
		in := healthyInputs()
		ev := EvaluateRulebook(in, pol)
		r := rowByID(t, ev, RuleCashSellOnly)
		found := false
		for _, n := range r.Notes {
			if strings.Contains(n, "never observed") {
				found = true
			}
		}
		if !found {
			t.Errorf("never-seen regime stage must be disclosed, notes = %v", r.Notes)
		}
	})
}

// Rule 2's hedge tier: classified hedge legs measure against 15/25, normal
// legs keep 5/10, unclassifiable legs get no relief.
func TestOptionLinePremiumHedgeTier(t *testing.T) {
	pol := DefaultRulebookPolicy()

	base := func() RuleInputs {
		in := healthyInputs()
		// Quiet the normal-tier offenders so the hedge line drives status.
		in.Names[0].Legs = in.Names[0].Legs[:1] // drop NOW C115 (14.7%)
		in.Names[1].Legs = nil                  // drop BB C12 (13.9%)
		return in
	}

	in := base() // SPY 38000 = 15.5%: hedge tier watch, would be act on normal tier
	ev := EvaluateRulebook(in, pol)
	r := rowByID(t, ev, RuleOptionLinePremium)
	if r.Status != RuleStatusWatch {
		t.Fatalf("15.5%% hedge line = %s, want hedge-tier watch (evidence: %s)", r.Status, r.Evidence)
	}
	if !strings.Contains(r.Evidence, "hedge") {
		t.Errorf("hedge-tier verdict must say so: %s", r.Evidence)
	}
	if r.Observed == nil || *r.Observed != 15.5 {
		t.Errorf("hedge-tier observed = %v, want 15.5", r.Observed)
	}
	if r.Threshold == nil || *r.Threshold != pol.HedgeLineWatchPct {
		t.Errorf("hedge-tier threshold = %v, want %.1f", r.Threshold, pol.HedgeLineWatchPct)
	}

	in = base()
	in.Names[3].Legs[0].MarketValueBase = 66000 // 26.9% > hedge act 25
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleOptionLinePremium)
	if r.Status != RuleStatusAct {
		t.Errorf("26.9%% hedge line = %s, want act", r.Status)
	}
	if r.Observed == nil || *r.Observed != 26.9 || r.Threshold == nil || *r.Threshold != pol.HedgeLineWatchPct {
		t.Errorf("hedge-tier act row observed/threshold = %v/%v, want 26.9/%.1f", r.Observed, r.Threshold, pol.HedgeLineWatchPct)
	}

	in = base()
	in.Names[3].Legs[0].Delta = nil // unclassifiable: normal tier, no relief
	in.Names[3].GreeksGapNotionalBase = 38000
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleOptionLinePremium)
	if r.Status != RuleStatusAct {
		t.Errorf("15.5%% unclassifiable hedge line = %s, want normal-tier act", r.Status)
	}
	if r.Observed == nil || *r.Observed != 15.5 || r.Threshold == nil || *r.Threshold != pol.OptionLineWatchPct {
		t.Errorf("unclassifiable normal-tier observed/threshold = %v/%v, want 15.5/%.1f", r.Observed, r.Threshold, pol.OptionLineWatchPct)
	}

	// A joined (stock-leg-mark) underlying never classifies a hedge — the
	// derived spot must not unlock the softer tier.
	in = base()
	in.Names[3].Legs[0].UnderlyingSource = UnderlyingSourceStockLegMark
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleOptionLinePremium)
	if r.Status != RuleStatusAct {
		t.Errorf("15.5%% hedge line with derived underlying = %s, want normal-tier act (no classification from joined spots)", r.Status)
	}
	if r.Observed == nil || *r.Observed != 15.5 || r.Threshold == nil || *r.Threshold != pol.OptionLineWatchPct {
		t.Errorf("derived-spot normal-tier observed/threshold = %v/%v, want 15.5/%.1f", r.Observed, r.Threshold, pol.OptionLineWatchPct)
	}

	// Tie case: both tiers at watch, hedge line carrying the larger impact —
	// the headline must caption the NORMAL-tier offender with the 5%% cap,
	// never the hedge line (mislabeling the hedge with the speculative cap
	// misdirects the operator to cut protection).
	in = base()
	in.Names[1].Legs = []LegInput{{Desc: "BB 20260821 C 12", Right: "C", Strike: 12,
		Expiry: etDate(2026, 8, 21), DTE: 45, Quantity: 100, Multiplier: 100, Mark: 2,
		Underlying: new(11.3), Delta: new(0.5), MarketValueBase: 20000, ExtrinsicBase: new(20000.0)}}
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleOptionLinePremium)
	if r.Status != RuleStatusWatch {
		t.Fatalf("tie case status = %s, want watch", r.Status)
	}
	if !strings.Contains(r.Evidence, "BB") || !strings.Contains(r.Evidence, "cap 5") {
		t.Errorf("tie-case headline must name the normal-tier offender with its own cap, got: %s", r.Evidence)
	}
	if r.Observed == nil || *r.Observed != 8.2 || r.Threshold == nil || *r.Threshold != pol.OptionLineWatchPct {
		t.Errorf("tie-case normal-tier observed/threshold = %v/%v, want 8.2/%.1f", r.Observed, r.Threshold, pol.OptionLineWatchPct)
	}
}

func TestFXExposureWatchBoundary(t *testing.T) {
	pol := DefaultRulebookPolicy()
	in := healthyInputs()
	in.NonBaseNLVBase = new(*in.NLVBase * pol.FXExposureWatchPct / 100)

	r := rowByID(t, EvaluateRulebook(in, pol), RuleFXExposure)
	if r.Status != RuleStatusWatch {
		t.Fatalf("FX exposure exactly %.0f%% = %s, want watch", pol.FXExposureWatchPct, r.Status)
	}

	in.NonBaseNLVBase = new(*in.NLVBase * (pol.FXExposureWatchPct - 0.1) / 100)
	r = rowByID(t, EvaluateRulebook(in, pol), RuleFXExposure)
	if r.Status != RuleStatusPass {
		t.Fatalf("FX exposure just below the %.0f%% boundary = %s, want pass", pol.FXExposureWatchPct, r.Status)
	}
}

// Rule 7 short puts: spanning puts are watch, escalating to act on assignment
// notional; unknown FX never quietly escalates or drops.
func TestOverwriteEarningsShortPuts(t *testing.T) {
	pol := DefaultRulebookPolicy()
	mk := func(qty float64, strike float64, fx *float64) RuleInputs {
		in := healthyInputs()
		in.Names[2].Legs = []LegInput{{
			Desc: "MSFT 20261016 P 350", Right: "P", Strike: strike, Expiry: etDate(2026, 10, 16), DTE: 101,
			Quantity: qty, Multiplier: 100, Mark: 14, Underlying: new(386.0), Delta: new(-0.28),
			MarketValueBase: -6200, FXToBase: fx,
		}}
		return in
	}

	// 5 short 350P × 0.9 fx = 157.5k assignment ≈ 64% of NLV ⇒ act.
	ev := EvaluateRulebook(mk(-5, 350, new(0.9)), pol)
	r := rowByID(t, ev, RuleOverwriteEarnings)
	if r.Status != RuleStatusAct {
		t.Fatalf("64%% NLV assignment short put = %s, want act (evidence: %s)", r.Status, r.Evidence)
	}

	// 1 short 100P × 0.9 = 9k ≈ 3.7% ⇒ watch.
	ev = EvaluateRulebook(mk(-1, 100, new(0.9)), pol)
	if got := rowByID(t, ev, RuleOverwriteEarnings).Status; got != RuleStatusWatch {
		t.Errorf("3.7%% NLV assignment short put = %s, want watch", got)
	}

	// Several small lines summing past the name tier escalate together:
	// 3 × ~7.3% (each under the 10% line tier) = ~22% ≥ 20% ⇒ act.
	in := healthyInputs()
	put := func(strike float64) LegInput {
		return LegInput{Desc: "MSFT put", Right: "P", Strike: strike, Expiry: etDate(2026, 10, 16), DTE: 101,
			Quantity: -1, Multiplier: 100, Mark: 10, Underlying: new(386.0), Delta: new(-0.2),
			MarketValueBase: -1000, FXToBase: new(0.9)}
	}
	in.Names[2].Legs = []LegInput{put(198), put(199), put(200)}
	ev = EvaluateRulebook(in, pol)
	if got := rowByID(t, ev, RuleOverwriteEarnings).Status; got != RuleStatusAct {
		t.Errorf("name-sum of spanning short puts past 20%% NLV = %s, want act", got)
	}

	// FX unknown: the act tier is unassessable — stays watch, disclosed.
	ev = EvaluateRulebook(mk(-5, 350, nil), pol)
	r = rowByID(t, ev, RuleOverwriteEarnings)
	if r.Status != RuleStatusWatch {
		t.Errorf("short put with unknown FX = %s, want watch", r.Status)
	}
	found := false
	for _, o := range r.Offenders {
		if strings.Contains(o.Note, "unassessable") {
			found = true
		}
	}
	if !found {
		t.Errorf("unknown-FX put must disclose the unassessable notional, offenders = %+v", r.Offenders)
	}
}

// Rule 8: a greeks-gapped name is only skippable when earnings are provably
// beyond the freeze window; unknown or near earnings make it a named unknown
// (the rule-6 silent-skip bug class).
func TestEarningsSizeFreezeGapPropagation(t *testing.T) {
	pol := DefaultRulebookPolicy()
	gapName := func(in *RuleInputs, sessions *int, known bool) {
		in.Names[1].GreeksGapNotionalBase = 34000 // BB gapped, material
		e := EarningsInput{Known: known, Date: etDate(2026, 7, 9), SessionsUntil: sessions, Source: "fetched"}
		in.Earnings["BB"] = e
		// The test isolates BB's missing-Greeks decision. SPY is an option-only
		// group here, so no exact stock identity can prove it is a nonissuer.
		in.Earnings["SPY"] = EarningsInput{Known: true, Date: etDate(2026, 7, 20), SessionsUntil: new(11), Source: "fetched"}
	}

	in := healthyInputs()
	gapName(&in, nil, false) // earnings unknown
	ev := EvaluateRulebook(in, pol)
	r := rowByID(t, ev, RuleEarningsSizeFreeze)
	if r.Status != RuleStatusUnknown {
		t.Fatalf("gapped name with unknown earnings = %s, want unknown", r.Status)
	}

	in = healthyInputs()
	gapName(&in, new(2), true) // inside the freeze window
	ev = EvaluateRulebook(in, pol)
	if got := rowByID(t, ev, RuleEarningsSizeFreeze).Status; got != RuleStatusUnknown {
		t.Errorf("gapped name 2 sessions from earnings = %s, want unknown", got)
	}

	in = healthyInputs()
	gapName(&in, new(11), true) // provably outside the window: skippable
	ev = EvaluateRulebook(in, pol)
	if got := rowByID(t, ev, RuleEarningsSizeFreeze).Status; got != RuleStatusPass {
		t.Errorf("gapped name 11 sessions out = %s, want pass (other names clean)", got)
	}
}

// Rule 1 lower bound: partial data may indict, never acquit. The bound is
// asserted only when the delta-less legs' signed interval proves it.
func TestSingleNameExposureLowerBound(t *testing.T) {
	pol := DefaultRulebookPolicy()

	in := healthyInputs()
	// NOW: known legs sum to 120k (49% of NLV); one delta-less long call
	// (interval [intrinsic, notional] both ≥ 0) can't reduce it. Provable
	// act at ≥ 49% — no waiting for greeks.
	in.Names[0].ExposureBase = 120000
	in.Names[0].GreeksGapNotionalBase = 36000
	in.Names[0].Legs[1].Delta = nil
	ev := EvaluateRulebook(in, pol)
	r := rowByID(t, ev, RuleSingleNameExposure)
	if r.Status != RuleStatusAct || !r.ObservedIsLowerBound {
		t.Fatalf("stock 49%% + delta-less long call = %s (lower_bound=%v), want act lower bound (evidence: %s)", r.Status, r.ObservedIsLowerBound, r.Evidence)
	}
	if !strings.Contains(r.Evidence, "lower bound") {
		t.Errorf("lower-bound act must say so: %s", r.Evidence)
	}

	// A delta-less long PUT makes the interval straddle zero: nothing
	// provable, the name stays a gap unknown (never a false act).
	in = healthyInputs()
	in.Names[0].ExposureBase = 120000
	in.Names[0].GreeksGapNotionalBase = 36000
	in.Names[0].Legs[1] = LegInput{Desc: "NOW 20260821 P 100", Right: "P", Strike: 100,
		Expiry: etDate(2026, 8, 21), DTE: 45, Quantity: 50, Multiplier: 100, Mark: 7.86,
		Underlying: new(108.0), MarketValueBase: 36000, FXToBase: new(0.9)}
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleSingleNameExposure)
	if r.Status != RuleStatusUnknown || r.ObservedIsLowerBound {
		t.Errorf("delta-less long put must block the bound: %s (lower_bound=%v)", r.Status, r.ObservedIsLowerBound)
	}

	// Missing FX on the delta-less leg: unbounded, no assertion.
	in = healthyInputs()
	in.Names[0].ExposureBase = 120000
	in.Names[0].GreeksGapNotionalBase = 36000
	in.Names[0].Legs[1].Delta = nil
	in.Names[0].Legs[1].FXToBase = nil
	ev = EvaluateRulebook(in, pol)
	if got := rowByID(t, ev, RuleSingleNameExposure).Status; got != RuleStatusUnknown {
		t.Errorf("delta-less leg without FX = %s, want unknown", got)
	}

	// An incomplete known sum (aggregator excluded a priced leg) blocks the
	// bound: "proven ≥" must never build on a partial ExposureBase.
	in = healthyInputs()
	in.Names[0].ExposureBase = 120000
	in.Names[0].GreeksGapNotionalBase = 36000
	in.Names[0].Legs[1].Delta = nil
	in.Names[0].ExposureBaseComplete = false
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleSingleNameExposure)
	if r.Status != RuleStatusUnknown || r.ObservedIsLowerBound {
		t.Errorf("incomplete exposure sum must block the bound: %s (lower_bound=%v)", r.Status, r.ObservedIsLowerBound)
	}
}

// Rule 13: the loss fence bites at 40/60 of premium paid; hedge legs are
// exempt (decay is the cost of protection).
func TestExitDiscipline(t *testing.T) {
	pol := DefaultRulebookPolicy()

	in := healthyInputs()
	in.Names[1].Legs[0].CostBasisBase = new(64000.0) // BB at -46.9% ⇒ watch
	ev := EvaluateRulebook(in, pol)
	r := rowByID(t, ev, RuleExitDiscipline)
	if r.Status != RuleStatusWatch {
		t.Fatalf("-46.9%% line = %s, want watch (evidence: %s)", r.Status, r.Evidence)
	}

	in.Names[1].Legs[0].CostBasisBase = new(100000.0) // -66% ⇒ act
	ev = EvaluateRulebook(in, pol)
	if got := rowByID(t, ev, RuleExitDiscipline).Status; got != RuleStatusAct {
		t.Errorf("-66%% line = %s, want act", got)
	}

	// The SPY hedge at -70% stays exempt: rule 12 owns hedge decay.
	in = healthyInputs()
	in.Names[3].Legs[0].CostBasisBase = new(127000.0)
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleExitDiscipline)
	if r.Status != RuleStatusPass {
		t.Errorf("decayed hedge leg = %s, want pass with exemption", r.Status)
	}
	found := false
	for _, e := range r.Exempt {
		if e.Symbol == "SPY" {
			found = true
		}
	}
	if !found {
		t.Errorf("hedge exemption must be disclosed, exempt = %+v", r.Exempt)
	}
}

// Every new policy field must move the fingerprint — a threshold outside the
// fingerprint is a silent policy change.
func TestPolicyFingerprintCoversNewFields(t *testing.T) {
	base := DefaultRulebookPolicy().FingerprintKey()
	mutations := []func(*RulebookPolicy){
		func(p *RulebookPolicy) { p.HedgeLineWatchPct = 16 },
		func(p *RulebookPolicy) { p.HedgeLineActPct = 26 },
		func(p *RulebookPolicy) { p.ShortPutActLinePctNLV = 11 },
		func(p *RulebookPolicy) { p.ShortPutActNamePctNLV = 21 },
		func(p *RulebookPolicy) { p.RegimeCalm.CashSellOnlyPct = -20 },
		func(p *RulebookPolicy) { p.RegimeEarlyWarning.ExtrinsicWatchPct = 8 },
		func(p *RulebookPolicy) { p.RegimeConfirmed.HedgeBandMaxPct = 75 },
		func(p *RulebookPolicy) { p.RegimeStageMaxAgeMinutes = 300 },
		func(p *RulebookPolicy) { p.ExitWatchLossPct = 45 },
		func(p *RulebookPolicy) { p.ExitActLossPct = 70 },
		func(p *RulebookPolicy) { p.FXExposureWatchPct = 65 },
	}
	for i, mut := range mutations {
		p := DefaultRulebookPolicy()
		mut(&p)
		if p.FingerprintKey() == base {
			t.Errorf("mutation %d did not change the policy fingerprint", i)
		}
	}
}

func TestPolicyFingerprintIdentity(t *testing.T) {
	basePolicy := DefaultRulebookPolicy()
	base := basePolicy.FingerprintKey()
	if !strings.HasPrefix(base, "sha256:") {
		t.Fatalf("fingerprint = %q, want sha256 prefix", base)
	}
	if got := basePolicy.FingerprintKey(); got != base {
		t.Fatalf("second fingerprint = %q, want deterministic %q", got, base)
	}

	mutations := map[string]func(*RulebookPolicy){
		"scalar threshold": func(p *RulebookPolicy) { p.SingleNameWatchPct += 0.00001 },
		"regime threshold": func(p *RulebookPolicy) { p.RegimeConfirmed.ExtrinsicActPct += 0.00001 },
		"hedge list":       func(p *RulebookPolicy) { p.HedgeSymbols = append(p.HedgeSymbols, "DIA") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := DefaultRulebookPolicy()
			mutate(&p)
			if got := p.FingerprintKey(); got == base {
				t.Fatalf("mutated fingerprint = %q, want a changed key", got)
			}
		})
	}

	reordered := DefaultRulebookPolicy()
	slices.Reverse(reordered.HedgeSymbols)
	if got := reordered.FingerprintKey(); got != base {
		t.Fatalf("reordered hedge list fingerprint = %q, want %q", got, base)
	}
}

func TestOptionMathHelpers(t *testing.T) {
	if got := OptionIntrinsicPerShare("C", 108, 130); got != 0 {
		t.Errorf("OTM call intrinsic = %v, want 0", got)
	}
	if got := OptionIntrinsicPerShare("P", 700, 710); got != 10 {
		t.Errorf("ITM put intrinsic = %v, want 10", got)
	}
	if _, ok := OptionExtrinsicPerShare("C", nil, 130, 0.44); ok {
		t.Error("extrinsic with nil underlying must be uncomputable, not zero")
	}
	if ext, ok := OptionExtrinsicPerShare("C", new(140.0), 130, 8.0); !ok || ext != 0 {
		t.Errorf("stale mark below intrinsic: extrinsic = %v ok=%v, want 0 true", ext, ok)
	}
	if _, ok := OptionSpreadPct(new(1.3), new(1.2)); ok {
		t.Error("crossed quote must not produce a spread")
	}
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// TestDesignDocDisclosesRule1HedgeExemption pins the rule-1 hedge-exemption
// wording in docs/design/trading-rulebook.md to the semantics implemented
// here, so the design doc cannot silently lag another rule-1 change (the
// trading-paper-smoke drift-guard precedent). The blank reference keeps the
// doc's predicate name honest: renaming rule12HedgeLeg breaks this file and
// points at the doc pin that must move with it.
func TestDesignDocDisclosesRule1HedgeExemption(t *testing.T) {
	t.Parallel()
	_ = rule12HedgeLeg

	data, err := os.ReadFile("../../docs/design/trading-rulebook.md")
	if err != nil {
		t.Fatalf("read design doc: %v", err)
	}
	// Collapse the doc's hard wrapping so pins can span line breaks.
	doc := strings.Join(strings.Fields(string(data)), " ")
	for _, pin := range []string{
		// Exemption scope is exactly what rule 12 can size, via the shared predicate.
		"rule12HedgeLeg",
		// Short exposure the sized legs don't cover keeps ranking as concentration.
		"Residual short beyond the sized legs stays a concentration offender",
		// Hedge-symbol shorts with no sizeable legs are never exempted.
		"no Exempt row",
	} {
		if !strings.Contains(doc, pin) {
			t.Errorf("docs/design/trading-rulebook.md no longer states %q — rule-1 hedge-exemption semantics changed without updating the design doc", pin)
		}
	}
}

func TestDesignDocTracksRulebookRuntimeContract(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../../docs/design/trading-rulebook.md")
	if err != nil {
		t.Fatalf("read design doc: %v", err)
	}
	doc := strings.Join(strings.Fields(string(data)), " ")
	for _, pin := range []string{
		"compiled `rulebook-v2`",
		"one-minute canonical refresh",
		"up to 75 seconds",
		"≥ 60%",
		"source-neutral alert",
	} {
		if !strings.Contains(doc, pin) {
			t.Errorf("docs/design/trading-rulebook.md missing current runtime contract %q", pin)
		}
	}
	for _, stale := range []string{
		"No new scheduler",
		"cached eval ≤45s",
		"operator-tunable thresholds",
	} {
		if strings.Contains(doc, stale) {
			t.Errorf("docs/design/trading-rulebook.md retained stale runtime claim %q", stale)
		}
	}
}

func TestRulebookDesignDocDiscoverable(t *testing.T) {
	t.Parallel()

	for path, link := range map[string]string{
		"../../README.md": "(docs/design/trading-rulebook.md)",
		"../../docs/docs/internals/architecture.md": "(../../design/trading-rulebook.md)",
		"../../docs/index.html":                     `href="docs/understand/sensors.html#rulebook"`,
		"../../docs/docs/understand/sensors.md":     "(../../design/trading-rulebook.md)",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if !strings.Contains(string(data), link) {
			t.Errorf("%s must link the Rulebook authority as %q", path, link)
		}
	}
}
