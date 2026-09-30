package daemon

import (
	"context"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// cashSweepTestNow is a Wednesday, so the settlement window and the next
// business day are one calendar day either side.
func cashSweepTestNow() time.Time { return time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC) }

// cashSweepTestPolicy enables the sweep with a cap far above any fixture's
// free cash unless a test sets one.
func cashSweepTestPolicy(mode string, maxOrderNotional float64) protectionPolicy {
	p := defaultProtectionPolicy()
	p.PolicyVersion = 9
	p.Buckets.CashSweep = &protectionCashSweepPolicy{Enabled: true, Mode: mode, MaxOrderNotional: maxOrderNotional}
	return p
}

// cashSweepTestInput is a clean book in base EUR: the given trade-date cash
// per currency (USD at 0.9 EUR), a settled window, no commitments, no
// holdings.
func cashSweepTestInput(cash map[string]float64) cashSweepInput {
	in := cashSweepInput{
		BaseCurrency: "EUR",
		Ledger:       map[string]cashSweepLedgerRow{},
		Settlement:   cashSweepSettlement{Known: true, Unknown: map[string]string{}, SaleProceeds: map[string]float64{}, PurchaseCosts: map[string]float64{}, EquivalentSales: map[string]float64{}},
		Commitments:  cashSweepCommitments{Known: true, Unknown: map[string]string{}, ByCurrency: map[string]float64{}},
		Holdings:     map[string][]cashSweepHolding{},
		Unclassified: map[string]string{},
	}
	for ccy, v := range cash {
		rate := 1.0
		if ccy == "USD" {
			rate = 0.9
		}
		in.Ledger[ccy] = cashSweepLedgerRow{Observed: true, TradeDate: v, ExchangeRate: rate}
	}
	return in
}

// cashSweepTestBill is a held vocabulary bill: face value in units of 1,000
// at 99% of face, maturing days after cashSweepTestNow.
func cashSweepTestBill(conID int, ccy, instrument string, units float64, days int) cashSweepHolding {
	mv := units * 1000 * 0.99
	return cashSweepHolding{
		Row:        rpc.PositionView{Symbol: "BBB", SecType: "BOND", ConID: conID, Currency: ccy, Quantity: units, MarketValue: mv},
		Instrument: instrument, Maturity: cashSweepDay(cashSweepTestNow()).AddDate(0, 0, days), FaceValue: units * 1000, MarketValue: mv,
	}
}

func cashSweepCurrencyOf(t *testing.T, plan cashSweepPlan, ccy string) cashSweepCurrencyPlan {
	t.Helper()
	for _, cp := range plan.currencies {
		if cp.status.Currency == ccy {
			return cp
		}
	}
	t.Fatalf("no %s in plan %+v", ccy, plan.status)
	return cashSweepCurrencyPlan{}
}

// The band at each edge: free cash exactly at min_tranche holds, one unit
// above invests; cash less commitments exactly at keep_cash holds, one unit
// below redeems, or holds when nothing is held to sell.
func TestCashSweepBandEdges(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	for _, tc := range []struct {
		name     string
		cash     float64
		held     bool
		state    string
		side     string
		quantity int
	}{
		{"free equals tranche", 6000, false, rpc.CashSweepStateHold, "", 0},
		{"free one above tranche", 6001, false, rpc.CashSweepStateInvest, rpc.CashSweepSideInvest, 1001},
		{"exactly keep_cash", 5000, true, rpc.CashSweepStateHold, "", 0},
		{"one below keep_cash", 4999, true, rpc.CashSweepStateRedeem, rpc.CashSweepSideRedeem, 1},
		{"below keep_cash, nothing held", 4999, false, rpc.CashSweepStateHold, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := cashSweepTestInput(map[string]float64{"USD": tc.cash})
			if tc.held {
				in.Holdings["USD"] = []cashSweepHolding{cashSweepTestBill(801, "USD", "us_tbill", 10, 60)}
			}
			cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD")
			if cp.status.State != tc.state || cp.side != tc.side || cp.quantity != tc.quantity {
				t.Fatalf("state %s side %q qty %d; want %s %q %d (%s)", cp.status.State, cp.side, cp.quantity, tc.state, tc.side, tc.quantity, cp.status.Reason)
			}
		})
	}
}

// Commitments and settlement: working buys and armed queued buys reduce free
// cash; unsettled sale proceeds lower cash to the settled figure; unsettled
// sales of cash equivalents count toward keep_cash so a redemption is not
// repeated before it settles.
func TestCashSweepCommitmentsAndSettlement(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	in := cashSweepTestInput(map[string]float64{"USD": 60000})
	in.Commitments.ByCurrency["USD"] = 2000
	in.Settlement.SaleProceeds["USD"] = 8000
	cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD")
	st := cp.status
	if *st.TradeDateCash != 60000 || *st.SettledCash != 52000 || *st.Cash != 52000 || *st.Committed != 2000 || *st.Free != 45000 {
		t.Fatalf("figures = td %v settled %v cash %v committed %v free %v", *st.TradeDateCash, *st.SettledCash, *st.Cash, *st.Committed, *st.Free)
	}
	if cp.side != rpc.CashSweepSideInvest || cp.quantity != 45000 {
		t.Fatalf("invest = %s %d", cp.side, cp.quantity)
	}
	// Unsettled purchases raise settled above trade-date; cash is the lower.
	in = cashSweepTestInput(map[string]float64{"USD": 10000})
	in.Settlement.PurchaseCosts["USD"] = 3000
	if cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD"); *cp.status.Cash != 10000 || *cp.status.SettledCash != 13000 {
		t.Fatalf("cash %v settled %v", *cp.status.Cash, *cp.status.SettledCash)
	}
	// A redemption sold yesterday: settled cash is still low, but the
	// pending proceeds cover keep_cash, so nothing is sold again.
	in = cashSweepTestInput(map[string]float64{"USD": 6000})
	in.Settlement.SaleProceeds["USD"], in.Settlement.EquivalentSales["USD"] = 2000, 2000
	in.Holdings["USD"] = []cashSweepHolding{cashSweepTestBill(801, "USD", "us_tbill", 10, 60)}
	if cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD"); cp.status.State != rpc.CashSweepStateHold || *cp.status.PendingRedemptions != 2000 {
		t.Fatalf("pending redemption re-sold: %+v", cp.status)
	}
}

