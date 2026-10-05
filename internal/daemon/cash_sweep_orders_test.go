package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// synthCABill is a synthetic Canadian bill ISIN with a valid check digit.
const synthCABill = "CA000ZZZZZZ0"

// cashSweepTestResolve names a synthetic bill for an invest plan the way
// resolution does (cashSweepBillFrom, cashSweepInvestUnits), maturing on the
// rung's target and quoted live with its ask at price. A plan the bill's
// grid cannot size comes back without a side.
func cashSweepTestResolve(cp cashSweepCurrencyPlan, price float64, now time.Time) cashSweepCurrencyPlan {
	if cp.side != rpc.CashSweepSideInvest || !cashSweepIsBill(cp.instrument) {
		return cp
	}
	ccy := cp.status.Currency
	id := map[string]string{"USD": synthCUSIP60, "EUR": synthDEBill, "GBP": synthGBBill, "CAD": synthCABill}[ccy]
	idType, source := ibkrlib.BondIdentifierISIN, rpc.CashSweepBillSourcePolicyISINs
	if ccy == "USD" {
		idType, source = ibkrlib.BondIdentifierCUSIP, rpc.CashSweepBillSourceTreasuryDirect
	}
	maturity := cashSweepDay(now).AddDate(0, 0, cp.targetDays)
	line := synthBondLine(7900, id, ccy, maturity)
	bill := cashSweepBillFrom(cashSweepBillCandidate{idType: idType, id: id, instrument: cp.instrument, source: source, maturity: maturity, days: cp.targetDays},
		line, synthLiveQuote(price), now)
	rules, err := ibkrlib.BondOrderRulesFrom(line)
	if err != nil {
		panic(err)
	}
	units, _ := cashSweepInvestUnits(cp.orderAmount, cashSweepInstrumentConventions[bill.Instrument], rules, *bill.Price, ccy)
	if units == 0 {
		cp.side = ""
		return cp
	}
	cp.bill, cp.rules, cp.units, cp.session = &bill, &rules, units, bill.Session
	return cp
}

// usBillRules and eurBillRules are the synthetic lines' grids
// (synthBondLine): one bond of 1,000 face for USD, 1,000 of face in steps of
// 1,000 for EUR.
var (
	usBillRules  = ibkrlib.BondOrderRules{MinTick: 0.0001, MinSize: 1, SizeIncrement: 1}
	eurBillRules = ibkrlib.BondOrderRules{MinTick: 0.0001, MinSize: 1000, SizeIncrement: 1000}
)

// An invest order is whole order units on the line's grid, priced at the
// higher of par and the quote so neither its face nor its cost passes the
// planned amount; a redemption rounds up onto the grid within the position.
func TestCashSweepSizingOnTheBillsGrid(t *testing.T) {
	us, eur := cashSweepInstrumentConventions[cashSweepInstrumentUSTBill], cashSweepInstrumentConventions[cashSweepInstrumentDEBubill]
	for _, tc := range []struct {
		name   string
		amount float64
		conv   cashSweepInstrumentConvention
		rules  ibkrlib.BondOrderRules
		price  float64
		want   int
	}{
		{"USD below par", 55000, us, usBillRules, 99.6, 55},
		{"USD above par", 55000, us, usBillRules, 100.5, 54},
		{"USD odd amount", 55999.99, us, usBillRules, 99.6, 55},
		{"EUR on the step", 55500, eur, eurBillRules, 99.8, 55000},
		{"EUR above par", 55000, eur, eurBillRules, 100.2, 54000},
	} {
		got, reason := cashSweepInvestUnits(tc.amount, tc.conv, tc.rules, tc.price, "USD")
		cost := float64(got) * tc.conv.FacePerUnit * tc.price / 100
		if got != tc.want || reason != "" || float64(got)*tc.conv.FacePerUnit > tc.amount || cost > tc.amount || tc.rules.CheckQuantity(got) != nil {
			t.Errorf("%s: units %d (%q), want %d", tc.name, got, reason, tc.want)
		}
	}
	if units, reason := cashSweepInvestUnits(900, eur, eurBillRules, 99.8, "EUR"); units != 0 || !strings.Contains(reason, "minimum order of 1000") {
		t.Fatalf("below the minimum = %d %q", units, reason)
	}
	if units, _ := cashSweepInvestUnits(55000, us, ibkrlib.BondOrderRules{}, 99.6, "USD"); units != 0 {
		t.Fatal("sized without a grid")
	}
	for _, tc := range []struct {
		want, held int
		rules      ibkrlib.BondOrderRules
		units      int
	}{
		{2011, 10000, eurBillRules, 3000},
		{500, 10000, eurBillRules, 1000},
		{20000, 10000, eurBillRules, 10000},
		{3, 10, usBillRules, 3},
		{20, 10, usBillRules, 10},
		{1500, 1500, eurBillRules, 1000},
		{800, 800, eurBillRules, 0},
		{2500, 2600, eurBillRules, 2000},
		{1, 0, usBillRules, 0},
	} {
		if got, reason := cashSweepRedeemUnits(tc.want, tc.held, tc.rules); got != tc.units || (got == 0) != (reason != "") {
			t.Errorf("redeem %d of %d = %d (%q), want %d", tc.want, tc.held, got, reason, tc.units)
		}
	}
}

