package daemon

import (
	"math"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The reserve and order sizing (owner decisions 2026-10-05 18:35 CEST):
// reserve = the largest of reserve_floor_base, reserve_pct_nlv of NLV and the
// planned needs, held in the base currency; a buy is at least
// min_order_notional and at most the larger of max_order_notional and
// max_order_pct_nlv of NLV; a redemption is never held to the minimum; every
// number comes from the file.

// ownerSizedSweepPolicy writes the owner's numbers: floor 10,000, 10% of
// NLV, orders 20,000 to max(50,000, 10% of NLV) on a 1,000 grid, keep_cash
// 5,000.
func ownerSizedSweepPolicy() protectionPolicy {
	p := cashSweepTestPolicy(rpc.CashSweepModeActive, 50000)
	b := p.Cash.Sweep
	b.ReserveFloorBase, b.ReservePctNLV = new(10000.0), new(10.0)
	b.MinOrderNotional, b.MaxOrderPctNLV = new(20000.0), new(10.0)
	b.BillsExemptFromTradingMaxNotional = new(true)
	b.OrderStepBase = new(1000.0)
	for ccy, c := range b.Currency {
		c.MinTranche = nil
		b.Currency[ccy] = c
	}
	return p
}

func sizedSweepInput(nlv float64, cash map[string]float64) cashSweepInput {
	in := cashSweepTestInput(cash)
	in.NLVBase = new(nlv)
	in.PlannedNeeds = cashSweepPlannedNeeds{Reason: cashSweepPlannedNeedsUnavailable}
	return in
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// Worked check A: NLV 200,000 EUR, EUR cash 60,000, USD cash 6,000. The
// reserve is 20,000 EUR (10% of NLV); EUR invests one order of 40,000
// (60,000 − 20,000, under the 50,000 cap); USD's free cash of 1,000 USD is
// below 20,000 base, so USD stays cash.
func TestCashSweepWorkedCheckBookOf200k(t *testing.T) {
	now := cashSweepTestNow()
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), sizedSweepInput(200000, map[string]float64{"EUR": 60000, "USD": 6000}), now)
	sz := plan.status.Sizing
	if sz == nil || !near(sz.ReserveBase, 20000) || sz.ReserveBound != rpc.CashSweepReserveBoundPctNLV || !near(sz.ReservePctNLVBase, 20000) ||
		sz.MinOrderBase != 20000 || sz.MaxOrderBase != 50000 || sz.MaxOrderBound != rpc.CashSweepMaxOrderBoundNotional || sz.PlannedNeedsKnown ||
		sz.PlannedNeedsReason == "" || sz.ReserveShortfallBase != 0 || !sz.TradingMaxNotionalExempt || sz.BaseCurrency != "EUR" {
		t.Fatalf("sizing = %+v", sz)
	}
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if eur.side != rpc.CashSweepSideInvest || !near(eur.orderAmount, 40000) || eur.heldToCap || eur.status.ReserveHeld == nil || !near(*eur.status.ReserveHeld, 15000) || eur.status.KeepCash != 5000 {
		t.Fatalf("EUR = %s %v %v (%s)", eur.side, eur.orderAmount, eur.heldToCap, eur.status.Reason)
	}
	usd := cashSweepCurrencyOf(t, plan, "USD")
	if usd.side != "" || usd.status.State != rpc.CashSweepStateHold || !strings.Contains(usd.status.Reason, "smallest order") || !near(usd.free, 1000) {
		t.Fatalf("USD = %+v", usd.status)
	}
	if plan.status.MaxOrderNotionalBase == nil || *plan.status.MaxOrderNotionalBase != 50000 || plan.status.MinOrderNotionalBase != 20000 {
		t.Fatalf("status caps = %v %v", plan.status.MaxOrderNotionalBase, plan.status.MinOrderNotionalBase)
	}
	eur.bill = &rpc.TradeProposalCashSweepBill{Instrument: cashSweepInstrumentDEBubill, ConID: 77, SecType: "BILL", Maturity: "2026-12-30", QuantityUnit: rpc.BondQuantityUnitFace1, PriceConvention: rpc.BondPriceConventionPer100}
	eur.units = 40000
	row := cashSweepRow(ownerSizedSweepPolicy(), rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, eur)
	s := row.CashSweep
	if s.Sizing == nil || !near(s.Sizing.ReserveBase, 20000) || s.Sizing.ReserveBound != rpc.CashSweepReserveBoundPctNLV || s.MaxOrderNotionalBase != 50000 ||
		s.MinOrderNotionalBase != 20000 || !near(s.ReserveHeld, 15000) {
		t.Fatalf("row block = %+v / %+v", s, s.Sizing)
	}
	if !slices.ContainsFunc(row.Details, func(d string) bool {
		return strings.Contains(d, "kept as cash: 20000 EUR (10% of NLV 200000 EUR), held in EUR; orders from 20000 EUR to 50000 EUR (max_order_notional)")
	}) {
		t.Fatalf("details = %q", row.Details)
	}
	if terms := cashSweepOrderTerms(row); terms == nil || terms.TradingCapExemptUpToBase != 50000 {
		t.Fatalf("order terms = %+v", terms)
	}
}

