package stress

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// rule15 builds a measured rule 15 reading with the default bands.
func rule15(status string, pct float64) *rpc.StressNetExposure {
	return &rpc.StressNetExposure{Status: status, PctNLV: new(pct), Direction: "long", WatchPct: new(100.0), ActPct: new(150.0)}
}

func netExposureSummary(n *rpc.StressNetExposure) StressPortfolioSummary {
	return StressPortfolioSummary{NetExposure: n, GrossExposurePctNLV: new(60.0), GrossDeltaPctNLV: new(60.0)}
}

var confirmedStress = StressMarketSummary{EligibleRedClusters: 2}

// netDeltaBook is a calm, fully current book whose positions aggregate
// carries 130% of NLV in net dollar delta: above the retired stress watch of
// 125, but only at rule 15's watch band.
func netDeltaBook(n *rpc.StressNetExposure) StressInput {
	net := 130_000.0
	return StressInput{
		Now:     stressTestNow,
		Account: baseStressAccount(),
		Positions: rpc.PositionsResult{AsOf: stressTestNow, Portfolio: &rpc.PositionsPortfolio{
			DollarDeltaBase: &net,
			ExposureBase:    []rpc.UnderlyingExposure{{Underlying: "AAA", DollarDeltaBase: new(net), MarketValuePctNLV: new(60.0)}},
		}},
		Regime:      healthyStressRegime(),
		NetExposure: n,
	}
}

// The stress read's net exposure is rule 15's (amendment 16). A book whose
// positions aggregate shows 130% net delta, which the retired stress watch of
// 125 flagged, sits at rule 15's watch band: in a calm market that is no
// stress signal, and the figure served is rule 15's.
func TestStressNetExposureReadsRule15NotThePositionsAggregate(t *testing.T) {
	t.Parallel()
	res := ComputeStress(netDeltaBook(rule15(risk.RuleStatusWatch, 112)))
	if hasSignal(res.Signals, risk.SignalNetDeltaHigh) {
		t.Fatalf("rule 15 at watch in a calm market raised net_delta_high: %+v", res.Signals)
	}
	if got := res.Portfolio.NetDeltaPctNLV; got == nil || *got != 112 {
		t.Fatalf("net_delta_pct_nlv = %v, want rule 15's 112, not the positions aggregate's 130", got)
	}
	row := stressRowByTitle(res.Rows, "US equity/options exposure")
	if row == nil || row.Severity != risk.SeverityObserve ||
		!strings.Contains(row.Evidence, "net exposure long 112.0% NLV (Rulebook watch 100%, act 150%)") {
		t.Fatalf("exposure row = %+v", row)
	}
	if n := res.Portfolio.NetExposure; n == nil || n.Status != risk.RuleStatusWatch || *n.WatchPct != 100 || *n.ActPct != 150 {
		t.Fatalf("portfolio.net_exposure = %+v", n)
	}
}

// Without a rule 15 measurement the stress read has no net figure and never
// reads the book as within its net-exposure limit, whatever the positions
// aggregate shows.
func TestStressNetExposureWithoutARule15ReadingIsNotAPass(t *testing.T) {
	t.Parallel()
	for name, n := range map[string]*rpc.StressNetExposure{
		"absent":       nil,
		"rulebook off": {Reason: "the Rulebook is turned off"},
		"no row":       {Reason: "the Rulebook result has no rule 15 row"},
		"unknown":      {Status: risk.RuleStatusUnknown, RuleReason: "greeks_gap", WatchPct: new(100.0), ActPct: new(150.0)},
	} {
		res := ComputeStress(netDeltaBook(n))
		if res.Portfolio.NetDeltaPctNLV != nil {
			t.Errorf("%s: net_delta_pct_nlv = %v, want absent without a rule 15 measurement", name, *res.Portfolio.NetDeltaPctNLV)
		}
		if hasSignal(res.Signals, risk.SignalNetDeltaHigh) {
			t.Errorf("%s: raised net_delta_high without a rule 15 measurement", name)
		}
		row := stressRowByTitle(res.Rows, "US equity/options exposure")
		if row == nil || row.Direction != risk.DirectionDataQuality || row.Severity != risk.SeverityWatch {
			t.Errorf("%s: exposure row = %+v, want a data-quality watch", name, row)
		}
	}
	// A rule the owner turned off is not assessed rather than a data gap.
	off := ComputeStress(netDeltaBook(&rpc.StressNetExposure{Status: risk.RuleStatusNotEvaluated, RuleReason: "rule_off"}))
	row := stressRowByTitle(off.Rows, "US equity/options exposure")
	if row == nil || row.Severity != risk.SeverityObserve || !strings.Contains(row.Evidence, "net exposure not assessed (Rulebook rule 15 is off)") {
		t.Fatalf("rule 15 off: exposure row = %+v", row)
	}
}

