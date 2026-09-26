package risk

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// The three concentration watches beside rule 1 (amendment 15). Each reads
// the same issuer netting as rule 1, each can only watch, and none enters an
// act count or drives a trim: they are rows of the Rulebook, numbered 16-18,
// so every surface, the alert authority and the history treat them like any
// other rule.

// deltaSwing is rule 16: one issuer's dollar delta as a share of NLV, the
// measure rule 1 used before amendment 15. It says how far a 10% move in the
// issuer moves the book, and mentions gamma when it bends that materially.
// Missing delta may indict (a provable lower bound), never acquit. The short
// delta of a protection-classified index put is exempt: rule 12 sizes it.
func (c *ruleContext) deltaSwing() RuleRow {
	row := RuleRow{ID: RuleDeltaSwing, Number: 16, Title: "Delta swing on one issuer", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch := c.pol.DeltaSwingWatchPct
	row.Threshold = new(watch)
	var offenders, unknowns, hedges []RuleOffender
	worst, worstName, worstBound := 0.0, "", false
	gapsFromGreeks := false
	for _, e := range c.issuers() {
		issuer := e.exposure.Issuer
		exact, bounded := true, true
		low, high, gammaPnL := 0.0, 0.0, 0.0
		exempt := 0.0
		for _, line := range e.lines {
			n := line.name
			switch {
			case c.greeksGapMaterial(n):
				exact = false
				gapsFromGreeks = true
				lo, hi, ok := nameExposureInterval(n)
				if !ok {
					bounded = false
					continue
				}
				low, high = low+lo, high+hi
			case !n.ExposureBaseComplete:
				exact, bounded = false, false
				continue
			default:
				low, high = low+n.ExposureBase, high+n.ExposureBase
				if c.pol.IsHedgeSymbol(n.Symbol) && n.ExposureBase < 0 {
					sized := 0.0
					for _, l := range n.Legs {
						if rule12HedgeLeg(l) {
							sized += math.Abs(*l.Delta*l.Quantity*l.Multiplier**l.Underlying) * fxOrOne(l.FXToBase)
						}
					}
					exempt += math.Min(sized, math.Abs(n.ExposureBase))
				}
			}
			for _, l := range n.Legs {
				if l.Gamma != nil && l.Underlying != nil && l.FXToBase != nil {
					move := 0.1 * *l.Underlying
					gammaPnL += 0.5 * *l.Gamma * move * move * l.Quantity * l.Multiplier * *l.FXToBase
				}
			}
		}
		if !bounded {
			unknowns = append(unknowns, RuleOffender{Symbol: issuer, Note: "dollar delta not measurable (delta, price or FX missing)"})
			continue
		}
		net, lowerBound := low, false
		switch {
		case exact:
		case low > 0:
			lowerBound = true
		case high < 0:
			net, lowerBound = high, true
		default:
			unknowns = append(unknowns, RuleOffender{Symbol: issuer, Note: "delta missing on material legs; the bounds straddle zero"})
			continue
		}
		if exempt > 0 && net < 0 {
			exempt = math.Min(exempt, math.Abs(net))
			hedges = append(hedges, RuleOffender{Symbol: issuer, Observed: round1(pct(exempt, c.nlv)),
				Note: "protection-classified short delta — sized by rule 12, not a delta swing"})
			net += exempt
		}
		p := pct(math.Abs(net), c.nlv)
		if !lowerBound && p > worst || worstName == "" {
			worst, worstName, worstBound = p, issuer, lowerBound
		}
		if p < watch {
			if lowerBound {
				unknowns = append(unknowns, RuleOffender{Symbol: issuer, Note: "delta missing on material legs; at least " + fmt.Sprintf("%.1f%%", round1(p)) + " of NLV"})
			}
			continue
		}
		direction := "fall"
		if net < 0 {
			direction = "rise"
		}
		note := fmt.Sprintf("a 10%% %s costs about %.1f%% of NLV", direction, round1(p/10))
		if lowerBound {
			note = "lower bound — delta missing on some legs; " + note + " or more"
		}
		if g := pct(gammaPnL, c.nlv); math.Abs(g) >= c.pol.GreeksGapFloorPctNLV {
			if g < 0 {
				note += fmt.Sprintf("; short gamma adds about %.1f%% of NLV on a 10%% move either way", round1(-g))
			} else {
				note += fmt.Sprintf("; long gamma offsets about %.1f%% of NLV on a 10%% move either way", round1(g))
			}
		}
		offenders = append(offenders, RuleOffender{Symbol: issuer, Observed: round1(p), ImpactBase: math.Abs(net), Note: note})
		if p > worst || worstBound {
			worst, worstName, worstBound = p, issuer, lowerBound
		}
	}
	sortOffenders(offenders)
	row.Offenders = offenders
	row.Exempt = hedges
	switch {
	case len(offenders) > 0:
		top := offenders[0]
		row.Status = RuleStatusWatch
		row.Observed = new(top.Observed)
		row.ObservedIsLowerBound = strings.HasPrefix(top.Note, "lower bound")
		bound := ""
		if row.ObservedIsLowerBound {
			bound = " at least"
		}
		row.Evidence = fmt.Sprintf("%s carries%s %.1f%% of NLV in dollar delta, at or above the %s%% watch level: %s.", top.Symbol, bound, top.Observed, limitText(watch), strings.TrimPrefix(top.Note, "lower bound — delta missing on some legs; "))
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "greeks_gap"
		if !gapsFromGreeks {
			row.Reason = "exposure_incomplete"
		}
		row.Evidence = fmt.Sprintf("Canary could not measure the dollar delta of %d issuer(s): option delta, a price or an FX rate is missing.", len(unknowns))
		if gapsFromGreeks {
			c.offSessionGreeksNote(&row)
		}
	default:
		row.Status = RuleStatusPass
		row.Observed = new(round1(worst))
		name := worstName
		if name == "" {
			name = "no issuer"
		}
		row.Evidence = fmt.Sprintf("The largest dollar delta on one issuer is %.1f%% of NLV (%s), under the %s%% watch level.", round1(worst), name, limitText(watch))
	}
	row.Offenders = append(row.Offenders, unknowns...)
	if row.Status == RuleStatusWatch && len(unknowns) > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("%d issuer(s) additionally not measurable — the watch above stands regardless.", len(unknowns)))
	}
	for _, o := range row.Offenders {
		row.ImpactBase += o.ImpactBase
	}
	return row
}

