package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/watchlist"
)

func TestRunWatchlistAddListJSONWithoutDaemon(t *testing.T) {
	oldPath := watchlistDefaultPath
	t.Cleanup(func() { watchlistDefaultPath = oldPath })
	path := filepath.Join(t.TempDir(), "watchlist.json")
	watchlistDefaultPath = func() (string, error) { return path, nil }

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := Run(context.Background(), env, "watch", []string{"IBM", "--add", "--json"}); code != 0 {
		t.Fatalf("watch add exit = %d, stderr=%q", code, stderr.String())
	}
	var add watchlist.Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &add); err != nil {
		t.Fatalf("decode add JSON: %v\n%s", err, stdout.String())
	}
	if len(add.Symbols) != 1 || add.Symbols[0] != "IBM" {
		t.Fatalf("add snapshot = %+v, want IBM", add)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), env, "watch", []string{"--list", "--json"}); code != 0 {
		t.Fatalf("watch list exit = %d, stderr=%q", code, stderr.String())
	}
	var list watchlist.Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &list); err != nil {
		t.Fatalf("decode list JSON: %v\n%s", err, stdout.String())
	}
	if len(list.Symbols) != 1 || list.Symbols[0] != "IBM" {
		t.Fatalf("list snapshot = %+v, want IBM", list)
	}
}

