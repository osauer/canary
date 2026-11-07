package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// An order's session gate reads the listing's own calendar. Every listing Canary
// has a calendar for used to fall through to New York's, except Germany and
// London.
func TestStockOrderSessionFollowsTheListing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contract rpc.ContractParams
		want     marketcal.Market
	}{
		{"Hong Kong by primary", rpc.ContractParams{Exchange: "SMART", PrimaryExch: "SEHK", Currency: "HKD"}, marketcal.MarketHKHKEX},
		{"Hong Kong by exchange", rpc.ContractParams{Exchange: "SEHK", Currency: "HKD"}, marketcal.MarketHKHKEX},
		{"Singapore by exchange", rpc.ContractParams{Exchange: "SGX", Currency: "SGD"}, marketcal.MarketSGSGX},
		{"Tokyo through SMART", rpc.ContractParams{Exchange: "SMART", PrimaryExch: "TSEJ", Currency: "JPY"}, marketcal.MarketJPTSE},
		{"Sydney", rpc.ContractParams{Exchange: "SMART", PrimaryExch: "ASX", Currency: "AUD"}, marketcal.MarketAUASX},
		{"a symbol with only its currency", rpc.ContractParams{Exchange: "SMART", Currency: "HKD"}, marketcal.MarketHKHKEX},
		{"a dollar line in Singapore", rpc.ContractParams{Exchange: "SGX", Currency: "USD"}, marketcal.MarketSGSGX},
		{"Xetra", rpc.ContractParams{Exchange: "SMART", PrimaryExch: "IBIS2", Currency: "EUR"}, marketcal.MarketDEXetra},
		{"London", rpc.ContractParams{Exchange: "SMART", PrimaryExch: "LSEETF", Currency: "GBP"}, marketcal.MarketUKLSE},
		{"a US stock", rpc.ContractParams{Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}, marketcal.MarketUSEquity},
		{"nothing named", rpc.ContractParams{}, marketcal.MarketUSEquity},
		{"Toronto has no calendar", rpc.ContractParams{Exchange: "SMART", PrimaryExch: "TSE", Currency: "CAD"}, marketcal.MarketUSEquity},
	} {
		if got := quoteMarketForStockContract(tc.contract); got != tc.want {
			t.Errorf("%s: market = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The gate this decides: a Hong Kong preview is judged by Hong Kong's hours, so
// it passes while New York sleeps and is refused while only New York trades.
func TestStockOrderPreviewIsRefusedOnTheListingsHoursNotNewYorks(t *testing.T) {
	hongKong := rpc.ContractParams{Symbol: "SYNH", SecType: "STK", Exchange: "SMART", PrimaryExch: "SEHK", Currency: "HKD"}
	us := rpc.ContractParams{Symbol: "SYNU", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}
	hkOpen := time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)  // 10:00 in Hong Kong, 21:00 the evening before in New York
	usOpen := time.Date(2026, 11, 9, 16, 0, 0, 0, time.UTC) // 00:00 in Hong Kong, 11:00 in New York
	s := &Server{}
	for _, tc := range []struct {
		name     string
		contract rpc.ContractParams
		at       time.Time
		refused  bool
	}{
		{"Hong Kong in its session", hongKong, hkOpen, false},
		{"Hong Kong in New York's session", hongKong, usOpen, true},
		{"US in New York's session", us, usOpen, false},
		{"US in Hong Kong's session", us, hkOpen, true},
	} {
		err := s.previewMarketClosedRefusal(tc.contract, rpc.OrderTypeLMT, "", nil, nil, tc.at)
		if (err != nil) != tc.refused {
			t.Errorf("%s: refusal = %v, want refused %v", tc.name, err, tc.refused)
		}
	}
}

// IBKR's Adaptive algorithm needs SMART, so an order on a listing's own exchange
// goes as a plain limit: a queued owner order must not demand the algorithm the
// broker layer then refuses.
func TestQueuedOwnerOrderCarriesAdaptiveOnlyOnSMART(t *testing.T) {
	for _, tc := range []struct {
		contract rpc.ContractParams
		want     string
	}{
		{rpc.ContractParams{SecType: "STK", Exchange: "SMART", Currency: "USD"}, "Normal"},
		{rpc.ContractParams{SecType: "OPT", Exchange: "SMART", Currency: "USD"}, "Normal"},
		{rpc.ContractParams{SecType: "STK", Exchange: "SEHK", PrimaryExch: "SEHK", Currency: "HKD"}, ""},
		{rpc.ContractParams{SecType: "STK", Exchange: "SGX", PrimaryExch: "SGX", Currency: "SGD"}, ""},
	} {
		if got := queuedOwnerAdaptivePriority(tc.contract); got != tc.want {
			t.Errorf("%+v: priority = %q, want %q", tc.contract, got, tc.want)
		}
	}
}

// listingPreviewServer previews a Hong Kong share whose route the resolver moves
// to SEHK, as ResolveOrderContractForSession does for a listing off SMART.
func listingPreviewServer(t *testing.T, whatIf func(rpc.OrderDraft)) *Server {
	t.Helper()
	now := time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC) // 10:00 in Hong Kong
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: new(1e6)})
	srv.now = func() time.Time { return now }
	srv.orderContractResolverForTest = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.ContractParams, error) {
		c.ConID, c.MinTick, c.Exchange, c.PrimaryExch = 4243, 0.2, "SEHK", "SEHK"
		return c, nil
	}
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		if c.Exchange != "SEHK" {
			t.Errorf("quoted on %q, want the listing's exchange", c.Exchange)
		}
		bid, ask := 419.8, 420.0
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: now, AsOf: now}, nil
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(100, 0, rpc.OrderPositionEffectReduce)
	srv.orderFXRateForTest = func(context.Context, string, string, time.Duration) (float64, time.Time, error) {
		return 0.11, now, nil
	}
	srv.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		whatIf(d)
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	return srv
}