// Working buy orders from every client commit cash at their price bound;
// an armed, held or sending queued buy commits its worst price; a prepared,
// unarmed queue entry is never a commitment. A buy with no price bound makes
// its currency unknown.
func TestCashSweepCommitmentsFromOrdersAndQueue(t *testing.T) {
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	orders := []ibkrlib.OrderLifecycleEvent{
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Action: "BUY", SecType: "STK", Currency: "USD", TotalQuantity: 10, Remaining: 10, LimitPrice: 50, Account: "DU1234567"},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Action: "BUY", SecType: "OPT", Currency: "USD", TotalQuantity: 2, Remaining: 2, LimitPrice: 1.5, Multiplier: 100, Account: "DU1234567"},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Action: "SELL", SecType: "STK", Currency: "USD", TotalQuantity: 10, Remaining: 10, LimitPrice: 99, Account: "DU1234567"},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Action: "BUY", SecType: "STK", Currency: "USD", TotalQuantity: 10, Remaining: 10, LimitPrice: 99, Account: "DU7654321"},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Filled", Action: "BUY", SecType: "STK", Currency: "USD", TotalQuantity: 10, Filled: 10, LimitPrice: 99, Account: "DU1234567"},
		{Type: ibkrlib.OrderLifecycleEventOpenOrder, Status: "Submitted", Action: "BUY", SecType: "STK", Currency: "EUR", TotalQuantity: 5, Remaining: 5, OrderType: "MKT", Account: "DU1234567"},
	}
	queued := func(state string) queuedAuthRecord {
		return queuedAuthRecord{State: state, Terms: rpc.QueuedAuthTerms{AccountID: "DU1234567", AccountMode: "paper", Action: "BUY", MaxQuantity: 3, WorstPrice: 20, Currency: "USD",
			Contract: rpc.ContractParams{SecType: "STK", Currency: "USD"}}}
	}
	got := cashSweepCommitmentsFrom(orders, []queuedAuthRecord{queued(rpc.QueuedAuthPrepared), queued(rpc.QueuedAuthArmed), queued(rpc.QueuedAuthFilled)}, scope)
	if !got.Known || got.ByCurrency["USD"] != 10*50+2*1.5*100+3*20 {
		t.Fatalf("USD committed = %v (%+v)", got.ByCurrency["USD"], got)
	}
	if got.Unknown["EUR"] == "" || got.Unknown["USD"] != "" {
		t.Fatalf("unknown = %+v", got.Unknown)
	}
	// Without the prepared record's arm nothing of it counts.
	if only := cashSweepCommitmentsFrom(nil, []queuedAuthRecord{queued(rpc.QueuedAuthPrepared)}, scope); only.ByCurrency["USD"] != 0 {
		t.Fatalf("an unauthorised queue entry committed %v", only.ByCurrency["USD"])
	}
	// The planner reads an unknown commitment as settlement_unknown for that
	// currency only.
	in := cashSweepTestInput(map[string]float64{"USD": 60000, "EUR": 60000})
	in.Commitments = got
	plan := cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9), in, cashSweepTestNow())
	if s := cashSweepCurrencyOf(t, plan, "EUR").status.State; s != rpc.CashSweepStateSettlementUnknown {
		t.Fatalf("EUR = %s", s)
	}
	if s := cashSweepCurrencyOf(t, plan, "USD").status.State; s != rpc.CashSweepStateInvest {
		t.Fatalf("USD = %s", s)
	}
}

// The ladder: targets spread evenly from min to max days (O2); a tranche
// goes to the rung holding least face value, the shorter rung on a tie.
func TestCashSweepRungChoice(t *testing.T) {
	if got := cashSweepRungTargets(28, 91, 4); !slices.Equal(got, []int{28, 49, 70, 91}) {
		t.Fatalf("USD targets = %v", got)
	}
	if got := cashSweepRungTargets(28, 182, 4); !slices.Equal(got, []int{28, 79, 131, 182}) {
		t.Fatalf("EUR targets = %v", got)
	}
	if got := cashSweepRungTargets(28, 91, 1); !slices.Equal(got, []int{91}) {
		t.Fatalf("one rung = %v", got)
	}
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	in := cashSweepTestInput(map[string]float64{"USD": 60000})
	in.Holdings["USD"] = []cashSweepHolding{
		cashSweepTestBill(801, "USD", "us_tbill", 10, 30), // rung 1
		cashSweepTestBill(802, "USD", "us_tbill", 5, 68),  // rung 3
		cashSweepTestBill(803, "USD", "us_tbill", 5, 90),  // rung 4
		cashSweepTestBill(804, "USD", "us_tbill", 5, 400), // beyond 397 days: not a cash equivalent
	}
	cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD")
	if cp.side != rpc.CashSweepSideInvest || cp.rung != 2 || cp.targetDays != 49 {
		t.Fatalf("rung %d target %d side %s", cp.rung, cp.targetDays, cp.side)
	}
	faces := []float64{}
	for _, r := range cp.status.Rungs {
		faces = append(faces, r.FaceValue)
	}
	if !slices.Equal(faces, []float64{10000, 0, 5000, 5000}) || *cp.status.CashEquivalents != 20*1000*0.99 {
		t.Fatalf("rungs %v equivalents %v", faces, *cp.status.CashEquivalents)
	}
	// Empty ladder: rung 1.
	empty := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": 60000}), now), "USD")
	if empty.rung != 1 || empty.targetDays != 28 {
		t.Fatalf("empty ladder rung %d", empty.rung)
	}
}

// max_order_notional is compared in base currency at the ledger rate: an
// order above it is held to it, and a cap that holds one order below
// min_tranche holds the currency.
func TestCashSweepMaxOrderNotionalHold(t *testing.T) {
	now := cashSweepTestNow()
	in := cashSweepTestInput(map[string]float64{"USD": 60000})
	// 9,000 EUR at 0.9 EUR per USD is 10,000 USD.
	cp := cashSweepCurrencyOf(t, cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeShadow, 9000), in, now), "USD")
	if cp.side != rpc.CashSweepSideInvest || !cp.heldToCap || cp.quantity != 10000 {
		t.Fatalf("held order = %s %v %d", cp.side, cp.heldToCap, cp.quantity)
	}
	// 810 EUR is 900 USD, under the 1,000 USD tranche: hold.
	cp = cashSweepCurrencyOf(t, cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeShadow, 810), in, now), "USD")
	if cp.status.State != rpc.CashSweepStateHold || cp.side != "" || !strings.Contains(cp.status.Reason, "below min_tranche") {
		t.Fatalf("capped below tranche = %+v", cp.status)
	}
	// No number written: every currency reads needs_your_number with its
	// figures shown.
	plan := cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeShadow, 0), in, now)
	cp = cashSweepCurrencyOf(t, plan, "USD")
	if cp.status.State != rpc.CashSweepStateNeedsYourNumber || cp.side != "" || cp.status.Free == nil || !slices.Equal(plan.status.NeedsYourNumber, []string{"max_order_notional"}) || plan.status.MaxOrderNotionalBase != nil {
		t.Fatalf("no cap = %+v / %+v", cp.status, plan.status)
	}
}

// A currency without a vocabulary issuer keeps its cash as cash.
func TestCashSweepCHFHasNoInstrument(t *testing.T) {
	cp := cashSweepCurrencyOf(t, cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9), cashSweepTestInput(map[string]float64{"CHF": 90000}), cashSweepTestNow()), "CHF")
	if cp.status.State != rpc.CashSweepStateNoInstrument || cp.side != "" || *cp.status.Cash != 90000 {
		t.Fatalf("CHF = %+v", cp.status)
	}
}

