package canary

import "github.com/osauer/canary/v2/internal/rpc"

// LendingPosition is a statement-dated managed-stock-loan annotation.
type LendingPosition = rpc.LendingPosition

// FinancingSummary is missing-aware earned-fee attribution for an exact period.
type FinancingSummary = rpc.FinancingSummary

// FinancingCurrencyAmount is known earned income in one native currency.
type FinancingCurrencyAmount = rpc.FinancingCurrencyAmount

// FinancingFee is one sanitized net earned fee, with optional conversion evidence.
type FinancingFee = rpc.FinancingFee

// FinancingFeesParams selects a bounded canary_lending_fees read.
type FinancingFeesParams = rpc.FinancingFeesParams

// FinancingFeesResult contains full-period totals and a separate filtered page.
type FinancingFeesResult = rpc.FinancingFeesResult

// ValidateFinancingFeesResult validates the public lending fee contract.
func ValidateFinancingFeesResult(result FinancingFeesResult) error {
	return rpc.ValidateFinancingFeesResult(result)
}