// A bill's session: its contract's liquid hours when the details carry
// them. Missing or malformed hours stay unknown; historical assumed hours
// cannot authorize execution. Broker windows expire after the last named day.
func TestCashSweepBondSessions(t *testing.T) {
	wed := cashSweepTestNow()
	if s := cashSweepBondSession(nil, cashSweepInstrumentUSTBill, wed); s != nil {
		t.Fatalf("missing hours gained a session: %+v", s)
	}
	if _, known := bondSessionAt(cashSweepAssumedSession(cashSweepInstrumentUSTBill, wed), wed); known {
		t.Fatal("assumed hours supplied authority")
	}
	line := synthBondLine(7101, synthCUSIP35, "USD", cashSweepDay(wed).AddDate(0, 0, 35))
	line.LiquidHours = "20260930:0800-1500"
	line.TimeZoneID = "America/New_York"
	s := cashSweepBondSession(&line, cashSweepInstrumentUSTBill, wed)
	if s == nil || s.Source != rpc.BondSessionSourceLiquidHours || len(s.Windows) != 1 || !s.Windows[0].Close.Equal(time.Date(2026, 9, 30, 19, 0, 0, 0, time.UTC)) {
		t.Fatalf("broker hours: %+v", s)
	}
	if session, known := bondSessionAt(s, wed); !known || !session.IsOpen {
		t.Fatal("broker open was lost")
	}
	if _, known := bondSessionAt(s, wed.AddDate(0, 0, 1)); known {
		t.Fatal("expired hours remained authoritative")
	}
	line.LiquidHours = "20260930:9-15"
	if s := cashSweepBondSession(&line, cashSweepInstrumentUSTBill, wed); s != nil {
		t.Fatal("malformed hours gained assumed authority")
	}
}

// eurRedeemInput holds 10,000 EUR of face in one Bubill (quantity 10,000,
// worth 9,950) with cash 3,000 EUR against keep_cash 5,000.
func eurRedeemInput(held float64) cashSweepInput {
	in := cashSweepTestInput(map[string]float64{"EUR": 3000})
	in.Holdings["EUR"] = []cashSweepHolding{{
		Row:        rpc.PositionView{Symbol: "SYNTHB", SecType: "BOND", ConID: 7401, Currency: "EUR", Quantity: held, MarketValue: held * 0.995},
		Instrument: cashSweepInstrumentDEBubill, Maturity: cashSweepDay(cashSweepTestNow()).AddDate(0, 0, 60), FaceValue: held, MarketValue: held * 0.995,
	}}
	return in
}

