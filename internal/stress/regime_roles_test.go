package stress

import (
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// stressOn computes the stress read over the healthy fixture after shape has
// changed the regime rows it is about.
func stressOn(shape func(r *rpc.RegimeSnapshotResult)) StressResult {
	r := healthyStressRegime()
	shape(&r)
	return ComputeStress(StressInput{Account: baseStressAccount(), Positions: freshStressPositions(), Regime: r, Now: stressTestNow})
}

func stressRed(meta *rpc.RegimeIndicatorMeta, eligible bool) {
	meta.Band = "red"
	meta.Eligibility = &rpc.RegimeEligibility{Eligible: eligible}
}

func stressText(res StressResult) string {
	parts := []string{res.Summary}
	for _, row := range res.Rows {
		parts = append(parts, row.Title, row.Guidance, row.Evidence)
	}
	return strings.Join(parts, "\n")
}

// The stress read follows the regime's own list of what opens a market
// warning. A co-sign red with no stress witness beside it is context: it is
// not partial market pressure and nothing calls it an early warning.
func TestStressCoSignRedAloneIsNotMarketPressure(t *testing.T) {
	t.Parallel()
	if calm := stressOn(func(*rpc.RegimeSnapshotResult) {}); calm.MarketConfirmation != stressMarketNone {
		t.Fatalf("the healthy fixture reads market confirmation %q, want none", calm.MarketConfirmation)
	}

	coSign := stressOn(func(r *rpc.RegimeSnapshotResult) { stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true) })
	if coSign.MarketConfirmation != stressMarketNone {
		t.Fatalf("a proxy red alone reads market confirmation %q, want none", coSign.MarketConfirmation)
	}
	if text := stressText(coSign); strings.Contains(text, "arly warning") || strings.Contains(text, "arly market warning") || strings.Contains(text, "unconfirmed") {
		t.Fatalf("a proxy red alone is worded as a warning:\n%s", text)
	}
	if text := stressText(coSign); !strings.Contains(text, "context, not a stress reading on its own: credit") {
		t.Fatalf("the proxy red is not named as context:\n%s", text)
	}

	witness := stressOn(func(r *rpc.RegimeSnapshotResult) { stressRed(&r.VIXTermStructure.RegimeIndicatorMeta, false) })
	if witness.MarketConfirmation != stressMarketPartial {
		t.Fatalf("a first-day term inversion reads market confirmation %q, want partial", witness.MarketConfirmation)
	}
}

// "An early warning is flashing" names a witness that is red without
// confirming, never a co-sign.
func TestStressEarlyWarningWordsNameOnlyAWitness(t *testing.T) {
	t.Parallel()
	// An isolated VVIX red is a witness the vote holds at yellow; the proxy
	// beside it is a co-sign.
	res := stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.VolOfVol.RegimeIndicatorMeta, true)
		r.VolOfVol.Last = new(112.0)
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	})
	row := stressRowByTitle(res.Rows, "Market stress")
	if row == nil || !strings.Contains(row.Guidance, "An early warning is flashing (vol red, not confirmed)") {
		t.Fatalf("market row = %+v, want the early warning to name vol alone", row)
	}
	if !strings.Contains(row.Evidence, "red but unconfirmed: vol") || !strings.Contains(row.Evidence, "context, not a stress reading on its own: credit") {
		t.Fatalf("evidence = %q, want vol unconfirmed and credit as context", row.Evidence)
	}
}

// Three yellow clusters are a deteriorating tape only when a stress witness
// is among them, the same rule as the regime vote.
func TestStressDeterioratingTapeNeedsAWitness(t *testing.T) {
	t.Parallel()
	coSigns := stressOn(func(r *rpc.RegimeSnapshotResult) {
		r.HYGSPYDivergence.Band, r.USDJPY.Band, r.Breadth.Band = "yellow", "yellow", "yellow"
	})
	if stressRowByTitle(coSigns.Rows, "Deteriorating tape") != nil || coSigns.MarketConfirmation != stressMarketNone {
		t.Fatalf("three co-sign yellows read as a deteriorating tape (market confirmation %q)", coSigns.MarketConfirmation)
	}

	withWitness := stressOn(func(r *rpc.RegimeSnapshotResult) {
		r.VolOfVol.Band, r.HYGSPYDivergence.Band, r.Breadth.Band = "yellow", "yellow", "yellow"
	})
	if stressRowByTitle(withWitness.Rows, "Deteriorating tape") == nil || withWitness.MarketConfirmation != stressMarketPartial {
		t.Fatalf("a yellow VVIX with two co-sign yellows is not a deteriorating tape (market confirmation %q)", withWitness.MarketConfirmation)
	}
}

