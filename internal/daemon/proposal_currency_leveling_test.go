package daemon

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Synthetic books for every leveling test (none of it is an account's): EUR
// base, illustrative rates (EUR.USD 1.17), an order cap in force of 50,000
// EUR, and illustrative interest rates as the broker's statements would show
// them (loan / cash): EUR 3.4 / 1.4, USD 5.1 / 3.3, CHF 1.5 / 0.0, AUD
// 5.6 / 3.6, GBP 5.5 / 3.5, JPY 1.6 / 0.0.

// levelingPolicy is the table at the values policy ensure writes, enabled.
func levelingPolicy() *protectionCurrencyLevelingPolicy {
	p := defaultCurrencyLevelingPolicy()
	p.Enabled = true
	return p
}

// levelingRate is a currency measured on both sides through 2026-10-05.
func levelingRate(loan, cash float64) currencyLevelingRate {
	return currencyLevelingRate{Loan: new(loan), Cash: new(cash), LoanThrough: "2026-10-05", CashThrough: "2026-10-05"}
}

// levelingRates are the illustrative rates of every synthetic currency.
func levelingRates() map[string]currencyLevelingRate {
	return map[string]currencyLevelingRate{
		"EUR": levelingRate(0.034, 0.014), "USD": levelingRate(0.051, 0.033), "CHF": levelingRate(0.015, 0),
		"AUD": levelingRate(0.056, 0.036), "GBP": levelingRate(0.055, 0.035), "JPY": levelingRate(0.016, 0),
	}
}

// levelingRateToBase is each synthetic currency's ledger rate (EUR per unit).
var levelingRateToBase = map[string]float64{"EUR": 1, "USD": 1 / 1.17, "CHF": 1.07, "AUD": 0.565, "GBP": 1.15, "JPY": 0.0058}

// levelingBook is a ledger of the given trade-date cash per currency.
func levelingBook(cash map[string]float64) currencyLevelingInput {
	in := currencyLevelingInput{BaseCurrency: "EUR", Working: map[string]bool{}, OrderCapBase: 50000, Ledger: map[string]cashSweepLedgerRow{},
		Rates: levelingRates(), RatesThrough: "2026-10-05"}
	for ccy, v := range cash {
		in.Ledger[ccy] = cashSweepLedgerRow{Observed: true, TradeDate: v, ExchangeRate: levelingRateToBase[ccy]}
	}
	// The commission minimum is valued at the ledger's USD rate.
	if _, ok := in.Ledger["USD"]; !ok {
		in.Ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: 0, ExchangeRate: levelingRateToBase["USD"]}
	}
	return in
}

// levelingInput is a ledger with USD and EUR cash.
func levelingInput(usd, eur float64) currencyLevelingInput {
	return levelingBook(map[string]float64{"USD": usd, "EUR": eur})
}

func levelingStatus(t *testing.T, plan currencyLevelingPlan, ccy string) rpc.TradeProposalCurrencyLevelingCurrency {
	t.Helper()
	for _, c := range plan.status.Currencies {
		if c.Currency == ccy {
			return c
		}
	}
	t.Fatalf("no %s verdict in %+v", ccy, plan.status.Currencies)
	return rpc.TradeProposalCurrencyLevelingCurrency{}
}

// levelingBundleFor is the loan's bundle, or nil.
func levelingBundleFor(plan currencyLevelingPlan, ccy string) *currencyLevelingBundle {
	for i := range plan.bundles {
		if plan.bundles[i].currency == ccy {
			return &plan.bundles[i]
		}
	}
	return nil
}

// levelingResolved gives every leg its pair's contract id, as the engine
// resolves it after planning.
func levelingResolved(b currencyLevelingBundle) currencyLevelingBundle {
	b.legs = append([]currencyLevelingLeg(nil), b.legs...)
	for i := range b.legs {
		b.legs[i].contract.ConID, b.legs[i].contract.MinTick = 12087792+i, 0.00005
	}
	return b
}

// levelingLegs summarises a bundle as payer:action:pair for comparison.
func levelingLegs(b *currencyLevelingBundle) []string {
	if b == nil {
		return nil
	}
	var out []string
	for _, leg := range b.legs {
		out = append(out, leg.block.FundingCurrency+":"+leg.action+":"+leg.block.Pair)
	}
	return out
}

