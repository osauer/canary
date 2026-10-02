// Package financing deterministically projects managed stock lending and net
// earned fees from retained statement evidence. It performs no I/O, broker
// requests, enrollment, policy changes or account-P/L calculation.
package financing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Result contains full-period attribution and all sanitized rows before
// paging. Consumers must not derive period totals from a page.
type Result struct {
	Summary rpc.FinancingSummary
	Fees    []rpc.FinancingFee
}

// Calculate uses opening-exclusive/closing-inclusive dates. Each day's newest
// complete section supersedes older rows, including an explicit empty section.
// A newer absent/invalid section preserves older known fees as partial context.
func Calculate(statements []flexstmt.Statement, scope string, from, to time.Time, baseCurrency string) Result {
	result := Result{Fees: []rpc.FinancingFee{}, Summary: rpc.FinancingSummary{
		SchemaVersion: rpc.FinancingSchemaVersion, State: rpc.FinancingUnavailable,
		Reason: "lending_sections_missing", From: from, To: to, BaseCurrency: baseCurrency,
		Native: []rpc.FinancingCurrencyAmount{}, PNLReconciliation: "unproved", PaymentLinkage: "unavailable",
	}}
	summary := &result.Summary
	ordered := orderedStatements(statements)
	for _, st := range ordered {
		if st.ToDate.After(summary.AsOf) {
			summary.AsOf = st.ToDate
		}
	}
	native := map[string]float64{}
	knownBase, haveFees, missingFX, missingAmount := 0.0, false, false, false
	for day := from.AddDate(0, 0, 1); !day.After(to); day = day.AddDate(0, 0, 1) {
		summary.ExpectedDays++
		latest := covering(ordered, day, false)
		if latest == nil {
			continue
		}
		f := latest.Financing
		completeDay := f != nil && f.FeesPresent && f.FeeReason == ""
		if completeDay {
			summary.CoveredDays++
		}
		if f == nil || !f.FeesPresent || f.FeeReason != "" {
			// Keep the latest observed rows, but never certify the range from
			// a superseded incomplete/missing optional section.
			if older := covering(ordered, day, true); older != nil {
				f = older.Financing
			}
		}
		if f == nil || !f.FeesPresent {
			continue
		}
		haveFees = true
		for _, fee := range f.Fees {
			if !fee.ValueDate.Equal(day) {
				continue
			}
			row := publicFee(fee, scope, baseCurrency)
			result.Fees = append(result.Fees, row)
			if row.NetFee == nil {
				missingAmount = true
				continue
			}
			native[row.Currency] += *row.NetFee
			if row.BaseAmount == nil {
				missingFX = true
			} else {
				knownBase += *row.BaseAmount
			}
		}
	}
	sort.Slice(result.Fees, func(i, j int) bool {
		if !result.Fees[i].ValueDate.Equal(result.Fees[j].ValueDate) {
			return result.Fees[i].ValueDate.After(result.Fees[j].ValueDate)
		}
		return result.Fees[i].ID < result.Fees[j].ID
	})
	summary.FeeCount = len(result.Fees)
	for currency, amount := range native {
		summary.Native = append(summary.Native, rpc.FinancingCurrencyAmount{Currency: currency, Amount: amount})
	}
	sort.Slice(summary.Native, func(i, j int) bool { return summary.Native[i].Currency < summary.Native[j].Currency })
	if haveFees {
		summary.State, summary.Reason = rpc.FinancingPartial, "fee_range_incomplete"
		if summary.CoveredDays == summary.ExpectedDays && !missingAmount {
			summary.State, summary.Reason = rpc.FinancingComplete, ""
		}
		if !missingFX && baseCurrency != "" && finite(knownBase) {
			summary.KnownEarnedBase = new(knownBase)
			if summary.State == rpc.FinancingComplete {
				summary.EarnedBase = new(knownBase)
			}
		} else {
			summary.Reason = "base_conversion_unavailable"
		}
		if missingAmount {
			summary.Reason = "net_fee_missing"
		}
	}
	// Include the evidence itself: a restatement or changed account/mode
	// invalidates cursors even when a coincidental total is unchanged.
	raw, _ := json.Marshal(struct {
		Scope    string
		From, To time.Time
		Base     string
		Evidence []flexstmt.Statement
	}{scope, from, to, baseCurrency, ordered})
	summary.Fingerprint = digest("finance_", raw)
	return result
}

func finite(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) }

func digest(prefix string, raw []byte) string {
	h := sha256.Sum256(raw)
	return prefix + hex.EncodeToString(h[:16])
}

