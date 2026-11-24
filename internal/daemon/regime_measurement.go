package daemon

import (
	"time"

	"github.com/osauer/canary/v3/internal/breadth/spx"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func regimeHistoryCurrent(bars []ibkrlib.HistoricalBar, now time.Time) bool {
	if len(bars) == 0 {
		return false
	}
	date := historyBarSessionDate(bars[len(bars)-1])
	return date != "" && date >= spx.CompletedSessionKey(now) && date <= nySessionKey(now)
}

// The row is judged on settled daily closes, so its clock is the session of
// those closes (owner decision 2026-11-24 18:26 CET, "Yes, closes only"); the
// live quotes carry their own times in their qualities. A current quote
// cannot freshen a stale SMA or annual-high baseline in the source summary.
func hygSPYRowAsOf(now time.Time, r rpc.RegimeHYGSPYDivergence) *rpc.RegimeAsOfSummary {
	out := gatewayAsOf(now, r.Status, r.HYGDataType, "IBKR HYG/SPY quotes plus HMDS daily bars", r.HYGQuality, r.HYG50DMAQuality, r.SPYQuality, r.SPY52WHighQuality)
	if r.CloseSession != "" && (r.Status == rpc.RegimeStatusOK || r.Status == rpc.RegimeStatusStale) {
		out = officialRowAsOf(now, r.CloseSession, "IBKR HMDS settled daily closes (HYG, SPY); live quotes shown, not banded", r.Status)
	}
	for _, missing := range r.FieldsMissing {
		var q *rpc.Quality
		if missing == "hyg_history_stale" {
			q = r.HYG50DMAQuality
		}
		if missing == "spy_history_stale" {
			q = r.SPY52WHighQuality
		}
		if q != nil && !q.AsOf.IsZero() && (out.Time.IsZero() || q.AsOf.Before(out.Time)) {
			out = asOfSummary("historical baseline stale", "stale", out.Source, q.AsOf, q.AsOf.Format("2006-01-02"), now)
		}
	}
	return out
}
