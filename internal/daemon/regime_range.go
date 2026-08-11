package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// range52WCoverage is the minimum calendar span the collected history must
// actually cover before a "52-week" range is served; anything shorter would
// label a partial window as a year.
const range52WCoverage = 330 * 24 * time.Hour

func range52WFromBars(bars []ibkrlib.HistoricalBar, last float64, now time.Time) *rpc.RegimeRange52W {
	cutoff := now.Add(-365 * 24 * time.Hour)
	var vals []float64
	oldest := time.Time{}
	for _, bar := range bars {
		at := historyBarAsOf(bar, now)
		if at.IsZero() || at.Before(cutoff) || bar.Close <= 0 {
			continue
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
		vals = append(vals, bar.Close)
	}
	return range52WFrom(vals, oldest, last, now)
}

func range52WFromSeries(points []regimeSeriesPoint, last float64, now time.Time) *rpc.RegimeRange52W {
	cutoff := now.Add(-365 * 24 * time.Hour)
	var vals []float64
	oldest := time.Time{}
	for _, p := range points {
		if p.Date.Before(cutoff) || p.Value <= 0 {
			continue
		}
		if oldest.IsZero() || p.Date.Before(oldest) {
			oldest = p.Date
		}
		vals = append(vals, p.Value)
	}
	return range52WFrom(vals, oldest, last, now)
}

// range52WFrom folds the current reading into the year's low/high so a print
// outside the historical range still lands inside [0,100].
func range52WFrom(vals []float64, oldest time.Time, last float64, now time.Time) *rpc.RegimeRange52W {
	if last <= 0 || len(vals) == 0 || oldest.IsZero() || now.Sub(oldest) < range52WCoverage {
		return nil
	}
	low, high := last, last
	for _, v := range vals {
		low = min(low, v)
		high = max(high, v)
	}
	if high <= low {
		return nil
	}
	return &rpc.RegimeRange52W{Low: low, High: high, Pos: (last - low) / (high - low) * 100}
}
