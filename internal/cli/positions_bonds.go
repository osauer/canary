package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

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
