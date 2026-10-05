package daemon

import (
	"slices"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// No bill buys while any currency is borrowed (owner decision 2026-10-05
// 21:24 CEST). The fixtures mirror the live case with synthetic numbers: EUR
// cash 60,000 and USD cash −20,000 on a 200,000 EUR book.

func borrowedSweepInput(cash map[string]float64) cashSweepInput {
	return sizedSweepInput(200000, cash)
}

func hasBlocker(blockers []rpc.TradingBlocker, code string) bool {
	return slices.ContainsFunc(blockers, func(b rpc.TradingBlocker) bool { return b.Code == code })
}

// A borrowed USD holds the EUR buy: the row stays listed and blocked with
// currency_borrowed naming the USD debit, and the status carries it typed.
func TestCashSweepBorrowedUSDHoldsEURBuys(t *testing.T) {
	now := cashSweepTestNow()
	policy := ownerSizedSweepPolicy()
	plan := cashSweepPlanFor(policy, borrowedSweepInput(map[string]float64{"EUR": 60000, "USD": -20000}), now)
	b := plan.status.Borrowing
	if b == nil || b.State != rpc.CashSweepBorrowingBorrowed || !b.HoldsBuys || b.NoBuyWhileBorrowed == nil || !*b.NoBuyWhileBorrowed ||
		len(b.Borrowed) != 1 || b.Borrowed[0].Currency != "USD" || b.Borrowed[0].Cash != -20000 || b.Borrowed[0].Borrowed != 20000 ||
		b.Borrowed[0].BorrowedBase == nil || !near(*b.Borrowed[0].BorrowedBase, 18000) || len(b.Unknown) != 0 || b.ToleranceUnits != 1 {
		t.Fatalf("borrowing = %+v", b)
	}
	const want = "USD is borrowed: −20,000 USD; bill buys wait until it is repaid"
	if b.Message != want || !strings.Contains(b.Action, "convert") || !strings.Contains(b.Action, "deposit") || !strings.Contains(b.Action, "Canary does not convert") {
		t.Fatalf("words = %q / %q", b.Message, b.Action)
	}
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if eur.side != rpc.CashSweepSideInvest || eur.status.State != rpc.CashSweepStateHold || !strings.HasPrefix(eur.status.Reason, want) ||
		!hasBlocker(eur.status.Blockers, rpc.CashSweepBlockerCurrencyBorrowed) {
		t.Fatalf("EUR = %s %+v", eur.side, eur.status)
	}
	eur.bill = &rpc.TradeProposalCashSweepBill{Instrument: cashSweepInstrumentDEBubill, ConID: 77, SecType: "BILL", Maturity: "2026-12-30", QuantityUnit: rpc.BondQuantityUnitFace1, PriceConvention: rpc.BondPriceConventionPer100, QuoteFresh: true}
	eur.units = 40000
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, eur)
	if row.State != rpc.TradeProposalStateBlocked || !hasBlocker(row.Blockers, rpc.CashSweepBlockerCurrencyBorrowed) || row.AutomaticEligible() {
		t.Fatalf("row = %s %+v", row.State, row.Blockers)
	}
	// The same row without the debit is an ordinary proposal.
	clear := cashSweepPlanFor(policy, borrowedSweepInput(map[string]float64{"EUR": 60000, "USD": 6000}), now)
	if c := clear.status.Borrowing; c == nil || c.State != rpc.CashSweepBorrowingClear || c.HoldsBuys || c.Message != "no currency is borrowed" {
		t.Fatalf("clear = %+v", c)
	}
	if eur := cashSweepCurrencyOf(t, clear, "EUR"); eur.status.State != rpc.CashSweepStateInvest || len(eur.status.Blockers) != 0 {
		t.Fatalf("clear EUR = %+v", eur.status)
	}
	// A debit within the tolerance is dust, not a loan.
	dust := cashSweepPlanFor(policy, borrowedSweepInput(map[string]float64{"EUR": 60000, "USD": -0.6}), now)
	if dust.status.Borrowing.State != rpc.CashSweepBorrowingClear || cashSweepCurrencyOf(t, dust, "EUR").status.State != rpc.CashSweepStateInvest {
		t.Fatalf("dust = %+v", dust.status.Borrowing)
	}
}

// A negative settled balance counts even when trade-date cash is positive:
// the borrowing reads the band's own cash, the lower of the two.
func TestCashSweepBorrowingReadsSettledCash(t *testing.T) {
	in := borrowedSweepInput(map[string]float64{"EUR": 60000, "USD": 500})
	row := in.Ledger["USD"]
	row.Settled = new(-12000.0)
	in.Ledger["USD"] = row
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), in, cashSweepTestNow())
	if b := plan.status.Borrowing; b.State != rpc.CashSweepBorrowingBorrowed || b.Borrowed[0].Cash != -12000 {
		t.Fatalf("borrowing = %+v", b)
	}
}

