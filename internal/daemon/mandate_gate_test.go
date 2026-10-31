package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

// mandateTestNow is a Wednesday during the US session, so the last completed
// session is the Tuesday before it.
var mandateTestNow = time.Date(2026, 10, 14, 15, 0, 0, 0, time.UTC)

func testMandateConstitution() *risk.Constitution {
	c := testConstitutionV3()
	c.SchemaVersion = 2
	c.OrderLimits = testOrderLimitsTable(10000)
	c.Mandate = &risk.ConstitutionMandate{MaxLossPct: new(30.0), VolTargetPct: new(15.0), TaxReserveBase: new(70000.0),
		StartedAt: "2026-10-01", GapShockPct: new(25.0), GapRoomPct: new(33.3),
		Sleeves: &risk.ConstitutionMandateSleeves{OwnerVolPct: new(7.0), CoreVolPct: new(6.0), OverlayVolPct: new(4.0)}}
	return c
}

// mandateTestServer is a preview server whose constitution carries the
// mandate, whose capital store observed one fresh equity reading and whose
// positions and daily history the test supplies.
func mandateTestServer(t *testing.T, c *risk.Constitution) (*Server, *riskCapitalStore) {
	t.Helper()
	srv, st, _, _ := brakeTestServer(t)
	srv.now = func() time.Time { return mandateTestNow }
	// Option admission requires a complete current all-client inventory.
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkr.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkr.OpenOrderSnapshot{Complete: true, AsOf: srv.orderNow()}, srv.currentBrokerStateScope(), nil
	}
	// The broker's independent receipt agrees with the fixture book unless a
	// test overrides it to exercise fills, missing scope or other drift.
	srv.reduceFillSnapshotForTest = func(ctx context.Context, _ time.Duration) ([]*ibkr.RawPosition, error) {
		if srv.mandatePositionsForTest == nil {
			return nil, errors.New("fixture positions unavailable")
		}
		positions, err := srv.mandatePositionsForTest(ctx)
		if err != nil || positions == nil {
			return nil, err
		}
		var receipt []*ibkr.RawPosition
		for _, rows := range [][]rpc.PositionView{positions.Stocks, positions.Options} {
			for _, row := range rows {
				receipt = append(receipt, &ibkr.RawPosition{Account: srv.currentBrokerStateScope().Account,
					Contract: ibkr.Contract{ConID: row.ConID, Symbol: row.Symbol, SecType: positionWireSecType(row.SecType), Currency: row.Currency, Right: row.Right, Multiplier: row.Multiplier}, Position: row.Quantity})
			}
		}
		return receipt, nil
	}
	st.now = srv.now
	srv.riskPolicies.active = c
	st.Observe(300000, mandateTestNow.Add(-2*time.Minute), c, testLiveObserveScope, true)
	// The premium limits are shares of the account's net liquidation value,
	// read as the order limits read it.
	srv.recordOrderLimitsNLV(orderLimitsTestAccount(srv.currentBrokerStateScope().Account, 300000, "EUR", mandateTestNow.Add(-2*time.Minute)))
	return srv, st
}

// mandateTestDates walks weekdays back from the last completed US session.
func mandateTestDates(n int) []string {
	last, _, ok := mandateLastSessions(mandateTestNow)
	if !ok {
		panic("no completed session")
	}
	day, _ := time.Parse(time.DateOnly, last)
	out := make([]string, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = day.Format(time.DateOnly)
		day = day.AddDate(0, 0, -1)
		for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			day = day.AddDate(0, 0, -1)
		}
	}
	return out
}

// mandateTestSeries is a seeded random walk with the given annualised
// volatility ending on the last completed session; lag drops that many
// newest dates.
func mandateTestSeries(key string, vol float64, seed uint64, lag int) *mandateHistorySeries {
	dates := mandateTestDates(300)
	dates = dates[:len(dates)-lag]
	r := rand.New(rand.NewPCG(seed, seed+1))
	s := &mandateHistorySeries{Version: mandateHistoryDocVer, Key: key, Dates: dates, FetchedAt: mandateTestNow.Add(-time.Hour), Source: "test"}
	switch key {
	case "u:SPY":
		s.Contract = mandateIndexHistoryNeed().Contract
	case "u:SYNA":
		s.Contract = rpc.ContractParams{ConID: 9001, Symbol: "SYNA", SecType: "STK", Currency: "USD", Exchange: "SMART"}
	case "u:SYNC":
		s.Contract = rpc.ContractParams{ConID: 9100, Symbol: "SYNC", SecType: "STK", Currency: "EUR", Exchange: "SMART"}
	case "u:SYND":
		s.Contract = rpc.ContractParams{ConID: 9100, Symbol: "SYND", SecType: "STK", Currency: "CHF", Exchange: "SMART"}
	case "fx:CHF":
		s.Contract = rpc.ContractParams{Symbol: "EUR", SecType: "CASH", Currency: "CHF", Exchange: "IDEALPRO"}
	case "u:SXR8":
		s.Contract = rpc.ContractParams{Symbol: "SXR8", SecType: "STK", Currency: "EUR", Exchange: "SMART"}
	case "u:SYNB":
		s.Contract = rpc.ContractParams{ConID: 9002, Symbol: "SYNB", SecType: "STK", Currency: "EUR", Exchange: "SMART"}
	case "fx:USD":
		s.Contract = rpc.ContractParams{Symbol: "EUR", SecType: "CASH", Currency: "USD", Exchange: "IDEALPRO"}
	}
	price := 100.0
	for range dates {
		s.Closes = append(s.Closes, price)
		price *= math.Exp(vol / math.Sqrt(252) * r.NormFloat64())
	}
	return s
}

func seedMandateHistory(t *testing.T, srv *Server, lag int) {
	t.Helper()
	for _, s := range []*mandateHistorySeries{
		mandateTestSeries("u:SPY", 0.16, 1, 0), mandateTestSeries("u:SYNA", 0.30, 2, lag), mandateTestSeries("u:SYNB", 0.40, 3, 0), mandateTestSeries("fx:USD", 0.08, 4, 0),
	} {
		if err := srv.saveMandateSeries(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
}

func mandateTestPositions(synaShares float64) *rpc.PositionsResult {
	return mandateTestGrouped(mandateTestUngrouped(synaShares))
}

// mandateTestGrouped adds the by-underlying groups the positions handler
// serves, which the exposure view and the premium measure read.
func mandateTestGrouped(pos *rpc.PositionsResult) *rpc.PositionsResult {
	pos.ByUnderlying = groupByUnderlying(pos.Stocks, pos.Options, "EUR", nil)
	return pos
}

func mandateTestUngrouped(synaShares float64) *rpc.PositionsResult {
	iv := 0.35
	fx := 0.9
	return &rpc.PositionsResult{
		AsOf: mandateTestNow.Add(-time.Minute),
		Authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent,
			Scope: rpc.AccountDataScope{AccountID: "DU1234567", AccountMode: rpc.AccountModePaper}},
		Portfolio: &rpc.PositionsPortfolio{BaseCurrency: "EUR"},
		Stocks: []rpc.PositionView{
			{Symbol: "SYNA", SecType: rpc.SecTypeStock, ConID: 9001, Currency: "USD", Quantity: synaShares, Multiplier: 1, Mark: 100, FXRate: &fx},
			{Symbol: "SYNB", SecType: rpc.SecTypeStock, ConID: 9002, Currency: "EUR", Quantity: 100, Multiplier: 1, Mark: 80},
			{Symbol: "SYNT", SecType: "BILL", ConID: 9003, Currency: "EUR", Quantity: 10000, Multiplier: 1, Mark: 99},
		},
		Options: []rpc.PositionView{
			{Symbol: "SYNA", SecType: rpc.SecTypeOption, ConID: 9004, Currency: "USD", Quantity: -2, Multiplier: 100, Mark: 3, Expiry: "20270115", Strike: 110, Right: "C", IV: &iv, Underlying: new(100.0), FXRate: &fx},
		},
	}
}

// mandateTestPositionsOverLoss is the small stock fixture with bought calls
// on two names, each line priced at its mark with the price paid beside it:
// 18 SYNB calls (36,000 EUR of value, 32,400 paid) and 18 SYNA calls
// (32,400 EUR of value, 35,640 EUR paid, in USD at 0.9), 71,640 EUR of
// premium at risk against the 69,000 EUR loss room (30% of the 230,000 EUR
// budget base at full room). With the gap room at 100% no name breaches
// its gap cap, so the loss measure alone reads reduce (spec TM-04.12).
func mandateTestPositionsOverLoss() *rpc.PositionsResult {
	pos := mandateTestUngrouped(100)
	iv := 0.3
	fx := 0.9
	bmv := 18 * 100 * 20.0
	amv := 18 * 100 * 20.0 * fx
	pos.Options = append(pos.Options,
		rpc.PositionView{Symbol: "SYNB", SecType: rpc.SecTypeOption, ConID: 9005, Currency: "EUR", Quantity: 18, Multiplier: 100, Mark: 20, AvgCost: 1800,
			MarketValue: bmv, MarketValueBase: &bmv, Expiry: "20270115", Strike: 80, Right: "C", IV: &iv, Underlying: new(80.0),
			Delta: new(0.6), Gamma: new(0.02), Theta: new(-0.05), Vega: new(0.3)},
		rpc.PositionView{Symbol: "SYNA", SecType: rpc.SecTypeOption, ConID: 9007, Currency: "USD", Quantity: 18, Multiplier: 100, Mark: 20, AvgCost: 2200, FXRate: &fx,
			MarketValue: 18 * 100 * 20, MarketValueBase: &amv, Expiry: "20270115", Strike: 100, Right: "C", IV: &iv, Underlying: new(100.0),
			Delta: new(0.6), Gamma: new(0.02), Theta: new(-0.05), Vega: new(0.3)})
	return mandateTestGrouped(pos)
}

// mandateTestOverLossServer is a server whose book reads reduce on the loss
// measure alone: the premium at risk over the loss room, no gap breach.
func mandateTestOverLossServer(t *testing.T) (*Server, *rpc.MandateRiskResult) {
	t.Helper()
	c := testMandateConstitution()
	c.Mandate.GapRoomPct = new(100.0)
	srv, _ := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositionsOverLoss(), nil }
	res := srv.evaluateMandate(t.Context(), false)
	v := res.Verdict
	if !res.Active || v == nil || v.Status != mandate.StatusReduce || v.Loss == nil || v.Loss.TotalBase <= v.Loss.RoomBase || v.Stresses == nil || len(v.Stresses.GapBreaches) != 0 {
		t.Fatalf("fixture: %+v", v)
	}
	return srv, res
}

