//go:build trading

package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// reduceTokenRig is a paper trading rig for the tokened reduce: positions,
// the quote and the clock are the rig's and can move between the preview
// and the submit; every broker send is recorded. The preview-token signer
// runs on the rig clock so a token can age.
type reduceTokenRig struct {
	t        *testing.T
	srv      *Server
	mu       sync.Mutex
	now      time.Time
	pos      *rpc.PositionsResult
	bid, ask float64
	sent     []*ibkrlib.RawOrder
}

func newReduceTokenRig(t *testing.T, rows ...rpc.PositionView) *reduceTokenRig {
	t.Helper()
	srv, pos := newReducePreviewTestServer(t, rows...)
	r := &reduceTokenRig{t: t, srv: srv, now: srv.orderNow(), pos: pos, bid: 24.9, ask: 25.1}
	srv.now = r.clock
	signer, err := newOrderTokenSigner(filepath.Join(t.TempDir(), "order-preview-key"), r.clock)
	if err != nil {
		t.Fatal(err)
	}
	head, err := srv.coreStore.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.bindAuthority(head.AuthorityEpoch, head.SignerGeneration); err != nil {
		t.Fatal(err)
	}
	srv.orderTokens = signer
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		bid, ask := r.bid, r.ask
		if c.SecType == "OPT" {
			bid, ask = 2.4, 2.6
		}
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
	}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: r.clock()}, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}, nil
	}
	next := 2000
	srv.orderReserveBrokerID = func(context.Context) (int, error) { next++; return next, nil }
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		copied := *order
		r.sent = append(r.sent, &copied)
		return nil
	}
	return r
}

func (r *reduceTokenRig) clock() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.now
}

func (r *reduceTokenRig) quote(bid, ask float64) {
	r.mu.Lock()
	r.bid, r.ask = bid, ask
	r.mu.Unlock()
}

func (r *reduceTokenRig) hold(conID int, qty float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.pos.Stocks {
		if r.pos.Stocks[i].ConID == conID {
			r.pos.Stocks[i].Quantity = qty
		}
	}
	for i := range r.pos.Options {
		if r.pos.Options[i].ConID == conID {
			r.pos.Options[i].Quantity = qty
		}
	}
}

func (r *reduceTokenRig) sends() []*ibkrlib.RawOrder {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*ibkrlib.RawOrder(nil), r.sent...)
}

func (r *reduceTokenRig) preview(conID, percent int) *rpc.TradeProposalReduceResult {
	r.t.Helper()
	res, err := r.srv.tradeProposalReducePreview(r.t.Context(), rpc.TradeProposalReduceParams{ConID: conID, Percent: percent})
	if err != nil {
		r.t.Fatalf("preview: %v", err)
	}
	if !res.Accepted || res.PreviewToken == "" || res.Terms == nil {
		r.t.Fatalf("preview = %+v, want an accepted preview with a token", res)
	}
	return res
}

func (r *reduceTokenRig) submit(token, ref string) *rpc.TradeProposalReduceResult {
	r.t.Helper()
	res, err := r.srv.tradeProposalReduceSubmit(r.t.Context(), rpc.TradeProposalReduceParams{PreviewToken: token, RequestRef: ref})
	if err != nil {
		r.t.Fatalf("submit: %v", err)
	}
	return res
}

func firstBlockerCode(blockers []rpc.TradingBlocker) string {
	if len(blockers) == 0 {
		return ""
	}
	return blockers[0].Code
}

