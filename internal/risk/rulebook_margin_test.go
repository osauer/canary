package risk

import (
	"math"
	"slices"
	"strings"
	"testing"
)

// marginInputs is the quiet limits book (NLV 100,000) with the broker's
// excess liquidity set, so every percentage reads off the fixture.
func marginInputs(excess *float64) RuleInputs {
	in := limitsInputs()
	in.ExcessLiquidityBase = excess
	return in
}

// Rule 19 (amendment 18) reads downward: watch strictly below 30, act
// strictly below 15, and a reading exactly at a level passes that level.
func TestMarginHeadroomBandsReadDownward(t *testing.T) {
	for _, c := range []struct {
		name      string
		excess    float64
		status    string
		observed  float64
		threshold float64
	}{
		{"plenty", 100000, RuleStatusPass, 100, 30},
		{"exactly at the watch level passes", 30000, RuleStatusPass, 30, 30},
		{"just under the watch level", 29900, RuleStatusWatch, 29.9, 30},
		{"exactly at the act level only watches", 15000, RuleStatusWatch, 15, 30},
		{"just under the act level", 14900, RuleStatusAct, 14.9, 15},
		{"a margin deficit", -5000, RuleStatusAct, -5, 15},
	} {
		row := rowByID(t, EvaluateRulebook(marginInputs(new(c.excess)), DefaultRulebookPolicy()), RuleMarginHeadroom)
		if row.Status != c.status || row.Observed == nil || *row.Observed != c.observed || row.Threshold == nil || *row.Threshold != c.threshold ||
			row.WatchThreshold == nil || *row.WatchThreshold != 30 || row.ActThreshold == nil || *row.ActThreshold != 15 {
			t.Fatalf("%s: row = %+v (observed %v threshold %v)", c.name, row, row.Observed, row.Threshold)
		}
		if row.Number != 19 || row.Title != "Margin headroom" || row.Mode != RuleModeAlert || row.Unit != "% NLV" ||
			row.ImpactBase != 0 || len(row.Offenders) != 0 || row.SellOnly || !strings.Contains(row.Evidence, "Excess liquidity is") {
			t.Fatalf("%s: row = %+v", c.name, row)
		}
	}
	// The owner's levels move the bands and the evidence with them.
	pol := DefaultRulebookPolicy()
	pol.MarginHeadroomWatchPct, pol.MarginHeadroomActPct = 40, 20
	row := rowByID(t, EvaluateRulebook(marginInputs(new(35000.0)), pol), RuleMarginHeadroom)
	if row.Status != RuleStatusWatch || !strings.Contains(row.Evidence, "below the 40% watch level (act below 20%)") {
		t.Fatalf("owner levels: %+v", row)
	}
}

// Without the broker's excess liquidity, without NLV, or with an unhealthy
// account source the row is unknown, never a pass. The broker's figure nets
// the whole book, so the rule does not wait for positions.
func TestMarginHeadroomUnknownNeverPasses(t *testing.T) {
	for _, c := range []struct {
		name   string
		mutate func(*RuleInputs)
		reason string
	}{
		{"no excess liquidity", func(in *RuleInputs) { in.ExcessLiquidityBase = nil }, RuleReasonExcessLiquidityUnavailable},
		{"excess liquidity not a number", func(in *RuleInputs) { in.ExcessLiquidityBase = new(math.NaN()) }, RuleReasonExcessLiquidityUnavailable},
		{"no NLV", func(in *RuleInputs) { in.NLVBase = nil }, "account_unavailable"},
		{"zero NLV", func(in *RuleInputs) { in.NLVBase = new(0.0) }, "account_unavailable"},
		{"unhealthy account", func(in *RuleInputs) { in.Account = SourceState{Reason: "account_incomplete"} }, "account_incomplete"},
	} {
		in := marginInputs(new(10000.0)) // an act reading, were it measured
		c.mutate(&in)
		row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleMarginHeadroom)
		if row.Status != RuleStatusUnknown || row.Reason != c.reason || row.Observed != nil || row.Threshold != nil || row.Evidence == "" {
			t.Fatalf("%s: row = %+v", c.name, row)
		}
	}
	in := marginInputs(new(40000.0))
	in.Positions = SourceState{Reason: "positions_pending"}
	if row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleMarginHeadroom); row.Status != RuleStatusPass {
		t.Fatalf("pending positions held back an account-level measure: %+v", row)
	}
}

