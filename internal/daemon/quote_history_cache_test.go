package daemon

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestQuoteHistoryBarsAreReadOncePerContractUntilTheNextClose(t *testing.T) {
	s := &Server{quoteHistory: newQuoteHistoryCache()}
	saturday := time.Date(2026, 9, 19, 14, 0, 0, 0, time.UTC)
	reads := 0
	fetch := func() ([]ibkr.HistoricalBar, error) {
		reads++
		return []ibkr.HistoricalBar{{Date: "20260918", Close: 100 + float64(reads)}}, nil
	}
	spy := quoteLiquidityKey{symbol: "SPY", exchange: "SMART", currency: "USD"}
	bars, err := s.quoteHistoryBars(spy, marketcal.MarketUSEquity, saturday, fetch)
	if err != nil || len(bars) != 1 || reads != 1 {
		t.Fatalf("first read: %v %v reads=%d", bars, err, reads)
	}
	again, _ := s.quoteHistoryBars(spy, marketcal.MarketUSEquity, saturday.Add(3*time.Hour), fetch)
	if reads != 1 || again[0].Close != 101 {
		t.Fatalf("a second quote request on the same closed weekend must not read again: reads=%d %v", reads, again)
	}
	if _, _ = s.quoteHistoryBars(quoteLiquidityKey{symbol: "IBM", exchange: "SMART", currency: "USD"}, marketcal.MarketUSEquity, saturday, fetch); reads != 2 {
		t.Fatalf("another contract is its own read: reads=%d", reads)
	}
	mondayOpen := time.Date(2026, 9, 21, 19, 50, 0, 0, time.UTC) // 15:50 New York, session open
	if _, _ = s.quoteHistoryBars(spy, marketcal.MarketUSEquity, mondayOpen, fetch); reads != 2 {
		t.Fatalf("completed sessions stay current through the next session: reads=%d", reads)
	}
	amd := quoteLiquidityKey{symbol: "AMD", exchange: "SMART", currency: "USD"}
	if _, _ = s.quoteHistoryBars(amd, marketcal.MarketUSEquity, mondayOpen, fetch); reads != 3 {
		t.Fatalf("a contract first read during a session is read: reads=%d", reads)
	}
	if _, _ = s.quoteHistoryBars(amd, marketcal.MarketUSEquity, mondayOpen.Add(30*time.Second), fetch); reads != 3 {
		t.Fatalf("within the minute its moving bar is reused: reads=%d", reads)
	}
	if _, _ = s.quoteHistoryBars(amd, marketcal.MarketUSEquity, mondayOpen.Add(2*time.Minute), fetch); reads != 4 {
		t.Fatalf("bars read during a session last a minute, the last bar is still moving: reads=%d", reads)
	}
	mondayJustClosed := time.Date(2026, 9, 21, 20, 5, 0, 0, time.UTC) // 16:05 New York
	if _, _ = s.quoteHistoryBars(spy, marketcal.MarketUSEquity, mondayJustClosed, fetch); reads != 5 {
		t.Fatalf("the close ends the weekend's bars: reads=%d", reads)
	}
	if _, _ = s.quoteHistoryBars(spy, marketcal.MarketUSEquity, mondayJustClosed.Add(5*time.Minute), fetch); reads != 5 {
		t.Fatalf("bars read while the close settles are reused inside the settle window: reads=%d", reads)
	}
	mondaySettled := time.Date(2026, 9, 21, 20, 20, 0, 0, time.UTC) // 16:20 New York
	if _, _ = s.quoteHistoryBars(spy, marketcal.MarketUSEquity, mondaySettled, fetch); reads != 6 {
		t.Fatalf("after the settle window the final bar is read once: reads=%d", reads)
	}
	if _, _ = s.quoteHistoryBars(spy, marketcal.MarketUSEquity, time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC), fetch); reads != 6 {
		t.Fatalf("those bars stand until Tuesday's close: reads=%d", reads)
	}
	if _, _ = s.quoteHistoryBars(spy, marketcal.MarketUSEquity, time.Date(2026, 9, 22, 20, 1, 0, 0, time.UTC), fetch); reads != 7 {
		t.Fatalf("Tuesday's close ends them: reads=%d", reads)
	}
}

