package register

import (
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/risk"
)

// A synthetic catalogue: the core's rule and one entry rule, either of which
// a test may let send.
func testRules(coreSends, entrySends bool) []Rule {
	rules := Catalogue()
	rules[0].Sends = coreSends
	return append(rules, Rule{ID: "synthetic-v1", SpecID: "s9_synthetic", SpecHash: strings.Repeat("ab", 32), Form: FormEntry, Sends: entrySends, Bucket: "synthetic_rows"})
}

func coreEntry(stage, specHash string, scale float64) risk.StrategyEntry {
	return risk.StrategyEntry{ID: risk.CoreStrategyID, Form: risk.StrategyFormCore, Rule: core.RuleID, SpecHash: specHash, Sleeve: risk.CoreStrategyID,
		AllocationPct: new(6.0), AllocationKey: "mandate.sleeves.core_vol_pct", Stage: stage, ScalePct: new(scale)}
}

func entry(stage string, scale float64) risk.StrategyEntry {
	return risk.StrategyEntry{ID: "s9", Form: risk.StrategyFormEntry, Rule: "synthetic-v1", SpecHash: strings.Repeat("ab", 32), Sleeve: "s9",
		AllocationPct: new(2.0), AllocationKey: "mandate.strategies.s9.risk_allocation_pct", Stage: stage, ScalePct: new(scale)}
}

// Each row of the design's stage table (strategy-register.md §2).
func TestResolveStages(t *testing.T) {
	for _, tc := range []struct {
		name     string
		e        risk.StrategyEntry
		sends    bool
		state    State
		reserved float64
		scale    float64
		note     string
	}{
		{"core first form, no stage", coreEntry("", "", 25), false, StateShadow, 0, 25, "stage unwritten"},
		{"core shadow", coreEntry(risk.StageShadow, core.SpecHash, 25), true, StateShadow, 0, 25, ""},
		{"core live, build sends", coreEntry(risk.StageLive, core.SpecHash, 25), true, StateLive, 25, 25, ""},
		{"core scale 0 suspends", coreEntry(risk.StageLive, core.SpecHash, 0), true, StateSuspended, 0, 0, ""},
		{"core suspended", coreEntry(risk.StageSuspended, core.SpecHash, 25), true, StateSuspended, 0, 0, ""},
		{"entry shadow", entry(risk.StageShadow, 25), true, StateShadow, 0, 25, ""},
		{"entry live", entry(risk.StageLive, 50), true, StateLive, 50, 50, ""},
		{"entry suspended", entry(risk.StageSuspended, 25), true, StateSuspended, 0, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Resolve([]risk.StrategyEntry{tc.e}, testRules(tc.sends, tc.sends))[0]
			if got.State != tc.state || got.ReservedPct != tc.reserved || got.ScalePct != tc.scale || len(got.Reasons) != 0 {
				t.Fatalf("got %+v, want %s reserving %g at %g", got, tc.state, tc.reserved, tc.scale)
			}
			if got.Reserves() != (tc.reserved > 0) {
				t.Fatalf("Reserves() = %v with %g reserved", got.Reserves(), got.ReservedPct)
			}
			if tc.note != "" && !slices.ContainsFunc(got.Notes, func(n string) bool { return strings.Contains(n, tc.note) }) {
				t.Fatalf("notes %q lack %q", got.Notes, tc.note)
			}
		})
	}
}

// No entry written live runs as anything else (owner, 2026-10-18 21:05
// CEST: "we NEVER want shadow"): a rule this build serves no rows for holds
// the entry, named, and Refusals refuses the file before it loads. The
// first build's fallback ran it as shadow and said "this build sends no
// orders"; it is gone.
func TestLiveOnARuleWithoutRowsHoldsAndRefusesTheFile(t *testing.T) {
	for _, e := range []risk.StrategyEntry{coreEntry(risk.StageLive, core.SpecHash, 50), entry(risk.StageLive, 100)} {
		got := Resolve([]risk.StrategyEntry{e}, testRules(false, false))[0]
		if got.State != StateHold || got.Reserves() || !slices.ContainsFunc(got.Reasons, func(r string) bool { return strings.Contains(r, "serves no rows in this build") }) ||
			slices.ContainsFunc(got.Notes, func(n string) bool { return strings.Contains(n, "sends no orders") }) {
			t.Fatalf("%s: %+v", e.ID, got)
		}
		e.KeyPrefix = "mandate.strategies." + e.ID
		refused := Refusals([]risk.StrategyEntry{e}, testRules(false, false))
		if len(refused) != 1 || !strings.Contains(refused[0], "["+e.KeyPrefix+"] is written live, but rule "+e.Rule+" serves no rows in this build") {
			t.Fatalf("%s refusals %q", e.ID, refused)
		}
		if refused := Refusals([]risk.StrategyEntry{e}, testRules(true, true)); len(refused) != 0 {
			t.Fatalf("%s refused on a build that sends: %q", e.ID, refused)
		}
	}
	for _, r := range Catalogue() {
		if !r.Sends {
			t.Fatalf("rule %s serves no rows in this build", r.ID)
		}
	}
}