func mandateBuy(shares int) rpc.OrderDraft {
	return rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: shares, LimitPrice: 100, Contract: rpc.ContractParams{ConID: 9001, Symbol: "SYNA", SecType: "STK", Currency: "USD"}}
}

var mandateOpen = rpc.OrderPositionImpact{Before: 0, After: 100, Effect: rpc.OrderPositionEffectOpen}

// A book whose loss measure is over the loss room (spec TM-04.12; owner,
// 2026-10-27 19:50 CET) refuses a risk-adding order at the preview, at
// admission and at the wire guard with mandate_budget, naming the measure
// and the room; a reduction, a close and a hedge entry pass. The share
// stops the measure reads come from the open orders, which this server
// cannot read, so the verdict says every share line counts as unstopped.
func TestMandateGateRefusesAdditionsOverBudget(t *testing.T) {
	srv, _ := mandateTestOverLossServer(t)
	// This case specifically exercises unavailable broker stop inventory.
	srv.openOrderInventoryForTest = nil
	res := srv.evaluateMandate(t.Context(), false)
	if math.Abs(res.Verdict.Loss.TotalBase-71640) > 1e-6 || math.Abs(res.Verdict.Loss.RoomBase-69000) > 1e-6 || res.Verdict.Loss.PremiumBase != res.Verdict.Loss.TotalBase {
		t.Fatalf("loss %+v, want 71,640 EUR of premium against the 69,000 EUR room", res.Verdict.Loss)
	}
	if res.Gate == nil || !res.Gate.RefusesAdditions || !strings.Contains(res.Gate.Reason, "the loss measure 71,640 EUR (premium at risk 71,640 EUR, stop loss 0 EUR) is over the mandate's loss room 69,000 EUR (104%)") {
		t.Fatalf("gate %+v", res.Gate)
	}
	if nm := res.Verdict.NotModelled; len(nm) != 2 || !strings.Contains(nm[0], "bill") || !strings.Contains(nm[1], "share stops unread") || !strings.Contains(nm[1], "every share line counts as unstopped") {
		t.Fatalf("not modelled %v", res.Verdict.NotModelled)
	}
	if u := res.Verdict.Loss.Unstopped; !slices.Equal(u, []string{"SYNA", "SYNB"}) {
		t.Fatalf("unstopped %v", u)
	}
	if len(res.Evidence.Series) != 4 || res.Evidence.PositionsAsOf.IsZero() || res.Evidence.EquityAsOf.IsZero() {
		t.Fatalf("evidence %+v", res.Evidence)
	}

	srv.orderPreviewQuote = fixedPreviewQuote(99, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 100, rpc.OrderPositionEffectOpen)
	limit := 100.0
	_, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "buy", Quantity: 10, LimitPrice: &limit,
		Contract: rpc.ContractParams{ConID: 9002, Symbol: "SYNB", SecType: "STK", Currency: "EUR"}})
	blockers := previewFailureBlockers(err)
	if err == nil || len(blockers) != 1 || blockers[0].Code != mandateBudgetCode || !strings.Contains(blockers[0].Message, "over the mandate's loss room 69,000 EUR") || !strings.Contains(blockers[0].Message, "stay open") {
		t.Fatalf("buy over the loss room: err %v blockers %+v, want mandate_budget", err, blockers)
	}
	if err := srv.mandateBudgetGateError(mandateBuy(10), mandateOpen); err == nil || !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "Mandate budget") {
		t.Fatalf("admission over budget: %v", err)
	}
	// The wire guard reads the same gate on a risk-bound binding:
	// TestMandateWireGuardRefusesAdditionsOverBudget (trading build).
	for _, effect := range []string{rpc.OrderPositionEffectReduce, rpc.OrderPositionEffectClose} {
		sell := rpc.OrderDraft{Action: rpc.OrderActionSell, Quantity: 50, Contract: rpc.ContractParams{ConID: 9001, Symbol: "SYNA", SecType: "STK", Currency: "USD"}}
		if blocker := srv.mandateBudgetGate(sell, rpc.OrderPositionImpact{Before: 100, After: 50, Effect: effect}); blocker != nil {
			t.Fatalf("%s over budget: %+v, want allowed", effect, blocker)
		}
	}
	hedge := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 1, Contract: rpc.ContractParams{Symbol: srv.rulebookPolicy().HedgeSymbols[0], SecType: "OPT", Right: "P", Currency: "USD"}}
	if blocker := srv.mandateBudgetGate(hedge, mandateOpen); blocker != nil {
		t.Fatalf("hedge put over budget: %+v, want allowed", blocker)
	}
}

// An account read that lands just after a scheduled evaluation read its
// clock is that evaluation's evidence, not an observation from the future:
// the capital is judged once the store holds it, so the verdict is measured
// on that equity, stamped no earlier than it, and counts for the forced
// reduction.
func TestMandateScheduledVerdictCountsAnObservationLandingAsItStarts(t *testing.T) {
	c := testMandateConstitution()
	srv, st := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(3000), nil }
	var mu sync.Mutex
	clock := mandateTestNow
	landed := mandateTestNow.Add(50 * time.Millisecond)
	accountRead := func() {
		mu.Lock()
		clock = landed
		mu.Unlock()
		st.Observe(300000, landed, c, testLiveObserveScope, true)
	}
	srv.now = func() time.Time {
		mu.Lock()
		now, read := clock, accountRead
		accountRead = nil
		mu.Unlock()
		if read != nil {
			read()
		}
		return now
	}
	st.now = srv.now

	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict == nil || res.Verdict.Status != mandate.StatusReduce || !res.Verdict.Capital.Known {
		t.Fatalf("verdict %+v, want reduce on known capital", res.Verdict)
	}
	if !res.Verdict.Capital.EquityAsOf.Equal(landed) || res.AsOf.Before(landed) {
		t.Fatalf("equity as of %s, verdict as of %s; want the observation at %s and a verdict no earlier", res.Verdict.Capital.EquityAsOf, res.AsOf, landed)
	}
	if got := srv.mandateVerdicts.kept().overStreak; got != 1 {
		t.Fatalf("over streak %d, want the scheduled verdict counted", got)
	}
}

// Stale history reads unknown and refuses additions; fresh history with a
// book within budget releases the gate on the next verdict, with no reset.
func TestMandateGateUnknownOnStaleHistoryThenReleasesWithin(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 3)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusUnknown || !strings.Contains(strings.Join(res.Verdict.Reasons, " "), "SYNA is stale") {
		t.Fatalf("stale history: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "cannot be measured") {
		t.Fatalf("buy on stale history: %+v", blocker)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), rpc.OrderPositionImpact{Before: 100, After: 50, Effect: rpc.OrderPositionEffectReduce}); blocker != nil {
		t.Fatalf("reduce on stale history: %+v", blocker)
	}
	seedMandateHistory(t, srv, 0)
	res = srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusWithin || res.Gate.RefusesAdditions {
		t.Fatalf("fresh history: %s %v gate %+v", res.Verdict.Status, res.Verdict.Reasons, res.Gate)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker != nil {
		t.Fatalf("buy within budget: %+v", blocker)
	}
	sum := 0.0
	for _, l := range res.Verdict.Legs {
		sum += l.ContributionPct
	}
	if math.Abs(sum-100) > 1e-6 || len(res.Verdict.Legs) != 3 {
		t.Fatalf("contributions %v sum %g", res.Verdict.Legs, sum)
	}
	// An old verdict cannot be shown current.
	srv.now = func() time.Time { return mandateTestNow.Add(mandateVerdictMaxAge + time.Second) }
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "old") {
		t.Fatalf("buy on an old verdict: %+v", blocker)
	}
}

// Within budget, an order that would take the risk over the budget is
// refused on its own, with the figure it would reach.
func TestMandateGateIncrementalCheckRefusesAnAdditionOverBudget(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker != nil {
		t.Fatalf("small buy: %+v", blocker)
	}
	// A share buy carries no premium and no stop, so the loss measure does
	// not see it; the gap cap bounds it (and the sleeve's allocation).
	blocker := srv.mandateBudgetGate(mandateBuy(5000), mandateOpen)
	if blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "with this order a −25% gap in SYNA would lose") || !strings.Contains(blocker.Message, "over the gap cap") {
		t.Fatalf("large buy: %+v", blocker)
	}
	// A new option is judged by the premium limits (owner, 2026-10-23 18:22
	// CEST): 200 calls at 8 USD are 144,000 EUR of premium, 48% of the
	// 300,000 EUR net liquidation, over the 15% limit for one name.
	call := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 200, LimitPrice: 8, Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "OPT", Currency: "USD", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
	if blocker := srv.mandateBudgetGate(call, mandateOpen); blocker == nil || blocker.Code != rpc.MandatePremiumBlockerCode || !strings.Contains(blocker.Message, "over the 15.0% limit for one name") {
		t.Fatalf("large call buy: %+v", blocker)
	}
	// Ten calls, 7,200 EUR: inside the limit, and the volatility verdict
	// becomes the preview's warning where it would have refused.
	call.Quantity = 10
	if blocker, warning := srv.mandateBudgetJudgement(call, mandateOpen); blocker != nil || warning != nil {
		t.Fatalf("ten calls within the budget: blocker %+v warning %+v", blocker, warning)
	}
	// Without a limit there is no premium to bound the buy: refused.
	unpriced := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 200, Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "OPT", Currency: "USD", Right: "C", Strike: 100, Expiry: "20270115"}}
	if blocker := srv.mandateBudgetGate(unpriced, mandateOpen); blocker == nil || blocker.Code != rpc.MandatePremiumBlockerCode || !strings.Contains(blocker.Message, "no premium to bound its loss") {
		t.Fatalf("unpriced option: %+v", blocker)
	}
	// On an underlying the book does not hold, with no history, the premium
	// limits still judge it; the missing volatility measure is the warning.
	unheld := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 10, LimitPrice: 8, Contract: rpc.ContractParams{Symbol: "SYNZ", SecType: "OPT", Currency: "USD", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
	if blocker, warning := srv.mandateBudgetJudgement(unheld, mandateOpen); blocker != nil || warning == nil || !strings.Contains(warning.Message, "no daily history for SYNZ") {
		t.Fatalf("option on an unheld name without history: blocker %+v warning %+v", blocker, warning)
	}
}