// A redemption reads its held bill's grid by contract id and rounds the sale
// up onto it (down when up would pass what it may sell); a line it cannot
// read, or a sale the grid refuses, keeps the row (the shortfall is news)
// and blocks it. max_order_notional holds a sale like a buy.
func TestCashSweepRedemptionOnTheBillsGrid(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	held := &fakeBillSource{heldLines: map[int][]ibkrlib.BondContractDetails{7401: {synthBondLine(7401, synthDEBill2, "EUR", cashSweepDay(now).AddDate(0, 0, 60))}}}
	row := func(in cashSweepInput, src cashSweepBillSource) rpc.TradeProposal {
		t.Helper()
		plan, byCcy := planAndResolve(t, policy, in, src)
		eur := byCcy["EUR"]
		if eur.side != rpc.CashSweepSideRedeem {
			t.Fatalf("EUR = %+v", eur.status)
		}
		return cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, eur)
	}
	// The gap of 2,000 at 0.995 a unit asks for 2,011; the grid sells 3,000.
	p := row(eurRedeemInput(10000), held)
	if p.Quantity != 3000 || p.MaxQuantity != 10000 || p.PositionEffect != rpc.OrderPositionEffectReduce || len(p.Blockers) != 0 || p.Contract.SecType != "BOND" ||
		p.Contract.ConID != 7401 || p.CashSweep.Session == nil || !slices.ContainsFunc(p.Details, func(d string) bool { return strings.Contains(d, "rounded to 3000") }) {
		t.Fatalf("redeem row = %d/%d %s blockers %+v details %v", p.Quantity, p.MaxQuantity, p.PositionEffect, p.Blockers, p.Details)
	}
	if !p.AutomaticEligible() || cashSweepOrderTerms(p) == nil || cashSweepOrderTerms(p).FacePerUnit != 1 {
		t.Fatalf("redeem row is not an ordinary proposal: %+v", p.CashSweep)
	}
	// The held line is asked as its position's type first, then the bill's.
	if len(held.heldTypes) == 0 || !slices.Equal(held.heldTypes[0], []string{"BOND", "BILL"}) {
		t.Fatalf("held lookups asked %v", held.heldTypes)
	}
	// The held line cannot be read: blocked, not dropped.
	p = row(eurRedeemInput(10000), &fakeBillSource{})
	if len(p.Blockers) != 1 || p.Blockers[0].Code != rpc.CashSweepBlockerBillRules || p.CashSweep.Session != nil {
		t.Fatalf("unreadable line = %+v", p.Blockers)
	}
	// 1,500 held sells the 1,000 the grid allows; 800 held cannot be sold on
	// a 1,000 grid at all.
	if p = row(eurRedeemInput(1500), held); p.Quantity != 1000 || p.PositionEffect != rpc.OrderPositionEffectReduce || len(p.Blockers) != 0 {
		t.Fatalf("partial sale = %d %s %+v", p.Quantity, p.PositionEffect, p.Blockers)
	}
	p = row(eurRedeemInput(800), held)
	if len(p.Blockers) != 1 || p.Blockers[0].Code != rpc.CashSweepBlockerBelowMinimum || !strings.Contains(p.Blockers[0].Message, "size") {
		t.Fatalf("off-grid close = %+v", p.Blockers)
	}
	// max_order_notional 1,500 (EUR base) holds the sale to 1,507 units at
	// 0.995, which the grid rounds down to 1,000; the next cycle sells the
	// rest.
	capped := cashSweepTestPolicy(rpc.CashSweepModeActive, 1500)
	plan, byCcy := planAndResolve(t, capped, eurRedeemInput(10000), held)
	eur := byCcy["EUR"]
	p = cashSweepRow(capped, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, eur)
	if !eur.heldToCap || p.Quantity != 1000 || p.MaxQuantity != 1000 || !p.CashSweep.HeldToCap || !strings.Contains(eur.status.Reason, "next cycle sells the rest") {
		t.Fatalf("capped sale = %d %+v (%s)", p.Quantity, p.CashSweep, eur.status.Reason)
	}
	// A cap below one unit holds the currency.
	tiny := cashSweepTestPolicy(rpc.CashSweepModeActive, 0.5)
	if _, byCcy := planAndResolve(t, tiny, eurRedeemInput(10000), held); byCcy["EUR"].side != "" || !strings.Contains(byCcy["EUR"].status.Reason, "below one unit") {
		t.Fatalf("tiny cap = %+v", byCcy["EUR"].status)
	}
}

// Working bond buys and bond fills are valued at their currency's bill
// convention (per 100 × face per unit); a bill sale counts as a pending
// redemption; a bond in a currency without a convention stays unknown.
func TestCashSweepValuesBondOrdersAndFills(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	orders := []ibkrlib.OrderLifecycleEvent{
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: "DU1234567", SecType: "BOND", Currency: "USD", Action: rpc.OrderActionBuy, OrderType: "LMT", TotalQuantity: 10, Remaining: 10, LimitPrice: 99.5, Status: "Submitted"},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: "DU1234567", SecType: "BOND", Currency: "EUR", Action: rpc.OrderActionBuy, OrderType: "LMT", TotalQuantity: 5000, Remaining: 5000, LimitPrice: 99.8, Status: "Submitted"},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: "DU1234567", SecType: "BOND", Currency: "CHF", Action: rpc.OrderActionBuy, OrderType: "LMT", TotalQuantity: 5000, Remaining: 5000, LimitPrice: 99.8, Status: "Submitted"},
	}
	got := cashSweepCommitmentsFrom(orders, nil, scope)
	if math.Abs(got.ByCurrency["USD"]-9950) > 1e-9 || math.Abs(got.ByCurrency["EUR"]-4990) > 1e-9 || got.Unknown["CHF"] == "" || !strings.Contains(got.Unknown["USD"], "commission") || got.Unknown[""] == "" {
		t.Fatalf("commitments = %+v", got)
	}
	now := cashSweepTestNow()
	since := cashSweepSettlementWindowStart(now)
	view := func(ref, action, ccy string) rpc.OrderView {
		return rpc.OrderView{OrderRef: ref, Account: "DU1234567", Mode: "paper", Symbol: "SYNTHB", SecType: "BOND", Currency: ccy, Action: action}
	}
	views := []rpc.OrderView{view("a", rpc.OrderActionSell, "USD"), view("b", rpc.OrderActionBuy, "CAD")}
	events := map[string][]rpc.OrderEvent{
		orderViewKey(views[0]): {{At: now, Filled: 2, AvgFillPrice: 99.5}},
		orderViewKey(views[1]): {{At: now, Filled: 3000, AvgFillPrice: 99.9}},
	}
	settled := cashSweepSettlementFrom(views, events, scope, since)
	if len(settled.Unknown) != 0 || math.Abs(settled.SaleProceeds["USD"]-1990) > 1e-9 || math.Abs(settled.EquivalentSales["USD"]-1990) > 1e-9 ||
		math.Abs(settled.PurchaseCosts["CAD"]-2997) > 1e-9 || settled.EquivalentSales["CAD"] != 0 {
		t.Fatalf("settlement = %+v", settled)
	}
}

