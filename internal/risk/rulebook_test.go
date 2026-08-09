package risk

import (
	"math"

	"slices"
	"strings"
	"testing"
	"time"
)

func etDate(y int, m time.Month, d int) time.Time {
	loc, _ := time.LoadLocation("America/New_York")
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

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

				Symbol: "SPY", ExposureBase: -80000, MarketValueBase: 38000, HasStockLeg: false, ExposureBaseComplete: true,
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
		RuleSingleNameExposure: RuleStatusAct,
		RuleOptionLinePremium:  RuleStatusAct,
		RuleCashSellOnly:       RuleStatusAct,
		RuleExtrinsicBudget:    RuleStatusAct,
		RuleExpiryRunway:       RuleStatusWatch,
		RuleCatalystCoverage:   RuleStatusWatch,
		RuleOverwriteEarnings:  RuleStatusAct,
		RuleEarningsSizeFreeze: RuleStatusUnknown,
		RuleRedOnGreen:         RuleStatusWatch,
		RuleWinnerTrim:         RuleStatusPass,
		RuleGreenDayAction:     RuleStatusInfo,
		RuleHedgeIntegrity:     RuleStatusAct,
		RuleExitDiscipline:     RuleStatusPass,
		RuleFXExposure:         RuleStatusWatch,
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
		in.Names = nil
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

	t.Run("exposure unmeasured", func(t *testing.T) {

		in := healthyInputs()
		for i := range in.Names {
			in.Names[i].ExposureBaseComplete = false
		}

		in.Names[2].StockDayChangePct = new(5.0)
		e := in.Earnings["MSFT"]
		e.SessionsUntil = new(2)
		in.Earnings["MSFT"] = e
		ev := EvaluateRulebook(in, pol)
		assertNoPass(t, ev, RuleSingleNameExposure, RuleEarningsSizeFreeze, RuleWinnerTrim, RuleHedgeIntegrity)
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

func TestEarningsUnknownReasonIsDisclosed(t *testing.T) {
	if got := earningsGapWord(EarningsInput{Reason: "conflicting_sources"}); got != "conflicting across providers" {
		t.Fatalf("conflict label = %q", got)
	}
	if got := earningsGapWord(EarningsInput{Reason: "no_date_published"}); got != "not published by the provider" {
		t.Fatalf("no-date label = %q", got)
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

func TestHedgeExemptionSuppressedWhenOverHedged(t *testing.T) {
	in := healthyInputs()

	spy := &in.Names[3].Legs[0]
	spy.Delta = new(-0.60)
	spy.DTE = 10
	spy.Expiry = etDate(2026, 7, 17)
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())

	hedge := rowByID(t, ev, RuleHedgeIntegrity)

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

func TestSingleNameExposureExemptsShortHedge(t *testing.T) {
	in := healthyInputs()
	in.Names[3].ExposureBase = -640000
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

	in.Names[3].ExposureBase = 640000
	ev = EvaluateRulebook(in, DefaultRulebookPolicy())
	row = rowByID(t, ev, RuleSingleNameExposure)
	if len(row.Offenders) == 0 || row.Offenders[0].Symbol != "SPY" {
		t.Fatalf("long index exposure must still count as concentration, got %+v", row.Offenders)
	}
}

func TestSingleNameExposureResidualBeyondSizedHedge(t *testing.T) {
	in := healthyInputs()

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

func TestRegimeConditionalThresholds(t *testing.T) {
	pol := DefaultRulebookPolicy()

	t.Run("fresh confirmed tightens the cash floor", func(t *testing.T) {
		in := healthyInputs()
		in.CashBase = new(-12250.0)
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

		in := healthyInputs()
		in.Names[3].Legs[0].Delta = new(-0.0908)
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

func TestOptionLinePremiumHedgeTier(t *testing.T) {
	pol := DefaultRulebookPolicy()

	base := func() RuleInputs {
		in := healthyInputs()

		in.Names[0].Legs = in.Names[0].Legs[:1]
		in.Names[1].Legs = nil
		return in
	}

	in := base()
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
	in.Names[3].Legs[0].MarketValueBase = 66000
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleOptionLinePremium)
	if r.Status != RuleStatusAct {
		t.Errorf("26.9%% hedge line = %s, want act", r.Status)
	}
	if r.Observed == nil || *r.Observed != 26.9 || r.Threshold == nil || *r.Threshold != pol.HedgeLineWatchPct {
		t.Errorf("hedge-tier act row observed/threshold = %v/%v, want 26.9/%.1f", r.Observed, r.Threshold, pol.HedgeLineWatchPct)
	}

	in = base()
	in.Names[3].Legs[0].Delta = nil
	in.Names[3].GreeksGapNotionalBase = 38000
	ev = EvaluateRulebook(in, pol)
	r = rowByID(t, ev, RuleOptionLinePremium)
	if r.Status != RuleStatusAct {
		t.Errorf("15.5%% unclassifiable hedge line = %s, want normal-tier act", r.Status)
	}
	if r.Observed == nil || *r.Observed != 15.5 || r.Threshold == nil || *r.Threshold != pol.OptionLineWatchPct {
		t.Errorf("unclassifiable normal-tier observed/threshold = %v/%v, want 15.5/%.1f", r.Observed, r.Threshold, pol.OptionLineWatchPct)
	}

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

func TestEarningsSizeFreezeGapPropagation(t *testing.T) {
	pol := DefaultRulebookPolicy()
	gapName := func(in *RuleInputs, sessions *int, known bool) {
		in.Names[1].GreeksGapNotionalBase = 34000
		e := EarningsInput{Known: known, Date: etDate(2026, 7, 9), SessionsUntil: sessions, Source: "fetched"}
		in.Earnings["BB"] = e

		in.Earnings["SPY"] = EarningsInput{Known: true, Date: etDate(2026, 7, 20), SessionsUntil: new(11), Source: "fetched"}
	}

	in := healthyInputs()
	gapName(&in, nil, false)
	ev := EvaluateRulebook(in, pol)
	r := rowByID(t, ev, RuleEarningsSizeFreeze)
	if r.Status != RuleStatusUnknown {
		t.Fatalf("gapped name with unknown earnings = %s, want unknown", r.Status)
	}

	in = healthyInputs()
	gapName(&in, new(2), true)
	ev = EvaluateRulebook(in, pol)
	if got := rowByID(t, ev, RuleEarningsSizeFreeze).Status; got != RuleStatusUnknown {
		t.Errorf("gapped name 2 sessions from earnings = %s, want unknown", got)
	}

	in = healthyInputs()
	gapName(&in, new(11), true)
	ev = EvaluateRulebook(in, pol)
	if got := rowByID(t, ev, RuleEarningsSizeFreeze).Status; got != RuleStatusPass {
		t.Errorf("gapped name 11 sessions out = %s, want pass (other names clean)", got)
	}
}

func TestExitDiscipline(t *testing.T) {
	pol := DefaultRulebookPolicy()

	in := healthyInputs()
	in.Names[1].Legs[0].CostBasisBase = new(64000.0)
	ev := EvaluateRulebook(in, pol)
	r := rowByID(t, ev, RuleExitDiscipline)
	if r.Status != RuleStatusWatch {
		t.Fatalf("-46.9%% line = %s, want watch (evidence: %s)", r.Status, r.Evidence)
	}

	in.Names[1].Legs[0].CostBasisBase = new(100000.0)
	ev = EvaluateRulebook(in, pol)
	if got := rowByID(t, ev, RuleExitDiscipline).Status; got != RuleStatusAct {
		t.Errorf("-66%% line = %s, want act", got)
	}

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

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestOptionLinePremiumUnconvertibleLegBlocksPass(t *testing.T) {
	pol := DefaultRulebookPolicy()

	quiet := func() RuleInputs {
		in := healthyInputs()

		in.Names[0].Legs = in.Names[0].Legs[:1]
		in.Names[0].Legs[0].MarketValueBase = 100
		in.Names[0].Legs[0].Delta = nil
		for i := 1; i < len(in.Names); i++ {
			in.Names[i].Legs = nil
		}
		return in
	}

	in := quiet()
	if got := rowByID(t, EvaluateRulebook(in, pol), RuleOptionLinePremium).Status; got != RuleStatusPass {
		t.Fatalf("baseline with only measured legs = %s, want pass (fixture must pass before the marker matters)", got)
	}

	in = quiet()
	in.Names[0].Legs[0].MarketValueBaseSource = MarketValueBaseSourceSubstituted
	r := rowByID(t, EvaluateRulebook(in, pol), RuleOptionLinePremium)
	if r.Status != RuleStatusUnknown {
		t.Fatalf("unconvertible leg = %s, want unknown — no pass by absence of data (evidence: %s)", r.Status, r.Evidence)
	}
	if r.Reason != "premium_unconvertible" {
		t.Errorf("reason = %q, want premium_unconvertible", r.Reason)
	}
	var named bool
	for _, o := range r.Offenders {
		if strings.Contains(o.Note, "no FX rate") {
			named = true
		}
	}
	if !named {
		t.Errorf("the unmeasured leg must be disclosed as an offender, got %+v", r.Offenders)
	}

	in = quiet()
	in.Names[0].Legs[0].MarketValueBase = 40000
	in.Names[1].Legs = []LegInput{{Desc: "FX-less", Quantity: 1, MarketValueBase: 100,
		MarketValueBaseSource: MarketValueBaseSourceSubstituted}}
	r = rowByID(t, EvaluateRulebook(in, pol), RuleOptionLinePremium)
	if r.Status != RuleStatusAct {
		t.Errorf("measured breach beside an unconvertible leg = %s, want act (breach not downgraded)", r.Status)
	}

	var disclosed bool
	for _, o := range r.Offenders {
		if o.Leg == "FX-less" {
			disclosed = true
		}
	}
	if !disclosed {
		t.Errorf("unconvertible leg must stay disclosed on an act row, offenders = %+v", r.Offenders)
	}

	for _, o := range r.Offenders {
		if o.Leg == "FX-less" && o.ImpactBase != 0 {
			t.Errorf("unmeasurable leg claimed ImpactBase %v, want 0", o.ImpactBase)
		}
	}
}

func TestSingleNameExposureUnmeasuredNameBlocksPass(t *testing.T) {
	pol := DefaultRulebookPolicy()
	quiet := func() RuleInputs {
		in := healthyInputs()
		in.Names = []NameInput{{Symbol: "AAA", ExposureBase: 10000, ExposureBaseComplete: true, HasStockLeg: true}}
		return in
	}

	if got := rowByID(t, EvaluateRulebook(quiet(), pol), RuleSingleNameExposure); got.Status != RuleStatusPass {
		t.Fatalf("baseline = %s, want pass (fixture must pass before the marker matters)", got.Status)
	}

	in := quiet()
	in.Names = append(in.Names, NameInput{Symbol: "FXLESS", ExposureBase: 0, ExposureBaseComplete: false, HasStockLeg: true})
	r := rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure)
	if r.Status != RuleStatusUnknown {
		t.Fatalf("unmeasured name = %s, want unknown — 0.0%% is absence of data, not a measurement (evidence: %s)", r.Status, r.Evidence)
	}
	if r.Reason != "exposure_incomplete" {
		t.Errorf("reason = %q, want exposure_incomplete", r.Reason)
	}
	var named bool
	for _, o := range r.Offenders {
		if o.Symbol == "FXLESS" && strings.Contains(o.Note, "not fully measured") {
			named = true
		}
	}
	if !named {
		t.Errorf("the unmeasured name must be disclosed as an offender, got %+v", r.Offenders)
	}

	in = quiet()
	in.Names[0].ExposureBase = 120000
	in.Names = append(in.Names, NameInput{Symbol: "FXLESS", ExposureBaseComplete: false, HasStockLeg: true})
	r = rowByID(t, EvaluateRulebook(in, pol), RuleSingleNameExposure)
	if r.Status != RuleStatusAct {
		t.Errorf("measured breach beside an unmeasured name = %s, want act (breach not downgraded)", r.Status)
	}
	var disclosed bool
	for _, o := range r.Offenders {
		if o.Symbol == "FXLESS" {
			disclosed = true
			if o.ImpactBase != 0 {
				t.Errorf("unmeasured name claimed ImpactBase %v, want 0", o.ImpactBase)
			}
		}
	}
	if !disclosed {
		t.Errorf("unmeasured name must stay disclosed on an act row, offenders = %+v", r.Offenders)
	}
}

func TestEarningsSizeFreezeUnmeasuredNameBlocksPass(t *testing.T) {
	pol := DefaultRulebookPolicy()
	base := func(sessions int, complete bool) RuleInputs {
		in := healthyInputs()
		in.Names = []NameInput{{Symbol: "ERN", ExposureBase: 1000, ExposureBaseComplete: complete, HasStockLeg: true}}
		in.Earnings = map[string]EarningsInput{"ERN": {Known: true, Date: etDate(2026, 7, 9), SessionsUntil: new(sessions), Source: "fetched"}}
		return in
	}

	if got := rowByID(t, EvaluateRulebook(base(2, true), pol), RuleEarningsSizeFreeze); got.Status != RuleStatusPass {
		t.Fatalf("baseline = %s, want pass (small measured name inside the window)", got.Status)
	}

	r := rowByID(t, EvaluateRulebook(base(2, false), pol), RuleEarningsSizeFreeze)
	if r.Status != RuleStatusUnknown {
		t.Fatalf("unmeasured size inside the freeze window = %s, want unknown (evidence: %s)", r.Status, r.Evidence)
	}
	var named bool
	for _, o := range r.Offenders {
		if o.Symbol == "ERN" && strings.Contains(o.Note, "exposure not fully measured") {
			named = true
		}
	}
	if !named {
		t.Errorf("the unmeasured name must be disclosed, got %+v", r.Offenders)
	}

	if got := rowByID(t, EvaluateRulebook(base(9, false), pol), RuleEarningsSizeFreeze); got.Status != RuleStatusPass {
		t.Errorf("unmeasured size with earnings provably outside the window = %s, want pass", got.Status)
	}
}

func TestWinnerTrimUnmeasuredNameBlocksPass(t *testing.T) {
	pol := DefaultRulebookPolicy()
	book := func(names ...NameInput) RuleInputs {
		in := healthyInputs()
		in.Names = names
		return in
	}
	measured := func(sym string, day, exposure float64) NameInput {
		return NameInput{Symbol: sym, ExposureBase: exposure, ExposureBaseComplete: true, HasStockLeg: true, StockDayChangePct: new(day)}
	}
	unmeasured := func(sym string, day float64) NameInput {
		return NameInput{Symbol: sym, ExposureBaseComplete: false, HasStockLeg: true, StockDayChangePct: new(day)}
	}

	if got := rowByID(t, EvaluateRulebook(book(measured("AAA", 5, 1000)), pol), RuleWinnerTrim); got.Status != RuleStatusPass {
		t.Fatalf("baseline = %s, want pass (measured winner under the floor)", got.Status)
	}

	r := rowByID(t, EvaluateRulebook(book(unmeasured("WIN", 5)), pol), RuleWinnerTrim)
	if r.Status != RuleStatusUnknown {
		t.Fatalf("winner up hard with unmeasured exposure = %s, want unknown (evidence: %s)", r.Status, r.Evidence)
	}
	if r.Reason != "exposure_incomplete" {
		t.Errorf("reason = %q, want exposure_incomplete", r.Reason)
	}

	if got := rowByID(t, EvaluateRulebook(book(unmeasured("FLAT", 1)), pol), RuleWinnerTrim); got.Status != RuleStatusPass {
		t.Errorf("unmeasured name below the day trigger = %s, want pass (true negative)", got.Status)
	}

	r = rowByID(t, EvaluateRulebook(book(measured("BIG", 6, 40000), unmeasured("WIN", 5)), pol), RuleWinnerTrim)
	if r.Status != RuleStatusWatch {
		t.Errorf("measured breach beside an unmeasured winner = %s, want watch (breach not downgraded)", r.Status)
	}
	var disclosed bool
	for _, o := range r.Offenders {
		if o.Symbol == "WIN" {
			disclosed = true
			if o.ImpactBase != 0 {
				t.Errorf("unmeasured winner claimed ImpactBase %v, want 0", o.ImpactBase)
			}
		}
	}
	if !disclosed {
		t.Errorf("unmeasured winner must stay disclosed on a watch row, offenders = %+v", r.Offenders)
	}
}

func TestHedgeIntegrityUnmeasuredNameBlocksVerdict(t *testing.T) {
	pol := DefaultRulebookPolicy()
	long := NameInput{Symbol: "LNG", ExposureBase: 100000, ExposureBaseComplete: true, HasStockLeg: true}
	hedge := NameInput{Symbol: "SPY", ExposureBase: -30000, ExposureBaseComplete: true, Legs: []LegInput{
		{Desc: "SPY 20261016 P 710", Right: "P", Quantity: 40, Multiplier: 100, Mark: 10,
			Underlying: new(750.0), Delta: new(-0.01), HedgeListed: true, MarketValueBase: 38000},
	}}

	in := healthyInputs()
	in.Names = []NameInput{long, hedge}
	if got := rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity); got.Status != RuleStatusPass {
		t.Fatalf("baseline = %s, want pass (30%% ratio inside the calm 25-35 band)", got.Status)
	}

	in.Names = append(in.Names, NameInput{Symbol: "FXLESS", ExposureBaseComplete: false, HasStockLeg: true})
	r := rowByID(t, EvaluateRulebook(in, pol), RuleHedgeIntegrity)
	if r.Status != RuleStatusUnknown {
		t.Fatalf("unmeasured name in the book = %s, want unknown (evidence: %s)", r.Status, r.Evidence)
	}
	if r.Reason != "exposure_incomplete" {
		t.Errorf("reason = %q, want exposure_incomplete", r.Reason)
	}
	var named bool
	for _, o := range r.Offenders {
		if o.Symbol == "FXLESS" && strings.Contains(o.Note, "not fully measured") {
			named = true
		}
	}
	if !named {
		t.Errorf("the unmeasured name must be disclosed, got %+v", r.Offenders)
	}
}

func TestNonIssuerSecurityRequiresTheTypedSource(t *testing.T) {
	for name, earnings := range map[string]EarningsInput{
		"flag without source":  {NonIssuerSecurity: true, Reason: EarningsReasonNonIssuerSecurity},
		"source without flag":  {Source: "security_type", Reason: EarningsReasonNonIssuerSecurity},
		"stale classification": {NonIssuerSecurity: true, Source: "security_type", Stale: true},
	} {
		t.Run(name, func(t *testing.T) {
			in := healthyInputs()
			in.Names = []NameInput{{Symbol: "IDXQ", ExposureBase: 150000, ExposureBaseComplete: true}}
			in.Earnings = map[string]EarningsInput{"IDXQ": earnings}
			ev := EvaluateRulebook(in, DefaultRulebookPolicy())
			row := rowByID(t, ev, RuleCatalystCoverage)
			if row.Status == RuleStatusNotEvaluated {
				t.Fatalf("%s exempted a name without a typed classification", name)
			}
		})
	}
}