// A first buy of a name the book does not hold is never passed unmeasured:
// without fresh decision-grade history it is refused and the series is
// requested on the background lane; once it has arrived the order is
// measured, admitted within the budget and refused over it. A currency
// without its exchange-rate history is refused the same way. Held names are
// measured as before and request nothing.
func TestMandateGateMeasuresANameTheBookDoesNotHold(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	buy := func(sym, ccy string, shares int) rpc.OrderDraft {
		return rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: shares, LimitPrice: 100, Contract: rpc.ContractParams{ConID: 9100, Symbol: sym, SecType: "STK", Currency: ccy}}
	}
	requested := func() []string {
		var keys []string
		for _, need := range srv.mandateHistoryRequests(mandateTestNow) {
			keys = append(keys, mandateHistoryLogicalKey(need.Key))
		}
		return keys
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker != nil || len(requested()) != 0 {
		t.Fatalf("a held name: %+v, requested %v", blocker, requested())
	}
	blocker := srv.mandateBudgetGate(buy("SYNC", "EUR", 10), mandateOpen)
	if blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "no daily history for SYNC yet; the desk is fetching it") {
		t.Fatalf("an unheld name without history: %+v", blocker)
	}
	if got := requested(); !slices.Equal(got, []string{"u:SYNC"}) {
		t.Fatalf("requested %v", got)
	}
	needs := srv.mandateHistoryNeedsWithRequests(t.Context(), mandateTestPositions(100), "EUR", mandateTestNow)
	if !slices.ContainsFunc(needs, func(n mandateHistoryNeed) bool {
		return mandateHistoryLogicalKey(n.Key) == "u:SYNC" && n.Contract.Symbol == "SYNC" && n.Contract.ConID == 9100
	}) {
		t.Fatalf("the background pass does not read the request: %+v", needs)
	}
	if err := srv.saveMandateSeries(t.Context(), mandateTestSeries("u:SYNC", 0.25, 5, 0)); err != nil {
		t.Fatal(err)
	}
	if blocker := srv.mandateBudgetGate(buy("SYNC", "EUR", 10), mandateOpen); blocker != nil {
		t.Fatalf("an unheld name, history arrived, within budget: %+v", blocker)
	}
	if blocker := srv.mandateBudgetGate(buy("SYNC", "EUR", 20000), mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "with this order a −25% gap in SYNC would lose") {
		t.Fatalf("an unheld name, history arrived, over the gap cap: %+v", blocker)
	}
	// A currency the book holds nothing in: the exchange rate's history too.
	if err := srv.saveMandateSeries(t.Context(), mandateTestSeries("u:SYND", 0.25, 6, 0)); err != nil {
		t.Fatal(err)
	}
	blocker = srv.mandateBudgetGate(buy("SYND", "CHF", 10), mandateOpen)
	if blocker == nil || !strings.Contains(blocker.Message, "no daily history for the EUR/CHF exchange rate yet") || strings.Contains(blocker.Message, "SYND yet") {
		t.Fatalf("a currency without its rate's history: %+v", blocker)
	}
	if got := requested(); !slices.Contains(got, "fx:CHF") {
		t.Fatalf("requested %v", got)
	}
	if err := srv.saveMandateSeries(t.Context(), mandateTestSeries("fx:CHF", 0.06, 7, 0)); err != nil {
		t.Fatal(err)
	}
	if blocker := srv.mandateBudgetGate(buy("SYND", "CHF", 10), mandateOpen); blocker != nil {
		t.Fatalf("a currency, history arrived, within budget: %+v", blocker)
	}
	// A request lapses after a day.
	if got := srv.mandateHistoryRequests(mandateTestNow.Add(25 * time.Hour)); len(got) != 0 {
		t.Fatalf("requests outlived a day: %+v", got)
	}
}

// Once the drawdown reaches the maximum loss the budget is zero and the
// ratio unbounded: mandate.risk still answers at the daemon wire, with the
// ratio null, so readers see the reduce instead of an encoding failure.
func TestMandateRoomZeroVerdictCrossesTheWire(t *testing.T) {
	c := testMandateConstitution()
	srv, st := mandateTestServer(t, c)
	// The high is 300,000 equity (230,000 after the reserve); 220,000 is a
	// market loss of 80,000 the day's P&L explains, past 30% of 230,000.
	observeWithPnL(st, c, 300000, 0, mandateTestNow.Add(-90*time.Second))
	observeWithPnL(st, c, 220000, -80000, mandateTestNow.Add(-time.Minute))
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	res := srv.evaluateMandate(t.Context(), false)
	v := res.Verdict
	if v == nil || v.Status != mandate.StatusReduce || v.Capital.Room != 0 || v.Capital.BudgetAnnualSD != 0 || v.Risk == nil || v.Risk.SDAnnualBase <= 0 || !math.IsInf(v.Risk.RatioToBudget, 1) {
		t.Fatalf("room 0 verdict %+v", v)
	}
	if !res.Gate.RefusesAdditions || !strings.Contains(res.Gate.Reason, "the loss has reached the mandate's maximum") {
		t.Fatalf("gate %+v", res.Gate)
	}

	var out bytes.Buffer
	srv.dispatch(t.Context(), &rpc.Request{ID: "room-0", Method: rpc.MethodMandateRisk, Params: json.RawMessage(`{}`)}, json.NewEncoder(&out), bufio.NewReader(strings.NewReader("")))
	var response rpc.Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("mandate.risk: %s: %v", out.Bytes(), err)
	}
	if !response.Ok || response.Error != nil || !bytes.Contains(response.Result, []byte(`"ratio_to_budget":null`)) {
		t.Fatalf("mandate.risk at room 0: ok %v error %+v", response.Ok, response.Error)
	}
	var got rpc.MandateRiskResult
	if err := json.Unmarshal(response.Result, &got); err != nil {
		t.Fatal(err)
	}
	if got.Verdict == nil || got.Verdict.Status != mandate.StatusReduce || got.Verdict.Risk == nil || !math.IsInf(got.Verdict.Risk.RatioToBudget, 1) ||
		got.Verdict.Risk.SDAnnualBase != v.Risk.SDAnnualBase || got.Gate == nil || got.Gate.Reason != res.Gate.Reason {
		t.Fatalf("served %+v", got)
	}
	// The MCP tool and `canary mandate --json` encode the result again.
	if _, err := json.Marshal(got); err != nil {
		t.Fatalf("re-encode: %v", err)
	}
}

// Without a [mandate] table nothing changes: the gate refuses nothing, the
// verdict reads inactive and the refresh loop reads no history.
func TestMandateAbsentChangesNothing(t *testing.T) {
	srv, st, c, now := brakeTestServer(t)
	st.Observe(260000, now.Add(-2*time.Minute), c, testLiveObserveScope, true)
	fetches := 0
	srv.mandateHistoryFetchForTest = func(context.Context, rpc.MarketHistoryParams, int, time.Time) (*rpc.MarketHistoryResult, error) {
		fetches++
		return nil, errors.New("must not be called")
	}
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(3000), nil }
	srv.refreshMandateHistoryOnce(t.Context())
	srv.refreshMandateVerdict(t.Context())
	if fetches != 0 {
		t.Fatalf("an inactive mandate read history %d times", fetches)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker != nil {
		t.Fatalf("gate without a mandate: %+v", blocker)
	}
	res, err := srv.handleMandateRisk(t.Context(), &rpc.Request{})
	if err != nil || res.Active || res.Verdict != nil || res.Gate.RefusesAdditions || !strings.Contains(res.Message, "no [mandate] table") {
		t.Fatalf("inactive result: %+v err %v", res, err)
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 100, rpc.OrderPositionEffectOpen)
	if _, err := srv.previewOrder(t.Context(), brakeStockBuy()); err != nil {
		t.Fatalf("buy without a mandate: %v", err)
	}
	if st.state.Mandate != nil {
		t.Fatal("an observation without a mandate wrote mandate state")
	}
}

// An incomplete table is active and unknown: the verdict names the keys and
// additions are refused until they are written.
func TestMandateIncompleteTableRefusesAdditions(t *testing.T) {
	c := testMandateConstitution()
	c.Mandate.GapRoomPct = nil
	srv, _ := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusUnknown || !strings.Contains(strings.Join(res.Verdict.Reasons, " "), "mandate.gap_room_pct") || len(res.Policy.Unapproved) != 1 {
		t.Fatalf("incomplete table: %s %v %+v", res.Verdict.Status, res.Verdict.Reasons, res.Policy)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker == nil {
		t.Fatal("incomplete table admitted an addition")
	}
}

// The high-water mark seeds at the first observation on or after the start,
// advances with gains, treats a withdrawal as a lower base and not a
// drawdown, and starts again when started_at changes.
func TestMandateCapitalStateHighWaterMarkAndFlows(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return mandateTestNow }
	c := testMandateConstitution()
	reconcileNow(t, st)
	before := mandateTestNow.Add(-20 * 24 * time.Hour) // before the start: not observed by the mandate
	st.Observe(400000, before, c, testLiveObserveScope, true)
	if st.state.Mandate != nil {
		t.Fatal("an observation before the start seeded the mandate")
	}
	// Each move is the book's own P&L, as the broker reports it.
	observeWithPnL(st, c, 300000, 0, mandateTestNow.Add(-3*24*time.Hour))
	// A new high counts once a reading at least five minutes later
	// confirms it, at the lower of the two.
	observeWithPnL(st, c, 312000, 12000, mandateTestNow.Add(-2*24*time.Hour))
	observeWithPnL(st, c, 310000, 10000, mandateTestNow.Add(-2*24*time.Hour+10*time.Minute))
	observeWithPnL(st, c, 295000, -15000, mandateTestNow.Add(-time.Minute))
	capital, realised := st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow)
	if !capital.Known || capital.HWMRiskBase != 240000 || capital.RiskBase != 225000 || capital.DrawdownBase != 15000 || math.Abs(capital.DrawdownPct-6.25) > 1e-9 {
		t.Fatalf("capital %+v", capital)
	}
	if realised.Samples != 3 || realised.Vol20Pct != nil {
		t.Fatalf("realised %+v", realised)
	}
	// A withdrawal of 50,000 lowers the budget base, not the drawdown.
	if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "withdrawal", AmountBase: 50000}, rpc.OrderOriginHumanTTY, c); err != nil {
		t.Fatal(err)
	}
	observeWithPnL(st, c, 245000, -15000, mandateTestNow)
	capital, _ = st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow)
	if !capital.Known || capital.FlowsSinceStart != -50000 || capital.BudgetBase != 190000 || capital.DrawdownBase != 15000 {
		t.Fatalf("after a withdrawal %+v", capital)
	}
	// A new start date starts the series again from the next observation.
	c.Mandate.StartedAt = "2026-10-14"
	st.now = func() time.Time { return mandateTestNow.Add(time.Minute) }
	st.Observe(245000, mandateTestNow.Add(time.Minute), c, testLiveObserveScope, true)
	capital, _ = st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow.Add(time.Minute))
	if !capital.Known || capital.DrawdownBase != 0 || capital.FlowsSinceStart != 0 || capital.BudgetBase != 175000 {
		t.Fatalf("after a restart %+v", capital)
	}
	// Stale equity is unknown, never within.
	later := mandateTestNow.Add(time.Duration(*c.Capital.MaxEquityAgeMinutes+2) * time.Minute)
	if capital, _ = st.MandateCapitalForScope(c, brokerStateScope{}, later); capital.Known || !capital.EquityStale {
		t.Fatalf("stale equity %+v", capital)
	}
}