// The submit sends the reviewed terms verbatim: the quantity and the limit
// the owner confirmed, Adaptive Normal, even though the market has moved a
// little since the preview. Nothing is re-sized or re-priced.
func TestReduceTokenRoundTripPlacesExactlyTheReviewedTerms(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	preview := r.preview(reduceTestStockConID, 50)
	r.quote(24.95, 25.15) // the mid is now 25.05, inside the reviewed bound
	res := r.submit(preview.PreviewToken, "req-1")
	if !res.Accepted || len(res.Blockers) != 0 || res.Place == nil || res.OrderRef == "" || res.RequestRef != "req-1" {
		t.Fatalf("submit = %+v, want the reviewed reduction placed", res)
	}
	sent := r.sends()
	if len(sent) != 1 {
		t.Fatalf("sends = %d, want one", len(sent))
	}
	o := sent[0]
	if o.Action != rpc.OrderActionSell || o.TotalQty != 50 || o.LmtPrice != preview.Terms.LimitPrice || o.LmtPrice != 24.66 ||
		o.OrderType != rpc.OrderTypeLMT || o.TIF != rpc.OrderTIFDay || o.AdaptivePriority != "Normal" || o.OutsideRth {
		t.Fatalf("broker order = %+v, want SELL 50 LMT 24.66 DAY Adaptive Normal as reviewed", o)
	}
	if res.Terms == nil || *res.Terms != *preview.Terms || res.PreviewTokenID != preview.PreviewTokenID || res.ReduceQuantity != 50 || res.PositionQuantity != 100 {
		t.Fatalf("submit terms = %+v, want the preview's own (%+v)", res.Terms, preview.Terms)
	}
	if res.PreviewToken != "" {
		t.Fatal("a submit handed the token back out")
	}
	if res.Place.Draft.Quantity != 50 || res.Place.Draft.LimitPrice != 24.66 || res.Place.OCAGroup != "" {
		t.Fatalf("place = %+v, want the reviewed draft sent plain (no stop of Canary's own on the line)", res.Place)
	}
}

func TestReduceTokenRefusesAStaleToken(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	preview := r.preview(reduceTestStockConID, 50)
	r.mu.Lock()
	r.now = r.now.Add(orderPreviewTokenTTL + time.Minute)
	r.mu.Unlock()
	res := r.submit(preview.PreviewToken, "req-stale")
	if res.Accepted || firstBlockerCode(res.Blockers) != "token_expired" || len(r.sends()) != 0 {
		t.Fatalf("stale submit = %+v sends %d, want token_expired and nothing sent", res, len(r.sends()))
	}
	garbled := r.submit("ibkrp4.not.a.token", "req-garbled")
	if garbled.Accepted || firstBlockerCode(garbled.Blockers) != "token_invalid" || len(r.sends()) != 0 {
		t.Fatalf("garbled submit = %+v, want token_invalid and nothing sent", garbled)
	}
}

// A position that shrank below the reviewed reduction refuses in plain
// words; the quantity is never cut to fit.
func TestReduceTokenRefusesWhenThePositionShrank(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	preview := r.preview(reduceTestStockConID, 50)
	r.hold(reduceTestStockConID, 40)
	res := r.submit(preview.PreviewToken, "req-shrank")
	if res.Accepted || firstBlockerCode(res.Blockers) != "position_changed" || !strings.Contains(res.Blockers[0].Message, "40") || len(r.sends()) != 0 {
		t.Fatalf("submit after the position shrank = %+v sends %d, want position_changed naming 40 held and nothing sent", res, len(r.sends()))
	}
	if res.ReduceQuantity != 50 || res.PositionQuantity != 40 || res.Terms == nil {
		t.Fatalf("refusal = %+v, want the reviewed 50 and the current 40 disclosed", res)
	}
}

// The reviewed limit is a worst price (owner decision 2026-10-22 15:26
// CEST): a bid already below it refuses and sends nothing; a bid above it,
// however far the market rose, sends the reviewed limit unchanged.
func TestReduceTokenHoldsTheMarketToTheReviewedWorstPrice(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	preview := r.preview(reduceTestStockConID, 50)
	r.quote(24.6, 24.8) // the bid fell below the reviewed 24.66
	res := r.submit(preview.PreviewToken, "req-below")
	if res.Accepted || firstBlockerCode(res.Blockers) != "beyond_worst_price" || len(r.sends()) != 0 {
		t.Fatalf("submit with the bid below the worst price = %+v sends %d, want beyond_worst_price and nothing sent", res, len(r.sends()))
	}
	r.quote(26.0, 26.2) // 4% higher: no reason to refuse
	res = r.submit(preview.PreviewToken, "req-above")
	if !res.Accepted || len(r.sends()) != 1 || r.sends()[0].LmtPrice != 24.66 {
		t.Fatalf("submit after a rise = %+v sends %+v, want the reviewed 24.66 limit sent unchanged", res, r.sends())
	}
}