func TestRunWatchlistClearText(t *testing.T) {
	oldPath := watchlistDefaultPath
	t.Cleanup(func() { watchlistDefaultPath = oldPath })
	path := filepath.Join(t.TempDir(), "watchlist.json")
	watchlistDefaultPath = func() (string, error) { return path, nil }

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := Run(context.Background(), env, "watch", []string{"IBM", "--add"}); code != 0 {
		t.Fatalf("watch add exit = %d, stderr=%q", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), env, "watch", []string{"--clear"}); code != 0 {
		t.Fatalf("watch clear exit = %d, stderr=%q", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "No symbols in watchlist") {
		t.Fatalf("clear output missing empty-list message:\n%s", out)
	}
}

func TestRenderWatchlistQuoteTextRichRows(t *testing.T) {
	t.Parallel()
	nextOpen := time.Date(2026, 5, 26, 9, 30, 0, 0, mustTestLocation(t, "America/New_York"))
	res := &rpc.WatchlistResult{
		Name:    "default",
		Symbols: []string{"AAPL", "MSFT"},
		AsOf:    time.Date(2026, 5, 25, 16, 15, 0, 0, time.UTC),
		Rows: []rpc.WatchlistRow{
			{
				Quote: rpc.Quote{
					Symbol:            "AAPL",
					Contract:          rpc.ContractParams{Symbol: "AAPL", SecType: "STK", Currency: "USD"},
					Price:             new(190.12),
					PriceSource:       "last",
					RegularClose:      new(188.20),
					RegularCloseAt:    time.Date(2026, 5, 22, 16, 0, 0, 0, mustTestLocation(t, "America/New_York")),
					PriorRegularClose: new(186.28),
					RegularChange:     new(1.92),
					RegularChangePct:  new(1.03),
					QuotePrice:        new(190.12),
					QuotePriceSource:  "last",
					QuotePriceAt:      time.Date(2026, 5, 22, 16, 1, 2, 0, mustTestLocation(t, "America/New_York")),
					QuoteChangePct:    new(1.02),
					PrevClose:         new(188.20),
					Change:            new(1.92),
					ChangePct:         new(1.02),
					DayLow:            new(187.55),
					DayHigh:           new(191.30),
					Week52Low:         new(164.08),
					Week52High:        new(199.62),
					Volume:            new(int64(41762007)),
					AvgVolume:         new(int64(58900000)),
					DataType:          rpc.MarketDataDelayed,
					PriceAt:           time.Date(2026, 5, 22, 16, 1, 2, 0, mustTestLocation(t, "America/New_York")),
					AsOf:              time.Date(2026, 5, 25, 16, 15, 0, 0, time.UTC),
					PriceAsOf:         "Delayed: May 22 at 04:01:02 PM EDT",
					StaleReason:       "price timestamp is 20m old during market hours",
					SessionContext: &rpc.MarketSession{
						Label:    "US equities",
						Timezone: "America/New_York",
						State:    "holiday",
						IsOpen:   false,
						Reason:   "Memorial Day",
						NextOpen: &nextOpen,
					},
				},
				Holding: &rpc.WatchlistHolding{Quantity: 25, Currency: "USD"},
			},
			{
				Quote: rpc.Quote{
					Symbol:            "MSFT",
					Contract:          rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Currency: "USD"},
					Price:             new(421.44),
					PriceSource:       "last",
					RegularClose:      new(422.25),
					PriorRegularClose: new(423.06),
					RegularChange:     new(-0.81),
					RegularChangePct:  new(-0.19),
					QuotePrice:        new(421.44),
					QuotePriceSource:  "last",
					QuotePriceAt:      time.Date(2026, 5, 25, 17, 30, 0, 0, mustTestLocation(t, "Europe/Berlin")),
					QuoteChangePct:    new(-0.19),
					PrevClose:         new(422.25),
					Change:            new(-0.81),
					ChangePct:         new(-0.19),
					DataType:          rpc.MarketDataLive,
					PriceAt:           time.Date(2026, 5, 25, 17, 30, 0, 0, mustTestLocation(t, "Europe/Berlin")),
					AsOf:              time.Date(2026, 5, 25, 21, 15, 0, 0, mustTestLocation(t, "Europe/Berlin")),
					SessionContext: &rpc.MarketSession{
						Timezone: "Europe/Berlin",
						State:    "regular",
						IsOpen:   true,
					},
				},
			},
		},
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
	if code := renderWatchlistQuoteText(env, &stdout, res); code != 0 {
		t.Fatalf("render exit = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"SYMBOL", "CCY", "CLOSE", "C-CHG", "C%", "QUOTE", "Q%", "DAY", "52W", "VOL/AVG", "DATA", "AS OF",
		"AAPL", "25 sh", "USD", "188.20", "+1.92", "+1.03%", "190.12", "+1.02%",
		"187.55-191.30", "164.08-199.62", "41.8M/58.9M",
		"stale", "closed close May22 / quote May22 16:01 EDT",
		"MSFT", "422.25", "421.44", "-0.81", "-0.19%", "live", "open quote May25 17:30 CEST",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("watchlist quote output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Memorial Day") || strings.Contains(out, "next open") {
		t.Fatalf("watchlist monitor should not print per-row calendar prose:\n%s", out)
	}
	if !strings.Contains(out, ansiGreen) {
		t.Fatalf("positive movement should be green:\n%q", out)
	}
	if !strings.Contains(out, ansiRed) {
		t.Fatalf("negative movement should be red:\n%q", out)
	}
	if !strings.Contains(out, ansiYellow+"stale"+ansiReset) {
		t.Fatalf("stale data badge should be yellow:\n%q", out)
	}
}

func TestFormatWatchlistAsOfIncludesPreMarketState(t *testing.T) {
	t.Parallel()
	loc := mustTestLocation(t, "America/New_York")
	row := rpc.WatchlistRow{
		Quote: rpc.Quote{
			Symbol:      "IBM",
			Price:       new(252.01),
			PriceSource: "last",
			PriceAt:     time.Date(2026, 5, 26, 8, 15, 0, 0, loc),
			AsOf:        time.Date(2026, 5, 26, 8, 15, 0, 0, loc),
			SessionContext: &rpc.MarketSession{
				Timezone: "America/New_York",
				State:    "regular",
				IsOpen:   false,
				Open:     time.Date(2026, 5, 26, 9, 30, 0, 0, loc),
			},
		},
	}

	if got, want := formatWatchlistAsOf(row), "pre-market quote May26 08:15 EDT"; got != want {
		t.Fatalf("formatWatchlistAsOf = %q, want %q", got, want)
	}
}

func TestRunWatchlistQuotesRejectsPositionalSymbols(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := Run(context.Background(), env, "watch", []string{"AAPL", "--quotes"}); code == 0 {
		t.Fatalf("watch --quotes with positional symbol should fail, stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unexpected symbol") {
		t.Fatalf("stderr should explain positional-symbol error:\n%s", stderr.String())
	}
}

func TestWatchlistQuoteContractUsesHoldingRoute(t *testing.T) {
	t.Parallel()
	got := watchlistQuoteContract("MBG", &rpc.WatchlistHolding{Currency: "EUR", Exchange: "IBIS"})
	if got.Currency != "EUR" || got.Market != "de" || got.Exchange != "" {
		t.Fatalf("watchlistQuoteContract route = %+v, want market=de EUR", got)
	}
}

func mustTestLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

type watchlistFakePositionsConn struct {
	pos rpc.PositionsResult
}

func (f *watchlistFakePositionsConn) Call(ctx context.Context, method string, params any, out any) error {
	if method == rpc.MethodPositionsList {
		*(out.(*rpc.PositionsResult)) = f.pos
		return nil
	}
	return fmt.Errorf("unexpected method %s", method)
}

func (f *watchlistFakePositionsConn) Stream(ctx context.Context, method string, params any, fn func(json.RawMessage) error) error {
	return fmt.Errorf("unexpected stream %s", method)
}

// The positions "Stocks" slice carries every secType that is not OPT — bonds,
// bills, funds, futures, cash. Keyed by bare symbol with no filter, the
// desk's own T-bond shape rendered as an AT&T holding in `canary watch`, its
// currency and exchange steering the quote contract, and with the equity also
// held the winner flipped per call. Only equity rows may join.
func TestFetchWatchlistStockHoldingsFiltersNonEquityRows(t *testing.T) {
	t.Parallel()
	bond := rpc.PositionView{Symbol: "T", SecType: "BOND", Quantity: 10000, Mark: 98.5, Currency: "USD", Exchange: "SMART"}
	stock := rpc.PositionView{Symbol: "T", SecType: rpc.SecTypeStock, Quantity: 100, Mark: 27.1, AvgCost: 25, Currency: "USD", Exchange: "NYSE"}

	env := &Env{Conn: &watchlistFakePositionsConn{pos: rpc.PositionsResult{Stocks: []rpc.PositionView{bond}}}}
	if got := fetchWatchlistStockHoldings(context.Background(), env); got["T"] != nil {
		t.Fatalf("a bond row joined the watchlist as a stock holding: %+v", got["T"])
	}

	// With the equity also held, the equity's values must win in either slice
	// order — the pre-fix winner depended on iteration order.
	for _, rows := range [][]rpc.PositionView{{bond, stock}, {stock, bond}} {
		env := &Env{Conn: &watchlistFakePositionsConn{pos: rpc.PositionsResult{Stocks: rows}}}
		h := fetchWatchlistStockHoldings(context.Background(), env)["T"]
		if h == nil || h.Quantity != 100 || h.Exchange != "NYSE" || h.Mark != 27.1 {
			t.Fatalf("holding for T = %+v, want the equity row (qty 100, NYSE) regardless of row order", h)
		}
	}
}