// One glitchy equity print never raises the high-water mark: a new high
// counts once a reading at least five minutes later confirms it, at the
// lower of the two, so a spike can never read as a drawdown from then on.
func TestMandateHighWaterMarkIgnoresASingleGlitch(t *testing.T) {
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return mandateTestNow }
	c := testMandateConstitution()
	reconcileNow(t, st)
	at := mandateTestNow.Add(-time.Hour)
	observeWithPnL(st, c, 300000, 0, at)
	mark := func() float64 {
		t.Helper()
		capital, _ := st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow)
		return capital.HWMRiskBase
	}
	seeded := mark()
	observeWithPnL(st, c, 600000, 0, at.Add(time.Minute)) // the glitch
	observeWithPnL(st, c, 300000, 0, at.Add(7*time.Minute))
	if got := mark(); got != seeded {
		t.Fatalf("a single spike moved the mark: %v → %v", seeded, got)
	}
	observeWithPnL(st, c, 320000, 20000, at.Add(10*time.Minute))
	observeWithPnL(st, c, 330000, 30000, at.Add(12*time.Minute))
	if got := mark(); got != seeded {
		t.Fatalf("a high confirmed too soon: %v", got)
	}
	observeWithPnL(st, c, 325000, 25000, at.Add(16*time.Minute))
	if got := mark(); got != seeded+20000 {
		t.Fatalf("a confirmed high: mark %v, want %v (the lower of the readings)", got, seeded+20000)
	}
}

// observeWithPnL is one account read: equity with the broker's daily P&L
// of the same session.
func observeWithPnL(st *riskCapitalStore, c *risk.Constitution, equity, pnl float64, at time.Time) {
	st.ObserveWithPnL(equity, mandatePnLReading{DailyBase: &pnl, Session: mandateSessionDate(at)}, at, c, testLiveObserveScope, true)
}

// observeWithPnLCash is observeWithPnL with the broker's total cash and the
// book's quantities fingerprint on the reading, as the live account read
// carries them ("" where the portfolio cache is not current).
func observeWithPnLCash(st *riskCapitalStore, c *risk.Constitution, equity, cash, pnl float64, book string, at time.Time) {
	st.ObserveWithPnL(equity, mandatePnLReading{DailyBase: &pnl, Session: mandateSessionDate(at), CashBase: &cash, PositionsKey: book}, at, c, testLiveObserveScope, true)
}

// mandateFlowTestStore is a capital store seeded at 300,000 an hour before
// mandateTestNow, with the helpers the flow tests read it through.
func mandateFlowTestStore(t *testing.T) (*riskCapitalStore, *risk.Constitution, time.Time, func() mandate.Capital) {
	t.Helper()
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return mandateTestNow }
	c := testMandateConstitution()
	reconcileNow(t, st)
	at := mandateTestNow.Add(-time.Hour)
	observeWithPnL(st, c, 300000, 0, at)
	return st, c, at, func() mandate.Capital {
		t.Helper()
		got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, mandateTestNow)
		return got
	}
}

// The part of an equity move that declared flows and the book's own P&L
// leave unexplained, more than 1% of equity (or 1,000), is a suspected cash
// flow: the room is held (the capital reads unknown, so additions stay
// refused and the reduction sizes nothing) and the high-water mark does not
// move. It is measured cumulatively, so a later P&L move never offsets it:
// it clears when a declared or statement flow accounts for it, when the
// reading that raised it is undone, or when a statement covering its day is
// reconciled. A move the P&L explains is no flow; without the broker's P&L
// the whole move counts.
func TestMandateUnexplainedEquityMoveHoldsTheRoom(t *testing.T) {
	suspected := func(t *testing.T, got mandate.Capital) bool {
		t.Helper()
		return !got.Known && len(got.Reasons) > 0 && strings.Contains(got.Reasons[0], "a cash flow is suspected")
	}
	t.Run("a market move the P&L explains is no flow", func(t *testing.T) {
		st, c, at, capital := mandateFlowTestStore(t)
		observeWithPnL(st, c, 285000, -15000, at.Add(10*time.Minute))
		if got := capital(); !got.Known {
			t.Fatalf("a P&L-explained move held the room: %v", got.Reasons)
		}
	})
	t.Run("an undeclared 3% withdrawal stays suspect through a +1% day", func(t *testing.T) {
		st, c, at, capital := mandateFlowTestStore(t)
		observeWithPnL(st, c, 291000, 0, at.Add(10*time.Minute))
		observeWithPnL(st, c, 294000, 3000, at.Add(20*time.Minute))
		observeWithPnL(st, c, 294100, 3100, at.Add(30*time.Minute))
		if got := capital(); !suspected(t, got) {
			t.Fatalf("a later P&L move offset the withdrawal: %+v", got)
		}
		// Declared, it is accounted for and the room returns; the drawdown
		// is the market's, never the withdrawal.
		if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "withdrawal", AmountBase: 9000}, rpc.OrderOriginHumanTTY, c); err != nil {
			t.Fatal(err)
		}
		observeWithPnL(st, c, 294100, 3100, at.Add(35*time.Minute))
		if got := capital(); !got.Known || got.DrawdownBase > 1 {
			t.Fatalf("after the declaration: %+v", got)
		}
	})
	t.Run("an 8% withdrawal on a -4% day is flagged", func(t *testing.T) {
		st, c, at, capital := mandateFlowTestStore(t)
		observeWithPnL(st, c, 264000, -12000, at.Add(10*time.Minute))
		if got := capital(); !suspected(t, got) {
			t.Fatalf("a withdrawal on a falling day passed: %+v", got)
		}
	})
	t.Run("a 10% jump without P&L is flagged, a small move is not", func(t *testing.T) {
		st, c, at, capital := mandateFlowTestStore(t)
		st.ObserveWithPnL(301500, mandatePnLReading{}, at.Add(5*time.Minute), c, testLiveObserveScope, true)
		if got := capital(); !got.Known {
			t.Fatalf("a 0.5%% move without P&L held the room: %v", got.Reasons)
		}
		st.ObserveWithPnL(331650, mandatePnLReading{}, at.Add(10*time.Minute), c, testLiveObserveScope, true)
		if got := capital(); !suspected(t, got) {
			t.Fatalf("a 10%% jump without P&L passed: %+v", got)
		}
		// Undone by the next reading (a glitch), it clears.
		st.ObserveWithPnL(301600, mandatePnLReading{}, at.Add(12*time.Minute), c, testLiveObserveScope, true)
		if got := capital(); !got.Known {
			t.Fatalf("an undone glitch stayed suspect: %v", got.Reasons)
		}
	})
	t.Run("a deposit declared late never reads as drawdown", func(t *testing.T) {
		st, c, at, capital := mandateFlowTestStore(t)
		mark := capital().HWMRiskBase
		observeWithPnL(st, c, 330000, 0, at.Add(10*time.Minute))
		observeWithPnL(st, c, 330200, 200, at.Add(20*time.Minute))
		if got := capital(); !suspected(t, got) || got.HWMRiskBase != 0 && got.HWMRiskBase != mark {
			t.Fatalf("an undeclared deposit: %+v", got)
		}
		if _, err := st.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: "deposit", AmountBase: 30000}, rpc.OrderOriginHumanTTY, c); err != nil {
			t.Fatal(err)
		}
		observeWithPnL(st, c, 330300, 300, at.Add(30*time.Minute))
		if got := capital(); !got.Known || got.HWMRiskBase != mark || got.DrawdownBase > 1 {
			t.Fatalf("a deposit declared late read as drawdown: %+v", got)
		}
	})
	t.Run("a statement covering the day clears what nothing else explains", func(t *testing.T) {
		st, c, at, capital := mandateFlowTestStore(t)
		observeWithPnL(st, c, 280000, 0, at.Add(10*time.Minute))
		observeWithPnL(st, c, 280100, 100, at.Add(20*time.Minute))
		if got := capital(); !suspected(t, got) {
			t.Fatalf("not suspected: %+v", got)
		}
		st.mu.Lock()
		st.state.StatementCoverageTo = at
		st.mu.Unlock()
		observeWithPnL(st, c, 280200, 200, at.Add(30*time.Minute))
		if got := capital(); !got.Known {
			t.Fatalf("a statement covering the day did not clear it: %v", got.Reasons)
		}
	})
	t.Run("overnight the new session's own P&L is the yardstick", func(t *testing.T) {
		st, c, at, _ := mandateFlowTestStore(t)
		next := at.Add(24 * time.Hour)
		st.now = func() time.Time { return next.Add(time.Minute) }
		observeWithPnL(st, c, 280000, 100, next)
		if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, next.Add(time.Minute)); !suspected(t, got) {
			t.Fatalf("an overnight drop the new session's P&L does not explain passed: %+v", got)
		}
	})
}

