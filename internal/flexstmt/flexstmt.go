// Package flexstmt parses IBKR Flex query statements into typed records
// for the post-trade reconciliation engine
// (internal-docs/design/post-trade-truth.md). Pure parsing: no I/O, no policy, no
// matching. Statement content is untrusted broker data — everything is
// extracted through typed attributes, an unknown cash-transaction type
// lands in Uncategorized rather than being dropped, and nothing in a
// statement can carry an instruction anywhere.
package flexstmt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// Cash-transaction categories after classification.
const (
	// CategoryFlow is an external capital flow (deposit/withdrawal) —
	// the lines the declared ledger must account for.
	CategoryFlow = "flow"
	// CategoryClassified is a known non-flow line (dividend, interest,
	// fee, tax, …): legitimate P&L, excluded from flow matching by type.
	CategoryClassified = "classified"
	// CategoryUncategorized is a line type this parser does not know.
	// Always surfaced as an exception downstream, never silently dropped.
	CategoryUncategorized = "uncategorized"
)

// knownNonFlowTypes maps Flex cash-transaction type strings that are
// account P&L, not external flows. Everything not here and not
// "Deposits/Withdrawals" is uncategorized.
var knownNonFlowTypes = map[string]bool{
	"Dividends":                    true,
	"Payment In Lieu Of Dividends": true,
	"Withholding Tax":              true,
	"Broker Interest Paid":         true,
	"Broker Interest Received":     true,
	"Bond Interest Paid":           true,
	"Bond Interest Received":       true,
	"Other Fees":                   true,
	"Commission Adjustments":       true,
	"Price Adjustments":            true,
}

// CashLine is one classified cash-transaction line.
type CashLine struct {
	// ID is the broker transactionID when present, else a stable hash of
	// the line's identifying attributes. Restatements supersede by ID.
	ID       string
	Category string // flow | classified | uncategorized
	Type     string // raw Flex type attribute
	Currency string
	// Amount is nil when the statement omitted the amount attribute. An
	// explicit zero remains a present value.
	Amount *float64
	// AmountBase is Amount converted at the statement's fxRateToBase;
	// nil when the statement carried no amount or no usable rate (never
	// fabricated).
	AmountBase  *float64
	ValueDate   time.Time // settleDate when present, else dateTime's day
	Description string
}

// Transfer is one position/cash transfer (ACATS, internal). Treated as a
// flow candidate downstream; a transfer with no computable base amount is
// an uncategorized exception, not a guess.
type Transfer struct {
	ID        string
	Direction string // IN | OUT
	Date      time.Time
	// CashTransfer is nil when the statement omitted the cashTransfer
	// attribute. An explicit zero remains a present value.
	CashTransfer *float64
	AmountBase   *float64
	Description  string
}

// EquityRow is one day of the base-currency equity series.
type EquityRow struct {
	ReportDate time.Time
	TotalBase  float64
}

// Statement is one parsed Flex statement.
type Statement struct {
	AccountID     string
	FromDate      time.Time
	ToDate        time.Time
	WhenGenerated time.Time
	Cash          []CashLine
	Transfers     []Transfer
	Equity        []EquityRow
}

type xmlFlexQueryResponse struct {
	XMLName    xml.Name `xml:"FlexQueryResponse"`
	Statements []struct {
		AccountID     string `xml:"accountId,attr"`
		FromDate      string `xml:"fromDate,attr"`
		ToDate        string `xml:"toDate,attr"`
		WhenGenerated string `xml:"whenGenerated,attr"`
		Cash          []struct {
			TransactionID string   `xml:"transactionID,attr"`
			Type          string   `xml:"type,attr"`
			Currency      string   `xml:"currency,attr"`
			FXRateToBase  float64  `xml:"fxRateToBase,attr"`
			Amount        *float64 `xml:"amount,attr"`
			DateTime      string   `xml:"dateTime,attr"`
			SettleDate    string   `xml:"settleDate,attr"`
			Description   string   `xml:"description,attr"`
		} `xml:"CashTransactions>CashTransaction"`
		Transfers []struct {
			TransactionID        string   `xml:"transactionID,attr"`
			Date                 string   `xml:"date,attr"`
			Direction            string   `xml:"direction,attr"`
			CashTransfer         *float64 `xml:"cashTransfer,attr"`
			PositionAmountInBase float64  `xml:"positionAmountInBase,attr"`
			FXRateToBase         float64  `xml:"fxRateToBase,attr"`
			Description          string   `xml:"description,attr"`
		} `xml:"Transfers>Transfer"`
		Equity []struct {
			ReportDate string  `xml:"reportDate,attr"`
			Total      float64 `xml:"total,attr"`
		} `xml:"EquitySummaryInBase>EquitySummaryByReportDateInBase"`
	} `xml:"FlexStatements>FlexStatement"`
}

