//go:build !trading

package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// TestStandardBuildPreviewsButRefusesBrokerWrites pins the boundary SECURITY.md
// states for the standard build (the v3.25.4 review's finding 6; owner
// decision 2026-10-20 13:30 CEST to keep preview): order.preview is served
// and mints its token, while every broker write refuses before anything is
// sent, with the build named as the blocker.
func TestStandardBuildPreviewsButRefusesBrokerWrites(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	bid, ask := 2.05, 2.15
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		return rpc.OrderQuoteSnapshot{Symbol: "AAA_20260619C52", Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}, nil
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	whatIfCalls := 0
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		whatIfCalls++
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted}, nil
	}
	brokerWrites := 0
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		brokerWrites++
		return nil
	}

	call := func(method string, params any) rpc.Response {
		t.Helper()
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		srv.dispatch(t.Context(), &rpc.Request{ID: "boundary", Method: method, Params: raw}, json.NewEncoder(&out), bufio.NewReader(strings.NewReader("")))
		var resp rpc.Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("%s response %q: %v", method, out.String(), err)
		}
		return resp
	}

	limit := 2.10
	resp := call(rpc.MethodOrderPreview, rpc.OrderPreviewParams{
		Action:   "buy",
		Contract: rpc.ContractParams{Symbol: "AAA", SecType: "OPT", Expiry: "20260619", Right: "C", Strike: 52, Multiplier: 100},
		Quantity: 1, LimitPrice: &limit,
	})
	if !resp.Ok {
		t.Fatalf("order.preview refused in the standard build: %+v", resp.Error)
	}
	var preview rpc.OrderPreviewResult
	if err := json.Unmarshal(resp.Result, &preview); err != nil {
		t.Fatal(err)
	}
	if !preview.TokenMinted || preview.PreviewToken == "" || whatIfCalls != 1 {
		t.Fatalf("preview token minted %v (token %q), what-if calls %d; want a minted token after one what-if", preview.TokenMinted, preview.PreviewToken, whatIfCalls)
	}

	for _, write := range []struct {
		method string
		params any
	}{
		{rpc.MethodOrderPlace, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken}},
		{rpc.MethodOrderModify, rpc.OrderModifyParams{ID: "synthetic-ref", PreviewToken: preview.PreviewToken}},
		{rpc.MethodOrderCancel, rpc.OrderCancelParams{ID: "synthetic-ref"}},
	} {
		resp := call(write.method, write.params)
		if resp.Ok || resp.Error == nil || resp.Error.Code != rpc.CodeTradingDisabled || !strings.Contains(resp.Error.Message, "unavailable in this standard build") {
			t.Fatalf("%s in the standard build = ok %v error %+v; want %s naming the build", write.method, resp.Ok, resp.Error, rpc.CodeTradingDisabled)
		}
	}

	resp = call(rpc.MethodOrderTWSCancel, rpc.OrderTWSControlParams{
		PermID: 7001,
		Terms:  rpc.OrderTWSControlTerms{Symbol: "AAA", Action: "BUY", Quantity: 1, OrderType: "LMT", LimitPrice: limit, TIF: "DAY"},
	})
	if !resp.Ok {
		t.Fatalf("order.tws_cancel error %+v; want a refused result", resp.Error)
	}
	var tws rpc.OrderTWSControlResult
	if err := json.Unmarshal(resp.Result, &tws); err != nil {
		t.Fatal(err)
	}
	if tws.Outcome != rpc.TWSOrderControlOutcomeRefused || !hasTradingBlocker(tws.Blockers, "order_writes_unavailable") {
		t.Fatalf("order.tws_cancel outcome %q blockers %+v; want refused with order_writes_unavailable", tws.Outcome, tws.Blockers)
	}

	if auth := srv.brokerWriteAuthorization(srv.tradingStatus(srv.endpoint)); auth.Allowed || !hasTradingBlocker(auth.Blockers, "order_writes_unavailable") {
		t.Fatalf("broker write authorization = allowed %v blockers %+v; want the build blocker", auth.Allowed, auth.Blockers)
	}
	if brokerWrites != 0 {
		t.Fatalf("standard build reached the broker write hook %d times", brokerWrites)
	}
}