// The fast carry unwind reads the confirmed FX band. A yen red with breadth
// weak and no stress witness is two co-signs: no act-grade row, no signal,
// no confirmed market stress.
func TestStressCarryUnwindNeedsAStandingWitness(t *testing.T) {
	t.Parallel()
	coSigns := stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.USDJPY.RegimeIndicatorMeta, true)
		r.Breadth.Band = "yellow"
	})
	if _, fired := findSignal(coSigns.Signals, risk.SignalFXCarryUnwind); fired || coSigns.MarketConfirmation != stressMarketNone || stressRowByTitle(coSigns.Rows, "Fast carry unwind") != nil {
		t.Fatalf("a yen red with weak breadth and no witness: carry-unwind signal %v, market confirmation %q", fired, coSigns.MarketConfirmation)
	}

	withWitness := stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.VIXTermStructure.RegimeIndicatorMeta, true)
		stressRed(&r.USDJPY.RegimeIndicatorMeta, false)
		r.Breadth.Band = "yellow"
	})
	if _, fired := findSignal(withWitness.Signals, risk.SignalFXCarryUnwind); !fired || withWitness.MarketConfirmation != stressMarketConfirmed {
		t.Fatalf("a yen red beside a standing term inversion with weak breadth: carry-unwind signal %v, market confirmation %q", fired, withWitness.MarketConfirmation)
	}
}

// A co-sign row that is red while no witness stands is shown amber with the
// context sentence, keeping the figure its own gate asks for.
func TestStressCoSignIndicatorIsContext(t *testing.T) {
	t.Parallel()
	indicator := func(res StressResult, name string) StressMarketIndicator {
		i := slices.IndexFunc(res.MarketIndicators, func(row StressMarketIndicator) bool { return row.Name == name })
		if i < 0 {
			t.Fatalf("no %s indicator", name)
		}
		return res.MarketIndicators[i]
	}
	alone := indicator(stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	}), "HYG vs SPY")
	if alone.Status != "amber" || !strings.Contains(alone.Comment, "Context, not a stress reading on its own") {
		t.Fatalf("eligible proxy red alone: status %q, comment %q; want amber and the context sentence", alone.Status, alone.Comment)
	}

	shallow := indicator(stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, false)
		r.HYGSPYDivergence.Eligibility.Reasons = []string{"depth_below_min"}
	}), "HYG vs SPY")
	if strings.Contains(shallow.Comment, "Provisional") || !strings.Contains(shallow.Comment, "0.25% below the 50-day average") {
		t.Fatalf("shallow proxy red: comment %q; want context wording that keeps the 0.25%% floor", shallow.Comment)
	}

	// A red the served band holds through its exit hysteresis, after SPY has
	// fallen away from its high, is still the proxy's red.
	held := indicator(stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, false)
		r.HYGSPYDivergence.Eligibility.Reasons = []string{"depth_below_min"}
		r.HYGSPYDivergence.HYGPrice, r.HYGSPYDivergence.HYG50DMA = new(79.5), new(79.7)
		r.HYGSPYDivergence.SPYPrice, r.HYGSPYDivergence.SPY52WHigh = new(95.0), new(100.0)
	}), "HYG vs SPY")
	if held.Status != "amber" || strings.Contains(held.Comment, "Provisional") || !strings.Contains(held.Comment, "Context, not a stress reading on its own") {
		t.Fatalf("held proxy red: status %q, comment %q; want amber and the context sentence", held.Status, held.Comment)
	}

	counted := indicator(stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.VIXTermStructure.RegimeIndicatorMeta, true)
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	}), "HYG vs SPY")
	if counted.Status != "red" || strings.Contains(counted.Comment, "Context") {
		t.Fatalf("proxy red beside a standing witness: status %q, comment %q; want red and no context sentence", counted.Status, counted.Comment)
	}
}

// The proxy red beside a widening cash row is still context: the widening is
// the market pressure, and nothing says the proxy's red is waiting to confirm.
func TestStressProxyRedBesideCashWideningIsContext(t *testing.T) {
	t.Parallel()
	res := stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		stressRed(&r.Breadth.RegimeIndicatorMeta, true)
		r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.5)
		r.CreditSpreads.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}
	})
	if res.MarketConfirmation != stressMarketPartial {
		t.Fatalf("a widening cash row reads market confirmation %q, want partial", res.MarketConfirmation)
	}
	if text := stressText(res); strings.Contains(text, "red, not confirmed") || !strings.Contains(text, "context, not a stress reading on its own: breadth and credit") {
		t.Fatalf("the proxy and breadth reds are not worded as context:\n%s", text)
	}
}
