package daemon

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	reduceTestStockConID  = 101
	reduceTestOptionConID = 202
)

func reduceTestStockRow(qty float64) rpc.PositionView {
	return rpc.PositionView{Symbol: "SYN", SecType: "STK", ConID: reduceTestStockConID, Currency: "USD", Exchange: "SMART", Quantity: qty, Mark: 25}
}

func reduceTestOptionRow(qty float64) rpc.PositionView {
	return rpc.PositionView{Symbol: "SYN", SecType: "OPTION", ConID: reduceTestOptionConID, Currency: "USD", Exchange: "SMART",
		Quantity: qty, Mark: 2.5, Right: "C", Strike: 30, Expiry: "20261218", Multiplier: 100, Delta: new(0.5), Underlying: new(25.0)}
}

// newReducePreviewTestServer is a paper preview rig holding the given rows:
// the quote is fixed per security type and the position impact is read from
// the rows, so a reduce preview runs the production pricing and gates.
func newReducePreviewTestServer(t *testing.T, rows ...rpc.PositionView) (*Server, *rpc.PositionsResult) {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	var stocks, options []rpc.PositionView
	for _, row := range rows {
		if positionWireSecType(row.SecType) == "OPT" {
			options = append(options, row)
		} else {
			stocks = append(stocks, row)
		}
	}
	pos := reduceTestPortfolio(stocks, options)
	srv.reducePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 24.9, 25.1
		if c.SecType == "OPT" {
			bid, ask = 2.4, 2.6
		}
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
	}
	srv.orderPreviewPositionImpact = func(_ context.Context, c rpc.ContractParams, action string, qty int) (rpc.OrderPositionImpact, error) {
		row, blockers := findReducePosition(pos, c.ConID, "")
		if len(blockers) > 0 {
			t.Fatalf("position impact for an unheld contract %d: %+v", c.ConID, blockers)
		}
		after := row.Quantity - float64(qty)
		if action == rpc.OrderActionBuy {
			after = row.Quantity + float64(qty)
		}
		return rpc.OrderPositionImpact{Before: row.Quantity, After: after, Effect: classifyPositionEffect(row.Quantity, after)}, nil
	}
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	return srv, pos
}

func TestReduceAdaptivePriorityResolves(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
		refused  bool
	}{
		{"", "Normal", false}, {"Normal", "Normal", false}, {"patient", "Patient", false}, {"URGENT", "Urgent", false},
		{"none", "", false}, {"off", "", false}, {"fast", "", true}, {"Adaptive", "", true},
	} {
		got, blockers := reduceAdaptivePriority(tc.in)
		if got != tc.want || (len(blockers) > 0) != tc.refused || (tc.refused && blockers[0].Code != "bad_request") {
			t.Errorf("reduceAdaptivePriority(%q) = %q %+v, want %q refused=%v", tc.in, got, blockers, tc.want, tc.refused)
		}
	}
}