// The EUR fallback ETF acts only after a completed contract search found no
// bill line; Phase A never searches, so EUR plans bills, and a missing
// fallback symbol is named without holding the bills (O3).
func TestCashSweepEURFallbackOnlyAfterAnEmptySearch(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	in := cashSweepTestInput(map[string]float64{"EUR": 60000})
	cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "EUR")
	if cp.instrument != "de_bubill" || !slices.Equal(cp.status.NeedsYourNumber, []string{"etf_symbol", "etf_exchange"}) || cp.status.State != rpc.CashSweepStateInvest {
		t.Fatalf("EUR default = %s %+v", cp.instrument, cp.status)
	}
	// Without a symbol an empty search leaves nothing usable: hold.
	in.BillSearch = map[string]cashSweepBillSearch{"EUR": {Completed: true}}
	if cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "EUR"); cp.status.State != rpc.CashSweepStateHold || cp.side != "" {
		t.Fatalf("empty search without a symbol = %+v", cp.status)
	}
	setSweepCcy(policy.Buckets.CashSweep, "EUR", func(c *protectionCashSweepCurrency) { c.ETFSymbol, c.ETFExchange = "BBB", "IBIS" })
	for _, tc := range []struct {
		search     map[string]cashSweepBillSearch
		instrument string
	}{
		{nil, "de_bubill"},
		{map[string]cashSweepBillSearch{"EUR": {Completed: false}}, "de_bubill"},
		{map[string]cashSweepBillSearch{"EUR": {Completed: true, Lines: 3}}, "de_bubill"},
		{map[string]cashSweepBillSearch{"EUR": {Completed: true}}, "etf"},
	} {
		in.BillSearch = tc.search
		cp := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "EUR")
		if cp.instrument != tc.instrument {
			t.Fatalf("search %+v chose %s, want %s", tc.search, cp.instrument, tc.instrument)
		}
		if tc.instrument == "etf" {
			// The ETF is matched by contract id, which Canary does not
			// resolve: resolution leaves no row and says why.
			plan := cashSweepPlanFor(policy, in, now)
			cashSweepResolveBills(context.Background(), &fakeBillSource{}, policy.Buckets.CashSweep, &plan, now)
			etf := cashSweepCurrencyOf(t, plan, "EUR")
			if etf.side != "" || etf.status.State != rpc.CashSweepStateInstrumentUnresolved || !strings.Contains(etf.status.Reason, "ETF") {
				t.Fatalf("ETF invest = %+v", etf.status)
			}
		}
	}
	// An ETF declared as the only instrument without its symbol holds the
	// currency with needs_your_number.
	usdETF := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	setSweepCcy(usdETF.Buckets.CashSweep, "USD", func(c *protectionCashSweepCurrency) { c.Instruments = []string{"etf"} })
	if cp := cashSweepCurrencyOf(t, cashSweepPlanFor(usdETF, cashSweepTestInput(map[string]float64{"USD": 60000}), now), "USD"); cp.status.State != rpc.CashSweepStateNeedsYourNumber {
		t.Fatalf("ETF-only without symbol = %+v", cp.status)
	}
}

// The unknown posture generates nothing and names why, in order: cash,
// settlement (or commitments), equivalents.
func TestCashSweepUnknownPostureGeneratesNothing(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	for _, tc := range []struct {
		name   string
		change func(*cashSweepInput)
		state  string
	}{
		{"ledger unavailable", func(in *cashSweepInput) { in.Ledger, in.LedgerReason = nil, "no ledger" }, rpc.CashSweepStateCashUnavailable},
		{"cash not observed", func(in *cashSweepInput) { in.Ledger["USD"] = cashSweepLedgerRow{TradeDate: 0, ExchangeRate: 0.9} }, rpc.CashSweepStateCashUnavailable},
		{"no exchange rate", func(in *cashSweepInput) { in.Ledger["USD"] = cashSweepLedgerRow{Observed: true, TradeDate: 60000} }, rpc.CashSweepStateCashUnavailable},
		{"journal gap", func(in *cashSweepInput) { in.Settlement = cashSweepSettlement{Reason: "gap"} }, rpc.CashSweepStateSettlementUnknown},
		{"fill unattributed", func(in *cashSweepInput) { in.Settlement.Unknown[""] = "a conversion" }, rpc.CashSweepStateSettlementUnknown},
		{"orders unavailable", func(in *cashSweepInput) { in.Commitments = cashSweepCommitments{Reason: "no inventory"} }, rpc.CashSweepStateSettlementUnknown},
		{"bond unclassified", func(in *cashSweepInput) { in.Unclassified["USD"] = "1 bond" }, rpc.CashSweepStateEquivalentsUnclassified},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := cashSweepTestInput(map[string]float64{"USD": 60000})
			tc.change(&in)
			if tc.name == "ledger unavailable" {
				setSweepCcy(policy.Buckets.CashSweep, "USD", func(*protectionCashSweepCurrency) {})
				defer func() { policy.Buckets.CashSweep.Currency = nil }()
			}
			plan := cashSweepPlanFor(policy, in, now)
			cp := cashSweepCurrencyOf(t, plan, "USD")
			if cp.status.State != tc.state || cp.side != "" || cp.status.Reason == "" {
				t.Fatalf("state %s side %q reason %q", cp.status.State, cp.side, cp.status.Reason)
			}
			if tc.state == rpc.CashSweepStateEquivalentsUnclassified && (cp.status.Cash == nil || cp.status.Free == nil) {
				t.Fatalf("figures dropped: %+v", cp.status)
			}
		})
	}
}

// Phase A classifies nothing: a bond or bill holding, or a holding carrying
// the declared ETF's symbol, makes that currency unclassified; the symbol
// only ever blocks.
func TestCashSweepClassifyPhaseA(t *testing.T) {
	bucket := &protectionCashSweepPolicy{Enabled: true, Currency: map[string]protectionCashSweepCurrency{}}
	eur := defaultCashSweepCurrency("EUR")
	eur.ETFSymbol, eur.ETFExchange = "BBB", "IBIS"
	bucket.Currency["EUR"] = eur
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{
		{Symbol: "AAA", SecType: "STOCK", Currency: "USD", Quantity: 100},
		{Symbol: "CCC", SecType: "BOND", Currency: "USD", Quantity: 10},
		{Symbol: "BBB", SecType: "STOCK", Currency: "EUR", Quantity: 50},
		{Symbol: "BBB", SecType: "STOCK", Currency: "USD", Quantity: 50},
	}}
	holdings, unclassified := cashSweepClassify(bucket, pos)
	if holdings != nil || unclassified["USD"] == "" || unclassified["EUR"] == "" || len(unclassified) != 2 {
		t.Fatalf("holdings %v unclassified %v", holdings, unclassified)
	}
	// No ETF declared: an ordinary stock is not a cash equivalent and blocks
	// nothing.
	_, unclassified = cashSweepClassify(&protectionCashSweepPolicy{Enabled: true}, &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "BBB", SecType: "STOCK", Currency: "EUR", Quantity: 50}}})
	if len(unclassified) != 0 {
		t.Fatalf("unclassified = %v", unclassified)
	}
	// A bond without a currency blocks every currency.
	_, unclassified = cashSweepClassify(bucket, &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "CCC", SecType: "BOND", Quantity: 1}}})
	in := cashSweepTestInput(map[string]float64{"USD": 60000, "CAD": 60000})
	in.Unclassified = unclassified
	for _, cp := range cashSweepPlanFor(cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9), in, cashSweepTestNow()).currencies {
		if cp.status.State != rpc.CashSweepStateEquivalentsUnclassified {
			t.Fatalf("%s = %s", cp.status.Currency, cp.status.State)
		}
	}
}

