//go:build trading

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// deltaTradingTestServer is a paper trading rig for the delta-reducing exit:
// the positions and the open-order inventory come from seams, the broker
// send is recorded. trading sets the retired config gates the test table
// starts from (allow_stock_short for a stock exit's re-read).
func deltaTradingTestServer(t *testing.T, trading config.Trading, position func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error)) (*Server, *[]*ibkrlib.RawOrder) {
	t.Helper()
	srv := newOrderPreviewTestServer(t, trading)
	srv.orderPreviewPositionImpact = position
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.orderDeltaPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return deltaTestPositions(), nil }
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.orderNow()}, deltaTestScope, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	sent := &[]*ibkrlib.RawOrder{}
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		copied := *order
		*sent = append(*sent, &copied)
		return nil
	}
	return srv, sent
}

// Through preview, admission and the first-byte wire guard: admission
// measures the underlying again from the positions as they are then, and
// the wire guard reuses that reading with the re-read position and no
// broker request. A hedge that appeared since the preview is refused at
// admission; a cap that binds only at the wire, because the book moved,
// asks for a new preview in plain words.
func TestDeltaReducingExitThroughAdmissionAndTheWireGuard(t *testing.T) {
	t.Parallel()
	optionLimit := 2.5
	optionParams := rpc.OrderPreviewParams{Action: "buy", Quantity: 8, LimitPrice: &optionLimit,
		Contract: rpc.ContractParams{ConID: deltaTestShortPuts, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Expiry: "20261218", Right: "P", Strike: 75, Multiplier: 100}}
	closeShortPuts := fixedPreviewPosition(-8, 0, rpc.OrderPositionEffectClose)

	t.Run("admission re-measures and the wire guard reuses the reading", func(t *testing.T) {
		srv, sent := deltaTradingTestServer(t, config.Trading{Mode: config.TradingModePaper}, closeShortPuts)
		srv.orderPreviewQuote = fixedPreviewQuote(2.4, 2.6)
		res, err := srv.previewOrder(t.Context(), optionParams)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		reads := 0
		srv.orderDeltaPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { reads++; return deltaTestPositions(), nil }
		place, err := srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: res.PreviewToken})
		if err != nil || !place.Accepted {
			t.Fatalf("place: %+v %v, want the buy-to-close of 8 contracts above the 5-contract cap accepted", place, err)
		}
		if len(*sent) != 1 || (*sent)[0].TotalQty != 8 {
			t.Fatalf("broker sends = %+v, want one order of 8 contracts", *sent)
		}
		if reads != 1 {
			t.Fatalf("positions reads after the preview = %d, want one at admission and none at the wire guard", reads)
		}
	})

	t.Run("a hedge that appeared since the preview is refused at admission", func(t *testing.T) {
		srv, sent := deltaTradingTestServer(t, config.Trading{Mode: config.TradingModePaper}, closeShortPuts)
		srv.orderPreviewQuote = fixedPreviewQuote(2.4, 2.6)
		res, err := srv.previewOrder(t.Context(), optionParams)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		// The stock was sold by hand: the options alone net −9,600, so buying
		// back the short puts removes their +38,400 and leaves −48,000: the
		// absolute delta rises.
		srv.orderDeltaPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
			pos := deltaTestPositions()
			pos.Stocks = pos.Stocks[1:]
			return pos, nil
		}
		_, err = srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: res.PreviewToken})
		if err == nil || !strings.Contains(err.Error(), "does not lower the absolute delta of SYNB (9,600 EUR before, 48,000 EUR after)") {
			t.Fatalf("place err = %v, want the admission refusal naming the raised delta", err)
		}
		if len(*sent) != 0 {
			t.Fatalf("a refused order reached the broker: %+v", *sent)
		}
	})

	t.Run("a cap that binds only at the wire asks for a new preview", func(t *testing.T) {
		// 150 of 1,000 shares at 75 EUR is 11,250 EUR: within the 12,000 EUR
		// cap (5% of NLV 240,000) at preview and admission, so no measurement
		// is read; the cap tightens before the broker send (4% of NLV is 9,600
		// EUR, so the 10,000 EUR floor binds).
		srv, sent := deltaTradingTestServer(t, config.Trading{Mode: config.TradingModePaper, AllowStockShort: new(true)}, fixedPreviewPosition(1000, 850, rpc.OrderPositionEffectReduce))
		srv.orderPreviewQuote = fixedPreviewQuote(75, 75.2)
		srv.recordOrderLimitsNLV(orderLimitsTestAccount(srv.currentBrokerStateScope().Account, 240000, "EUR", srv.orderNow()))
		if l := srv.orderLimitsInForce("EUR"); l.CapBase != 12000 {
			t.Fatalf("cap in force = %+v, want 12,000 EUR", l)
		}
		reads := 0
		srv.orderDeltaPositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { reads++; return deltaTestPositions(), nil }
		limit := 75.0
		res, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Quantity: 150, LimitPrice: &limit,
			Contract: rpc.ContractParams{ConID: deltaTestStock, Symbol: "SYNB", SecType: "STK", Currency: "EUR"}})
		if err != nil {
			t.Fatalf("preview within the cap: %v", err)
		}
		srv.orderWriteBeforeBrokerSend = func() {
			setTestOrderLimits(srv, func(o *risk.ConstitutionOrderLimits) { o.MaxOrderPctNLV = new(4.0) })
		}
		_, err = srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: res.PreviewToken})
		if err == nil || !strings.Contains(err.Error(), "order notional 11,250 EUR exceeds the order cap in force 10,000 EUR (the floor; 4% of NLV 240,000 EUR is 9,600 EUR") ||
			!strings.Contains(err.Error(), "the order cap did not apply when you previewed this order; preview it again") {
			t.Fatalf("place err = %v, want the wire guard to ask for a new preview", err)
		}
		if len(*sent) != 0 || reads != 0 {
			t.Fatalf("sends %d reads %d, want none: the cap did not bind until the wire", len(*sent), reads)
		}
	})
}

