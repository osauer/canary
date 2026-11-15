package daemon

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// bondFactsTestServer is a paper server whose bond directory answers held
// contract ids from lines and whose quotes are pre-read; the clock is now.
func bondFactsTestServer(t *testing.T, now time.Time, lines map[int]ibkrlib.BondContractDetails, quotes map[int]rpc.BondQuote) *Server {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: new(1e6)})
	dir := &bondDirectory{now: func() time.Time { return now }, quotes: map[int]bondQuoteEntry{},
		fetch: bondFetchOf(func(_ context.Context, r ibkrlib.BondContractRequest) ([]ibkrlib.BondContractDetails, error) {
			if line, ok := lines[r.ConID]; ok {
				return []ibkrlib.BondContractDetails{line}, nil
			}
			return nil, ibkrlib.ErrBondContractNotFound
		})}
	for conID, q := range quotes {
		q.AsOf = now
		dir.quotes[conID] = bondQuoteEntry{quote: q, at: now}
	}
	srv.cashSweepB.dir = dir
	return srv
}

func bondFactsBuy(at time.Time, permID, conID int, symbol, ccy string, quantity, price float64) orderJournalEvent {
	return orderJournalEvent{At: at, Type: orderJournalEventStatusUpdated, PermID: permID, ClientID: 15, Account: "DU1234567", Endpoint: "127.0.0.1:4001", Mode: "paper",
		Symbol: symbol, SecType: "BILL", ConID: conID, Currency: ccy, Action: rpc.OrderActionBuy, Quantity: quantity, Status: "Filled", Filled: quantity,
		AvgFillPrice: price, ExecTime: at.Format("20060102-15:04:05"), SendState: orderSendStateTerminal}
}

func bondFactsQuote(bid, close float64) rpc.BondQuote {
	ask := bid + 0.02
	return rpc.BondQuote{Bid: &bid, Ask: &ask, Close: &close, DataType: "live", Fresh: true, PriceConvention: rpc.BondPriceConventionPer100}
}

// A US bill bought through Canary: the price is the quote's bid, the yield
// is the simple actual/365 yield, the change since the prior close is in
// basis points, 25 bp costs about 18 USD on 50,000 of face, the face and
// maturity payment are stated, and the gain since purchase from the
// journal's lot splits into carry and rates that sum to it; US bills carry
// no spread.
func TestBondFactsUSBillFromJournal(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC) // a Sunday
	maturity := cashSweepDay(now).AddDate(0, 0, 54)
	cusip := syntheticCUSIP(t, "912797ZZ")
	line := synthBondLine(7701, cusip, "USD", maturity)
	line.SecType, line.MinSize, line.SizeIncrement = ibkrlib.SecTypeBill, 1, 1
	srv := bondFactsTestServer(t, now, map[int]ibkrlib.BondContractDetails{7701: line}, map[int]rpc.BondQuote{7701: bondFactsQuote(99.40, 99.38)})
	bought := time.Date(2026, 10, 2, 14, 3, 5, 0, time.UTC)
	if err := srv.orderJournal.AppendAll([]orderJournalEvent{bondFactsBuy(bought, 7001, 7701, "SYNTH", "USD", 50, 99.18)}); err != nil {
		t.Fatal(err)
	}
	bonds := []rpc.PositionBond{{ConID: 7701, Symbol: "SYNTH", Currency: "USD", Class: rpc.BondClassBill, CUSIP: cusip, Maturity: maturity.Format(time.DateOnly),
		DaysToMaturity: new(54), Quantity: 50, Mark: 99.40, MarketValue: 49700}}
	rows := []rpc.PositionView{{ConID: 7701, Symbol: "SYNTH", SecType: "BILL", Currency: "USD", Quantity: 50, Mark: 99.40, MarketValue: 49700, UnrealizedPnL: 110}}
	srv.fillBondFacts(context.Background(), bonds, rows, now)

	f := bonds[0].Facts
	if f == nil || f.Unavailable != "" {
		t.Fatalf("facts = %+v", f)
	}
	if f.Price == nil || *f.Price != 99.40 || f.PriceSource != rpc.BondPriceSourceBid || !f.QuoteFresh || f.Mark == nil || *f.Mark != 99.40 {
		t.Fatalf("price = %+v", f)
	}
	if f.YieldBasis != risk.BillYieldBasisAct365 || f.YieldPct == nil || math.Abs(*f.YieldPct-4.08) > 0.01 {
		t.Fatalf("yield = %v %s", f.YieldPct, f.YieldBasis)
	}
	// The prior weekday before a Sunday is Friday; its close was two more days from maturity.
	if f.PriorCloseDate != "2026-10-09" || f.YieldChangeBp == nil || f.PriorCloseYieldPct == nil || *f.YieldChangeBp <= 0 || *f.YieldChangeBp > 5 {
		t.Fatalf("yield change = %+v", f)
	}
	if f.ValueChangeUp25Bp == nil || math.Abs(*f.ValueChangeUp25Bp+18.3) > 0.3 || f.ValueChangeDown25Bp == nil || *f.ValueChangeDown25Bp <= 0 ||
		f.DV01 == nil || math.Abs(*f.DV01-0.73) > 0.03 || f.ScenarioModel == "" {
		t.Fatalf("scenario = %+v", f)
	}
	if f.FaceValue == nil || *f.FaceValue != 50000 || f.FaceSource != rpc.BondFaceSourceQuantityUnit || f.PaysAtMaturity == nil ||
		f.PaysAtMaturity.Amount != 50000 || f.PaysAtMaturity.Currency != "USD" || f.PaysAtMaturity.Date != maturity.Format(time.DateOnly) || f.PaysAtMaturity.Days != 54 {
		t.Fatalf("face = %+v / %+v", f.FaceValue, f.PaysAtMaturity)
	}
	// Bought with 63 days left at 99.18: a 4.79% purchase yield; the yield
	// has since fallen to 4.08%, so the rates part is a gain.
	if f.Purchase == nil || f.Purchase.Source != rpc.BondPurchaseSourceJournal || f.Purchase.Date != "2026-10-02" || f.Purchase.PricePer100 != 99.18 || f.Purchase.Lots != 1 ||
		f.Purchase.YieldPct == nil || math.Abs(*f.Purchase.YieldPct-4.79) > 0.01 {
		t.Fatalf("purchase = %+v", f.Purchase)
	}
	p := f.PnL
	if p == nil || p.Scope != rpc.BondPnLScopeCleanPrice || p.TotalSource != rpc.BondPnLTotalSourceLots || math.Abs(p.Total-110) > 1e-9 || p.Carry == nil || p.Rates == nil {
		t.Fatalf("pnl = %+v", p)
	}
	if math.Abs(*p.Carry+*p.Rates-p.Total) > 1e-9 || *p.Carry <= 0 || *p.Rates <= 0 {
		t.Fatalf("carry %v + rates %v != total %v", *p.Carry, *p.Rates, p.Total)
	}
	if p.Spread != nil || !strings.Contains(p.SpreadUnavailable, "USD benchmark") || f.SincePurchaseUnavailable != "" {
		t.Fatalf("spread = %+v", p)
	}
}

