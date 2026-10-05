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
// NLV, orders 20,000 to max(50,000, 10% of NLV), keep_cash 5,000.
func ownerSizedSweepPolicy() protectionPolicy {
	p := cashSweepTestPolicy(rpc.CashSweepModeActive, 50000)
	b := p.Buckets.CashSweep
	b.ReserveFloorBase, b.ReservePctNLV = new(10000.0), new(10.0)
	b.MinOrderNotional, b.MaxOrderPctNLV = new(20000.0), new(10.0)
	b.BillsExemptFromTradingMaxNotional = new(true)
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

// Worked check A: NLV 233,000 EUR, EUR cash 70,500, USD cash 6,800. The
// reserve is 23,300 EUR (10% of NLV); EUR invests one order of about 47,000;
// USD's free cash is below 20,000 base, so USD stays cash.
func TestCashSweepWorkedCheckBookOf233k(t *testing.T) {
	now := cashSweepTestNow()
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), sizedSweepInput(233000, map[string]float64{"EUR": 70500, "USD": 6800}), now)
	sz := plan.status.Sizing
	if sz == nil || !near(sz.ReserveBase, 23300) || sz.ReserveBound != rpc.CashSweepReserveBoundPctNLV || !near(sz.ReservePctNLVBase, 23300) ||
		sz.MinOrderBase != 20000 || sz.MaxOrderBase != 50000 || sz.MaxOrderBound != rpc.CashSweepMaxOrderBoundNotional || sz.PlannedNeedsKnown ||
		sz.PlannedNeedsReason == "" || sz.ReserveShortfallBase != 0 || !sz.TradingMaxNotionalExempt || sz.BaseCurrency != "EUR" {
		t.Fatalf("sizing = %+v", sz)
	}
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if eur.side != rpc.CashSweepSideInvest || !near(eur.orderAmount, 47200) || eur.heldToCap || eur.status.ReserveHeld == nil || !near(*eur.status.ReserveHeld, 18300) || eur.status.KeepCash != 5000 {
		t.Fatalf("EUR = %s %v %v (%s)", eur.side, eur.orderAmount, eur.heldToCap, eur.status.Reason)
	}
	usd := cashSweepCurrencyOf(t, plan, "USD")
	if usd.side != "" || usd.status.State != rpc.CashSweepStateHold || !strings.Contains(usd.status.Reason, "smallest order") || !near(usd.free, 1800) {
		t.Fatalf("USD = %+v", usd.status)
	}
	if plan.status.MaxOrderNotionalBase == nil || *plan.status.MaxOrderNotionalBase != 50000 || plan.status.MinOrderNotionalBase != 20000 {
		t.Fatalf("status caps = %v %v", plan.status.MaxOrderNotionalBase, plan.status.MinOrderNotionalBase)
	}
	eur.bill = &rpc.TradeProposalCashSweepBill{Instrument: cashSweepInstrumentDEBubill, ConID: 77, SecType: "BILL", Maturity: "2026-12-30", QuantityUnit: rpc.BondQuantityUnitFace1, PriceConvention: rpc.BondPriceConventionPer100}
	eur.units = 47200
	row := cashSweepRow(ownerSizedSweepPolicy(), rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, eur)
	s := row.CashSweep
	if s.Sizing == nil || !near(s.Sizing.ReserveBase, 23300) || s.Sizing.ReserveBound != rpc.CashSweepReserveBoundPctNLV || s.MaxOrderNotionalBase != 50000 ||
		s.MinOrderNotionalBase != 20000 || !near(s.ReserveHeld, 18300) {
		t.Fatalf("row block = %+v / %+v", s, s.Sizing)
	}
	if !slices.ContainsFunc(row.Details, func(d string) bool {
		return strings.Contains(d, "kept as cash: 23300 EUR (10% of NLV 233000 EUR), held in EUR; orders from 20000 EUR to 50000 EUR (max_order_notional)")
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
		{"planned needs above both", 233000, cashSweepPlannedNeeds{Known: true, Base: 30000}, 30000, rpc.CashSweepReserveBoundPlannedNeeds, 50000, rpc.CashSweepMaxOrderBoundNotional, true, true},
		{"planned needs below", 233000, cashSweepPlannedNeeds{Known: true, Base: 1000}, 23300, rpc.CashSweepReserveBoundPctNLV, 50000, rpc.CashSweepMaxOrderBoundNotional, true, true},
		{"nonfinite planned needs add nothing", 233000, cashSweepPlannedNeeds{Known: true, Base: math.Inf(1)}, 23300, rpc.CashSweepReserveBoundPctNLV, 50000, rpc.CashSweepMaxOrderBoundNotional, false, false},
		{"cap lifted by NLV", 600000, cashSweepPlannedNeeds{}, 60000, rpc.CashSweepReserveBoundPctNLV, 60000, rpc.CashSweepMaxOrderBoundPctNLV, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := sizedSweepInput(tc.nlv, map[string]float64{"EUR": 1000})
			in.PlannedNeeds = tc.planned
			sz, reason := cashSweepSizingFor(ownerSizedSweepPolicy().Buckets.CashSweep, in)
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
	in := sizedSweepInput(233000, map[string]float64{"EUR": 70500})
	for _, key := range []string{"max_order_notional", "max_order_pct_nlv", "min_order_notional", "reserve_floor_base", "reserve_pct_nlv"} {
		t.Run(key, func(t *testing.T) {
			p := ownerSizedSweepPolicy()
			b := p.Buckets.CashSweep
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
		eurTable := p.Buckets.CashSweep.Currency["EUR"]
		eurTable.KeepCash = nil
		p.Buckets.CashSweep.Currency["EUR"] = eurTable
		eur := cashSweepCurrencyOf(t, cashSweepPlanFor(p, in, now), "EUR")
		if eur.side != "" || eur.status.State != rpc.CashSweepStateNeedsYourNumber || !strings.Contains(eur.status.Reason, "keep_cash") || !slices.Contains(eur.status.NeedsYourNumber, "keep_cash") {
			t.Fatalf("missing keep_cash: %+v", eur.status)
		}
		// The bucket's keep_cash serves every currency without its own.
		p.Buckets.CashSweep.KeepCash = new(7000.0)
		eur = cashSweepCurrencyOf(t, cashSweepPlanFor(p, in, now), "EUR")
		if eur.side != rpc.CashSweepSideInvest || eur.status.KeepCash != 7000 {
			t.Fatalf("bucket keep_cash: %+v", eur.status)
		}
	})
	t.Run("parse keeps absent numbers absent", func(t *testing.T) {
		p, _, err := parseProtectionPolicy([]byte(cashSweepPolicyHead + "[buckets.cash_sweep]\nenabled = true\nmax_order_notional = 10000\n[buckets.cash_sweep.currency.EUR]\nfallback = \"none\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		b := p.Buckets.CashSweep
		if _, ok := b.keepCash("EUR"); ok || b.MinOrderNotional != nil || b.Currency["EUR"].MinTranche != nil ||
			!slices.Equal(b.missingNumbers(), []string{"max_order_pct_nlv", "min_order_notional", "reserve_floor_base", "reserve_pct_nlv"}) || b.billsExempt() {
			t.Fatalf("absent numbers read as values: %+v", b)
		}
	})
}

// Fail closed: a percentage term with no readable NLV holds every currency,
// with no sizing and no order; with both percentages 0 NLV is not needed.
func TestCashSweepUnreadableNLVHolds(t *testing.T) {
	now := cashSweepTestNow()
	in := sizedSweepInput(233000, map[string]float64{"EUR": 70500})
	in.NLVBase = nil
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), in, now)
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if plan.status.Sizing != nil || eur.side != "" || eur.status.State != rpc.CashSweepStateHold || !strings.Contains(eur.status.Reason, "net liquidation value is unavailable") {
		t.Fatalf("unreadable NLV = %+v", eur.status)
	}
	p := ownerSizedSweepPolicy()
	p.Buckets.CashSweep.ReservePctNLV, p.Buckets.CashSweep.MaxOrderPctNLV = new(0.0), new(0.0)
	plan = cashSweepPlanFor(p, in, now)
	if eur = cashSweepCurrencyOf(t, plan, "EUR"); plan.status.Sizing == nil || eur.side != rpc.CashSweepSideInvest || !near(eur.orderAmount, 50000) {
		t.Fatalf("fixed-only sizing = %+v (%s)", plan.status.Sizing, eur.status.Reason)
	}
}

// Rule 2: a redemption restoring the reserve is never held to the minimum:
// EUR cash 15,000 against a 23,300 reserve sells the 8,300 gap.
func TestCashSweepRedemptionBelowMinimumRestoresReserve(t *testing.T) {
	in := sizedSweepInput(233000, map[string]float64{"EUR": 15000})
	in.Holdings["EUR"] = eurRedeemInput(50000).Holdings["EUR"]
	eur := cashSweepCurrencyOf(t, cashSweepPlanFor(ownerSizedSweepPolicy(), in, cashSweepTestNow()), "EUR")
	if eur.side != rpc.CashSweepSideRedeem || !near(eur.gap, 8300) || !near(eur.orderAmount, 8300) || eur.quantity != int(math.Ceil(8300/0.995)) ||
		!strings.Contains(eur.status.Reason, "below the reserve") {
		t.Fatalf("EUR = %s gap %v amount %v qty %d (%s)", eur.side, eur.gap, eur.orderAmount, eur.quantity, eur.status.Reason)
	}
	prop, preview := sweepFeePreview(30000, 8300, 10)
	prop.CashSweep.Side, prop.CashSweep.MinOrderNotionalBase, prop.CashSweep.MinTranche, prop.CashSweep.RedemptionTarget = rpc.CashSweepSideRedeem, 20000, 0, 8300
	preview.Draft.Quantity, preview.Draft.LimitPrice = 8300, 100
	if got := cashSweepEconomicsBlockers(prop, preview); hasTradingBlocker(got, "cash_sweep_below_minimum_tranche") {
		t.Fatalf("a reserve redemption was held to the minimum: %v", got)
	}
}

// The base reserve's shortfall is kept in the other currencies before they
// invest; unknown base cash holds their buys.
func TestCashSweepReserveShortfallCarriesToOtherCurrencies(t *testing.T) {
	now := cashSweepTestNow()
	// EUR 10,000 holds 10,000 of the 23,300 reserve; 13,300 EUR is kept in
	// USD (14,777.78 USD at 0.9) before USD invests.
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), sizedSweepInput(233000, map[string]float64{"EUR": 10000, "USD": 60000}), now)
	if plan.status.Sizing == nil || !near(plan.status.Sizing.ReserveShortfallBase, 13300) {
		t.Fatalf("sizing = %+v", plan.status.Sizing)
	}
	usd := cashSweepCurrencyOf(t, plan, "USD")
	carry := 13300 / 0.9
	if usd.side != rpc.CashSweepSideInvest || usd.status.ReserveHeld == nil || !near(*usd.status.ReserveHeld, carry) || !near(usd.free, 55000-carry) || !near(usd.orderAmount, 55000-carry) {
		t.Fatalf("USD = %s %v (%s)", usd.side, usd.orderAmount, usd.status.Reason)
	}
	// A shortfall the foreign surplus cannot clear above the minimum holds it.
	plan = cashSweepPlanFor(ownerSizedSweepPolicy(), sizedSweepInput(233000, map[string]float64{"EUR": 0, "USD": 40000}), now)
	if usd = cashSweepCurrencyOf(t, plan, "USD"); usd.side != "" {
		t.Fatalf("USD invested past the reserve: %s", usd.status.Reason)
	}
	// Base cash unknown: USD cannot show the reserve funded and holds.
	in := sizedSweepInput(233000, map[string]float64{"USD": 60000})
	usd = cashSweepCurrencyOf(t, cashSweepPlanFor(ownerSizedSweepPolicy(), in, now), "USD")
	if usd.side != "" || !strings.Contains(usd.status.Reason, "the reserve is held in EUR") {
		t.Fatalf("USD with unknown EUR cash = %+v", usd.status)
	}
}

// Rule 4: the exemption boundary. Only a same-currency sweep bill order
// within the sweep's cap in force passes the order cap in force, here scaled
// with the book: 5% of NLV 233,800 EUR is 11,690 EUR.
func TestCashSweepTradingCapExemptionBoundary(t *testing.T) {
	cfg := risk.EvaluateOrderLimits(testOrderLimitsTable(10000), "EUR", risk.OrderLimitsNLV{Base: 233800, AsOf: time.Now()}, nil, "")
	if cfg.CapBase != 11690 || cfg.CapBound != risk.OrderCapBoundPctNLV {
		t.Fatalf("cap in force = %v (%s), want 11690 bound by pct of NLV", cfg.CapBase, cfg.CapBound)
	}
	bill := func(ccy, secType, instrument string, cap float64) rpc.OrderDraft {
		return rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 30, Contract: rpc.ContractParams{ConID: 9, SecType: secType, Currency: ccy},
			Bond: &rpc.OrderBondTerms{Instrument: instrument, QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100, TradingCapExemptUpToBase: cap}}
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
			err := validateOrderRiskAuthority(cfg, tc.draft, tc.position, protectiveExitTestNotional(tc.notional), "EUR", protectiveExitInventory{})
			if tc.pass && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.pass && (err == nil || !strings.Contains(err.Error(), "order cap in force 11,690 EUR") && !strings.Contains(err.Error(), "bond order")) {
				t.Fatalf("admitted or wrong refusal: %v", err)
			}
		})
	}
	// Below the cap in force nothing needs the exemption: 11,000 EUR passes
	// the scaled cap though it is above the 10,000 floor.
	if err := validateOrderRiskAuthority(cfg, stock, open, protectiveExitTestNotional(11000), "EUR", protectiveExitInventory{}); err != nil {
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

[buckets.cash_sweep]
enabled = true
mode = "active"
max_order_notional = 10000

[buckets.cash_sweep.currency.EUR]
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
	want := []string{"added buckets.cash_sweep.max_order_pct_nlv = 10.0", "added buckets.cash_sweep.min_order_notional = 20000.0",
		"added buckets.cash_sweep.reserve_floor_base = 10000.0", "added buckets.cash_sweep.reserve_pct_nlv = 10.0", "added buckets.cash_sweep.keep_cash = 5000.0",
		"added buckets.cash_sweep.bills_exempt_from_trading_max_notional = true", "raised policy_version 12 to 13: the sweep sizing numbers above take effect"}
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
	b := p.Buckets.CashSweep
	eurKeep, _ := b.keepCash("EUR")
	usdKeep, usdOK := b.keepCash("USD")
	if p.PolicyVersion != 13 || b.MaxOrderNotional != 10000 || eurKeep != 6000 || !usdOK || usdKeep != 5000 || len(b.missingNumbers()) != 0 || !b.billsExempt() ||
		*b.MinOrderNotional != 20000 || *b.ReservePctNLV != 10 || *b.MaxOrderPctNLV != 10 || *b.ReserveFloorBase != 10000 {
		t.Fatalf("materialised = version %d %+v", p.PolicyVersion, b)
	}
	// A second pass has nothing to add.
	for _, a := range EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true}) {
		if a.Policy == PolicyFileProtection && a.Action != PolicyFileUnchanged {
			t.Fatalf("second pass = %+v", a)
		}
	}
	// No sweep table: nothing is added, and the sweep stays off.
	writePolicyTestFile(t, set.Protection, strings.Split(ownerLikeSweepProtection, "[buckets.cash_sweep]")[0])
	for _, a := range EnsurePolicyFiles(set, EnsureOptions{Release: "test", DryRun: true}) {
		if a.Policy == PolicyFileProtection && a.Action != PolicyFileUnchanged {
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
	sweep := *before.Buckets.CashSweep
	sweep.MinOrderNotional, sweep.KeepCash = new(20000.0), new(5000.0)
	after.Buckets.CashSweep = &sweep
	if !protectionMaterialisationPreserves(before, after) {
		t.Fatal("writing missing numbers refused")
	}
	changed := sweep
	changed.MaxOrderNotional = 50000
	after.Buckets.CashSweep = &changed
	if protectionMaterialisationPreserves(before, after) {
		t.Fatal("an owner value changed")
	}
	other := sweep
	other.Mode = "shadow"
	after.Buckets.CashSweep = &other
	if protectionMaterialisationPreserves(before, after) {
		t.Fatal("a non-sizing value changed")
	}
}
