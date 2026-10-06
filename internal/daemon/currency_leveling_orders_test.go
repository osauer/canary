package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// levelingRow is the synthetic example as a conversion row on the pair's
// resolved contract: SELL 17,219 EUR.USD to repay USD −20,000 from EUR
// +60,000 at 1.17.
func levelingRow(t *testing.T) rpc.TradeProposal {
	t.Helper()
	return levelingRows(t, levelingInput(-20000, 60000), "USD")[0]
}

// levelingRows are a loan's conversion rows on resolved contracts.
func levelingRows(t *testing.T, in currencyLevelingInput, loan string) []rpc.TradeProposal {
	t.Helper()
	p := levelingPolicy()
	plan := currencyLevelingPlanFor(p, in)
	b := levelingBundleFor(plan, loan)
	if b == nil {
		t.Fatalf("no %s bundle: %+v", loan, plan.status.Currencies)
	}
	resolved := levelingResolved(*b)
	id := currencyLevelingBundleID(resolved)
	var rows []rpc.TradeProposal
	for _, leg := range resolved.legs {
		leg.block.BundleID = id
		row := currencyLevelingRow(protectionPolicy{Cash: protectionCashPolicy{Leveling: p}}, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{},
			time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC), plan.status, resolved, leg)
		row.Revision = "rev-level-" + leg.block.FundingCurrency
		rows = append(rows, row)
	}
	return rows
}

// levelingPreview is a preview of row's own conversion at bid/ask with its
// limit bounded the way the preview bounds it.
func levelingPreview(t *testing.T, row rpc.TradeProposal, bid, ask float64) *rpc.OrderPreviewResult {
	t.Helper()
	limit, err := fxBoundedLimitPrice(row.Action, 0.00005, rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: time.Now()}, row.CurrencyLeveling.MaxSlippageBP)
	if err != nil {
		t.Fatalf("limit: %v", err)
	}
	fx := currencyLevelingOrderTerms(row)
	fx.Bid, fx.Ask = &bid, &ask
	return &rpc.OrderPreviewResult{Draft: rpc.OrderDraft{Action: row.Action, Contract: row.Contract, Quantity: row.Quantity, OrderType: rpc.OrderTypeLMT,
		LimitPrice: limit, TIF: rpc.OrderTIFDay, Strategy: rpc.OrderStrategyBoundedLimit, Source: proposalOrderSource, FX: fx},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectOpenShort}}
}

func TestFXBoundedLimitPrice(t *testing.T) {
	live := func(bid, ask float64) rpc.OrderQuoteSnapshot {
		return rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: time.Now()}
	}
	// A sell sits 2 bp under the mid, on the tick toward the mid, at or
	// below the bid: marketable, never worse than the bound.
	limit, err := fxBoundedLimitPrice(rpc.OrderActionSell, 0.00005, live(1.16995, 1.17005), 2)
	if err != nil || limit != 1.16980 {
		t.Fatalf("sell limit = %v, %v; want 1.16980", limit, err)
	}
	limit, err = fxBoundedLimitPrice(rpc.OrderActionBuy, 0.00005, live(1.16995, 1.17005), 2)
	if err != nil || limit != 1.17020 {
		t.Fatalf("buy limit = %v, %v; want 1.17020", limit, err)
	}
	refusals := map[string]struct {
		quote rpc.OrderQuoteSnapshot
		code  string
	}{
		"delayed":   {rpc.OrderQuoteSnapshot{Bid: new(1.1699), Ask: new(1.1701), DataType: rpc.MarketDataDelayed, PriceAt: time.Now()}, previewQuoteNotLiveCode},
		"stale":     {rpc.OrderQuoteSnapshot{Bid: new(1.1699), Ask: new(1.1701), DataType: rpc.MarketDataLive}, previewQuoteStaleCode},
		"one-sided": {rpc.OrderQuoteSnapshot{Bid: new(1.1699), DataType: rpc.MarketDataLive, PriceAt: time.Now()}, previewQuoteNotTwoSidedCode},
		"crossed":   {rpc.OrderQuoteSnapshot{Bid: new(1.1702), Ask: new(1.1701), DataType: rpc.MarketDataLive, PriceAt: time.Now()}, previewQuoteNotTwoSidedCode},
		"wide":      {live(1.1690, 1.1710), currencyLevelingWideQuoteCode},
	}
	for name, tc := range refusals {
		_, err := fxBoundedLimitPrice(rpc.OrderActionSell, 0.00005, tc.quote, 2)
		refusal, ok := errors.AsType[*previewRefusal](err)
		if !ok || len(refusal.blockers) == 0 || refusal.blockers[0].Code != tc.code {
			t.Errorf("%s: err = %v, want %s", name, err, tc.code)
		}
	}
}

