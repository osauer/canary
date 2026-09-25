package cli

import (
	"fmt"

	"github.com/osauer/canary/v2/internal/rpc"
)

func renderMarketTapeHistory(env *Env, result *rpc.MarketTapeResult, explain bool) {
	fmt.Fprintln(env.Stdout)
	fmt.Fprintln(env.Stdout, env.bold("Market tape · what happened next"))
	riskReadLine(env, "  Latest saved observations and later S&P 500 price changes; no forecast.")
	if result.History == nil {
		riskReadLine(env, "  History unavailable.")
		return
	}
	rows := make([][]string, 0, len(result.History.Rows))
	for _, day := range result.History.Rows {
		var change, share *float64
		date := sanitizeRunText(day.Date)
		if day.Latest != nil {
			if day.Latest.Session.SPX != nil {
				change = day.Latest.Session.SPX.ChangePct
			}
			if day.Latest.Session.Breadth != nil {
				share = day.Latest.Session.Breadth.PctAbove50DMA
			}
			if day.Latest.Timing == "reconstructed" {
				date += "*"
			}
		}
		after := map[int]string{1: "—", 3: "—", 5: "—"}
		for _, follow := range day.FollowUps {
			if follow.Status == "pending" {
				after[follow.Sessions] = "waiting"
			} else {
				after[follow.Sessions] = marketTapeNumber(env, follow.SPXChangePct, "%", true)
			}
		}
		rows = append(rows, []string{date, marketTapeNumber(env, change, "%", true), marketTapeNumber(env, share, "%", false), after[1], after[3], after[5]})
	}
	fmt.Fprintln(env.Stdout)
	renderPositionTable(env, env.Stdout, []positionTableColumn{{"DAY", positionAlignLeft}, {"S&P DAY", positionAlignRight}, {"ABOVE AVG", positionAlignRight}, {"AFTER 1D", positionAlignRight}, {"AFTER 3D", positionAlignRight}, {"AFTER 5D", positionAlignRight}}, rows)
	fmt.Fprintln(env.Stdout)
	riskReadLine(env, "  Stocks above avg: % at or above their own 50-day average closing price.")
	riskReadLine(env, "  Later changes run from that day's close, using trading days only.")
	riskReadLine(env, "  * Reconstructed later. Waiting = not closed yet; — = missing data.")
	if result.Archive != nil {
		if !result.Archive.LastStoredAt.IsZero() {
			riskReadLine(env, "  Last collection: "+result.Archive.LastStoredAt.UTC().Format("2006-01-02 15:04 UTC"))
		}
		if result.Archive.Status == "collection_failed" {
			riskReadLine(env, "  Latest collection failed; saved history may be incomplete.")
		}
		if result.Archive.Status == "not_started" {
			riskReadLine(env, "  Collection has not started; keep the daemon running after the close.")
		}
	}
	if explain {
		fmt.Fprintln(env.Stdout)
		riskReadLine(env, "  This checks what followed a recorded day. It does not tell us whether a trade would have worked: the day's close preceded our observation.")
		riskReadLine(env, "  The table shows the latest saved version. Originals and corrections are kept; --json includes first/latest versions, capture times and QQQ follow-ups.")
		riskReadLine(env, "  Collection runs after the close while the daemon is running, and catches up on restart. Older imported days are labelled reconstructed.")
		riskReadLine(env, "  A scorecard and any trading rules still need to be designed and tested.")
	}
	if result.History.NextBefore != "" {
		fmt.Fprintln(env.Stdout)
		riskReadLine(env, "  Earlier: canary market tape --history --before "+sanitizeRunText(result.History.NextBefore))
	}
	fmt.Fprintln(env.Stdout)
}