// A missing key, a rule not compiled, a hash that is not the rule's and a
// rule in the other form each hold the strategy, name why, and reserve
// nothing, even when its stage is live and the build sends.
func TestResolveHolds(t *testing.T) {
	missing := entry(risk.StageLive, 25)
	missing.Unwritten = []string{"mandate.strategies.s9.spec_hash"}
	unknown := entry(risk.StageLive, 25)
	unknown.Rule = "not-compiled-v1"
	mismatch := coreEntry(risk.StageLive, strings.Repeat("0", 64), 25)
	coreInEntry := entry(risk.StageLive, 25)
	coreInEntry.Rule = core.RuleID
	entryInCore := coreEntry(risk.StageLive, strings.Repeat("ab", 32), 25)
	entryInCore.Rule = "synthetic-v1"
	for name, tc := range map[string]struct {
		e    risk.StrategyEntry
		want string
	}{
		"missing key":                        {missing, "mandate.strategies.s9.spec_hash unwritten"},
		"not compiled":                       {unknown, "rule not-compiled-v1 is not compiled into this Canary"},
		"hash mismatch":                      {mismatch, "is not the spec rule " + core.RuleID + " implements"},
		"core's rule outside [mandate.core]": {coreInEntry, "runs only as [mandate.core]"},
		"another rule in [mandate.core]":     {entryInCore, "[mandate.core] runs only the core's rule"},
	} {
		t.Run(name, func(t *testing.T) {
			got := Resolve([]risk.StrategyEntry{tc.e}, testRules(true, true))[0]
			if got.State != StateHold || got.Reserves() || got.ScalePct != 0 {
				t.Fatalf("got %+v, want a hold that reserves nothing", got)
			}
			if !slices.ContainsFunc(got.Reasons, func(r string) bool { return strings.Contains(r, tc.want) }) {
				t.Fatalf("reasons %q lack %q", got.Reasons, tc.want)
			}
		})
	}
}

// The catalogue carries the core's rule as the core compiles it: sending
// (its monthly rows are served since phase 2) and never in the other form.
func TestCatalogueCarriesTheCoreRule(t *testing.T) {
	rule, ok := Find(Catalogue(), core.RuleID)
	if !ok || rule.SpecHash != core.SpecHash || rule.SpecID != core.SpecID || rule.Form != FormCore || !rule.Sends || rule.Bucket != CoreRebalanceBucket {
		t.Fatalf("catalogue core rule %+v", rule)
	}
	if len(rule.Data.Series) != 1 || rule.Data.Series[0].ISIN != core.ISIN || rule.Data.Series[0].Sessions != core.WindowCloses || len(rule.Data.Chains) != 0 {
		t.Fatalf("core declares %+v", rule.Data)
	}
}

// The recorder reads US option chains in USD on the US session only, on its
// v1 plan; a rule declaring any other chain cannot be collected, so the
// catalogue refuses it here (strategy-register.md, section 4). Every rule
// names its bucket and declares a positive window for each series.
func TestCatalogueDeclaresOnlyWhatCanaryCollects(t *testing.T) {
	for _, r := range Catalogue() {
		if r.ID == "" || r.SpecHash == "" && !r.Parameterised || r.Bucket == "" {
			t.Fatalf("rule %+v lacks an id, a spec hash or a bucket", r)
		}
		if r.Parameterised && (len(r.Data.Series) != 0 || r.Trend != "" || r.Instrument.ISIN != "") {
			t.Fatalf("parameterised rule %+v compiles what the entry supplies", r)
		}
		if !r.Parameterised {
			if _, ok := core.FindTrend(r.Trend); !ok {
				t.Fatalf("rule %s compiles filter %q, which the library lacks", r.ID, r.Trend)
			}
		}
		for _, s := range r.Data.Series {
			if s.Sessions <= 0 || s.Currency == "" || s.ISIN == "" && s.Symbol == "" {
				t.Fatalf("%s declares a series it cannot be read by: %+v", r.ID, s)
			}
		}
		for _, c := range r.Data.Chains {
			if c.Plan != "v1" || c.Underlying == "" {
				t.Fatalf("%s declares a chain the recorder cannot collect: %+v", r.ID, c)
			}
		}
	}
}