func TestIdealproSessionAt(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	for _, tc := range []struct {
		at       time.Time
		open     bool
		nextOpen string
	}{
		{time.Date(2026, 10, 5, 10, 0, 0, 0, ny), true, ""},                            // Monday morning
		{time.Date(2026, 10, 5, 17, 5, 0, 0, ny), false, "2026-10-05T17:15:00-04:00"},  // the daily break
		{time.Date(2026, 10, 9, 17, 30, 0, 0, ny), false, "2026-10-11T17:15:00-04:00"}, // Friday after the close
		{time.Date(2026, 10, 10, 12, 0, 0, 0, ny), false, "2026-10-11T17:15:00-04:00"}, // Saturday
		{time.Date(2026, 10, 11, 18, 0, 0, 0, ny), true, ""},                           // Sunday evening
	} {
		s, ok := idealproSessionAt(tc.at)
		if !ok || s.IsOpen != tc.open || s.Market != idealproSessionMarket {
			t.Fatalf("%s: session = %+v, want open %v", tc.at, s, tc.open)
		}
		if !tc.open && (s.NextOpen == nil || !s.NextOpen.Equal(mustTime(t, tc.nextOpen)) || s.State != marketcal.StateClosed) {
			t.Fatalf("%s: next open = %v, want %s", tc.at, s.NextOpen, tc.nextOpen)
		}
	}
	if err := currencyLevelingSessionRefusal(time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("a Saturday preview must refuse")
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// No RPC caller can preview a conversion: the terms are daemon-internal, a
// CASH preview without them is refused, and the contract must be a
// conventional pair on IDEALPRO.
func TestCashPreviewIsAdmittedOnlyForALevelingRow(t *testing.T) {
	var decoded rpc.OrderPreviewParams
	if err := json.Unmarshal([]byte(`{"action":"SELL","contract":{"symbol":"EUR","sec_type":"CASH","currency":"USD"},"quantity":1,"fx":{"currency":"USD","max_slippage_bp":2}}`), &decoded); err != nil || decoded.FX != nil {
		t.Fatalf("decoded FX = %+v, %v; the terms must not decode from RPC", decoded.FX, err)
	}
	if err := validatePreviewFXParams(decoded, false); err == nil || !strings.Contains(err.Error(), "currency_leveling") {
		t.Fatalf("a CASH preview without terms = %v", err)
	}
	row := levelingRow(t)
	params := proposalOrderPreviewParams(row, row.Quantity, 0)
	if params.FX == nil || validatePreviewFXParams(params, false) != nil {
		t.Fatalf("the row's own params = %+v", params.FX)
	}
	for name, edit := range map[string]func(*rpc.OrderPreviewParams){
		"replace": nil,
		"market":  func(p *rpc.OrderPreviewParams) { p.OrderType = "MKT" },
		"GTC":     func(p *rpc.OrderPreviewParams) { p.TIF = rpc.OrderTIFGTC },
		"limit":   func(p *rpc.OrderPreviewParams) { p.LimitPrice = new(1.2) },
		"outside": func(p *rpc.OrderPreviewParams) { p.OutsideRTH = true },
	} {
		p := params
		if edit != nil {
			edit(&p)
		}
		if err := validatePreviewFXParams(p, edit == nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := normalizePreviewContract(rpc.ContractParams{Symbol: "EUR", SecType: "CASH", Currency: "USD"}); err != nil {
		t.Fatalf("EUR.USD: %v", err)
	}
	for name, c := range map[string]rpc.ContractParams{
		"inverted pair": {Symbol: "USD", SecType: "CASH", Currency: "EUR"},
		"SMART":         {Symbol: "EUR", SecType: "CASH", Exchange: "SMART", Currency: "USD"},
		"unknown pair":  {Symbol: "EUR", SecType: "CASH", Currency: "HKD"},
		"expiry":        {Symbol: "EUR", SecType: "CASH", Currency: "USD", Expiry: "20261219"},
	} {
		if _, err := normalizePreviewContract(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if blockers := proposalPreviewSafetyBlockers(rpc.TradeProposal{Bucket: rpc.TradeProposalBucketTrailingStop, SecType: "CASH", Action: rpc.OrderActionSell, Quantity: 1, MaxQuantity: 1,
		PositionEffect: rpc.OrderPositionEffectReduce, OrderType: rpc.OrderTypeLMT, Contract: row.Contract}, levelingPreview(t, row, 1.16995, 1.17005)); !hasBlockerCode(blockers, "unsupported_security_type") ||
		!hasBlockerCode(blockers, "preview_effect_not_close_reduce") {
		t.Fatalf("a CASH order outside leveling = %+v", blockers)
	}
}

func hasBlockerCode(blockers []rpc.TradingBlocker, code string) bool {
	for _, b := range blockers {
		if b.Code == code {
			return true
		}
	}
	return false
}

// The second exception to close_reduce_only admits only the row's own
// conversion inside its bounds, judged against the ledger terms, not the
// pair's position effect.
func TestCurrencyLevelingReduceOnlyException(t *testing.T) {
	row := levelingRow(t)
	if blockers := proposalPreviewSafetyBlockers(row, levelingPreview(t, row, 1.16995, 1.17005)); len(blockers) != 0 {
		t.Fatalf("the row's own conversion = %+v", blockers)
	}
	for name, edit := range map[string]func(*rpc.TradeProposal){
		"shadow":           func(p *rpc.TradeProposal) { p.Shadow = true },
		"other bucket":     func(p *rpc.TradeProposal) { p.Bucket = rpc.TradeProposalBucketRiskReduction },
		"other side":       func(p *rpc.TradeProposal) { p.Action = rpc.OrderActionBuy },
		"inverted pair":    func(p *rpc.TradeProposal) { p.Contract.Symbol, p.Contract.Currency = "USD", "EUR" },
		"not borrowed":     func(p *rpc.TradeProposal) { p.CurrencyLeveling.Cash = 100 },
		"target above cap": func(p *rpc.TradeProposal) { p.CurrencyLeveling.Target = 5000 },
		"no funding":       func(p *rpc.TradeProposal) { p.CurrencyLeveling.FundingCommitted = 60000 },
		"no allotment":     func(p *rpc.TradeProposal) { p.CurrencyLeveling.Allotment = 0 },
		"allotment > cash": func(p *rpc.TradeProposal) { p.CurrencyLeveling.Allotment = 70000 },
		"leg out of range": func(p *rpc.TradeProposal) { p.CurrencyLeveling.Leg = 2 },
		"SMART":            func(p *rpc.TradeProposal) { p.Contract.Exchange = "SMART" },
		"no contract id":   func(p *rpc.TradeProposal) { p.Contract.ConID = 0 },
	} {
		p := levelingRow(t)
		edit(&p)
		if _, ok := currencyLevelingReduceException(p); ok {
			t.Errorf("%s: qualified", name)
		}
	}
	bound := map[string]struct {
		edit func(*rpc.TradeProposal, *rpc.OrderPreviewResult)
		code string
	}{
		// USD moved to −19,300 since the plan: the same quantity overshoots.
		"beyond the cushion": {func(p *rpc.TradeProposal, _ *rpc.OrderPreviewResult) { p.CurrencyLeveling.Cash = -19300 }, rpc.CurrencyLevelingBlockerBeyondTarget},
		// EUR.USD rose 100 pips since the plan: at the ask the sell brings in
		// more than the cushion allows.
		"far side": {func(p *rpc.TradeProposal, r *rpc.OrderPreviewResult) { *r = *levelingPreview(t, *p, 1.1820, 1.1822) }, rpc.CurrencyLevelingBlockerBeyondTarget},
		// The conversion may spend only its allotment of the funding cash.
		"funding": {func(p *rpc.TradeProposal, _ *rpc.OrderPreviewResult) { p.CurrencyLeveling.Allotment = 12000 }, rpc.CurrencyLevelingBlockerFundingShort},
		// The preview resolved another contract than the row names.
		"contract": {func(_ *rpc.TradeProposal, r *rpc.OrderPreviewResult) { r.Draft.Contract.ConID = 1 }, rpc.CurrencyLevelingBlockerOrderTerms},
		"quantity": {func(_ *rpc.TradeProposal, r *rpc.OrderPreviewResult) { r.Draft.Quantity = 30000 }, rpc.CurrencyLevelingBlockerOrderTerms},
		"limit":    {func(_ *rpc.TradeProposal, r *rpc.OrderPreviewResult) { r.Draft.LimitPrice = 1.1600 }, rpc.CurrencyLevelingBlockerOrderTerms},
		"no quote": {func(_ *rpc.TradeProposal, r *rpc.OrderPreviewResult) { r.Draft.FX = nil }, rpc.CurrencyLevelingBlockerOrderTerms},
		"venue":    {func(_ *rpc.TradeProposal, r *rpc.OrderPreviewResult) { r.Draft.Contract.Exchange = "SMART" }, rpc.CurrencyLevelingBlockerOrderTerms},
	}
	for name, tc := range bound {
		p := levelingRow(t)
		preview := levelingPreview(t, p, 1.16995, 1.17005)
		tc.edit(&p, preview)
		if blockers := proposalPreviewSafetyBlockers(p, preview); !hasBlockerCode(blockers, tc.code) {
			t.Errorf("%s: blockers = %+v, want %s", name, blockers, tc.code)
		}
	}
}

// levelingPreviewRig is the order-preview test server wired for the example
// conversion: a live EUR.USD quote read during the preview, an accepted
// WhatIf and an empty open-order inventory.
type levelingPreviewRig struct {
	srv    *Server
	engine *proposalEngine
	row    rpc.TradeProposal
	now    time.Time
	bid    *float64
	ask    *float64
	quotes int
	drafts []rpc.OrderDraft
	orders []ibkrlib.OrderLifecycleEvent
}

func newLevelingPreviewRig(t *testing.T, now time.Time) *levelingPreviewRig {
	t.Helper()
	rig := &levelingPreviewRig{now: now, row: levelingRow(t), bid: new(1.16995), ask: new(1.17005)}
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: new(1e6)})
	srv.now = func() time.Time { return rig.now }
	srv.orderContractResolverForTest = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.ContractParams, error) {
		c.ConID, c.MinTick = 12087792, 0.00005
		return c, nil
	}
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		rig.quotes++
		if c.SecType != "CASH" || c.Exchange != "IDEALPRO" || c.Symbol != "EUR" || c.Currency != "USD" || c.ConID != 12087792 {
			t.Fatalf("quoted contract = %+v", c)
		}
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: rig.bid, Ask: rig.ask, DataType: rpc.MarketDataLive, PriceAt: rig.now, AsOf: rig.now}, nil
	}
	// The pair's own position says nothing about a currency balance: a sell
	// with no EUR.USD position reads open_short, and only the exception's
	// ledger bounds decide.
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, -17219, rpc.OrderPositionEffectOpenShort)
	srv.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		rig.drafts = append(rig.drafts, d)
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: rig.now, Orders: rig.orders}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, nil
	}
	rig.srv = srv
	rig.engine = &proposalEngine{server: srv, now: func() time.Time { return rig.now }, queued: &queuedAuthStore{},
		resolve: func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			return rig.row, rig.row.Blockers, nil
		}}
	return rig
}

