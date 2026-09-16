package daemon

import (
	"context"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"math"
	"testing"
	"time"
)

func TestMarketHistorySMARTUsesResolvedSession(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, now, wantDay string }{
		{"Sunday", "2026-09-13T12:00:00Z", "2026-09-11"},
		{"holiday_Monday", "2026-09-07T16:00:00Z", "2026-09-04"},
		{"after_holiday_before_premarket", "2026-09-08T07:59:00Z", "2026-09-04"},
		{"after_holiday_premarket", "2026-09-08T08:00:00Z", "2026-09-08"},
		{"spring_DST", "2026-03-08T12:00:00Z", "2026-03-06"},
		{"fall_DST_before_premarket", "2026-11-02T08:59:00Z", "2026-10-30"},
		{"fall_DST_premarket", "2026-11-02T09:00:00Z", "2026-11-02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, _ := time.Parse(time.RFC3339, tc.now)
			start, _ := time.ParseInLocation("2006-01-02", tc.wantDay, loc)
			p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "1D"}
			got, err := fetchMarketHistory(t.Context(), p, 0, now, func(_ context.Context, c ibkr.Contract, days int, interval string, _ time.Duration) (ibkr.ChartSeries, error) {
				if days < int(math.Ceil(now.Sub(start).Hours()/24)) || days > 6 || interval != "5 mins" {
					t.Errorf("acquisition cannot cover the selected session within its bound: days=%d interval=%s start=%s", days, interval, start)
				}
				if c.Exchange != "SMART" || c.ConID != 0 || c.PrimaryExch != "" {
					t.Fatal("unresolved request was relabelled before broker resolution")
				}
				c.ConID, c.PrimaryExch = 123456, "NYSE"
				return ibkr.ChartSeries{Contract: c, WhatToShow: "TRADES", Bars: []ibkr.HistoricalBar{
					{Time: start.Add(-20 * time.Hour), Close: 99, Volume: 10},
					{Time: start.Add(4 * time.Hour), Close: 101, Volume: 20},
				}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !got.RequestedStart.Equal(start) || len(got.Points) != 1 || got.Points[0].Value != 101 || got.Contract.PrimaryExch != "NYSE" {
				t.Fatalf("resolved session not retained exactly: %+v", got)
			}
			if got.Range != "1D" || got.AsOf != now || got.RegularHoursOnly || got.TimestampKind != "instant" || got.PriceBasis != "TRADES" {
				t.Fatalf("selection lost source semantics: %+v", got)
			}
		})
	}
}

func TestMarketHistoryUnsupportedVenueKeepsRollingWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	p := rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "1D"}
	got, err := fetchMarketHistory(t.Context(), p, 0, now, func(_ context.Context, c ibkr.Contract, _ int, _ string, _ time.Duration) (ibkr.ChartSeries, error) {
		c.ConID, c.PrimaryExch = 123456, "LSE"
		return ibkr.ChartSeries{Contract: c, WhatToShow: "TRADES", Bars: []ibkr.HistoricalBar{
			{Time: now.Add(-48 * time.Hour), Close: 99},
			{Time: now.Add(-time.Hour), Close: 101},
		}}, nil
	})
	if err != nil || !got.RequestedStart.Equal(now.Add(-24*time.Hour)) || len(got.Points) != 1 || got.Points[0].Value != 101 {
		t.Fatalf("unknown venue acquired a US calendar: %+v %v", got, err)
	}
}

func TestMarketHistoryCannotTurnDailyBarsIntoIntraday(t *testing.T) {
	for _, r := range []string{"", "All", "1S", "unbounded"} {
		if _, _, err := marketHistoryWindow(r, time.Now()); err == nil {
			t.Fatal("unbounded history accepted")
		}
	}
	_, interval, err := marketHistoryWindow("1D", time.Now())
	if err != nil || interval != "5 mins" {
		t.Fatal("intraday request became daily data")
	}
}
func TestUnderlyingQuoteRetainsHeldStockIdentity(t *testing.T) {
	g := rpc.PositionGroup{Underlying: "SYNTH", Stock: &rpc.PositionView{ConID: 91, Symbol: "SYNTH", SecType: "STOCK", Currency: "EUR", Exchange: "IBIS"}}
	c, ok := rpc.UnderlyingMarketContract(g)
	if !ok || c.ConID != 91 || c.Currency != "EUR" || c.Exchange != "IBIS" {
		t.Fatal("held identity lost")
	}
	g.Stock = nil
	g.Options = []rpc.PositionView{{Symbol: "SYNTH", Currency: "USD"}}
	c, ok = rpc.UnderlyingMarketContract(g)
	if !ok || c.SecType != "STK" || c.ConID != 0 {
		t.Fatal("option identity masquerades as underlying")
	}
}

func TestHistoryCalendarRangeDoesNotExposeRoundedBrokerLookback(t *testing.T) {
	now := time.Date(2026, 9, 9, 16, 0, 0, 0, time.UTC)
	for r, want := range map[string]string{"1Y": "2025-09-09", "5Y": "2021-09-09", "YTD": "2026-01-01", "1M": "2026-08-09"} {
		if got := marketHistoryStart(r, now).Format("2006-01-02"); got != want {
			t.Fatalf("%s: %s != %s", r, got, want)
		}
	}
}

func TestMarketHistoryClampsMonthEnds(t *testing.T) {
	for _, tc := range []struct{ now, rangeName, want string }{
		{"2026-03-31", "1M", "2026-02-28"},
		{"2024-03-31", "1M", "2024-02-29"},
		{"2026-10-31", "6M", "2026-04-30"},
		{"2024-02-29", "1Y", "2023-02-28"},
		{"2024-02-29", "5Y", "2019-02-28"},
	} {
		now, _ := time.Parse("2006-01-02", tc.now)
		now = now.Add(16*time.Hour + 17*time.Minute)
		got := marketHistoryStart(tc.rangeName, now)
		if got.Format("2006-01-02") != tc.want || got.Hour() != 16 || got.Minute() != 17 {
			t.Fatalf("%s %s: got %s, want %s at 16:17", tc.now, tc.rangeName, got, tc.want)
		}
	}
}