// A reduction of a holding starts on SMART with the listing as its primary. The
// broker's check and the token both see the route the order will take.
func TestPreviewOfAHongKongShareGoesToTheBrokerOnItsOwnExchange(t *testing.T) {
	var sent []rpc.OrderDraft
	srv := listingPreviewServer(t, func(d rpc.OrderDraft) { sent = append(sent, d) })
	limit := 420.0
	preview, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: rpc.OrderActionSell, Quantity: 100, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, LimitPrice: &limit,
		Contract: rpc.ContractParams{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", PrimaryExch: "SEHK", Currency: "HKD"}})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(sent) != 1 || sent[0].Contract.Exchange != "SEHK" || sent[0].Contract.Currency != "HKD" {
		t.Fatalf("broker check saw %+v", sent)
	}
	if preview.Draft.Contract.Exchange != "SEHK" {
		t.Fatalf("draft route = %q", preview.Draft.Contract.Exchange)
	}
}

// An owner who asks for Adaptive on such a listing is told why, before the
// broker layer refuses a shape it cannot send, and nothing reaches the broker.
func TestPreviewRefusesAdaptiveOnAListingsOwnExchangeInPlainWords(t *testing.T) {
	srv := listingPreviewServer(t, func(rpc.OrderDraft) { t.Error("the broker check ran") })
	limit := 420.0
	_, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: rpc.OrderActionSell, Quantity: 100, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, LimitPrice: &limit, AdaptivePriority: "Normal",
		Contract: rpc.ContractParams{ConID: 4243, Symbol: "SYNH", SecType: "STK", Exchange: "SMART", PrimaryExch: "SEHK", Currency: "HKD"}})
	if err == nil || !strings.Contains(err.Error(), "only on SMART") || !strings.Contains(err.Error(), "SEHK") {
		t.Fatalf("err = %v", err)
	}
}