// A redemption sells the nearest maturity (the ETF last) in whole units
// covering the gap, a full close when that takes everything, and is skipped
// while a held bill pays out before a sale today would settle.
func TestCashSweepRedeemNearestMaturity(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	in := cashSweepTestInput(map[string]float64{"USD": 3500})
	etf := cashSweepHolding{Row: rpc.PositionView{Symbol: "BBB", SecType: "STOCK", ConID: 900, Currency: "USD", Quantity: 100, MarketValue: 5000}, Instrument: "etf", MarketValue: 5000}
	in.Holdings["USD"] = []cashSweepHolding{etf, cashSweepTestBill(802, "USD", "us_tbill", 10, 80), cashSweepTestBill(801, "USD", "us_tbill", 2, 20)}
	plan := cashSweepPlanFor(policy, in, now)
	cp := cashSweepCurrencyOf(t, plan, "USD")
	// Gap 1,500; the 20-day bill is worth 990 a unit: sell both (a close).
	if cp.side != rpc.CashSweepSideRedeem || cp.holding.Row.ConID != 801 || cp.quantity != 2 || math.Abs(cp.gap-1500) > 1e-9 {
		t.Fatalf("redeem = %s conid %d qty %d gap %v", cp.side, cp.holding.Row.ConID, cp.quantity, cp.gap)
	}
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cp)
	if row.Action != rpc.OrderActionSell || row.PositionEffect != rpc.OrderPositionEffectClose || row.SecType != "BOND" || row.Contract.ConID != 801 ||
		row.CashSweep.MaturityDate != "2026-10-20" || row.CashSweep.QuantityUnit != rpc.CashSweepQuantityPosition || row.MaxQuantity != 2 {
		t.Fatalf("redeem row = %+v / %+v", row, row.CashSweep)
	}
	// Only the ETF: a partial reduce.
	in.Holdings["USD"] = []cashSweepHolding{etf}
	cp = cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD")
	if cp.holding.Instrument != "etf" || cp.quantity != 30 {
		t.Fatalf("ETF redeem = %+v qty %d", cp.holding, cp.quantity)
	}
	// A bill paying out tomorrow (the next business day) makes the sale
	// pointless.
	in.Holdings["USD"] = []cashSweepHolding{etf, cashSweepTestBill(803, "USD", "us_tbill", 5, 1)}
	cp = cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), "USD")
	if cp.status.State != rpc.CashSweepStateHold || !strings.Contains(cp.status.Reason, "pays out on 2026-10-01") {
		t.Fatalf("pays out first = %+v", cp.status)
	}
}

// A resolved row is an ordinary proposal: active rows carry no blocker and
// are automatically eligible, shadow rows carry only shadow_mode, and every
// row waits the full veto window. The tax review is advisory (owner decision
// 2026-09-30 12:35 CEST): without it a row carries a detail line and no
// blocker, and the status says tax_reviewed false.
func TestCashSweepResolvedRowsAreOrdinaryProposals(t *testing.T) {
	now := cashSweepTestNow()
	for _, tc := range []struct {
		mode, tax string
		codes     []string
	}{
		{rpc.CashSweepModeShadow, "", []string{"shadow_mode"}},
		{rpc.CashSweepModeActive, "", nil},
		{rpc.CashSweepModeActive, "2026-09-30", nil},
	} {
		policy := cashSweepTestPolicy(tc.mode, 1e9)
		policy.Buckets.CashSweep.TaxReviewedAt = policyDate(tc.tax)
		setSweepCcy(policy.Buckets.CashSweep, "EUR", func(c *protectionCashSweepCurrency) { c.ISINs = []string{synthDEBill} })
		src := usBillSource(now)
		src.byID[synthDEBill] = []ibkrlib.BondContractDetails{synthBondLine(7301, synthDEBill, "EUR", cashSweepDay(now).AddDate(0, 0, 40))}
		src.quotes[7301] = synthLiveQuote(99.8)
		plan, _ := planAndResolve(t, policy, cashSweepTestInput(map[string]float64{"USD": 60000, "EUR": 60000, "CHF": 60000}), src)
		var rows []rpc.TradeProposal
		for _, cp := range plan.currencies {
			if cp.side != "" {
				rows = append(rows, cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cp))
			}
		}
		if len(rows) != 2 {
			t.Fatalf("%s rows = %d", tc.mode, len(rows))
		}
		for _, row := range rows {
			var codes []string
			for _, b := range row.Blockers {
				codes = append(codes, b.Code)
				if b.Message == "" || b.Action == "" {
					t.Fatalf("blocker without message or action: %+v", b)
				}
			}
			active := tc.mode == rpc.CashSweepModeActive
			// A shadow row keeps its generated state behind shadow_mode, as a
			// shadow budget row does.
			if !slices.Equal(codes, tc.codes) || !row.NeverSkipVeto || row.AutomaticEligible() != active ||
				row.State != rpc.TradeProposalStateGenerated || row.Shadow != !active || row.Bucket != rpc.TradeProposalBucketCashSweep {
				t.Fatalf("%s/%q row = %+v codes %v", tc.mode, tc.tax, row, codes)
			}
			if row.Action != rpc.OrderActionBuy || row.PositionEffect != rpc.OrderPositionEffectOpen || row.SecType != "BOND" || row.Contract.ConID <= 0 ||
				row.CashSweep.Currency != row.Contract.Currency || row.CashSweep.Bill == nil || row.Contract.ConID != row.CashSweep.Bill.ConID {
				t.Fatalf("invest row = %+v", row)
			}
			if !active && !strings.Contains(row.Blockers[0].Message, "cash sweep") {
				t.Fatalf("shadow blocker names another bucket: %+v", row.Blockers[0])
			}
			if got := slices.Contains(row.Details, rpc.CashSweepTaxUnreviewedDetail); got != (tc.tax == "") {
				t.Fatalf("%s/%q tax detail present = %v: %v", tc.mode, tc.tax, got, row.Details)
			}
		}
		if plan.status.TaxReviewed != (tc.tax != "") {
			t.Fatalf("%s/%q status tax_reviewed = %v", tc.mode, tc.tax, plan.status.TaxReviewed)
		}
	}
	// Keys are stable per currency, side and contract while the rung moves,
	// and change with the bill.
	first, again := cashSweepKey("USD", "invest", "us_tbill", 7101), cashSweepKey("USD", "invest", "us_tbill", 7101)
	if first != again || first == cashSweepKey("CAD", "invest", "us_tbill", 7101) || first == cashSweepKey("USD", "redeem", "us_tbill", 7101) ||
		first == cashSweepKey("USD", "invest", "us_tbill", 7102) {
		t.Fatal("sweep keys are not stable per currency, side and bill")
	}
	// A row built straight from the plan, without resolution, names no bill
	// and is blocked: there is nothing to order.
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": 60000}), now)
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, plan.currencies[0])
	if row.AutomaticEligible() || len(row.Blockers) != 1 || row.Blockers[0].Code != rpc.CashSweepStateInstrumentUnresolved {
		t.Fatalf("unresolved row = %+v", row)
	}
	if _, ok := cashSweepOpenException(row); ok {
		t.Fatal("an unresolved row passed the typed exception")
	}
}