// A BTF against the held Bubill nearest in maturity: the first read names
// the benchmark and the spread now but has no benchmark yield on record for
// the purchase, so the spread part is unavailable with that reason and the
// yield effect is all rates; the read records today's yields; the next read
// states the spread part from that record, says it runs from the record's
// day, and the three parts sum to the gain. The Bubill itself has no spread.
// The records persist in daemon.db.
func TestBondFactsBTFSpreadFromBenchmarkRecord(t *testing.T) {
	now := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC) // a Monday
	today := cashSweepDay(now)
	btf := synthBondLine(7601, synthFRBill, "EUR", today.AddDate(0, 0, 94))
	bubill := synthBondLine(7602, synthDEBill, "EUR", today.AddDate(0, 0, 100))
	farBubill := synthBondLine(7603, synthDEBill2, "EUR", today.AddDate(0, 0, 200))
	srv := bondFactsTestServer(t, now, map[int]ibkrlib.BondContractDetails{7601: btf, 7602: bubill, 7603: farBubill},
		map[int]rpc.BondQuote{7601: bondFactsQuote(99.40, 99.41), 7602: bondFactsQuote(99.45, 99.45), 7603: bondFactsQuote(98.90, 98.90)})
	bought := time.Date(2026, 9, 22, 9, 30, 0, 0, time.UTC)
	if err := srv.orderJournal.AppendAll([]orderJournalEvent{bondFactsBuy(bought, 7002, 7601, "SYNTHB", "EUR", 40, 99.32)}); err != nil {
		t.Fatal(err)
	}
	build := func() []rpc.PositionBond {
		return []rpc.PositionBond{
			{ConID: 7601, Symbol: "SYNTHB", Currency: "EUR", Class: rpc.BondClassBill, ISIN: synthFRBill, Maturity: today.AddDate(0, 0, 94).Format(time.DateOnly), Quantity: 40, Mark: 99.40, MarketValue: 39760},
			{ConID: 7602, Symbol: "SYNTHB", Currency: "EUR", Class: rpc.BondClassBill, ISIN: synthDEBill, Maturity: today.AddDate(0, 0, 100).Format(time.DateOnly), Quantity: 10, Mark: 99.45, MarketValue: 9945},
			{ConID: 7603, Symbol: "SYNTHB", Currency: "EUR", Class: rpc.BondClassBill, ISIN: synthDEBill2, Maturity: today.AddDate(0, 0, 200).Format(time.DateOnly), Quantity: 10, Mark: 98.90, MarketValue: 9890},
		}
	}
	rows := []rpc.PositionView{
		{ConID: 7601, SecType: "BILL", Currency: "EUR", Quantity: 40, Mark: 99.40, MarketValue: 39760, UnrealizedPnL: 32},
		{ConID: 7602, SecType: "BILL", Currency: "EUR", Quantity: 10, Mark: 99.45, MarketValue: 9945, UnrealizedPnL: 5},
		{ConID: 7603, SecType: "BILL", Currency: "EUR", Quantity: 10, Mark: 98.90, MarketValue: 9890, UnrealizedPnL: 5},
	}
	bonds := build()
	srv.fillBondFacts(context.Background(), bonds, rows, now)
	f := bonds[0].Facts
	if f == nil || f.Unavailable != "" || f.YieldBasis != risk.BillYieldBasisAct360 || f.YieldPct == nil || math.Abs(*f.YieldPct-2.31) > 0.01 {
		t.Fatalf("btf facts = %+v", f)
	}
	if f.PnL == nil || f.PnL.Benchmark == nil || f.PnL.Benchmark.ConID != 7602 || f.PnL.Benchmark.ISIN != synthDEBill || f.PnL.Benchmark.YieldPct == nil ||
		f.PnL.Benchmark.SpreadBpNow == nil || *f.PnL.Benchmark.SpreadBpNow <= 0 || f.PnL.Benchmark.YieldBasis != risk.BillYieldBasisAct360 {
		t.Fatalf("benchmark = %+v", f.PnL)
	}
	if f.PnL.Spread != nil || !strings.Contains(f.PnL.SpreadUnavailable, "on record") || f.PnL.Carry == nil || f.PnL.Rates == nil ||
		math.Abs(*f.PnL.Carry+*f.PnL.Rates-f.PnL.Total) > 1e-9 || math.Abs(f.PnL.Total-32) > 1e-9 {
		t.Fatalf("first read pnl = %+v", f.PnL)
	}
	if b := bonds[1].Facts; b == nil || b.PnL == nil || !strings.Contains(b.PnL.SpreadUnavailable, "EUR benchmark") {
		t.Fatalf("bubill = %+v", bonds[1].Facts)
	}

	// The records are persisted and reload from the store.
	srv.cashSweepB.yields = nil
	recs := srv.bondYieldRecords()
	line := recs.doc.Lines["7601"]
	if len(line) != 1 || line[0].Date != today.Format(time.DateOnly) || line[0].BenchmarkYieldPct == nil || line[0].BenchmarkConID != 7602 {
		t.Fatalf("records = %+v", recs.doc.Lines)
	}

	later := now.Add(2 * time.Hour)
	bonds = build()
	srv.fillBondFacts(context.Background(), bonds, rows, later)
	p := bonds[0].Facts.PnL
	if p == nil || p.Spread == nil || p.Carry == nil || p.Rates == nil || p.Benchmark == nil || p.Benchmark.SpreadSince != today.Format(time.DateOnly) || p.Benchmark.SpreadBpThen == nil {
		t.Fatalf("second read pnl = %+v / %+v", p, p.Benchmark)
	}
	if math.Abs(*p.Carry+*p.Rates+*p.Spread-p.Total) > 1e-9 || math.Abs(p.Total-32) > 1e-9 {
		t.Fatalf("carry %v + rates %v + spread %v != total %v", *p.Carry, *p.Rates, *p.Spread, p.Total)
	}
	// Nothing moved since the record, so the spread part is zero.
	if math.Abs(*p.Spread) > 1e-6 {
		t.Fatalf("spread = %v with an unchanged benchmark", *p.Spread)
	}
}

