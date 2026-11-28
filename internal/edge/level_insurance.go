package edge

import (
	"slices"

	"github.com/osauer/canary/v3/internal/rpc"
)

// levelInsuranceVersions are the versions whose cut is read as insurance.
var levelInsuranceVersions = []string{rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsVersionBuyBack, rpc.EdgeLevelsVersionIndex, rpc.EdgeLevelsVersionNearTrim}

// LevelReplayInsurance reads the rule's sales as insurance, by the terms of
// the trading-loop review's test: a cut is a fall that turned when the stock
// closed that session above the cut level and a fall that kept going when it
// closed at or under it; what the plan cost in the first and saved in the
// second is the version's result less held's on those days, at the close and
// five sessions on; and the worst result over the group's days, held and
// with the plan. Days is every drafted day of the group, cut or not. The
// days must carry their half (LevelReplaySummary sets it).
func LevelReplayInsurance(days []rpc.EdgeLevelsDay) []rpc.EdgeLevelsInsurance {
	var out []rpc.EdgeLevelsInsurance
	for _, reading := range []string{rpc.EdgeLevelsReadingPriorLow, rpc.EdgeLevelsReadingDownOnDay} {
		for _, size := range []string{rpc.EdgeLevelsSizeHalves, rpc.EdgeLevelsSizeThirds} {
			for _, half := range []string{rpc.EdgeLevelsHalfAll, rpc.EdgeLevelsHalfFirst, rpc.EdgeLevelsHalfSecond} {
				for _, version := range levelInsuranceVersions {
					for _, basis := range []string{rpc.EdgeLevelsBasisStock, rpc.EdgeLevelsBasisOption} {
						if row, ok := levelInsuranceRow(days, reading, size, half, version, basis); ok {
							out = append(out, row)
						}
					}
				}
			}
		}
	}
	return out
}

// levelOnBasis is a result's two valuations on one basis; ok is false when
// the basis has no close value, and h5 nil when it has none five sessions on.
func levelOnBasis(r rpc.EdgeLevelsDayResult, basis string) (closePct float64, h5 *float64, ok bool) {
	if basis == rpc.EdgeLevelsBasisStock {
		return r.ClosePct, r.H5Pct, true
	}
	if r.Option == nil {
		return 0, nil, false
	}
	return r.Option.ClosePct, r.Option.H5Pct, true
}

func levelInsuranceRow(days []rpc.EdgeLevelsDay, reading, size, half, version, basis string) (rpc.EdgeLevelsInsurance, bool) {
	row := rpc.EdgeLevelsInsurance{Reading: reading, Size: size, Half: half, Version: version, Basis: basis}
	var turned, turned5, kept, kept5, nearLow []float64
	var planRes0 rpc.EdgeLevelsOptionPct
	// Money-weighted: the turned and kept-going cuts and all cuts, at the
	// close and at five.
	var cells [6]levelMoneyCell
	add := func(diffPct float64, at ...int) {
		for _, i := range at {
			cells[i].add(diffPct, planRes0.MomentValue)
		}
	}
	for i := range days {
		d := &days[i]
		if half != rpc.EdgeLevelsHalfAll && d.Half != half {
			continue
		}
		if reading == rpc.EdgeLevelsReadingPriorLow && d.Row != rpc.EdgeLevelsRowLosing || reading == rpc.EdgeLevelsReadingDownOnDay && !d.DownOnDay {
			continue
		}
		heldRes, okHeld := levelResult(&d.Look, rpc.EdgeLevelsVersionHeld, "")
		planRes, okPlan := levelResult(&d.Look, version, size)
		if !okHeld || !okPlan {
			continue
		}
		held, held5, okHeld := levelOnBasis(heldRes, basis)
		plan, plan5, okPlan := levelOnBasis(planRes, basis)
		if !okHeld || !okPlan {
			continue
		}
		row.Days++
		row.WorstHeldClosePct, row.WorstPlanClosePct = min(row.WorstHeldClosePct, held), min(row.WorstPlanClosePct, plan)
		both5 := held5 != nil && plan5 != nil
		if both5 {
			row.WorstHeldH5Pct, row.WorstPlanH5Pct = min(row.WorstHeldH5Pct, *held5), min(row.WorstPlanH5Pct, *plan5)
		}
		cut := slices.IndexFunc(planRes.Events, func(e rpc.EdgeLevelsEvent) bool { return e.Kind == "cut" })
		if cut < 0 {
			continue
		}
		row.Cuts++
		planRes0 = rpc.EdgeLevelsOptionPct{}
		if basis == rpc.EdgeLevelsBasisOption {
			planRes0 = *planRes.Option
		}
		if d.Close > d.Look.Cut {
			turned = append(turned, plan-held)
			add(plan-held, 0, 4)
			if both5 {
				turned5 = append(turned5, *plan5-*held5)
				add(*plan5-*held5, 1, 5)
			}
		} else {
			kept = append(kept, plan-held)
			add(plan-held, 2, 4)
			if both5 {
				kept5 = append(kept5, *plan5-*held5)
				add(*plan5-*held5, 3, 5)
			}
		}
		if d.SessionLow > 0 {
			above := (planRes.Events[cut].Price - d.SessionLow) / d.SessionLow * 100
			nearLow = append(nearLow, above)
			if above <= 1 {
				row.CutsWithin1PctOfLow++
			}
		}
	}
	if row.Days == 0 {
		return row, false
	}
	row.Turned, row.TurnedCostClosePct, row.TurnedH5 = len(turned), levelAvg(turned), len(turned5)
	row.KeptGoing, row.KeptSavingClosePct, row.KeptH5 = len(kept), levelAvg(kept), len(kept5)
	row.TurnedCostCloseMedianPct, row.KeptSavingCloseMedianPct = levelMedian(turned), levelMedian(kept)
	if len(turned5) > 0 {
		row.TurnedCostH5Pct, row.TurnedCostH5MedianPct = new(levelAvg(turned5)), new(levelMedian(turned5))
	}
	if len(kept5) > 0 {
		row.KeptSavingH5Pct, row.KeptSavingH5MedianPct = new(levelAvg(kept5)), new(levelMedian(kept5))
	}
	row.CutAboveLowMedianPct = levelMedian(nearLow)
	if basis == rpc.EdgeLevelsBasisOption {
		row.Money = &rpc.EdgeLevelsInsuranceMoney{TurnedClose: cells[0].cell(), TurnedH5: cells[1].cell(), KeptClose: cells[2].cell(), KeptH5: cells[3].cell(), AllClose: cells[4].cell(), AllH5: cells[5].cell()}
	}
	return row, true
}