func (r *levelingPreviewRig) preview(t *testing.T) rpc.TradeProposalPreviewResult {
	t.Helper()
	out, err := r.engine.Preview(context.Background(), rpc.TradeProposalPreviewParams{Key: r.row.Key, Revision: r.row.Revision})
	if err != nil {
		t.Fatalf("preview err = %v", err)
	}
	return out
}

// An active conversion row previews as a CASH LMT DAY order on IDEALPRO:
// the limit 2 bp under a live mid, the quote it was bounded from in the
// draft, WhatIf on the built order, every gate an ordinary proposal meets.
func TestCurrencyLevelingPreview(t *testing.T) {
	rig := newLevelingPreviewRig(t, time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)) // 10:00 in New York
	out := rig.preview(t)
	if !out.Accepted || !out.SubmitEligible || len(out.Blockers) != 0 || len(rig.drafts) != 1 {
		t.Fatalf("preview = %+v", out)
	}
	d := rig.drafts[0]
	if d.Contract.SecType != "CASH" || d.Contract.Exchange != "IDEALPRO" || d.Contract.ConID != rig.row.Contract.ConID || d.Action != rpc.OrderActionSell || d.Quantity != 17219 ||
		d.OrderType != rpc.OrderTypeLMT || d.TIF != rpc.OrderTIFDay || d.OutsideRTH || d.LimitPrice != 1.16980 || d.Strategy != rpc.OrderStrategyBoundedLimit {
		t.Fatalf("draft = %+v", d)
	}
	if d.FX == nil || d.FX.Bid == nil || *d.FX.Bid != 1.16995 || d.FX.Currency != "USD" || d.FX.FundingCurrency != "EUR" {
		t.Fatalf("draft FX terms = %+v", d.FX)
	}
	// The protobuf encoder takes the conversion in exactly this shape.
	contract, order := previewIBKRStrategyContract(d), previewIBKROrder(d)
	if err := ibkrlib.ValidateOrder(&ibkrlib.IBKROrder{ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType, Exchange: contract.Exchange,
		Currency: contract.Currency, Action: order.Action, TotalQty: order.TotalQty, OrderType: order.OrderType,
		LmtPrice: order.LmtPrice, LmtPriceSet: order.LmtPriceSet, TIF: order.TIF}); err != nil {
		t.Fatalf("the previewed conversion fails the broker check: %v", err)
	}
}

