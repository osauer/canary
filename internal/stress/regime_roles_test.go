package stress

import (
	"encoding/json"
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
	if text := stressText(coSign); !strings.Contains(text, "context, not a stress reading on its own: ETF credit proxy") {
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
	// A VVIX red that may not confirm yet is a witness that warns; the proxy
	// beside it is a co-sign.
	res := stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.VolOfVol.RegimeIndicatorMeta, false)
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	})
	if !strings.HasPrefix(res.Summary, "An early market warning is flashing (vol red, not confirmed yet)") {
		t.Fatalf("summary = %q, want the early warning to name vol alone", res.Summary)
	}
	// The vol cluster is red with the proxy's cluster yellow beside it.
	row := stressRowByTitle(res.Rows, "Early stress filtered")
	if row == nil || !strings.Contains(row.Evidence, "red but unconfirmed: vol") || !strings.Contains(row.Evidence, "context, not a stress reading on its own: ETF credit proxy") {
		t.Fatalf("market row = %+v, want vol unconfirmed and the proxy as context", row)
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
	if alone.Status != "amber" || !strings.HasSuffix(alone.Comment, "; context, not a stress reading on its own") {
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
		r.HYGSPYDivergence.HYGClose, r.HYGSPYDivergence.SPYClose, r.HYGSPYDivergence.SPYClose52WHigh = new(79.5), new(95.0), new(100.0)
	}), "HYG vs SPY")
	if held.Status != "amber" || strings.Contains(held.Comment, "Provisional") || !strings.Contains(held.Comment, "; context, not a stress reading on its own") {
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
	if text := stressText(res); strings.Contains(text, "red, not confirmed") || !strings.Contains(text, "context, not a stress reading on its own: ETF credit proxy and breadth") {
		t.Fatalf("the proxy and breadth reds are not worded as context:\n%s", text)
	}
}

// Beside a standing witness a co-sign red that has yet to pass its own gate
// is waiting to confirm: red but unconfirmed, in the words the read used
// before, and not context.
func TestStressCoSignRedBesideAStandingWitnessIsUnconfirmed(t *testing.T) {
	t.Parallel()
	res := stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.VIXTermStructure.RegimeIndicatorMeta, true)
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, false)
	})
	row := stressRowByTitle(res.Rows, "Stress pending confirmation")
	if row == nil || !strings.Contains(row.Evidence, "red but unconfirmed: credit") || strings.Contains(stressText(res), "ontext, not a stress reading") {
		t.Fatalf("a proxy red waiting beside a standing term inversion is not worded as unconfirmed:\n%s", stressText(res))
	}
}

// A widening of cash credit spreads is named as the cause wherever a witness
// that warns without confirming is named, in the same sentences.
func TestStressNamesTheCashWidening(t *testing.T) {
	t.Parallel()
	widening := func(r *rpc.RegimeSnapshotResult) {
		r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.5)
		r.CreditSpreads.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}
	}
	alone := stressOn(widening)
	row := stressRowByTitle(alone.Rows, "Market stress")
	if !strings.HasPrefix(alone.Summary, "An early market warning is flashing (cash credit spreads widening, not confirmed yet)") ||
		row == nil || row.Guidance != "An early warning is flashing (cash credit spreads widening, not confirmed); below the de-risking trigger." ||
		!strings.Contains(row.Evidence, "1 yellow (credit); cash credit spreads widening") {
		t.Fatalf("a widening cash row is not named as the cause:\n%s", stressText(alone))
	}

	beside := stressOn(func(r *rpc.RegimeSnapshotResult) {
		widening(r)
		stressRed(&r.VolOfVol.RegimeIndicatorMeta, false)
	})
	if !strings.HasPrefix(beside.Summary, "An early market warning is flashing (vol red and cash credit spreads widening, not confirmed yet)") {
		t.Fatalf("a widening beside an isolated VVIX red: summary %q", beside.Summary)
	}
}