// levelingBandHolds checks a bundle never lands its loan above the cushion,
// even with every conversion filling at the far edge of its price bound, and
// that no conversion's own target passes the cushion.
func levelingBandHolds(t *testing.T, b *currencyLevelingBundle, slipBP float64) {
	t.Helper()
	cash, far := b.legs[0].block.Cash, 0.0
	for _, leg := range b.legs {
		x := leg.block
		q := float64(leg.quantity)
		received := q
		if leg.action == rpc.OrderActionSell {
			received = q * x.PlanningPrice * (1 + slipBP/10000)
		}
		if x.Cash+received > x.Target+1e-6 {
			t.Fatalf("leg %d: a fill at the bound brings %s to %.2f, above its share %.2f", x.Leg, x.Currency, x.Cash+received, x.Target)
		}
		if x.Target > x.CushionBase/x.ExchangeRate+1e-6 {
			t.Fatalf("leg %d: target %.2f passes the cushion", x.Leg, x.Target)
		}
		spent := q * x.PlanningPrice * (1 + slipBP/10000)
		if leg.action == rpc.OrderActionSell {
			spent = q
		}
		if spent > x.Allotment+1e-6 {
			t.Fatalf("leg %d: spends up to %.2f %s, more than its allotment %.2f", x.Leg, spent, x.FundingCurrency, x.Allotment)
		}
		far += received
	}
	if cushion := b.legs[0].block.CushionBase / b.legs[0].block.ExchangeRate; cash+far > cushion+1e-6 {
		t.Fatalf("the bundle can bring %s to %.2f, above the cushion %.2f", b.currency, cash+far, cushion)
	}
}

func TestCurrencyLevelingWorkedExample(t *testing.T) {
	plan := currencyLevelingPlanFor(levelingPolicy(), levelingInput(-20000, 60000))
	st := plan.status
	if st.BalanceSource != rpc.CurrencyLevelingBalanceTradeDate || st.OrderCapBase == nil || *st.OrderCapBase != 50000 ||
		st.RatesSource != rpc.CurrencyLevelingRatesBrokerStatements || st.PaybackDays == nil || *st.PaybackDays != 30 {
		t.Fatalf("status = %+v, want trade-date cash, the order cap in force, statement rates and 30 days", st)
	}
	usd := levelingStatus(t, plan, "USD")
	if usd.State != rpc.CurrencyLevelingStateConvert || usd.Role != rpc.CurrencyLevelingRoleLoan || usd.LoanRate == nil || *usd.LoanRate != 0.051 {
		t.Fatalf("USD = %+v, want a loan to convert at 5.1%%", usd)
	}
	bundle := levelingBundleFor(plan, "USD")
	if got := levelingLegs(bundle); len(got) != 1 || got[0] != "EUR:SELL:EUR.USD" {
		t.Fatalf("legs = %v, want one SELL EUR.USD from EUR", got)
	}
	leg := bundle.legs[0]
	b := leg.block
	if leg.contract.SecType != "CASH" || leg.contract.Exchange != "IDEALPRO" || b.Leg != 1 || b.Legs != 1 {
		t.Fatalf("conversion = %+v %+v, want CASH on IDEALPRO, 1 of 1", leg.contract, b)
	}
	// The band: at least 20,000 USD at 1.17 × (1 − 2 bp), 17,098 EUR; at
	// most 20,000 + 292.50 (250 EUR) USD at 1.17 × (1 + 2 bp), 17,340 EUR.
	// The row takes the middle.
	if leg.quantity != 17219 {
		t.Fatalf("quantity = %d EUR, want 17219", leg.quantity)
	}
	// A single conversion's share of the band is the whole band: it may
	// bring USD to the cushion, 250 EUR = 292.50 USD, never above.
	if math.Abs(b.Target-292.5) > 1e-6 {
		t.Fatalf("target %.4f, want 292.50: the whole cushion for a single conversion", b.Target)
	}
	levelingBandHolds(t, bundle, 2)
	if b.LoanRate != 0.051 || b.FundingRate != 0.014 || b.PaybackDays != 30 || b.SavingBase <= b.CostBase {
		t.Fatalf("economics = %+v, want 5.1%% against 1.4%% paying back within 30 days", b)
	}
	if eur := levelingStatus(t, plan, "EUR"); eur.State != rpc.CurrencyLevelingStatePays || eur.Role != rpc.CurrencyLevelingRolePayer || eur.PaysBase <= 0 {
		t.Fatalf("EUR = %+v, want it to pay", eur)
	}
	resolved := levelingResolved(*bundle)
	row := currencyLevelingRow(protectionPolicy{Cash: protectionCashPolicy{Leveling: levelingPolicy()}}, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{},
		time.Date(2026, 10, 5, 19, 28, 0, 0, time.UTC), plan.status, resolved, resolved.legs[0])
	if want := "Convert 17,219 EUR into about 20,146 USD: USD cash is −20,000 USD, a margin loan beyond the 10,000 EUR band; this brings it to about +146 USD"; row.Reason != want {
		t.Fatalf("reason = %q\nwant      %q", row.Reason, want)
	}
	if row.Shadow || len(row.Blockers) != 0 || !row.AutomaticEligible() || row.CurrencyLeveling == nil || row.Contract.LocalSymbol != "EUR.USD" || row.Contract.ConID != 12087792 {
		t.Fatalf("row = %+v, want an unblocked conversion on the pair's exact contract", row)
	}
	if _, ok := currencyLevelingReduceException(row); !ok {
		t.Fatalf("row = %+v, want it inside the reduce-only exception", row)
	}
	if blockers := currencyLevelingSingleApprovalBlockers(row); len(blockers) != 0 {
		t.Fatalf("a single conversion is approved on its own; got %+v", blockers)
	}
	// The key binds the contract: another contract id is another row.
	if currencyLevelingKey("USD", "EUR.USD", rpc.OrderActionSell, 12087792) == currencyLevelingKey("USD", "EUR.USD", rpc.OrderActionSell, 1) {
		t.Fatal("the key does not bind the pair's contract id")
	}
}