// A suspected cash flow makes the verdict unknown: additions are refused and
// the forced reduction sizes nothing until it is explained.
func TestMandateSuspectedFlowMakesTheVerdictUnknown(t *testing.T) {
	srv, st := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(1000), nil }
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("baseline verdict %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	c := srv.riskPolicies.active
	session := mandateSessionDate(mandateTestNow)
	pnl := 100.0
	st.ObserveWithPnL(300000, mandatePnLReading{DailyBase: &pnl, Session: session}, mandateTestNow.Add(-90*time.Second), c, testLiveObserveScope, true)
	st.ObserveWithPnL(280000, mandatePnLReading{DailyBase: &pnl, Session: session}, mandateTestNow.Add(-time.Minute), c, testLiveObserveScope, true)
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusUnknown || !slices.ContainsFunc(res.Verdict.Reasons, func(r string) bool { return strings.Contains(r, "a cash flow is suspected") }) {
		t.Fatalf("verdict under a suspected flow: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	if blocker := srv.mandateBudgetGate(mandateBuy(10), mandateOpen); blocker == nil || blocker.Code != mandateBudgetCode {
		t.Fatalf("an addition passed under a suspected flow: %+v", blocker)
	}
	if _, err := json.Marshal(res); err != nil {
		t.Fatal(err)
	}
}

// Realised volatility reads from the daily samples once enough exist.
func TestMandateRealisedVolatility(t *testing.T) {
	daily := map[string]float64{}
	day := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	value := 300000.0
	for i := range 70 {
		if i%2 == 0 {
			value *= 1.01
		} else {
			value /= 1.01
		}
		daily[day.AddDate(0, 0, i).Format(time.DateOnly)] = value
	}
	r := mandateRealisedFrom(daily, 70000)
	if r.Samples != 70 || r.Vol20Pct == nil || r.Vol60Pct == nil || *r.Vol20Pct < 10 || *r.Vol60Pct < 10 {
		t.Fatalf("realised %+v", r)
	}
}

// The history store refreshes a due series once per broker session on the
// background lane, keeps the newest closes with their source time, and
// judges freshness against the last completed session.
func TestMandateHistoryRefreshOncePerSessionAndFreshness(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	dates := mandateTestDates(mandateHistoryKeep + 10)
	fetches := 0
	srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, tail int, now time.Time) (*rpc.MarketHistoryResult, error) {
		fetches++
		if tail != -mandateHistoryFetchDays || p.Range != "5Y" {
			t.Fatalf("fetch asked for %d days of %s", tail, p.Range)
		}
		res := &rpc.MarketHistoryResult{Contract: p.Contract, Interval: "1 day", AsOf: now, Source: "IBKR historical bars · TRADES"}
		for i, d := range dates {
			at, _ := time.Parse(time.DateOnly, d)
			res.Points = append(res.Points, rpc.MarketHistoryPoint{At: at, Value: 100 + float64(i)})
		}
		return res, nil
	}
	need := mandateHistoryNeed{Key: "u:SYNA", Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "STK", Currency: "USD"}}
	fresh, err := srv.fetchMandateSeries(t.Context(), need, mandateTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Closes) != mandateHistoryKeep || fresh.Dates[len(fresh.Dates)-1] != dates[len(dates)-1] || fresh.FetchedAt.IsZero() || fresh.Source == "" {
		t.Fatalf("fetched series %d closes through %s", len(fresh.Closes), fresh.Dates[len(fresh.Dates)-1])
	}
	if mandateSeriesStale(fresh, mandateTestNow) || mandateSeriesDue(fresh, mandateTestNow) {
		t.Fatal("a series through the last completed session read stale or due")
	}
	lagged := mandateTestSeries("u:SYNA", 0.3, 5, 1)
	if mandateSeriesStale(lagged, mandateTestNow) || !mandateSeriesDue(lagged, mandateTestNow) {
		t.Fatal("a series one session behind must be due and not yet stale")
	}
	if !mandateSeriesStale(mandateTestSeries("u:SYNA", 0.3, 5, 2), mandateTestNow) || !mandateSeriesStale(nil, mandateTestNow) {
		t.Fatal("a series two sessions behind, or none, must be stale")
	}
	// The needs of a book: every underlying, the index and the base pair.
	needs := mandateHistoryNeeds(mandateTestPositions(100), "EUR", nil)
	var keys []string
	for _, n := range needs {
		keys = append(keys, n.Key)
	}
	if strings.Join(keys, " ") != "fx:USD u:SPY u:SYNA u:SYNB" {
		t.Fatalf("needs %v", keys)
	}
	for _, n := range needs {
		if n.Key == "fx:USD" && (n.Contract.SecType != "CASH" || n.Contract.Symbol != "EUR" || n.Contract.Currency != "USD" || n.Contract.Exchange != "IDEALPRO") {
			t.Fatalf("fx need %+v", n.Contract)
		}
	}
	// An FX series is inverted to base units per unit of the currency.
	pair := &mandateHistorySeries{Key: "fx:USD", Dates: []string{"2026-10-12", "2026-10-13"}, Closes: []float64{1.25, 1.0}, FetchedAt: mandateTestNow}
	if conv := mandateSeriesInput(pair, mandateTestNow); conv.Closes[0] != 0.8 || conv.Closes[1] != 1 {
		t.Fatalf("inverted pair %v", conv.Closes)
	}
	// Persistence round trip through the core store.
	if err := srv.saveMandateSeries(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	srv.mandateHistory = mandateHistoryStore{}
	if loaded := srv.mandateHistoryForNeed(t.Context(), need); loaded == nil || len(loaded.Closes) != mandateHistoryKeep || !loaded.FetchedAt.Equal(fresh.FetchedAt) {
		t.Fatalf("reloaded series %+v", loaded)
	}
}

// Within budget, an addition that would take one name's gap loss over its
// cap is refused on its own, though the volatility budget would admit it,
// with the gap and the cap it would breach; the same notional in a broad
// index fund passes at the owner's index gap (D4) and is refused without
// it. A smaller addition inside the cap passes (core-sleeve.md, review B1).
func TestMandateGateRefusesAnAdditionThatBreachesAGapCap(t *testing.T) {
	c := testMandateConstitution()
	srv, _ := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	for _, s := range []*mandateHistorySeries{mandateTestSeries("u:SYNC", 0.25, 5, 0), mandateTestSeries("u:SXR8", 0.12, 9, 0)} {
		if err := srv.saveMandateSeries(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	capBase := res.Verdict.Stresses.GapCapBase // 0.30 × 230,000 × 0.333 at full room
	buy := func(sym string, shares int) rpc.OrderDraft {
		return rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: shares, LimitPrice: 100, Contract: rpc.ContractParams{Symbol: sym, SecType: "STK", Currency: "EUR"}}
	}
	// An unheld name is priced at its last decision-grade close.
	spot := func(key string) float64 {
		series := srv.mandateHistorySeries(t.Context(), key)
		return series.Closes[len(series.Closes)-1]
	}
	inside := int(capBase / (spot("u:SYNC") * 0.25))
	if blocker := srv.mandateBudgetGate(buy("SYNC", inside), mandateOpen); blocker != nil {
		t.Fatalf("a buy inside the gap cap: %+v", blocker)
	}
	blocker := srv.mandateBudgetGate(buy("SYNC", inside+20), mandateOpen)
	if blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "a −25% gap in SYNC would lose") || !strings.Contains(blocker.Message, "over the gap cap") {
		t.Fatalf("a buy over the gap cap: %+v", blocker)
	}
	// The volatility alone would admit it.
	_, kept, _, _ := srv.mandateVerdicts.current()
	book, missing := srv.mandateOrderBook(buy("SYNC", inside+20), kept, mandateTestNow)
	leg, ok := mandateOrderLeg(buy("SYNC", inside+20), &book)
	if len(missing) != 0 || !ok {
		t.Fatalf("order book: %v %v", missing, ok)
	}
	if with := mandate.Evaluate(mandate.WithLeg(book, leg)); with.Risk == nil || with.Risk.SDAnnualBase > with.Capital.BudgetAnnualSD {
		t.Fatalf("fixture: the volatility budget already refuses %+v", with.Risk)
	}
	// A broad index fund worth six times the cap: its 25% gap loses one
	// and a half caps, refused; its 12% gap 0.72 of a cap, admitted. At
	// 8.6 times the cap even the 12% gap breaches.
	fund := buy("SXR8", int(6*capBase/spot("u:SXR8")))
	if blocker := srv.mandateBudgetGate(fund, mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "a −25% gap in SXR8") {
		t.Fatalf("a broad index without index_gap_shock_pct: %+v", blocker)
	}
	c.Mandate.IndexGapShockPct = new(12.0)
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book with the index gap: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	if blocker := srv.mandateBudgetGate(fund, mandateOpen); blocker != nil {
		t.Fatalf("a broad index at the 12%% index gap: %+v", blocker)
	}
	if blocker := srv.mandateBudgetGate(buy("SXR8", int(8.6*capBase/spot("u:SXR8"))), mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "a −12% gap in SXR8") {
		t.Fatalf("a broad index over its cap at 12%%: %+v", blocker)
	}
}

// A breach the book already carries is never deepened by an addition, and
// a gap-cap check never refuses an order that leaves the breach as it was.
// A broad-index line is judged by the index gap group (owner decision
// "1. A", 2026-10-30 09:41 CET): a new hedge line in a group whose standing
// breach it lessens adds none; one that leaves the group losing more, or
// breaches a group that fit before, does.
func TestMandateGapBreachAddedJudgesTheIndexGroup(t *testing.T) {
	verdict := func(loss float64, breach bool, rows ...string) mandate.Verdict {
		v := mandate.Verdict{Stresses: &mandate.Stresses{GapCapBase: 20000, IndexGap: &mandate.IndexGapReport{Members: rows, LossBase: loss, Breach: breach}}}
		for _, r := range rows {
			v.Underlyings = append(v.Underlyings, mandate.UnderlyingReport{Underlying: r, BroadIndex: true, GapLossBase: loss, GapCapBase: 20000, GapBreach: breach})
		}
		return v
	}
	before := verdict(30000, true, "SPY")
	if u, ok := mandateGapBreachAdded(&before, verdict(25000, true, "SPY", mandate.MicroSP500Future)); ok {
		t.Fatalf("a hedge lessening the index breach added %+v", u)
	}
	if _, ok := mandateGapBreachAdded(&before, verdict(31000, true, "SPY", mandate.MicroSP500Future)); !ok {
		t.Fatal("a deeper index breach was not reported")
	}
	fit := verdict(10000, false, "SPY")
	if u, ok := mandateGapBreachAdded(&fit, verdict(21000, true, "SPY", mandate.MicroSP500Future)); !ok || u.Underlying != "SPY" {
		t.Fatalf("an over-hedge breaching a fitting group: %+v %v", u, ok)
	}
}