func TestCurrencyLevelingPreviewRefusals(t *testing.T) {
	monday := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	t.Run("shadow is never previewed", func(t *testing.T) {
		rig := newLevelingPreviewRig(t, monday)
		rig.row.Shadow, rig.row.Blockers = true, nil
		out := rig.preview(t)
		if out.Accepted || !hasBlockerCode(out.Blockers, "shadow_mode") || rig.quotes != 0 || len(rig.drafts) != 0 {
			t.Fatalf("shadow preview = %+v (quotes %d)", out, rig.quotes)
		}
	})
	t.Run("IDEALPRO closed refuses before any quote", func(t *testing.T) {
		rig := newLevelingPreviewRig(t, time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC)) // Saturday
		out := rig.preview(t)
		if out.Accepted || !hasBlockerCode(out.Blockers, previewMarketClosedCode) || rig.quotes != 0 {
			t.Fatalf("Saturday preview = %+v (quotes %d)", out, rig.quotes)
		}
	})
	t.Run("one-sided quote holds", func(t *testing.T) {
		rig := newLevelingPreviewRig(t, monday)
		rig.ask = nil
		if out := rig.preview(t); out.Accepted || !hasBlockerCode(out.Blockers, previewQuoteNotTwoSidedCode) {
			t.Fatalf("one-sided preview = %+v", out)
		}
	})
	t.Run("wide quote holds", func(t *testing.T) {
		rig := newLevelingPreviewRig(t, monday)
		rig.bid, rig.ask = new(1.1690), new(1.1710)
		if out := rig.preview(t); out.Accepted || !hasBlockerCode(out.Blockers, currencyLevelingWideQuoteCode) || len(rig.drafts) != 0 {
			t.Fatalf("wide preview = %+v", out)
		}
	})
	t.Run("a conversion already working holds", func(t *testing.T) {
		rig := newLevelingPreviewRig(t, monday)
		rig.orders = []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder, SecType: "CASH", Symbol: "EUR", Currency: "USD", Action: "BUY",
			TotalQuantity: 1000, Remaining: 1000, Status: "Submitted", Account: "DU1234567"}}
		if out := rig.preview(t); out.Accepted || !hasBlockerCode(out.Blockers, rpc.CurrencyLevelingBlockerWorking) {
			t.Fatalf("preview beside a working conversion = %+v", out)
		}
	})
	t.Run("a balance that moved past the plan holds", func(t *testing.T) {
		rig := newLevelingPreviewRig(t, monday)
		rig.row.CurrencyLeveling.Cash = -19300
		if out := rig.preview(t); out.Accepted || !hasBlockerCode(out.Blockers, rpc.CurrencyLevelingBlockerBeyondTarget) {
			t.Fatalf("overshooting preview = %+v", out)
		}
	})
}

