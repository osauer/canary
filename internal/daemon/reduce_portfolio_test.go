package daemon

import (
	"math"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// reduceTestPortfolio wraps stocks/options in a PositionsResult with a USD
// base currency portfolio aggregate, so every fixture position priced in USD
// converts at rate=1 without needing an explicit FXRate.
func reduceTestPortfolio(stocks, options []rpc.PositionView) *rpc.PositionsResult {
	return &rpc.PositionsResult{
		Stocks:    stocks,
		Options:   options,
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "USD"},
	}
}

func reduceFindCandidate(cands []reduceSweepCandidate, conID int) (reduceSweepCandidate, bool) {
	for _, c := range cands {
		if c.row.ConID == conID {
			return c, true
		}
	}
	return reduceSweepCandidate{}, false
}

func approxEqual(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

// TestReduceSweepProtectsLowDeltaLongFromForcedTrim is the "do not force a
// sale to hit target" invariant: a tiny-delta long option's pro-rata dollar
// share floors below one contract and is omitted entirely, while a same-sign
// stock position with real weight gets a real, non-zero quantity.
func TestReduceSweepProtectsLowDeltaLongFromForcedTrim(t *testing.T) {
	delta := 0.02
	underlying := 500.0
	pos := reduceTestPortfolio(
		[]rpc.PositionView{
			{Symbol: "AAPL", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 1000, Mark: 50},
		},
		[]rpc.PositionView{
			{Symbol: "LOTTO", SecType: "OPTION", ConID: 2, Currency: "USD", Quantity: 2, Right: "C", Delta: &delta, Underlying: &underlying, Multiplier: 100},
		},
	)
	cands, netDelta, netComplete, target, blockers := reduceSweepCandidates(pos, 25)
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers: %+v", blockers)
	}
	if !netComplete {
		t.Fatalf("net should be complete: both rows have computable delta")
	}
	if !approxEqual(netDelta, 52000, 0.01) {
		t.Fatalf("netDelta=%v, want 52000", netDelta)
	}
	if !approxEqual(target, 13000, 0.01) {
		t.Fatalf("target=%v, want 13000 (25%% of 52000)", target)
	}
	stock, ok := reduceFindCandidate(cands, 1)
	if !ok || stock.qty != 250 {
		t.Fatalf("AAPL candidate=%+v ok=%v, want qty=250", stock, ok)
	}
	if _, ok := reduceFindCandidate(cands, 2); ok {
		t.Fatalf("LOTTO option should be omitted: its pro-rata share (~$500) floors below one contract (~$1000/contract)")
	}
}

// TestReduceSweep100PercentDoesNotFullyCloseWithHedgePresent is the disclosed
// semantic change: 100% targets full neutralization of NET delta using only
// the same-sign eligible book, not "close every eligible position." A
// protective long put offsets part of the net long exposure, so the
// contributing stock is not closed in full even at percent=100.
func TestReduceSweep100PercentDoesNotFullyCloseWithHedgePresent(t *testing.T) {
	hedgeDelta := -0.5
	hedgeUnderlying := 100.0
	pos := reduceTestPortfolio(
		[]rpc.PositionView{
			{Symbol: "AAPL", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 1000, Mark: 50},
		},
		[]rpc.PositionView{
			// Long put: positive Quantity (in reduceEligible scope) but
			// negative Delta — a protective hedge against the long stock.
			{Symbol: "SPY", SecType: "OPTION", ConID: 2, Currency: "USD", Quantity: 2, Right: "P", Delta: &hedgeDelta, Underlying: &hedgeUnderlying, Multiplier: 100},
		},
	)
	cands, netDelta, _, target, blockers := reduceSweepCandidates(pos, 100)
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers: %+v", blockers)
	}
	if !approxEqual(netDelta, 40000, 0.01) { // 50000 (AAPL) - 10000 (hedge)
		t.Fatalf("netDelta=%v, want 40000", netDelta)
	}
	if !approxEqual(target, 40000, 0.01) {
		t.Fatalf("target=%v, want 40000 (100%% of net 40000)", target)
	}
	if _, ok := reduceFindCandidate(cands, 2); ok {
		t.Fatalf("the long put hedge must never be selected, at any percent including 100")
	}
	stock, ok := reduceFindCandidate(cands, 1)
	if !ok {
		t.Fatalf("AAPL should be a candidate")
	}
	if stock.qty != 800 {
		t.Fatalf("AAPL qty=%d, want 800 — NOT the full 1000 held, because the hedge already absorbs part of net risk", stock.qty)
	}
	if stock.qty >= 1000 {
		t.Fatalf("100%% must not mean \"close everything eligible\" when a hedge offsets net exposure")
	}
}