func TestMandateGapBreachAddedComparesWithTheStandingBook(t *testing.T) {
	before := &mandate.Verdict{Underlyings: []mandate.UnderlyingReport{{Underlying: "SYNA", GapLossBase: 30000, GapCapBase: 20000, GapBreach: true}, {Underlying: "SYNB", GapLossBase: 1000, GapCapBase: 20000}}}
	same := mandate.Verdict{Underlyings: []mandate.UnderlyingReport{{Underlying: "SYNA", GapLossBase: 30000, GapCapBase: 20000, GapBreach: true}, {Underlying: "SYNB", GapLossBase: 5000, GapCapBase: 20000}}}
	if u, ok := mandateGapBreachAdded(before, same); ok {
		t.Fatalf("an addition elsewhere refused for a standing breach: %+v", u)
	}
	deeper := mandate.Verdict{Underlyings: []mandate.UnderlyingReport{{Underlying: "SYNA", GapLossBase: 31000, GapCapBase: 20000, GapBreach: true}}}
	if u, ok := mandateGapBreachAdded(before, deeper); !ok || u.Underlying != "SYNA" {
		t.Fatal("a deeper breach passed")
	}
	created := mandate.Verdict{Underlyings: []mandate.UnderlyingReport{{Underlying: "SYNC", GapLossBase: 21000, GapCapBase: 20000, GapBreach: true}}}
	if u, ok := mandateGapBreachAdded(before, created); !ok || u.Underlying != "SYNC" {
		t.Fatal("a new breach passed")
	}
}

// A held IBCF line is measured on SPY's returns (review S5): the book reads
// no IBCF series, so none can make its verdict stale; an IBCF order the
// book does not hold is priced at its limit, and one without a limit is
// refused, never passed unmeasured.
func TestMandateMeasuresIBCFOnTheIndex(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	pos := mandateTestPositions(100)
	pos.Stocks = append(pos.Stocks, rpc.PositionView{Symbol: "IBCF", SecType: rpc.SecTypeStock, ConID: 9020, Currency: "EUR", PrimaryExch: "IBIS", Quantity: 200, Multiplier: 1, Mark: 50})
	for _, need := range mandateHistoryNeeds(pos, "EUR", nil) {
		if need.Key == "u:IBCF" {
			t.Fatalf("the book reads an IBCF series: %+v", need)
		}
	}
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return pos, nil }
	res := srv.evaluateMandate(t.Context(), false)
	if res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book with IBCF: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	var found bool
	for _, u := range res.Verdict.Underlyings {
		if u.Underlying == "IBCF" {
			found = math.Abs(u.Beta-1) < 1e-12
		}
	}
	if !found {
		t.Fatalf("IBCF not measured at beta 1: %+v", res.Verdict.Underlyings)
	}
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	buy := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 100, LimitPrice: 50, Contract: rpc.ContractParams{Symbol: "IBCF", SecType: "STK", Currency: "EUR", PrimaryExch: "IBIS"}}
	if blocker := srv.mandateBudgetGate(buy, mandateOpen); blocker != nil {
		t.Fatalf("an IBCF buy at a limit: %+v", blocker)
	}
	if got := srv.mandateHistoryRequests(mandateTestNow); len(got) != 0 {
		t.Fatalf("an IBCF buy requested a series: %+v", got)
	}
	buy.Quantity = 20000
	if blocker := srv.mandateBudgetGate(buy, mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "with this order") {
		t.Fatalf("a large IBCF buy: %+v", blocker)
	}
	buy.Quantity, buy.LimitPrice = 100, 0
	if blocker := srv.mandateBudgetGate(buy, mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "no price for IBCF") {
		t.Fatalf("an IBCF buy without a limit: %+v", blocker)
	}
}

// An option order the evaluator cannot price is never passed unmeasured
// (core sleeve review, 2026-10-14). A sale is refused: a short option's
// loss has no bound without a volatility (the reviewer's probe: 20 puts at
// a limit of 0). A buy is judged by a volatility-free bound: its premium
// at the limit for the gap, the underlying's full daily moves for the
// risk; admitted when both fit, refused on whichever does not. A held
// contract with its IV and a closing order are unaffected.
func TestMandateGateFailsClosedOnUnpricedOptions(t *testing.T) {
	srv, _ := mandateTestServer(t, testMandateConstitution())
	seedMandateHistory(t, srv, 0)
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return mandateTestPositions(100), nil }
	if res := srv.evaluateMandate(t.Context(), false); res.Verdict.Status != mandate.StatusWithin {
		t.Fatalf("book: %s %v", res.Verdict.Status, res.Verdict.Reasons)
	}
	option := func(action, right string, qty int, strike, limit float64) rpc.OrderDraft {
		return rpc.OrderDraft{Action: action, Quantity: qty, LimitPrice: limit, Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "OPT", Currency: "USD", Right: right, Strike: strike, Expiry: "20270115", Multiplier: 100}}
	}
	for name, draft := range map[string]rpc.OrderDraft{
		"20 puts at a limit of 0": option(rpc.OrderActionSell, "P", 20, 90, 0),
		"calls below intrinsic":   option(rpc.OrderActionSell, "C", 20, 50, 10),
		"puts past expiry": func() rpc.OrderDraft {
			d := option(rpc.OrderActionSell, "P", 5, 90, 2)
			d.Contract.Expiry = "20261009"
			return d
		}(),
		"a trailing sale (no limit price)": option(rpc.OrderActionSell, "C", 3, 120, 0),
	} {
		blocker := srv.mandateBudgetGate(draft, mandateOpen)
		if blocker == nil || blocker.Code != mandateBudgetCode || !strings.Contains(blocker.Message, "option sale cannot be measured") || !strings.Contains(blocker.Message, "stay open") {
			t.Fatalf("%s: %+v", name, blocker)
		}
	}
	// A buy below intrinsic (strike 50 with SYNA at 100, limit 40): the IV
	// does not solve, so the bound judges it.
	if _, ok := mandateOrderLeg(option(rpc.OrderActionBuy, "C", 1, 50, 40), func() *mandate.Input { _, in, _, _ := srv.mandateVerdicts.current(); return in }()); ok {
		t.Fatal("fixture: the evaluator priced a call bought below intrinsic")
	}
	if blocker, warning := srv.mandateBudgetJudgement(option(rpc.OrderActionBuy, "C", 1, 50, 40), mandateOpen); blocker != nil || warning != nil {
		t.Fatalf("one contract inside both bounds: %+v %+v", blocker, warning)
	}
	// Seven contracts: the risk bound fits, the premium does not fit the
	// gap cap on top of SYNA's own gap loss. Since the owner's decision of
	// 2026-10-23 18:22 CEST that is the buy's warning, not its refusal: the
	// premium, 25,200 EUR, is 8.4% of net liquidation, inside the limits.
	if blocker, warning := srv.mandateBudgetJudgement(option(rpc.OrderActionBuy, "C", 7, 50, 40), mandateOpen); blocker != nil || warning == nil || !strings.Contains(warning.Message, "a gap in SYNA could lose") {
		t.Fatalf("seven contracts: %+v %+v", blocker, warning)
	}
	// Twelve: the risk bound is over the budget, the warning says so; the
	// premium, 43,200 EUR, is 14.4%, still inside.
	if blocker, warning := srv.mandateBudgetJudgement(option(rpc.OrderActionBuy, "C", 12, 50, 40), mandateOpen); blocker != nil || warning == nil || !strings.Contains(warning.Message, "the risk could reach") {
		t.Fatalf("twelve contracts: %+v %+v", blocker, warning)
	}
	// Thirteen: 46,800 EUR, 15.6% of net liquidation, over the 15% limit
	// for one name: refused by the premium limits, naming the figures.
	if blocker := srv.mandateBudgetGate(option(rpc.OrderActionBuy, "C", 13, 50, 40), mandateOpen); blocker == nil || blocker.Code != rpc.MandatePremiumBlockerCode || !strings.Contains(blocker.Message, "SYNA would be 46,800 EUR, 15.6% of net liquidation 300,000 EUR, over the 15.0% limit for one name (now 0.0%)") {
		t.Fatalf("thirteen contracts: %+v", blocker)
	}
	// The bound is above the evaluator's own figure where both apply: a
	// call the IV solves for, measured both ways.
	_, kept, _, _ := srv.mandateVerdicts.current()
	priced := option(rpc.OrderActionBuy, "C", 5, 100, 8)
	book, _ := srv.mandateOrderBook(priced, kept, mandateTestNow)
	leg, ok := mandateOrderLeg(priced, &book)
	if !ok {
		t.Fatal("fixture: a solvable call was not priced")
	}
	with := mandate.Evaluate(mandate.WithLeg(book, leg))
	res, _, _, _ := srv.mandateVerdicts.current()
	b, why := mandateLongOptionBound(priced, book, res.Verdict)
	if why != "" || b.riskAnnual < with.Risk.SDAnnualBase {
		t.Fatalf("the bound %.0f (%s) is below the evaluator's %.0f", b.riskAnnual, why, with.Risk.SDAnnualBase)
	}
	// A held contract with its IV is priced from the position for the
	// volatility warning; its premium still needs a limit (the premium
	// limits judge the buy). A closing order never reaches the check.
	held := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 1, Contract: rpc.ContractParams{ConID: 9004, Symbol: "SYNA", SecType: "OPT", Currency: "USD", Right: "C", Strike: 110, Expiry: "20270115", Multiplier: 100}}
	if blocker := srv.mandateBudgetGate(held, mandateOpen); blocker == nil || blocker.Code != rpc.MandatePremiumBlockerCode || !strings.Contains(blocker.Message, "no premium to bound its loss") {
		t.Fatalf("a held contract without a limit: %+v", blocker)
	}
	held.LimitPrice = 3
	if blocker, warning := srv.mandateBudgetJudgement(held, mandateOpen); blocker != nil || warning != nil {
		t.Fatalf("a held contract with its IV at a limit: %+v %+v", blocker, warning)
	}
	closing := option(rpc.OrderActionSell, "P", 20, 90, 0)
	if blocker := srv.mandateBudgetGate(closing, rpc.OrderPositionImpact{Before: 20, After: 0, Effect: rpc.OrderPositionEffectClose}); blocker != nil {
		t.Fatalf("a closing sale: %+v", blocker)
	}
}