// Leveling is never pre-authorised or queued: the policy refuses it in
// pre_authorised, and no automatic bucket maps to it.
func TestCurrencyLevelingIsNeverPreAuthorised(t *testing.T) {
	row := levelingRow(t)
	if bucket := automaticBucketFor(row); bucket != "" {
		t.Fatalf("automatic bucket = %q", bucket)
	}
	p := defaultProtectionPolicy()
	p.Authority.PreAuthorised = []string{"currency_leveling"}
	if err := validateProtectionPolicy(p); err == nil {
		t.Fatal("pre_authorised accepted currency_leveling")
	}
	market, session, hasMarket, known := (&proposalEngine{}).proposalSessionAt(row, time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC), nil)
	if market != idealproSessionMarket || !hasMarket || !known || session.IsOpen {
		t.Fatalf("Saturday readiness session = %s %+v", market, session)
	}
}

// A working conversion of either side holds the sweep: a sell of EUR.USD
// spends EUR the sweep would otherwise invest.
func TestCashSweepHoldsWhileAConversionWorks(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	for _, action := range []string{"BUY", "SELL"} {
		got := cashSweepCommitmentsFrom([]ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder, SecType: "CASH", Symbol: "EUR", Currency: "USD",
			Action: action, OrderType: "LMT", LimitPrice: 1.17, TotalQuantity: 1000, Remaining: 1000, Status: "Submitted", Account: scope.Account}}, nil, scope)
		if got.Unknown[""] == "" {
			t.Fatalf("%s conversion: commitments = %+v, want every currency unknown", action, got)
		}
	}
}