// A bill not bought through Canary: the price paid is read back from the
// broker's market value, mark and unrealised gain; without a date the
// broker's gain is the total and the split is unavailable with the reason.
// Without a cached quote the mark prices the facts and a quote is requested.
func TestBondFactsBrokerCostWithoutJournal(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	maturity := cashSweepDay(now).AddDate(0, 0, 54)
	cusip := syntheticCUSIP(t, "912797ZY")
	srv := bondFactsTestServer(t, now, map[int]ibkrlib.BondContractDetails{7702: synthBondLine(7702, cusip, "USD", maturity)}, nil)
	bonds := []rpc.PositionBond{{ConID: 7702, Symbol: "SYNTH", Currency: "USD", Class: rpc.BondClassBill, CUSIP: cusip, Maturity: maturity.Format(time.DateOnly), Quantity: 50, Mark: 99.40, MarketValue: 49700}}
	rows := []rpc.PositionView{{ConID: 7702, SecType: "BILL", Currency: "USD", Quantity: 50, Mark: 99.40, MarketValue: 49700, UnrealizedPnL: 110}}
	srv.fillBondFacts(context.Background(), bonds, rows, now)
	f := bonds[0].Facts
	if f == nil || f.Unavailable != "" || f.PriceSource != rpc.BondPriceSourceMark || f.Bid != nil || !strings.Contains(f.QuoteNote, "requested") {
		t.Fatalf("facts = %+v", f)
	}
	if f.YieldChangeBp != nil || !strings.Contains(f.YieldChangeUnavailable, "no prior close") {
		t.Fatalf("yield change without a close = %+v", f)
	}
	if f.Purchase == nil || f.Purchase.Source != rpc.BondPurchaseSourceBrokerCost || f.Purchase.Date != "" || math.Abs(f.Purchase.PricePer100-99.18) > 1e-9 {
		t.Fatalf("purchase = %+v", f.Purchase)
	}
	if f.PnL == nil || f.PnL.TotalSource != rpc.BondPnLTotalSourceBroker || f.PnL.Total != 110 || f.PnL.Carry != nil || f.PnL.Rates != nil || f.PnL.Spread != nil ||
		!strings.Contains(f.SincePurchaseUnavailable, "purchase date is unknown") {
		t.Fatalf("pnl = %+v / %q", f.PnL, f.SincePurchaseUnavailable)
	}
}