// Worked check B: NLV 1.2M with 1M cash: reserve 120,000 and orders up to
// 120,000 each (10% of NLV lifts the cap above 50,000).
func TestCashSweepWorkedCheckBookOf1_2M(t *testing.T) {
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), sizedSweepInput(1200000, map[string]float64{"EUR": 1000000}), cashSweepTestNow())
	sz := plan.status.Sizing
	if sz == nil || !near(sz.ReserveBase, 120000) || sz.ReserveBound != rpc.CashSweepReserveBoundPctNLV || !near(sz.MaxOrderBase, 120000) || sz.MaxOrderBound != rpc.CashSweepMaxOrderBoundPctNLV {
		t.Fatalf("sizing = %+v", sz)
	}
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if eur.side != rpc.CashSweepSideInvest || !near(eur.orderAmount, 120000) || !eur.heldToCap || !near(eur.free, 880000) {
		t.Fatalf("EUR = %s %v %v free %v", eur.side, eur.orderAmount, eur.heldToCap, eur.free)
	}
}

// Rule 1: the reserve is the largest of its three terms, and the bound names
// which; rule 3: the cap is the larger of its two.
func TestCashSweepReserveAndCapBounds(t *testing.T) {
	for _, tc := range []struct {
		name               string
		nlv                float64
		planned            cashSweepPlannedNeeds
		reserve            float64
		bound              string
		maxOrder           float64
		maxBound           string
		plannedKnown       bool
		plannedReasonEmpty bool
	}{
		{"floor above 10% of NLV", 50000, cashSweepPlannedNeeds{}, 10000, rpc.CashSweepReserveBoundFloor, 50000, rpc.CashSweepMaxOrderBoundNotional, false, false},
		{"10% of NLV above floor", 400000, cashSweepPlannedNeeds{}, 40000, rpc.CashSweepReserveBoundPctNLV, 50000, rpc.CashSweepMaxOrderBoundNotional, false, false},
		{"planned needs above both", 200000, cashSweepPlannedNeeds{Known: true, Base: 30000}, 30000, rpc.CashSweepReserveBoundPlannedNeeds, 50000, rpc.CashSweepMaxOrderBoundNotional, true, true},
		{"planned needs below", 200000, cashSweepPlannedNeeds{Known: true, Base: 1000}, 20000, rpc.CashSweepReserveBoundPctNLV, 50000, rpc.CashSweepMaxOrderBoundNotional, true, true},
		{"nonfinite planned needs add nothing", 200000, cashSweepPlannedNeeds{Known: true, Base: math.Inf(1)}, 20000, rpc.CashSweepReserveBoundPctNLV, 50000, rpc.CashSweepMaxOrderBoundNotional, false, false},
		{"cap lifted by NLV", 600000, cashSweepPlannedNeeds{}, 60000, rpc.CashSweepReserveBoundPctNLV, 60000, rpc.CashSweepMaxOrderBoundPctNLV, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := sizedSweepInput(tc.nlv, map[string]float64{"EUR": 1000})
			in.PlannedNeeds = tc.planned
			sz, reason := cashSweepSizingFor(ownerSizedSweepPolicy().Cash.Sweep, in)
			if sz == nil || reason != "" || !near(sz.ReserveBase, tc.reserve) || sz.ReserveBound != tc.bound || !near(sz.MaxOrderBase, tc.maxOrder) || sz.MaxOrderBound != tc.maxBound ||
				sz.PlannedNeedsKnown != tc.plannedKnown || (sz.PlannedNeedsReason == "") != tc.plannedReasonEmpty {
				t.Fatalf("sizing = %+v (%s)", sz, reason)
			}
		})
	}
}

