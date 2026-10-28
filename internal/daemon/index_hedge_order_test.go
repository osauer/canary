package daemon

import (
	"context"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/ibkr"
)

const hedgeTestConID = 750001

// hedgeOrderServer is a paper preview rig on the hedge plan's book: the
// mandate is kept on rows, the front Micro S&P future reads 5,999.75 /
// 6,000.25, the US session is open, the position impact is read from the
// rows (the future is not held unless a row says so), and the owner's
// [mandate.index_hedge] table writes maxContracts (none when zero).
func hedgeOrderServer(t *testing.T, maxContracts int, rows ...rpc.PositionView) *Server {
	t.Helper()
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	hedgeServerOn(t, s, rows...)
	clock := func() time.Time { return mandateTestNow }
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), clock)
	if err != nil {
		t.Fatal(err)
	}
	head, err := s.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.bindAuthority(head.AuthorityEpoch, head.SignerGeneration); err != nil {
		t.Fatal(err)
	}
	s.orderTokens = signer
	if maxContracts > 0 {
		s.riskPolicies.active.Mandate.IndexHedge = &risk.ConstitutionMandateIndexHedge{MaxContracts: new(maxContracts)}
		// The kept verdict is keyed to the policy it was measured under.
		result, input, at, _ := s.currentMandateVerdict()
		s.mandateVerdicts.set(result, input, at, s.riskPolicies.snapshot().policy.FingerprintKey(), false, s.captureMandateBookBinding())
	}
	pos := mandateReductionBook(rows...)
	pos.Authority = reduceTestPortfolio(nil, nil).Authority
	s.reducePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
	s.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow()}, s.currentBrokerStateScope(), nil
	}

	s.previewSessionAt = func(m marketcal.Market, _ time.Time) (marketcal.Session, error) {
		return marketcal.Session{Market: m, Label: "US equities", State: marketcal.StateRegular, IsOpen: true}, nil
	}
	s.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 5999.75, 6000.25
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
	}
	// A EUR account: the future's USD notional reaches the order cap at 0.9.
	s.orderRiskAuthorityForTest = func(_ context.Context, status rpc.TradingStatus, c rpc.ContractParams, action string, qty int) (orderPositionAuthority, error) {
		before := 0.0
		if row, ok := reduceHeldRow(pos, c.ConID); ok {
			before = row.Quantity
		}
		after := before - float64(qty)
		if action == rpc.OrderActionBuy {
			after = before + float64(qty)
		}
		now := s.orderNow()
		return orderPositionAuthority{Impact: rpc.OrderPositionImpact{Before: before, After: after, Effect: classifyPositionEffect(before, after)}, Generation: 1,
			BaseCurrency: "EUR", BaseCurrencyProvenance: ibkr.AccountBaseCurrencyExplicitTag, TestOnly: true,
			Health: ibkr.PortfolioStreamHealth{Account: status.Account, InitialCompletedAt: now, LastUpdateAt: now, ProjectionGeneration: 1}, EvidenceAt: now}, nil
	}
	s.orderFXRateForTest = func(context.Context, string, string, time.Duration) (float64, time.Time, error) {
		return 0.9, s.orderNow(), nil
	}
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	return s
}

func hedgeClosedSession(s *Server) {
	s.previewSessionAt = func(m marketcal.Market, _ time.Time) (marketcal.Session, error) {
		return marketcal.Session{Market: m, Label: "US equities", State: marketcal.StateClosed, IsOpen: false}, nil
	}
}

// A hedge preview prices the sized leg through the one order path and
// hands back its signed token, as a cut's leg does (owner decision
// 2026-10-28 20:07 CET): five contracts sold at the bid, a plain limit good
// for the day in regular hours. Its face value is far above the order cap,
// which a hedge within max_contracts passes.
func TestIndexHedgePreviewMintsATokenAtTheBid(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
	if len(res.Blockers) > 0 || res.PlanOnly || !res.Accepted || len(res.Legs) != 1 {
		t.Fatalf("preview = %+v, want one accepted leg", res)
	}
	leg := res.Legs[0]
	if leg.PreviewToken == "" || !leg.SubmitEligible || len(leg.Blockers) > 0 || leg.Terms == nil {
		t.Fatalf("leg = %+v, want a submit-eligible token", leg)
	}
	terms := leg.Terms
	if terms.Action != rpc.OrderActionSell || terms.Quantity != 5 || terms.OrderType != rpc.OrderTypeLMT || terms.TIF != rpc.OrderTIFDay || terms.LimitPrice != 5999.75 ||
		terms.Contract.SecType != "FUT" || terms.Contract.ConID != hedgeTestConID || terms.Contract.Multiplier != 5 || terms.AdaptivePriority != "" {
		t.Fatalf("terms = %+v, want SELL 5 MESZ6 LMT 5999.75 DAY", terms)
	}
	if b := terms.PriceBound; b.Side != "bid" || b.AllowancePct != 0 || b.WorstPrice != 5999.75 || b.Reference != 5999.75 {
		t.Fatalf("price bound = %+v, want the bid itself", b)
	}
	if leg.Preview == nil || leg.Preview.Draft.Source != hedgeOrderSource || leg.Preview.Draft.OutsideRTH || leg.Preview.Notional <= leg.Preview.MaxNotional {
		t.Fatalf("draft = %+v notional %v cap %v, want the hedge source above the order cap", leg.Preview.Draft, leg.Preview.Notional, leg.Preview.MaxNotional)
	}
	if res.IndexLossAfterBase == nil || math.Abs(*res.IndexLossAfterBase-1500) > 1e-6 {
		t.Fatalf("figures = %+v, want the plan's own", res.IndexLoss)
	}
}

