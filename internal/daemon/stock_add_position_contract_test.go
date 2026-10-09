package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestStockAddCanonicalHeldStockAccounting(t *testing.T) {
	for _, currency := range []string{"EUR", "USD"} {
		t.Run(currency, func(t *testing.T) {
			row := rpc.PositionView{Symbol: "SYNA", SecType: positionSecType("STK"), ConID: 101, Currency: currency, Quantity: 4, Mark: 20, FXRate: new(2.)}
			input := risk.StockAddInput{}
			contract := rpc.ContractParams{Symbol: row.Symbol, ConID: row.ConID, SecType: "STK", Currency: currency}
			want := 80.
			if currency == "EUR" {
				want = 160
			}
			if err := stockAddHeldStocks(&input, &rpc.PositionsResult{Stocks: []rpc.PositionView{row}}, contract, "USD"); err != nil || input.CurrentQuantity != 4 || input.StockValueBase != want || input.UnderlyingStockBase != want {
				t.Fatalf("canonical held stock lost: %+v %v", input, err)
			}
			for _, unsupported := range []string{rpc.SecTypeOption, rpc.SecTypeFuture, "BOND", ""} {
				row.SecType = unsupported
				if err := stockAddHeldStocks(&risk.StockAddInput{}, &rpc.PositionsResult{Stocks: []rpc.PositionView{row}}, contract, "USD"); err == nil {
					t.Fatalf("non-stock %q gained stock authority", unsupported)
				}
			}
		})
	}
}

func TestPortfolioPlanCanonicalHeldStocksRemainInUniverse(t *testing.T) {
	rows := []rpc.PositionView{
		{Symbol: "SYNE", SecType: positionSecType("STK"), ConID: 101, Currency: "EUR", Quantity: 4, Mark: 20, FXRate: new(2.)},
		{Symbol: "SYNU", SecType: positionSecType("STK"), ConID: 102, Currency: "USD", Quantity: 3, Mark: 30},
	}
	policy := &risk.PortfolioPlanPolicy{Targets: []risk.PortfolioPlanTarget{{ConID: 101, Symbol: "SYNE", Currency: "EUR"}, {ConID: 102, Symbol: "SYNU", Currency: "USD"}}}
	evidence, unassigned := portfolioTargetEvidence(policy, &rpc.AccountResult{BaseCurrency: "USD"}, &rpc.PositionsResult{Stocks: rows}, &rpc.Watchlist{}, time.Now())
	if len(evidence) != 2 || len(unassigned) != 0 {
		t.Fatalf("canonical stocks omitted or unassigned: %+v %+v", evidence, unassigned)
	}
	for i, e := range evidence {
		if !e.Complete || !e.InUniverse || e.Quantity != rows[i].Quantity {
			t.Fatalf("holding mistaken for an opening: %+v", e)
		}
	}
}
