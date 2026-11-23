package daemon

import (
	"maps"
	"math"
	"reflect"
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A EUR-based book with EUR and USD cash and no HKD row: the broker lists
// only currencies with a balance. Synthetic figures.
func borrowedTestBook() (map[string]cashSweepLedgerRow, cashSweepCommitments) {
	return map[string]cashSweepLedgerRow{
		"EUR": {Observed: true, TradeDate: 50000, Settled: new(50000.), ExchangeRate: 1},
		"USD": {Observed: true, TradeDate: 20000, Settled: new(20000.), ExchangeRate: 0.9},
	}, cashSweepCommitments{Known: true, ByCurrency: map[string]float64{}}
}

func borrowedTestBucket(on *bool) *protectionCashSweepPolicy {
	return &protectionCashSweepPolicy{KeepCash: new(5000.), NoBuyWhileBorrowed: new(true), BuyStockOnBorrowedCurrency: on}
}

const hkdRate, borrowedReserve = 0.11, 10000.

// withHKD is the ledger as stockAddEvidence completes it for an absent row.
func withHKD(ledger map[string]cashSweepLedgerRow, row cashSweepLedgerRow) map[string]cashSweepLedgerRow {
	out := map[string]cashSweepLedgerRow{"HKD": row}
	maps.Copy(out, ledger)
	return out
}

func TestStockAddNativeCashReadsAnAbsentCurrencyOnlyWhenBorrowingIsAllowed(t *testing.T) {
	ledger, _ := borrowedTestBook()
	if _, err := stockAddNativeCash(false, "EUR", ledger, "HKD"); err == nil || err.Error() != "native settled cash is unavailable for HKD" {
		t.Fatalf("key off: %v", err)
	}
	if absent, err := stockAddNativeCash(true, "EUR", ledger, "HKD"); err != nil || !absent {
		t.Fatalf("key on: absent=%v %v", absent, err)
	}
	unobserved := map[string]cashSweepLedgerRow{"EUR": {TradeDate: 50000, ExchangeRate: 1}}
	if _, err := stockAddNativeCash(true, "EUR", unobserved, "HKD"); err == nil {
		t.Fatal("an unobserved ledger read an absent currency as zero")
	}
	if absent, err := stockAddNativeCash(true, "EUR", ledger, "USD"); err != nil || absent {
		t.Fatalf("a held currency read as absent: %v %v", absent, err)
	}
}

func TestStockAddBorrowsItsCurrencyAgainstOtherCashBeyondTheReserve(t *testing.T) {
	ledger, commitments := borrowedTestBook()
	ledger = withHKD(ledger, cashSweepLedgerRow{Observed: true, Settled: new(0.), ExchangeRate: hkdRate})
	buy := rpc.AddParams{Quantity: 1000, LimitPrice: 80}
	off, err := stockAddFunding(borrowedTestBucket(nil), ledger, commitments, "HKD", borrowedReserve, hkdRate, buy)
	if err != nil || off.BorrowAllowed || off.Spendable >= 0 {
		t.Fatalf("key absent borrowed: %+v %v", off, err)
	}
	on, err := stockAddFunding(borrowedTestBucket(new(true)), ledger, commitments, "HKD", borrowedReserve, hkdRate, buy)
	if err != nil || !on.BorrowAllowed || on.Currency != "HKD" {
		t.Fatalf("key on: %+v %v", on, err)
	}
	// own free cash + (other free cash in base - reserve) / rate
	want := -5000.0 + ((50000-5000)*1+(20000-5000)*0.9-borrowedReserve)/hkdRate
	if math.Abs(on.Spendable-want) > 1e-6 || on.Spendable < off.Spendable {
		t.Fatalf("spendable %v, want %v", on.Spendable, want)
	}
	// The plan warns with the borrowed amount and the cash stays true.
	in := stockAddTestInput()
	in.Cash, in.FreeCash, in.Price, in.FX, in.Requested, in.Fee = on, on.Spendable, 80, hkdRate, 1000, 30
	in.BrokerMargin = &risk.StockAddMargin{Quantity: 1000, ExcessLiquidityBase: 40000, InitialMarginBase: 1, MaintenanceMarginBase: 1}
	in.OrderCapBase, in.Policy.MaxUnderlyingStockPctNLV = 100000, new(50.)
	plan := risk.CheckStockAdd(in)
	if len(plan.Blockers) > 0 || plan.Quantity != 1000 || plan.Cash == nil || plan.Cash.Borrowed != 80030 || plan.Cash.Available != 0 {
		t.Fatalf("plan %+v cash %+v", plan.Blockers, plan.Cash)
	}
	const sentence = "This purchase borrows 80,030 HKD; your cash in other currencies covers it. IBKR charges interest on the borrowed HKD until you convert."
	if i := slicesIndexCode(plan.Warnings, "add_borrowed_currency"); i < 0 || plan.Warnings[i].Message != sentence {
		t.Fatalf("warnings %+v", plan.Warnings)
	}
	// The account's cash in base stays at or above the reserve at the limit.
	if after := (on.Available-on.Committed-on.Spendable)*hkdRate + (50000 - 5000) + (20000-5000)*0.9 - 5000*hkdRate; after < borrowedReserve-1e-6 {
		t.Fatalf("cash in base after the largest purchase %v is below the reserve", after)
	}
}

func slicesIndexCode(w []risk.StockAddBlocker, code string) int {
	for i, b := range w {
		if b.Code == code {
			return i
		}
	}
	return -1
}

func TestStockAddBorrowingIsHeldWhenOtherCashDoesNotCoverTheReserve(t *testing.T) {
	ledger := map[string]cashSweepLedgerRow{
		"EUR": {Observed: true, TradeDate: 12000, Settled: new(12000.), ExchangeRate: 1},
		"HKD": {Observed: true, Settled: new(0.), ExchangeRate: hkdRate},
	}
	cash, err := stockAddFunding(borrowedTestBucket(new(true)), ledger, cashSweepCommitments{Known: true}, "HKD", borrowedReserve, hkdRate, rpc.AddParams{Quantity: 1000, LimitPrice: 80})
	if err != nil || cash.Spendable >= 0 {
		t.Fatalf("other cash short of the reserve still funded a borrow: %+v %v", cash, err)
	}
	in := stockAddTestInput()
	in.Cash, in.FreeCash, in.Price, in.FX, in.Requested, in.Fee = cash, cash.Spendable, 80, hkdRate, 1000, 30
	plan := risk.CheckStockAdd(in)
	if len(plan.Blockers) == 0 || plan.Quantity != 0 || slicesIndexCode(plan.Warnings, "add_borrowed_currency") >= 0 {
		t.Fatalf("held add: %+v", plan)
	}
	if plan.Binding != "cash" || plan.Blockers[0].Code != "add_no_capacity" {
		t.Fatalf("held by %q, not cash: %+v", plan.Binding, plan.Blockers)
	}
}

// After a first borrowed buy the ledger carries a negative HKD row and a
// second buy's commitment; the next HKD add is still bounded by the total.
// The debit alone no longer holds the add, while unknown cash does.
func TestStockAddSecondBorrowedBuySeesTheDebitAndTheCommitment(t *testing.T) {
	ledger, _ := borrowedTestBook()
	ledger = withHKD(ledger, cashSweepLedgerRow{Observed: true, TradeDate: -80000, Settled: new(-80000.), ExchangeRate: hkdRate})
	commitments := cashSweepCommitments{Known: true, ByCurrency: map[string]float64{"HKD": 40000}}
	cash, err := stockAddFunding(borrowedTestBucket(new(true)), ledger, commitments, "HKD", borrowedReserve, hkdRate, rpc.AddParams{Quantity: 100, LimitPrice: 80})
	if err != nil || !cash.BorrowAllowed {
		t.Fatalf("second add: %+v %v", cash, err)
	}
	want := -80000 - 40000 - 5000 + ((50000-5000)+(20000-5000)*0.9-borrowedReserve)/hkdRate
	if math.Abs(cash.Spendable-want) > 1e-6 {
		t.Fatalf("spendable %v, want %v", cash.Spendable, want)
	}
	// A USD add on the same book counts the HKD debit against the reserve.
	usd, err := stockAddFunding(borrowedTestBucket(new(true)), ledger, commitments, "USD", borrowedReserve, 0.9, rpc.AddParams{Quantity: 10, LimitPrice: 100})
	if err != nil || usd.BorrowAllowed || math.Abs(usd.OtherReserveFundingBase-(45000+(-80000-40000-5000)*hkdRate)) > 1e-6 {
		t.Fatalf("USD add beside the debit: %+v %v", usd, err)
	}
	for _, tc := range []struct {
		borrow  bool
		unknown bool
		holds   bool
	}{{false, false, true}, {true, false, false}, {true, true, true}} {
		in := cashSweepInput{Ledger: ledger}
		ccys := []string{"EUR", "HKD", "USD"}
		if tc.unknown {
			ccys = append(ccys, "SGD")
		}
		b := cashSweepBorrowingFor(borrowedTestBucket(&tc.borrow), in, ccys)
		if got := stockAddBorrowingHolds(tc.borrow, b); got != tc.holds {
			t.Errorf("borrow %v unknown %v: holds %v, want %v (%s)", tc.borrow, tc.unknown, got, tc.holds, b.State)
		}
	}
}

// A float written for no currency keeps none for the debit currency, so the
// first borrowed buy does not fail every later add.
func TestStockAddDebitCurrencyWithoutAFloatKeepsNone(t *testing.T) {
	bucket := &protectionCashSweepPolicy{NoBuyWhileBorrowed: new(true), Currency: map[string]protectionCashSweepCurrency{"EUR": {KeepCash: new(5000.)}, "USD": {KeepCash: new(5000.)}}}
	ledger, commitments := borrowedTestBook()
	ledger = withHKD(ledger, cashSweepLedgerRow{Observed: true, TradeDate: -80000, Settled: new(-80000.), ExchangeRate: hkdRate})
	if _, err := stockAddFunding(bucket, ledger, commitments, "USD", borrowedReserve, 0.9, rpc.AddParams{Quantity: 10, LimitPrice: 100}); err == nil {
		t.Fatal("key off: a currency without a float was funded")
	}
	bucket.BuyStockOnBorrowedCurrency = new(true)
	got, err := stockAddFunding(bucket, ledger, commitments, "USD", borrowedReserve, 0.9, rpc.AddParams{Quantity: 10, LimitPrice: 100})
	if err != nil || math.Abs(got.OtherReserveFundingBase-(45000-80000*hkdRate)) > 1e-6 {
		t.Fatalf("debit without a float: %+v %v", got, err)
	}
}

// A USD add its own cash pays is the same to the cent with the key on or off.
func TestStockAddNativePurchaseUnchangedByTheKey(t *testing.T) {
	ledger, _ := borrowedTestBook()
	commitments := cashSweepCommitments{Known: true, ByCurrency: map[string]float64{"USD": 1000}}
	buy := rpc.AddParams{Quantity: 100, LimitPrice: 100}
	off, err := stockAddFunding(borrowedTestBucket(nil), ledger, commitments, "USD", borrowedReserve, 0.9, buy)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []*bool{new(false), new(true)} {
		got, err := stockAddFunding(borrowedTestBucket(key), ledger, commitments, "USD", borrowedReserve, 0.9, buy)
		if err != nil {
			t.Fatal(err)
		}
		got.BorrowSpendable, off.BorrowSpendable = 0, 0
		if !reflect.DeepEqual(got, off) {
			t.Fatalf("key %v changed a native add:\n%+v\n%+v", *key, got, off)
		}
	}
	// A maximum never borrows.
	got, err := stockAddFunding(borrowedTestBucket(new(true)), ledger, commitments, "USD", borrowedReserve, 0.9, rpc.AddParams{Max: true, LimitPrice: 100})
	if err != nil || got.BorrowAllowed || got.Spendable != off.Spendable {
		t.Fatalf("maximum borrowed: %+v %v", got, err)
	}
}
