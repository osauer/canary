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
	// Money-weighted sums: the difference to held and the value at the
	// moment, for the turned and kept-going cuts at the close and at five.
	var money, weight [4]float64
	add := func(i int, diffPct float64) {
		money[i] += diffPct / 100 * planRes0.MomentValue
		weight[i] += planRes0.MomentValue
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
			add(0, plan-held)
			if both5 {
				turned5 = append(turned5, *plan5-*held5)
				add(1, *plan5-*held5)
			}
		} else {
			kept = append(kept, plan-held)
			add(2, plan-held)
			if both5 {
				kept5 = append(kept5, *plan5-*held5)
				add(3, *plan5-*held5)
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
		pct := func(i int) float64 {
			if weight[i] <= 0 {
				return 0
			}
			return money[i] / weight[i] * 100
		}
		row.Money = &rpc.EdgeLevelsInsuranceMoney{TurnedCostClosePct: pct(0), TurnedCostClose: money[0], TurnedCostH5Pct: pct(1), TurnedCostH5: money[1],
			KeptSavingClosePct: pct(2), KeptSavingClose: money[2], KeptSavingH5Pct: pct(3), KeptSavingH5: money[3]}
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
