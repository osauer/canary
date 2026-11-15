package daemon

import (
	"fmt"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The core row's due time (core-sleeve.md, "Window and order terms", "Due
// time"): fifteen minutes after the US cash open of the row's session, when
// European S&P 500 fund lines quote tightest (their market makers hedge in
// US instruments, the reason the window skips US-closed sessions too).
// Before it a served row reads opening_window with the due time as its
// default send, the preview, the prepared path and the submit hold it with
// one blocker, and the standing-mandate controller refuses it before
// journaling and sends at its first ready read; the owner's hand is told the
// same. Until 2026-10-18 readiness read ready five minutes after the London
// open, hours before the US open, and nothing enforced the due time (the
// quality loop, 2026-10-18 23:00 CEST; built at the integrator's ask).

const (
	// coreDueAfterUSOpen is how long after the US cash open the row is due:
	// 15:45 Europe/Berlin on a day both markets keep their usual clocks.
	coreDueAfterUSOpen = 15 * time.Minute
	// coreBeforeDueCode is the blocker a core row carries before its due
	// time; readiness reads it as opening_window.
	coreBeforeDueCode = "core_before_due_time"
)

// coreRowDueAt is the due time of a core row's session, in UTC: the US cash
// open of that date plus coreDueAfterUSOpen. False for a row outside the
// core, a row without a session, or a date the US calendar does not trade
// or cover (the window never names one, and a sale to zero's open session
// neither); the row then follows the stock opening offset as before.
func coreRowDueAt(prop rpc.TradeProposal, now time.Time) (time.Time, bool) {
	if !rpc.StrategyRowBucket(prop.Bucket) || prop.Core == nil || prop.Core.Session == "" {
		return time.Time{}, false
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	res, err := cal.Query(marketcal.Query{Market: marketcal.MarketUSEquity, Date: prop.Core.Session, Days: 1})
	if err != nil || (res.Session.State != marketcal.StateRegular && res.Session.State != marketcal.StateEarlyClose) || res.Session.Open.IsZero() {
		return time.Time{}, false
	}
	return res.Session.Open.Add(coreDueAfterUSOpen).UTC(), true
}

// strategyDuePhrase says when a strategy row is due: the core's words for
// the core's row, the same rule in the entry's name for another entry's.
func strategyDuePhrase(prop rpc.TradeProposal, due time.Time) string {
	if prop.Bucket == rpc.TradeProposalBucketStrategyRebalance && prop.Core != nil {
		return fmt.Sprintf("the %s row is sent from %s, %d minutes after the US cash open of its session, as the core's is; before that it holds",
			prop.Core.Strategy, due.UTC().Format(time.RFC3339), int(coreDueAfterUSOpen/time.Minute))
	}
	return fmt.Sprintf("the core's row is sent from %s, %d minutes after the US cash open of its session, when European S&P 500 fund lines quote tightest; before that it holds",
		due.UTC().Format(time.RFC3339), int(coreDueAfterUSOpen/time.Minute))
}

// coreDueBlockers holds a core row before its due time at preview, prepare
// and submit, so the owner's hand is told and nothing sends into the spread
// before the US open. Nil for any other row, and for a core row from its
// due time on.
func coreDueBlockers(prop rpc.TradeProposal, now time.Time) []rpc.TradingBlocker {
	due, ok := coreRowDueAt(prop, now)
	if !ok || !now.Before(due) {
		return nil
	}
	return []rpc.TradingBlocker{{Code: coreBeforeDueCode, Message: strategyDuePhrase(prop, due),
		Action: "Review the row from its due time; under the standing mandate the controller sends it at its first ready read after it."}}
}
