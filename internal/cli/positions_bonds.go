package cli

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// positionsStockTableRows is the stock table: every stock row except the
// held bonds the bonds section classifies, which print there instead. A
// BOND row the daemon did not classify stays in the stock table, so nothing
// held ever disappears from the text view.
func positionsStockTableRows(r *rpc.PositionsResult) []rpc.PositionView {
	if len(r.Bonds) == 0 {
		return r.Stocks
	}
	classified := map[int]bool{}
	for _, b := range r.Bonds {
		classified[b.ConID] = b.ConID > 0
	}
	return slices.DeleteFunc(slices.Clone(r.Stocks), func(p rpc.PositionView) bool {
		switch strings.ToUpper(strings.TrimSpace(p.SecType)) {
		case "BOND", "BILL":
			return classified[p.ConID]
		}
		return false
	})
}

// renderBondsTable prints held bills and bonds with their class, maturity,
// identifiers and currency.
func renderBondsTable(env *Env, out io.Writer, rows []rpc.PositionBond) {
	if len(rows) == 0 {
		return
	}
	fmt.Fprintln(out, "Bills & bonds")
	cols := []positionTableColumn{
		{header: "SYMBOL", align: positionAlignLeft},
		{header: "CLASS", align: positionAlignLeft},
		{header: "ISIN / CUSIP", align: positionAlignLeft},
		{header: "CCY", align: positionAlignLeft},
		{header: "MATURITY", align: positionAlignLeft},
		{header: "DAYS", align: positionAlignRight},
		{header: "POS", align: positionAlignRight},
		{header: "MARK", align: positionAlignRight},
		{header: "VALUE", align: positionAlignRight},
		{header: "NOTE", align: positionAlignLeft},
	}
	table := make([][]string, 0, len(rows))
	for _, b := range rows {
		days := "—"
		if b.DaysToMaturity != nil {
			days = fmt.Sprintf("%d", *b.DaysToMaturity)
		}
		ids := strings.Join(slices.DeleteFunc([]string{b.ISIN, b.CUSIP}, func(s string) bool { return s == "" }), " / ")
		table = append(table, []string{
			b.Symbol, b.Class, nonEmpty(ids, "—"), formatPositionCurrency(b.Currency), nonEmpty(b.Maturity, "—"), days,
			formatPositionQuantity(b.Quantity, ""), formatPositionPrice(b.Mark), formatMoneyCcyForPnL(b.MarketValue, b.Currency), b.Reason,
		})
	}
	renderPositionTable(env, out, cols, table)
	fmt.Fprintln(out)
}

// renderBondRisk prints the held bonds' rate risk and issuers
// (internal-docs/design/bond-risk.md, phase 1): per line its issuer, class,
// yield, duration, DV01 and loss if yields rise one point, then the book's
// sums and every line the sums miss. Measurement only.
func renderBondRisk(env *Env, out io.Writer, rows []rpc.PositionBond, book *risk.BondBook) {
	if book == nil {
		return
	}
	ccy := book.BaseCurrency
	fmt.Fprintln(out, "Bond risk · loss if yields rise one point")
	cols := []positionTableColumn{
		{header: "SYMBOL", align: positionAlignLeft},
		{header: "ISSUER", align: positionAlignLeft},
		{header: "CLASS", align: positionAlignLeft},
		{header: "YIELD", align: positionAlignRight},
		{header: "DURATION", align: positionAlignRight},
		{header: "DV01", align: positionAlignRight},
		{header: "LOSS", align: positionAlignRight},
	}
	classWords := map[string]string{rpc.BondIssuerGovernment: "government", rpc.BondIssuerInvestmentGrade: "inv. grade"}
	table := make([][]string, 0, len(rows))
	for _, b := range rows {
		if b.ModifiedDuration == nil || b.YieldPct == nil {
			continue
		}
		table = append(table, []string{b.Symbol, truncateRunes(nonEmpty(b.EvidenceIssuer, b.Issuer), 20), nonEmpty(classWords[b.IssuerClass], "unknown"),
			fmt.Sprintf("%.2f%%", *b.YieldPct), fmt.Sprintf("%.1fy", *b.ModifiedDuration),
			formatOptionalMoney(b.DV01Base, ccy, 2), formatOptionalMoney(b.RateShockLossBase, ccy, 0)})
	}
	if len(table) > 0 {
		renderPositionTable(env, out, cols, table)
	}
	row := func(label, value string) { fmt.Fprintf(out, "  %-22s %s\n", label, value) }
	row("Bonds held", risk.FormatOrderMoney(math.Round(book.MarketValueBase), ccy)+formatPctNLV(book.PctNLV))
	row("One-point rise costs", risk.FormatOrderMoney(math.Round(book.RateShockLossBase), ccy)+formatPctNLV(book.RateShockLossPctNLV))
	if book.NonGovernmentBase != 0 {
		row("Non-government", risk.FormatOrderMoney(math.Round(book.NonGovernmentBase), ccy)+formatPctNLV(book.NonGovernmentPctNLV))
	}
	if len(book.Issuers) > 0 {
		top := book.Issuers[0]
		row("Largest issuer", truncateRunes(top.Issuer, 30)+", "+risk.FormatOrderMoney(math.Round(top.MarketValueBase), ccy)+formatPctNLV(top.PctNLV))
	}
	for _, u := range book.Unmeasured {
		row("Not in the sums", u.Symbol+": "+u.Reason)
	}
	fmt.Fprintln(out)
}

func formatOptionalMoney(v *float64, ccy string, decimals int) string {
	if v == nil {
		return "—"
	}
	scale := math.Pow(10, float64(decimals))
	return risk.FormatOrderMoney(math.Round(*v*scale)/scale, ccy)
}

func formatPctNLV(v *float64) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf(" (%.1f%% of NLV)", *v)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
