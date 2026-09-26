package stress

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func concentrationSummary(c *rpc.StressConcentration) StressPortfolioSummary {
	return StressPortfolioSummary{Concentration: c, LargestExposure: "AAA", LargestExposurePct: new(36.0), LargestDeltaExposure: "AAA", LargestDeltaPctNLV: new(36.0)}
}

// The stress read takes concentration from the Rulebook (amendment 15). A name
// worth 36% of NLV in market value and delta, whose worst-case loss and delta
// swing the Rulebook passes, is no concentration here; the retired stress
// watch of 35% flagged it.
func TestStressConcentrationFollowsTheRulebookVerdict(t *testing.T) {
	pass := &rpc.StressConcentration{Status: risk.RuleStatusPass, Issuer: "AAA", LossPctNLV: new(12.0), WatchPct: new(30.0), ActPct: new(40.0),
		DeltaStatus: risk.RuleStatusPass, DeltaPctNLV: new(29.0), DeltaWatchPct: new(30.0)}
	stressed := StressMarketSummary{EligibleRedClusters: 2}
	if row := stressConcentrationRow(concentrationSummary(pass), stressed); row.Severity != risk.SeverityObserve {
		t.Fatalf("a Rulebook pass must not flag concentration: %+v", row)
	}
	if sigs := stressConcentrationSignals(concentrationSummary(pass), stressed); len(sigs) != 0 {
		t.Fatalf("a Rulebook pass raised stress signals: %+v", sigs)
	}
}

// At rule 1's watch the stress read rebalances in calm markets and trims in
// confirmed stress, quoting the Rulebook's watch level (the issuer's own
// band, 20% for an illiquid issuer) as the level to cap it below.
func TestStressConcentrationQuotesTheRulebookWatchLevel(t *testing.T) {
	watch := &rpc.StressConcentration{Status: risk.RuleStatusWatch, Issuer: "AAA", LossPctNLV: new(34.0), WatchPct: new(30.0), ActPct: new(40.0),
		DeltaStatus: risk.RuleStatusPass, DeltaPctNLV: new(10.0), DeltaWatchPct: new(30.0)}
	calm := stressConcentrationRow(concentrationSummary(watch), StressMarketSummary{})
	if calm.Severity != risk.SeverityWatch || calm.Direction != risk.DirectionRebalance ||
		!strings.Contains(calm.Evidence, "AAA worst-case loss 34.0% NLV (Rulebook watch 30%, cap 40%)") {
		t.Fatalf("calm concentration row = %+v", calm)
	}
	stressed := StressMarketSummary{EligibleRedClusters: 2}
	row := stressConcentrationRow(concentrationSummary(watch), stressed)
	if row.Severity != risk.SeverityAct || !strings.Contains(row.Guidance, "cap it below 30% NLV in stress") {
		t.Fatalf("stressed concentration row = %+v", row)
	}
	sigs := stressConcentrationSignals(concentrationSummary(watch), stressed)
	if len(sigs) != 1 || sigs[0].ID != risk.SignalSingleNameExposureHigh || sigs[0].Metric != "worst_case_loss_pct_nlv" ||
		*sigs[0].Threshold != 30 || *sigs[0].Target != 30 || sigs[0].Subject != "AAA" {
		t.Fatalf("cap signal = %+v", sigs)
	}
	illiquid := *watch
	illiquid.LossPctNLV, illiquid.WatchPct, illiquid.ActPct = new(25.0), new(20.0), new(30.0)
	if row := stressConcentrationRow(concentrationSummary(&illiquid), stressed); !strings.Contains(row.Guidance, "cap it below 20% NLV") {
		t.Fatalf("illiquid issuer must quote its own watch band: %+v", row)
	}
}

// Rule 16's delta-swing watch alone flags the row and raises the delta signal
// against the delta-swing watch level.
func TestStressConcentrationReadsTheDeltaSwingWatch(t *testing.T) {
	delta := &rpc.StressConcentration{Status: risk.RuleStatusPass, Issuer: "AAA", LossPctNLV: new(10.0), WatchPct: new(30.0), ActPct: new(40.0),
		DeltaStatus: risk.RuleStatusWatch, DeltaIssuer: "BBB", DeltaPctNLV: new(33.0), DeltaWatchPct: new(30.0)}
	row := stressConcentrationRow(concentrationSummary(delta), StressMarketSummary{})
	if row.Severity != risk.SeverityWatch || !strings.Contains(row.Evidence, "BBB delta 33.0% NLV (Rulebook delta-swing watch 30%)") {
		t.Fatalf("delta swing row = %+v", row)
	}
	sigs := stressConcentrationSignals(concentrationSummary(delta), StressMarketSummary{})
	if len(sigs) != 1 || sigs[0].ID != risk.SignalSingleNameDeltaHigh || *sigs[0].Threshold != 30 || sigs[0].Subject != "BBB" {
		t.Fatalf("delta signal = %+v", sigs)
	}
}

// Without a Rulebook reading, or with rule 1 unknown, the row is a
// data-quality watch, never a clean pass.
func TestStressConcentrationWithoutARulebookReadingIsNotAPass(t *testing.T) {
	for name, c := range map[string]*rpc.StressConcentration{
		"absent":  nil,
		"off":     {Reason: "the Rulebook is turned off"},
		"unknown": {Status: risk.RuleStatusUnknown, WatchPct: new(30.0), ActPct: new(40.0), DeltaStatus: risk.RuleStatusPass},
	} {
		row := stressConcentrationRow(concentrationSummary(c), StressMarketSummary{})
		if row.Severity == risk.SeverityObserve || row.Direction != risk.DirectionDataQuality {
			t.Errorf("%s: row = %+v, want a data-quality watch", name, row)
		}
	}
	if got := rpc.StressConcentrationFromRules(nil); got.Reason == "" {
		t.Fatal("a missing Rulebook result must read as unavailable")
	}
}