// TestReduceSweepHedgeExclusionIsSignRelativeToNet proves the exclusion
// mechanism is relative to the book's actual net direction, not a relabeled
// hardcoded "puts/short calls/short stock" list: a long CALL is excluded
// when the book is net SHORT, the mirror image of the long-put-vs-net-long
// case above. The old isProtectiveShort-based flag could never produce this
// case (it always treats a long call as bullish, never as a hedge).
func TestReduceSweepHedgeExclusionIsSignRelativeToNet(t *testing.T) {
	hedgeDelta := 0.6
	hedgeUnderlying := 150.0
	pos := reduceTestPortfolio(
		[]rpc.PositionView{
			// Short stock: the dominant net-short contributor, and itself an
			// in-scope, same-sign candidate (stocks are eligible at any sign).
			{Symbol: "TSLA", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: -500, Mark: 100},
		},
		[]rpc.PositionView{
			// Long call: positive delta, offsetting part of the net short —
			// a hedge against a net-short book.
			{Symbol: "AAPL", SecType: "OPTION", ConID: 2, Currency: "USD", Quantity: 3, Right: "C", Delta: &hedgeDelta, Underlying: &hedgeUnderlying, Multiplier: 100},
		},
	)
	cands, netDelta, _, _, blockers := reduceSweepCandidates(pos, 50)
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers: %+v", blockers)
	}
	if netDelta >= 0 {
		t.Fatalf("netDelta=%v, want net short (negative)", netDelta)
	}
	if _, ok := reduceFindCandidate(cands, 2); ok {
		t.Fatalf("the long call must be excluded as a hedge against the net-short book")
	}
	if _, ok := reduceFindCandidate(cands, 1); !ok {
		t.Fatalf("the short stock driving net exposure should be a candidate")
	}
}

// TestReduceSweepBasisBlind asserts cost-basis/P&L fields never influence
// candidate selection, order, or sizing — only sizing/Greeks/quantity do.
func TestReduceSweepBasisBlind(t *testing.T) {
	delta := 0.7
	underlying := 200.0
	build := func(avgCost, unrealized float64) *rpc.PositionsResult {
		return reduceTestPortfolio(
			[]rpc.PositionView{
				{Symbol: "AAPL", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 1000, Mark: 50, AvgCost: avgCost, UnrealizedPnL: unrealized},
			},
			[]rpc.PositionView{
				{Symbol: "MSFT", SecType: "OPTION", ConID: 2, Currency: "USD", Quantity: 5, Right: "C", Delta: &delta, Underlying: &underlying, Multiplier: 100, AvgCost: avgCost * 2, UnrealizedPnL: -unrealized},
			},
		)
	}
	deepUnderwater := build(500, -400000) // wildly underwater cost basis
	atCost := build(50, 0)                // breakeven cost basis

	for _, percent := range []int{25, 50, 100} {
		candsA, netA, completeA, targetA, blockersA := reduceSweepCandidates(deepUnderwater, percent)
		candsB, netB, completeB, targetB, blockersB := reduceSweepCandidates(atCost, percent)
		if len(blockersA) != len(blockersB) || netA != netB || completeA != completeB || targetA != targetB {
			t.Fatalf("percent=%d: basket-level outputs diverged on basis alone: A(net=%v complete=%v target=%v blockers=%v) B(net=%v complete=%v target=%v blockers=%v)",
				percent, netA, completeA, targetA, blockersA, netB, completeB, targetB, blockersB)
		}
		if len(candsA) != len(candsB) {
			t.Fatalf("percent=%d: candidate count diverged on basis alone: %d vs %d", percent, len(candsA), len(candsB))
		}
		for i := range candsA {
			if candsA[i].row.ConID != candsB[i].row.ConID || candsA[i].qty != candsB[i].qty || candsA[i].allocatedDollars != candsB[i].allocatedDollars {
				t.Fatalf("percent=%d: candidate[%d] diverged on basis alone: %+v vs %+v", percent, i, candsA[i], candsB[i])
			}
		}
	}
}