// Readiness reads a sweep bill's own session: closed outside it (with the
// next open), and a stale bill quote waits rather than refusing.
func TestCashSweepReadinessReadsTheBillSession(t *testing.T) {
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	_, got := planAndResolve(t, policy, cashSweepTestInput(map[string]float64{"USD": 60000}), usBillSource(cashSweepTestNow()))
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, cashSweepTestNow(), cashSweepPlan{status: rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeActive}}, got["USD"])
	at := time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)
	e := &proposalEngine{server: &Server{}, now: func() time.Time { return at }}
	r := e.classifyReadiness(row, nil, false, readinessSessions{})
	if r.Code != rpc.ReadinessMarketClosed || r.Market != string(bondSessionMarket) || r.OpensAt == nil ||
		!r.OpensAt.Equal(time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)) || !strings.Contains(r.Message, "US Treasury bills") {
		t.Fatalf("closed readiness = %+v", r)
	}
	e.now = func() time.Time { return time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC) }
	if r := e.classifyReadiness(row, nil, false, readinessSessions{}); r.Code != rpc.ReadinessReady || r.SessionState != rpc.ReadinessSessionOpen {
		t.Fatalf("open readiness = %+v", r)
	}
	stale := []rpc.TradingBlocker{{Code: rpc.CashSweepBlockerFreshQuote, Message: "stale"}}
	if r := e.classifyReadiness(row, stale, false, readinessSessions{}); r.Code != rpc.ReadinessQuoteUnusable {
		t.Fatalf("stale quote readiness = %+v", r)
	}
	// The pre-authorised scheduler waits for the bill's open plus the offset.
	due := e.automaticSessionDue(row, at)
	if !due.Equal(time.Date(2026, 10, 1, 12, 5, 0, 0, time.UTC)) {
		t.Fatalf("automatic due = %s", due)
	}
}

// cash_sweep joins the pre-authorised vocabulary: a sweep row maps to it,
// the policy accepts it, and its notice has its own copy with no "now"
// variant, since a sweep row always waits the full window.
func TestCashSweepPreAuthorisedVocabulary(t *testing.T) {
	if !validPreAuthorisedBucket("cash_sweep") || automaticBucketFor(rpc.TradeProposal{Bucket: rpc.TradeProposalBucketCashSweep}) != preAuthorisedBucketCashSweep {
		t.Fatal("cash_sweep is not pre-authorisable")
	}
	m := writePreAuthPolicy(t, preAuthPolicyTOML(`pre_authorised = ["cash_sweep"]`, 1))
	read, err := m.loadPolicy()
	if err != nil || !read.policy.Authority.preAuthorised("cash_sweep") {
		t.Fatalf("policy = %+v err %v", read.policy.Authority, err)
	}
	for _, latched := range []bool{false, true} {
		if code := alertProtectionAutomaticPresentationCode(automaticNoticeKey{Bucket: preAuthorisedBucketCashSweep, Latched: latched}); code != rpc.AlertPresentationProtectionAutoCashSweep {
			t.Fatalf("latched=%v presentation = %s", latched, code)
		}
	}
}