func TestCurrencyLevelingNeverConvertsBeyondTheCushion(t *testing.T) {
	for _, usd := range []float64{-12000, -20000, -40000, -55000} {
		plan := currencyLevelingPlanFor(levelingPolicy(), levelingInput(usd, 500000))
		b := levelingBundleFor(plan, "USD")
		if b == nil {
			t.Fatalf("USD %.0f: %+v, want a conversion", usd, levelingStatus(t, plan, "USD"))
		}
		levelingBandHolds(t, b, 2)
	}
}

func TestCurrencyLevelingTriggerBand(t *testing.T) {
	// −11,000 USD is −9,402 EUR: inside the 10,000 EUR band.
	plan := currencyLevelingPlanFor(levelingPolicy(), levelingInput(-11000, 60000))
	if c := levelingStatus(t, plan, "USD"); c.State != rpc.CurrencyLevelingStateInBand || levelingBundleFor(plan, "USD") != nil || !strings.Contains(c.Reason, "within the 10,000 EUR band") {
		t.Fatalf("USD = %+v, want in_band", c)
	}
}

func TestCurrencyLevelingDeliberateCarry(t *testing.T) {
	p := levelingPolicy()
	p.Currency = map[string]protectionCurrencyLevelingCurrency{"USD": {DeliberateCarry: true}}
	plan := currencyLevelingPlanFor(p, levelingInput(-20000, 60000))
	if c := levelingStatus(t, plan, "USD"); c.State != rpc.CurrencyLevelingStateDeliberateCarry || len(plan.bundles) != 0 {
		t.Fatalf("USD = %+v, want deliberate_carry and no conversion", c)
	}
	// A carried currency never pays either.
	p.Currency = map[string]protectionCurrencyLevelingCurrency{"EUR": {DeliberateCarry: true}}
	plan = currencyLevelingPlanFor(p, levelingInput(-20000, 60000))
	if c := levelingStatus(t, plan, "USD"); c.State != rpc.CurrencyLevelingStateHold || len(plan.bundles) != 0 || !strings.Contains(c.Reason, "never borrows") {
		t.Fatalf("USD with EUR carried = %+v, want hold: nothing may pay", c)
	}
}