// The table's presence is the permission and max_contracts a hard stop,
// never the target: a hedge past it is refused, not resized; outside US
// regular hours nothing is previewed.
func TestIndexHedgePreviewRefusals(t *testing.T) {
	book := mandateReductionStock("SPY", 9101, 3000, 50)
	for _, tc := range []struct {
		name string
		max  int
		edit func(*Server)
		code string
	}{
		{"no table", 0, nil, "index_hedge_unplaceable"},
		{"above max_contracts", 3, nil, "index_hedge_over_max"},
		{"held hedge counts", 4, nil, "index_hedge_over_max"},
		{"outside regular hours", 25, hedgeClosedSession, "outside_regular_hours"},
	} {
		rows := []rpc.PositionView{book}
		if tc.name == "held hedge counts" {
			// Four held short in another month leave the fall 15,000 -
			// 10,800 = 4,200: one more contract, five in all, above 4.
			rows = append(rows, mandateReductionFuture(750002, -4, 6000))
		}
		s := hedgeOrderServer(t, tc.max, rows...)
		if tc.edit != nil {
			tc.edit(s)
		}
		res := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
		if len(res.Legs) != 1 || len(res.Legs[0].Blockers) == 0 || res.Legs[0].Blockers[0].Code != tc.code || res.Legs[0].PreviewToken != "" || res.Accepted {
			t.Fatalf("%s: preview = %+v, want %s and no token", tc.name, res.Legs, tc.code)
		}
	}
}

// The order gate's rule for a future, read at preview, admission and the
// wire guard: the hedge sale needs the table and stays within
// max_contracts on the contract; a buy only shrinks a held short; every
// other future order is refused.
func TestIndexFutureOrderRule(t *testing.T) {
	mes := rpc.ContractParams{ConID: hedgeTestConID, Symbol: "MES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218", LocalSymbol: "MESZ6", Multiplier: 5}
	limits := risk.OrderLimitsInForce{IndexHedgeMaxContracts: 25}
	sell := func(qty int) rpc.OrderDraft {
		return rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: mes, Quantity: qty, Source: hedgeOrderSource}
	}
	pos := func(before, after float64) rpc.OrderPositionImpact {
		return rpc.OrderPositionImpact{Before: before, After: after, Effect: classifyPositionEffect(before, after)}
	}
	for _, tc := range []struct {
		name   string
		limits risk.OrderLimitsInForce
		draft  rpc.OrderDraft
		pos    rpc.OrderPositionImpact
		want   string
	}{
		{"hedge within the cap", limits, sell(5), pos(0, -5), ""},
		{"adding to a held hedge up to the cap", limits, sell(5), pos(-20, -25), ""},
		{"past the cap", limits, sell(6), pos(-20, -26), "max_contracts 25"},
		{"no table", risk.OrderLimitsInForce{IndexHedgeRefusal: "the risk constitution carries no [mandate.index_hedge] table"}, sell(1), pos(0, -1), "[mandate.index_hedge]"},
		{"a sale not from the hedge", limits, rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: mes, Quantity: 1}, pos(0, -1), "only as the index hedge"},
		{"selling a long future", limits, sell(1), pos(2, 1), "never from a long future"},
		{"buying back a held hedge", limits, rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: mes, Quantity: 2, Source: reduceOrderSource}, pos(-2, 0), ""},
		{"buying past flat", limits, rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: mes, Quantity: 3, Source: reduceOrderSource}, pos(-2, 1), "only closes or shrinks"},
		{"buying from flat", limits, rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: mes, Quantity: 1}, pos(0, 1), "only closes or shrinks"},
		{"another future", limits, rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: rpc.ContractParams{ConID: 9, Symbol: "ES", SecType: "FUT", Currency: "USD"}, Quantity: 1, Source: hedgeOrderSource}, pos(0, -1), "only future"},
	} {
		err := indexFutureOrderRule(tc.limits, tc.draft, tc.pos)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.want)
		}
	}
}

// A held hedge is closed as any line is reduced, deliberately: a percent
// reduce with include_hedges buys it back; without it the hedge is
// excluded; the portfolio sweep never selects it.
func TestIndexHedgeCloseIsADeliberateReduce(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50), mandateReductionFuture(hedgeTestConID, -2, 6000))
	res, err := s.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: hedgeTestConID, Percent: 100})
	if err != nil || firstBlockerCodeOf(res.Blockers) != "hedge_excluded" {
		t.Fatalf("close without include_hedges = %+v %v, want hedge_excluded", res, err)
	}
	res, err = s.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: hedgeTestConID, Percent: 100, IncludeHedges: true})
	if err != nil || !res.Accepted || res.PreviewToken == "" || res.Terms == nil || res.Terms.Action != rpc.OrderActionBuy || res.Terms.Quantity != 2 || res.Terms.Contract.SecType != "FUT" {
		t.Fatalf("close = %+v %v, want BUY 2 with a token", res, err)
	}
	if reduceEligible(mandateReductionFuture(hedgeTestConID, -2, 6000)) {
		t.Fatal("the portfolio sweep may select the hedge")
	}
}

func firstBlockerCodeOf(blockers []rpc.TradingBlocker) string {
	if len(blockers) == 0 {
		return ""
	}
	return blockers[0].Code
}