// sweepPreviewRig is the order-preview test server wired for one USD bill:
// its contract details by contract id, a live two-sided quote read during
// the preview, an opening position impact, an accepted WhatIf and an empty
// open-order inventory. calls counts quote reads.
type sweepPreviewRig struct {
	srv    *Server
	engine *proposalEngine
	row    rpc.TradeProposal
	now    time.Time
	line   ibkrlib.BondContractDetails
	quotes int
	drafts []rpc.OrderDraft
	// marginFactor scales the WhatIf's initial-margin change against the
	// draft's value at the assumed unit (1: the broker agrees, as in a cash
	// account); marginCcy is the currency it is reported in.
	marginFactor float64
	marginCcy    string
}

func newSweepPreviewRig(t *testing.T, now time.Time) *sweepPreviewRig {
	t.Helper()
	rig := &sweepPreviewRig{now: now, marginFactor: 1, marginCcy: "USD"}
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 1e6})
	srv.now = func() time.Time { return rig.now }
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": 60000}), now)
	cashSweepResolveBills(context.Background(), usBillSource(now), policy.Buckets.CashSweep, &plan, now)
	rig.row = cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cashSweepCurrencyOf(t, plan, "USD"))
	rig.row.Revision = "rev-sweep"
	if rig.row.Contract.ConID != 7101 || rig.row.Quantity != 55 || len(rig.row.Blockers) != 0 {
		t.Fatalf("fixture row = %+v", rig.row)
	}
	rig.line = synthBondLine(7101, synthCUSIP35, "USD", cashSweepDay(now).AddDate(0, 0, 35))
	srv.orderBondDetailsForTest = func(_ context.Context, conID int, ccy string) ([]ibkrlib.BondContractDetails, error) {
		if conID != 7101 || ccy != "USD" {
			t.Fatalf("details asked for %d %s", conID, ccy)
		}
		return []ibkrlib.BondContractDetails{rig.line}, nil
	}
	srv.orderPreviewQuote = func(_ context.Context, c rpc.ContractParams, _ time.Duration) (rpc.OrderQuoteSnapshot, error) {
		rig.quotes++
		if c.SecType != "BILL" || c.ConID != 7101 || c.MinTick != 0.0001 {
			t.Fatalf("quoted contract = %+v", c)
		}
		bid, ask := 99.58, 99.62
		return rpc.OrderQuoteSnapshot{Symbol: c.Symbol, Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: rig.now, AsOf: rig.now}, nil
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 55, rpc.OrderPositionEffectOpen)
	srv.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		rig.drafts = append(rig.drafts, d)
		before := 1000.0
		after := before + float64(d.Quantity)*d.Bond.FacePerUnit*d.LimitPrice/100*rig.marginFactor
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true,
			Margin: &rpc.OrderMarginImpact{Currency: rig.marginCcy, InitialMarginBefore: &before, InitialMarginAfter: &after,
				CommissionCurrency: "USD", MaxCommission: new(1.0)}}, nil
	}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: rig.now}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, nil
	}
	rig.srv = srv
	rig.engine = &proposalEngine{server: srv, now: func() time.Time { return rig.now }, queued: &queuedAuthStore{},
		resolve: func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			return rig.row, rig.row.Blockers, nil
		}}
	return rig
}

func (r *sweepPreviewRig) preview(t *testing.T) rpc.TradeProposalPreviewResult {
	t.Helper()
	out, err := r.engine.Preview(context.Background(), rpc.TradeProposalPreviewParams{Key: r.row.Key, Revision: r.row.Revision})
	if err != nil {
		t.Fatalf("preview err = %v", err)
	}
	return out
}