func fxOrOne(fx *float64) float64 {
	if fx == nil {
		return 1
	}
	return *fx
}

// clusterStress is rule 17: every issuer in a declared cluster falls
// cluster_drop_pct together, each netted with rule 1's leg rules, and the
// cluster's combined loss is compared with cluster_watch_pct of NLV. Gains
// on one member offset losses on another, as they would in the fall.
func (c *ruleContext) clusterStress() RuleRow {
	row := RuleRow{ID: RuleClusterStress, Number: 17, Title: "Cluster falling together", Unit: "% NLV"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch, drop := c.pol.ClusterWatchPct, c.pol.ClusterDropPct
	if len(c.pol.Clusters) == 0 {
		row.Status = RuleStatusNotEvaluated
		row.Reason = RuleReasonNoClusters
		row.Evidence = "No cluster is declared in the Rulebook policy; list related issuers under clusters to test them falling together."
		return row
	}
	row.Threshold = new(watch)
	byIssuer := map[string]issuerEval{}
	for _, e := range c.issuers() {
		byIssuer[e.exposure.Issuer] = e
	}
	names := make([]string, 0, len(c.pol.Clusters))
	for name := range c.pol.Clusters {
		names = append(names, name)
	}
	sort.Strings(names)
	m := 1 - drop/100
	var offenders, unknowns []RuleOffender
	worst, worstName, held := 0.0, "", 0
	for _, cluster := range names {
		seen := map[string]bool{}
		var members []issuerEval
		for _, member := range c.pol.Clusters[cluster] {
			issuer := c.clusterMemberIssuer(member)
			if e, ok := byIssuer[issuer]; ok && !seen[issuer] {
				seen[issuer] = true
				members = append(members, e)
			}
		}
		if len(members) == 0 {
			continue
		}
		held++
		loss := 0.0
		var parts, missing []string
		for _, e := range members {
			if !e.scenarioReady() || !c.hasNLV {
				missing = append(missing, e.exposure.Issuer)
				continue
			}
			l := -e.pnlAt(m)
			loss += l
			parts = append(parts, fmt.Sprintf("%s %+.1f%%", e.exposure.Issuer, round1(-pct(l, c.nlv))))
		}
		if len(missing) > 0 {
			unknowns = append(unknowns, RuleOffender{Symbol: cluster, Note: "not measured: " + strings.Join(missing, ", ") + " lack a price, share count or FX rate"})
			continue
		}
		p := pct(math.Max(loss, 0), c.nlv)
		if p > worst || worstName == "" {
			worst, worstName = p, cluster
		}
		if p >= watch {
			offenders = append(offenders, RuleOffender{Symbol: cluster, Observed: round1(p), ImpactBase: loss,
				Note: fmt.Sprintf("a %s%% fall together: %s of NLV", limitText(drop), strings.Join(parts, ", "))})
		}
	}
	sortOffenders(offenders)
	switch {
	case len(offenders) > 0:
		top := offenders[0]
		row.Status = RuleStatusWatch
		row.Observed = new(top.Observed)
		row.Evidence = fmt.Sprintf("Cluster %s loses %.1f%% of NLV if its issuers fall %s%% together, at or above the %s%% watch level.", top.Symbol, top.Observed, limitText(drop), limitText(watch))
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "exposure_incomplete"
		row.Evidence = fmt.Sprintf("Canary could not measure %d declared cluster(s) because a member's price, share count or FX rate is missing.", len(unknowns))
	case held == 0:
		row.Status = RuleStatusPass
		row.Observed = new(0.0)
		row.Evidence = fmt.Sprintf("No declared cluster is held; the watch level is %s%% of NLV.", limitText(watch))
	default:
		row.Status = RuleStatusPass
		row.Observed = new(round1(worst))
		row.Evidence = fmt.Sprintf("The worst declared cluster (%s) loses %.1f%% of NLV in a %s%% fall together, under the %s%% watch level.", worstName, round1(worst), limitText(drop), limitText(watch))
	}
	row.Offenders = append(offenders, unknowns...)
	if row.Status == RuleStatusWatch && len(unknowns) > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("%d cluster(s) additionally not measured — the watch above stands regardless.", len(unknowns)))
	}
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	return row
}

