package spx

import (
	"fmt"
	"math"
	"time"
)

// Compute reduces a set of constituent windows to a single snapshot
// excluded with reason "no_window". A cached window contributes only when its
// from being relabelled and published as today's snapshot after a failed warm
func Compute(members []string, windows map[string]ConstituentWindow, sessionKey string, asOf time.Time) Snapshot {
	snap := Snapshot{
		AsOf:        asOf,
		SessionKey:  sessionKey,
		Method:      methodConstituentFanout,
		MemberCount: len(members),
	}

	above50 := 0
	above200 := 0
	coverage50 := 0
	coverage200 := 0
	coverageHL := 0
	newHighs := 0
	newLows := 0

	// Iterating in member order makes the exclusion list stable for
	// tests and easy to diff in verify.log entries.
	for _, sym := range members {
		w, ok := windows[sym]
		if !ok {
			snap.Excluded = append(snap.Excluded, ExcludedMember{Symbol: sym, Reason: "no_window"})
			continue
		}
		if w.LastBarAt != sessionKey {
			snap.Excluded = append(snap.Excluded, ExcludedMember{
				Symbol: sym,
				Reason: "session_mismatch",
			})
			continue
		}
		if len(w.Closes) < WindowSize {
			snap.Excluded = append(snap.Excluded, ExcludedMember{
				Symbol: sym,
				Reason: fmt.Sprintf("thin_history(%d)", len(w.Closes)),
			})
			continue
		}
		coverage50++
		// 50-DMA: slice the last WindowSize closes (Closes is
		// chronological, oldest first). SMA includes today's close per
		// $SPXA50R / S&P DJI convention.
		w50 := w.Closes[len(w.Closes)-WindowSize:]
		var sum50 float64
		for _, c := range w50 {
			sum50 += c
		}
		sma50 := sum50 / float64(WindowSize)
		if w50[WindowSize-1] >= sma50 {
			above50++
		}

		// 200-DMA: only contributes when the name has enough history.
		if len(w.Closes) >= WindowSize200 {
			w200 := w.Closes[len(w.Closes)-WindowSize200:]
			var sum200 float64
			for _, c := range w200 {
				sum200 += c
			}
			sma200 := sum200 / float64(WindowSize200)
			coverage200++
			if w200[WindowSize200-1] >= sma200 {
				above200++
			}
		}

		// New-highs/lows: only contributes when the rolling max/min
		if w.HighRollingBarsHad >= RollingMaxBars {
			coverageHL++
			today := w.Closes[len(w.Closes)-1]
			if w.HighRollingMax > 0 && today > w.HighRollingMax {
				newHighs++
			}
			if w.LowRollingMin > 0 && today < w.LowRollingMin {
				newLows++
			}
		}
	}

	snap.Coverage = coverage50
	snap.Coverage200 = coverage200
	snap.CoverageHighsLows = coverageHL
	if coverage50 > 0 {
		snap.Value = 100.0 * float64(above50) / float64(coverage50)
		snap.PctAbove50DMA = snap.Value
	}
	if coverage200 > 0 {
		snap.PctAbove200DMA = 100.0 * float64(above200) / float64(coverage200)
	}
	snap.NewHighsToday = newHighs
	snap.NewLowsToday = newLows
	if coverageHL > 0 {
		snap.NetNewHighsPct = 100.0 * float64(newHighs-newLows) / float64(coverageHL)
	}
	return snap
}

// SlideWindow folds today's close into a constituent window. It does
// three things in one pass:
//
//  1. Append today's close to the chronological Closes slice and trim
//     to the v2 cap of WindowSize200 entries.
//  2. Update the rolling max/min over the previous RollingMaxBars
//     closes (excluding today's), so the next Compute can detect
//     "today made a new 252-bar high".
//  3. Track HighRollingBarsHad / LowRollingBarsHad so a name with
//     fewer than RollingMaxBars of history doesn't get counted as
//     making a new high on its 30th day of trading.
//
// Same-day idempotency: if barDate matches w.LastBarAt, the existing
// tail close is overwritten and counters are not double-bumped. The
// rolling-max state for that name doesn't change on a same-day
// re-fetch — late prints settling shouldn't kick a new-high.
//
// The input is not mutated; callers assign the result back if they
// want persistence.
func SlideWindow(w ConstituentWindow, close float64, barDate string) ConstituentWindow {
	out := ConstituentWindow{
		Symbol:             w.Symbol,
		Closes:             append([]float64(nil), w.Closes...),
		LastBarAt:          barDate,
		HighRollingMax:     w.HighRollingMax,
		HighRollingBarsHad: w.HighRollingBarsHad,
		LowRollingMin:      w.LowRollingMin,
		LowRollingBarsHad:  w.LowRollingBarsHad,
	}
	if w.LastBarAt == barDate && len(out.Closes) > 0 {
		// Same trading day appearing twice — overwrite the tail to
		// reflect the corrected close. Don't grow the window. Rolling
		// max/min stays as-is: a late-print correction to today's
		// close shouldn't kick a new-high vs the prior-251-day max
		// that's already locked in.
		out.Closes[len(out.Closes)-1] = close
		return out
	}
	// Roll the prior close (if any) into the rolling max/min. The
	if len(out.Closes) > 0 {
		prevClose := out.Closes[len(out.Closes)-1]
		out.HighRollingBarsHad = min(out.HighRollingBarsHad+1, RollingMaxBars)
		out.LowRollingBarsHad = out.HighRollingBarsHad
		if out.HighRollingMax == 0 || prevClose > out.HighRollingMax {
			out.HighRollingMax = prevClose
		}
		if out.LowRollingMin == 0 || prevClose < out.LowRollingMin {
			out.LowRollingMin = prevClose
		}
		// Once we've seen RollingMaxBars bars, the simple "max-so-far"
		// exactly on every slide; instead, after we've seen the full
		if out.HighRollingBarsHad == RollingMaxBars && len(out.Closes) >= WindowSize200 {
			out.HighRollingMax = sliceMax(out.Closes)
			out.LowRollingMin = sliceMin(out.Closes)
		}
	}
	out.Closes = append(out.Closes, close)
	if len(out.Closes) > WindowSize200 {
		// Drop oldest. Keep at most WindowSize200 entries — older
		out.Closes = out.Closes[len(out.Closes)-WindowSize200:]
	}
	return out
}

// sliceMax / sliceMin are the rolling-max / rolling-min helpers used
func sliceMax(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := math.Inf(-1)
	for _, x := range xs {
		if x > m {
			m = x
		}
	}
	return m
}

func sliceMin(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	m := math.Inf(1)
	for _, x := range xs {
		if x < m {
			m = x
		}
	}
	return m
}