// An active sweep row previews its bill as a BOND LMT DAY order: the grid
// from contract details, a patient limit on the line's tick from the live
// quote, notional at face × price / 100, WhatIf on the built order, and
// every gate an ordinary proposal meets.
func TestCashSweepBondPreview(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC) // 10:00 in New York
	rig := newSweepPreviewRig(t, now)
	out := rig.preview(t)
	if !out.Accepted || !out.SubmitEligible || out.Preview == nil || len(out.Blockers) != 0 {
		t.Fatalf("preview = %+v", out)
	}
	d := rig.drafts[0]
	if d.Contract.SecType != "BILL" || d.Contract.ConID != 7101 || d.Quantity != 55 || d.OrderType != rpc.OrderTypeLMT || d.TIF != rpc.OrderTIFDay ||
		d.LimitPrice != 99.6 || d.Action != rpc.OrderActionBuy || d.OutsideRTH || d.Strategy != rpc.OrderStrategyPatientLimit || d.OpenClose != "O" {
		t.Fatalf("draft = %+v", d)
	}
	if b := d.Bond; b == nil || b.Instrument != cashSweepInstrumentUSTBill || b.FacePerUnit != 1000 || b.QuantityUnit != rpc.BondQuantityUnitFace1000 ||
		b.MinTick != 0.0001 || b.MinSize != 1 || b.SizeIncrement != 1 || b.FaceValue != 55000 {
		t.Fatalf("bond terms = %+v", d.Bond)
	}
	if math.Abs(out.Preview.Notional-54780) > 1e-6 {
		t.Fatalf("notional = %v, want 55 × 1,000 × 99.6 / 100", out.Preview.Notional)
	}
	// The WhatIf and place encoders receive the grid with the contract.
	contract, order := previewIBKRStrategyContract(d), previewIBKROrder(d)
	if contract.SecType != "BILL" || contract.BondRules == nil || *contract.BondRules != usBillRules || contract.Multiplier != 0 {
		t.Fatalf("broker contract = %+v", contract)
	}
	if err := ibkrlib.ValidateOrder(&ibkrlib.IBKROrder{ConID: contract.ConID, Symbol: contract.Symbol, SecType: contract.SecType, Exchange: contract.Exchange,
		Currency: contract.Currency, BondRules: contract.BondRules, Action: order.Action, TotalQty: order.TotalQty, OrderType: order.OrderType,
		LmtPrice: order.LmtPrice, LmtPriceSet: order.LmtPriceSet, TIF: order.TIF}); err != nil {
		t.Fatalf("the previewed order fails the broker's bond check: %v", err)
	}
	// A prepared submit binds the reviewed terms: the same row compares
	// equal, a row naming another bill does not.
	same, _ := json.Marshal(proposalOrderPreviewParams(rig.row, 55, 0))
	other := rig.row
	other.Contract.ConID = 7102
	moved, _ := json.Marshal(proposalOrderPreviewParams(other, 55, 0))
	if bytes.Equal(same, moved) {
		t.Fatal("prepared terms do not bind the bill")
	}
}

// A closed bill session refuses before any quote is read, with the
// readiness module's closed-session code.
func TestCashSweepBondPreviewRefusesAClosedSession(t *testing.T) {
	rig := newSweepPreviewRig(t, time.Date(2026, 9, 30, 22, 0, 0, 0, time.UTC)) // 18:00 in New York
	out := rig.preview(t)
	if out.Accepted || len(out.Blockers) != 1 || out.Blockers[0].Code != previewMarketClosedCode || rig.quotes != 0 ||
		out.Readiness == nil || out.Readiness.Code != rpc.ReadinessMarketClosed || len(rig.drafts) != 0 {
		t.Fatalf("closed preview = %+v readiness %+v quotes %d", out.Blockers, out.Readiness, rig.quotes)
	}
}

// Technical refusals: a line that is not the row's bill, a grid the quantity
// is off, a stale or one-sided quote, and a working order for the same bill.
func TestCashSweepBondPreviewRefusals(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		setup func(*sweepPreviewRig)
		code  string
	}{
		"a coupon bond": {func(r *sweepPreviewRig) { r.line.Coupon = 4.25 }, previewContractUnresolvedCode},
		"no size rules": {func(r *sweepPreviewRig) { r.line.Complete = false }, previewContractUnresolvedCode},
		"off the grid":  {func(r *sweepPreviewRig) { r.line.MinSize, r.line.SizeIncrement = 100, 100 }, previewBondOrderInvalidCode},
		"above [trading].max_notional": {func(r *sweepPreviewRig) {
			tr := r.srv.cfg.Trading
			tr.MaxNotional = 10000
			r.srv.cfg = &config.Resolved{Gateway: r.srv.cfg.Gateway, Trading: tr}
		}, previewRiskLimitCode},
		"a delayed quote": {func(r *sweepPreviewRig) {
			r.srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
				bid, ask := 99.58, 99.62
				return rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataDelayed, PriceAt: now}, nil
			}
		}, previewQuoteNotLiveCode},
		"no fresh tick": {func(r *sweepPreviewRig) {
			r.srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
				bid, ask := 99.58, 99.62
				return rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}, nil
			}
		}, previewQuoteStaleCode},
		"one-sided": {func(r *sweepPreviewRig) {
			r.srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
				ask := 99.62
				return rpc.OrderQuoteSnapshot{Ask: &ask, DataType: rpc.MarketDataLive, PriceAt: now}, nil
			}
		}, previewQuoteNotTwoSidedCode},
		"a working buy of the bill": {func(r *sweepPreviewRig) {
			r.srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
				working := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, Account: "DU1234567", ConID: 7101, SecType: "BOND", Currency: "USD",
					Action: rpc.OrderActionBuy, OrderType: "LMT", TotalQuantity: 10, Remaining: 10, LimitPrice: 99.6, Status: "Submitted"}
				return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: now, Orders: []ibkrlib.OrderLifecycleEvent{working}}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, nil
			}
		}, reductionOrderExistingCode},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newSweepPreviewRig(t, now)
			tc.setup(rig)
			out := rig.preview(t)
			if out.Accepted || !slices.ContainsFunc(out.Blockers, func(b rpc.TradingBlocker) bool { return b.Code == tc.code }) {
				t.Fatalf("blockers = %+v, want %s", out.Blockers, tc.code)
			}
		})
	}
}