// Preview and Submit refuse a shadow sweep row with the sweep's own
// shadow_mode before any broker call.
func TestCashSweepShadowRowRefusedByPreviewAndSubmit(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": 60000}), now)
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, plan.currencies[0])
	row.Revision = "rev-1"
	// Strip the row's own blockers: the shadow gate alone must refuse it.
	row.Blockers, row.State = nil, rpc.TradeProposalStateGenerated
	engine := &proposalEngine{
		server: &Server{cfg: &config.Resolved{}},
		now:    func() time.Time { return now },
		resolve: func(_ context.Context, key, revision string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			return row, nil, nil
		},
	}
	preview, err := engine.Preview(context.Background(), rpc.TradeProposalPreviewParams{Key: row.Key, Revision: row.Revision})
	if err != nil || preview.Accepted || len(preview.Blockers) != 1 || preview.Blockers[0].Code != "shadow_mode" || !strings.Contains(preview.Blockers[0].Action, "[buckets.cash_sweep]") {
		t.Fatalf("preview = %+v err %v", preview, err)
	}
	for _, fastPath := range []bool{false, true} {
		submit, err := engine.Submit(context.Background(), rpc.TradeProposalSubmitParams{Key: row.Key, Revision: row.Revision, FastPath: fastPath})
		if err != nil || submit.Accepted || submit.OrderRef != "" || len(submit.Blockers) != 1 || submit.Blockers[0].Code != "shadow_mode" {
			t.Fatalf("submit(fast_path=%v) = %+v err %v", fastPath, submit, err)
		}
	}
	// A budget row keeps its own shadow wording.
	if b := shadowProposalBlockers(rpc.TradeProposal{Bucket: rpc.TradeProposalBucketBudgetReduction, Shadow: true}); len(b) != 1 || !strings.Contains(b[0].Message, "budget reduction") {
		t.Fatalf("budget shadow blocker = %+v", b)
	}
}

// The close_reduce_only carve-out (O1) is a typed exception: a cash_sweep
// buy opening or increasing the row's own resolved bill, a vocabulary
// instrument of the row's currency, within the free cash it was planned
// against, at its own limit and within max_order_notional. Nothing else
// passes the effect gate, and BOND passes the security-type gate only there.
func TestCashSweepCloseReduceOnlyException(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeActive, 1e9)
	_, got := planAndResolve(t, policy, cashSweepTestInput(map[string]float64{"USD": 60000}), usBillSource(now))
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, cashSweepPlan{status: rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeActive}}, got["USD"])
	x, ok := cashSweepOpenException(row)
	// Free 55,000 USD buys 55 bills of 1,000 face.
	if !ok || x.Currency != "USD" || x.Instrument != "us_tbill" || x.MaxQuantity != 55 || x.ConID != row.Contract.ConID || x.FacePerUnit != 1000 || x.MaxCost != 55000 {
		t.Fatalf("exception = %+v %v", x, ok)
	}
	preview := func(qty int, limit float64, effect string) *rpc.OrderPreviewResult {
		p := &rpc.OrderPreviewResult{Draft: rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: qty, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, LimitPrice: limit,
			Contract: row.Contract, Source: proposalOrderSource, Bond: cashSweepOrderTerms(row)}, NotionalBase: float64(qty) * 1000 * limit / 100 * 0.9}
		p.Position.Effect = effect
		return p
	}
	if !x.admits(preview(55, 99.6, rpc.OrderPositionEffectOpen)) || !x.admits(preview(1, 99.6, rpc.OrderPositionEffectIncrease)) ||
		x.admits(preview(56, 99.6, rpc.OrderPositionEffectOpen)) || x.admits(preview(1, 99.6, rpc.OrderPositionEffectFlip)) || x.admits(preview(0, 99.6, rpc.OrderPositionEffectOpen)) {
		t.Fatal("admits is wider than the planned buy")
	}
	// A price above par would cost more than the free cash.
	if x.admits(preview(55, 100.5, rpc.OrderPositionEffectOpen)) {
		t.Fatal("a buy costing more than the free cash passed")
	}
	other := preview(55, 99.6, rpc.OrderPositionEffectOpen)
	other.Draft.Contract.ConID++
	if x.admits(other) {
		t.Fatal("another contract passed")
	}
	capped := x
	capped.MaxBaseNotional = 10000
	if capped.admits(preview(55, 99.6, rpc.OrderPositionEffectOpen)) {
		t.Fatal("a buy above max_order_notional passed")
	}
	for name, change := range map[string]func(*rpc.TradeProposal){
		"another bucket":       func(p *rpc.TradeProposal) { p.Bucket = rpc.TradeProposalBucketBudgetReduction },
		"a sell":               func(p *rpc.TradeProposal) { p.Action = rpc.OrderActionSell },
		"a reduce":             func(p *rpc.TradeProposal) { p.PositionEffect = rpc.OrderPositionEffectReduce },
		"a flip":               func(p *rpc.TradeProposal) { p.PositionEffect = rpc.OrderPositionEffectFlip },
		"another currency":     func(p *rpc.TradeProposal) { p.Contract.Currency = "EUR" },
		"a bill of another":    func(p *rpc.TradeProposal) { p.CashSweep.Instrument = "de_bubill" },
		"none":                 func(p *rpc.TradeProposal) { p.CashSweep.Instrument = "none" },
		"the ETF":              func(p *rpc.TradeProposal) { p.CashSweep.Instrument = "etf" },
		"outside the vocab":    func(p *rpc.TradeProposal) { p.CashSweep.Instrument = "corporate_bond" },
		"more than free cash":  func(p *rpc.TradeProposal) { p.MaxQuantity, p.Quantity = 56, 56 },
		"no sweep block":       func(p *rpc.TradeProposal) { p.CashSweep = nil },
		"a redemption block":   func(p *rpc.TradeProposal) { p.CashSweep.Side = rpc.CashSweepSideRedeem },
		"a position unit":      func(p *rpc.TradeProposal) { p.CashSweep.QuantityUnit = rpc.CashSweepQuantityPosition },
		"another unit":         func(p *rpc.TradeProposal) { p.CashSweep.QuantityUnit = rpc.BondQuantityUnitFace1 },
		"quantity above max":   func(p *rpc.TradeProposal) { p.Quantity = p.MaxQuantity + 1 },
		"no quantity":          func(p *rpc.TradeProposal) { p.Quantity = 0 },
		"no currency on block": func(p *rpc.TradeProposal) { p.CashSweep.Currency = "" },
		"no bill":              func(p *rpc.TradeProposal) { p.CashSweep.Bill = nil },
		"another contract":     func(p *rpc.TradeProposal) { p.Contract.ConID++ },
		"a stock":              func(p *rpc.TradeProposal) { p.Contract.SecType = "STK" },
		"no order cap":         func(p *rpc.TradeProposal) { p.CashSweep.MaxOrderNotionalBase = 0 },
	} {
		p := row
		block := *row.CashSweep
		p.CashSweep = &block
		change(&p)
		if _, ok := cashSweepOpenException(p); ok {
			t.Fatalf("%s passed the exception", name)
		}
	}
	// Through the preview safety gate: the sweep row passes the effect and
	// security-type checks; the same buy under another bucket fails all
	// three.
	codes := func(p rpc.TradeProposal, preview *rpc.OrderPreviewResult) []string {
		var out []string
		for _, b := range proposalPreviewSafetyBlockers(p, preview) {
			out = append(out, b.Code)
		}
		return out
	}
	if got := codes(row, preview(55, 99.6, rpc.OrderPositionEffectOpen)); len(got) != 0 {
		t.Fatalf("sweep row gate codes = %v", got)
	}
	bystander := row
	bystander.Bucket, bystander.CashSweep = rpc.TradeProposalBucketRiskReduction, nil
	if got := codes(bystander, preview(55, 99.6, rpc.OrderPositionEffectOpen)); !slices.Contains(got, "proposal_effect_not_close_reduce") ||
		!slices.Contains(got, "preview_effect_not_close_reduce") || !slices.Contains(got, "unsupported_security_type") {
		t.Fatalf("a non-sweep bond buy passed the gate: %v", got)
	}
	if got := codes(row, preview(56, 99.6, rpc.OrderPositionEffectOpen)); !slices.Contains(got, "preview_effect_not_close_reduce") {
		t.Fatalf("an oversized sweep buy passed: %v", got)
	}
	// Above par the planned units would cost more than the free cash: the
	// exception names that bound, not the effect.
	if got := codes(row, preview(55, 100.5, rpc.OrderPositionEffectOpen)); !slices.Equal(got, []string{"cash_sweep_cost_above_free_cash"}) {
		t.Fatalf("an above-par buy = %v", got)
	}
	tight := row
	block := *row.CashSweep
	block.MaxOrderNotionalBase = 40000 // 54,780 USD at 0.9 is 49,302 EUR
	tight.CashSweep = &block
	if got := codes(tight, preview(55, 99.6, rpc.OrderPositionEffectOpen)); !slices.Equal(got, []string{"cash_sweep_above_max_order_notional"}) {
		t.Fatalf("a buy above max_order_notional = %v", got)
	}
	noTerms := preview(55, 99.6, rpc.OrderPositionEffectOpen)
	noTerms.Draft.Bond = nil
	if got := codes(row, noTerms); !slices.Contains(got, "unsupported_security_type") {
		t.Fatalf("a bond draft without its bill terms passed: %v", got)
	}
}

