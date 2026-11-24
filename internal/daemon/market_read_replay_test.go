package daemon

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// TestMarketReadReplay replays recorded sessions through the read, five
// minutes at a time, for a person judging the tags against the day. It runs
// only when MARKET_READ_REPLAY_DATA names a directory of recorded series and
// carries no data of its own:
//
//	spx_5m.json spy_5m.json vix_5m.json   five-minute series
//	spx_30m.json spy_30m.json vix_30m.json thirty-minute series (optional)
//	spx_1d.json vix_1d.json                daily series
//	leaders/<SYMBOL>.json                  daily series per leader
//
// Each file is a market.history result or a chart-cache document holding
// one. MARKET_READ_REPLAY_OUT, when set, receives <date>/<HHMM>.json and
// <date>/closed.json, and summary.txt. A session is replayed from its SPX
// five-minute bars, or, labelled coarse, from thirty-minute bars when only
// those are recorded: then a hold is two half-hour closes, close times are
// moved to the half-hour bar's end, and the 30-minute volume tags cannot
// fire. At each step the read sees only the bars
// completed by then; the volume baseline sees only the sessions before the
// replayed one. No intraday leader series is recorded, so leader states are
// replayed at the close only, from daily bars.
func TestMarketReadReplay(t *testing.T) {
	dir := os.Getenv("MARKET_READ_REPLAY_DATA")
	if dir == "" {
		t.Skip("MARKET_READ_REPLAY_DATA not set")
	}
	out := os.Getenv("MARKET_READ_REPLAY_OUT")
	load := func(name string) *rpc.MarketHistoryResult {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil
		}
		var doc struct {
			Result *rpc.MarketHistoryResult `json:"result"`
		}
		if json.Unmarshal(raw, &doc) == nil && doc.Result != nil && len(doc.Result.Points) > 0 {
			return doc.Result
		}
		var r rpc.MarketHistoryResult
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return &r
	}
	fine := [3]*rpc.MarketHistoryResult{load("spx_5m.json"), load("spy_5m.json"), load("vix_5m.json")}
	coarse := [3]*rpc.MarketHistoryResult{load("spx_30m.json"), load("spy_30m.json"), load("vix_30m.json")}
	spx := fine[0]
	spxDaily, vixDaily := load("spx_1d.json"), load("vix_1d.json")
	if spx == nil || spxDaily == nil {
		t.Fatal("spx_5m.json and spx_1d.json are required")
	}
	ny, _ := time.LoadLocation("America/New_York")
	fineDates := map[string]bool{}
	for _, p := range spx.Points {
		fineDates[p.At.In(ny).Format(time.DateOnly)] = true
	}
	leaders := map[string]*rpc.MarketHistoryResult{}
	files, _ := filepath.Glob(filepath.Join(dir, "leaders", "*.json"))
	for _, f := range files {
		symbol := strings.TrimSuffix(filepath.Base(f), ".json")
		leaders[symbol] = load(filepath.Join("leaders", filepath.Base(f)))
	}
	dates := maps.Clone(fineDates)
	if coarse[0] != nil {
		for _, p := range coarse[0].Points {
			dates[p.At.In(ny).Format(time.DateOnly)] = true
		}
	}
	var summary strings.Builder
	for _, date := range slices.Sorted(func(yield func(string) bool) {
		for d := range dates {
			if !yield(d) {
				return
			}
		}
	}) {
		day, _ := time.ParseInLocation(time.DateOnly, date, ny)
		sessions, err := tapeArchiveCalendar(day.AddDate(0, 0, -45), 50)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(sessions, func(s marketcal.Session) bool { return s.Date == date })
		if i < 1 {
			continue
		}
		session, prior := sessions[i], sessions[i-1]
		set, step, label := fine, marketReadBar, "5-minute"
		if !fineDates[date] {
			set, step, label = coarse, 30*time.Minute, "COARSE 30-minute"
		}
		spx, spy, vix := set[0], set[1], set[2]
		cut := func(r *rpc.MarketHistoryResult, now time.Time, keep func(time.Time) bool) *rpc.MarketHistoryResult {
			if r == nil {
				return nil
			}
			c := *r
			c.AsOf, c.Cache = now, nil
			// The read checks the series is intraday; coarse bars pass as such.
			c.Interval = "5 mins"
			c.Points = nil
			for _, p := range r.Points {
				if keep(p.At) && !p.At.Add(step).After(now) {
					c.Points = append(c.Points, p)
				}
			}
			return &c
		}
		sameDay := func(at time.Time) bool { return at.In(ny).Format(time.DateOnly) == date }
		beforeOrSameDay := func(at time.Time) bool { return at.In(ny).Format(time.DateOnly) <= date }
		baselineDays := map[string]bool{}
		if spy != nil {
			for _, p := range spy.Points {
				if d := p.At.In(ny).Format(time.DateOnly); d < date && marketReadRegular(sessionOn(sessions, d), p.At) {
					baselineDays[d] = true
				}
			}
		}
		var vixPrior *broker.BriefQuote
		if bar := marketReadDailyBar(vixDaily, "VIX", prior.Date); bar != nil {
			vixPrior = &broker.BriefQuote{Close: bar.Value}
		}
		read := func(now time.Time, closed bool) *rpc.MarketReadResult {
			src := marketReadSources{SPXDaily: spxDaily, SPXIntraday: cut(spx, now, sameDay), SPYIntraday: cut(spy, now, beforeOrSameDay), VIXIntraday: cut(vix, now, sameDay)}
			if vixPrior != nil {
				q := *vixPrior
				src.VIXQuote = &q
			}
			if closed {
				for _, c := range marketReadLeaderContracts() {
					src.Leaders = append(src.Leaders, marketReadLeaderSource{Symbol: c.Symbol, Daily: leaders[c.Symbol]})
				}
			}
			r, err := assembleMarketRead(now, sessions, src)
			if err != nil {
				t.Fatal(err)
			}
			// The read serves five-minute bars only and dates a bar's close
			// at its start plus five minutes. Coarse bars close later: move
			// every bar-derived close time to the bar's real end.
			if shift := step - marketReadBar; shift > 0 {
				move := func(at *time.Time) {
					if !at.IsZero() {
						*at = at.Add(shift)
					}
				}
				move(&r.Index.LastAt)
				move(&r.VIX.LastAt)
				for i := range r.Tags {
					move(&r.Tags[i].Since)
				}
				for i := range r.TagEvents {
					move(&r.TagEvents[i].From)
					move(&r.TagEvents[i].To)
				}
			}
			return r
		}
		write := func(name string, r *rpc.MarketReadResult) {
			if out == "" {
				return
			}
			raw, _ := json.MarshalIndent(r, "", "  ")
			path := filepath.Join(out, date, name+".json")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Fprintf(&summary, "== %s (prior %s) %s bars; baseline sessions recorded: %d\n", date, prior.Date, label, len(baselineDays))
		moments := map[string]bool{"0945": true, "1030": true, "1200": true, "1400": true, "1555": true}
		for now := session.Open.Add(marketReadBar); !now.After(session.Close); now = now.Add(marketReadBar) {
			hhmm := now.In(ny).Format("1504")
			r := read(now, false)
			if moments[hhmm] {
				write(hhmm, r)
				fmt.Fprintf(&summary, "  %s %s\n", hhmm, replayLine(r))
			} else if now.In(ny).Minute()%30 == 0 {
				fmt.Fprintf(&summary, "  %s %s\n", hhmm, replayVolume(r))
			}
		}
		closed := read(session.Close.Add(20*time.Minute), true)
		write("closed", closed)
		fmt.Fprintf(&summary, "  close %s\n", replayLine(closed))
		for _, row := range closed.Leaders.Rows {
			gap := "—"
			if row.GapPct != nil {
				gap = fmt.Sprintf("%+.2f%%", *row.GapPct)
			}
			fmt.Fprintf(&summary, "    leader %-5s gap %s %s%s\n", row.Symbol, gap, row.State, row.StateUnavailable)
		}
		for _, e := range closed.TagEvents {
			to := "standing"
			if !e.To.IsZero() {
				to = e.To.In(ny).Format("15:04") + fmt.Sprintf(" (%d min)", int(e.To.Sub(e.From).Minutes()))
			}
			fmt.Fprintf(&summary, "  event %-6s %-22s %s → %s\n", e.Scope, e.Code, e.From.In(ny).Format("15:04"), to)
		}
	}
	t.Log("\n" + summary.String())
	if out != "" {
		if err := os.WriteFile(filepath.Join(out, "summary.txt"), []byte(summary.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func sessionOn(sessions []marketcal.Session, date string) marketcal.Session {
	for _, s := range sessions {
		if s.Date == date {
			return s
		}
	}
	return marketcal.Session{}
}

func replayLine(r *rpc.MarketReadResult) string {
	var tags []string
	for _, tag := range r.Tags {
		tags = append(tags, tag.Scope+":"+tag.Code)
	}
	last := "—"
	if r.Index.Last != nil {
		last = fmt.Sprintf("%.2f", *r.Index.Last)
	}
	vix := "—"
	if r.VIX.Last != nil {
		vix = fmt.Sprintf("%.2f", *r.VIX.Last)
	}
	return fmt.Sprintf("SPX %s %s VIX %s %s [%s]", last, r.Index.Position, vix, replayVolume(r), strings.Join(tags, " "))
}

func replayVolume(r *rpc.MarketReadResult) string {
	v := r.Index.Volume
	switch {
	case v == nil:
		return "rel vol — (" + r.Index.VolumeUnavailable + ")"
	case v.Relative == nil:
		return fmt.Sprintf("rel vol — (%s; %d sessions)", v.RelativeUnavailable, v.BaselineSessions)
	}
	return fmt.Sprintf("rel vol %.2f (%d sessions)", *v.Relative, v.BaselineSessions)
}