// TestReduceSweepNetDeltaImmaterialBlocks asserts the "no meaningful
// direction" edge case never picks an arbitrary side to trim.
func TestReduceSweepNetDeltaImmaterialBlocks(t *testing.T) {
	pos := reduceTestPortfolio(
		[]rpc.PositionView{
			{Symbol: "AAPL", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 100, Mark: 10}, // +1000
			{Symbol: "TSLA", SecType: "STOCK", ConID: 2, Currency: "USD", Quantity: -95, Mark: 10}, // -950
		},
		nil,
	)
	cands, netDelta, _, _, blockers := reduceSweepCandidates(pos, 50)
	if len(blockers) != 1 || blockers[0].Code != "net_delta_immaterial" {
		t.Fatalf("blockers=%+v, want a single net_delta_immaterial blocker", blockers)
	}
	if len(cands) != 0 {
		t.Fatalf("cands=%+v, want none enumerated when net is immaterial", cands)
	}
	if !approxEqual(netDelta, 50, 0.01) {
		t.Fatalf("netDelta=%v, want 50", netDelta)
	}
}

// TestReduceSweepShortfallCapsAndDiscloses covers eligible same-sign supply
// being less than the target: every eligible candidate closes in full, no
// error, and the shortfall is disclosed via the returned target vs. what the
// candidates can actually deliver.
func TestReduceSweepShortfallCapsAndDiscloses(t *testing.T) {
	shortCallDelta := 0.5
	shortCallUnderlying := 50.0
	pos := reduceTestPortfolio(
		[]rpc.PositionView{
			// Small in-scope same-sign contributor.
			{Symbol: "TSLA", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: -100, Mark: 20}, // -2000
		},
		[]rpc.PositionView{
			// Short call: out of reduceEligible scope (only long options are
			// eligible) but its delta still counts toward net.
			{Symbol: "AAPL", SecType: "OPTION", ConID: 2, Currency: "USD", Quantity: -10, Right: "C", Delta: &shortCallDelta, Underlying: &shortCallUnderlying, Multiplier: 100}, // -25000
		},
	)
	cands, netDelta, _, target, blockers := reduceSweepCandidates(pos, 100)
	if len(blockers) > 0 {
		t.Fatalf("a supply shortfall must not be an error: %+v", blockers)
	}
	if !approxEqual(netDelta, -27000, 0.01) {
		t.Fatalf("netDelta=%v, want -27000", netDelta)
	}
	if !approxEqual(target, 27000, 0.01) {
		t.Fatalf("target=%v, want 27000", target)
	}
	tsla, ok := reduceFindCandidate(cands, 1)
	if !ok {
		t.Fatalf("TSLA should be a candidate")
	}
	if tsla.qty != 100 {
		t.Fatalf("TSLA qty=%d, want 100 (its full held quantity — shortfall closes available supply in full)", tsla.qty)
	}
	if !approxEqual(tsla.allocatedDollars, 2000, 0.01) {
		t.Fatalf("TSLA allocatedDollars=%v, want 2000 (capped at its own contribution, not the larger target share)", tsla.allocatedDollars)
	}
	if _, ok := reduceFindCandidate(cands, 2); ok {
		t.Fatalf("the short call is out of reduceEligible scope and must never become a candidate")
	}
}

