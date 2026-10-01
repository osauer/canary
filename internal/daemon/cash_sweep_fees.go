package daemon

import (
	"fmt"
	"math"

	"github.com/osauer/canary/v2/internal/rpc"
)

// cashSweepFeeUpper uses only the exact draft's accepted broker WhatIf.
// Commission alone is an estimate, MinCommission is a lower bound, and a
// missing or foreign-currency upper bound cannot protect this currency's cash.
// Zero is usable only when the broker explicitly reports it.
func cashSweepFeeUpper(preview *rpc.OrderPreviewResult, currency string) (float64, bool) {
	if preview == nil || preview.WhatIf.Status != rpc.OrderWhatIfStatusAccepted || !preview.WhatIf.Available {
		return 0, false
	}
	m := preview.WhatIf.Margin
	if m == nil || m.MaxCommission == nil || normCcy(currency) == "" || normCcy(m.CommissionCurrency) != normCcy(currency) {
		return 0, false
	}
	valid := func(n float64) bool { return n >= 0 && finiteProtectionOptionPolicyValue(n) && n != math.MaxFloat64 }
	upper := *m.MaxCommission
	if !valid(upper) {
		return 0, false
	}
	for _, estimate := range []*float64{m.Commission, m.MinCommission} {
		if estimate != nil && (!valid(*estimate) || *estimate > upper) {
			return 0, false
		}
	}
	if m.Commission != nil && m.MinCommission != nil && *m.Commission < *m.MinCommission {
		return 0, false
	}
	return upper, true
}

func cashSweepFeeReserveBlockers(preview *rpc.OrderPreviewResult, principal, free float64, currency string) []rpc.TradingBlocker {
	fee, known := cashSweepFeeUpper(preview, currency)
	if !known {
		return []rpc.TradingBlocker{{Code: "cash_sweep_fees_unknown",
			Message: "the sweep buy has no consistent broker WhatIf maximum commission in its cash currency; protected cash cannot be verified",
			Action:  "Refresh and preview again with an accepted broker fee upper bound in the bill's currency."}}
	}
	total := principal + fee
	if !positiveFinite(total) || total > free+cashSweepMoneyEpsilon {
		return []rpc.TradingBlocker{{Code: "cash_sweep_cost_above_free_cash",
			Message: fmt.Sprintf("the sweep buy costs %s at its limit plus maximum broker commission %s, above the free cash %s", formatBudgetMoney(principal, currency), formatBudgetMoney(fee, currency), formatBudgetMoney(free, currency)),
			Action:  "Preview a smaller sweep buy that leaves the configured cash reserve after commission."}}
	}
	return nil
}

// Outstanding-order and queued-authorisation DTOs carry principal bounds,
// but no exact commission upper bound or fee currency. Preserve that principal
// diagnostic without treating it as a known total. An unknown fee can debit a
// different currency, so all new sweep work waits for those buys to resolve.
func cashSweepOutstandingFeesUnknown(out *cashSweepCommitments, currency, source string) {
	if out.Unknown[currency] == "" {
		out.Unknown[currency] = fmt.Sprintf("%s buy in %s has no broker commission upper bound, so total committed cash is unknown", source, currency)
	}
	out.Unknown[""] = "an outstanding working or armed buy has no broker commission currency or upper bound, so fee-inclusive commitments are unknown"
}