// clusterMemberIssuer resolves a cluster member: an issuer group's name (in
// any case) names that issuer, and a symbol names the issuer it belongs to.
func (c *ruleContext) clusterMemberIssuer(member string) string {
	for name := range c.pol.IssuerGroups {
		if strings.EqualFold(name, strings.TrimSpace(member)) {
			return name
		}
	}
	return c.pol.IssuerOf(member)
}

// lossBudget is rule 18: one issuer's worst-case loss against the
// constitution's effective risk capital, min(declared risk capital, equity −
// protected floor). Without that number the row is unknown and names what is
// missing; it is never a pass.
func (c *ruleContext) lossBudget() RuleRow {
	row := RuleRow{ID: RuleLossBudget, Number: 18, Title: "Issuer loss against risk capital", Unit: "% risk capital"}
	if g := c.portfolioGate(row.ID, row.Number, row.Title); g != nil {
		return *g
	}
	watch := c.pol.BudgetWatchPct
	rc := c.in.RiskCapital
	if rc == nil || rc.EffectiveBase == nil || math.IsNaN(*rc.EffectiveBase) || math.IsInf(*rc.EffectiveBase, 0) {
		missing := "your effective risk capital (risk-policy.toml)"
		if rc != nil && strings.TrimSpace(rc.Missing) != "" {
			missing = strings.TrimSpace(rc.Missing)
		}
		row.Status = RuleStatusUnknown
		row.Reason = RuleReasonRiskCapitalUnavailable
		row.Evidence = "Canary needs your number: " + missing + "."
		return row
	}
	row.Threshold = new(watch)
	eff := *rc.EffectiveBase
	var offenders, unknowns []RuleOffender
	worst, worstName := 0.0, ""
	for _, e := range c.issuers() {
		x := e.exposure
		if e.status == RuleStatusUnknown && !(x.LowerBound && x.WorstCaseLossBase > 0) {
			unknowns = append(unknowns, RuleOffender{Symbol: x.Issuer, Note: "worst-case loss not measured: " + strings.Join(e.gaps, "; ")})
			continue
		}
		if eff <= 0 {
			if x.WorstCaseLossBase > 0 {
				offenders = append(offenders, issuerOffender(e, 0, "effective risk capital is used up; any loss exceeds it"))
			}
			continue
		}
		ratio := x.WorstCaseLossBase / eff * 100
		if !x.LowerBound && (ratio > worst || worstName == "") {
			worst, worstName = ratio, x.Issuer
		}
		if ratio < watch {
			if x.LowerBound {
				unknowns = append(unknowns, RuleOffender{Symbol: x.Issuer, Note: "an unbounded leg cannot be sized without the price"})
			}
			continue
		}
		note := fmt.Sprintf("worst-case loss %.1f%% of NLV", x.WorstCaseLossPct)
		if x.LowerBound {
			note = "lower bound — " + note
		}
		offenders = append(offenders, issuerOffender(e, ratio, note))
	}
	sortOffenders(offenders)
	switch {
	case eff <= 0 && len(offenders) > 0:
		row.Status = RuleStatusWatch
		row.Evidence = fmt.Sprintf("Effective risk capital is used up (equity is at or below the protected floor), and %d issuer(s) still carry a worst-case loss; the watch level is %s%% of it.", len(offenders), limitText(watch))
	case len(offenders) > 0:
		top := offenders[0]
		row.Status = RuleStatusWatch
		row.Observed = new(top.Observed)
		row.ObservedIsLowerBound = strings.HasPrefix(top.Note, "lower bound")
		bound := ""
		if row.ObservedIsLowerBound {
			bound = " at least"
		}
		row.Evidence = fmt.Sprintf("%s can lose%s %.1f%% of your effective risk capital at worst, at or above the %s%% watch level.", top.Symbol, bound, top.Observed, limitText(watch))
	case len(unknowns) > 0:
		row.Status = RuleStatusUnknown
		row.Reason = "exposure_incomplete"
		row.Evidence = fmt.Sprintf("Canary could not measure the worst-case loss on %d issuer(s) because a price, share count or FX rate is missing.", len(unknowns))
	default:
		row.Status = RuleStatusPass
		row.Observed = new(round1(worst))
		name := worstName
		if name == "" {
			name = "no issuer"
		}
		row.Evidence = fmt.Sprintf("The largest worst-case loss on one issuer is %.1f%% of your effective risk capital (%s), under the %s%% watch level.", round1(worst), name, limitText(watch))
	}
	row.Offenders = append(offenders, unknowns...)
	if row.Status == RuleStatusWatch && len(unknowns) > 0 {
		row.Notes = append(row.Notes, fmt.Sprintf("%d issuer(s) additionally not measured — the watch above stands regardless.", len(unknowns)))
	}
	for _, o := range offenders {
		row.ImpactBase += o.ImpactBase
	}
	return row
}