// No RPC caller can preview a bond: without the proposal engine's bill terms
// the preview refuses, and the terms are never read from the wire.
func TestOrderPreviewAdmitsBondOnlyForASweepRow(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	rig := newSweepPreviewRig(t, now)
	params := proposalOrderPreviewParams(rig.row, 55, 0)
	params.Bond = nil
	if _, err := rig.srv.previewOrder(context.Background(), params); err == nil || !strings.Contains(err.Error(), "cash_sweep") {
		t.Fatalf("a bond preview without bill terms = %v", err)
	}
	raw, _ := json.Marshal(proposalOrderPreviewParams(rig.row, 55, 0))
	var decoded rpc.OrderPreviewParams
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Bond != nil || strings.Contains(string(raw), "face_per_unit") {
		t.Fatalf("bill terms crossed the wire: %s", raw)
	}
	for name, change := range map[string]func(*rpc.OrderPreviewParams){
		"an explicit limit": func(p *rpc.OrderPreviewParams) { p.LimitPrice = new(99.5) },
		"GTC":               func(p *rpc.OrderPreviewParams) { p.TIF = rpc.OrderTIFGTC },
		"outside RTH":       func(p *rpc.OrderPreviewParams) { p.OutsideRTH = true },
		"a trail":           func(p *rpc.OrderPreviewParams) { p.OrderType = rpc.OrderTypeTRAIL },
		"no contract id":    func(p *rpc.OrderPreviewParams) { p.Contract.ConID = 0 },
	} {
		p := proposalOrderPreviewParams(rig.row, 55, 0)
		change(&p)
		if _, err := rig.srv.previewOrder(context.Background(), p); err == nil {
			t.Errorf("%s: previewed", name)
		}
	}
	// A bond order that would open a short is refused whatever the config.
	draft := rpc.OrderDraft{Action: rpc.OrderActionSell, Contract: rpc.ContractParams{SecType: "BOND", Currency: "USD", ConID: 7101}, Quantity: 5}
	auth := orderNotionalAuthority{QuoteNotional: 5000, ContractCurrency: "USD", BaseNotional: 5000, BaseCurrency: "USD", BasePerContract: 1, EvidenceAt: now, Source: orderFXSourceIdentity}
	if err := validateOrderRiskAuthority(config.Trading{AllowStockShort: true}, draft, rpc.OrderPositionImpact{Before: 0, After: -5, Effect: rpc.OrderPositionEffectOpenShort}, auth, "USD", protectiveExitInventory{}); err == nil {
		t.Fatal("a bond short passed the risk authority")
	}
	if err := validateOrderRiskAuthority(config.Trading{}, draft, rpc.OrderPositionImpact{Before: 10, After: 5, Effect: rpc.OrderPositionEffectReduce}, auth, "USD", protectiveExitInventory{}); err != nil {
		t.Fatalf("a bond reduce was refused: %v", err)
	}
}

