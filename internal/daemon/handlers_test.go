package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// newTestServer constructs a Server with no connector — a "daemon up but
// gateway not connected" simulation. gatewayReady() is the seam every
// read handler short-circuits on.
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

// When the gateway isn't connected, every read handler must return
// ErrIBKRUnavailable so dispatch maps to gateway_unavailable instead of
// silently returning empty results (D1, D2, D3 in the review).
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

	// FX-pair quote (USD.JPY) routes through the same handler as STK but
	// flows through pkg/ibkr.classifySymbol → CASH/IDEALPRO inside the
	// connector. With the gateway down, the handler must short-circuit
	// before it tries to subscribe, exactly like the STK path above.
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

// computeQuoteChange returns nil for either output unless both Last and
// PrevClose are present and PrevClose is strictly positive. No
// fabrication — pre-market with no Last must show em-dash, not zero.
func TestComputeQuoteChange(t *testing.T) {
	t.Parallel()
	f := func(v float64) *float64 { return &v }

	cases := []struct {
		name      string
		last      *float64
		prev      *float64
		wantChg   *float64
		wantPct   *float64
		precision float64
	}{
		{"both nil", nil, nil, nil, nil, 0},
		{"last nil", nil, f(100), nil, nil, 0},
		{"prev nil", f(105), nil, nil, nil, 0},
		{"prev zero", f(105), f(0), nil, nil, 0},
		{"prev negative", f(105), f(-1), nil, nil, 0},
		{"positive change", f(101.50), f(100), f(1.50), f(1.50), 0.0001},
		{"negative change", f(95), f(100), f(-5), f(-5), 0.0001},
		{"zero change", f(100), f(100), f(0), f(0), 0.0001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chg, pct := computeQuoteChange(tc.last, tc.prev)
			if (chg == nil) != (tc.wantChg == nil) {
				t.Fatalf("chg nil mismatch: got=%v want=%v", chg, tc.wantChg)
			}
			if (pct == nil) != (tc.wantPct == nil) {
				t.Fatalf("pct nil mismatch: got=%v want=%v", pct, tc.wantPct)
			}
			if chg != nil && math.Abs(*chg-*tc.wantChg) > tc.precision {
				t.Errorf("chg got %f want %f", *chg, *tc.wantChg)
			}
			if pct != nil && math.Abs(*pct-*tc.wantPct) > tc.precision {
				t.Errorf("pct got %f want %f", *pct, *tc.wantPct)
			}
		})
	}
}

func TestNormaliseOptionQuoteContract(t *testing.T) {
	t.Parallel()
	got, err := normaliseOptionQuoteContract(rpc.ContractParams{
		Symbol: "spy", Expiry: "20260619", Right: "c", Strike: 600,
	})
	if err != nil {
		t.Fatalf("normaliseOptionQuoteContract: %v", err)
	}
	if got.Symbol != "SPY" || got.SecType != "OPT" || got.Exchange != "SMART" || got.Currency != "USD" || got.Right != "C" {
		t.Fatalf("normalised contract = %+v", got)
	}
	for _, tc := range []rpc.ContractParams{
		{Symbol: "SPY", Expiry: "260619", Right: "C", Strike: 600},
		{Symbol: "SPY", Expiry: "20260619", Right: "X", Strike: 600},
		{Symbol: "SPY", Expiry: "20260619", Right: "C", Strike: 0},
	} {
		if _, err := normaliseOptionQuoteContract(tc); err == nil {
			t.Fatalf("normaliseOptionQuoteContract(%+v) returned nil error", tc)
		}
	}
}

// chain.expiries with an empty symbol must surface as bad_request, not
// internal — the CLI relies on this to render a usage hint.
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

func TestChainHistoricalSpotFromBarsUsesLatestPositiveClose(t *testing.T) {
	t.Parallel()
	bars := []ibkrlib.HistoricalBar{
		{Close: 100},
		{Close: 0},
		{Close: 103.25},
	}
	spot, dataType := chainHistoricalSpotFromBars(bars)
	if spot != 103.25 {
		t.Fatalf("spot = %v, want 103.25", spot)
	}
	if dataType != rpc.MarketDataFrozen {
		t.Fatalf("dataType = %q, want %q", dataType, rpc.MarketDataFrozen)
	}
}

func TestDaysUntilFromUsesNYCalendarDate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 25, 20, 0, 0, 0, time.UTC) // Monday afternoon in New York.
	if got := daysUntilFrom("20260529", now); got != 4 {
		t.Fatalf("daysUntilFrom Friday expiry = %d, want 4", got)
	}
}

