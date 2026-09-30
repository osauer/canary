package risk

import (
	"fmt"
	"math"
	"strings"
)

// marginHeadroom is rule 19 (amendment 18, owner decision 2026-09-30): the
// broker's excess liquidity as a share of NLV, the room left before margin
// calls start closing positions. Headroom falls as margin use grows, so the
// rule reads downward: watch strictly below margin_headroom_watch_pct, act
// strictly below margin_headroom_act_pct, and a reading exactly at a level
// passes that level. It needs no positions, because the broker's figure
// already nets the whole book. When the broker also reports its look-ahead
// excess liquidity, the rule judges the worse (lower) of the two, and the
// evidence names both and which one governed (amendment 19 R3); without it,
// the current figure alone. A missing current excess liquidity or NLV, or an
// unhealthy account source, reads unknown, never pass. It carries no impact,
// so it ranks by severity, then rule number; it never trims and drives no
// proposal bucket.
func (c *ruleContext) marginHeadroom() RuleRow {
	row := RuleRow{ID: RuleMarginHeadroom, Number: 19, Title: "Margin headroom", Unit: "% NLV"}
	if !c.in.Account.Healthy || !c.hasNLV {
		row.Status = RuleStatusUnknown
		row.Reason = nonEmpty(c.in.Account.Reason, "account_unavailable")
		row.Evidence = "Canary does not have a current account value."
		return row
	}
	excess := c.in.ExcessLiquidityBase
	if excess == nil || math.IsNaN(*excess) || math.IsInf(*excess, 0) {
		row.Status = RuleStatusUnknown
		row.Reason = RuleReasonExcessLiquidityUnavailable
		row.Evidence = "The broker's account summary carried no excess liquidity, so Canary cannot measure margin headroom."
		return row
	}
	watch, act := c.pol.MarginHeadroomWatchPct, c.pol.MarginHeadroomActPct
	p, basis := c.marginHeadroomMeasure(*excess)
	row.Observed = new(round1(p))
	row.setBands(watch, act)
	context := c.marginContext()
	switch {
	case p < act:
		row.Status = RuleStatusAct
		row.Evidence = fmt.Sprintf("Excess liquidity is %.1f%% of NLV, below the %s%% act level (watch below %s%%)%s.%s", round1(p), limitText(act), limitText(watch), basis, context)
	case p < watch:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("Excess liquidity is %.1f%% of NLV, below the %s%% watch level (act below %s%%)%s.%s", round1(p), limitText(watch), limitText(act), basis, context)
	default:
		row.Status = RuleStatusPass
		row.Evidence = fmt.Sprintf("Excess liquidity is %.1f%% of NLV, at or above the %s%% watch level%s.%s", round1(p), limitText(watch), basis, context)
	}
	return row
}

// marginHeadroomMeasure is rule 19's observed headroom as % of NLV: the
// current excess liquidity, or the broker's look-ahead excess liquidity when
// it reported one that is lower (amendment 19 R3). basis names both figures
// and which one governed ("; look-ahead 18.0% governs; current 22.0%"), and
// is empty when there is no look-ahead figure; a tie goes to the current one.
func (c *ruleContext) marginHeadroomMeasure(excess float64) (p float64, basis string) {
	current := pct(excess, c.nlv)
	ahead := c.in.LookAheadExcessLiquidityBase
	if ahead == nil || math.IsNaN(*ahead) || math.IsInf(*ahead, 0) {
		return current, ""
	}
	lookAhead := pct(*ahead, c.nlv)
	if lookAhead < current {
		return lookAhead, fmt.Sprintf("; look-ahead %.1f%% governs; current %.1f%%", round1(lookAhead), round1(current))
	}
	return current, fmt.Sprintf("; current %.1f%% governs; look-ahead %.1f%%", round1(current), round1(lookAhead))
}

// marginContext names the broker's maintenance and initial margin as shares
// of NLV, each only when the account summary reported it: " Maintenance
// margin is 42.0% of NLV, initial margin 55.0%."
func (c *ruleContext) marginContext() string {
	var parts []string
	for _, m := range []struct {
		name  string
		value *float64
	}{{"maintenance margin", c.in.MaintenanceMarginBase}, {"initial margin", c.in.InitialMarginBase}} {
		if m.value == nil || math.IsNaN(*m.value) || math.IsInf(*m.value, 0) {
			continue
		}
		v := round1(pct(*m.value, c.nlv))
		if len(parts) == 0 {
			parts = append(parts, fmt.Sprintf("%s is %.1f%% of NLV", m.name, v))
		} else {
			parts = append(parts, fmt.Sprintf("%s %.1f%%", m.name, v))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	text := strings.Join(parts, ", ")
	return " " + strings.ToUpper(text[:1]) + text[1:] + "."
}