// A wrong face unit is caught by the broker's own figure (reviewer decisions
// 2026-09-30 15:25 and 15:45 CEST): a WhatIf whose initial-margin change,
// divided by the order's value at the assumed unit, falls outside [0.005,
// 1.2] refuses the preview with bill_unit_mismatch naming both figures, the
// unit and both bounds; the instrument's rows stay blocked, and no submit
// passes, until a preview checks clean. A bill margined at one percent
// (0.01) and a cash account (1.0) pass; a 1,000-fold unit error either way
// is refused in both.
func TestCashSweepBillUnitMismatch(t *testing.T) {
	now := time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)
	rig := newSweepPreviewRig(t, now)
	rig.marginFactor = 1000
	out := rig.preview(t)
	var got *rpc.TradingBlocker
	for i := range out.Blockers {
		if out.Blockers[i].Code == rpc.CashSweepBlockerBillUnitMismatch {
			got = &out.Blockers[i]
		}
	}
	if out.Accepted || got == nil || !strings.Contains(got.Message, "54780000 USD") || !strings.Contains(got.Message, "expected value 54780 USD") ||
		!strings.Contains(got.Message, "face_1000") || !strings.Contains(got.Message, "outside the band 0.005 to 1.2") ||
		out.Readiness == nil || out.Readiness.Code != rpc.ReadinessNotExecutable {
		t.Fatalf("1,000x WhatIf = %+v readiness %+v", out.Blockers, out.Readiness)
	}
	// The instrument's rows now carry the blocker: not eligible for the
	// scheduler, and a submit is refused before any preview or broker call.
	row := rig.row
	rig.engine.applyBillUnitLatch(&row)
	if row.AutomaticEligible() || len(row.Blockers) != 1 || row.Blockers[0].Code != rpc.CashSweepBlockerBillUnitMismatch {
		t.Fatalf("latched row = %+v", row.Blockers)
	}
	other := row
	other.CashSweep = &rpc.TradeProposalCashSweep{Side: rpc.CashSweepSideInvest, Currency: "EUR", Instrument: cashSweepInstrumentDEBubill}
	other.Blockers = nil
	if rig.engine.applyBillUnitLatch(&other); len(other.Blockers) != 0 {
		t.Fatal("the latch blocked another instrument")
	}
	rig.row = row
	previews := len(rig.drafts)
	for _, fastPath := range []bool{false, true} {
		submit, err := rig.engine.Submit(context.Background(), rpc.TradeProposalSubmitParams{Key: row.Key, Revision: row.Revision, FastPath: fastPath})
		if err != nil || submit.Accepted || !slices.ContainsFunc(submit.Blockers, func(b rpc.TradingBlocker) bool { return b.Code == rpc.CashSweepBlockerBillUnitMismatch }) {
			t.Fatalf("submit of a latched row = %+v err %v", submit, err)
		}
	}
	if len(rig.drafts) != previews {
		t.Fatal("a latched submit reached the broker's WhatIf")
	}
	// A 1,000-fold unit error in a margin account (a one-percent margin read
	// 1,000 times too large or too small), a cash account's figure 1,000
	// times too small, and a ratio of 0.001 are refused as well.
	for _, factor := range []float64{10, 0.00001, 0.001} {
		rig.marginFactor = factor
		if out := rig.preview(t); out.Accepted {
			t.Fatalf("a WhatIf at %g of the value passed", factor)
		}
	}
	// A preview of the latched row still runs, and a clean one clears the
	// latch: a bill margined at one percent reads 0.01.
	rig.marginFactor = 0.01
	if out := rig.preview(t); !out.Accepted {
		t.Fatalf("clean preview of a latched row = %+v", out.Blockers)
	}
	fresh := rig.row
	fresh.Blockers, fresh.State = nil, rpc.TradeProposalStateGenerated
	if rig.engine.applyBillUnitLatch(&fresh); len(fresh.Blockers) != 0 {
		t.Fatalf("a clean preview left the latch: %+v", fresh.Blockers)
	}
	// A cash account's figure (1.0) passes too.
	rig.row = fresh
	rig.marginFactor = 1
	if out := rig.preview(t); !out.Accepted {
		t.Fatalf("cash-account preview = %+v", out.Blockers)
	}
	// A figure in a third currency, or none at all, cannot vouch for the unit.
	rig.marginCcy = "JPY"
	if out := rig.preview(t); out.Accepted || !strings.Contains(fmt.Sprint(out.Blockers), "JPY") {
		t.Fatalf("margin in a third currency = %+v", out.Blockers)
	}
	check := func(m *rpc.OrderMarginImpact) bool {
		_, mismatch, checked := cashSweepBillUnitCheck(rig.row, &rpc.OrderPreviewResult{Draft: rig.drafts[len(rig.drafts)-1], Notional: 54780, NotionalBase: 54780,
			BaseCurrency: "USD", NotionalCurrency: "USD", WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Margin: m}})
		return checked && mismatch
	}
	if !check(nil) || check(&rpc.OrderMarginImpact{InitialMarginBefore: new(0.0), InitialMarginAfter: new(54780.0)}) {
		t.Fatal("margin figure handling")
	}
	// A redemption's quantity is the broker's own position count: unchecked.
	redeem := rig.row
	redeem.CashSweep = &rpc.TradeProposalCashSweep{Side: rpc.CashSweepSideRedeem, Currency: "USD", Instrument: cashSweepInstrumentUSTBill}
	if _, _, checked := cashSweepBillUnitCheck(redeem, &rpc.OrderPreviewResult{Draft: rig.drafts[0], WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted}}); checked {
		t.Fatal("a redemption was unit-checked")
	}
}