func TestCurrencyLevelingCommitmentsAndWorkingOrders(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	working := func(o ibkrlib.OrderLifecycleEvent) ibkrlib.OrderLifecycleEvent {
		o.Type, o.Status, o.Account = ibkrlib.OrderLifecycleEventOpenOrder, "Submitted", nonEmptyString(o.Account, scope.Account)
		return o
	}
	orders := []ibkrlib.OrderLifecycleEvent{
		working(ibkrlib.OrderLifecycleEvent{SecType: "BILL", Currency: "EUR", Action: "BUY", OrderType: "LMT", LimitPrice: 99.5, TotalQuantity: 10000, Remaining: 10000, Multiplier: 0}),
		working(ibkrlib.OrderLifecycleEvent{SecType: "STK", Currency: "USD", Action: "BUY", OrderType: "MKT", TotalQuantity: 10, Remaining: 10}),
		working(ibkrlib.OrderLifecycleEvent{SecType: "CASH", Symbol: "GBP", Currency: "USD", Action: "SELL", OrderType: "LMT", LimitPrice: 1.3, TotalQuantity: 1000, Remaining: 1000}),
		working(ibkrlib.OrderLifecycleEvent{SecType: "CASH", Symbol: "EUR", Currency: "CHF", Action: "BUY", TotalQuantity: 1000, Remaining: 1000, Account: "U9999999"}),
	}
	committed, unknown := currencyLevelingCommitted(orders, nil, scope)
	if committed["EUR"] <= 0 || unknown["USD"] == "" || committed["GBP"] != 0 || unknown["GBP"] != "" {
		t.Fatalf("committed %+v unknown %+v", committed, unknown)
	}
	w := currencyLevelingWorkingFrom(orders, scope)
	if !w["GBP"] || !w["USD"] || w["CHF"] || w["EUR"] {
		t.Fatalf("working = %+v", w)
	}
	// The planner leaves what working buys hold in the funding currency, and
	// the cushion it keeps.
	in := levelingInput(-20000, 60000)
	in.Committed = map[string]float64{"EUR": 48000}
	b := levelingBundleFor(currencyLevelingPlanFor(levelingPolicy(), in), "USD")
	if b == nil || !b.legs[0].block.FundingShort || b.legs[0].block.Spent > 11750 || b.legs[0].block.FundingCommitted != 48000 || b.legs[0].block.Allotment > 11750+1e-6 {
		t.Fatalf("plan beside a working bill buy = %+v", b)
	}
	in.CommittedUnknown = map[string]string{"EUR": "a working buy in EUR has no price bound"}
	if b := levelingBundleFor(currencyLevelingPlanFor(levelingPolicy(), in), "USD"); b != nil {
		t.Fatalf("plan with unknown EUR commitments = %v", levelingLegs(b))
	}
}