// TestReduceSweepAllocatedDollarsReflectsFlooredQty asserts the disclosed
// RiskContributionCut (allocatedDollars) tracks what the floored integer
// order quantity actually removes (qty*perUnit), not the pre-floor pro-rata
// target — the achieved-risk figure must never overstate what the order
// placed. Here the pro-rata share is $12,800 at $1,000/contract, which
// floors the order to 12 contracts ($12,000); the old code disclosed the
// pre-floor $12,800 even though only $12,000 of risk is actually removed.
func TestReduceSweepAllocatedDollarsReflectsFlooredQty(t *testing.T) {
	delta := 1.0
	underlying := 100.0
	pos := reduceTestPortfolio(
		nil,
		[]rpc.PositionView{
			{Symbol: "DEEP", SecType: "OPTION", ConID: 1, Currency: "USD", Quantity: 20, Right: "C", Delta: &delta, Underlying: &underlying, Multiplier: 10},
		},
	)
	cands, netDelta, _, target, blockers := reduceSweepCandidates(pos, 64)
	if len(blockers) > 0 {
		t.Fatalf("unexpected blockers: %+v", blockers)
	}
	if !approxEqual(netDelta, 20000, 0.01) {
		t.Fatalf("netDelta=%v, want 20000 (20 contracts * 1.0 delta * 100 spot * 10 multiplier)", netDelta)
	}
	if !approxEqual(target, 12800, 0.01) {
		t.Fatalf("target=%v, want 12800 (64%% of 20000)", target)
	}
	c, ok := reduceFindCandidate(cands, 1)
	if !ok {
		t.Fatalf("DEEP should be a candidate")
	}
	if c.qty != 12 {
		t.Fatalf("qty=%d, want 12 (floor(12800/1000))", c.qty)
	}
	if !approxEqual(c.allocatedDollars, 12000, 0.01) {
		t.Fatalf("allocatedDollars=%v, want 12000 (qty*perUnit = 12*1000), not the pre-floor 12800 target share", c.allocatedDollars)
	}
}

// TestReduceSweepDeltaUnavailableDisclosed asserts a reduceEligible row with
// no computable delta surfaces as a disclosed, zero-quantity candidate rather
// than being silently dropped (or, worse, silently mis-sized).
func TestReduceSweepDeltaUnavailableDisclosed(t *testing.T) {
	pos := reduceTestPortfolio(
		[]rpc.PositionView{
			{Symbol: "AAPL", SecType: "STOCK", ConID: 1, Currency: "USD", Quantity: 1000, Mark: 50},
		},
		[]rpc.PositionView{
			// In scope (long option) but Delta is nil — Greeks unavailable.
			{Symbol: "MSFT", SecType: "OPTION", ConID: 2, Currency: "USD", Quantity: 5, Right: "C"},
		},
	)
	cands, _, netComplete, _, blockers := reduceSweepCandidates(pos, 25)
	if len(blockers) > 0 {
		t.Fatalf("unexpected basket-level blockers: %+v", blockers)
	}
	if netComplete {
		t.Fatalf("net should be incomplete: MSFT had no computable delta")
	}
	msft, ok := reduceFindCandidate(cands, 2)
	if !ok {
		t.Fatalf("MSFT should be disclosed as a candidate, not silently dropped")
	}
	if msft.qty != 0 {
		t.Fatalf("MSFT qty=%d, want 0 — never sized without a computable delta", msft.qty)
	}
	if len(msft.blockers) == 0 || msft.blockers[0].Code != "delta_unavailable" {
		t.Fatalf("MSFT blockers=%+v, want delta_unavailable", msft.blockers)
	}
	if _, ok := reduceFindCandidate(cands, 1); !ok {
		t.Fatalf("AAPL should still be sized normally despite MSFT's missing delta")
	}
}