func TestCurrencyLevelingUnknownInputsHold(t *testing.T) {
	in := levelingInput(-20000, 60000)
	in.Ledger["USD"] = cashSweepLedgerRow{Observed: false, ExchangeRate: 1 / 1.17}
	if c := levelingStatus(t, currencyLevelingPlanFor(levelingPolicy(), in), "USD"); c.State != rpc.CurrencyLevelingStateCashUnavailable {
		t.Fatalf("USD = %+v, want cash_unavailable", c)
	}
	for name, edit := range map[string]func(*currencyLevelingInput){
		"payer cash unknown": func(in *currencyLevelingInput) {
			in.Ledger["EUR"] = cashSweepLedgerRow{Observed: false, ExchangeRate: 1}
		},
		"stale ledger": func(in *currencyLevelingInput) { in.LedgerReason = "the ledger predates a confirmed fill" },
		"no order cap": func(in *currencyLevelingInput) {
			in.OrderCapBase, in.OrderCapReason = 0, "[order_limits] is incomplete"
		},
		"no rates": func(in *currencyLevelingInput) {
			in.Rates, in.RatesReason = nil, "the broker's daily statements are unavailable"
		},
		"loan rate unknown":  func(in *currencyLevelingInput) { delete(in.Rates, "USD") },
		"payer rate unknown": func(in *currencyLevelingInput) { delete(in.Rates, "EUR") },
		"commitments": func(in *currencyLevelingInput) {
			in.CommittedUnknown = map[string]string{"": "a working buy carries no currency"}
		},
		"no USD rate": func(in *currencyLevelingInput) {
			*in = levelingBook(map[string]float64{"GBP": -12000, "EUR": 60000})
			delete(in.Ledger, "USD")
		},
	} {
		in := levelingInput(-20000, 60000)
		edit(&in)
		plan := currencyLevelingPlanFor(levelingPolicy(), in)
		if len(plan.bundles) != 0 {
			t.Fatalf("%s: planned %v, want a hold", name, levelingLegs(&plan.bundles[0]))
		}
	}
	in = levelingInput(-20000, 60000)
	in.OrderCapBase, in.OrderCapReason = 0, "[order_limits] is incomplete"
	if c := levelingStatus(t, currencyLevelingPlanFor(levelingPolicy(), in), "USD"); !strings.Contains(c.Reason, "order cap in force is unavailable") {
		t.Fatalf("USD without an order cap = %+v", c)
	}
	in = levelingInput(-20000, 60000)
	delete(in.Rates, "USD")
	if c := levelingStatus(t, currencyLevelingPlanFor(levelingPolicy(), in), "USD"); !strings.Contains(c.Reason, "not in the broker's statements yet") {
		t.Fatalf("USD without a loan rate = %+v", c)
	}
}

// A side the statements never showed stands in from the currency's other
// side, in the safe direction: a loan costs at least what its cash earns.
func TestCurrencyLevelingRateStandIns(t *testing.T) {
	in := levelingInput(-20000, 60000)
	in.Rates["USD"] = currencyLevelingRate{Cash: new(0.033), CashThrough: "2026-10-05"}
	plan := currencyLevelingPlanFor(levelingPolicy(), in)
	c := levelingStatus(t, plan, "USD")
	if c.LoanRate == nil || *c.LoanRate != 0.033 || !c.LoanRateBound || levelingBundleFor(plan, "USD") == nil {
		t.Fatalf("USD = %+v, want its cash rate standing in as the loan's floor and a conversion", c)
	}
	in = levelingInput(-20000, 60000)
	in.Rates["EUR"] = currencyLevelingRate{Loan: new(0.034), LoanThrough: "2026-05-20"}
	plan = currencyLevelingPlanFor(levelingPolicy(), in)
	if c := levelingStatus(t, plan, "EUR"); c.CashRate == nil || *c.CashRate != 0.034 || !c.CashRateBound || c.CashRateThrough != "2026-05-20" {
		t.Fatalf("EUR = %+v, want its loan rate standing in as its cash's ceiling", c)
	}
}