// Rule 5: a missing number holds the sweep at needs_your_number naming the
// key; no compiled value stands in.
func TestCashSweepMissingNumberHoldsNamingTheKey(t *testing.T) {
	now := cashSweepTestNow()
	in := sizedSweepInput(200000, map[string]float64{"EUR": 60000})
	for _, key := range []string{"max_order_notional", "max_order_pct_nlv", "min_order_notional", "reserve_floor_base", "reserve_pct_nlv", "order_step_base"} {
		t.Run(key, func(t *testing.T) {
			p := ownerSizedSweepPolicy()
			b := p.Cash.Sweep
			switch key {
			case "max_order_notional":
				b.MaxOrderNotional = 0
			case "max_order_pct_nlv":
				b.MaxOrderPctNLV = nil
			case "min_order_notional":
				b.MinOrderNotional = nil
			case "reserve_floor_base":
				b.ReserveFloorBase = nil
			case "reserve_pct_nlv":
				b.ReservePctNLV = nil
			case "order_step_base":
				b.OrderStepBase = nil
			}
			plan := cashSweepPlanFor(p, in, now)
			eur := cashSweepCurrencyOf(t, plan, "EUR")
			if eur.side != "" || eur.status.State != rpc.CashSweepStateNeedsYourNumber || !strings.Contains(eur.status.Reason, key) ||
				!slices.Equal(plan.status.NeedsYourNumber, []string{key}) || plan.status.Sizing != nil {
				t.Fatalf("missing %s: %+v / %v", key, eur.status, plan.status.NeedsYourNumber)
			}
		})
	}
	t.Run("keep_cash", func(t *testing.T) {
		p := ownerSizedSweepPolicy()
		eurTable := p.Cash.Sweep.Currency["EUR"]
		eurTable.KeepCash = nil
		p.Cash.Sweep.Currency["EUR"] = eurTable
		eur := cashSweepCurrencyOf(t, cashSweepPlanFor(p, in, now), "EUR")
		if eur.side != "" || eur.status.State != rpc.CashSweepStateNeedsYourNumber || !strings.Contains(eur.status.Reason, "keep_cash") || !slices.Contains(eur.status.NeedsYourNumber, "keep_cash") {
			t.Fatalf("missing keep_cash: %+v", eur.status)
		}
		// The bucket's keep_cash serves every currency without its own.
		p.Cash.Sweep.KeepCash = new(7000.0)
		eur = cashSweepCurrencyOf(t, cashSweepPlanFor(p, in, now), "EUR")
		if eur.side != rpc.CashSweepSideInvest || eur.status.KeepCash != 7000 {
			t.Fatalf("bucket keep_cash: %+v", eur.status)
		}
	})
	t.Run("parse keeps absent numbers absent", func(t *testing.T) {
		p, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[cash.sweep]\nenabled = true\nmax_order_notional = 10000\n[cash.sweep.currency.EUR]\nfallback = \"none\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		b := p.Cash.Sweep
		if _, ok := b.keepCash("EUR"); ok || b.MinOrderNotional != nil || b.Currency["EUR"].MinTranche != nil ||
			!slices.Equal(b.missingNumbers(), []string{"max_order_pct_nlv", "min_order_notional", "reserve_floor_base", "reserve_pct_nlv", "order_step_base", "no_buy_while_borrowed"}) || b.billsExempt() || b.noBuyWhileBorrowed() {
			t.Fatalf("absent numbers read as values: %+v", b)
		}
	})
}

// Fail closed: a percentage term with no readable NLV holds every currency,
// with no sizing and no order; with both percentages 0 NLV is not needed
// (EUR cash 80,000 less the 10,000 floor is held to the 50,000 cap).
func TestCashSweepUnreadableNLVHolds(t *testing.T) {
	now := cashSweepTestNow()
	in := sizedSweepInput(200000, map[string]float64{"EUR": 80000})
	in.NLVBase = nil
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), in, now)
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if plan.status.Sizing != nil || eur.side != "" || eur.status.State != rpc.CashSweepStateHold || !strings.Contains(eur.status.Reason, "net liquidation value is unavailable") {
		t.Fatalf("unreadable NLV = %+v", eur.status)
	}
	p := ownerSizedSweepPolicy()
	p.Cash.Sweep.ReservePctNLV, p.Cash.Sweep.MaxOrderPctNLV = new(0.0), new(0.0)
	plan = cashSweepPlanFor(p, in, now)
	if eur = cashSweepCurrencyOf(t, plan, "EUR"); plan.status.Sizing == nil || eur.side != rpc.CashSweepSideInvest || !near(eur.orderAmount, 50000) {
		t.Fatalf("fixed-only sizing = %+v (%s)", plan.status.Sizing, eur.status.Reason)
	}
}