// The stress read serves what a surface may name beside the confirmed reds,
// with the reader's name and what it is, so the app words it without a name
// list or a rule of its own.
func TestStressServesTheUnconfirmedSignals(t *testing.T) {
	t.Parallel()
	served := func(shape func(r *rpc.RegimeSnapshotResult)) string {
		raw, err := json.Marshal(stressOn(shape))
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Market struct {
				Signals []struct{ Cluster, Name, Role string } `json:"unconfirmed_signals"`
			} `json:"market"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, signal := range got.Market.Signals {
			parts = append(parts, signal.Cluster+"/"+signal.Name+"/"+signal.Role)
		}
		return strings.Join(parts, ", ")
	}
	for _, tc := range []struct {
		name  string
		shape func(r *rpc.RegimeSnapshotResult)
		want  string
	}{
		{"nothing red", func(*rpc.RegimeSnapshotResult) {}, ""},
		{"proxy and breadth red, no witness", func(r *rpc.RegimeSnapshotResult) {
			stressRed(&r.Breadth.RegimeIndicatorMeta, true)
			stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, "credit/ETF credit proxy/context, breadth/breadth/context"},
		{"yen and gamma red, no witness", func(r *rpc.RegimeSnapshotResult) {
			stressRed(&r.USDJPY.RegimeIndicatorMeta, true)
			stressRed(&r.GammaZero.RegimeIndicatorMeta, true)
		}, "fx/FX carry proxy/context, gamma/dealer gamma/context"},
		{"cash credit red that may not confirm", func(r *rpc.RegimeSnapshotResult) {
			stressRed(&r.CreditSpreads.RegimeIndicatorMeta, false)
			stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
		}, "credit/cash credit spreads/witness, credit/ETF credit proxy/context"},
		{"VVIX red that may not confirm yet", func(r *rpc.RegimeSnapshotResult) {
			stressRed(&r.VolOfVol.RegimeIndicatorMeta, false)
		}, "vol/vol-of-vol/witness"},
		{"proxy red waiting beside a standing term inversion", func(r *rpc.RegimeSnapshotResult) {
			stressRed(&r.VIXTermStructure.RegimeIndicatorMeta, true)
			stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, false)
		}, "credit/ETF credit proxy/waiting"},
		{"cash credit widening with the proxy red", func(r *rpc.RegimeSnapshotResult) {
			stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
			r.CreditSpreads.Band, r.CreditSpreads.HY20DChange = "yellow", new(0.5)
			r.CreditSpreads.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}
		}, "credit/ETF credit proxy/context, credit/cash credit spreads widening/widening"},
	} {
		if got := served(tc.shape); got != tc.want {
			t.Errorf("%s: unconfirmed signals %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The credit cluster holds two rows with two roles. A cash red that may not
// confirm is a witness that warns; the proxy red beside it, with no witness
// standing, is context on its own row: amber with the context sentence, and
// named as context beside the witness. The sentence is served with it.
func TestStressProxyRedBesideACashRedThatMayNotConfirmIsContext(t *testing.T) {
	t.Parallel()
	res := stressOn(func(r *rpc.RegimeSnapshotResult) {
		stressRed(&r.CreditSpreads.RegimeIndicatorMeta, false)
		r.CreditSpreads.HYOAS = new(5.8)
		stressRed(&r.HYGSPYDivergence.RegimeIndicatorMeta, true)
	})
	i := slices.IndexFunc(res.MarketIndicators, func(row StressMarketIndicator) bool { return row.Name == "HYG vs SPY" })
	if i < 0 || res.MarketIndicators[i].Status != "amber" || !strings.Contains(res.MarketIndicators[i].Comment, "context, not a stress reading on its own") {
		t.Fatalf("proxy indicator %+v; want amber with the context sentence", res.MarketIndicators)
	}
	row := stressRowByTitle(res.Rows, "Market stress")
	if row == nil || !strings.Contains(row.Guidance, "(credit red, not confirmed)") ||
		!strings.Contains(row.Evidence, "context, not a stress reading on its own: ETF credit proxy") {
		t.Fatalf("market row %+v; want the cash red as the warning and the proxy as context", row)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var served struct {
		Market struct {
			ContextNote string `json:"context_note"`
		} `json:"market"`
	}
	if err := json.Unmarshal(raw, &served); err != nil || served.Market.ContextNote != "Context, not a stress reading on its own" {
		t.Fatalf("served context note %q (%v); want the sentence served with the context signals", served.Market.ContextNote, err)
	}
}