// Adaptive rides only on the one shape the broker layer sends it for: a US
// stock or ETF limit routed SMART. An option, a non-USD line or a directed
// route sends a plain limit at the mid.
func TestReduceAdaptiveSupportedPerContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		contract rpc.ContractParams
		want     bool
	}{
		{"us stock smart", rpc.ContractParams{SecType: "STK", Exchange: "SMART", Currency: "USD"}, true},
		{"us etf smart", rpc.ContractParams{SecType: "ETF", Exchange: "SMART", Currency: "USD"}, true},
		{"us option", rpc.ContractParams{SecType: "OPT", Exchange: "SMART", Currency: "USD"}, false},
		{"german stock", rpc.ContractParams{SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR", Market: "de"}, false},
		{"directed route", rpc.ContractParams{SecType: "STK", Exchange: "ARCA", Currency: "USD"}, false},
		{"a dollar line on Singapore", rpc.ContractParams{SecType: "STK", Exchange: "SMART", PrimaryExch: "SGX", Currency: "USD"}, false},
		{"a dollar line routed to Hong Kong", rpc.ContractParams{SecType: "STK", Exchange: "SEHK", PrimaryExch: "SEHK", Currency: "USD"}, false},
		{"us stock with its listing named", rpc.ContractParams{SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"}, true},
		{"bond", rpc.ContractParams{SecType: "BOND", Exchange: "SMART", Currency: "USD"}, false},
	} {
		if got := reduceAdaptiveSupported(tc.contract); got != tc.want {
			t.Errorf("%s: reduceAdaptiveSupported = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBuildPreparedReduceCarriesAdaptiveOnlyWhereSupported(t *testing.T) {
	t.Parallel()
	stock := buildPreparedReduce(reduceTestStockRow(100), 50, 0, "Normal", 1)
	if stock.params.AdaptivePriority != "Normal" || stock.algoNote != "" {
		t.Fatalf("stock prepared = priority %q note %q, want Adaptive Normal with no note", stock.params.AdaptivePriority, stock.algoNote)
	}
	plain := buildPreparedReduce(reduceTestStockRow(100), 50, 0, "", 1)
	if plain.params.AdaptivePriority != "" || plain.algoNote != "" {
		t.Fatalf("plain prepared = priority %q note %q, want a plain limit with no note", plain.params.AdaptivePriority, plain.algoNote)
	}
	option := buildPreparedReduce(reduceTestOptionRow(5), 2, 0, "Normal", 5)
	if option.params.AdaptivePriority != "" || option.algoNote != reduceAdaptiveNote {
		t.Fatalf("option prepared = priority %q note %q, want a plain limit that says Adaptive is not supported", option.params.AdaptivePriority, option.algoNote)
	}
	if option.params.Strategy != rpc.OrderStrategyAllowanceLimit || option.params.Allowance == nil || option.params.Allowance.Pct != 5 || option.params.OrderType != rpc.OrderTypeLMT || option.params.TIF != rpc.OrderTIFDay {
		t.Fatalf("option prepared order = %+v, want an LMT DAY priced 5%% beyond the touch", option.params)
	}
}

// The reviewed limit is a worst price (owner decision 2026-10-22 15:26
// CEST): the send refuses only when the market is already beyond it, never
// because it moved the other way.
func TestReducePriceBoundCheck(t *testing.T) {
	t.Parallel()
	quote := func(bid, ask float64) rpc.OrderQuoteSnapshot {
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, Midpoint: &mid}
	}
	sale := rpc.OrderDraft{Action: rpc.OrderActionSell, LimitPrice: 99, Allowance: &rpc.OrderAllowanceLimit{Pct: 1, Reference: 100}}
	reviewed := quote(100, 100.2)
	bound, blockers := reducePriceBoundCheck(sale, reviewed, quote(99.2, 99.4))
	if len(blockers) != 0 || bound.Side != "bid" || bound.Reference != 100 || bound.AllowancePct != 1 || bound.WorstPrice != 99 || math.Abs(bound.ReferenceMid-100.1) > 1e-9 {
		t.Fatalf("a fall within the allowance: bound %+v blockers %+v, want admitted with the worst price 99", bound, blockers)
	}
	if _, blockers := reducePriceBoundCheck(sale, reviewed, quote(110, 110.2)); len(blockers) != 0 {
		t.Fatalf("a 10%% rise: blockers %+v, want admitted; the broker fills at the better price", blockers)
	}
	if _, blockers := reducePriceBoundCheck(sale, reviewed, quote(98.9, 99.1)); len(blockers) != 1 || blockers[0].Code != "beyond_worst_price" {
		t.Fatalf("the bid below the worst price: blockers %+v, want beyond_worst_price", blockers)
	}
	if _, blockers := reducePriceBoundCheck(sale, reviewed, rpc.OrderQuoteSnapshot{}); len(blockers) != 1 || blockers[0].Code != "quote_unavailable" {
		t.Fatalf("no current quote: blockers %+v, want quote_unavailable", blockers)
	}
	if _, blockers := reducePriceBoundCheck(rpc.OrderDraft{Action: rpc.OrderActionSell, LimitPrice: 25}, reviewed, quote(99.9, 100.1)); len(blockers) != 1 || blockers[0].Code != "price_bound_unavailable" {
		t.Fatalf("a draft without an allowance: blockers %+v, want price_bound_unavailable", blockers)
	}
	cover := rpc.OrderDraft{Action: rpc.OrderActionBuy, LimitPrice: 50.5, Allowance: &rpc.OrderAllowanceLimit{Pct: 1, Reference: 50}}
	if bound, blockers := reducePriceBoundCheck(cover, quote(49.9, 50), quote(50.3, 50.5)); len(blockers) != 0 || bound.Side != "ask" {
		t.Fatalf("a purchase with the ask at the worst price: bound %+v blockers %+v, want admitted", bound, blockers)
	}
	if _, blockers := reducePriceBoundCheck(cover, quote(49.9, 50), quote(50.5, 50.6)); len(blockers) != 1 || blockers[0].Code != "beyond_worst_price" {
		t.Fatalf("a purchase with the ask above the worst price: blockers %+v, want beyond_worst_price", blockers)
	}
}

func TestReduceTokenPositionCheck(t *testing.T) {
	t.Parallel()
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 50, Contract: rpc.ContractParams{ConID: reduceTestStockConID, Symbol: "SYN", SecType: "STK"}}
	for _, tc := range []struct {
		name string
		held []rpc.PositionView
		want string
	}{
		{"still held", []rpc.PositionView{reduceTestStockRow(100)}, ""},
		{"grew", []rpc.PositionView{reduceTestStockRow(120)}, ""},
		{"exactly the reduction", []rpc.PositionView{reduceTestStockRow(50)}, ""},
		{"shrank below the reduction", []rpc.PositionView{reduceTestStockRow(40)}, "position_changed"},
		{"gone", nil, "position_changed"},
		{"flipped short", []rpc.PositionView{reduceTestStockRow(-10)}, "position_changed"},
	} {
		_, blockers := reduceTokenPositionCheck(reduceTestPortfolio(tc.held, nil), draft)
		got := ""
		if len(blockers) > 0 {
			got = blockers[0].Code
		}
		if got != tc.want {
			t.Errorf("%s: blockers %+v, want %q", tc.name, blockers, tc.want)
		}
	}
	if _, blockers := reduceTokenPositionCheck(nil, draft); len(blockers) != 1 || blockers[0].Code != "positions_unavailable" {
		t.Fatalf("no positions: blockers %+v, want positions_unavailable", blockers)
	}
}

// The preview hands out the signed token with the exact terms it binds: the
// terms are the token's own draft, Adaptive Normal on a limit at the mid,
// and the bound the submit will hold the market to.
func TestReducePreviewReturnsTokenAndExactTerms(t *testing.T) {
	t.Parallel()
	srv, _ := newReducePreviewTestServer(t, reduceTestStockRow(100))
	res, err := srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{Symbol: "SYN", Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || !res.SubmitEligible || res.PreviewToken == "" || res.Terms == nil || len(res.Blockers) != 0 {
		t.Fatalf("preview = %+v, want an accepted preview with a token and terms", res)
	}
	terms := res.Terms
	if terms.Action != rpc.OrderActionSell || terms.Quantity != 50 || terms.OrderType != rpc.OrderTypeLMT || terms.TIF != rpc.OrderTIFDay ||
		terms.LimitPrice != 24.66 || terms.Algo != "Adaptive" || terms.AdaptivePriority != "Normal" || terms.AlgoNote != "" ||
		terms.Contract.ConID != reduceTestStockConID || terms.PreviewTokenID != res.PreviewTokenID || !terms.ExpiresAt.Equal(res.PreviewTokenExpiresAt) {
		t.Fatalf("terms = %+v, want SELL 50 SYN LMT 24.66 (1%% below the 24.90 bid) DAY Adaptive Normal under token %s", terms, res.PreviewTokenID)
	}
	if b := terms.PriceBound; b.Side != "bid" || b.AllowancePct != 1 || b.WorstPrice != terms.LimitPrice || !positiveFinite(b.Reference) || terms.LimitPrice >= b.Reference {
		t.Fatalf("price bound = %+v, want the limit as the worst price, 1%% below the reviewed bid", b)
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil {
		t.Fatalf("the preview's token does not verify: %v", err)
	}
	draft := payload.Draft
	if payload.TokenID != res.PreviewTokenID || draft.Source != reduceOrderSource || draft.Quantity != terms.Quantity || draft.LimitPrice != terms.LimitPrice ||
		draft.AdaptivePriority != terms.AdaptivePriority || draft.Action != terms.Action || draft.TIF != terms.TIF || draft.Contract.ConID != terms.Contract.ConID {
		t.Fatalf("token draft = %+v, want the terms the preview returned (%+v)", draft, terms)
	}
	if payload.ExpiresAt.Sub(payload.IssuedAt) != orderPreviewTokenTTL || !payload.ExpiresAt.Equal(terms.ExpiresAt) {
		t.Fatalf("token issued %s expires %s, want the %s preview TTL stated in the terms (%s)", payload.IssuedAt, payload.ExpiresAt, orderPreviewTokenTTL, terms.ExpiresAt)
	}
	if res.Preview == nil || res.Preview.Draft.AdaptivePriority != "Normal" {
		t.Fatalf("sanitized preview = %+v, want the Adaptive draft disclosed", res.Preview)
	}
}

func TestReducePreviewHonoursThePriorityAsked(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		asked, wantPriority, wantAlgo string
	}{
		{"Patient", "Patient", "Adaptive"}, {"urgent", "Urgent", "Adaptive"}, {"none", "", ""},
	} {
		srv, _ := newReducePreviewTestServer(t, reduceTestStockRow(100))
		res, err := srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestStockConID, Percent: 25, AdaptivePriority: tc.asked})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Accepted || res.Terms == nil || res.Terms.AdaptivePriority != tc.wantPriority || res.Terms.Algo != tc.wantAlgo || res.Terms.AlgoNote != "" || res.Terms.Quantity != 25 {
			t.Fatalf("asked %q: preview = %+v terms %+v, want priority %q algo %q and 25 shares", tc.asked, res, res.Terms, tc.wantPriority, tc.wantAlgo)
		}
	}
	srv, _ := newReducePreviewTestServer(t, reduceTestStockRow(100))
	res, err := srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestStockConID, Percent: 25, AdaptivePriority: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted || res.PreviewToken != "" || len(res.Blockers) != 1 || res.Blockers[0].Code != "bad_request" {
		t.Fatalf("an unknown priority minted a token: %+v", res)
	}
}

// An option reduction is a plain limit at the mid, and the terms say why.
func TestReducePreviewOptionSendsPlainLimitAndSaysWhy(t *testing.T) {
	t.Parallel()
	srv, _ := newReducePreviewTestServer(t, reduceTestOptionRow(5))
	res, err := srv.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: reduceTestOptionConID, Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || res.PreviewToken == "" || res.Terms == nil {
		t.Fatalf("option preview = %+v, want an accepted preview with a token", res)
	}
	terms := res.Terms
	if terms.Algo != "" || terms.AdaptivePriority != "" || terms.AlgoNote != reduceAdaptiveNote || terms.Quantity != 2 || terms.OrderType != rpc.OrderTypeLMT || terms.LimitPrice != 2.28 || terms.Contract.SecType != "OPT" {
		t.Fatalf("option terms = %+v, want a plain LMT 2.28 (5%% below the 2.40 bid) for 2 contracts noting %q", terms, reduceAdaptiveNote)
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil || payload.Draft.AdaptivePriority != "" {
		t.Fatalf("option token = %+v %v, want a plain-limit draft", payload.Draft, err)
	}
}

// A portfolio preview hands out one token per eligible leg with its terms.
func TestReducePortfolioPreviewReturnsTokenPerLeg(t *testing.T) {
	t.Parallel()
	second := reduceTestStockRow(200)
	second.Symbol, second.ConID, second.Mark = "SYNB", 102, 10
	srv, _ := newReducePreviewTestServer(t, reduceTestStockRow(100), second)
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		bid, ask := 24.9, 25.1
		if c.ConID == 102 {
			bid, ask = 9.95, 10.05
		}
		mid := (bid + ask) / 2
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive, QuoteQuality: "firm"}, nil
	}
	res, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted || len(res.Legs) != 2 || res.EligibleCount != 2 {
		t.Fatalf("portfolio preview = %+v, want two eligible legs", res)
	}
	for _, leg := range res.Legs {
		if leg.PreviewToken == "" || leg.Terms == nil || leg.Terms.Quantity != leg.ReduceQuantity || leg.Terms.AdaptivePriority != "Normal" || leg.Terms.PreviewTokenID != leg.PreviewTokenID {
			t.Fatalf("leg %s = %+v terms %+v, want its own token and terms", leg.Symbol, leg, leg.Terms)
		}
		if payload, err := srv.orderTokens.verify(leg.PreviewToken); err != nil || payload.Draft.Contract.ConID != leg.ConID || payload.Draft.Quantity != leg.ReduceQuantity {
			t.Fatalf("leg %s token = %+v %v, want its own draft", leg.Symbol, payload.Draft, err)
		}
	}
}

// In the standard build a tokened submit is refused by the write gate before
// the token is read, like every other broker write.
func TestReduceTokenSubmitFailsClosedWithoutTheTradingBuild(t *testing.T) {
	if orderWritesAvailable {
		t.Skip("trading build")
	}
	t.Parallel()
	srv, _ := newReducePreviewTestServer(t, reduceTestStockRow(100))
	res, err := srv.tradeProposalReduceSubmit(t.Context(), rpc.TradeProposalReduceParams{PreviewToken: "ibkrp4.synthetic.token", RequestRef: "req-1"})
	if err != nil || res.Accepted || len(res.Blockers) == 0 {
		t.Fatalf("submit = %+v %v, want a refusal from the write gate", res, err)
	}
}

// The allowance limit (owner decision 2026-10-22 15:26 CEST): the policy's
// percent beyond the touch, rounded toward the touch so the allowance is
// never exceeded, and an option at least one tick beyond it.
func TestAllowanceLimitPrice(t *testing.T) {
	t.Parallel()
	quote := func(bid, ask float64) rpc.OrderQuoteSnapshot {
		return rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}
	}
	stock := rpc.ContractParams{SecType: "STK"}
	option := rpc.ContractParams{SecType: "OPT"}
	for _, tc := range []struct {
		name      string
		action    string
		contract  rpc.ContractParams
		pct       float64
		q         rpc.OrderQuoteSnapshot
		limit     float64
		reference float64
	}{
		{"a sale 1% below the bid, rounded up to the cent", rpc.OrderActionSell, stock, 1, quote(24.9, 25.1), 24.66, 24.9},
		{"a purchase 1% above the ask, rounded down", rpc.OrderActionBuy, stock, 1, quote(49.9, 50.03), 50.53, 50.03},
		{"an option sale 5% below the bid", rpc.OrderActionSell, option, 5, quote(2.4, 2.6), 2.28, 2.4},
		{"a cheap option still a tick below the bid", rpc.OrderActionSell, option, 5, quote(0.1, 0.15), 0.09, 0.1},
	} {
		limit, reference, err := allowanceLimitPrice(tc.action, &rpc.OrderAllowanceLimit{Pct: tc.pct}, nil, tc.contract, tc.q)
		if err != nil || math.Abs(limit-tc.limit) > 1e-9 || reference != tc.reference {
			t.Errorf("%s: limit %v reference %v err %v, want %v from %v", tc.name, limit, reference, err, tc.limit, tc.reference)
		}
	}
	if _, _, err := allowanceLimitPrice(rpc.OrderActionSell, nil, nil, stock, quote(24.9, 25.1)); err == nil {
		t.Fatal("a request without the daemon's allowance was priced")
	}
	if _, _, err := allowanceLimitPrice(rpc.OrderActionSell, &rpc.OrderAllowanceLimit{Pct: 1}, nil, stock, rpc.OrderQuoteSnapshot{DataType: rpc.MarketDataLive}); err == nil {
		t.Fatal("a sale without a bid was priced")
	}
}
