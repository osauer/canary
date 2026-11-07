package daemon

import (
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The hk and sg shortcuts name the listing's own exchange and currency, because
// IBKR refuses those listings on SMART; de and uk stay SMART over a primary.
func TestTechnicalMarketShortcutsRouteToTheListing(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		in                                       rpc.TechnicalParams
		wantMarket, wantExch, wantPrimary, wantC string
	}{
		{"hong kong", rpc.TechnicalParams{Market: "hk"}, "hk", "SEHK", "", "HKD"},
		{"singapore in any case", rpc.TechnicalParams{Market: "SG"}, "sg", "SGX", "", "SGD"},
		{"an explicit exchange wins", rpc.TechnicalParams{Market: "hk", Exchange: "SEHKNTL", Currency: "CNH"}, "hk", "SEHKNTL", "", "CNH"},
		{"germany stays SMART over Xetra", rpc.TechnicalParams{Market: "de"}, "de", "SMART", "IBIS", "EUR"},
		{"london stays SMART over LSE", rpc.TechnicalParams{Market: "uk"}, "uk", "SMART", "LSE", "GBP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, routed, err := technicalRoute(tc.in)
			if err != nil || !routed {
				t.Fatalf("route = %+v routed=%v err=%v", route, routed, err)
			}
			if route.Market != tc.wantMarket || route.Exchange != tc.wantExch || route.PrimaryExch != tc.wantPrimary || route.Currency != tc.wantC {
				t.Fatalf("route = %+v", route)
			}
			contract, _, _, err := normaliseStockQuoteContract(technicalContract(route, "700"))
			if err != nil || contract.Symbol != "700" || contract.Exchange != tc.wantExch || contract.PrimaryExch != tc.wantPrimary || contract.Currency != tc.wantC {
				t.Fatalf("contract = %+v, %v", contract, err)
			}
		})
	}
	_, _, err := technicalRoute(rpc.TechnicalParams{Market: "mars"})
	if err == nil || !strings.Contains(err.Error(), `unsupported technical market "mars"`) || !strings.Contains(err.Error(), "hk, sg") {
		t.Fatalf("unknown market = %v", err)
	}
}

// The shortcuts stay out of the contract normaliser order preview shares: until
// the order path's routing is decided, an order for hk or sg is refused as it
// was, and this read widens nothing a broker write accepts.
func TestOrderContractNormaliserStillRefusesTheListingMarkets(t *testing.T) {
	for _, market := range []string{"hk", "sg"} {
		if _, _, _, err := normaliseStockQuoteContract(rpc.ContractParams{Symbol: "700", Market: market}); err == nil {
			t.Fatalf("market %s reached the shared contract normaliser", market)
		}
	}
}

func listingBar(date string, close float64) ibkrlib.HistoricalBar {
	at, err := time.ParseInLocation(time.DateOnly, date, time.UTC)
	if err != nil {
		panic(err)
	}
	return ibkrlib.HistoricalBar{Date: strings.ReplaceAll(date, "-", ""), Time: at, Open: close, High: close, Low: close, Close: close, Volume: 1000}
}

// IBKR's daily series for a Tokyo listing read through SMART on a Saturday ends
// with a thin bar dated the coming Monday. It is not a close yet, on the
// listing's own calendar, until that Monday begins in Tokyo.
func TestSettledDailyBarsDropABarDatedAfterTheListingsToday(t *testing.T) {
	berlin := time.FixedZone("CET", 60*60)
	bars := []ibkrlib.HistoricalBar{listingBar("2026-11-05", 100), listingBar("2026-11-06", 101), listingBar("2026-11-09", 90)}
	for _, tc := range []struct {
		name              string
		exchange, primary string
		now               time.Time
		wantLast          string
	}{
		{"Saturday morning in Berlin, Tokyo by exchange", "TSEJ", "", time.Date(2026, 11, 7, 8, 19, 0, 0, berlin), "2026-11-06"},
		{"Saturday morning in Berlin, Tokyo by primary", "SMART", "TSEJ", time.Date(2026, 11, 7, 8, 19, 0, 0, berlin), "2026-11-06"},
		{"Sunday evening in Tokyo", "TSEJ", "", time.Date(2026, 11, 8, 14, 0, 0, 0, berlin), "2026-11-06"},
		{"Monday open in Tokyo", "TSEJ", "", time.Date(2026, 11, 9, 1, 0, 0, 0, berlin), "2026-11-09"},
		{"a venue with no calendar, Saturday", "SMART", "", time.Date(2026, 11, 7, 8, 19, 0, 0, berlin), "2026-11-06"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := settledDailyBars(bars, tc.exchange, tc.primary, tc.now)
			if len(got) == 0 || barDate(got[len(got)-1]) != tc.wantLast {
				t.Fatalf("last bar = %v, want %s", got, tc.wantLast)
			}
		})
	}
	if got := settledDailyBars(nil, "SEHK", "", time.Now()); got != nil {
		t.Fatalf("no bars read = %v", got)
	}
}

// With the label gone the row reports Friday's close and builds its averages
// without it; a Hong Kong series ending on its last session is untouched.
func TestTechnicalRowReportsTheLastSettledClose(t *testing.T) {
	now := time.Date(2026, 11, 7, 7, 19, 0, 0, time.UTC)
	var bars []ibkrlib.HistoricalBar
	for day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC); day.Before(time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)); day = day.AddDate(0, 0, 1) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
			bars = append(bars, listingBar(day.Format(time.DateOnly), 100))
		}
	}
	withLabel := append(append([]ibkrlib.HistoricalBar{}, bars...), listingBar("2026-11-09", 80))

	row := buildTechnicalRow("SYNJ", settledDailyBars(withLabel, "TSEJ", "", now), nil, nil, now)
	if row.PriceAsOf != "2026-11-06" || row.Price == nil || *row.Price != 100 || row.Bars != len(bars) {
		t.Fatalf("Tokyo row: as of %s price %v bars %d", row.PriceAsOf, row.Price, row.Bars)
	}
	if row.SMA50 == nil || *row.SMA50 != 100 {
		t.Fatalf("Tokyo 50-day average = %v, want the label left out", row.SMA50)
	}
	hk := buildTechnicalRow("700", settledDailyBars(bars, "SEHK", "", now), nil, nil, now)
	if hk.PriceAsOf != "2026-11-06" || hk.Bars != len(bars) {
		t.Fatalf("Hong Kong row: as of %s bars %d", hk.PriceAsOf, hk.Bars)
	}
}

// A quote's history fallback reads the contract the quote named, by its contract
// id: the fill quote and the display know the listing only that way, and with no
// id the read would look the stock up on SMART, which IBKR refuses for SEHK and SGX.
func TestQuoteHistoryKeepsTheQuotesContractID(t *testing.T) {
	c := quoteHistoricalContract(&rpc.Quote{Symbol: "700", Contract: rpc.ContractParams{ConID: 900700, Symbol: "700", SecType: "STK", Exchange: "SMART", Currency: "HKD"}})
	if c.ConID != 900700 {
		t.Fatalf("history contract = %+v, want the quote's contract id", c)
	}
}