func TestQuoteHistoryKeepsAFailedReadBrieflyAndWorksWithoutACache(t *testing.T) {
	s := &Server{quoteHistory: newQuoteHistoryCache()}
	now := time.Date(2026, 9, 19, 14, 0, 0, 0, time.UTC)
	reads := 0
	refused := errors.New("historical rate limit: context deadline exceeded")
	fetch := func() ([]ibkr.HistoricalBar, error) { reads++; return nil, refused }
	key := quoteLiquidityKey{symbol: "HGENQ", exchange: "SMART", currency: "USD"}
	if _, err := s.quoteHistoryBars(key, marketcal.MarketUSEquity, now, fetch); !errors.Is(err, refused) || reads != 1 {
		t.Fatalf("first read: %v reads=%d", err, reads)
	}
	if _, err := s.quoteHistoryBars(key, marketcal.MarketUSEquity, now.Add(2*time.Minute), fetch); !errors.Is(err, refused) || reads != 1 {
		t.Fatalf("a refused symbol is not asked for on every quote request: %v reads=%d", err, reads)
	}
	if _, _ = s.quoteHistoryBars(key, marketcal.MarketUSEquity, now.Add(6*time.Minute), fetch); reads != 2 {
		t.Fatalf("after five minutes it is tried again: reads=%d", reads)
	}
	if until := quoteHistoryValidUntil("", now); !until.IsZero() {
		t.Fatalf("an unknown market has no close to wait for: %v", until)
	}
	if until := quoteHistoryValidUntil(marketcal.MarketUSEquity, now); !until.Equal(time.Date(2026, 9, 21, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("a weekend's bars stand until Monday's close: %v", until)
	}
	bare := &Server{}
	if _, _ = bare.quoteHistoryBars(key, marketcal.MarketUSEquity, now, fetch); reads != 3 {
		t.Fatalf("without a cache every request reads, as before: reads=%d", reads)
	}
}

// The broker's own "no security definition" verdict is kept as long as the
// connector's longest re-resolution backoff, so a delisted holding is not
// asked for on every five-minute cycle.
func TestQuoteHistoryKeepsTheBrokersDefinitionVerdictLonger(t *testing.T) {
	s := &Server{quoteHistory: newQuoteHistoryCache()}
	now := time.Date(2026, 9, 19, 14, 0, 0, 0, time.UTC)
	reads := 0
	verdict := fmt.Errorf("contract details unresolved for HGENQ: %w", ibkr.ErrContractNoDefinition)
	fetch := func() ([]ibkr.HistoricalBar, error) { reads++; return nil, verdict }
	key := quoteLiquidityKey{symbol: "HGENQ", exchange: "SMART", currency: "USD"}
	if _, err := s.quoteHistoryBars(key, marketcal.MarketUSEquity, now, fetch); !errors.Is(err, ibkr.ErrContractNoDefinition) || reads != 1 {
		t.Fatalf("first read: %v reads=%d", err, reads)
	}
	if _, _ = s.quoteHistoryBars(key, marketcal.MarketUSEquity, now.Add(25*time.Minute), fetch); reads != 1 {
		t.Fatalf("the verdict was asked for again within half an hour: reads=%d", reads)
	}
	if _, _ = s.quoteHistoryBars(key, marketcal.MarketUSEquity, now.Add(31*time.Minute), fetch); reads != 2 {
		t.Fatalf("after half an hour the name earns one probe: reads=%d", reads)
	}
}

// The bare-symbol fallback re-routes a stock the classifier may know better;
// it cannot help an index or future, a definition verdict, or an inactive
// name, and re-asking there only draws a second code 200.
func TestQuoteHistoryRetriesBySymbolOnlyWhereTheClassifierCanHelp(t *testing.T) {
	transport := errors.New("historical rate limit: context deadline exceeded")
	for _, tc := range []struct {
		secType string
		err     error
		want    bool
	}{
		{"STK", transport, true},
		{"", transport, true},
		{"IND", transport, false},
		{"FUT", transport, false},
		{"STK", fmt.Errorf("unresolved: %w", ibkr.ErrContractNoDefinition), false},
		{"STK", ibkr.ErrSymbolInactive, false},
	} {
		if got := quoteHistoryRetriesBySymbol(tc.secType, tc.err); got != tc.want {
			t.Errorf("quoteHistoryRetriesBySymbol(%q, %v) = %t, want %t", tc.secType, tc.err, got, tc.want)
		}
	}
}
