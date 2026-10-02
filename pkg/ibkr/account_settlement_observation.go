package ibkr

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// AccountSettlementObservation describes completed request callbacks without
// account identifiers or amounts. It cannot authorize a sweep.
type AccountSettlementObservation struct {
	AsOf      time.Time
	Callbacks int
	Rows      []AccountSettlementRow
}

// AccountSettlementRow preserves the broker label and mathematical finiteness,
// not the value or availability. A finite IBKR unset sentinel remains unavailable.
type AccountSettlementRow struct {
	Currency string
	Source   string
	Finite   bool
}

func observeAccountSettlementRow(out *AccountSettlementObservation, account, tag, value, currency, expected string, managed []string) {
	field, prefixed := splitGatewayLedgerTag(tag)
	if field != "SettledCash" {
		return
	}
	disposition := accountSummaryRequestRowDisposition(account, tag, currency, expected, managed)
	if disposition == accountSummaryRowReject {
		return
	}
	source := "account_total"
	switch {
	case disposition == accountSummaryRowAcceptLedger && prefixed:
		source = "broker_ledger_label"
	case accountCodeConcrete(account) && disposition == accountSummaryRowIgnore && !strings.EqualFold(strings.TrimSpace(account), strings.TrimSpace(expected)):
		return // managed sibling
	case disposition == accountSummaryRowIgnore:
		source = "aggregate_ambiguous"
	}
	if !concreteAccountSummaryLedgerCurrency(currency) && currency != "BASE" {
		currency, source = "", "invalid_currency"
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	out.Callbacks++
	// Bound diagnostics even if a provider repeats or floods callbacks.
	if len(out.Rows) < 128 {
		out.Rows = append(out.Rows, AccountSettlementRow{Currency: currency, Source: source, Finite: err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0)})
	}
}
