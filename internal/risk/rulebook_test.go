package risk

import (
	"github.com/BurntSushi/toml"

	"strconv"
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
		AsOf:               now,
		BaseCurrency:       "EUR",
		Positions:          SourceState{Healthy: true},
		Account:            SourceState{Healthy: true},
		NLVBase:            new(245000.0),
		CashBase:           new(-62000.0),
		AvailableFundsBase: new(196000.0),
		DailyPnLBase:       new(9700.0),
		SessionOpen:        true,
		SPYDayChangePct:    new(1.0),
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
		in.NLVBase, in.CashBase, in.AvailableFundsBase, in.DailyPnLBase = nil, nil, nil, nil
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
		row := rowByID(t, ev, RuleRedOnGreen)
		if row.Status != RuleStatusNotEvaluated || row.Reason != RuleReasonRuleOff {
			t.Errorf("red_on_green = %s/%s, want off", row.Status, row.Reason)
		}
	})
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

func TestExtremeIndexPutPositionIsDirectional(t *testing.T) {
	in := healthyInputs()

	spy := &in.Names[3].Legs[0]
	spy.Delta = new(-0.60)
	spy.DTE = 10
	spy.Expiry = etDate(2026, 7, 17)
	ev := EvaluateRulebook(in, DefaultRulebookPolicy())

	hedge := rowByID(t, ev, RuleHedgeIntegrity)

	if hedge.Status != RuleStatusNotEvaluated || hedge.Reason != RuleReasonNoProtection || !strings.Contains(hedge.Evidence, "directional short") {
		t.Fatalf("protection row = %s/%s (%s), want directional short", hedge.Status, hedge.Reason, hedge.Evidence)
	}
	exposure := rowByID(t, ev, RuleSingleNameExposure)
	found := false
	for _, o := range exposure.Offenders {
		if o.Symbol == "SPY" && strings.Contains(o.Note, "directional") {
			found = true
		}
	}
	if !found {
		t.Fatalf("directional SPY exposure must follow the ordinary concentration rule: %+v", exposure)
	}
}

func TestIndexPutRoleClassificationDoesNotMutateInputs(t *testing.T) {
	in := healthyInputs()
	before := in.Names[3].Legs[0].IndexPutRole
	_ = EvaluateRulebook(in, DefaultRulebookPolicy())
	if got := in.Names[3].Legs[0].IndexPutRole; got != before {
		t.Fatalf("EvaluateRulebook mutated caller input role from %q to %q", before, got)
	}
}

