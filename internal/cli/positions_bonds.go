package cli

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
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
	if positionTableWidth(cols, table) > terminalTextWidth(out) {
		// Keep comparable figures together; move identifiers and explanations
		// beneath each row. No bond facts are discarded.
		indices := []int{0, 1, 3, 4, 5, 6, 7, 8}
		compactCols := make([]positionTableColumn, len(indices))
		compactRows := make([][]string, len(table))
		for i, index := range indices {
			compactCols[i] = cols[index]
		}
		for i, row := range table {
			for _, index := range indices {
				compactRows[i] = append(compactRows[i], row[index])
			}
		}
		details := make([]string, len(table))
		for i, row := range table {
			detail := "ISIN / CUSIP " + row[2]
			if row[9] != "" {
				detail += " · " + row[9]
			}
			details[i] = detail
		}
		renderPositionTableDetails(env, out, compactCols, compactRows, details)
	} else {
		renderPositionTable(env, out, cols, table)
	}
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
	row := func(label, value string) { labeledRow(env, out, "  ", 23, label, value) }
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

// bondYieldBasisWords are the short names of the yield bases
// (risk.BillYieldBasis*, risk.BondYieldBasis*) the facts table prints.
var bondYieldBasisWords = map[string]string{
	risk.BillYieldBasisAct360: "act/360", risk.BillYieldBasisAct365: "act/365",
	risk.BondYieldBasisAnnual: "ytm annual", risk.BondYieldBasisSemiannual: "ytm semi-annual",
}

// renderBondFacts prints the console's per-line facts
// (internal-docs/design/bond-risk.md, "Bill and bond facts"): per bill or
// bond the price it trades at and where it was read, its yield on a named
// basis, the change since the prior close, the value change for a 25 bp rise
// and the loss per basis point, and what it pays at maturity; beneath each
// row, the purchase and the gain since it split into carry, rates and
// spread, with every part that cannot be stated saying why. Model figures,
// in the line's currency; nothing here judges a line.
func renderBondFacts(env *Env, out io.Writer, rows []rpc.PositionBond) {
	var facts []rpc.PositionBond
	for _, b := range rows {
		if b.Facts != nil {
			facts = append(facts, b)
		}
	}
	if len(facts) == 0 {
		return
	}
	fmt.Fprintln(out, "Bill & bond facts · price per 100 of face, yield on the stated basis")
	cols := []positionTableColumn{
		{header: "SYMBOL", align: positionAlignLeft},
		{header: "PRICE", align: positionAlignRight},
		{header: "YIELD", align: positionAlignRight},
		{header: "VS CLOSE", align: positionAlignRight},
		{header: "+25 BP", align: positionAlignRight},
		{header: "PER BP", align: positionAlignRight},
		{header: "PAYS AT MATURITY", align: positionAlignLeft},
	}
	table := make([][]string, 0, len(facts))
	details := make([]string, 0, len(facts))
	for _, b := range facts {
		f := b.Facts
		price, yield, change, up, dv01, pays := "—", "—", "—", "—", "—", "—"
		if f.Price != nil {
			price = formatPositionPrice(*f.Price) + " " + f.PriceSource
		}
		if f.YieldPct != nil {
			yield = fmt.Sprintf("%.2f%% %s", *f.YieldPct, nonEmpty(bondYieldBasisWords[f.YieldBasis], f.YieldBasis))
		}
		if f.YieldChangeBp != nil {
			change = fmt.Sprintf("%+.0f bp", *f.YieldChangeBp)
		}
		if f.ValueChangeUp25Bp != nil {
			up = formatSignedMoney(*f.ValueChangeUp25Bp, f.Currency, 0)
		}
		if f.DV01 != nil {
			dv01 = formatSignedMoney(-*f.DV01, f.Currency, 2)
		}
		if f.PaysAtMaturity != nil {
			pays = fmt.Sprintf("%s on %s · %dd", risk.FormatOrderMoney(f.PaysAtMaturity.Amount, f.PaysAtMaturity.Currency), f.PaysAtMaturity.Date, f.PaysAtMaturity.Days)
		}
		table = append(table, []string{b.Symbol, price, yield, change, up, dv01, pays})
		details = append(details, bondFactsDetail(f))
	}
	renderPositionTableDetails(env, out, cols, table, details)
	fmt.Fprintln(out)
}

// bondFactsDetail is the line beneath a facts row: the purchase and the
// split gain, or the reasons they are missing, then any quote note.
func bondFactsDetail(f *rpc.BondFacts) string {
	var parts []string
	if f.Unavailable != "" {
		parts = append(parts, f.Unavailable)
	}
	if p := f.Purchase; p != nil {
		bought := "bought"
		if p.Date != "" {
			bought += " " + p.Date
		}
		bought += " at " + formatPositionPrice(p.PricePer100)
		if p.YieldPct != nil {
			bought += fmt.Sprintf(" (%.2f%%)", *p.YieldPct)
		}
		if p.Lots > 1 {
			bought += fmt.Sprintf(", %d lots", p.Lots)
		}
		parts = append(parts, bought+" · "+strings.ReplaceAll(p.Source, "_", " "))
	}
	if pnl := f.PnL; pnl != nil {
		gain := "gain " + formatSignedMoney(pnl.Total, pnl.Currency, 0)
		if pnl.Carry != nil && pnl.Rates != nil {
			gain += " = carry " + formatSignedMoney(*pnl.Carry, "", 0) + " · rates " + formatSignedMoney(*pnl.Rates, "", 0)
			switch {
			case pnl.Spread != nil:
				gain += " · spread " + formatSignedMoney(*pnl.Spread, "", 0)
				if pnl.Benchmark != nil && pnl.Benchmark.SpreadSince != "" {
					gain += " since " + pnl.Benchmark.SpreadSince
				}
			case pnl.SpreadUnavailable != "":
				gain += " · spread: " + pnl.SpreadUnavailable
			}
		}
		if pnl.Benchmark != nil && pnl.Benchmark.SpreadBpNow != nil {
			gain += fmt.Sprintf(" · %+.0f bp over %s", *pnl.Benchmark.SpreadBpNow, nonEmpty(pnl.Benchmark.ISIN, "the benchmark"))
		}
		parts = append(parts, gain)
	}
	if f.SincePurchaseUnavailable != "" {
		parts = append(parts, f.SincePurchaseUnavailable)
	}
	if f.YieldChangeUnavailable != "" {
		parts = append(parts, "vs close: "+f.YieldChangeUnavailable)
	}
	if f.QuoteNote != "" {
		parts = append(parts, f.QuoteNote)
	}
	return strings.Join(parts, " · ")
}

// formatSignedMoney prints a signed amount with its sign and, when given,
// its currency, rounded to decimals.
func formatSignedMoney(v float64, ccy string, decimals int) string {
	scale := math.Pow(10, float64(decimals))
	v = math.Round(v*scale) / scale
	sign := "+"
	if v < 0 {
		sign, v = "-", -v
	}
	if v == 0 {
		sign = ""
	}
	if ccy == "" {
		return sign + strconv.FormatFloat(v, 'f', decimals, 64)
	}
	return sign + risk.FormatOrderMoney(v, ccy)
}