// timedEntry is a complete [mandate.strategies.trend-fund] entry on the
// parameterised rule, on a synthetic fund.
func timedEntry() risk.StrategyEntry {
	return risk.StrategyEntry{ID: "trend-fund", Form: risk.StrategyFormEntry, Rule: TimedHoldingRuleID, SpecHash: strings.Repeat("cd", 32), Sleeve: risk.OverlaySleeve,
		AllocationPct: new(4.0), AllocationKey: "mandate.sleeves.overlay_vol_pct", Stage: risk.StageShadow, ScalePct: new(25.0),
		KeyPrefix: "mandate.strategies.trend-fund", ISIN: "XS0000000001", Currency: "EUR", Signal: core.TrendSMA10.ID,
		BandPct: new(10.0), MinOrderBase: new(2500.0), MaxValuePct: new(40.0)}
}

// The parameterised rule (strategy-evaluator.md §1): a complete entry
// resolves to shadow with its instrument, filter, terms, cited spec and
// declared series from the entry's keys; the core resolves with its
// compiled ones and the entry's own numbers; the rule itself sends nothing
// in this build, so live written runs as shadow.
func TestResolveParameterisedEntry(t *testing.T) {
	got := Resolve([]risk.StrategyEntry{timedEntry()}, Catalogue())[0]
	if got.State != StateShadow || len(got.Reasons) != 0 || got.Rule == nil || !got.Rule.Parameterised {
		t.Fatalf("resolved %+v", got)
	}
	if got.Instrument != (Instrument{ISIN: "XS0000000001", Currency: "EUR"}) || got.Trend != core.TrendSMA10.ID || got.SpecHash != strings.Repeat("cd", 32) {
		t.Fatalf("instrument %+v trend %s spec %s", got.Instrument, got.Trend, got.SpecHash)
	}
	if got.Terms != (Terms{BandPct: 10, MinOrderBase: 2500, MaxValuePct: 40}) || len(got.Data.Series) != 1 || got.Data.Series[0] != (SeriesNeed{ISIN: "XS0000000001", Currency: "EUR", Sessions: core.WindowCloses}) {
		t.Fatalf("terms %+v data %+v", got.Terms, got.Data)
	}
	pinned := timedEntry()
	pinned.ConID, pinned.Exchange, pinned.KillDrawdownK = new(777), "LSEETF", new(1.5)
	if got := Resolve([]risk.StrategyEntry{pinned}, Catalogue())[0]; got.Instrument.ConID != 777 || got.Instrument.Exchange != "LSEETF" || got.KillDrawdownK == nil || *got.KillDrawdownK != 1.5 {
		t.Fatalf("pins %+v kill %v", got.Instrument, got.KillDrawdownK)
	}
	live := timedEntry()
	live.Stage = risk.StageLive
	if got := Resolve([]risk.StrategyEntry{live}, Catalogue())[0]; got.State != StateLive || !got.Reserves() || got.ReservedPct != 25 || len(got.Notes) != 0 || len(got.Reasons) != 0 {
		t.Fatalf("live written on the parameterised rule: %+v", got)
	}
	c := coreEntry(risk.StageShadow, core.SpecHash, 25)
	c.ISIN, c.Currency, c.BandPct, c.MinOrderBase, c.MaxValuePct = core.ISIN, core.Currency, new(10.0), new(2500.0), new(40.0)
	if got := Resolve([]risk.StrategyEntry{c}, Catalogue())[0]; got.Instrument != (Instrument{ISIN: core.ISIN, Currency: core.Currency, Symbol: core.Symbol}) || got.Trend != core.TrendSMA10.ID || got.SpecHash != core.SpecHash || got.Terms.BandPct != 10 || len(got.Data.Series) != 1 {
		t.Fatalf("the core resolves %+v", got)
	}
}