// mergeStrikeSide is the publish step in handleChainFetch's parallel
// fan-out: each worker fills a private rpc.ChainStrike and merges its
// side under a shared mutex. Verifies the side-disjoint copy: a "C"
// merge must touch only Call* fields and leave Put* untouched, and
// vice versa — otherwise concurrent workers would clobber each
// other's results.
func TestMergeStrikeSide(t *testing.T) {
	t.Parallel()
	cb, ca, cl, civ := 1.0, 2.0, 3.0, 0.4
	pb, pa, pl, piv := 4.0, 5.0, 6.0, 0.5

	t.Run("call merge leaves put fields untouched", func(t *testing.T) {
		dst := rpc.ChainStrike{Strike: 100, IsATM: true,
			PutBid: &pb, PutAsk: &pa, PutLast: &pl, PutIV: &piv,
		}
		src := rpc.ChainStrike{
			CallBid: &cb, CallAsk: &ca, CallLast: &cl, CallIV: &civ,
		}
		mergeStrikeSide(&dst, &src, "C")
		if dst.CallBid == nil || *dst.CallBid != cb {
			t.Errorf("CallBid not copied")
		}
		if dst.PutBid == nil || *dst.PutBid != pb {
			t.Errorf("PutBid was clobbered: %+v", dst.PutBid)
		}
		if !dst.IsATM || dst.Strike != 100 {
			t.Errorf("metadata fields lost: strike=%v atm=%v", dst.Strike, dst.IsATM)
		}
	})

	t.Run("put merge leaves call fields untouched", func(t *testing.T) {
		dst := rpc.ChainStrike{Strike: 100,
			CallBid: &cb, CallAsk: &ca, CallLast: &cl, CallIV: &civ,
		}
		src := rpc.ChainStrike{
			PutBid: &pb, PutAsk: &pa, PutLast: &pl, PutIV: &piv,
		}
		mergeStrikeSide(&dst, &src, "P")
		if dst.PutBid == nil || *dst.PutBid != pb {
			t.Errorf("PutBid not copied")
		}
		if dst.CallBid == nil || *dst.CallBid != cb {
			t.Errorf("CallBid was clobbered")
		}
	})
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

func TestChainSummariesCountsExtendedSessionBidAskWhenFeedIsLive(t *testing.T) {
	t.Parallel()
	cb, ca := 2.00, 2.20
	pb, pa := 1.50, 2.10
	res := &rpc.ChainResult{
		Symbol:       "SPY",
		Spot:         55,
		Expiry:       "2026-06-01",
		DataType:     rpc.MarketDataClosed,
		FeedType:     rpc.MarketDataLive,
		SessionState: rpc.SessionPre.String(),
		Strikes: []rpc.ChainStrike{{
			Strike: 55, IsATM: true,
			CallBid: &cb, CallAsk: &ca, CallDataStatus: "quoted",
			PutBid: &pb, PutAsk: &pa, PutDataStatus: "quoted",
		}},
	}

	tradable, liquidity := chainSummaries(res, true, true)
	if !tradable.OptionsTradable || tradable.LiveBidAskLegs != 2 {
		t.Fatalf("extended-session bid/ask should count as live: %+v", tradable)
	}
	if tradable.FeedGap != "" || tradable.StaleLegs != 0 {
		t.Fatalf("extended-session summary should not report stale feed gap: %+v", tradable)
	}
	if liquidity.NearestLiveCall == nil || liquidity.NearestLivePut == nil || liquidity.MinSpreadLiveStrike == nil {
		t.Fatalf("extended-session live leg summaries missing: %+v", liquidity)
	}
}

func TestChainSummariesClassifyUntradableChain(t *testing.T) {
	t.Parallel()
	prev := 0.90
	res := &rpc.ChainResult{
		Symbol: "ONDS", Spot: 10, Expiry: "2026-09-18", DataType: rpc.MarketDataLive, SessionState: rpc.SessionRTH.String(),
		Strikes: []rpc.ChainStrike{
			{Strike: 10, IsATM: true, CallPrevClose: &prev, CallDataStatus: "prev_close"},
			{Strike: 11, CallDataStatus: "model_only"},
		},
	}
	tradable, liquidity := chainSummaries(res, true, false)
	if tradable.OptionsTradable {
		t.Fatalf("expected untradable summary, got %+v", tradable)
	}
	if tradable.FeedGap != "thin_contract" {
		t.Fatalf("feed gap = %q, want thin_contract", tradable.FeedGap)
	}
	if liquidity.LiquidityGrade != "untradable" || liquidity.RecommendedStructureHint != "untradable_chain" {
		t.Fatalf("liquidity = %+v, want untradable/untradable_chain", liquidity)
	}
}

func TestAnnotateRepeatedExpiryIVMarksQualityAndWarning(t *testing.T) {
	t.Parallel()
	iv := 0.42
	other := 0.47
	rows := []rpc.ChainExpiry{
		{Date: "2026-06-19", IV: &iv, IVQuality: "live_model"},
		{Date: "2026-09-18", IV: &iv, IVQuality: "live_model"},
		{Date: "2027-01-15", IV: &iv, IVQuality: "cached"},
		{Date: "2027-06-18", IV: &other, IVQuality: "live_model"},
	}
	warnings := annotateRepeatedExpiryIV("ASTS", rows)
	if len(warnings) != 1 || warnings[0].Code != "repeated_expiry_iv" {
		t.Fatalf("warnings = %+v, want repeated_expiry_iv", warnings)
	}
	for i := range 3 {
		if rows[i].IVQuality != "reused_fallback" {
			t.Fatalf("row %d quality = %q, want reused_fallback", i, rows[i].IVQuality)
		}
	}
	if rows[3].IVQuality != "live_model" {
		t.Fatalf("non-repeated row quality changed to %q", rows[3].IVQuality)
	}
}

// marketDataTypeName maps the gateway's per-reqID notice to the
// stable wire string. Locks the mapping so a future change to the
// IBKR enum surfaces here, not in the CLI's badge-rendering switch.
func TestMarketDataTypeName(t *testing.T) {
	t.Parallel()
	cases := map[int]string{
		0: "",
		1: "live",
		2: "frozen",
		3: "delayed",
		4: "delayed-frozen",
		5: "",
	}
	for in, want := range cases {
		if got := marketDataTypeName(in); got != want {
			t.Errorf("marketDataTypeName(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteDataTypeNameFallbacks(t *testing.T) {
	t.Parallel()

	if got := quoteDataTypeName(2, true, false); got != rpc.MarketDataFrozen {
		t.Fatalf("explicit notice wins: got %q, want frozen", got)
	}
	if got := quoteDataTypeName(1, false, true); got != rpc.MarketDataFrozen {
		t.Fatalf("fallback-only price with live notice: got %q, want frozen", got)
	}
	if got := quoteDataTypeName(3, false, true); got != rpc.MarketDataDelayed {
		t.Fatalf("fallback-only price with delayed notice: got %q, want delayed", got)
	}
	if got := quoteDataTypeName(0, true, false); got != rpc.MarketDataLive {
		t.Fatalf("current price without notice: got %q, want live", got)
	}
	if got := quoteDataTypeName(0, false, true); got != rpc.MarketDataFrozen {
		t.Fatalf("fallback-only price without notice: got %q, want frozen", got)
	}
	if got := quoteDataTypeName(0, false, false); got != "" {
		t.Fatalf("no price without notice: got %q, want empty", got)
	}
}

func TestQuoteFallbackReadyWaitsBrieflyForLastTick(t *testing.T) {
	t.Parallel()
	mark := 456.50
	q := &rpc.Quote{Mark: &mark}

	if quoteFallbackReady(q, time.Now(), 5*time.Second) {
		t.Fatal("mark-only fallback should wait briefly so a last tick can arrive")
	}
	if !quoteFallbackReady(q, time.Now().Add(-time.Second), 5*time.Second) {
		t.Fatal("mark-only fallback should become usable after the grace window")
	}
}

func TestApplyQuoteHistoricalFallback(t *testing.T) {
	t.Parallel()
	loc := mustLocation(t, "America/New_York")
	q := &rpc.Quote{
		Symbol: "IBM",
		AsOf:   time.Date(2026, 5, 25, 15, 0, 0, 0, loc),
	}
	bars := []ibkrlib.HistoricalBar{
		{Date: "20260521", Time: time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC), Close: 449.00, High: 452.00, Low: 447.00, Volume: 2_000_000},
		{Date: "20260522", Time: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC), Close: 456.50, High: 459.25, Low: 454.75, Volume: 3_000_000},
	}

	applyQuoteHistoricalFallback(q, marketcal.MarketUSEquity, bars)
	(&Server{}).decorateQuote(q, marketcal.MarketUSEquity)

	if q.Price == nil || *q.Price != 456.50 {
		t.Fatalf("Price = %v, want latest daily close", q.Price)
	}
	if q.PriceSource != "historical_close" {
		t.Fatalf("PriceSource = %q, want historical_close", q.PriceSource)
	}
	if q.PrevClose == nil || *q.PrevClose != 449.00 {
		t.Fatalf("PrevClose = %v, want prior daily close", q.PrevClose)
	}
	if q.Change == nil || *q.Change != 7.50 {
		t.Fatalf("Change = %v, want 7.50", q.Change)
	}
	if got, want := q.PriceAt.Format(time.RFC3339), "2026-05-22T16:00:00-04:00"; got != want {
		t.Fatalf("PriceAt = %q, want %q", got, want)
	}
	if got, want := q.PriceAsOf, "At close: May 22 at 04:00:00 PM EDT"; got != want {
		t.Fatalf("PriceAsOf = %q, want %q", got, want)
	}
}

func TestQuoteNeedsHistoricalFallbackWithPrevCloseOnly(t *testing.T) {
	t.Parallel()
	prev := 456.50
	if !quoteNeedsHistoricalFallback(&rpc.Quote{PrevClose: &prev}) {
		t.Fatal("prev-close-only quote should still try historical fallback for latest close/range/volume context")
	}
	mark := 456.50
	if quoteNeedsHistoricalFallback(&rpc.Quote{Mark: &mark}) {
		t.Fatal("mark price should be enough quote context without historical fallback")
	}
}

func TestQuoteNeedsClosedMarketHistoricalContext(t *testing.T) {
	t.Parallel()
	loc := mustLocation(t, "America/New_York")
	mark, prev := 456.50, 456.50
	holiday := &rpc.Quote{
		Symbol:    "IBM",
		Mark:      &mark,
		PrevClose: &prev,
		AsOf:      time.Date(2026, 5, 25, 15, 0, 0, 0, loc), // Memorial Day.
	}
	if !quoteNeedsClosedMarketHistoricalContext(holiday, marketcal.MarketUSEquity) {
		t.Fatal("closed-market mark/prev-close-only quote should fetch historical context")
	}

	last := 456.50
	closedWithLast := &rpc.Quote{
		Symbol:    "IBM",
		Last:      &last,
		PrevClose: &prev,
		AsOf:      time.Date(2026, 5, 25, 15, 0, 0, 0, loc),
	}
	if quoteNeedsClosedMarketHistoricalContext(closedWithLast, marketcal.MarketUSEquity) {
		t.Fatal("closed-market quote with a last price should not need historical replacement")
	}

	open := &rpc.Quote{
		Symbol:    "IBM",
		Mark:      &mark,
		PrevClose: &prev,
		AsOf:      time.Date(2026, 5, 26, 10, 0, 0, 0, loc),
	}
	if quoteNeedsClosedMarketHistoricalContext(open, marketcal.MarketUSEquity) {
		t.Fatal("open-market quote should not fetch historical close context")
	}
}

func TestQuoteNeedsHistoricalContextFillsClosedMarketMetadata(t *testing.T) {
	t.Parallel()
	loc := mustLocation(t, "America/New_York")
	last, prev, dayLow, dayHigh, weekLow, weekHigh, avgVol := 456.50, 449.00, 454.75, 459.25, 400.00, 500.00, int64(3_000_000)
	closedMissingRanges := &rpc.Quote{
		Symbol:    "IBM",
		Last:      &last,
		PrevClose: &prev,
		AsOf:      time.Date(2026, 5, 25, 15, 0, 0, 0, loc),
	}
	if !quoteNeedsHistoricalContext(closedMissingRanges, marketcal.MarketUSEquity) {
		t.Fatal("closed-market quote should fetch historical context when range/volume metadata is missing")
	}

	complete := &rpc.Quote{
		Symbol:       "IBM",
		Last:         &last,
		RegularClose: &prev,
		PrevClose:    &prev,
		DayLow:       &dayLow,
		DayHigh:      &dayHigh,
		Week52Low:    &weekLow,
		Week52High:   &weekHigh,
		AvgVolume:    &avgVol,
		AsOf:         time.Date(2026, 5, 25, 15, 0, 0, 0, loc),
	}
	if quoteNeedsHistoricalContext(complete, marketcal.MarketUSEquity) {
		t.Fatal("complete closed-market quote should not fetch historical context")
	}
}

func TestQuoteHistoricalFallbackTimeoutCapsAtQuoteBudget(t *testing.T) {
	t.Parallel()
	if got := quoteHistoricalFallbackTimeout(2 * time.Second); got != 2*time.Second {
		t.Fatalf("short quote timeout = %s, want 2s", got)
	}
	if got := quoteHistoricalFallbackTimeout(30 * time.Second); got != 5*time.Second {
		t.Fatalf("long quote timeout cap = %s, want 5s", got)
	}
}

func TestQuoteHistoricalContractUsesQuoteRoute(t *testing.T) {
	t.Parallel()
	got := quoteHistoricalContract(&rpc.Quote{
		Symbol: "MBG",
		Contract: rpc.ContractParams{
			Symbol:  "MBG",
			SecType: "STK",
			Market:  "de",
		},
	})
	if got.Symbol != "MBG" || got.SecType != "STK" || got.Exchange != "SMART" || got.PrimaryExch != "IBIS" || got.Currency != "EUR" {
		t.Fatalf("quoteHistoricalContract = %+v, want SMART/IBIS/EUR route", got)
	}
}

func TestQuoteLiquidityCacheKeyUsesQuoteSymbolFallback(t *testing.T) {
	t.Parallel()
	rgnt := quoteLiquidityCacheKey(&rpc.Quote{Symbol: "RGNT"})
	cupr := quoteLiquidityCacheKey(&rpc.Quote{Symbol: "CUPR"})
	if rgnt.symbol != "RGNT" || cupr.symbol != "CUPR" {
		t.Fatalf("cache keys = %+v / %+v, want quote symbols", rgnt, cupr)
	}
	if rgnt == cupr {
		t.Fatalf("cache keys collided for symbol-only quote shells: %+v", rgnt)
	}

	contractKey := quoteLiquidityCacheKey(&rpc.Quote{
		Symbol: "ignored",
		Contract: rpc.ContractParams{
			Symbol: "IBM",
		},
	})
	if contractKey.symbol != "IBM" {
		t.Fatalf("contract-backed cache key = %+v, want contract symbol", contractKey)
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

// closestStrike picks the strike closest to spot. Verifies the tie-break
// rule (lower wins) and the boundary cases at both ends of the array.
func TestClosestStrike(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		strikes []float64
		spot    float64
		want    float64
	}{
		{"exact match", []float64{200, 210, 220}, 210, 210},
		{"middle picks closer side", []float64{200, 210, 220}, 213, 210},
		{"tie picks lower", []float64{200, 210}, 205, 200},
		{"below range", []float64{200, 210, 220}, 100, 200},
		{"above range", []float64{200, 210, 220}, 500, 220},
		{"single strike", []float64{215}, 100, 215},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := closestStrike(tc.strikes, tc.spot); got != tc.want {
				t.Fatalf("closestStrike(%v,%v) = %v, want %v", tc.strikes, tc.spot, got, tc.want)
			}
		})
	}
}

// groupByUnderlying nests stock + option legs per underlying and sums
// market value / unrealized P&L. A pure-options group has Stock=nil; a
// stock-only group has empty Options.
func TestGroupByUnderlying(t *testing.T) {
	t.Parallel()
	stocks := []rpc.PositionView{
		{Symbol: "AAPL", SecType: rpc.SecTypeStock, Quantity: 100, MarketValue: 20000, UnrealizedPnL: 1500},
		{Symbol: "MSFT", SecType: rpc.SecTypeStock, Quantity: 50, MarketValue: 25000, UnrealizedPnL: -200},
	}
	options := []rpc.PositionView{
		{Symbol: "AAPL", SecType: rpc.SecTypeOption, Right: "C", Strike: 215, Expiry: "20260619", Quantity: 5, MarketValue: 4700, UnrealizedPnL: 1290},
		{Symbol: "TSLA", SecType: rpc.SecTypeOption, Right: "P", Strike: 200, Expiry: "20260516", Quantity: 2, MarketValue: 800, UnrealizedPnL: -90},
	}
	groups := groupByUnderlying(stocks, options, "", nil)
	if len(groups) != 3 {
		t.Fatalf("expected 3 groups, got %d", len(groups))
	}
	byName := map[string]rpc.PositionGroup{}
	for _, g := range groups {
		byName[g.Underlying] = g
	}
	aapl := byName["AAPL"]
	if aapl.Stock == nil || aapl.Stock.Quantity != 100 {
		t.Errorf("AAPL stock leg missing or wrong qty: %+v", aapl.Stock)
	}
	if len(aapl.Options) != 1 {
		t.Errorf("AAPL options: want 1, got %d", len(aapl.Options))
	}
	if aapl.GroupMarketValue != 24700 {
		t.Errorf("AAPL group MV: want 24700, got %g", aapl.GroupMarketValue)
	}
	if aapl.GroupUnrealizedPnL != 2790 {
		t.Errorf("AAPL group PnL: want 2790, got %g", aapl.GroupUnrealizedPnL)
	}
	tsla := byName["TSLA"]
	if tsla.Stock != nil {
		t.Errorf("TSLA expected pure-options group, got stock leg %+v", tsla.Stock)
	}
	if len(tsla.Options) != 1 {
		t.Errorf("TSLA options: want 1, got %d", len(tsla.Options))
	}
	msft := byName["MSFT"]
	if msft.Stock == nil || len(msft.Options) != 0 {
		t.Errorf("MSFT expected stock-only group, got %+v", msft)
	}
}

// Exact non-equity contract identity belongs to the flat positions authority,
// not the ticker-level stock + options projection. IBKR can report several
// bonds or futures under one base symbol; neither they nor an untyped row may
// overwrite the actual equity stock leg or leak into the group's totals.
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

// history.daily with an empty symbol must surface as bad_request, not
// internal — the CLI relies on this to render a usage hint.
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

func TestNormaliseStockQuoteContractMarketDE(t *testing.T) {
	t.Parallel()

	contract, echoed, routed, err := normaliseStockQuoteContract(rpc.ContractParams{
		Symbol: "mbg",
		Market: "de",
	})
	if err != nil {
		t.Fatalf("normaliseStockQuoteContract: %v", err)
	}
	if !routed {
		t.Fatal("market=de should use routed quote path")
	}
	if contract.Symbol != "MBG" || contract.SecType != "STK" || contract.Exchange != "SMART" || contract.PrimaryExch != "IBIS" || contract.Currency != "EUR" {
		t.Fatalf("contract = %+v, want MBG STK SMART/IBIS EUR", contract)
	}
	if echoed.Symbol != "MBG" || echoed.Market != "de" || echoed.Exchange != "SMART" || echoed.PrimaryExch != "IBIS" || echoed.Currency != "EUR" {
		t.Fatalf("echoed = %+v, want normalized DE route", echoed)
	}
}

// Greeks zero-substitution regression: a genuinely-zero Greek from the
// model (deep-ITM theta ≈ 0, ATM-straddle delta ≈ 0) must surface as a
// non-nil pointer. The previous per-field `!= 0` filter silently dropped
// real zeros and made consumers branching on `nil-as-unavailable` lie.
// Wire contract is documented at rpc.PositionView.Delta etc. ("never
// zero-substituted").
func TestFillOptionGreeksPreservesGenuineZero(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.greeks = newGreeksCache()

	opt := rpc.PositionView{
		Symbol:  "AAPL",
		SecType: rpc.SecTypeOption,
		Expiry:  "20260619",
		Strike:  195,
		Right:   "C",
	}
	key := optionGreeksKey(opt)
	if key == "" {
		t.Fatalf("optionGreeksKey returned empty for %+v", opt)
	}
	// Deep-ITM near expiry: delta ≈ 1.0, gamma ≈ 0, theta ≈ 0, vega > 0.
	srv.greeks.put(key, greeksEntry{
		value: ibkrlib.Greeks{Delta: 1.0, Gamma: 0, Theta: 0, Vega: 0.5},
		ok:    true,
	}, time.Now())

	options := []rpc.PositionView{opt}
	srv.fillOptionGreeks(nil, options)
	p := options[0]

	if p.Delta == nil || *p.Delta != 1.0 {
		t.Errorf("Delta = %v, want 1.0", ptrStr(p.Delta))
	}
	if p.Gamma == nil || *p.Gamma != 0 {
		t.Errorf("Gamma = %v, want non-nil 0", ptrStr(p.Gamma))
	}
	if p.Theta == nil || *p.Theta != 0 {
		t.Errorf("Theta = %v, want non-nil 0", ptrStr(p.Theta))
	}
	if p.Vega == nil || *p.Vega != 0.5 {
		t.Errorf("Vega = %v, want 0.5", ptrStr(p.Vega))
	}
}

func ptrStr(p *float64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64)
}