// Invariants over random books: no row converts, buys outside the
// vocabulary or the maturity window, or exceeds free cash; a currency never
// invests and redeems at once; and neither side, applied, turns into the
// other on the next plan.
func TestCashSweepInvariantsOverRandomBooks(t *testing.T) {
	now := cashSweepTestNow()
	rng := rand.New(rand.NewPCG(7, 9))
	currencies := []string{"USD", "EUR", "GBP", "CAD", "CHF"}
	sides := map[string]int{}
	for i := range 2000 {
		policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1000+rng.Float64()*100000)
		in := cashSweepTestInput(nil)
		in.BaseCurrency = "EUR"
		for _, ccy := range currencies {
			if rng.IntN(4) == 0 {
				continue
			}
			in.Ledger[ccy] = cashSweepLedgerRow{Observed: true, TradeDate: rng.Float64() * 40000, ExchangeRate: 0.5 + rng.Float64()}
			in.Commitments.ByCurrency[ccy] = rng.Float64() * 5000
			in.Settlement.SaleProceeds[ccy] = rng.Float64() * 3000
			in.Settlement.PurchaseCosts[ccy] = rng.Float64() * 3000
			if rng.IntN(2) == 0 {
				in.Holdings[ccy] = []cashSweepHolding{cashSweepTestBill(1000+i, ccy, cashSweepInstrumentsFor(ccy)[0], float64(1+rng.IntN(20)), 2+rng.IntN(120))}
			}
		}
		plan := cashSweepPlanFor(policy, in, now)
		for _, cp := range plan.currencies {
			ccy := cp.status.Currency
			sides[cp.side]++
			switch cp.side {
			case rpc.CashSweepSideInvest:
				if float64(cp.quantity) > cp.free+1e-6 || float64(cp.quantity) < policy.Buckets.CashSweep.currency(ccy).MinTranche-1 {
					t.Fatalf("book %d: planned amount out of bounds: %+v", i, cp.status)
				}
				// Resolved at a price either side of par, the order stays in
				// the bill's units on its grid, within free cash in face and
				// in cost.
				resolved := cashSweepTestResolve(cp, 99+2*rng.Float64(), now)
				if resolved.side == "" {
					continue
				}
				row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, resolved)
				conv := cashSweepInstrumentConventions[resolved.bill.Instrument]
				face := float64(row.Quantity) * conv.FacePerUnit
				cost := face * *resolved.bill.Price / 100
				if row.Contract.Currency != ccy || !cashSweepInstrumentAllowed(cp.instrument, ccy) || cp.instrument == "none" || row.Quantity < 1 ||
					face > cp.free+1e-6 || cost > cp.free+1e-6 || resolved.rules.CheckQuantity(row.Quantity) != nil ||
					cp.targetDays < 28 || cp.targetDays > policy.Buckets.CashSweep.currency(ccy).MaxMaturityDays {
					t.Fatalf("book %d: invest row out of bounds: %+v / %+v", i, row, cp.status)
				}
				if _, ok := cashSweepOpenException(row); !ok {
					t.Fatalf("book %d: planned buy is outside the typed exception: %+v", i, row.CashSweep)
				}
				// Applied as a working order, the buy never turns into a sale.
				next := in
				next.Commitments.ByCurrency = maps.Clone(in.Commitments.ByCurrency)
				next.Commitments.ByCurrency[ccy] += cost
				if again := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, next, now), ccy); again.side == rpc.CashSweepSideRedeem {
					t.Fatalf("book %d: invest alternated into redeem: %+v", i, again.status)
				}
			case rpc.CashSweepSideRedeem:
				if cp.holding.Row.Currency != ccy || cp.quantity < 1 || float64(cp.quantity) > cp.holding.Row.Quantity {
					t.Fatalf("book %d: redeem out of bounds: %+v", i, cp)
				}
				// Applied as a pending sale, the redemption never turns into
				// a buy.
				next := in
				next.Settlement.EquivalentSales = maps.Clone(in.Settlement.EquivalentSales)
				next.Settlement.SaleProceeds = maps.Clone(in.Settlement.SaleProceeds)
				proceeds := float64(cp.quantity) * cp.holding.MarketValue / cp.holding.Row.Quantity
				next.Settlement.EquivalentSales[ccy] += proceeds
				next.Settlement.SaleProceeds[ccy] += proceeds
				next.Ledger = maps.Clone(in.Ledger)
				row := next.Ledger[ccy]
				row.TradeDate += proceeds
				next.Ledger[ccy] = row
				if again := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, next, now), ccy); again.side == rpc.CashSweepSideInvest {
					t.Fatalf("book %d: redeem alternated into invest: %+v", i, again.status)
				}
			}
			// Unchanged inputs, unchanged verdict.
			if again := cashSweepCurrencyOf(t, cashSweepPlanFor(policy, in, now), ccy); again.side != cp.side || again.quantity != cp.quantity {
				t.Fatalf("book %d: the plan moved on unchanged inputs", i)
			}
		}
	}
	if sides[rpc.CashSweepSideInvest] < 100 || sides[rpc.CashSweepSideRedeem] < 100 || sides[""] < 100 {
		t.Fatalf("the random books did not exercise every side: %v", sides)
	}
}