// Rule 2: a redemption restoring the reserve is never held to the minimum:
// EUR cash 12,000 against a 20,000 reserve sells the 8,000 gap.
func TestCashSweepRedemptionBelowMinimumRestoresReserve(t *testing.T) {
	in := sizedSweepInput(200000, map[string]float64{"EUR": 12000})
	in.Holdings["EUR"] = eurRedeemInput(50000).Holdings["EUR"]
	eur := cashSweepCurrencyOf(t, cashSweepPlanFor(ownerSizedSweepPolicy(), in, cashSweepTestNow()), "EUR")
	if eur.side != rpc.CashSweepSideRedeem || !near(eur.gap, 8000) || !near(eur.orderAmount, 8000) || eur.quantity != int(math.Ceil(8000/0.995)) ||
		!strings.Contains(eur.status.Reason, "below the reserve") {
		t.Fatalf("EUR = %s gap %v amount %v qty %d (%s)", eur.side, eur.gap, eur.orderAmount, eur.quantity, eur.status.Reason)
	}
	prop, preview := sweepFeePreview(30000, 8000, 10)
	prop.CashSweep.Side, prop.CashSweep.MinOrderNotionalBase, prop.CashSweep.MinTranche, prop.CashSweep.RedemptionTarget = rpc.CashSweepSideRedeem, 20000, 0, 8000
	preview.Draft.Quantity, preview.Draft.LimitPrice = 8000, 100
	if got := cashSweepEconomicsBlockers(prop, preview); hasTradingBlocker(got, "cash_sweep_below_minimum_tranche") {
		t.Fatalf("a reserve redemption was held to the minimum: %v", got)
	}
}

// The base reserve's shortfall is kept in the other currencies before they
// invest; unknown base cash holds their buys.
func TestCashSweepReserveShortfallCarriesToOtherCurrencies(t *testing.T) {
	now := cashSweepTestNow()
	// EUR 12,000 holds 12,000 of the 20,000 reserve; 8,000 EUR is kept in
	// USD (8,888.89 USD at 0.9) before USD invests, and the buy rounds down
	// to the 1,000 EUR grid (1,111.11 USD steps).
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), sizedSweepInput(200000, map[string]float64{"EUR": 12000, "USD": 60000}), now)
	if plan.status.Sizing == nil || !near(plan.status.Sizing.ReserveShortfallBase, 8000) {
		t.Fatalf("sizing = %+v", plan.status.Sizing)
	}
	usd := cashSweepCurrencyOf(t, plan, "USD")
	carry, step := 8000/0.9, 1000/0.9
	if usd.side != rpc.CashSweepSideInvest || usd.status.ReserveHeld == nil || !near(*usd.status.ReserveHeld, carry) || !near(usd.free, 55000-carry) ||
		!near(usd.orderAmount, math.Floor((55000-carry)/step)*step) {
		t.Fatalf("USD = %s %v (%s)", usd.side, usd.orderAmount, usd.status.Reason)
	}
	// A shortfall the foreign surplus cannot clear above the minimum holds it.
	plan = cashSweepPlanFor(ownerSizedSweepPolicy(), sizedSweepInput(200000, map[string]float64{"EUR": 0, "USD": 40000}), now)
	if usd = cashSweepCurrencyOf(t, plan, "USD"); usd.side != "" {
		t.Fatalf("USD invested past the reserve: %s", usd.status.Reason)
	}
	// Base cash unknown: USD cannot show the reserve funded and holds.
	in := sizedSweepInput(200000, map[string]float64{"USD": 60000})
	usd = cashSweepCurrencyOf(t, cashSweepPlanFor(ownerSizedSweepPolicy(), in, now), "USD")
	if usd.side != "" || !strings.Contains(usd.status.Reason, "the reserve is held in EUR") {
		t.Fatalf("USD with unknown EUR cash = %+v", usd.status)
	}
}