// A retry with the same request_ref replays the recorded result; the same
// token under a new reference is refused as consumed. One order leaves.
func TestReduceTokenReplayNeverSendsTwice(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	preview := r.preview(reduceTestStockConID, 50)
	first := r.submit(preview.PreviewToken, "req-once")
	if !first.Accepted || len(r.sends()) != 1 {
		t.Fatalf("first submit = %+v sends %d, want one order", first, len(r.sends()))
	}
	again := r.submit(preview.PreviewToken, "req-once")
	if !again.Replayed || !again.Accepted || again.OrderRef != first.OrderRef || len(r.sends()) != 1 {
		t.Fatalf("replayed submit = %+v sends %d, want the first result replayed and nothing more sent", again, len(r.sends()))
	}
	reused := r.submit(preview.PreviewToken, "req-twice")
	if reused.Accepted || firstBlockerCode(reused.Blockers) != "token_consumed" || len(r.sends()) != 1 {
		t.Fatalf("reused token = %+v sends %d, want token_consumed and nothing more sent", reused, len(r.sends()))
	}
}

// Each basket leg is placed on its own at its reviewed terms: a leg whose
// position changed is refused, the other leaves, and the basket says so.
func TestReduceBasketPlacesEachLegOnItsOwn(t *testing.T) {
	t.Parallel()
	second := reduceTestStockRow(200)
	second.Symbol, second.ConID, second.Mark = "SYNB", 102, 10
	r := newReduceTokenRig(t, reduceTestStockRow(100), second)
	r.srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 24.9, 25.1
		if c.ConID == 102 {
			bid, ask = 9.95, 10.05
		}
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
	}
	preview, err := r.srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Accepted || len(preview.Legs) != 2 || preview.Legs[0].PreviewToken == "" || preview.Legs[1].PreviewToken == "" {
		t.Fatalf("portfolio preview = %+v, want two legs with tokens", preview)
	}
	tokens := []string{preview.Legs[0].PreviewToken, preview.Legs[1].PreviewToken}
	r.hold(102, 50) // SYNB shrank below its reviewed 100-share cut
	res, err := r.srv.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{PreviewTokens: tokens, RequestRef: "basket-1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || res.LegCount != 2 || res.EligibleCount != 1 || res.BlockedCount != 1 || res.RequestRef != "basket-1" {
		t.Fatalf("basket submit = %+v, want one placed and one refused", res)
	}
	syn, synb := res.Legs[0], res.Legs[1]
	if !syn.Placed || syn.Symbol != "SYN" || syn.ReduceQuantity != 50 || syn.Terms == nil || len(syn.Blockers) != 0 {
		t.Fatalf("SYN leg = %+v, want placed at its reviewed terms", syn)
	}
	if synb.Placed || synb.Symbol != "SYNB" || firstBlockerCode(synb.Blockers) != "position_changed" || synb.PositionQuantity != 50 || synb.ReduceQuantity != 100 {
		t.Fatalf("SYNB leg = %+v, want position_changed with the reviewed 100 and the current 50 disclosed", synb)
	}
	sent := r.sends()
	if len(sent) != 1 || sent[0].TotalQty != 50 || sent[0].LmtPrice != 24.66 || sent[0].AdaptivePriority != "Normal" {
		t.Fatalf("sends = %+v, want SYN's reviewed 50 at 24.66 alone", sent)
	}
	again, err := r.srv.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{PreviewTokens: tokens, RequestRef: "basket-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || again.EligibleCount != 1 || len(r.sends()) != 1 {
		t.Fatalf("replayed basket = %+v sends %d, want the recorded result and nothing more sent", again, len(r.sends()))
	}
	twice, err := r.srv.reducePortfolioSubmit(t.Context(), rpc.TradeProposalReducePortfolioParams{PreviewTokens: []string{tokens[0], tokens[0]}, RequestRef: "basket-2"})
	if err != nil {
		t.Fatal(err)
	}
	if twice.Accepted || firstBlockerCode(twice.Legs[0].Blockers) != "token_consumed" || firstBlockerCode(twice.Legs[1].Blockers) != "token_duplicate" || len(r.sends()) != 1 {
		t.Fatalf("basket with a consumed token twice = %+v sends %d, want token_consumed and token_duplicate, nothing sent", twice, len(r.sends()))
	}
}