// An ordinary rate move keeps the quantity, so the row, its revision and a
// review in progress stay put.
func TestCurrencyLevelingQuantityIsStableAcrossRateMoves(t *testing.T) {
	quantity := func(in currencyLevelingInput) int {
		t.Helper()
		b := levelingBundleFor(currencyLevelingPlanFor(levelingPolicy(), in), "USD")
		if b == nil {
			t.Fatal("no USD bundle")
		}
		return b.legs[0].quantity
	}
	first := quantity(levelingInput(-20000, 60000))
	key := currencyLevelingIdentity("USD", "EUR.USD", rpc.OrderActionSell)
	moved := levelingInput(-20000, 60000)
	moved.Ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: -20000, ExchangeRate: 1 / 1.1730}
	moved.Previous = map[string]int{key: first}
	if second := quantity(moved); second != first {
		t.Fatalf("quantity moved from %d to %d on a 30-pip move", first, second)
	}
	// A move that takes the old quantity out of the safe band re-plans.
	far := levelingInput(-20000, 60000)
	far.Ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: -20000, ExchangeRate: 1 / 1.2000}
	far.Previous = map[string]int{key: first}
	if third := quantity(far); third == first {
		t.Fatalf("quantity %d kept although it leaves the band at 1.20", third)
	}
}

// The engine builds leveling's rows and status from the account ledger, the
// open-order list and the journal; counts and the snapshot clone carry them.
func TestCurrencyLevelingEngineRowsCountsAndClone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 5, 14, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	if rows, st := (&proposalEngine{}).currencyLevelingProposals(ctx, defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, nil, rpc.TradeProposalSourceFingerprints{}, scope, now); rows != nil || st != nil {
		t.Fatal("the written table, off, generated leveling output")
	}
	policy := defaultProtectionPolicy()
	policy.Cash.Leveling = levelingPolicy()
	rows, st := (&proposalEngine{}).currencyLevelingProposals(ctx, policy, rpc.ProtectionPolicyStatus{}, nil, rpc.TradeProposalSourceFingerprints{}, scope, now)
	if rows != nil || st == nil || st.Reason == "" || st.Rows != 0 {
		t.Fatalf("no account = %+v rows %v", st, rows)
	}
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: new(1e6)})
	srv.now = func() time.Time { return now }
	var orders []ibkrlib.OrderLifecycleEvent
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: now, Orders: orders}, scope, nil
	}
	resolves, resolveErr := 0, error(nil)
	srv.orderContractResolverForTest = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.ContractParams, error) {
		resolves++
		c.ConID, c.MinTick = 12087792, 0.00005
		return c, resolveErr
	}
	rates := func(time.Time) (map[string]currencyLevelingRate, string, string) {
		return levelingRates(), "2026-10-05", ""
	}
	engine := &proposalEngine{server: srv, now: func() time.Time { return now }, queued: &queuedAuthStore{}, levelingRatesForTest: rates}
	acct := &rpc.AccountResult{AccountID: scope.Account, BaseCurrency: "EUR",
		CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "USD", CashCcy: -20000, CashObserved: true, ExchangeRate: 1 / 1.17}},
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashCcy: 60000, CashObserved: true, ExchangeRate: 1},
		Authority: &rpc.AccountDataAuthority{Scope: rpc.AccountDataScope{AccountID: scope.Account, AccountMode: scope.Mode}, Availability: rpc.AccountDataAvailable,
			Freshness: rpc.AccountDataFreshnessCurrent, AsOf: now, Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}},
	}
	rows, st = engine.currencyLevelingProposals(ctx, policy, rpc.ProtectionPolicyStatus{}, acct, rpc.TradeProposalSourceFingerprints{}, scope, now)
	if len(rows) != 1 || st == nil || st.Rows != 1 || rows[0].Quantity != 17219 || len(rows[0].Blockers) != 0 || rows[0].Contract.ConID != 12087792 || st.OrderCapBase == nil {
		t.Fatalf("rows = %+v status %+v", rows, st)
	}
	if len(st.Bundles) != 1 || st.Bundles[0].Currency != "USD" || len(st.Bundles[0].Keys) != 1 || st.Bundles[0].Keys[0] != rows[0].Key ||
		rows[0].CurrencyLeveling.BundleID == "" || st.Bundles[0].ID != rows[0].CurrencyLeveling.BundleID || st.RatesThrough != "2026-10-05" {
		t.Fatalf("bundles = %+v, want one USD bundle naming the row", st.Bundles)
	}
	rows[0].Revision = "rev-1"
	currencyLevelingBundleRevisions(st, rows)
	revision := st.Bundles[0].Revision
	rows[0].Revision = "rev-2"
	if currencyLevelingBundleRevisions(st, rows); st.Bundles[0].Revision == revision || revision == "" {
		t.Fatal("the bundle revision does not bind its rows' revisions")
	}
	if n := currencyLevelingCounts(append(rows, rpc.TradeProposal{Bucket: rpc.TradeProposalBucketTrailingStop})); n != 1 {
		t.Fatalf("count = %d", n)
	}
	snap := rpc.TradeProposalSnapshot{Proposals: rows, CurrencyLeveling: st}
	copied := cloneProposalSnapshot(snap)
	copied.Proposals[0].CurrencyLeveling.Cash = 0
	*copied.CurrencyLeveling.Currencies[1].Cash = 0
	if snap.Proposals[0].CurrencyLeveling.Cash != -20000 || *snap.CurrencyLeveling.Currencies[1].Cash != -20000 {
		t.Fatal("the snapshot clone shares leveling state")
	}
	// The pair resolves once per daemon: a second generation reads the cache.
	if rows, _ = engine.currencyLevelingProposals(ctx, policy, rpc.ProtectionPolicyStatus{}, acct, rpc.TradeProposalSourceFingerprints{}, scope, now); len(rows) != 1 || resolves != 1 {
		t.Fatalf("second generation: %d rows, %d resolutions", len(rows), resolves)
	}
	// A pair that does not resolve names no exact order, so it holds.
	cold := &proposalEngine{server: srv, now: func() time.Time { return now }, queued: &queuedAuthStore{}, levelingRatesForTest: rates}
	resolveErr = errors.New("no security definition")
	rows, st = cold.currencyLevelingProposals(ctx, policy, rpc.ProtectionPolicyStatus{}, acct, rpc.TradeProposalSourceFingerprints{}, scope, now)
	if len(rows) != 0 || st.Currencies[1].State != rpc.CurrencyLevelingStateHold || !strings.Contains(st.Currencies[1].Reason, "cannot be resolved") {
		t.Fatalf("unresolved pair: rows %v status %+v", rows, st.Currencies)
	}
	resolveErr = nil
	// A conversion working at the broker, from any client, holds the row.
	orders = []ibkrlib.OrderLifecycleEvent{{Type: ibkrlib.OrderLifecycleEventOpenOrder, SecType: "CASH", LocalSymbol: "EUR.USD", Action: "SELL",
		TotalQuantity: 20000, Remaining: 20000, Status: "Submitted", Account: scope.Account}}
	rows, st = engine.currencyLevelingProposals(ctx, policy, rpc.ProtectionPolicyStatus{}, acct, rpc.TradeProposalSourceFingerprints{}, scope, now)
	if len(rows) != 0 || st.Currencies[1].State != rpc.CurrencyLevelingStateHold || !strings.Contains(st.Currencies[1].Reason, "already working") {
		t.Fatalf("beside a working conversion: rows %v status %+v", rows, st.Currencies)
	}
}