// The evidence names excess liquidity, and the maintenance and initial
// margin as context when the broker reported them.
func TestMarginHeadroomEvidenceNamesTheMargins(t *testing.T) {
	in := marginInputs(new(22000.0))
	in.MaintenanceMarginBase, in.InitialMarginBase = new(40000.0), new(55000.0)
	row := rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleMarginHeadroom)
	if want := "Excess liquidity is 22.0% of NLV, below the 30% watch level (act below 15%). Maintenance margin is 40.0% of NLV, initial margin 55.0%."; row.Evidence != want {
		t.Fatalf("evidence = %q, want %q", row.Evidence, want)
	}
	in.MaintenanceMarginBase = nil
	row = rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleMarginHeadroom)
	if !strings.HasSuffix(row.Evidence, " Initial margin is 55.0% of NLV.") {
		t.Fatalf("evidence = %q", row.Evidence)
	}
	in.InitialMarginBase = nil
	in.ExcessLiquidityBase = new(9000.0)
	row = rowByID(t, EvaluateRulebook(in, DefaultRulebookPolicy()), RuleMarginHeadroom)
	if row.Evidence != "Excess liquidity is 9.0% of NLV, below the 15% act level (watch below 30%)." {
		t.Fatalf("evidence without margins = %q", row.Evidence)
	}
}

// Rule 19 carries no impact, so among rows of one mode it ranks by severity,
// then rule number, as the cash reserve rule did. It drives no sell-only and
// is not a watch-only rule: it can act.
func TestMarginHeadroomRanksBySeverityThenNumber(t *testing.T) {
	rows := []RuleRow{
		{ID: RuleMarginHeadroom, Number: 19, Mode: RuleModeAlert, Status: RuleStatusAct},
		{ID: RuleExpiryRunway, Number: 5, Mode: RuleModeAlert, Status: RuleStatusWatch, ImpactBase: 9000},
		{ID: RuleOverwriteEarnings, Number: 7, Mode: RuleModeAlert, Status: RuleStatusAct},
		{ID: RuleSingleNameExposure, Number: 1, Mode: RuleModeAlert, Status: RuleStatusAct, ImpactBase: 5000},
	}
	var order []int
	for _, i := range rankRows(rows) {
		order = append(order, rows[i].Number)
	}
	if !slices.Equal(order, []int{1, 7, 19, 5}) {
		t.Fatalf("ranking = %v, want 1 (act with impact), 7, 19 (acts by number), then 5 (watch)", order)
	}
	ev := EvaluateRulebook(marginInputs(new(5000.0)), DefaultRulebookPolicy())
	if ev.SellOnly.Active || WatchOnlyRule(RuleMarginHeadroom) || rowByID(t, ev, RuleMarginHeadroom).Status != RuleStatusAct {
		t.Fatalf("sell-only %+v, watch-only %v", ev.SellOnly, WatchOnlyRule(RuleMarginHeadroom))
	}
}

// 0 ≤ act ≤ watch ≤ 100; equal levels are allowed.
func TestMarginHeadroomLevelsValidate(t *testing.T) {
	p := DefaultRulebookPolicy()
	p.MarginHeadroomWatchPct, p.MarginHeadroomActPct = 20, 20
	if err := p.Validate(); err != nil {
		t.Fatalf("equal levels refused: %v", err)
	}
	p.MarginHeadroomWatchPct, p.MarginHeadroomActPct = 100, 0
	if err := p.Validate(); err != nil {
		t.Fatalf("the full range refused: %v", err)
	}
	p.MarginHeadroomWatchPct, p.MarginHeadroomActPct = 20, 25
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "margin_headroom_act_pct (25) must not exceed margin_headroom_watch_pct (20)") {
		t.Fatalf("act above watch: %v", err)
	}
}

// rulebook_margin_headroom is a Rulebook presentation code and no other
// source's.
func TestMarginHeadroomAlertCodeIsARulebookCode(t *testing.T) {
	if !validAlertPresentationCode(AlertSourceRulebook, AlertPresentationRulebookMarginHeadroom) ||
		validAlertPresentationCode(AlertSourceStress, AlertPresentationRulebookMarginHeadroom) {
		t.Fatal("rulebook_margin_headroom is not exactly a Rulebook presentation code")
	}
}