// A coupon bond: the yield is MeasureBond's on the currency's compounding,
// the scenario reprices fully, and the clean gain splits into carry and
// rates without a benchmark.
func TestBondFactsCouponBond(t *testing.T) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	maturity := cashSweepDay(now).AddDate(7, 0, 0)
	cusip := syntheticCUSIP(t, "91282CZY")
	line := synthBondLine(7801, cusip, "USD", maturity)
	line.Coupon = 4
	srv := bondFactsTestServer(t, now, map[int]ibkrlib.BondContractDetails{7801: line}, map[int]rpc.BondQuote{7801: bondFactsQuote(98.00, 98.10)})
	bought := time.Date(2026, 4, 13, 15, 0, 0, 0, time.UTC)
	if err := srv.orderJournal.AppendAll([]orderJournalEvent{bondFactsBuy(bought, 7003, 7801, "SYNT", "USD", 10, 98.50)}); err != nil {
		t.Fatal(err)
	}
	bonds := []rpc.PositionBond{{ConID: 7801, Symbol: "SYNT", Currency: "USD", Class: rpc.BondClassBond, CUSIP: cusip, Maturity: maturity.Format(time.DateOnly), Coupon: new(4.0), Quantity: 10, Mark: 98.00, MarketValue: 9800}}
	rows := []rpc.PositionView{{ConID: 7801, SecType: "BOND", Currency: "USD", Quantity: 10, Mark: 98.00, MarketValue: 9800, UnrealizedPnL: -50}}
	srv.fillBondFacts(context.Background(), bonds, rows, now)
	f := bonds[0].Facts
	if f == nil || f.Unavailable != "" || f.YieldBasis != risk.BondYieldBasisSemiannual || f.YieldPct == nil || *f.YieldPct < 4.2 || *f.YieldPct > 4.5 {
		t.Fatalf("bond facts = %+v", f)
	}
	if f.FaceValue == nil || *f.FaceValue != 10000 || f.FaceSource != rpc.BondFaceSourceMarketValue || f.ValueChangeUp25Bp == nil || *f.ValueChangeUp25Bp > -140 || *f.ValueChangeUp25Bp < -160 {
		t.Fatalf("face / scenario = %+v", f)
	}
	if f.YieldChangeBp == nil || *f.YieldChangeBp <= 0 {
		t.Fatalf("yield change = %+v", f)
	}
	p := f.PnL
	if p == nil || p.Carry == nil || p.Rates == nil || p.Spread != nil || math.Abs(p.Total+50) > 1e-9 || math.Abs(*p.Carry+*p.Rates-p.Total) > 1e-9 ||
		!strings.Contains(p.SpreadUnavailable, "coupon bond") {
		t.Fatalf("bond pnl = %+v", p)
	}
}