// Each evaluator key an entry on the parameterised rule leaves unwritten
// holds it and is named under its table; a filter the library lacks holds
// it; a cited spec hash is never checked against the rule, which carries
// none.
func TestResolveParameterisedHolds(t *testing.T) {
	for _, key := range []string{"isin", "currency", "signal", "band_pct", "min_order_base", "max_value_pct"} {
		e := timedEntry()
		switch key {
		case "isin":
			e.ISIN = ""
		case "currency":
			e.Currency = ""
		case "signal":
			e.Signal = ""
		case "band_pct":
			e.BandPct = nil
		case "min_order_base":
			e.MinOrderBase = nil
		case "max_value_pct":
			e.MaxValuePct = nil
		}
		got := Resolve([]risk.StrategyEntry{e}, Catalogue())[0]
		if got.State != StateHold || got.Instrument.ISIN != "" || !slices.ContainsFunc(got.Reasons, func(r string) bool { return strings.Contains(r, "mandate.strategies.trend-fund."+key+" unwritten") }) {
			t.Fatalf("%s unwritten: %+v", key, got)
		}
	}
	unknown := timedEntry()
	unknown.Signal = "trend-sma12m"
	if got := Resolve([]risk.StrategyEntry{unknown}, Catalogue())[0]; got.State != StateHold || !slices.ContainsFunc(got.Reasons, func(r string) bool { return strings.Contains(r, "signal trend-sma12m is not in this Canary's library") }) {
		t.Fatalf("unknown filter: %+v", got)
	}
	cited := timedEntry()
	cited.SpecHash = strings.Repeat("ef", 32)
	if got := Resolve([]risk.StrategyEntry{cited}, Catalogue())[0]; got.State != StateShadow || got.SpecHash != cited.SpecHash {
		t.Fatalf("a cited spec on a parameterised rule: %+v", got)
	}
}

// Two strategies on one fund line refuse the file (refuter finding F3,
// 2026-10-19): an entry naming the core's compiled fund while the core's
// keys are unwritten served a strategy_rebalance row beside the core's on
// the same contract, each counting the other's units as its own; two entries
// pinning one con_id are refused alike.
func TestTwoStrategiesOnOneFundLineRefuseTheFile(t *testing.T) {
	c := coreEntry(risk.StageLive, core.SpecHash, 25)
	onCore := timedEntry()
	onCore.ISIN, onCore.Currency, onCore.KeyPrefix = core.ISIN, core.Currency, "mandate.strategies.trend-fund"
	refused := Refusals([]risk.StrategyEntry{c, onCore}, Catalogue())
	if len(refused) != 1 || !strings.Contains(refused[0], "[mandate.strategies.trend-fund] names the fund "+core.ISIN+" in EUR, as [mandate.core] does") {
		t.Fatalf("an entry on the core's fund: %q", refused)
	}
	a, b := timedEntry(), timedEntry()
	a.ConID, b.ConID, b.ISIN, b.ID, b.KeyPrefix = new(777), new(777), "XS0000000002", "other", "mandate.strategies.other"
	if refused := Refusals([]risk.StrategyEntry{a, b}, Catalogue()); len(refused) != 1 || !strings.Contains(refused[0], "pins con_id 777") {
		t.Fatalf("two entries on one con_id: %q", refused)
	}
	if refused := Refusals([]risk.StrategyEntry{c, timedEntry()}, Catalogue()); len(refused) != 0 {
		t.Fatalf("distinct lines refused: %q", refused)
	}
}

// A live entry's written kill bound must be one its stage can reach: at 1 K
// or more (the whole sleeve invested at its stage) the kill rule fires only
// once the sleeve is gone, and the file is refused; shadow, which sends
// nothing, is not refused. An unwritten bound holds the entry in the daemon
// (strategy_target.go), never the file (review, 2026-10-19).
func TestLiveEntryWhoseKillRuleCannotFireRefusesTheFile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		e       risk.StrategyEntry
		bound   *float64
		refused bool
	}{
		{"entry live at 1 K", entry(risk.StageLive, 25), new(KillBoundMax), true},
		{"entry live at 1.5 K", entry(risk.StageLive, 100), new(1.5), true},
		{"entry live at 0.35 K", entry(risk.StageLive, 25), new(0.35), false},
		{"entry live unwritten", entry(risk.StageLive, 25), nil, false},
		{"entry shadow at 1 K", entry(risk.StageShadow, 25), new(KillBoundMax), false},
		{"entry suspended at 1 K", entry(risk.StageSuspended, 25), new(KillBoundMax), false},
		{"core live at 1 K", coreEntry(risk.StageLive, core.SpecHash, 25), new(KillBoundMax), true},
		{"core live unwritten", coreEntry(risk.StageLive, core.SpecHash, 25), nil, false},
	} {
		e := tc.e
		e.KillDrawdownK = tc.bound
		refused := Refusals([]risk.StrategyEntry{e}, testRules(true, true))
		if (len(refused) == 1) != tc.refused || tc.refused && !strings.Contains(refused[0], "["+Table(e)+"] is written live with kill_drawdown_k = ") || tc.refused && !strings.Contains(refused[0], "cannot fire before the sleeve is gone") {
			t.Fatalf("%s: refusals %q, want refused %v", tc.name, refused, tc.refused)
		}
	}
}