// Fills inside the settlement window are summed per currency from each
// order's cumulative fills; a conversion, a bond fill or a fill without a
// currency makes the affected currencies unknown; fills before the window
// have settled.
func TestCashSweepSettlementFromJournal(t *testing.T) {
	now := cashSweepTestNow()
	since := cashSweepSettlementWindowStart(now)
	if since != time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("window start = %v", since)
	}
	if monday := cashSweepSettlementWindowStart(time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)); monday.Weekday() != time.Friday {
		t.Fatalf("a Monday's window starts %v", monday)
	}
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	view := func(ref, action, secType, ccy string, mult int) rpc.OrderView {
		return rpc.OrderView{OrderRef: ref, Account: "DU1234567", Mode: "paper", Action: action, SecType: secType, Currency: ccy, Multiplier: mult}
	}
	views := []rpc.OrderView{
		view("a", "SELL", "STK", "USD", 0),
		view("b", "BUY", "OPT", "USD", 100),
		view("c", "SELL", "STK", "EUR", 0),
	}
	events := map[string][]rpc.OrderEvent{
		// Two partial fills: one before the window (settled), one inside.
		orderViewKey(views[0]): {{At: since.Add(-time.Hour), Filled: 10, AvgFillPrice: 100}, {At: since.Add(time.Hour), Filled: 30, AvgFillPrice: 110}},
		orderViewKey(views[1]): {{At: now.Add(-time.Hour), Filled: 2, AvgFillPrice: 1.5}},
		orderViewKey(views[2]): {{At: since.Add(-2 * time.Hour), Filled: 5, AvgFillPrice: 20}},
	}
	got := cashSweepSettlementFrom(views, events, scope, since)
	// USD sale: cumulative 30×110 − 10×100 = 2,300 inside the window.
	if !got.Known || math.Abs(got.SaleProceeds["USD"]-2300) > 1e-9 || math.Abs(got.PurchaseCosts["USD"]-300) > 1e-9 || got.SaleProceeds["EUR"] != 0 || len(got.Unknown) != 0 {
		t.Fatalf("settlement = %+v", got)
	}
	// A bond in a currency without a bill convention cannot be valued (a CAD
	// bond is: cash_sweep_orders_test.go).
	views = append(views, view("d", "BUY", "BOND", "CHF", 0), view("e", "BUY", "CASH", "USD", 0))
	events[orderViewKey(views[3])] = []rpc.OrderEvent{{At: now, Filled: 1, AvgFillPrice: 99}}
	events[orderViewKey(views[4])] = []rpc.OrderEvent{{At: now, Filled: 1000, AvgFillPrice: 1.1}}
	got = cashSweepSettlementFrom(views, events, scope, since)
	if got.Unknown["CHF"] == "" || got.Unknown[""] == "" {
		t.Fatalf("unknown = %+v", got.Unknown)
	}
	// Another account's fills do not count.
	other := view("f", "SELL", "STK", "USD", 0)
	other.Account = "DU7654321"
	got = cashSweepSettlementFrom([]rpc.OrderView{other}, map[string][]rpc.OrderEvent{orderViewKey(other): {{At: now, Filled: 1, AvgFillPrice: 1}}}, scope, since)
	if got.SaleProceeds["USD"] != 0 {
		t.Fatalf("another account's fill counted: %+v", got)
	}
	// The daemon started inside the window: its journal cannot vouch for it.
	engine := &proposalEngine{server: &Server{startedAt: now.Add(-time.Hour)}}
	if st := engine.cashSweepSettlement(scope, now); st.Known || !strings.Contains(st.Reason, "since the daemon started") {
		t.Fatalf("journal gap = %+v", st)
	}
	engine.server.startedAt = since.Add(-time.Hour)
	if st := engine.cashSweepSettlement(scope, now); st.Known || !strings.Contains(st.Reason, "no order journal") {
		t.Fatalf("no journal = %+v", st)
	}
}

// The ledger is read only from a current, one-shot account summary; the base
// currency comes from its own row.
func TestCashSweepLedgerFromAccount(t *testing.T) {
	acct := &rpc.AccountResult{AccountID: "DU1234567", BaseCurrency: "EUR",
		CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "USD", CashCcy: 12000, CashObserved: true, ExchangeRate: 0.9}, {Currency: "GBP", CashCcy: 0, ExchangeRate: 1.1}},
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashCcy: 8000, CashObserved: true, ExchangeRate: 1},
		Authority: &rpc.AccountDataAuthority{Scope: rpc.AccountDataScope{AccountID: "DU1234567", AccountMode: "paper"}, Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent,
			Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}},
	}
	base, ledger, reason := cashSweepLedger(acct)
	if reason != "" || base != "EUR" || ledger["USD"] != (cashSweepLedgerRow{Observed: true, TradeDate: 12000, ExchangeRate: 0.9}) ||
		ledger["EUR"] != (cashSweepLedgerRow{Observed: true, TradeDate: 8000, ExchangeRate: 1}) || ledger["GBP"].Observed {
		t.Fatalf("ledger = %s %+v %q", base, ledger, reason)
	}
	acct.Authority.Fields.CurrencyExposure = false
	if _, _, reason := cashSweepLedger(acct); reason == "" {
		t.Fatal("a cached ledger was read as current")
	}
	acct.Authority.Freshness = rpc.AccountDataFreshnessUnknown
	if _, _, reason := cashSweepLedger(acct); reason == "" {
		t.Fatal("a stale account was read")
	}
	// The presence flag reads every ledger dialect and never a missing key.
	raw := map[string]string{"$LEDGER:CashBalance_USD": "12000", "$LEDGER-CashBalance_GBP": "0", "CashBalance_CAD": "5", "CashBalance_CHF": "n/a", "TotalCashValue": "1"}
	for ccy, want := range map[string]bool{"USD": true, "GBP": true, "CAD": true, "CHF": false, "EUR": false} {
		if got := ledgerCashObserved(raw, ccy); got != want {
			t.Fatalf("%s observed = %v", ccy, got)
		}
	}
	res := &rpc.AccountResult{BaseCurrency: "EUR", CurrencyExposure: []rpc.CurrencyExposure{{Currency: "USD", CashCcy: 12000}}}
	annotateLedgerCash(res, map[string]ibkrlib.CurrencyLedger{"USD": {CashBalance: 12000, ExchangeRate: 0.9}, "EUR": {CashBalance: 8000, NetLiquidationByCurrency: 9000, ExchangeRate: 1}},
		map[string]string{"$LEDGER:CashBalance_USD": "12000", "$LEDGER:CashBalance_EUR": "8000"})
	if !res.CurrencyExposure[0].CashObserved || res.BaseCurrencyLedger == nil || res.BaseCurrencyLedger.CashCcy != 8000 || !res.BaseCurrencyLedger.CashObserved || res.BaseCurrencyLedger.ExchangeRate != 1 {
		t.Fatalf("annotated = %+v / %+v", res.CurrencyExposure, res.BaseCurrencyLedger)
	}
}

