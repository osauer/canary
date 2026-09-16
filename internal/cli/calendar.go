package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func runCalendar(ctx context.Context, env *Env, args []string) int {
	fs := flagSet(env, "calendar")
	jsonOut := fs.Bool("json", false, "emit the daemon's typed exchange-session calendar")
	market := fs.String("market", "us", "exchange sessions: us | us-options | de | uk | jp | hk")
	date := fs.String("date", "", "YYYY-MM-DD in the market timezone, evaluated at local noon; default now")
	at := fs.String("at", "", "RFC3339 instant with timezone offset; takes precedence over --date")
	days := fs.Int("days", 14, "forward calendar dates including the selected date; default 14, capped at 400")
	if err := fs.Parse(args); err != nil {
		return parseExit(err)
	}
	if fs.NArg() != 0 {
		return failUnexpectedArgs(env, fs)
	}
	params := rpc.MarketCalendarParams{Market: *market, Date: *date, Days: *days}
	if *at != "" {
		var err error
		params.At, err = time.Parse(time.RFC3339, *at)
		if err != nil {
			return fail(env, "calendar: --at must be an RFC3339 instant: %v", err)
		}
	}
	var res rpc.MarketCalendarResult
	if err := env.Conn.Call(ctx, rpc.MethodMarketCalendar, params, &res); err != nil {
		return fail(env, "calendar: %v", err)
	}
	if *jsonOut {
		return printJSON(env, res)
	}
	renderCalendar(env, res)
	return 0
}

func renderCalendar(env *Env, res rpc.MarketCalendarResult) {
	riskReadLine(env, "Market calendar", res.Label, res.Timezone)
	riskReadLine(env, "Source", res.Source, res.SourceURL)
	riskReadLine(env, "Coverage", res.CoverageStart+" to "+res.CoverageEnd)
	rows := res.Sessions
	if len(rows) == 0 {
		rows = []rpc.MarketSession{res.Session}
	}
	for _, row := range rows {
		riskReadLine(env, row.Date, row.State, row.Reason)
		if !row.Open.IsZero() && !row.Close.IsZero() {
			riskReadLine(env, "  Session", row.Open.Format(time.RFC3339)+" to "+row.Close.Format(time.RFC3339))
			if len(row.Windows) > 1 {
				for _, window := range row.Windows {
					riskReadLine(env, "  Trading window", window.Open.Format(time.RFC3339)+" to "+window.Close.Format(time.RFC3339))
				}
			}
		}
	}
	if res.Session.NextOpen != nil {
		riskReadLine(env, "Next open", res.Session.NextOpen.Format(time.RFC3339))
	}
	fmt.Fprintln(env.Stdout, "Exchange sessions only; economic releases and earnings require separate evidence.")
}
