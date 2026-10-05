package daemon

import (
	"fmt"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
)

// No bill buys while any currency is borrowed (owner decision 2026-10-05
// 21:24 CEST). The sweep judges each currency alone and never converts, so on
// 2026-10-05 it proposed a EUR bill buy while the account paid USD margin
// interest far above the bill's yield. With no_buy_while_borrowed = true,
// every invest row in every currency holds while any currency's cash is
// negative beyond CashSweepBorrowedToleranceUnits, or while some currency's
// cash is unknown (the sweep cannot prove nothing is borrowed). Redemptions
// are not held: selling a bill to cover cash still helps. Canary does not
// convert; the owner repays by converting or depositing.

// cashSweepBorrowingFor reads every currency the sweep lists (every ledger
// currency, every currency holding an equivalent) from the same ledger the
// band reads its cash from: the lower of trade-date cash and the broker's
// settled cash where observed. A listed currency whose cash cannot be read is
// unknown; a currency the ledger does not list carries no balance.
func cashSweepBorrowingFor(bucket *protectionCashSweepPolicy, in cashSweepInput, ccys []string) *rpc.CashSweepBorrowing {
	out := &rpc.CashSweepBorrowing{State: rpc.CashSweepBorrowingClear, ToleranceUnits: rpc.CashSweepBorrowedToleranceUnits}
	if bucket != nil && bucket.NoBuyWhileBorrowed != nil {
		out.NoBuyWhileBorrowed = new(*bucket.NoBuyWhileBorrowed)
	}
	for _, ccy := range slices.Sorted(slices.Values(ccys)) {
		row, inLedger := in.Ledger[ccy]
		switch {
		case in.LedgerReason != "":
			out.Unknown = append(out.Unknown, rpc.CashSweepUnknownCash{Currency: ccy, Reason: in.LedgerReason})
			continue
		case !inLedger:
			out.Unknown = append(out.Unknown, rpc.CashSweepUnknownCash{Currency: ccy, Reason: fmt.Sprintf("the account ledger has no %s row", ccy)})
			continue
		case !row.Observed || !finiteProtectionOptionPolicyValue(row.TradeDate):
			out.Unknown = append(out.Unknown, rpc.CashSweepUnknownCash{Currency: ccy, Reason: fmt.Sprintf("the account ledger's %s row carries no cash balance", ccy)})
			continue
		}
		cash := row.TradeDate
		if row.Settled != nil && finiteProtectionOptionPolicyValue(*row.Settled) {
			cash = min(cash, *row.Settled)
		}
		if cash >= -rpc.CashSweepBorrowedToleranceUnits {
			continue
		}
		b := rpc.CashSweepBorrowedCurrency{Currency: ccy, Cash: cash, Borrowed: -cash}
		if row.ExchangeRate > 0 && finiteProtectionOptionPolicyValue(row.ExchangeRate) {
			b.BorrowedBase = new(-cash * row.ExchangeRate)
		}
		out.Borrowed = append(out.Borrowed, b)
	}
	switch {
	case len(out.Borrowed) > 0:
		out.State = rpc.CashSweepBorrowingBorrowed
	case len(out.Unknown) > 0:
		out.State = rpc.CashSweepBorrowingUnknown
	}
	out.HoldsBuys = bucket.noBuyWhileBorrowed() && out.State != rpc.CashSweepBorrowingClear
	out.Message, out.Action = cashSweepBorrowingWords(out)
	return out
}

// cashSweepBorrowingWords says the borrowing state and what the owner can do.
func cashSweepBorrowingWords(b *rpc.CashSweepBorrowing) (string, string) {
	var borrowed, debitCcys []string
	for _, c := range b.Borrowed {
		borrowed = append(borrowed, fmt.Sprintf("%s is borrowed: %s", c.Currency, cashSweepSignedMoney(c.Cash, c.Currency)))
		debitCcys = append(debitCcys, c.Currency)
	}
	var unknown []string
	for _, c := range b.Unknown {
		unknown = append(unknown, c.Currency+" ("+c.Reason+")")
	}
	off := b.NoBuyWhileBorrowed != nil && !*b.NoBuyWhileBorrowed
	switch b.State {
	case rpc.CashSweepBorrowingBorrowed:
		msg := strings.Join(borrowed, "; ")
		if off {
			return msg + "; no_buy_while_borrowed = false, so bill buys still go ahead while it is borrowed", ""
		}
		them := "it is"
		if len(b.Borrowed) > 1 {
			them = "they are"
		}
		return msg + "; bill buys wait until " + them + " repaid",
			fmt.Sprintf("Repay the %s debit by converting another currency or depositing %s; Canary does not convert. Bill buys resume on the next cycle once no currency is below −%s of its own unit. Selling bills to cover cash is still allowed.",
				strings.Join(debitCcys, " and "), strings.Join(debitCcys, " or "), briefThousands(b.ToleranceUnits, 0))
	case rpc.CashSweepBorrowingUnknown:
		msg := "the cash of " + strings.Join(unknown, ", ") + " cannot be read, so the sweep cannot prove that no currency is borrowed"
		if off {
			return msg + "; no_buy_while_borrowed = false, so bill buys do not wait for it", ""
		}
		return msg + "; bill buys wait until it can",
			"Refresh once the account ledger reports every currency's cash; bill buys resume when no currency is borrowed. Selling bills to cover cash is still allowed."
	}
	return "no currency is borrowed", ""
}

// cashSweepHoldBorrowedBuys holds every invest row when the borrowing rule
// binds. The row stays listed and blocked, so the owner sees what would be
// bought and why it waits; a redemption is left alone.
func cashSweepHoldBorrowedBuys(plan *cashSweepPlan) {
	b := plan.status.Borrowing
	if b == nil || !b.HoldsBuys {
		return
	}
	code := rpc.CashSweepBlockerCurrencyBorrowed
	if b.State == rpc.CashSweepBorrowingUnknown {
		code = rpc.CashSweepBlockerBorrowingUnknown
	}
	blocker := rpc.TradingBlocker{Code: code, Message: b.Message, Action: b.Action}
	for i := range plan.currencies {
		cp := &plan.currencies[i]
		if cp.side != rpc.CashSweepSideInvest {
			continue
		}
		cp.blockers = appendTradingBlockerOnce(cp.blockers, blocker)
		cp.status.Blockers = appendTradingBlockerOnce(cp.status.Blockers, blocker)
		cp.status.State = rpc.CashSweepStateHold
		cp.status.Reason = b.Message + "; without the hold: " + cp.status.Reason
	}
}

// cashSweepSignedMoney is an amount with thousands separators and a true
// minus sign, for example "−20,000 USD".
func cashSweepSignedMoney(v float64, ccy string) string {
	text := briefThousands(v, 0)
	if rest, ok := strings.CutPrefix(text, "-"); ok {
		text = "−" + rest
	}
	return strings.TrimSpace(text + " " + ccy)
}