// A bill purchase passes the mandate gate whatever the verdict reads, as the
// mandate measures a held bill as carrying no equity or volatility risk
// (owner, 2026-10-17 06:47 CEST: "If cash like investments do not count
// against it, I'm fine with 15/30"). A bond purchase, whose interest-rate
// risk is not measured, a stock purchase and a sale that opens a short bill
// are refused as before.
func TestMandateGatePassesBillPurchasesOnly(t *testing.T) {
	bill := func(action string) rpc.OrderDraft {
		return rpc.OrderDraft{Action: action, Quantity: 10000, LimitPrice: 99, Contract: rpc.ContractParams{ConID: 9006, Symbol: "SYNU", SecType: "BILL", Currency: "EUR"}}
	}
	bond := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 10000, LimitPrice: 99, Contract: rpc.ContractParams{ConID: 9007, Symbol: "SYNL", SecType: "BOND", Currency: "EUR"}}
	heldBill := rpc.OrderPositionImpact{Before: 10000, After: 20000, Effect: rpc.OrderPositionEffectIncrease}
	for _, tc := range []struct {
		name    string
		status  string
		pos     *rpc.PositionsResult
		gapRoom float64
		lag     int
	}{
		{"reduce on the loss measure", mandate.StatusReduce, mandateTestPositionsOverLoss(), 100, 0},
		{"reduce on a gap breach", mandate.StatusReduce, mandateTestPositions(3000), 33.3, 0},
		{"unknown", mandate.StatusUnknown, mandateTestPositions(100), 33.3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testMandateConstitution()
			c.Mandate.GapRoomPct = new(tc.gapRoom)
			srv, _ := mandateTestServer(t, c)
			seedMandateHistory(t, srv, tc.lag)
			srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) { return tc.pos, nil }
			if res := srv.evaluateMandate(t.Context(), false); res.Verdict.Status != tc.status || !res.Gate.RefusesAdditions {
				t.Fatalf("fixture verdict %s %v gate %+v, want %s refusing additions", res.Verdict.Status, res.Verdict.Reasons, res.Gate, tc.status)
			}
			for name, impact := range map[string]rpc.OrderPositionImpact{"new": mandateOpen, "held": heldBill} {
				if blocker := srv.mandateBudgetGate(bill(rpc.OrderActionBuy), impact); blocker != nil {
					t.Fatalf("%s bill buy: %+v, want allowed", name, blocker)
				}
				if err := srv.mandateBudgetGateError(bill(rpc.OrderActionBuy), impact); err != nil {
					t.Fatalf("%s bill buy at admission: %v, want allowed", name, err)
				}
			}
			for name, draft := range map[string]rpc.OrderDraft{"bond buy": bond, "stock buy": mandateBuy(10), "bill sale to open": bill(rpc.OrderActionSell)} {
				if blocker := srv.mandateBudgetGate(draft, mandateOpen); blocker == nil || blocker.Code != mandateBudgetCode {
					t.Fatalf("%s: %+v, want mandate_budget", name, blocker)
				}
				if err := srv.mandateBudgetGateError(draft, mandateOpen); err == nil || !errors.Is(err, ErrTradingDisabled) {
					t.Fatalf("%s at admission: %v, want a trading-disabled refusal", name, err)
				}
			}
		})
	}
}

// The order preview passes a bill bought by identifier while the verdict
// reads reduce, as the broker's line names it a BILL, and refuses a
// Treasury note, which the broker names a BOND, with mandate_budget.
func TestMandateGatePreviewPassesABillAndRefusesABond(t *testing.T) {
	rig := newBondRequestRig(t)
	c := testMandateConstitution()
	c.Capital.BaseCurrency = "USD" // the rig's account base
	rig.srv.riskPolicies.active = c
	st := newTestRiskCapitalStore(t)
	st.now = rig.srv.now
	reconcileNow(t, st)
	st.Observe(300000, rig.now.Add(-2*time.Minute), c, testLiveObserveScope, true)
	rig.srv.riskCapital = st
	verdict := &mandate.Verdict{Status: mandate.StatusReduce, BaseCurrency: "USD"}
	rig.srv.mandateVerdicts.set(&rpc.MandateRiskResult{AsOf: rig.now, Active: true, Verdict: verdict}, &mandate.Input{}, rig.now, c.FingerprintKey(), false, rig.srv.captureMandateBookBinding())

	billLine := synthBondLine(8201, synthCUSIP35, "USD", cashSweepDay(rig.now).AddDate(0, 0, 120))
	rig.setLine(synthCUSIP35, billLine)
	maturity := cashSweepDay(rig.now).AddDate(0, 0, 120).Format(time.DateOnly) + "T00:00:00"
	rig.treasury[synthCUSIP35] = []treasuryDirectSecurity{{CUSIP: synthCUSIP35, SecurityType: "Bill", Type: "Bill", MaturityDate: maturity}}
	res, err := rig.preview(rpc.OrderActionBuy, synthCUSIP35, "BILL", "USD", 5000)
	if err != nil {
		t.Fatalf("bill buy under a reduce verdict: %v (%v), want a preview", err, refusalCodes(err))
	}
	if res.Draft.Contract.SecType != "BILL" || !res.TokenMinted {
		t.Fatalf("bill draft %+v minted %v", res.Draft.Contract, res.TokenMinted)
	}

	note := syntheticCUSIP(t, "91282CZZ")
	rig.line(note, "USD", 8202)
	rig.treasury[note] = []treasuryDirectSecurity{{CUSIP: note, SecurityType: "Note", Type: "Note", MaturityDate: cashSweepDay(rig.now).AddDate(7, 0, 0).Format(time.DateOnly) + "T00:00:00", InterestRate: "5.000000"}}
	_, err = rig.preview(rpc.OrderActionBuy, note, "BOND", "USD", 5000)
	if codes := refusalCodes(err); err == nil || !slices.Equal(codes, []string{mandateBudgetCode}) {
		t.Fatalf("note buy under a reduce verdict: %v (%v), want mandate_budget", err, codes)
	}
}

// The broker's daily P&L starts anew at the broker's own hour, before the
// New York midnight the session date turns at. On the evening of 2026-10-16
// a reading at 23:59:59 New York (03:59:59 UTC the next day) still carried
// the trading day's session date while the broker's figure had already gone
// back to zero: the within-session step charged a whole down day to
// a cash flow, and the room was held from Friday night until a statement
// covering the day was reconciled. The weekend below is that incident on a
// synthetic book; the reading is explained by the figures themselves (the
// figure near zero, the equity unmoved), a flow landing in the same reading
// is still caught, and the Friday statement covers a suspicion raised after
// UTC midnight but before the New York date turned.
func TestMandateFlowCheckSurvivesTheBrokerDailyPnLReset(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	fridayNight := time.Date(2026, 10, 16, 23, 59, 59, 0, ny)
	if got := mandateSessionDate(fridayNight); got != "2026-10-16" {
		t.Fatalf("session date of %s = %s", fridayNight, got)
	}
	weekend := func(t *testing.T) (*riskCapitalStore, *risk.Constitution, func(time.Time, float64, float64), func(string)) {
		t.Helper()
		clock := utc(15, 14, 0)
		st := newTestRiskCapitalStore(t)
		st.now = func() time.Time { return clock }
		c := testMandateConstitution()
		reconcileNow(t, st)
		read := func(at time.Time, equity, pnl float64) {
			clock = at.Add(time.Second)
			observeWithPnL(st, c, equity, pnl, at)
		}
		known := func(note string) {
			t.Helper()
			if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock); !got.Known {
				t.Fatalf("%s: %v", note, got.Reasons)
			}
		}
		read(utc(15, 14, 0), 300000, 0)      // Thursday, during the session
		read(utc(15, 20, 30), 303000, 3000)  // Thursday after the close
		read(utc(16, 4, 0), 303000, 0)       // Friday 00:00 New York: a new session date, the figure reset
		read(utc(16, 14, 0), 290000, -13000) // Friday morning
		read(utc(16, 20, 30), 264000, -39000)
		known("a down day the broker's P&L explains")
		return st, c, read, known
	}
	t.Run("the figure goes back to zero before the New York date turns", func(t *testing.T) {
		_, _, read, known := weekend(t)
		read(fridayNight, 264000, 0)
		known("the reading at 23:59:59 New York")
		read(utc(17, 10, 0), 264000, 0) // Saturday
		read(utc(19, 14, 0), 270000, 6000)
		known("Monday")
	})
	t.Run("a flow in the same reading is still caught", func(t *testing.T) {
		st, c, read, _ := weekend(t)
		read(fridayNight, 234000, 0)
		got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, fridayNight.Add(time.Second))
		if got.Known || len(got.Reasons) == 0 || !strings.Contains(got.Reasons[0], "a cash flow is suspected") {
			t.Fatalf("a withdrawal in the reset reading passed: %+v", got)
		}
	})
	t.Run("the statement for the New York date covers it", func(t *testing.T) {
		st, c, read, known := weekend(t)
		read(fridayNight, 234000, 0)
		if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, fridayNight.Add(time.Second)); got.Known {
			t.Fatalf("not suspected: %+v", got)
		}
		// Saturday morning's fetch reconciles Friday's statement: its
		// coverage date is Friday, the UTC date before the reading's.
		st.mu.Lock()
		st.state.StatementCoverageTo = utc(16, 0, 0)
		st.mu.Unlock()
		read(utc(17, 6, 30), 234000, 0)
		known("after the Friday statement")
	})
}

