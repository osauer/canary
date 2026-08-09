package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"

	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4001), ClientID: new(15)},
	}
	s := &Server{
		cfg:            cfg,
		endpoint:       discover.Endpoint{Host: "127.0.0.1", Port: 4001, ClientID: 15, PortOrigin: discover.OriginPinned},
		version:        "test",
		streams:        map[string]context.CancelFunc{},
		logger:         NewLogger(&bytes.Buffer{}, "error"),
		expiryIVs:      newExpiryIVCache(),
		quoteLiquidity: newQuoteLiquidityCache(),
		prevCloses:     newPrevCloseCache(),
		zeroGamma:      newGammaZeroCache(),
	}
	s.installSubs()
	return s
}

func TestReadHandlersReturnGatewayUnavailableWhenDisconnected(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	ctx := context.Background()

	t.Run("account.summary", func(t *testing.T) {
		_, err := srv.handleAccountSummary(ctx)
		assertGatewayUnavailable(t, err)
	})

	t.Run("positions.list", func(t *testing.T) {
		req := &rpc.Request{ID: "t1", Method: rpc.MethodPositionsList, Params: json.RawMessage(`{}`)}
		_, err := srv.handlePositionsList(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("quote.snapshot", func(t *testing.T) {
		params, _ := json.Marshal(rpc.QuoteSnapshotParams{
			Contract:  rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
			TimeoutMs: 100,
		})
		req := &rpc.Request{ID: "t2", Method: rpc.MethodQuoteSnapshot, Params: params}
		_, err := srv.handleQuoteSnapshot(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("quote.snapshot/fx-pair", func(t *testing.T) {
		params, _ := json.Marshal(rpc.QuoteSnapshotParams{
			Contract:  rpc.ContractParams{Symbol: "USD.JPY"},
			TimeoutMs: 100,
		})
		req := &rpc.Request{ID: "t2fx", Method: rpc.MethodQuoteSnapshot, Params: params}
		_, err := srv.handleQuoteSnapshot(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("chain.fetch", func(t *testing.T) {
		params, _ := json.Marshal(rpc.ChainFetchParams{
			Symbol: "AAPL", Expiry: "2026-06-19", Width: 1, Side: "both",
		})
		req := &rpc.Request{ID: "t3", Method: rpc.MethodChainFetch, Params: params}
		_, err := srv.handleChainFetch(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("history.daily", func(t *testing.T) {
		params, _ := json.Marshal(rpc.HistoryDailyParams{Symbol: "AAPL", Days: 30})
		req := &rpc.Request{ID: "t6", Method: rpc.MethodHistoryDaily, Params: params}
		_, err := srv.handleHistoryDaily(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("technical.snapshot", func(t *testing.T) {
		params, _ := json.Marshal(rpc.TechnicalParams{Symbols: []string{"AAPL"}})
		req := &rpc.Request{ID: "t6t", Method: rpc.MethodTechnical, Params: params}
		_, err := srv.handleTechnical(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("breadth.spx", func(t *testing.T) {
		req := &rpc.Request{ID: "t6b", Method: rpc.MethodBreadthSPX, Params: json.RawMessage(`{}`)}
		_, err := srv.handleBreadthSPX(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("gamma.zero_spx", func(t *testing.T) {
		req := &rpc.Request{ID: "t6c", Method: rpc.MethodGammaZeroSPX, Params: json.RawMessage(`{}`)}
		_, err := srv.handleGammaZeroSPX(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("regime.snapshot", func(t *testing.T) {
		req := &rpc.Request{ID: "t6d", Method: rpc.MethodRegimeSnapshot, Params: json.RawMessage(`{}`)}
		_, err := srv.handleRegimeSnapshot(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("chain.expiries", func(t *testing.T) {
		params, _ := json.Marshal(rpc.ChainExpiriesParams{Symbol: "AAPL"})
		req := &rpc.Request{ID: "t7", Method: rpc.MethodChainExpiries, Params: params}
		_, err := srv.handleChainExpiries(ctx, req)
		assertGatewayUnavailable(t, err)
	})

	t.Run("quote.subscribe", func(t *testing.T) {
		params, _ := json.Marshal(rpc.QuoteSubscribeParams{
			Contract: rpc.ContractParams{Symbol: "AAPL", SecType: "STK"},
		})
		req := &rpc.Request{ID: "t8", Method: rpc.MethodQuoteSubscribe, Params: params}
		var buf bytes.Buffer
		srv.handleQuoteSubscribe(ctx, req, json.NewEncoder(&buf), bufio.NewReader(bytes.NewReader(nil)))
		var resp rpc.Response
		if err := json.Unmarshal(buf.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if resp.Ok {
			t.Fatalf("expected !ok envelope, got %+v", resp)
		}
		if resp.Error == nil || resp.Error.Code != rpc.CodeGatewayUnavailable {
			t.Fatalf("got error %+v, want code %s", resp.Error, rpc.CodeGatewayUnavailable)
		}
	})
}

func TestHandleQuoteSubscribeHonorsRoutedContract(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	fake := newFakeConnector()
	srv.subs = newSubManager(func() ibkrMarketConnector { return fake })
	srv.subs.coalesce = 5 * time.Millisecond

	params, _ := json.Marshal(rpc.QuoteSubscribeParams{
		Contract: rpc.ContractParams{
			Symbol:      "VIX",
			SecType:     "IND",
			Exchange:    "CBOE",
			PrimaryExch: "CBOE",
			Currency:    "USD",
		},
	})
	req := &rpc.Request{ID: "route-quote", Method: rpc.MethodQuoteSubscribe, Params: params}
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleQuoteSubscribe(context.Background(), req, json.NewEncoder(&buf), bufio.NewReader(bytes.NewReader(nil)))
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleQuoteSubscribe did not return after client disconnect")
	}

	key := ibkrlib.MarketDataKeyForContract(ibkrlib.Contract{
		Symbol:      "VIX",
		SecType:     "IND",
		Exchange:    "CBOE",
		PrimaryExch: "CBOE",
		Currency:    "USD",
	})
	if got := fake.subCount("VIX"); got != 0 {
		t.Fatalf("quote.subscribe used bare symbol path %d times, want 0", got)
	}
	if got := fake.subCount(key); got != 1 {
		t.Fatalf("quote.subscribe routed subscribe count for %s = %d, want 1", key, got)
	}
}

func TestChainExpiriesEmptySymbolIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	params, _ := json.Marshal(rpc.ChainExpiriesParams{Symbol: " "})
	req := &rpc.Request{ID: "tx", Method: rpc.MethodChainExpiries, Params: params}
	_, err := srv.handleChainExpiries(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty symbol")
	}
	code, _ := classifyError(err)
	if code != rpc.CodeBadRequest {
		t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeBadRequest)
	}
}

func TestChainFetchInvalidParamsAreBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)

	tests := []struct {
		name   string
		params rpc.ChainFetchParams
	}{
		{
			name:   "invalid side",
			params: rpc.ChainFetchParams{Symbol: "AAPL", Expiry: "2026-06-19", Width: 1, Side: "sideways"},
		},
		{
			name:   "negative width",
			params: rpc.ChainFetchParams{Symbol: "AAPL", Expiry: "2026-06-19", Width: -1, Side: "both"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params, _ := json.Marshal(tc.params)
			req := &rpc.Request{ID: "tx", Method: rpc.MethodChainFetch, Params: params}
			_, err := srv.handleChainFetch(context.Background(), req)
			if err == nil {
				t.Fatal("expected error")
			}
			code, _ := classifyError(err)
			if code != rpc.CodeBadRequest {
				t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeBadRequest)
			}
		})
	}
}

func TestChainHistoricalSpotFallbackOnlyWhenMarketClosed(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	holiday := time.Date(2026, 5, 25, 12, 0, 0, 0, loc)
	if !chainCanUseHistoricalSpot(marketcal.MarketUSEquity, holiday) {
		t.Fatal("Memorial Day should allow historical spot fallback")
	}
	open := time.Date(2026, 5, 26, 10, 0, 0, 0, loc)
	if chainCanUseHistoricalSpot(marketcal.MarketUSEquity, open) {
		t.Fatal("open market should not allow historical spot fallback")
	}
	outsideCoverage := time.Date(2029, 1, 2, 10, 0, 0, 0, loc)
	if chainCanUseHistoricalSpot(marketcal.MarketUSEquity, outsideCoverage) {
		t.Fatal("outside calendar coverage should not allow historical spot fallback")
	}
}

func TestChainSummariesSurfaceTradabilityAndLiquidity(t *testing.T) {
	t.Parallel()
	cb, ca, civ := 2.00, 2.20, 0.55
	pb, pa := 1.50, 2.10
	oi := int64(1200)
	delta := 0.48
	prev := 0.90
	res := &rpc.ChainResult{
		Symbol: "ASTS", Spot: 55, Expiry: "2026-09-18", DataType: rpc.MarketDataLive, SessionState: rpc.SessionRTH.String(),
		Strikes: []rpc.ChainStrike{
			{
				Strike: 55, IsATM: true,
				CallBid: &cb, CallAsk: &ca, CallIV: &civ, CallOI: &oi, CallDelta: &delta, CallDataStatus: "quoted", CallOIStatus: "ok",
				PutBid: &pb, PutAsk: &pa, PutDataStatus: "quoted",
			},
			{Strike: 60, CallPrevClose: &prev, CallDataStatus: "prev_close", PutDataStatus: "model_only"},
			{Strike: 65, CallDataStatus: "subscribe_error", PutDataStatus: "no_quote"},
		},
	}

	tradable, liquidity := chainSummaries(res, true, true)
	if tradable == nil || liquidity == nil {
		t.Fatal("expected summaries")
	}
	if tradable.TotalLegs != 6 || tradable.LiveBidAskLegs != 2 || !tradable.OptionsTradable {
		t.Fatalf("tradable summary = %+v, want 6 total / 2 live / tradable", tradable)
	}
	if tradable.StaleLegs != 1 || tradable.ModelOnlyLegs != 1 || tradable.SubscribeErrorLegs != 1 || tradable.NoQuoteLegs != 1 {
		t.Fatalf("status counts = %+v, want one stale/model/subscribe/no_quote", tradable)
	}
	if math.Abs(tradable.OICoveragePct-(1.0/6.0)) > 1e-9 {
		t.Fatalf("oi coverage = %v, want 1/6", tradable.OICoveragePct)
	}
	if liquidity.LiquidityGrade != "good" || liquidity.RecommendedStructureHint != "calls_ok" {
		t.Fatalf("liquidity = %+v, want good/calls_ok", liquidity)
	}
	if liquidity.ATMSpreadPct == nil || math.Abs(*liquidity.ATMSpreadPct-0.0952380952380953) > 1e-9 {
		t.Fatalf("ATM spread pct = %v, want call spread around 9.5%%", liquidity.ATMSpreadPct)
	}
	if liquidity.NearestLiveCall == nil || liquidity.NearestLiveCall.Strike != 55 || liquidity.MinSpreadLiveStrike == nil {
		t.Fatalf("nearest/min live legs missing: %+v", liquidity)
	}
}

func TestChainSummariesTreatClosedSessionBidAskAsStaleContext(t *testing.T) {
	t.Parallel()
	cb, ca := 2.00, 2.20
	pb, pa := 1.50, 2.10
	res := &rpc.ChainResult{
		Symbol:       "ASTS",
		Spot:         55,
		Expiry:       "2026-09-18",
		DataType:     rpc.MarketDataClosed,
		SessionState: rpc.SessionClosed.String(),
		Strikes: []rpc.ChainStrike{{
			Strike: 55, IsATM: true,
			CallBid: &cb, CallAsk: &ca, CallDataStatus: "quoted",
			PutBid: &pb, PutAsk: &pa, PutDataStatus: "quoted",
		}},
	}

	tradable, liquidity := chainSummaries(res, true, true)
	if tradable.OptionsTradable || tradable.LiveBidAskLegs != 0 {
		t.Fatalf("closed-session bid/ask must not be executable: %+v", tradable)
	}
	if tradable.StaleLegs != 2 || tradable.FeedGap != "stale_close_only" {
		t.Fatalf("tradable summary = %+v, want 2 stale legs and stale_close_only", tradable)
	}
	if liquidity.LiquidityGrade != "untradable" || liquidity.RecommendedStructureHint != "untradable_chain" {
		t.Fatalf("liquidity = %+v, want untradable closed-session context", liquidity)
	}
	if liquidity.NearestLiveCall != nil || liquidity.NearestLivePut != nil || liquidity.MinSpreadLiveStrike != nil {
		t.Fatalf("closed-session quotes must not populate live leg summaries: %+v", liquidity)
	}
}

func TestApplyQuoteHistoricalFallbackPreservesLastPrice(t *testing.T) {
	t.Parallel()
	loc := mustLocation(t, "America/New_York")
	last := 456.25
	q := &rpc.Quote{
		Symbol: "IBM",
		Last:   &last,
		AsOf:   time.Date(2026, 5, 25, 15, 0, 0, 0, loc),
	}
	bars := []ibkrlib.HistoricalBar{
		{Date: "20260521", Time: time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC), Close: 449.00, High: 452.00, Low: 447.00, Volume: 2_000_000},
		{Date: "20260522", Time: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC), Close: 456.50, High: 459.25, Low: 454.75, Volume: 3_000_000},
	}

	applyQuoteHistoricalFallback(q, marketcal.MarketUSEquity, bars)
	(&Server{}).decorateQuote(q, marketcal.MarketUSEquity)

	if q.Price == nil || *q.Price != last {
		t.Fatalf("Price = %v, want last %.2f", q.Price, last)
	}
	if q.PriceSource != "last" {
		t.Fatalf("PriceSource = %q, want last", q.PriceSource)
	}
	if q.RegularClose == nil || *q.RegularClose != 456.50 {
		t.Fatalf("RegularClose = %v, want latest daily close", q.RegularClose)
	}
	if q.PrevClose == nil || *q.PrevClose != 456.50 {
		t.Fatalf("PrevClose = %v, want selected-price anchor", q.PrevClose)
	}
	if q.PriorRegularClose == nil || *q.PriorRegularClose != 449.00 {
		t.Fatalf("PriorRegularClose = %v, want prior daily close", q.PriorRegularClose)
	}
	if q.QuotePrice == nil || *q.QuotePrice != last {
		t.Fatalf("QuotePrice = %v, want last %.2f", q.QuotePrice, last)
	}
	if q.RegularChange == nil || *q.RegularChange != 7.50 {
		t.Fatalf("RegularChange = %v, want 7.50", q.RegularChange)
	}
	if q.QuoteChange == nil || math.Abs(*q.QuoteChange+0.25) > 0.0001 {
		t.Fatalf("QuoteChange = %v, want -0.25", q.QuoteChange)
	}
	if q.Week52Low == nil || q.Week52High == nil || *q.Week52Low != 447.00 || *q.Week52High != 459.25 {
		t.Fatalf("52w range = %v/%v, want historical low/high", q.Week52Low, q.Week52High)
	}
	if q.AvgVolume == nil || *q.AvgVolume != 2_500_000 {
		t.Fatalf("AvgVolume = %v, want historical average volume", q.AvgVolume)
	}
	if got, want := q.PriceAt.Format(time.RFC3339), "2026-05-25T15:00:00-04:00"; got != want {
		t.Fatalf("PriceAt = %q, want %q", got, want)
	}
	if got, want := q.RegularCloseAt.Format(time.RFC3339), "2026-05-22T16:00:00-04:00"; got != want {
		t.Fatalf("RegularCloseAt = %q, want %q", got, want)
	}
}

func TestGroupByUnderlyingExcludesSameSymbolNonEquitiesAndUnknown(t *testing.T) {
	t.Parallel()
	stocks := []rpc.PositionView{
		{Symbol: "T", SecType: rpc.SecTypeStock, ConID: 500000, Quantity: 10, MarketValue: 1000},
		{Symbol: "T", SecType: "BOND", ConID: 500001, LocalSymbol: "T 4 1/8 11/15/32", Quantity: 2, MarketValue: 2000},
		{Symbol: "T", SecType: "BOND", ConID: 500002, LocalSymbol: "T 4 5/8 02/15/35", Quantity: 3, MarketValue: 3000},
		{Symbol: "T", ConID: 500004, Quantity: 4, MarketValue: 4000},
	}
	options := []rpc.PositionView{
		{Symbol: "T", SecType: rpc.SecTypeOption, ConID: 500003, Quantity: 1, MarketValue: 500},
	}

	groups := groupByUnderlying(stocks, options, "USD", nil)
	if len(groups) != 1 {
		t.Fatalf("groups=%d, want one ticker-level group", len(groups))
	}
	group := groups[0]
	if group.Underlying != "T" || group.Stock == nil || group.Stock.ConID != 500000 {
		t.Fatalf("group stock=%+v underlying=%q, want exact equity stock", group.Stock, group.Underlying)
	}
	if len(group.Options) != 1 || group.Options[0].ConID != 500003 {
		t.Fatalf("group options=%+v, want exact option leg", group.Options)
	}
	if group.GroupMarketValue != 1500 {
		t.Fatalf("group market value=%.0f, want equity + option only", group.GroupMarketValue)
	}
}

func TestHistoryDailyEmptySymbolIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	params, _ := json.Marshal(rpc.HistoryDailyParams{Symbol: "  ", Days: 30})
	req := &rpc.Request{ID: "t7", Method: rpc.MethodHistoryDaily, Params: params}
	_, err := srv.handleHistoryDaily(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for empty symbol")
	}
	code, _ := classifyError(err)
	if code != rpc.CodeBadRequest {
		t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeBadRequest)
	}
}

func TestHistoryDailyUnsupportedWhatToShowIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	params, _ := json.Marshal(rpc.HistoryDailyParams{Symbol: "AAPL", Days: 30, WhatToShow: "BID"})
	req := &rpc.Request{ID: "t7b", Method: rpc.MethodHistoryDaily, Params: params}
	_, err := srv.handleHistoryDaily(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for unsupported what_to_show")
	}
	code, _ := classifyError(err)
	if code != rpc.CodeBadRequest {
		t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeBadRequest)
	}
}

func TestStatusHealthReportsDisconnected(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.lastConnectError = "test: handshake never completed"

	res := srv.handleStatusHealth()
	if res.Connected {
		t.Fatal("expected Connected=false when connector is nil")
	}
	if res.LastError != "test: handshake never completed" {
		t.Fatalf("LastError = %q, want test message", res.LastError)
	}
	if res.DataType != "" {
		t.Fatalf("DataType = %q, want empty when disconnected", res.DataType)
	}
	if res.GatewayPort != 4001 {
		t.Fatalf("GatewayPort = %d, want 4001", res.GatewayPort)
	}
	if res.PortOrigin != string(discover.OriginPinned) {
		t.Fatalf("PortOrigin = %q, want pinned", res.PortOrigin)
	}
}

func TestClassifyError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"bad request", errBadRequest("missing symbol"), rpc.CodeBadRequest},
		{"gateway unavailable", ibkrlib.ErrIBKRUnavailable, rpc.CodeGatewayUnavailable},
		{"symbol inactive", ibkrlib.ErrSymbolInactive, rpc.CodeSymbolInactive},
		{"deadline exceeded", context.DeadlineExceeded, rpc.CodeTimeout},
		{"contract details timeout (raw)", ibkrlib.ErrContractDetailsTimeout, rpc.CodeTimeout},
		{"chain contract timeout (wrapped)", wrapChainExpiriesErr("AAPL", ibkrlib.ErrContractDetailsTimeout), rpc.CodeTimeout},
		{"unclassified", errors.New("boom"), rpc.CodeInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, msg := classifyError(tc.err)
			if code != tc.want {
				t.Fatalf("code = %q, want %q", code, tc.want)
			}
			if !strings.Contains(msg, tc.err.Error()) {
				t.Fatalf("message %q does not contain underlying error %q", msg, tc.err.Error())
			}
		})
	}
}

func TestClassifyErrorPrefersRegimeUnavailableOverWrappedCause(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{ibkrlib.ErrIBKRUnavailable, context.DeadlineExceeded, errRegimeSnapshotRefreshIncomplete} {
		err := &regimeSnapshotCacheUnavailableError{cause: cause}
		code, message := classifyError(err)
		if code != rpc.CodeRegimeUnavailable {
			t.Fatalf("cause %v: code=%q, want %q", cause, code, rpc.CodeRegimeUnavailable)
		}
		if message != "regime snapshot last-good is unavailable" {
			t.Fatalf("cause %v: leaked/changed message %q", cause, message)
		}
	}
}

func assertGatewayUnavailable(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected gateway_unavailable error, got nil")
	}
	if !errors.Is(err, ibkrlib.ErrIBKRUnavailable) {
		t.Fatalf("expected ErrIBKRUnavailable, got %v", err)
	}
	code, _ := classifyError(err)
	if code != rpc.CodeGatewayUnavailable {
		t.Fatalf("classifyError code = %q, want %q", code, rpc.CodeGatewayUnavailable)
	}
}

func TestAttachQuoteSessionContextHoliday(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	asOf := time.Date(2026, 5, 25, 10, 0, 0, 0, mustLocation(t, "America/New_York"))
	q := &rpc.Quote{Symbol: "SPY", DataType: rpc.MarketDataFrozen, AsOf: asOf}

	srv.attachQuoteSessionContext(q, marketcal.MarketUSEquity)

	if q.SessionContext == nil {
		t.Fatal("expected session context")
	}
	if q.SessionContext.State != string(marketcal.StateHoliday) {
		t.Fatalf("State = %q, want %q", q.SessionContext.State, marketcal.StateHoliday)
	}
	if q.SessionContext.Reason != "Memorial Day" {
		t.Fatalf("Reason = %q, want Memorial Day", q.SessionContext.Reason)
	}
	if q.SessionContext.NextOpen == nil {
		t.Fatal("expected next_open")
	}
	if got, want := q.SessionContext.NextOpen.Format(time.RFC3339), "2026-05-26T09:30:00-04:00"; got != want {
		t.Fatalf("NextOpen = %q, want %q", got, want)
	}
}

func TestDecorateQuoteMarksOldLivePriceStale(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	loc := mustLocation(t, "America/New_York")
	last, prev := 652.10, 650.25
	asOf := time.Date(2026, 5, 26, 10, 30, 0, 0, loc)
	q := &rpc.Quote{
		Symbol:    "SPY",
		Last:      &last,
		PrevClose: &prev,
		DataType:  rpc.MarketDataLive,
		PriceAt:   asOf.Add(-20 * time.Minute),
		AsOf:      asOf,
	}

	srv.decorateQuote(q, marketcal.MarketUSEquity)

	if q.Price == nil || *q.Price != last {
		t.Fatalf("Price = %v, want last %.2f", q.Price, last)
	}
	if q.PriceSource != "last" {
		t.Fatalf("PriceSource = %q, want last", q.PriceSource)
	}
	if q.Change == nil || math.Abs(*q.Change-1.85) > 0.0001 {
		t.Fatalf("Change = %v, want 1.85", q.Change)
	}
	if !q.Stale {
		t.Fatal("expected stale quote during open market")
	}
	if !strings.Contains(q.StaleReason, "20m old") {
		t.Fatalf("StaleReason = %q, want age detail", q.StaleReason)
	}
	if got, want := q.PriceAsOf, "Frozen: May 26 at 10:10:00 AM EDT"; got != want {
		t.Fatalf("PriceAsOf = %q, want %q", got, want)
	}
	if q.DataType != rpc.MarketDataFrozen {
		t.Fatalf("DataType = %q, want frozen for stale selected price", q.DataType)
	}
}

func TestDecorateQuoteMarksOpenFrozenDataStale(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	loc := mustLocation(t, "Europe/Berlin")
	mark := 51.04
	q := &rpc.Quote{
		Symbol:   "MBG",
		Mark:     &mark,
		DataType: rpc.MarketDataFrozen,
		AsOf:     time.Date(2026, 5, 25, 13, 52, 0, 0, loc),
	}

	srv.decorateQuote(q, marketcal.MarketDEXetra)

	if q.Price == nil || *q.Price != mark {
		t.Fatalf("Price = %v, want mark %.2f", q.Price, mark)
	}
	if q.PriceSource != "mark" {
		t.Fatalf("PriceSource = %q, want mark", q.PriceSource)
	}
	if got, want := q.PriceAsOf, "Frozen: May 25 at 01:52:00 PM CEST"; got != want {
		t.Fatalf("PriceAsOf = %q, want %q", got, want)
	}
	if !q.Stale {
		t.Fatal("expected frozen data to be stale during an open market")
	}
	if q.StaleReason != "market is open but quote data is frozen" {
		t.Fatalf("StaleReason = %q", q.StaleReason)
	}
}

func TestDecorateCashFXQuoteDoesNotUseEquitySession(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	bid, ask, last := 159.455, 159.458, 159.46
	q := &rpc.Quote{
		Symbol: "USD.JPY",
		Contract: rpc.ContractParams{
			Symbol:   "USD",
			SecType:  "CASH",
			Exchange: "IDEALPRO",
			Currency: "JPY",
		},
		Bid:      &bid,
		Ask:      &ask,
		Last:     &last,
		DataType: rpc.MarketDataLive,
		AsOf:     time.Date(2026, 6, 1, 1, 30, 0, 0, mustLocation(t, "America/New_York")),
	}
	srv.decorateQuote(q, "")

	if q.SessionContext != nil {
		t.Fatalf("SessionContext = %+v, want nil for CASH FX", q.SessionContext)
	}
	if q.QuoteQuality != "firm" {
		t.Fatalf("QuoteQuality = %q, want firm", q.QuoteQuality)
	}
	if q.Indicative {
		t.Fatal("live CASH FX quote must not be marked indicative because U.S. equities are closed")
	}
	for _, w := range q.WarningDetails {
		if w.Code == "off_hours_quote" {
			t.Fatalf("WarningDetails = %+v, must not include equity off-hours warning for CASH FX", q.WarningDetails)
		}
	}
}

func TestHandleMarketCalendarWithoutGateway(t *testing.T) {
	t.Parallel()
	params, _ := json.Marshal(rpc.MarketCalendarParams{Market: "de", Date: "2026-05-25", Days: 2})
	req := &rpc.Request{ID: "calendar", Method: rpc.MethodMarketCalendar, Params: params}

	res, err := newTestServer(t).handleMarketCalendar(req)
	if err != nil {
		t.Fatalf("handleMarketCalendar: %v", err)
	}
	if res.Market != string(marketcal.MarketDEXetra) {
		t.Fatalf("Market = %q, want %q", res.Market, marketcal.MarketDEXetra)
	}
	if !res.Session.IsOpen || res.Session.State != string(marketcal.StateRegular) {
		t.Fatalf("Whit Monday 2026 should be an open Xetra session: %+v", res.Session)
	}
	if len(res.Sessions) != 2 {
		t.Fatalf("Sessions len = %d, want 2", len(res.Sessions))
	}
}

func TestHandleMarketCalendarBadMarketIsBadRequest(t *testing.T) {
	t.Parallel()
	params, _ := json.Marshal(rpc.MarketCalendarParams{Market: "mars"})
	req := &rpc.Request{ID: "calendar", Method: rpc.MethodMarketCalendar, Params: params}

	_, err := newTestServer(t).handleMarketCalendar(req)
	if err == nil {
		t.Fatal("expected bad_request for unsupported market")
	}
	code, _ := classifyError(err)
	if code != rpc.CodeBadRequest {
		t.Fatalf("code = %q, want %q (err=%v)", code, rpc.CodeBadRequest, err)
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}