// Two exits of one line within the open-order cache window: the inventory
// is served from a cache for up to 45 s and nothing refreshes it when
// Canary places an order, so the first exit is not in the snapshot the
// second reads. Canary's own journal row, open and moved after the
// snapshot, counts instead, and the second exit is refused.
func TestDeltaReducingExitSeesCanarysOwnOrderInsideTheCacheWindow(t *testing.T) {
	t.Parallel()
	position := fixedPreviewPosition(-8, -4, rpc.OrderPositionEffectReduce)
	srv, sent := deltaTradingTestServer(t, config.Trading{Mode: config.TradingModePaper}, func(ctx context.Context, c rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
		return position(ctx, c, action, qty)
	})
	srv.orderPreviewQuote = fixedPreviewQuote(2.4, 2.6)
	// The cached snapshot completed a second before the first order went out
	// and is served unchanged to the second preview.
	srv.openOrderInventoryForTest = func(_ context.Context, fresh bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		if fresh {
			t.Fatal("the journal answered, so no fresh broker read may be issued")
		}
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.orderNow().Add(-time.Second)}, deltaTestScope, nil
	}
	limit := 2.5
	first := rpc.OrderPreviewParams{Action: "buy", Quantity: 4, LimitPrice: &limit,
		Contract: rpc.ContractParams{ConID: deltaTestShortPuts, Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Expiry: "20261218", Right: "P", Strike: 75, Multiplier: 100}}
	res, err := srv.previewOrder(t.Context(), first)
	if err != nil {
		t.Fatalf("first preview: %v", err)
	}
	if _, err := srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: res.PreviewToken}); err != nil || len(*sent) != 1 {
		t.Fatalf("first exit: %v, sends %d, want it placed", err, len(*sent))
	}

	// The first is still working and unfilled: the second would buy back 8
	// of the 8 short contracts on top of it.
	position = fixedPreviewPosition(-8, 0, rpc.OrderPositionEffectClose)
	second := first
	second.Quantity = 8
	_, err = srv.previewOrder(t.Context(), second)
	blockers := previewFailureBlockers(err)
	if err == nil || len(blockers) != 1 || blockers[0].Code != previewRiskLimitCode ||
		!strings.Contains(blockers[0].Message, "another working order already buys back 4 of the 8 SYNB 20261218 P 75 contracts you are short; with this one, more would be bought back than you are short. Cancel it first") {
		t.Fatalf("second exit err %v blockers %+v, want the refusal naming Canary's own working buy-back", err, blockers)
	}
	if len(*sent) != 1 {
		t.Fatalf("sends = %d, want only the first exit at the broker", len(*sent))
	}
}
