package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

// The stress read's margin-safety episode rests on Rulebook rule 19
// (amendment 19): rule 19's act opens it at act, and only a rule 19
// measurement makes the cushion observed. The broker's look-ahead figure
// alone never does, so a source-level negative cannot recover the episode
// while rule 19 is unmeasured.
func TestAlertShadowMarginEpisodeRestsOnRule19(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	scope := alertShadowBrokerScope{account: "redacted", mode: "paper"}
	compute := func(row *risk.RuleRow) rpc.StressResult {
		rules := &rpc.RulesResult{Enabled: true}
		if row != nil {
			rules.Rules = []risk.RuleRow{*row}
		}
		// A broker cushion of 7% and a look-ahead of 5%: below every retired
		// stress level, and no verdict of their own since amendment 19.
		acct := rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100_000, ExcessLiquidity: 7_000, Cushion: 0.07, LookAheadExcess: 5_000, AsOf: now}
		return stress.ComputeStress(rpc.StressInput{Now: now, Account: acct, MarginHeadroom: rpc.StressMarginHeadroomFromRules(rules)})
	}
	band := func(status string, observed float64) *risk.RuleRow {
		return &risk.RuleRow{ID: risk.RuleMarginHeadroom, Number: 19, Status: status, Observed: new(observed), WatchThreshold: new(30.0), ActThreshold: new(15.0)}
	}

	for name, row := range map[string]*risk.RuleRow{
		"no rule 19 row": nil,
		"unknown":        {ID: risk.RuleMarginHeadroom, Number: 19, Status: risk.RuleStatusUnknown, Reason: risk.RuleReasonExcessLiquidityUnavailable},
		"off":            {ID: risk.RuleMarginHeadroom, Number: 19, Status: risk.RuleStatusNotEvaluated, Reason: risk.RuleReasonRuleOff},
	} {
		if _, active, observed := alertShadowMarginObservation(scope, compute(row), alertShadowSourceBatch{}, now); active || observed {
			t.Errorf("%s: active %v observed %v, want neither without a rule 19 measurement", name, active, observed)
		}
	}
	if _, active, observed := alertShadowMarginObservation(scope, compute(band(risk.RuleStatusPass, 42)), alertShadowSourceBatch{}, now); active || !observed {
		t.Fatalf("pass: active %v observed %v, want an observed clear cushion", active, observed)
	}
	obs, active, observed := alertShadowMarginObservation(scope, compute(band(risk.RuleStatusAct, 12)), alertShadowSourceBatch{}, now)
	if !active || !observed || obs.Kind != rpc.AlertKindMarginSafety || obs.Severity != rpc.AlertSeverityAct {
		t.Fatalf("act: observation %+v active %v observed %v", obs, active, observed)
	}
}