// Rule 4: the exemption boundary. Only a same-currency sweep bill order
// within the sweep's cap in force passes the order cap in force, here scaled
// with the book: 5% of NLV 240,000 EUR is 12,000 EUR.
func TestCashSweepTradingCapExemptionBoundary(t *testing.T) {
	cfg := risk.EvaluateOrderLimits(testOrderLimitsTable(10000), "EUR", risk.OrderLimitsNLV{Base: 240000, AsOf: time.Now()}, nil, "")
	if cfg.CapBase != 12000 || cfg.CapBound != risk.OrderCapBoundPctNLV {
		t.Fatalf("cap in force = %v (%s), want 12000 bound by pct of NLV", cfg.CapBase, cfg.CapBound)
	}
	bill := func(ccy, secType, instrument string, cap float64) rpc.OrderDraft {
		return rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 30, Contract: rpc.ContractParams{ConID: 9, SecType: secType, Currency: ccy},
			Bond: &rpc.OrderBondTerms{Instrument: instrument, QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100, TradingCapExemptUpToBase: cap,
				// A sweep bill buy carries its bill's maturity (cashSweepOrderTerms).
				Maturity: "2027-03-15"}}
	}
	open := rpc.OrderPositionImpact{Before: 0, After: 30000, Effect: rpc.OrderPositionEffectOpen}
	sell := bill("EUR", "BILL", cashSweepInstrumentDEBubill, 50000)
	sell.Action = rpc.OrderActionSell
	stock := rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 300, Contract: rpc.ContractParams{Symbol: "AAA", SecType: "STK", Currency: "EUR"}}
	etf := bill("EUR", "ETF", cashSweepInstrumentETF, 50000)
	etf.Contract.SecType = "STK"
	conversion := bill("EUR", "CASH", cashSweepInstrumentDEBubill, 50000)
	for _, tc := range []struct {
		name     string
		draft    rpc.OrderDraft
		position rpc.OrderPositionImpact
		notional float64
		pass     bool
	}{
		{"bill buy within the sweep cap", bill("EUR", "BILL", cashSweepInstrumentDEBubill, 50000), open, 30000, true},
		{"bond-typed bill buy within the cap", bill("EUR", "BOND", cashSweepInstrumentFRBTF, 50000), open, 50000, true},
		{"bill redemption within the cap", sell, rpc.OrderPositionImpact{Before: 50000, After: 20000, Effect: rpc.OrderPositionEffectReduce}, 30000, true},
		{"bill buy over the sweep cap", bill("EUR", "BILL", cashSweepInstrumentDEBubill, 50000), open, 50001, false},
		{"bill buy without the policy exemption", bill("EUR", "BILL", cashSweepInstrumentDEBubill, 0), open, 30000, false},
		{"unreadable NLV leaves no cap", bill("EUR", "BILL", cashSweepInstrumentDEBubill, math.NaN()), open, 30000, false},
		{"bill of another currency", bill("EUR", "BILL", cashSweepInstrumentUSTBill, 50000), open, 30000, false},
		{"conversion", conversion, open, 30000, false},
		{"stock", stock, open, 30000, false},
		{"ETF", etf, open, 30000, false},
		{"buy that does not open", bill("EUR", "BILL", cashSweepInstrumentDEBubill, 50000), rpc.OrderPositionImpact{Before: -10, After: 20, Effect: rpc.OrderPositionEffectFlip}, 30000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOrderRiskAuthority(cfg, tc.draft, tc.position, protectiveExitTestNotional(tc.notional), "EUR", protectiveExitInventory{Current: true}, deltaReductionEvidence{})
			if tc.pass && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.pass && (err == nil || !strings.Contains(err.Error(), "order cap in force 12,000 EUR") && !strings.Contains(err.Error(), "bond order")) {
				t.Fatalf("admitted or wrong refusal: %v", err)
			}
		})
	}
	// Below the cap in force nothing needs the exemption: 11,000 EUR passes
	// the scaled cap though it is above the 10,000 floor.
	if err := validateOrderRiskAuthority(cfg, stock, open, protectiveExitTestNotional(11000), "EUR", protectiveExitInventory{}, deltaReductionEvidence{}); err != nil {
		t.Fatalf("stock buy within the scaled cap: %v", err)
	}
}