func TestAggregateBasketPreview(t *testing.T) {
	nb := 100.0
	res := &rpc.TradeProposalReducePortfolioResult{BaseCurrency: "EUR", TargetDollarDelta: 200}
	res.Legs = []rpc.TradeProposalReduceLeg{
		{Symbol: "AMD", SubmitEligible: true, NotionalBase: &nb, RiskContributionCut: 100},
		{Symbol: "IBM", SubmitEligible: true, RiskContributionCut: 50}, // eligible but no base notional → FX incomplete
		{Symbol: "NOW", SubmitEligible: false, Blockers: []rpc.TradingBlocker{{Code: "preview_failed"}}},
	}
	aggregateBasket(res, false)
	if res.LegCount != 3 || res.EligibleCount != 2 || res.BlockedCount != 1 {
		t.Fatalf("counts wrong: %+v", res)
	}
	if !res.FXIncomplete {
		t.Fatalf("FXIncomplete should be true (IBM lacked base notional)")
	}
	if res.TotalNotional != 100 {
		t.Fatalf("TotalNotional=%v, want 100", res.TotalNotional)
	}
	if res.Accepted {
		t.Fatalf("Accepted must be false when a leg is blocked")
	}
	if res.AchievedDollarDelta != 150 {
		t.Fatalf("AchievedDollarDelta=%v, want 150 (100+50 over eligible legs)", res.AchievedDollarDelta)
	}
	if res.AchievedPctOfTarget == nil || !approxEqual(*res.AchievedPctOfTarget, 75, 0.01) {
		t.Fatalf("AchievedPctOfTarget=%v, want 75", res.AchievedPctOfTarget)
	}
}

func TestAggregateBasketSubmitAccepted(t *testing.T) {
	nb := 50.0
	res := &rpc.TradeProposalReducePortfolioResult{BaseCurrency: "EUR", TargetDollarDelta: 50}
	res.Legs = []rpc.TradeProposalReduceLeg{
		{Symbol: "AMD", Placed: true, NotionalBase: &nb, RiskContributionCut: 50},
	}
	aggregateBasket(res, true)
	if res.EligibleCount != 1 || res.BlockedCount != 0 {
		t.Fatalf("counts wrong: %+v", res)
	}
	if !res.Accepted {
		t.Fatalf("Accepted should be true: one placed, zero blocked")
	}
	if res.FXIncomplete || res.TotalNotional != 50 {
		t.Fatalf("FX wrong: incomplete=%v total=%v", res.FXIncomplete, res.TotalNotional)
	}
	if res.AchievedPctOfTarget == nil || !approxEqual(*res.AchievedPctOfTarget, 100, 0.01) {
		t.Fatalf("AchievedPctOfTarget=%v, want 100", res.AchievedPctOfTarget)
	}
}

func TestReduceBasketDedupe(t *testing.T) {
	cur := time.Unix(1000, 0).UTC()
	s := &Server{now: func() time.Time { return cur }}
	res := &rpc.TradeProposalReducePortfolioResult{EligibleCount: 3}

	if _, ok := s.reduceBasketReplay("ref1"); ok {
		t.Fatal("empty cache should miss")
	}
	s.reduceBasketStore("ref1", res)
	got, ok := s.reduceBasketReplay("ref1")
	if !ok || !got.Replayed || got.EligibleCount != 3 {
		t.Fatalf("replay should hit with Replayed=true: ok=%v got=%+v", ok, got)
	}
	// The stored result must not be mutated by the replay clone.
	if res.Replayed {
		t.Fatal("replay must not mutate the cached result")
	}
	// Empty ref never dedupes.
	if _, ok := s.reduceBasketReplay(""); ok {
		t.Fatal("empty ref must miss")
	}
	s.reduceBasketStore("", res) // no-op, must not panic
	// TTL expiry sweeps the entry.
	cur = cur.Add(reduceBasketDedupeTTL + time.Second)
	if _, ok := s.reduceBasketReplay("ref1"); ok {
		t.Fatal("expired entry should be swept")
	}
}
