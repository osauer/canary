package daemon

import (
	"time"

	"github.com/osauer/canary/v2/internal/ibkrledger"
	"github.com/osauer/canary/v2/internal/rpc"
)

func cashSweepCashObservation(row rpc.CurrencyExposure, account *rpc.AccountResult, now time.Time) cashSweepLedgerRow {
	out := cashSweepLedgerRow{Observed: row.CashObserved, TradeDate: row.CashCcy, ExchangeRate: row.ExchangeRate, Settled: cloneFloat64Ptr(row.SettledCashCcy)}
	if out.Settled != nil {
		out.SettledSourceKind = "native_tws_ledger"
	}
	if schedule := row.SettledCashSchedule; schedule != nil {
		// An admitted schedule's low is TWS's own per-currency settled cash.
		// A held one says why, unless the ledger already supplied a value.
		switch {
		case schedule.Status == rpc.SettledCashScheduleAdmitted && schedule.Low != nil && finiteProtectionOptionPolicyValue(*schedule.Low):
			low := *schedule.Low
			out.SettledSourceKind = "native_tws_settlement_schedule"
			if out.Settled != nil {
				low = min(low, *out.Settled)
				out.SettledSourceKind = "minimum_of_native_tws_ledger_and_schedule"
			}
			out.Settled = new(low)
		case out.Settled == nil:
			out.SettledReason = nonEmptyString(schedule.Reason, "TWS's settlement schedule was not admitted")
		}
	}
	native := out.Settled != nil
	if normCcy(row.Currency) == normCcy(account.BaseCurrency) {
		out.ExchangeRate = 1
	}
	if web := row.WebCash; web != nil {
		out.WebCashOriginalAsOf = web.AsOf
		if account.Authority == nil || web.Scope != account.Authority.Scope || web.Currency != normCcy(row.Currency) ||
			web.AsOf.IsZero() || web.AsOf.After(now) || now.Sub(web.AsOf) > ibkrledger.MaxAge ||
			!finiteProtectionOptionPolicyValue(web.CashBalance) || !finiteProtectionOptionPolicyValue(web.SettledCash) {
			out.SettledReason = "supplemental broker cash evidence is stale or outside the current account/mode"
		} else {
			// Different broker channels can lag one another. None can increase
			// the spending capacity beyond any of the current cash observations.
			out.TradeDate = min(out.TradeDate, web.CashBalance)
			settled := web.SettledCash
			if out.Settled != nil {
				settled = min(settled, *out.Settled)
			}
			out.Settled = new(settled)
			out.WebCashOriginalAsOf = web.AsOf
			out.SettledSourceKind = "native_web_ledger"
			if native {
				out.SettledSourceKind = "minimum_of_native_tws_and_web_ledgers"
			}
		}
	} else if out.Settled == nil && account.CashLedger != nil {
		out.SettledReason = "supplemental broker cash evidence unavailable: " + nonEmptyString(account.CashLedger.Reason, "no explicit row for "+normCcy(row.Currency))
	}
	return out
}