// The order terms carry the exemption only when the policy writes it.
func TestCashSweepOrderTermsCarryTheExemptionOnlyWhenWritten(t *testing.T) {
	row := rpc.TradeProposal{Bucket: rpc.TradeProposalBucketCashSweep, Contract: rpc.ContractParams{SecType: "BILL", Currency: "EUR"},
		CashSweep: &rpc.TradeProposalCashSweep{Instrument: cashSweepInstrumentDEBubill, Currency: "EUR", MaxOrderNotionalBase: 50000, Sizing: &rpc.CashSweepSizing{TradingMaxNotionalExempt: true}}}
	if terms := cashSweepOrderTerms(row); terms == nil || terms.TradingCapExemptUpToBase != 50000 {
		t.Fatalf("exempt terms = %+v", terms)
	}
	row.CashSweep.Sizing.TradingMaxNotionalExempt = false
	if terms := cashSweepOrderTerms(row); terms == nil || terms.TradingCapExemptUpToBase != 0 {
		t.Fatalf("unexempt terms = %+v", terms)
	}
	row.CashSweep.Sizing = nil
	if terms := cashSweepOrderTerms(row); terms == nil || terms.TradingCapExemptUpToBase != 0 {
		t.Fatalf("no sizing terms = %+v", terms)
	}
}

const ownerLikeSweepProtection = `kind = "canary.protection_policy"
schema_version = 1
policy_id = "protection-mvp"
policy_version = 12
profile = "theta-priority-mvp"

[authority]
close_reduce_only = true
auto_submit = false

[cash.sweep]
enabled = true
mode = "active"
max_order_notional = 10000

[cash.sweep.currency.EUR]
fallback = "none"
keep_cash = 6000
`

// Rule 5: policy ensure writes the missing sizing numbers into an existing
// sweep table, keeps the owner's values, raises policy_version, shows the
// keys on a dry run and backs the file up when applied.
func TestPolicyEnsureWritesMissingSweepNumbers(t *testing.T) {
	set := policyTestSet(t)
	writePolicyTestFile(t, set.Protection, ownerLikeSweepProtection)
	var preview PolicyFileAction
	for _, a := range EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true}) {
		if a.Policy == PolicyFileProtection {
			preview = a
		}
	}
	want := []string{"added cash.sweep.max_order_pct_nlv = 10.0", "added cash.sweep.min_order_notional = 20000.0",
		"added cash.sweep.reserve_floor_base = 10000.0", "added cash.sweep.reserve_pct_nlv = 10.0", "added cash.sweep.order_step_base = 1000.0", "added cash.sweep.keep_cash = 5000.0",
		"added cash.sweep.bills_exempt_from_trading_max_notional = true", "added cash.sweep.no_buy_while_borrowed = true", "added [cash.leveling] with enabled = false",
		`added [cash] with confirmation_window = "10m"`, "raised policy_version 12 to 13: the keys above take effect"}
	if preview.Action != PolicyFileWouldMigrate || !slices.Equal(preview.Changes, want) || !strings.Contains(preview.Diff, "reserve_pct_nlv = 10.0") {
		t.Fatalf("dry run = %+v", preview)
	}
	if string(readPolicyTestFile(t, set.Protection)) != ownerLikeSweepProtection {
		t.Fatal("dry run wrote the file")
	}
	got := applyReviewedTestConversions(t, set, EnsureOptions{Release: "test"})
	if len(got) != 1 || got[0].Action != PolicyFileMigrated || got[0].Backup == "" {
		t.Fatalf("apply = %+v", got)
	}
	if backup, err := os.ReadFile(got[0].Backup); err != nil || string(backup) != ownerLikeSweepProtection {
		t.Fatalf("backup = %q, %v", backup, err)
	}
	p, _, err := parseProtectionPolicy(readPolicyTestFile(t, set.Protection))
	if err != nil {
		t.Fatal(err)
	}
	b := p.Cash.Sweep
	eurKeep, _ := b.keepCash("EUR")
	usdKeep, usdOK := b.keepCash("USD")
	if p.PolicyVersion != 13 || b.MaxOrderNotional != 10000 || eurKeep != 6000 || !usdOK || usdKeep != 5000 || len(b.missingNumbers()) != 0 || !b.billsExempt() || !b.noBuyWhileBorrowed() ||
		*b.MinOrderNotional != 20000 || *b.ReservePctNLV != 10 || *b.MaxOrderPctNLV != 10 || *b.ReserveFloorBase != 10000 || *b.OrderStepBase != 1000 {
		t.Fatalf("materialised = version %d %+v", p.PolicyVersion, b)
	}
	// A second pass has nothing to add.
	for _, a := range EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true}) {
		if a.Policy == PolicyFileProtection && a.Action != PolicyFileUnchanged {
			t.Fatalf("second pass = %+v", a)
		}
	}
	// No sweep table: nothing of the sweep is added, and it stays off; only
	// the currency leveling table is written, off.
	writePolicyTestFile(t, set.Protection, strings.Split(ownerLikeSweepProtection, "[cash.sweep]")[0])
	for _, a := range EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true}) {
		if a.Policy == PolicyFileProtection && (slices.ContainsFunc(a.Changes, func(c string) bool { return strings.Contains(c, "cash_sweep") }) ||
			!slices.Contains(a.Changes, "added [cash.leveling] with enabled = false")) {
			t.Fatalf("absent sweep table = %+v", a)
		}
	}
}