func orderedStatements(statements []flexstmt.Statement) []flexstmt.Statement {
	out := append([]flexstmt.Statement(nil), statements...)
	sort.Slice(out, func(i, j int) bool {
		if !out[i].WhenGenerated.Equal(out[j].WhenGenerated) {
			return out[i].WhenGenerated.After(out[j].WhenGenerated)
		}
		// Deterministic tie-break, independent of input/import ordering.
		a, _ := json.Marshal(out[i])
		b, _ := json.Marshal(out[j])
		return digest("", a) > digest("", b)
	})
	return out
}

func covering(ordered []flexstmt.Statement, day time.Time, withFees bool) *flexstmt.Statement {
	for i := range ordered {
		st := &ordered[i]
		if day.Before(st.FromDate) || day.After(st.ToDate) {
			continue
		}
		if !withFees || st.Financing != nil && st.Financing.FeesPresent {
			return st
		}
	}
	return nil
}

func publicFee(fee flexstmt.LendingFee, scope, base string) rpc.FinancingFee {
	row := rpc.FinancingFee{ID: digest("fee_", []byte(scope+"\x00"+fee.RecordID)), ConID: fee.ConID,
		Symbol: fee.Symbol, ValueDate: fee.ValueDate, StartDate: fee.StartDate, Currency: fee.Currency,
		Quantity: fee.Quantity, NetFee: fee.NetFee, NetRatePct: fee.NetRatePct, Collateral: fee.Collateral}
	if base != "" && fee.Currency == base {
		row.FXRateToBase = new(1.0)
	} else if base != "" && fee.FXBaseCurrency == base {
		row.FXRateToBase = fee.FXRateToBase
	}
	if fee.NetFee != nil && row.FXRateToBase != nil {
		n := *fee.NetFee * *row.FXRateToBase
		if finite(n) {
			row.BaseAmount = new(n)
		}
	}
	return row
}

// LoanAnnotations returns the latest explicitly selected loan snapshot. A
// later empty snapshot clears older balances. Associations require a matching
// dated owned-position anchor; callers also check the live position quantity.
func LoanAnnotations(statements []flexstmt.Statement, expectedDay time.Time) map[int64]rpc.LendingPosition {
	annotations := map[int64]rpc.LendingPosition{}
	ordered := orderedStatements(statements)
	var latest *flexstmt.Statement
	for i := range ordered {
		st := &ordered[i]
		if st.ToDate.After(expectedDay) {
			continue
		}
		if latest == nil || st.ToDate.After(latest.ToDate) {
			latest = st
		}
	}
	if latest == nil || latest.Financing == nil || !latest.Financing.LoansPresent || latest.Financing.LoanReason != "" {
		return annotations
	}
	owned := map[int64]*float64{}
	for _, p := range latest.Positions {
		if p.ReportDate.Equal(latest.ToDate) && p.Quantity != nil && *p.Quantity > 0 && p.AssetClass == "STK" {
			if _, exists := owned[p.ConID]; exists {
				owned[p.ConID] = nil
			} else {
				owned[p.ConID] = p.Quantity
			}
		}
	}
	bad := map[int64]bool{}
	for _, loan := range latest.Financing.Loans {
		item, ok := annotations[loan.ConID]
		if !ok {
			item = rpc.LendingPosition{AsOf: latest.ToDate, State: "reported", Currency: loan.Currency, OwnedQuantity: owned[loan.ConID], Collateral: new(0.0)}
		}
		if item.Currency != loan.Currency {
			bad[loan.ConID] = true
		}
		item.Quantity += loan.Quantity
		if loan.Collateral == nil || item.Collateral == nil {
			item.Collateral = nil
		} else {
			item.Collateral = new(*item.Collateral + *loan.Collateral)
		}
		if latest.ToDate.Before(expectedDay) {
			item.State = "stale"
		}
		annotations[loan.ConID] = item
	}
	for conID, item := range annotations {
		if bad[conID] || item.OwnedQuantity == nil || item.Quantity > *item.OwnedQuantity || rpc.ValidateLendingPosition(item) != nil {
			delete(annotations, conID)
			continue
		}
		// Only a matching same-date fee establishes a customer net rate.
		var rate *float64
		haveRate, conflicting := false, false
		for _, fee := range latest.Financing.Fees {
			if fee.ConID != conID || !fee.ValueDate.Equal(latest.ToDate) {
				continue
			}
			if fee.NetRatePct == nil {
				conflicting = true
				continue
			}
			if haveRate && *rate != *fee.NetRatePct {
				conflicting = true
			}
			rate, haveRate = fee.NetRatePct, true
		}
		if haveRate && !conflicting {
			item.NetRatePct = rate
		}
		annotations[conID] = item
	}
	return annotations
}

// CursorScope binds a page cursor to the full snapshot and exact contract.
func CursorScope(fingerprint string, conID int64) string {
	return digest("", []byte(fingerprint+"\x00"+strconv.FormatInt(conID, 10)))
}