func TestCurrencyLevelingWorkingConversionHolds(t *testing.T) {
	in := levelingInput(-20000, 60000)
	in.Working = map[string]bool{"USD": true}
	plan := currencyLevelingPlanFor(levelingPolicy(), in)
	if c := levelingStatus(t, plan, "USD"); len(plan.bundles) != 0 || !strings.Contains(c.Reason, "already working") {
		t.Fatalf("USD = %+v, want hold while a conversion works", c)
	}
	in.Working = map[string]bool{"EUR": true}
	plan = currencyLevelingPlanFor(levelingPolicy(), in)
	if c := levelingStatus(t, plan, "EUR"); len(plan.bundles) != 0 || !strings.Contains(c.Reason, "in motion") {
		t.Fatalf("EUR = %+v, want it not paying while a conversion in it works", c)
	}
	in.Working, in.WorkingReason = nil, "the broker's complete open-order list is unavailable"
	if plan := currencyLevelingPlanFor(levelingPolicy(), in); len(plan.bundles) != 0 {
		t.Fatalf("planned %v without the open-order list", levelingLegs(&plan.bundles[0]))
	}
}

// Never sell a currency that earns more than the loan costs, and never a
// conversion that does not pay back within the window.
func TestCurrencyLevelingRatesDecide(t *testing.T) {
	// A borrowed EUR (3.4%) beside USD cash earning 3.6%: no conversion.
	in := levelingInput(30000, -15000)
	in.Rates["USD"] = levelingRate(0.056, 0.036)
	plan := currencyLevelingPlanFor(levelingPolicy(), in)
	if c := levelingStatus(t, plan, "EUR"); len(plan.bundles) != 0 || !strings.Contains(c.Reason, "never sells a currency that earns more than the loan costs") {
		t.Fatalf("EUR = %+v, want hold: USD earns more than the loan costs", c)
	}
	// USD at 3.3% against the 3.4% loan: allowed, but too thin to pay back.
	plan = currencyLevelingPlanFor(levelingPolicy(), levelingInput(30000, -15000))
	if c := levelingStatus(t, plan, "EUR"); len(plan.bundles) != 0 || !strings.Contains(c.Reason, "pays for itself within 30 days") {
		t.Fatalf("EUR = %+v, want hold: nothing pays back", c)
	}
	// A longer window lets the same spread pay back.
	p := levelingPolicy()
	p.PaybackDays = new(365)
	in = levelingInput(30000, -15000)
	in.Rates["USD"] = levelingRate(0.051, 0.030)
	if plan := currencyLevelingPlanFor(p, in); levelingBundleFor(plan, "EUR") == nil {
		t.Fatalf("EUR at a 365-day window = %+v, want a conversion", levelingStatus(t, plan, "EUR"))
	}
}

// Four currencies, two loans: the dearer loan takes the cheapest cash first,
// splitting when the cheapest cannot cover it alone; the cheaper loan holds
// when what is left would not pay back, and a currency that earns more than
// it costs never repays it.
func TestCurrencyLevelingTwoLoansFourCurrencies(t *testing.T) {
	plan := currencyLevelingPlanFor(levelingPolicy(), levelingBook(map[string]float64{"EUR": 30000, "CHF": 8000, "AUD": 15000, "USD": -30000, "JPY": -2000000}))
	usd := levelingBundleFor(plan, "USD")
	if got, want := levelingLegs(usd), []string{"CHF:BUY:USD.CHF", "EUR:SELL:EUR.USD"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("USD legs = %v, want %v", got, want)
	}
	levelingBandHolds(t, usd, 2)
	// CHF gives all but its cushion: 8,000 × 1.07 − 250 = 8,310 EUR.
	if v := usd.legs[0].block.ValueBase; v < 8250 || v > 8330 {
		t.Fatalf("CHF leg = %.0f EUR, want about 8,310", v)
	}
	for i, leg := range usd.legs {
		if leg.block.Leg != i+1 || leg.block.Legs != 2 {
			t.Fatalf("leg %d numbered %d of %d", i, leg.block.Leg, leg.block.Legs)
		}
	}
	if levelingBundleFor(plan, "JPY") != nil {
		t.Fatalf("JPY repaid by %v, want hold", levelingLegs(levelingBundleFor(plan, "JPY")))
	}
	if c := levelingStatus(t, plan, "JPY"); !strings.Contains(c.Reason, "pays for itself within 30 days") {
		t.Fatalf("JPY = %+v, want hold: EUR at 1.4%% against 1.6%% does not pay back", c)
	}
	if c := levelingStatus(t, plan, "CHF"); c.State != rpc.CurrencyLevelingStatePays {
		t.Fatalf("CHF = %+v, want pays", c)
	}
	if c := levelingStatus(t, plan, "AUD"); c.State == rpc.CurrencyLevelingStatePays || c.PaysBase != 0 {
		t.Fatalf("AUD = %+v, want it unused: cheaper cash covers the USD loan and it earns more than JPY costs", c)
	}
}