// levelMedian is the median, zero for none; it sorts v.
func levelMedian(v []float64) float64 {
	n := len(v)
	if n == 0 {
		return 0
	}
	slices.Sort(v)
	return (v[(n-1)/2] + v[n/2]) / 2
}

// levelMoneyCell sums a difference in percent of a day's value over days,
// in money.
type levelMoneyCell struct {
	days                   int
	money, weight, largest float64
}

func (c *levelMoneyCell) add(diffPct, value float64) {
	if value <= 0 {
		return
	}
	c.days++
	c.money += diffPct / 100 * value
	c.weight += value
	c.largest = max(c.largest, value)
}

func (c levelMoneyCell) cell() rpc.EdgeLevelsMoneyCell {
	out := rpc.EdgeLevelsMoneyCell{Days: c.days, Money: c.money, Weight: c.weight}
	if c.weight > 0 {
		out.Pct, out.LargestDayShare = c.money/c.weight*100, c.largest/c.weight
	}
	return out
}

// LevelReplayActual sets the owner's own trades beside holding and beside
// the rule in halves, weighted by money, over the days he traded the stock's
// calls whose result is valued and reconciles with the statements: every
// day of the sample, then the drafted days of each reading. The days must
// carry their half.
func LevelReplayActual(days []rpc.EdgeLevelsDay) []rpc.EdgeLevelsActualSum {
	var out []rpc.EdgeLevelsActualSum
	for _, reading := range []string{"", rpc.EdgeLevelsReadingPriorLow, rpc.EdgeLevelsReadingDownOnDay} {
		for _, half := range []string{rpc.EdgeLevelsHalfAll, rpc.EdgeLevelsHalfFirst, rpc.EdgeLevelsHalfSecond} {
			sum := rpc.EdgeLevelsActualSum{Reading: reading, Half: half}
			var cells [4]levelMoneyCell
			for i := range days {
				d := &days[i]
				if half != rpc.EdgeLevelsHalfAll && d.Half != half || d.Actual == nil {
					continue
				}
				if reading == rpc.EdgeLevelsReadingPriorLow && d.Row != rpc.EdgeLevelsRowLosing || reading == rpc.EdgeLevelsReadingDownOnDay && !d.DownOnDay {
					continue
				}
				sum.Traded++
				held, _ := levelResult(&d.Look, rpc.EdgeLevelsVersionHeld, "")
				if d.Actual.Result == nil || !d.Actual.Reconciled || held.Option == nil {
					continue
				}
				sum.Days++
				a, value := d.Actual.Result, d.Actual.Result.MomentValue
				cells[0].add(a.ClosePct-held.Option.ClosePct, value)
				if a.ClosePct > held.Option.ClosePct+1e-9 {
					sum.BeatsHeldClose++
				}
				if a.H5Pct != nil && held.Option.H5Pct != nil {
					cells[1].add(*a.H5Pct-*held.Option.H5Pct, value)
				}
				if rule, ok := levelResult(&d.Look, rpc.EdgeLevelsVersionRule, rpc.EdgeLevelsSizeHalves); ok && reading != "" && rule.Option != nil {
					cells[2].add(a.ClosePct-rule.Option.ClosePct, value)
					if a.H5Pct != nil && rule.Option.H5Pct != nil {
						cells[3].add(*a.H5Pct-*rule.Option.H5Pct, value)
					}
				}
			}
			sum.VsHeldClose, sum.VsHeldH5, sum.VsRuleClose, sum.VsRuleH5 = cells[0].cell(), cells[1].cell(), cells[2].cell(), cells[3].cell()
			out = append(out, sum)
		}
	}
	return out
}