// In a calm market only rule 15's act band is a stress watch; confirmed
// stress moves the reading one band up, so rule 15's watch band acts and its
// act band is urgent. The retired stress act level of 80 is gone: 90% of NLV
// in confirmed stress raises nothing.
func TestStressNetExposureEscalatesOneRule15BandInConfirmedStress(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		n         *rpc.StressNetExposure
		market    StressMarketSummary
		severity  risk.SignalSeverity
		threshold float64
		direction risk.SignalDirection
	}{
		{name: "calm watch", n: rule15(risk.RuleStatusWatch, 130), market: StressMarketSummary{}},
		{name: "calm act", n: rule15(risk.RuleStatusAct, 155), market: StressMarketSummary{}, severity: risk.SeverityWatch, threshold: 150, direction: risk.DirectionRebalance},
		{name: "stress pass at 90", n: rule15(risk.RuleStatusPass, 90), market: confirmedStress},
		{name: "stress watch", n: rule15(risk.RuleStatusWatch, 110), market: confirmedStress, severity: risk.SeverityAct, threshold: 100, direction: risk.DirectionDefensive},
		{name: "stress act", n: rule15(risk.RuleStatusAct, 160), market: confirmedStress, severity: risk.SeverityUrgent, threshold: 150, direction: risk.DirectionDefensive},
	}
	for _, tc := range cases {
		p := netExposureSummary(tc.n)
		sigs := stressExposureSignals(p, tc.market)
		sig, raised := findSignal(sigs, risk.SignalNetDeltaHigh)
		row := stressExposureRow(p, tc.market)
		if tc.severity == "" {
			if raised {
				t.Errorf("%s: raised %+v, want nothing", tc.name, sig)
			}
			if row.Severity != risk.SeverityObserve {
				t.Errorf("%s: exposure row = %+v, want observe", tc.name, row)
			}
			continue
		}
		if !raised || sig.Severity != tc.severity || sig.Direction != tc.direction || sig.Threshold == nil || *sig.Threshold != tc.threshold ||
			sig.Observed == nil || *sig.Observed != *tc.n.PctNLV {
			t.Errorf("%s: net_delta_high = %+v (raised %v), want %s/%s at threshold %v", tc.name, sig, raised, tc.direction, tc.severity, tc.threshold)
		}
		if row.Severity != tc.severity || row.Direction != tc.direction {
			t.Errorf("%s: exposure row = %s/%s, want %s/%s", tc.name, row.Direction, row.Severity, tc.direction, tc.severity)
		}
	}
}

// The stress read quotes and compares the owner's rule 15 bands, whatever
// they are, and a proven lower bound indicts at medium confidence.
func TestStressNetExposureUsesTheOwnersRule15Bands(t *testing.T) {
	t.Parallel()
	own := &rpc.StressNetExposure{Status: risk.RuleStatusWatch, PctNLV: new(70.0), IsLowerBound: true, Direction: "short", WatchPct: new(60.0), ActPct: new(90.0)}
	p := netExposureSummary(own)
	sig, ok := findSignal(stressExposureSignals(p, confirmedStress), risk.SignalNetDeltaHigh)
	if !ok || sig.Severity != risk.SeverityAct || *sig.Threshold != 60 || sig.Confidence != "medium" {
		t.Fatalf("owner bands in stress: %+v (raised %v), want act at 60 with medium confidence", sig, ok)
	}
	row := stressExposureRow(p, confirmedStress)
	if !strings.Contains(row.Evidence, "net exposure short ≥ 70.0% NLV (Rulebook watch 60%, act 90%)") {
		t.Fatalf("exposure evidence = %q", row.Evidence)
	}
}