// A borrowed base currency is repaid from foreign cash, cheapest first; a
// currency too close to the loan rate to pay back is left out, and the loan
// is repaid as far as the rest allows.
func TestCurrencyLevelingBorrowedBase(t *testing.T) {
	plan := currencyLevelingPlanFor(levelingPolicy(), levelingBook(map[string]float64{"EUR": -15000, "USD": 40000, "CHF": 12000}))
	b := levelingBundleFor(plan, "EUR")
	if got := levelingLegs(b); strings.Join(got, ",") != "CHF:BUY:EUR.CHF" {
		t.Fatalf("EUR legs = %v, want BUY EUR.CHF from CHF only", got)
	}
	if !b.legs[0].block.FundingShort {
		t.Fatalf("EUR leg = %+v, want funding short: USD is too thin to pay back", b.legs[0].block)
	}
	levelingBandHolds(t, b, 2)
}

// No single currency covers the loan, together they do: one bundle of three.
func TestCurrencyLevelingSplitsAcrossThree(t *testing.T) {
	plan := currencyLevelingPlanFor(levelingPolicy(), levelingBook(map[string]float64{"EUR": 9000, "CHF": 9000, "GBP": 9000, "USD": -25000}))
	b := levelingBundleFor(plan, "USD")
	if got, want := levelingLegs(b), []string{"CHF:BUY:USD.CHF", "EUR:SELL:EUR.USD", "GBP:SELL:GBP.USD"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("USD legs = %v, want %v", got, want)
	}
	if b.legs[0].block.FundingShort {
		t.Fatal("the three together cover the loan")
	}
	levelingBandHolds(t, b, 2)
}

// One payer is preferred when splitting would not repay its extra
// commission within the window.
func TestCurrencyLevelingSplitsOnlyWhenItPays(t *testing.T) {
	// 1,250 CHF (about 1,090 EUR spendable) at 0% beside ample EUR at 1.4%:
	// the extra commission outweighs the saving, so EUR pays alone.
	plan := currencyLevelingPlanFor(levelingPolicy(), levelingBook(map[string]float64{"EUR": 60000, "CHF": 1250, "USD": -20000}))
	if got := levelingLegs(levelingBundleFor(plan, "USD")); strings.Join(got, ",") != "EUR:SELL:EUR.USD" {
		t.Fatalf("USD legs = %v, want EUR alone", got)
	}
	// 6,000 CHF at 0%: the split saves more than the extra commission.
	plan = currencyLevelingPlanFor(levelingPolicy(), levelingBook(map[string]float64{"EUR": 60000, "CHF": 6000, "USD": -20000}))
	if got := levelingLegs(levelingBundleFor(plan, "USD")); strings.Join(got, ",") != "CHF:BUY:USD.CHF,EUR:SELL:EUR.USD" {
		t.Fatalf("USD legs = %v, want CHF then EUR", got)
	}
}