// A deposit or withdrawal moves the equity; the broker's daily P&L figure can
// jump by a day's P&L while the equity stands still (a daemon reconnect on a
// Sunday, the reset at the broker's own hour). On 2026-10-18 two such jumps
// read as cash flows and held the room for days. The check now needs an
// equity move beyond the threshold to suspect anything, and still catches a
// flow that moves the equity.
func TestMandateFlowCheckIgnoresAPnLJumpWhileEquityStandsStill(t *testing.T) {
	utc := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	clock := utc(18, 14, 0)
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return clock }
	c := testMandateConstitution()
	reconcileNow(t, st)
	read := func(at time.Time, equity, pnl float64) {
		clock = at.Add(time.Second)
		observeWithPnL(st, c, equity, pnl, at)
	}
	read(utc(18, 14, 0), 300000, 15000) // Sunday afternoon: the figure still carries Friday's P&L
	read(utc(18, 17, 54), 300000, 5000) // the daemon reconnects: the figure is a day's P&L lower, the equity unchanged
	if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock); !got.Known {
		t.Fatalf("a P&L jump with the equity unchanged read as a flow: %v", got.Reasons)
	}
	read(utc(18, 18, 0), 300000, -5000)
	if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock); !got.Known {
		t.Fatalf("a second jump with the equity unchanged read as a flow: %v", got.Reasons)
	}
	read(utc(18, 18, 10), 320000, -5000) // a deposit: the equity moves with no P&L
	if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock); got.Known || len(got.Reasons) == 0 || !strings.Contains(got.Reasons[0], "a cash flow is suspected") {
		t.Fatalf("a deposit passed: %+v", got)
	}
}

// An equity move the P&L figure does not explain, with the broker's cash and
// the book's quantities unchanged, is the positions' marks (IBKR re-marks
// the book when its pre-market opens while the daily figure still reads
// zero), never a flow: 2026-10-19 08:01 UTC held every machine sleeve on
// such a reading. A move that cash carries is a flow, deposit or withdrawal;
// a move that changes the quantities (a transfer in kind, a deposit spent
// on a purchase in the same reading) is held as before; so is a reading
// without the cash figure or a current positions cache.
func TestMandateFlowCheckReadsAMarkMoveWithCashAndQuantitiesUnchangedAsMarks(t *testing.T) {
	utc := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	suspected := func(got mandate.Capital) bool {
		return !got.Known && len(got.Reasons) > 0 && strings.Contains(got.Reasons[0], "a cash flow is suspected")
	}
	fresh := func(t *testing.T) (*riskCapitalStore, *risk.Constitution, func(at time.Time, equity, cash, pnl float64, book string), func() mandate.Capital) {
		clock := utc(19, 7, 58)
		st := newTestRiskCapitalStore(t)
		st.now = func() time.Time { return clock }
		c := testMandateConstitution()
		reconcileNow(t, st)
		read := func(at time.Time, equity, cash, pnl float64, book string) {
			clock = at.Add(time.Second)
			observeWithPnLCash(st, c, equity, cash, pnl, book, at)
		}
		capital := func() mandate.Capital { got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock); return got }
		return st, c, read, capital
	}
	t.Run("marks", func(t *testing.T) {
		_, _, read, capital := fresh(t)
		read(utc(19, 7, 58), 300000, 100000, 0, "book-a")
		read(utc(19, 8, 1), 304000, 100000, 0, "book-a") // the pre-market opens: the marks move the equity, the figure still reads zero
		if got := capital(); !got.Known {
			t.Fatalf("a mark move with cash and quantities unchanged read as a flow: %v", got.Reasons)
		}
		read(utc(19, 8, 3), 304000, 100000, 4000, "book-a") // the figure catches up with the equity unchanged
		if got := capital(); !got.Known {
			t.Fatalf("the figure catching up read as a flow: %v", got.Reasons)
		}
		read(utc(19, 8, 5), 324000, 120000, 4000, "book-a") // a deposit: equity and cash move together
		if got := capital(); !suspected(got) {
			t.Fatalf("a deposit carried by cash passed: %+v", got)
		}
	})
	t.Run("withdrawal under a gain in the marks", func(t *testing.T) {
		_, _, read, capital := fresh(t)
		read(utc(19, 7, 58), 300000, 100000, 0, "book-a")
		read(utc(19, 8, 1), 276000, 80000, 4000, "book-a")
		if got := capital(); !suspected(got) {
			t.Fatalf("a withdrawal carried by cash passed: %+v", got)
		}
	})
	t.Run("transfer in kind", func(t *testing.T) {
		_, _, read, capital := fresh(t)
		read(utc(19, 7, 58), 300000, 100000, 0, "book-a")
		read(utc(19, 8, 1), 320000, 100000, 0, "book-b") // shares arrive: cash unchanged, the quantities not
		if got := capital(); !suspected(got) {
			t.Fatalf("a transfer in kind passed as marks: %+v", got)
		}
	})
	t.Run("deposit spent on a purchase in the same reading", func(t *testing.T) {
		_, _, read, capital := fresh(t)
		read(utc(19, 7, 58), 300000, 100000, 0, "book-a")
		read(utc(19, 8, 1), 320000, 101000, 0, "book-b") // 20,000 in, 19,000 of it spent on shares
		if got := capital(); !suspected(got) {
			t.Fatalf("a deposit spent on a purchase passed as marks: %+v", got)
		}
	})
	t.Run("positions cache not current", func(t *testing.T) {
		_, _, read, capital := fresh(t)
		read(utc(19, 7, 58), 300000, 100000, 0, "")
		read(utc(19, 8, 1), 304000, 100000, 0, "") // the same mark move, the quantities unknown: the rule as it was
		if got := capital(); !suspected(got) {
			t.Fatalf("a move with the quantities unknown passed as marks: %+v", got)
		}
	})
}

// A suspicion raised by a real equity move stays even when the market later
// moves the equity back to its base: only an explanation or a statement
// covering its day clears it.
func TestMandateRealFlowSuspicionSurvivesTheMarketMovingBack(t *testing.T) {
	utc := func(day, hour, minute int) time.Time { return time.Date(2026, 10, day, hour, minute, 0, 0, time.UTC) }
	clock := utc(18, 14, 0)
	st := newTestRiskCapitalStore(t)
	st.now = func() time.Time { return clock }
	c := testMandateConstitution()
	reconcileNow(t, st)
	read := func(at time.Time, equity, pnl float64) {
		clock = at.Add(time.Second)
		observeWithPnL(st, c, equity, pnl, at)
	}
	read(utc(18, 14, 0), 300000, 15000)
	read(utc(18, 17, 54), 320000, 15000) // a deposit: equity moves, no P&L
	if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock); got.Known {
		t.Fatalf("the deposit was not suspected: %+v", got)
	}
	read(utc(18, 18, 0), 300000, -5000) // the market gives the same amount back
	if got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock); got.Known {
		t.Fatalf("a real flow cleared because the market moved the equity back: %+v", got)
	}
}

// policy.snapshot carries the mandate's capital state and realised figures
// its own plausibility check read (policyCheckMandate), so `canary policy
// check` runs the realised checks instead of skipping them; nothing is
// carried while the constitution has no [mandate].
func TestPolicyCheckMandateFollowsTheConstitution(t *testing.T) {
	c := testMandateConstitution()
	srv, _ := mandateTestServer(t, c)
	m := srv.policyCheckMandate(mandateTestNow)
	if m == nil || !m.Known || m.BaseCurrency != c.Capital.BaseCurrency || m.Samples != 1 || m.Room <= 0 {
		t.Fatalf("mandate figures %+v", m)
	}
	if v := m.View(); v == nil || v.Known != m.Known || v.Room != m.Room || v.Samples != m.Samples || v.BaseCurrency != m.BaseCurrency {
		t.Fatalf("view %+v of %+v", v, m)
	}
	srv.riskPolicies.active = testConstitutionV3()
	if got := srv.policyCheckMandate(mandateTestNow); got != nil {
		t.Fatalf("figures without [mandate]: %+v", got)
	}
}

// An option buy within the premium limits is still judged against the loss
// room (spec TM-04.12): the book's premium at risk plus the order's, over
// the room, is the buy's warning (an option buy is refused by the premium
// limits alone, owner 2026-10-23 18:22 CEST), naming the measure with the
// order, the room and the measure now; a smaller buy inside the room passes
// clean.
func TestMandateGateJudgesAnOptionBuyByTheLossRoom(t *testing.T) {
	c := testMandateConstitution()
	c.Mandate.GapRoomPct = new(100.0)
	srv, _ := mandateTestServer(t, c)
	seedMandateHistory(t, srv, 0)
	// 15 SYNB calls: 30,000 EUR of value (27,000 paid), inside the 69,000 EUR
	// room, the name inside its 45,000 EUR premium limit.
	srv.mandatePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		pos := mandateTestUngrouped(100)
		iv := 0.3
		mv := 15 * 100 * 20.0
		pos.Options = append(pos.Options, rpc.PositionView{Symbol: "SYNB", SecType: rpc.SecTypeOption, ConID: 9005, Currency: "EUR", Quantity: 15, Multiplier: 100, Mark: 20, AvgCost: 1800,
			MarketValue: mv, MarketValueBase: &mv, Expiry: "20270115", Strike: 80, Right: "C", IV: &iv, Underlying: new(80.0),
			Delta: new(0.6), Gamma: new(0.02), Theta: new(-0.05), Vega: new(0.3)})
		return mandateTestGrouped(pos), nil
	}
	res := srv.evaluateMandate(t.Context(), false)
	if v := res.Verdict; v.Status != mandate.StatusWithin || v.Loss == nil || math.Abs(v.Loss.TotalBase-30000) > 1e-6 || res.Gate == nil || res.Gate.RefusesAdditions {
		t.Fatalf("fixture: %s loss %+v gate %+v", res.Verdict.Status, res.Verdict.Loss, res.Gate)
	}
	// 20 SYNA calls at 22 USD: 39,600 EUR of premium, inside the name's
	// limit and the 35% premium budget, over the loss room with the book
	// (the total is not spelled out here: the holdings guard keeps figures
	// near the live book out of Git).
	call := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 20, LimitPrice: 22, Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "OPT", Currency: "USD", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
	blocker, warning := srv.mandateBudgetJudgement(call, mandateOpen)
	if blocker != nil || warning == nil || warning.Code != mandateBudgetCode ||
		!strings.Contains(warning.Message, "with this order the loss measure would be ") ||
		!strings.Contains(warning.Message, " EUR, over the mandate's loss room 69,000 EUR (now 30,000 EUR)") {
		t.Fatalf("an option buy over the loss room: blocker %+v warning %+v", blocker, warning)
	}
	// Ten calls, 19,800 EUR: inside the room.
	call.Quantity = 10
	if blocker, warning := srv.mandateBudgetJudgement(call, mandateOpen); blocker != nil || warning != nil {
		t.Fatalf("an option buy inside the loss room: blocker %+v warning %+v", blocker, warning)
	}
}