// Parse decodes one Flex query response document. It returns an error for
// anything that is not a well-formed statement — including the Flex
// service's own error/status envelope — so a failed fetch can never be
// mistaken for an empty week.
func Parse(data []byte) ([]Statement, error) {
	trimmed := strings.TrimSpace(string(data))
	if strings.Contains(trimmed, "<FlexStatementResponse") {
		return nil, fmt.Errorf("flex service envelope, not a statement (fetch not complete or errored)")
	}
	var doc xmlFlexQueryResponse
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse flex statement: %w", err)
	}
	if len(doc.Statements) == 0 {
		return nil, fmt.Errorf("flex response contains no FlexStatement")
	}
	out := make([]Statement, 0, len(doc.Statements))
	for _, raw := range doc.Statements {
		st := Statement{AccountID: strings.TrimSpace(raw.AccountID)}
		var err error
		if st.FromDate, err = parseFlexDate(raw.FromDate); err != nil {
			return nil, fmt.Errorf("statement fromDate: %w", err)
		}
		if st.ToDate, err = parseFlexDate(raw.ToDate); err != nil {
			return nil, fmt.Errorf("statement toDate: %w", err)
		}
		if st.WhenGenerated, err = parseFlexDate(raw.WhenGenerated); err != nil {
			return nil, fmt.Errorf("statement whenGenerated: %w", err)
		}
		for _, c := range raw.Cash {
			amount := 0.0
			if c.Amount != nil {
				amount = *c.Amount
			}
			line := CashLine{
				Type:        strings.TrimSpace(c.Type),
				Currency:    strings.TrimSpace(c.Currency),
				Amount:      c.Amount,
				Description: strings.TrimSpace(c.Description),
			}
			switch {
			case line.Type == "Deposits/Withdrawals":
				line.Category = CategoryFlow
			case knownNonFlowTypes[line.Type]:
				line.Category = CategoryClassified
			default:
				line.Category = CategoryUncategorized
			}
			if c.Amount != nil && c.FXRateToBase > 0 {
				base := amount * c.FXRateToBase
				line.AmountBase = &base
			}
			dateSrc := c.SettleDate
			if strings.TrimSpace(dateSrc) == "" {
				dateSrc = c.DateTime
			}
			if line.ValueDate, err = parseFlexDate(dateSrc); err != nil {
				return nil, fmt.Errorf("cash transaction date %q: %w", dateSrc, err)
			}
			line.ID = lineID(c.TransactionID, "cash", line.Type, dateSrc, amount, line.Description)
			st.Cash = append(st.Cash, line)
		}
		for _, t := range raw.Transfers {
			tr := Transfer{
				Direction:    strings.ToUpper(strings.TrimSpace(t.Direction)),
				CashTransfer: t.CashTransfer,
				Description:  strings.TrimSpace(t.Description),
			}
			if tr.Date, err = parseFlexDate(t.Date); err != nil {
				return nil, fmt.Errorf("transfer date: %w", err)
			}
			// A transfer's base value is cash (converted) plus in-kind
			// position value. Only computed from what the line carries.
			var base float64
			var have bool
			if t.CashTransfer != nil && t.FXRateToBase > 0 {
				base += *t.CashTransfer * t.FXRateToBase
				have = true
			}
			if t.PositionAmountInBase != 0 {
				base += t.PositionAmountInBase
				have = true
			}
			if have {
				tr.AmountBase = &base
			}
			amount := t.PositionAmountInBase
			if t.CashTransfer != nil {
				amount += *t.CashTransfer
			}
			tr.ID = lineID(t.TransactionID, "transfer", tr.Direction, t.Date, amount, tr.Description)
			st.Transfers = append(st.Transfers, tr)
		}
		for _, e := range raw.Equity {
			row := EquityRow{TotalBase: e.Total}
			if row.ReportDate, err = parseFlexDate(e.ReportDate); err != nil {
				return nil, fmt.Errorf("equity reportDate: %w", err)
			}
			st.Equity = append(st.Equity, row)
		}
		out = append(out, st)
	}
	return out, nil
}

// parseFlexDate accepts the Flex date shapes: "20260708", "2026-07-08",
// each with an optional ";HHMMSS" time suffix.
func parseFlexDate(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	datePart, timePart, hasTime := strings.Cut(v, ";")
	layout := "20060102"
	if strings.Contains(datePart, "-") {
		layout = "2006-01-02"
	}
	if hasTime {
		return time.Parse(layout+";150405", datePart+";"+timePart)
	}
	return time.Parse(layout, datePart)
}

// lineID prefers the broker transaction id; a line without one gets a
// stable content hash so restatement supersede-by-id still works.
func lineID(txnID, kind, typ, date string, amount float64, desc string) string {
	if id := strings.TrimSpace(txnID); id != "" {
		return kind + "-" + id
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%s|%.4f|%s", kind, typ, date, amount, desc))
	return kind + "-synth-" + hex.EncodeToString(sum[:8])
}