func TestRegimeConditionalThresholds(t *testing.T) {
	pol := DefaultRulebookPolicy()

	t.Run("cash reserve uses available funds", func(t *testing.T) {
		in := healthyInputs()
		in.AvailableFundsBase = new(171500.0)
		ev := EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleCashSellOnly).Status; got != RuleStatusWatch {
			t.Errorf("available funds at 70%% = %s, want watch", got)
		}
		in.AvailableFundsBase = new(196000.0)
		ev = EvaluateRulebook(in, pol)
		if got := rowByID(t, ev, RuleCashSellOnly).Status; got != RuleStatusPass {
			t.Errorf("available funds at 80%% = %s, want pass", got)
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

	t.Run("never-seen stage uses calm with disclosure", func(t *testing.T) {
		in := healthyInputs()
		ev := EvaluateRulebook(in, pol)
		r := rowByID(t, ev, RuleExtrinsicBudget)
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

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
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

func TestBuildAlertEpisodeKeyIsOpaqueStableAndDomainSeparated(t *testing.T) {
	const sensitive = "ACCOUNT-SECRET/ORDER-SECRET/SYMBOL-SECRET"
	first, err := BuildAlertEpisodeKey(AlertSourceRulebook, AlertKindPortfolioRisk, "  "+sensitive+"  ", "concentration")
	if err != nil {
		t.Fatal(err)
	}
	again, err := BuildAlertEpisodeKey(AlertSourceRulebook, AlertKindPortfolioRisk, sensitive, "concentration")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("episode key is not stable across boundary whitespace: %q != %q", first, again)
	}
	if strings.Contains(first, sensitive) || !strings.HasPrefix(first, alertEpisodeKeyPrefix) || len(first) != len(alertEpisodeKeyPrefix)+64 {
		t.Fatalf("episode key is not opaque: %q", first)
	}

	variants := []struct {
		source AlertSource
		kind   AlertKind
		parts  []string
	}{
		{AlertSourceRegime, AlertKindPortfolioRisk, []string{sensitive, "concentration"}},
		{AlertSourceRulebook, AlertKindMarketState, []string{sensitive, "concentration"}},
		{AlertSourceRulebook, AlertKindPortfolioRisk, []string{"concentration", sensitive}},
		{AlertSourceRulebook, AlertKindPortfolioRisk, []string{sensitive, "different"}},
	}
	for _, variant := range variants {
		got, err := BuildAlertEpisodeKey(variant.source, variant.kind, variant.parts...)
		if err != nil {
			t.Fatal(err)
		}
		if got == first {
			t.Fatalf("domain-separated identity collided for %#v", variant)
		}
	}
}

func TestAlertSnapshotClearRequiresCompleteCurrentCoverage(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)

	clear := validAlertSnapshot(now)
	if err := clear.Validate(); err != nil || !clear.IsClear() {
		t.Fatalf("complete current empty snapshot was not clear: err=%v snapshot=%#v", err, clear)
	}

	partial := clear
	partial.Sources = append([]AlertSourceCoverage(nil), clear.Sources...)
	partial.CurrentState = AlertSnapshotUnknown
	partial.Coverage.State = AlertCoveragePartial
	partial.Coverage.CoveredSources = []AlertSource{AlertSourceStress}
	partial.Sources[1].Covered = false
	partial.Sources[1].EvidenceHealth = AlertEvidenceUnavailable
	if err := partial.Validate(); err != nil || partial.IsClear() {
		t.Fatalf("partial empty snapshot did not remain unknown: err=%v snapshot=%#v", err, partial)
	}
	falseClear := partial
	falseClear.CurrentState = AlertSnapshotClear
	if err := falseClear.Validate(); err == nil || falseClear.IsClear() {
		t.Fatal("partial empty snapshot claimed clear")
	}

	stale := clear
	stale.CurrentState = AlertSnapshotUnknown
	stale.Coverage.Freshness = AlertCoverageStale
	if err := stale.Validate(); err != nil || stale.IsClear() {
		t.Fatalf("stale empty snapshot did not remain unknown: err=%v snapshot=%#v", err, stale)
	}

	misdatedCurrent := clear
	misdatedCurrent.Coverage.AsOf = now.Add(-time.Hour)
	if err := misdatedCurrent.Validate(); err == nil || misdatedCurrent.IsClear() {
		t.Fatal("current coverage with an older authority timestamp claimed clear")
	}

	active := partial
	active.CurrentState = AlertSnapshotActive
	active.Candidates = []AlertCandidate{validAlertCandidate(t, now)}
	if err := active.Validate(); err != nil || active.IsClear() {
		t.Fatalf("active partial snapshot failed: err=%v snapshot=%#v", err, active)
	}

	recovered := validAlertCandidate(t, now)
	recovered.State = AlertEpisodeRecovered
	recovered.EvidenceHealth = AlertEvidenceCurrent
	clear.Candidates = []AlertCandidate{recovered}
	if err := clear.Validate(); err != nil || !clear.IsClear() {
		t.Fatalf("current recovered occurrence should permit clear: err=%v snapshot=%#v", err, clear)
	}
}

func validAlertCandidate(t *testing.T, now time.Time) AlertCandidate {
	t.Helper()
	key, err := BuildAlertEpisodeKey(AlertSourceStress, AlertKindPortfolioRisk, "synthetic-book-condition")
	if err != nil {
		t.Fatal(err)
	}
	return AlertCandidate{
		EpisodeKey:          key,
		OccurrenceKey:       mustTestAlertOccurrenceKey(t, key, "occurrence-1"),
		EvidenceFingerprint: testAlertFingerprint("a"),
		Source:              AlertSourceStress,
		Kind:                AlertKindPortfolioRisk,
		PresentationCode:    AlertPresentationPortfolioStress,
		State:               AlertEpisodeOpen,
		Severity:            AlertSeverityWatch,
		EvidenceHealth:      AlertEvidenceCurrent,
		Destination:         AlertDestinationMonitor,
		EvidenceAsOf:        now.Add(-time.Minute),
		StateChangedAt:      now.Add(-2 * time.Minute),
		ObservedAt:          now,
	}
}

func mustTestAlertOccurrenceKey(t *testing.T, episodeKey string, identity string) string {
	t.Helper()
	key, err := BuildAlertOccurrenceKey(episodeKey, identity)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func completeAlertCoverage(now time.Time) AlertCoverage {
	return AlertCoverage{
		State:           AlertCoverageComplete,
		Freshness:       AlertCoverageCurrent,
		AsOf:            now,
		ExpectedSources: []AlertSource{AlertSourceStress, AlertSourceRegime},
		CoveredSources:  []AlertSource{AlertSourceStress, AlertSourceRegime},
	}
}

func validAlertSnapshot(now time.Time) AlertCandidateSnapshot {
	authority, err := BuildAlertAuthorityScope("DU-TEST", "paper")
	if err != nil {
		panic(err)
	}
	return AlertCandidateSnapshot{
		SchemaVersion: AlertCandidateSnapshotVersion, AuthorityScope: authority,
		AsOf:         now,
		CurrentState: AlertSnapshotClear,
		Coverage:     completeAlertCoverage(now),
		Sources: []AlertSourceCoverage{
			{Source: AlertSourceStress, Status: "current", Reason: "current", EvidenceHealth: AlertEvidenceCurrent, InputAsOf: now, ObservedAt: now, EvidenceAsOf: now, FreshUntil: now.Add(time.Minute), Covered: true},
			{Source: AlertSourceRegime, Status: "current", Reason: "current", EvidenceHealth: AlertEvidenceCurrent, InputAsOf: now, ObservedAt: now, EvidenceAsOf: now, FreshUntil: now.Add(time.Minute), Covered: true},
		},
		Candidates: []AlertCandidate{},
	}
}

func testAlertFingerprint(digit string) string {
	return alertEvidenceFingerprintPrefix + strings.Repeat(digit, 64)
}

func approvedConstitution() Constitution {
	return Constitution{
		Kind:          ConstitutionKind,
		SchemaVersion: 1,
		PolicyID:      "risk-constitution",
		PolicyVersion: 1,
		Capital: ConstitutionCapital{
			BaseCurrency:        "EUR",
			ProtectedFloor:      new(200000.0),
			DeclaredRiskCapital: new(50000.0),
			MaxEquityAgeMinutes: new(240),
			MaxUnreconciledDays: new(7),
		},
		Drawdown: ConstitutionDrawdown{
			WarnConsumedPct:  new(15.0),
			BlockConsumedPct: new(30.0),
			BlockEnforcement: EnforcementShadow,
		},
		Override: ConstitutionOverride{MaxDurationHours: new(24)},
		Recon: ConstitutionRecon{
			AmountTolerancePct:     new(0.5),
			AmountToleranceMin:     new(5.0),
			DateWindowBusinessDays: new(3),
			MaxReportAgeDays:       new(4),
		},
		Cadence: ConstitutionCadence{
			Morning: ConstitutionArtefact{Class: EnforcementAdvisory},
			EOD:     ConstitutionArtefact{Class: EnforcementAdvisory},
			Weekly:  ConstitutionArtefact{Class: EnforcementAdvisory},
		},
		Inventory: ConstitutionInventory{
			Rulebook: &ConstitutionPolicyPin{ID: "rulebook-v2", Version: "2"},
		},
	}
}

func approvedV3Constitution() Constitution {
	c := approvedConstitution()
	c.PolicyVersion = 3
	c.Recon.MaxEquityDivergencePct = new(1.25)
	return c
}

func approvedV4Constitution() Constitution {
	c := approvedV3Constitution()
	c.PolicyVersion = 4
	c.Cadence.Nudges = &ConstitutionNudgeCadence{
		Timezone:             new("Europe/Berlin"),
		ReconcileWarningDays: new(2),
	}
	c.Cadence.Monthly = &ConstitutionMonthlyCadence{
		Class:        new(EnforcementAdvisory),
		DayOfMonth:   new(1),
		NudgeAtLocal: new("09:00"),
	}
	return c
}

func TestConstitutionV4CadenceTOMLIsStrictAndVersioned(t *testing.T) {
	decode := func(t *testing.T, version int, cadence string) Constitution {
		t.Helper()
		input := "kind = \"ibkr.risk_policy\"\nschema_version = 1\npolicy_id = \"risk-constitution\"\npolicy_version = " + strconv.Itoa(version) + "\n" + cadence
		var c Constitution
		metadata, err := toml.Decode(input, &c)
		if err != nil {
			t.Fatal(err)
		}
		if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
			t.Fatalf("typed cadence left undecoded keys: %v", undecoded)
		}
		return c
	}
	fullCadence := `[cadence.nudges]
timezone = "Europe/Berlin"
reconcile_warning_days = 2

[cadence.monthly]
class = "advisory"
day_of_month = 1
nudge_at_local = "09:00"
`
	v4 := decode(t, 4, fullCadence)
	if err := v4.Validate(); err != nil {
		t.Fatalf("v4 authored cadence = %v", err)
	}
	for _, key := range v4.UnapprovedKeys() {
		if strings.HasPrefix(key, "cadence.nudges.") || strings.HasPrefix(key, "cadence.monthly.") {
			t.Fatalf("authored v4 cadence stayed unapproved: %s", key)
		}
	}

	v3 := decode(t, 3, fullCadence)
	if err := v3.Validate(); err == nil || !strings.Contains(err.Error(), "requires policy_version >= 4") {
		t.Fatalf("v3 parsed v4 cadence without targeted rejection: %v", err)
	}

	missing := decode(t, 4, `[cadence.nudges]
timezone = "Europe/Berlin"
`)
	for _, key := range missing.UnapprovedKeys() {
		if strings.HasPrefix(key, "cadence.") {
			t.Fatalf("absent v4 cadence key reported unapproved: %s", key)
		}
	}
}

func TestConstitutionInventoryRequireSignoffIsOptionalAndVersioned(t *testing.T) {
	decode := func(t *testing.T, version int, body string) Constitution {
		t.Helper()
		input := "kind = \"ibkr.risk_policy\"\nschema_version = 1\npolicy_id = \"risk-constitution\"\npolicy_version = " + strconv.Itoa(version) + "\n" + body
		var c Constitution
		metadata, err := toml.Decode(input, &c)
		if err != nil {
			t.Fatal(err)
		}
		if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
			t.Fatalf("typed inventory left undecoded keys: %v", undecoded)
		}
		return c
	}
	body := `[inventory]
require_signoff = true

[inventory.rulebook]
id = "rulebook-v2"
version = "2"
`
	v4 := decode(t, 4, body)
	if err := v4.Validate(); err != nil {
		t.Fatalf("v4 authored require_signoff = %v", err)
	}
	if !v4.SignoffRequired() {
		t.Fatal("authored require_signoff=true must require sign-off")
	}

	v3 := decode(t, 3, body)
	if err := v3.Validate(); err == nil || !strings.Contains(err.Error(), "requires policy_version >= 4") {
		t.Fatalf("v3 parsed inventory.require_signoff without targeted rejection: %v", err)
	}

	absent := approvedV4Constitution()
	if absent.SignoffRequired() {
		t.Fatal("absent require_signoff must default to no sign-off")
	}
	for _, key := range absent.UnapprovedKeys() {
		if strings.Contains(key, "require_signoff") {
			t.Fatalf("absent require_signoff reported unapproved: %s", key)
		}
	}

	authored := absent
	authored.Inventory.RequireSignoff = new(false)
	if authored.FingerprintKey() == absent.FingerprintKey() {
		t.Fatal("an authored require_signoff must be fingerprint-material")
	}
}

func TestGreeksGapOffSessionNoteExplainsExpectedUnknown(t *testing.T) {
	pol := DefaultRulebookPolicy()
	for _, open := range []bool{true, false} {
		in := healthyInputs()
		in.SessionOpen = open
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
		checked := 0
		for _, row := range ev.Rows {
			if row.Status != RuleStatusUnknown || (row.Reason != "greeks_gap" && row.Reason != "extrinsic_uncomputable") {
				continue
			}
			checked++
			noted := false
			for _, note := range row.Notes {
				if strings.Contains(note, "off-session") {
					noted = true
				}
			}
			if open && noted {
				t.Errorf("%s carries an off-session note during the open session", row.ID)
			}
			if !open && !noted {
				t.Errorf("%s is a greeks-driven unknown off-session but carries no expectation note", row.ID)
			}
		}
		if checked == 0 {
			t.Fatalf("fixture produced no greeks-driven unknowns (open=%v)", open)
		}
	}
}

func TestEvaluateMonthlyPulseAutomatesRoutineEvidence(t *testing.T) {
	c := approvedV4Constitution()
	zone, day, at := "UTC", 1, "09:00"
	c.Cadence.Nudges.Timezone = &zone
	c.Cadence.Monthly.DayOfMonth = &day
	c.Cadence.Monthly.NudgeAtLocal = &at
	due := time.Date(2026, time.August, 3, 9, 0, 0, 0, time.UTC)

	before := EvaluateMonthlyPulse(MonthlyPulseInput{Now: due.Add(-time.Minute), Cadence: c.Cadence, PolicyFingerprint: "sha256:policy", PolicyEvidenceReady: true})
	if before.Status != MonthlyPulseStatusNotDue {
		t.Fatalf("before due = %+v", before)
	}
	complete := EvaluateMonthlyPulse(MonthlyPulseInput{Now: due, Cadence: c.Cadence, PolicyFingerprint: "sha256:policy", PolicyEvidenceReady: true})
	if complete.Status != MonthlyPulseStatusCompleted || complete.Candidate != nil {
		t.Fatalf("routine current evidence must auto-complete without an operator candidate: %+v", complete)
	}
	blocked := EvaluateMonthlyPulse(MonthlyPulseInput{Now: due, Cadence: c.Cadence, PolicyFingerprint: "sha256:policy"})
	if blocked.Status != MonthlyPulseStatusBlocked || blocked.Candidate == nil || blocked.Candidate.State != NudgeStateOpen || blocked.Candidate.Severity != NudgeSeverityAct {
		t.Fatalf("missing evidence must return only an act-grade exception: %+v", blocked)
	}
}