// The engine emits nothing while the bucket is off, and the snapshot helpers
// count and deep-copy what it emits.
func TestCashSweepEngineGateCountsAndClone(t *testing.T) {
	engine := &proposalEngine{}
	now := cashSweepTestNow()
	if rows, st := engine.cashSweepProposals(context.Background(), defaultProtectionPolicy(), rpc.ProtectionPolicyStatus{}, nil, nil, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{}, now); rows != nil || st != nil {
		t.Fatal("a policy without the sweep generated sweep output")
	}
	// Enabled without a current account: every declared currency reads
	// cash_unavailable and nothing is generated.
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	setSweepCcy(policy.Buckets.CashSweep, "USD", func(*protectionCashSweepCurrency) {})
	rows, st := engine.cashSweepProposals(context.Background(), policy, rpc.ProtectionPolicyStatus{}, nil, nil, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{}, now)
	if rows != nil || st == nil || st.Reason == "" || len(st.Currencies) != 1 || st.Currencies[0].State != rpc.CashSweepStateCashUnavailable || st.Rows != 0 {
		t.Fatalf("no account = %+v rows %v", st, rows)
	}
	plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": 60000}), now)
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, plan.currencies[0])
	proposals := []rpc.TradeProposal{row, {Bucket: rpc.TradeProposalBucketTrailingStop}}
	if n, shadow := cashSweepCounts(proposals); n != 1 || shadow != 1 {
		t.Fatalf("counts = %d %d", n, shadow)
	}
	snap := rpc.TradeProposalSnapshot{Proposals: proposals, CashSweep: &plan.status}
	copied := cloneProposalSnapshot(snap)
	copied.Proposals[0].CashSweep.Free = -1
	copied.CashSweep.Currencies[0].Instruments[0] = "changed"
	*copied.CashSweep.Currencies[0].Cash = -1
	if snap.Proposals[0].CashSweep.Free == -1 || snap.CashSweep.Currencies[0].Instruments[0] == "changed" || *snap.CashSweep.Currencies[0].Cash == -1 {
		t.Fatal("the snapshot clone shares sweep state")
	}
}

// The brief's cash row and rule 14's evidence appear only while the sweep is
// enabled; an unclassified holding leaves equivalents unavailable, never
// zero.
func TestCashSweepCashLikeRows(t *testing.T) {
	acct := &rpc.AccountResult{AccountID: "DU1234567", BaseCurrency: "EUR",
		CurrencyExposure:   []rpc.CurrencyExposure{{Currency: "USD", CashCcy: 12000, CashObserved: true, ExchangeRate: 0.9}},
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", CashCcy: 8000, CashObserved: true, ExchangeRate: 1},
		Authority: &rpc.AccountDataAuthority{Scope: rpc.AccountDataScope{AccountID: "DU1234567", AccountMode: "paper"}, Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent,
			Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, CurrencyExposure: true}},
	}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "CCC", SecType: "BOND", Currency: "USD", Quantity: 5}}}
	rows, reason, ok := cashLikeRows(&protectionCashSweepPolicy{Enabled: true}, acct, pos, true)
	if !ok || reason != "" || len(rows) != 2 {
		t.Fatalf("rows = %+v %q %v", rows, reason, ok)
	}
	eur, usd := rows[0], rows[1]
	if eur.Currency != "EUR" || *eur.Cash != 8000 || *eur.CashEquivalents != 0 || *eur.CashLike != 8000 {
		t.Fatalf("EUR = %+v", eur)
	}
	if usd.Currency != "USD" || *usd.Cash != 12000 || usd.CashEquivalents != nil || usd.CashLike != nil {
		t.Fatalf("USD = %+v", usd)
	}
	// Positions not current: no currency's equivalents are known.
	rows, _, _ = cashLikeRows(&protectionCashSweepPolicy{Enabled: true}, acct, nil, false)
	for _, row := range rows {
		if row.CashEquivalents != nil {
			t.Fatalf("equivalents without positions: %+v", row)
		}
	}
	// A server without the sweep enabled serves neither the row nor the
	// evidence.
	s := &Server{protectionPolicies: newProtectionPolicyManager("", false, 0, nil)}
	s.protectionPolicies.reload()
	if s.briefCashRow(acct, pos, true) != nil || s.rulebookCashLike(acct, pos, true) != nil {
		t.Fatal("cash-like figures served without the sweep")
	}
}

// The snapshot revision ignores the sweep: its quantity follows cash to the
// unit, and a revision that moved with it would restart every pre-authorised
// veto window and stale every open preview. Every other row still binds it.
func TestCashSweepRowsStayOutOfTheSnapshotRevision(t *testing.T) {
	now := cashSweepTestNow()
	policy := cashSweepTestPolicy(rpc.CashSweepModeShadow, 1e9)
	stop := rpc.TradeProposal{Key: "trailing_stop:1", Bucket: rpc.TradeProposalBucketTrailingStop, Quantity: 100, PositionEffect: rpc.OrderPositionEffectClose}
	revision := func(cash float64) string {
		plan := cashSweepPlanFor(policy, cashSweepTestInput(map[string]float64{"USD": cash}), now)
		rows := []rpc.TradeProposal{stop}
		for _, cp := range plan.currencies {
			if cp.side != "" {
				rows = append(rows, cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, cp))
			}
		}
		return proposalRevision(rpc.Fingerprint{Key: "p"}, rpc.TradeProposalSourceFingerprints{}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, cashSweepRevisionRows(rows))
	}
	if revision(60000) != revision(60001) || revision(60000) != revision(5000) {
		t.Fatal("the sweep moved the snapshot revision")
	}
	before := revision(60000)
	stop.Quantity = 50
	if revision(60000) == before {
		t.Fatal("the revision no longer binds the other rows")
	}
}