// The materialisation guard refuses a conversion that would change an
// owner value.
func TestProtectionMaterialisationPreservesOwnerValues(t *testing.T) {
	before, _, err := parseProtectionPolicy([]byte(ownerLikeSweepProtection))
	if err != nil {
		t.Fatal(err)
	}
	after := before
	sweep := *before.Cash.Sweep
	sweep.MinOrderNotional, sweep.KeepCash = new(20000.0), new(5000.0)
	after.Cash.Sweep = &sweep
	if !protectionMaterialisationPreserves(before, after) {
		t.Fatal("writing missing numbers refused")
	}
	changed := sweep
	changed.MaxOrderNotional = 50000
	after.Cash.Sweep = &changed
	if protectionMaterialisationPreserves(before, after) {
		t.Fatal("an owner value changed")
	}
	other := sweep
	other.Mode = "shadow"
	after.Cash.Sweep = &other
	if protectionMaterialisationPreserves(before, after) {
		t.Fatal("a non-sizing value changed")
	}
}

// The order grid (owner decision 2026-10-06 08:22 CEST): a buy rounds down
// to order_step_base and a redemption target rounds up; no step, or a
// nonfinite amount, leaves the amount alone.
func TestCashSweepOnStep(t *testing.T) {
	for _, tc := range []struct {
		name         string
		amount, step float64
		up           bool
		want         float64
	}{
		{"buy rounds down", 40500, 1000, false, 40000},
		{"redemption rounds up", 8200, 1000, true, 9000},
		{"on the grid down", 40000, 1000, false, 40000},
		{"on the grid up", 8000, 1000, true, 8000},
		{"zero step", 40500, 0, false, 40500},
		{"negative step", 8200, -1000, true, 8200},
		{"below one step", 600, 1000, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cashSweepOnStep(tc.amount, tc.step, tc.up); !near(got, tc.want) {
				t.Fatalf("cashSweepOnStep(%v, %v, %v) = %v, want %v", tc.amount, tc.step, tc.up, got, tc.want)
			}
		})
	}
	if got := cashSweepOnStep(math.Inf(1), 1000, false); !math.IsInf(got, 1) {
		t.Fatalf("nonfinite amount = %v", got)
	}
}

// A small NLV move shifts the reserve and free cash but not the order: with
// NLV 200,000 and EUR cash 60,500 the reserve is 20,000, free 40,500 and the
// buy 40,000; at NLV 200,300 free is 40,470 and the buy is still 40,000.
func TestCashSweepOrderGridHoldsQuantityAcrossSmallNLVMoves(t *testing.T) {
	now := cashSweepTestNow()
	for _, tc := range []struct {
		nlv, reserve, free float64
	}{{200000, 20000, 40500}, {200300, 20030, 40470}} {
		plan := cashSweepPlanFor(ownerSizedSweepPolicy(), sizedSweepInput(tc.nlv, map[string]float64{"EUR": 60500}), now)
		sz := plan.status.Sizing
		if sz == nil || sz.OrderStepBase != 1000 || !near(sz.ReserveBase, tc.reserve) {
			t.Fatalf("NLV %v sizing = %+v", tc.nlv, sz)
		}
		eur := cashSweepCurrencyOf(t, plan, "EUR")
		if eur.side != rpc.CashSweepSideInvest || !near(eur.free, tc.free) || !near(eur.orderAmount, 40000) || eur.quantity != 40000 {
			t.Fatalf("NLV %v: EUR = %s free %v amount %v qty %d (%s)", tc.nlv, eur.side, eur.free, eur.orderAmount, eur.quantity, eur.status.Reason)
		}
	}
}