// Loans rank by what each unit costs: with too little cash for both, the
// dearer loan is repaid, not the larger one.
func TestCurrencyLevelingDearerLoanFirst(t *testing.T) {
	// 10,000 CHF is about 10,450 EUR to spend, less than the GBP loan alone.
	in := levelingBook(map[string]float64{"CHF": 10000, "USD": -60000, "GBP": -12000})
	in.OrderCapBase = 100000
	plan := currencyLevelingPlanFor(levelingPolicy(), in)
	if levelingBundleFor(plan, "GBP") == nil || levelingBundleFor(plan, "USD") != nil {
		t.Fatalf("bundles = GBP %v, USD %v; want GBP (5.5%%) repaid and USD (5.1%%) waiting", levelingLegs(levelingBundleFor(plan, "GBP")), levelingLegs(levelingBundleFor(plan, "USD")))
	}
	if c := levelingStatus(t, plan, "USD"); !strings.Contains(c.Reason, "never borrows") {
		t.Fatalf("USD = %+v, want hold: the CHF went to GBP", c)
	}
}

// What one loan takes is deducted before the next: allotments from a shared
// payer never add up to more than it may spend.
func TestCurrencyLevelingSharedPayerIsNeverOverdrawn(t *testing.T) {
	for _, eur := range []float64{60000, 28000} {
		in := levelingBook(map[string]float64{"EUR": eur, "USD": -20000, "GBP": -10000})
		plan := currencyLevelingPlanFor(levelingPolicy(), in)
		allotted := 0.0
		for _, b := range plan.bundles {
			for _, leg := range b.legs {
				if leg.block.FundingCurrency == "EUR" {
					allotted += leg.block.Allotment
				}
			}
			levelingBandHolds(t, &b, 2)
		}
		if allotted > eur-250+1e-6 {
			t.Fatalf("EUR %.0f: allotments %.2f, more than the %.2f it may spend", eur, allotted, eur-250)
		}
		if len(plan.bundles) == 0 {
			t.Fatalf("EUR %.0f: nothing planned", eur)
		}
	}
}

// One loan's conversions together are held to the order cap in force.
func TestCurrencyLevelingBundleHeldToTheCap(t *testing.T) {
	for _, book := range []map[string]float64{
		{"USD": -90000, "EUR": 500000},
		{"USD": -90000, "EUR": 30000, "CHF": 40000},
	} {
		plan := currencyLevelingPlanFor(levelingPolicy(), levelingBook(book))
		b := levelingBundleFor(plan, "USD")
		if b == nil {
			t.Fatalf("%v: no bundle: %+v", book, levelingStatus(t, plan, "USD"))
		}
		total := 0.0
		for _, leg := range b.legs {
			total += leg.block.ValueBase
			if !leg.block.HeldToCap {
				t.Fatalf("%v: leg %+v not marked held to the cap", book, leg.block)
			}
		}
		if total > 50000+1e-6 {
			t.Fatalf("%v: the bundle converts %.2f EUR, more than the 50,000 cap", book, total)
		}
	}
}

// The quantity of a conversion stays put across a small rate move.
func TestCurrencyLevelingKeepsThePreviousQuantity(t *testing.T) {
	in := levelingInput(-20000, 60000)
	in.Previous = map[string]int{currencyLevelingIdentity("USD", "EUR.USD", rpc.OrderActionSell): 17200}
	b := levelingBundleFor(currencyLevelingPlanFor(levelingPolicy(), in), "USD")
	if b == nil || b.legs[0].quantity != 17200 {
		t.Fatalf("quantity = %v, want the previous 17,200 kept inside the band", b)
	}
}

// A pair the broker cannot name removes that payer for the loan.
func TestCurrencyLevelingUnpairedPayer(t *testing.T) {
	in := levelingBook(map[string]float64{"EUR": 60000, "CHF": 6000, "USD": -20000})
	in.Unpaired = map[string]string{"USD.CHF": "no security definition"}
	if got := levelingLegs(levelingBundleFor(currencyLevelingPlanFor(levelingPolicy(), in), "USD")); strings.Join(got, ",") != "EUR:SELL:EUR.USD" {
		t.Fatalf("USD legs = %v, want EUR alone without USD.CHF", got)
	}
	in = levelingBook(map[string]float64{"CHF": 60000, "USD": -20000})
	in.Unpaired = map[string]string{"USD.CHF": "no security definition"}
	plan := currencyLevelingPlanFor(levelingPolicy(), in)
	if c := levelingStatus(t, plan, "USD"); len(plan.bundles) != 0 || !strings.Contains(c.Reason, "USD.CHF contract cannot be resolved") {
		t.Fatalf("USD = %+v, want hold naming the unresolved pair", c)
	}
}