// A stock reduction leaves with Adaptive Normal; an option reduction leaves
// as a plain limit at the mid and its terms say Adaptive is not supported.
func TestReduceTokenAdaptivePerSecurityType(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100), reduceTestOptionRow(5))
	stock := r.preview(reduceTestStockConID, 25)
	option := r.preview(reduceTestOptionConID, 50)
	if stock.Terms.Algo != "Adaptive" || stock.Terms.AdaptivePriority != "Normal" || option.Terms.Algo != "" || option.Terms.AlgoNote != reduceAdaptiveNote {
		t.Fatalf("terms stock %+v option %+v, want Adaptive Normal on the stock and the note on the option", stock.Terms, option.Terms)
	}
	if res := r.submit(stock.PreviewToken, "req-stock"); !res.Accepted {
		t.Fatalf("stock submit = %+v", res)
	}
	if res := r.submit(option.PreviewToken, "req-option"); !res.Accepted || res.Terms.AlgoNote != reduceAdaptiveNote {
		t.Fatalf("option submit = %+v", res)
	}
	sent := r.sends()
	if len(sent) != 2 || sent[0].AdaptivePriority != "Normal" || sent[0].TotalQty != 25 || sent[1].AdaptivePriority != "" || sent[1].TotalQty != 2 || sent[1].LmtPrice != 2.28 {
		t.Fatalf("sends = %+v, want the stock Adaptive Normal and the option a plain LMT 2.28 for 2 contracts", sent)
	}
}

// Without a token the submit still previews at the live book and places
// that, for the human CLI path; its result carries the terms it sent.
func TestReduceSubmitWithoutTokenPlacesAtTheLiveBook(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	res, err := r.srv.tradeProposalReduceSubmit(t.Context(), rpc.TradeProposalReduceParams{Symbol: "SYN", Percent: 50, RequestRef: "req-live"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.Terms == nil || res.Terms.Quantity != 50 || res.PreviewToken != "" || len(r.sends()) != 1 {
		t.Fatalf("un-tokened submit = %+v sends %d, want the live-book reduction placed with its terms", res, len(r.sends()))
	}
	again, err := r.srv.tradeProposalReduceSubmit(t.Context(), rpc.TradeProposalReduceParams{Symbol: "SYN", Percent: 50, RequestRef: "req-live"})
	if err != nil || !again.Replayed || len(r.sends()) != 1 {
		t.Fatalf("replayed un-tokened submit = %+v %v sends %d, want a replay", again, err, len(r.sends()))
	}
}

// A reduce sale goes through the same admission as any Canary sale: beside
// Canary's own working stop on the line it joins the stop's one-cancels-all
// group, so the first fill reduces the other at the broker.
func TestReduceSaleJoinsCanarysOwnStopOCAGroup(t *testing.T) {
	t.Parallel()
	r := newCloseLinkRig(t, false, "canary-oca-ord-prot")
	contract := closeLinkContract(false)
	pos := reduceTestPortfolio([]rpc.PositionView{{Symbol: contract.Symbol, SecType: "STK", ConID: contract.ConID, Currency: "USD", Exchange: "SMART", Quantity: 70, Mark: 100}}, nil)
	r.srv.reducePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
	preview, err := r.srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: contract.ConID, Percent: 50})
	if err != nil || !preview.Accepted || preview.PreviewToken == "" || preview.Terms.Quantity != 35 {
		t.Fatalf("preview = %+v %v, want a 35-share reduction of 70 with a token", preview, err)
	}
	res, err := r.srv.tradeProposalReduceSubmit(t.Context(), rpc.TradeProposalReduceParams{PreviewToken: preview.PreviewToken, RequestRef: "req-oca"})
	if err != nil || !res.Accepted || res.Place == nil {
		t.Fatalf("submit = %+v %v, want the reduction placed", res, err)
	}
	if got := r.sent(); len(got) != 1 || got[0] != "place 1003" {
		t.Fatalf("sends = %v, want the reduction alone: the stop is linked, not changed", got)
	}
	if res.Place.OCAGroup != "canary-oca-ord-prot" || res.Place.OCAType != ibkrlib.OcaTypeReduceWithBlock || r.placed[0].OcaGroup != "canary-oca-ord-prot" {
		t.Fatalf("place OCA = %q/%d wire %q, want the stop's group with reduce-with-block", res.Place.OCAGroup, res.Place.OCAType, r.placed[0].OcaGroup)
	}
}