// status.health is the only read endpoint that must succeed when the
// gateway is down — that is its entire purpose.
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

// Malformed params on any unary handler must classify as bad_request, not
// internal. Before the fix every handler returned fmt.Errorf("decode params:")
// which fell through to CodeInternal — the CLI couldn't distinguish a
// client-side mistake from a daemon bug.
func TestDecodeParamsMalformedIsBadRequest(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	malformed := json.RawMessage(`{"symbol":`)

	cases := []struct {
		name string
		call func() error
	}{
		{"positions.list", func() error {
			_, err := srv.handlePositionsList(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"chain.expiries", func() error {
			_, err := srv.handleChainExpiries(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"chain.fetch", func() error {
			_, err := srv.handleChainFetch(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"history.daily", func() error {
			_, err := srv.handleHistoryDaily(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"technical.snapshot", func() error {
			_, err := srv.handleTechnical(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"quote.snapshot", func() error {
			_, err := srv.handleQuoteSnapshot(context.Background(), &rpc.Request{ID: "t", Params: malformed})
			return err
		}},
		{"cancel", func() error {
			_, err := srv.handleCancel(&rpc.Request{ID: "t", Params: malformed})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("expected error for malformed params")
			}
			code, _ := classifyError(err)
			if code != rpc.CodeBadRequest {
				t.Fatalf("%s: code = %q, want %q (err=%v)", tc.name, code, rpc.CodeBadRequest, err)
			}
		})
	}
}

// classifyError is the seam between handler errors and CLI-visible codes.
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

// TestClassifyBreadthState pins the breadth-handler state-classification
// contract end-to-end. Three of the four wire states are produced from
// the (snapshot-exists, refreshing) pair; "degraded" stays reserved and
// unproduced by design — a snapshot whose session is no longer current is
// reported on BreadthSPXResult.Stale, because State gates whether regime,
// the composite and the brief consume breadth at all (see the helper's
// doc comment), and a stale close is still evidence there.
//
// The classification was a v0.27.3 fix: prior versions side-channelled
// "refreshing" via fetchRegimeBreadth, which was prone to drift between
// the breadth handler and the regime fetcher. A warm refresh must keep
// the last good snapshot rankable; Refreshing is the side-band progress
// bit. This test pins the single source of truth so any future surface
// added to the daemon must call the same helper.
func TestClassifyBreadthState(t *testing.T) {
	cases := []struct {
		name       string
		snap       bool
		refreshing bool
		want       rpc.BreadthState
	}{
		{"snapshot exists, no refresh in flight -> ready", true, false, rpc.BreadthStateReady},
		{"snapshot exists, refresh in flight     -> ready", true, true, rpc.BreadthStateReady},
		{"no snapshot, refresh in flight         -> computing", false, true, rpc.BreadthStateComputing},
		{"no snapshot, no refresh                -> cold", false, false, rpc.BreadthStateCold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyBreadthState(tc.snap, tc.refreshing)
			if got != tc.want {
				t.Errorf("classifyBreadthState(snap=%v, refreshing=%v) = %q, want %q",
					tc.snap, tc.refreshing, got, tc.want)
			}
		})
	}
}

// TestStatusHealthReportsMembersEmbedded: status.health populates the
// Members surface from the engine. When the engine has no external
// cache file loaded (newTestServer doesn't install one), the row
// surfaces source=embedded and the embedded as_of date.
func TestStatusHealthReportsMembersEmbedded(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.installBreadthEngine()
	res := srv.handleStatusHealth()
	if res.Members.Source != "embedded" {
		t.Errorf("Members.Source: want embedded, got %q", res.Members.Source)
	}
	if res.Members.Count == 0 {
		t.Error("Members.Count should be > 0 for the embedded list")
	}
	if res.Members.AsOf.IsZero() {
		t.Error("Members.AsOf should reflect embedded sp500AsOf")
	}
}

// TestStatusHealthMembersEmptyWithoutEngine: a daemon whose breadth
// engine failed to install (e.g. unresolvable cache dir) returns an
// empty Members shape. The CLI renderer hides the row in that case.
func TestStatusHealthMembersEmptyWithoutEngine(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	// Don't install breadth.
	res := srv.handleStatusHealth()
	if res.Members.Source != "" {
		t.Errorf("Members.Source: want empty (no engine), got %q", res.Members.Source)
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

func TestAttachQuoteSessionContextOmittedDuringLiveRTH(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	loc := mustLocation(t, "America/New_York")
	bid, ask, last := 100.0, 100.1, 100.05
	q := &rpc.Quote{
		Symbol:   "SPY",
		Bid:      &bid,
		Ask:      &ask,
		Last:     &last,
		DataType: rpc.MarketDataLive,
		AsOf:     time.Date(2026, 5, 26, 10, 0, 0, 0, loc),
	}

	srv.attachQuoteSessionContext(q, marketcal.MarketUSEquity)

	if q.SessionContext != nil {
		t.Fatalf("SessionContext = %+v, want nil during ordinary live RTH", q.SessionContext)
	}
}

func TestDecorateQuotePrevCloseUsesPriorMarketClose(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	loc := mustLocation(t, "America/New_York")
	prev := 650.25
	q := &rpc.Quote{
		Symbol:    "SPY",
		PrevClose: &prev,
		DataType:  rpc.MarketDataFrozen,
		AsOf:      time.Date(2026, 5, 25, 10, 0, 0, 0, loc), // Memorial Day.
	}

	srv.attachQuoteSessionContext(q, marketcal.MarketUSEquity)
	srv.decorateQuote(q, marketcal.MarketUSEquity)

	if q.Price == nil || *q.Price != prev {
		t.Fatalf("Price = %v, want prev close %.2f", q.Price, prev)
	}
	if q.PriceSource != "prev_close" {
		t.Fatalf("PriceSource = %q, want prev_close", q.PriceSource)
	}
	if got, want := q.PriceAt.Format(time.RFC3339), "2026-05-22T16:00:00-04:00"; got != want {
		t.Fatalf("PriceAt = %q, want %q", got, want)
	}
	if got, want := q.PriceAsOf, "At close: May 22 at 04:00:00 PM EDT"; got != want {
		t.Fatalf("PriceAsOf = %q, want %q", got, want)
	}
	if q.Stale {
		t.Fatalf("holiday prev-close quote should not be stale during closed market: %s", q.StaleReason)
	}
}

func TestDecorateQuoteLastWithoutExchangeTimestampUsesObservationTimeWhenClosed(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	loc := mustLocation(t, "Europe/Berlin")
	last, prev := 50.81, 50.12
	q := &rpc.Quote{
		Symbol:    "MBG",
		Last:      &last,
		PrevClose: &prev,
		DataType:  rpc.MarketDataLive,
		AsOf:      time.Date(2026, 5, 25, 21, 15, 0, 0, loc),
	}

	srv.attachQuoteSessionContext(q, marketcal.MarketDEXetra)
	srv.decorateQuote(q, marketcal.MarketDEXetra)

	if q.Price == nil || *q.Price != last {
		t.Fatalf("Price = %v, want last %.2f", q.Price, last)
	}
	if q.PriceSource != "last" {
		t.Fatalf("PriceSource = %q, want last", q.PriceSource)
	}
	if q.Change == nil || math.Abs(*q.Change-0.69) > 0.0001 {
		t.Fatalf("Change = %v, want 0.69", q.Change)
	}
	if got, want := q.PriceAt.Format(time.RFC3339), "2026-05-25T21:15:00+02:00"; got != want {
		t.Fatalf("PriceAt = %q, want %q", got, want)
	}
	if got, want := q.PriceAsOf, "As of: May 25 at 09:15:00 PM CEST"; got != want {
		t.Fatalf("PriceAsOf = %q, want %q", got, want)
	}
	if q.DataType != rpc.MarketDataLive {
		t.Fatalf("DataType = %q, want live feed type preserved after close", q.DataType)
	}
	if q.QuoteQuality != "indicative" {
		t.Fatalf("QuoteQuality = %q, want indicative after close", q.QuoteQuality)
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

func TestDecorateQuotePriorSessionLastStaysIndicative(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	loc := mustLocation(t, "America/New_York")
	last, prev := 310.86, 308.82
	q := &rpc.Quote{
		Symbol:    "AAPL",
		Last:      &last,
		PrevClose: &prev,
		DataType:  rpc.MarketDataLive,
		PriceAt:   time.Date(2026, 5, 22, 16, 0, 0, 0, loc),
		AsOf:      time.Date(2026, 5, 26, 8, 15, 0, 0, loc),
	}

	srv.attachQuoteSessionContext(q, marketcal.MarketUSEquity)
	srv.decorateQuote(q, marketcal.MarketUSEquity)

	if q.DataType != rpc.MarketDataLive {
		t.Fatalf("DataType = %q, want live feed type preserved", q.DataType)
	}
	if q.FeedType != "" {
		t.Fatalf("FeedType = %q, want empty when effective type equals feed", q.FeedType)
	}
	if q.QuoteQuality != "indicative" {
		t.Fatalf("QuoteQuality = %q, want indicative", q.QuoteQuality)
	}
	if !q.Indicative {
		t.Fatal("expected prior-session quote to be indicative")
	}
	if len(q.WarningDetails) == 0 || q.WarningDetails[0].Code != "off_hours_quote" {
		t.Fatalf("WarningDetails = %+v, want off_hours_quote", q.WarningDetails)
	}
}

func TestDecorateQuoteRejectsCloseTimestampForMismatchedOvernightLast(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	loc := mustLocation(t, "America/New_York")
	last, regularClose, priorClose := 248.91, 250.69, 253.84
	closeAt := time.Date(2026, 5, 26, 16, 0, 0, 0, loc)
	asOf := time.Date(2026, 5, 27, 0, 55, 0, 0, loc)
	q := &rpc.Quote{
		Symbol:            "IBM",
		Contract:          rpc.ContractParams{Symbol: "IBM", SecType: "STK", Currency: "USD"},
		Last:              &last,
		RegularClose:      &regularClose,
		RegularCloseAt:    closeAt,
		PriorRegularClose: &priorClose,
		PrevClose:         &regularClose,
		DataType:          rpc.MarketDataLive,
		PriceAt:           closeAt,
		AsOf:              asOf,
	}

	srv.attachQuoteSessionContext(q, marketcal.MarketUSEquity)
	srv.decorateQuote(q, marketcal.MarketUSEquity)

	if q.Price == nil || *q.Price != last {
		t.Fatalf("Price = %v, want overnight last %.2f", q.Price, last)
	}
	if q.QuotePrice == nil || *q.QuotePrice != last {
		t.Fatalf("QuotePrice = %v, want overnight last %.2f", q.QuotePrice, last)
	}
	if q.PrevClose == nil || *q.PrevClose != regularClose {
		t.Fatalf("PrevClose = %v, want regular close anchor %.2f", q.PrevClose, regularClose)
	}
	if got, want := q.PriceAt.Format(time.RFC3339), "2026-05-27T00:55:00-04:00"; got != want {
		t.Fatalf("PriceAt = %q, want %q", got, want)
	}
	if got, want := q.QuotePriceAt.Format(time.RFC3339), "2026-05-27T00:55:00-04:00"; got != want {
		t.Fatalf("QuotePriceAt = %q, want %q", got, want)
	}
	if q.DataType != rpc.MarketDataLive {
		t.Fatalf("DataType = %q, want live", q.DataType)
	}
	if q.QuoteQuality != "indicative" {
		t.Fatalf("QuoteQuality = %q, want indicative", q.QuoteQuality)
	}
	if q.RegularChange == nil || math.Abs(*q.RegularChange+3.15) > 0.0001 {
		t.Fatalf("RegularChange = %v, want -3.15", q.RegularChange)
	}
	if q.QuoteChange == nil || math.Abs(*q.QuoteChange+1.78) > 0.0001 {
		t.Fatalf("QuoteChange = %v, want -1.78", q.QuoteChange)
	}
	for _, w := range q.WarningDetails {
		if w.Code == "selected_price_prev_close" {
			t.Fatalf("WarningDetails = %+v, must not classify overnight last as selected prev close", q.WarningDetails)
		}
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

func TestQuoteMarketForStockContract(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.ContractParams
		want marketcal.Market
	}{
		{"explicit de market", rpc.ContractParams{Market: "de"}, marketcal.MarketDEXetra},
		{"xetra exchange", rpc.ContractParams{Exchange: "IBIS"}, marketcal.MarketDEXetra},
		{"xetra primary exchange", rpc.ContractParams{PrimaryExch: "IBIS"}, marketcal.MarketDEXetra},
		{"default US", rpc.ContractParams{Symbol: "SPY"}, marketcal.MarketUSEquity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quoteMarketForStockContract(tc.in); got != tc.want {
				t.Fatalf("quoteMarketForStockContract(%+v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestQuoteSessionMarketForContractSelectsInstrumentCalendar(t *testing.T) {
	t.Parallel()
	if got, ok := quoteSessionMarketForContract(rpc.ContractParams{
		Symbol:   "USD",
		SecType:  "CASH",
		Exchange: "IDEALPRO",
		Currency: "JPY",
	}); ok || got != "" {
		t.Fatalf("quoteSessionMarketForContract(CASH) = %q, %t; want no regular-session calendar", got, ok)
	}
	got, ok := quoteSessionMarketForContract(rpc.ContractParams{Symbol: "SPY", SecType: "STK"})
	if !ok || got != marketcal.MarketUSEquity {
		t.Fatalf("quoteSessionMarketForContract(STK) = %q, %t; want %q, true", got, ok, marketcal.MarketUSEquity)
	}
	got, ok = quoteSessionMarketForContract(rpc.ContractParams{Symbol: "SPY", SecType: "OPT"})
	if !ok || got != marketcal.MarketUSOptions {
		t.Fatalf("quoteSessionMarketForContract(OPT) = %q, %t; want %q, true", got, ok, marketcal.MarketUSOptions)
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