func TestCurrencyLevelingNeedsEveryNumber(t *testing.T) {
	for _, edit := range []func(*protectionCurrencyLevelingPolicy){
		func(p *protectionCurrencyLevelingPolicy) { p.CushionBase = nil },
		func(p *protectionCurrencyLevelingPolicy) { p.PaybackDays = nil },
	} {
		p := levelingPolicy()
		edit(p)
		plan := currencyLevelingPlanFor(p, levelingInput(-20000, 60000))
		if c := levelingStatus(t, plan, "USD"); len(plan.bundles) != 0 || !strings.Contains(c.Reason, "needs your number:") {
			t.Fatalf("USD = %+v, want needs your number", c)
		}
	}
}

func TestCurrencyLevelingPolicyValidation(t *testing.T) {
	for name, edit := range map[string]func(*protectionCurrencyLevelingPolicy){
		"zero band":       func(p *protectionCurrencyLevelingPolicy) { p.TriggerBase = new(0.0) },
		"negative":        func(p *protectionCurrencyLevelingPolicy) { p.CushionBase = new(-1.0) },
		"zero slippage":   func(p *protectionCurrencyLevelingPolicy) { p.MaxSlippageBP = new(0.0) },
		"wide slippage":   func(p *protectionCurrencyLevelingPolicy) { p.MaxSlippageBP = new(500.0) },
		"zero payback":    func(p *protectionCurrencyLevelingPolicy) { p.PaybackDays = new(0) },
		"payback a year+": func(p *protectionCurrencyLevelingPolicy) { p.PaybackDays = new(400) },
		"lowercase table": func(p *protectionCurrencyLevelingPolicy) {
			p.Currency = map[string]protectionCurrencyLevelingCurrency{"usd": {}}
		},
	} {
		p := levelingPolicy()
		edit(p)
		if err := validateCurrencyLevelingPolicy("currency_leveling", p); err == nil {
			t.Errorf("%s: accepted %+v", name, p)
		}
	}
	if err := validateCurrencyLevelingPolicy("currency_leveling", levelingPolicy()); err != nil {
		t.Fatal(err)
	}
	// A key the table no longer has is refused, so a stale file says so.
	if _, _, err := parseProtectionPolicy([]byte(pcProtectionHead + "\n[cash.leveling]\nenabled = true\nmode = \"active\"\n")); err == nil {
		t.Fatal("a removed key was accepted")
	}
}

// policy ensure writes payback_days into a table that lacks it, leaving every
// value the owner wrote as it was (owner decision 2026-10-06 07:11 CEST).
func TestCurrencyLevelingEnsureWritesPaybackDays(t *testing.T) {
	file := pcProtectionHead + "\n[cash.leveling]\nenabled = true\ntrigger_base = 12000.0\ncushion_base = 250.0\nmax_slippage_bp = 2.0\n"
	before, _, err := parseProtectionPolicy([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	if missing := before.Cash.Leveling.missingNumbers(); strings.Join(missing, ",") != "payback_days" {
		t.Fatalf("missing = %v, want payback_days", missing)
	}
	out, changes, _, err := migrateProtectionPolicyFile([]byte(file), "v9.9.9")
	if err != nil || !slices.Contains(changes, "added cash.leveling.payback_days = 30") {
		t.Fatalf("changes = %v, %v", changes, err)
	}
	after, _, err := parseProtectionPolicy(out)
	l := after.Cash.Leveling
	if err != nil || l == nil || l.PaybackDays == nil || *l.PaybackDays != 30 || *l.TriggerBase != 12000 || !l.Enabled || len(l.missingNumbers()) != 0 {
		t.Fatalf("after ensure = %+v, %v", l, err)
	}
	if !currencyLevelingMaterialisationPreserves(before.Cash.Leveling, l) {
		t.Fatal("ensure changed a value the owner wrote")
	}
	changed := *l
	changed.PaybackDays = new(31)
	if currencyLevelingMaterialisationPreserves(before.Cash.Leveling, &changed) {
		t.Fatal("a payback_days other than the written default passed as materialisation")
	}
}
