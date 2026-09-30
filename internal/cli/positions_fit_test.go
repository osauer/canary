package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A narrow terminal folds only columns that repeat what the screen already
// says, in order, and only until the table fits: a uniform currency moves
// into the title, and a quote that differs from the mark or a data cell that
// carries a warning keeps its column.
func TestStocksTableFoldsRedundantColumnsToFit(t *testing.T) {
	stock := func(sym string, qty, mark float64) rpc.PositionView {
		return rpc.PositionView{Symbol: sym, SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1, Quantity: qty,
			AvgCost: mark - 10, Mark: mark, QuotePrice: new(mark), DataType: rpc.MarketDataLive,
			RegularClose: new(mark - 1), MarketValue: qty * mark, UnrealizedPnL: qty * 10, DailyPnL: new(qty)}
	}
	rows := []rpc.PositionView{stock("AAPL", 1200, 227.40), stock("MSFT", 500, 438.20)}

	t.Setenv("COLUMNS", "100")
	var out bytes.Buffer
	renderStocksTable(&Env{}, &out, rows, rpc.MarketDataLive, false, false)
	text := out.String()
	if !strings.HasPrefix(text, "Stocks & ETFs · USD\n") {
		t.Fatalf("uniform currency did not move into the title:\n%s", text)
	}
	for _, folded := range []string{"CCY", "QUOTE", "DATA", "AS OF"} {
		if strings.Contains(text, folded) {
			t.Errorf("redundant column %s kept at 100 columns:\n%s", folded, text)
		}
	}
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		if visibleLen(line) > 100 {
			t.Errorf("line wider than the terminal (%d): %q", visibleLen(line), line)
		}
	}
	if !strings.Contains(text, "\n  ─") {
		t.Errorf("header rule is not indented under the header:\n%s", text)
	}

	rows[1].QuotePrice = new(438.90)
	rows[1].QuoteQuality = "wide"
	out.Reset()
	renderStocksTable(&Env{}, &out, rows, rpc.MarketDataLive, false, false)
	for _, kept := range []string{"QUOTE", "DATA", "wide", "438.90"} {
		if !strings.Contains(out.String(), kept) {
			t.Errorf("column carrying information folded; missing %q:\n%s", kept, out.String())
		}
	}

	// A pipe (no COLUMNS, not a terminal) keeps every column for scripts.
	t.Setenv("COLUMNS", "")
	out.Reset()
	renderStocksTable(&Env{}, &out, rows, rpc.MarketDataLive, false, false)
	for _, kept := range []string{"CCY", "QUOTE", "DATA", "AS OF"} {
		if !strings.Contains(out.String(), kept) {
			t.Errorf("pipe output lost column %s:\n%s", kept, out.String())
		}
	}
}