// Selling bills to cover cash is still allowed while a currency is
// borrowed: the redemption carries no borrowing blocker.
func TestCashSweepBorrowedStillRedeems(t *testing.T) {
	now := cashSweepTestNow()
	policy := ownerSizedSweepPolicy()
	in := borrowedSweepInput(map[string]float64{"EUR": 60000, "USD": -20000})
	in.Holdings["USD"] = []cashSweepHolding{cashSweepTestBill(801, "USD", cashSweepInstrumentUSTBill, 40, 20)}
	plan := cashSweepPlanFor(policy, in, now)
	usd := cashSweepCurrencyOf(t, plan, "USD")
	if usd.side != rpc.CashSweepSideRedeem || usd.status.State != rpc.CashSweepStateRedeem || len(usd.status.Blockers) != 0 || len(usd.blockers) != 0 {
		t.Fatalf("USD = %s %+v", usd.side, usd.status)
	}
	row := cashSweepRow(policy, rpc.ProtectionPolicyStatus{}, rpc.TradeProposalSourceFingerprints{}, now, plan, usd)
	if hasBlocker(row.Blockers, rpc.CashSweepBlockerCurrencyBorrowed) || hasBlocker(row.Blockers, rpc.CashSweepBlockerBorrowingUnknown) {
		t.Fatalf("redemption blocked = %+v", row.Blockers)
	}
	if !hasBlocker(cashSweepCurrencyOf(t, plan, "EUR").status.Blockers, rpc.CashSweepBlockerCurrencyBorrowed) {
		t.Fatal("EUR buy not held while USD is borrowed")
	}
}

// Unknown cash in any listed currency cannot prove that nothing is
// borrowed: buys hold with borrowing_unknown, and the status says which.
func TestCashSweepUnknownCashHoldsBuys(t *testing.T) {
	in := borrowedSweepInput(map[string]float64{"EUR": 60000, "USD": 6000})
	in.Ledger["GBP"] = cashSweepLedgerRow{ExchangeRate: 1.15}
	plan := cashSweepPlanFor(ownerSizedSweepPolicy(), in, cashSweepTestNow())
	b := plan.status.Borrowing
	if b.State != rpc.CashSweepBorrowingUnknown || !b.HoldsBuys || len(b.Unknown) != 1 || b.Unknown[0].Currency != "GBP" ||
		!strings.Contains(b.Message, "cannot prove that no currency is borrowed") {
		t.Fatalf("borrowing = %+v", b)
	}
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if eur.status.State != rpc.CashSweepStateHold || !hasBlocker(eur.status.Blockers, rpc.CashSweepBlockerBorrowingUnknown) {
		t.Fatalf("EUR = %+v", eur.status)
	}
}

// no_buy_while_borrowed = false lets buys go ahead; the status still names
// the debit so Desk can show it.
func TestCashSweepBorrowingKeyFalseAllowsBuys(t *testing.T) {
	policy := ownerSizedSweepPolicy()
	policy.Buckets.CashSweep.NoBuyWhileBorrowed = new(false)
	plan := cashSweepPlanFor(policy, borrowedSweepInput(map[string]float64{"EUR": 60000, "USD": -20000}), cashSweepTestNow())
	b := plan.status.Borrowing
	if b.State != rpc.CashSweepBorrowingBorrowed || b.HoldsBuys || !strings.Contains(b.Message, "no_buy_while_borrowed = false") {
		t.Fatalf("borrowing = %+v", b)
	}
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if eur.side != rpc.CashSweepSideInvest || eur.status.State != rpc.CashSweepStateInvest || len(eur.status.Blockers) != 0 || len(eur.blockers) != 0 {
		t.Fatalf("EUR = %+v", eur.status)
	}
}

// A file without the key holds the whole sweep at needs_your_number naming
// it, like every other number read from the file only.
func TestCashSweepBorrowingKeyMissingHolds(t *testing.T) {
	policy := ownerSizedSweepPolicy()
	policy.Buckets.CashSweep.NoBuyWhileBorrowed = nil
	plan := cashSweepPlanFor(policy, borrowedSweepInput(map[string]float64{"EUR": 60000, "USD": 6000}), cashSweepTestNow())
	if !slices.Contains(plan.status.NeedsYourNumber, "no_buy_while_borrowed") {
		t.Fatalf("needs = %v", plan.status.NeedsYourNumber)
	}
	eur := cashSweepCurrencyOf(t, plan, "EUR")
	if eur.side != "" || eur.status.State != rpc.CashSweepStateNeedsYourNumber || !strings.Contains(eur.status.Reason, "no_buy_while_borrowed") {
		t.Fatalf("EUR = %+v", eur.status)
	}
	if b := plan.status.Borrowing; b == nil || b.NoBuyWhileBorrowed != nil {
		t.Fatalf("borrowing = %+v", b)
	}
	if needs := cashSweepNeedsYourNumber(policy.Buckets.CashSweep); !slices.ContainsFunc(needs, func(s string) bool { return strings.Contains(s, "no_buy_while_borrowed") }) {
		t.Fatalf("policy status needs = %v", needs)
	}
}
