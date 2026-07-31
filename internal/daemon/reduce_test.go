package daemon

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestReduceQuantityForPercent(t *testing.T) {
	tests := []struct {
		name        string
		position    float64
		percent     int
		wantQty     int
		wantBlocker bool
	}{
		{"full long is exact close", 100, 100, 100, false},
		{"half long", 100, 50, 50, false},
		{"quarter of three floors to zero", 3, 25, 0, true},
		{"half of three floors to one", 3, 50, 1, false},
		{"seventy-five of four", 4, 75, 3, false},
		{"full short covers all", -10, 100, 10, false},
		{"half short", -10, 50, 5, false},
		{"fractional long floors magnitude", 10.5, 100, 10, false},
		{"percent zero rejected", 100, 0, 0, true},
		{"percent over hundred rejected", 100, 150, 0, true},
		{"sub-one position not reducible", 0.4, 50, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			qty, blockers := reduceQuantityForPercent(tt.position, tt.percent)
			if tt.wantBlocker {
				if len(blockers) == 0 {
					t.Fatalf("expected a blocker, got qty=%d", qty)
				}
				return
			}
			if len(blockers) > 0 {
				t.Fatalf("unexpected blocker: %+v", blockers)
			}
			if qty != tt.wantQty {
				t.Fatalf("qty=%d, want %d", qty, tt.wantQty)
			}
			// Safety invariant: the trim never sizes beyond the floored
			// position magnitude, so it can never flip or open exposure.
			if posInt := int(math.Floor(math.Abs(tt.position))); qty > posInt {
				t.Fatalf("qty %d exceeds floored position %d (would flip)", qty, posInt)
			}
		})
	}
}

func TestIsProtectiveShort(t *testing.T) {
	negDelta := -0.42
	posDelta := 0.55
	tests := []struct {
		name string
		row  rpc.PositionView
		want bool
	}{
		{"long put, nil delta (SPY-style hedge)", rpc.PositionView{SecType: "OPTION", Right: "P", Quantity: 30}, true},
		{"long call, nil delta", rpc.PositionView{SecType: "OPTION", Right: "C", Quantity: 10}, false},
		{"short call, nil delta", rpc.PositionView{SecType: "OPTION", Right: "C", Quantity: -10}, true},
		{"short put, nil delta (long delta, not hedge)", rpc.PositionView{SecType: "OPTION", Right: "P", Quantity: -10}, false},
		{"long put, positive delta wins", rpc.PositionView{SecType: "OPTION", Right: "P", Quantity: 30, Delta: &posDelta}, false},
		{"long call, negative delta wins", rpc.PositionView{SecType: "OPTION", Right: "C", Quantity: 10, Delta: &negDelta}, true},
		{"long stock", rpc.PositionView{SecType: "STOCK", Quantity: 100}, false},
		{"short stock", rpc.PositionView{SecType: "STOCK", Quantity: -100}, true},
		{"flat", rpc.PositionView{SecType: "STOCK", Quantity: 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isProtectiveShort(tt.row); got != tt.want {
				t.Fatalf("isProtectiveShort=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestReduceEligible(t *testing.T) {
	tests := []struct {
		name string
		row  rpc.PositionView
		want bool
	}{
		{"long stock", rpc.PositionView{SecType: "STOCK", Quantity: 100}, true},
		{"short stock", rpc.PositionView{SecType: "STOCK", Quantity: -100}, true},
		{"long option", rpc.PositionView{SecType: "OPTION", Quantity: 10}, true},
		{"short option out of scope", rpc.PositionView{SecType: "OPTION", Quantity: -10}, false},
		{"flat stock", rpc.PositionView{SecType: "STOCK", Quantity: 0}, false},
		{"bond out of scope", rpc.PositionView{SecType: "BOND", Quantity: 10}, false},
		{"future out of scope", rpc.PositionView{SecType: rpc.SecTypeFuture, Quantity: 1}, false},
		{"index out of scope", rpc.PositionView{SecType: rpc.SecTypeIndex, Quantity: 1}, false},
		{"unknown security type fails closed", rpc.PositionView{Quantity: 10}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reduceEligible(tt.row); got != tt.want {
				t.Fatalf("reduceEligible=%v, want %v", got, tt.want)
			}
		})
	}
}

// The reduce workflow is scoped to stocks/ETFs and long options. Unsupported
// holdings must fail before an order draft exists; defaulting an unknown
// PositionView security type to STK can otherwise turn a bond, future, or index
// holding into a stock-shaped broker preview.
func TestReducePreparationNeverRecastsUnsupportedContractAsStock(t *testing.T) {
	for _, secType := range []string{"BOND", rpc.SecTypeFuture, rpc.SecTypeIndex} {
		t.Run(secType, func(t *testing.T) {
			row := rpc.PositionView{
				Symbol:     "SAME",
				SecType:    secType,
				ConID:      500001,
				Currency:   "USD",
				Quantity:   10,
				Multiplier: 1,
				Mark:       100,
			}

			t.Run("single position", func(t *testing.T) {
				prep, blockers := prepareReduceForRow(row, rpc.TradeProposalReduceParams{
					ConID:   row.ConID,
					Percent: 50,
				})
				if len(blockers) != 1 || blockers[0].Code != "not_reducible" {
					t.Fatalf("blockers=%+v, want one not_reducible blocker", blockers)
				}
				if prep.params.Contract.SecType != "" || prep.params.Quantity != 0 {
					t.Fatalf("unsupported %s produced an order draft: %+v", secType, prep.params)
				}
			})

			t.Run("portfolio sweep", func(t *testing.T) {
				prep, blockers := preparedReduceWithQty(row, 5, 1000)
				if len(blockers) != 1 || blockers[0].Code != "not_reducible" {
					t.Fatalf("blockers=%+v, want one not_reducible blocker", blockers)
				}
				if prep.params.Contract.SecType != "" || prep.params.Quantity != 0 {
					t.Fatalf("unsupported %s produced an order draft: %+v", secType, prep.params)
				}
			})
		})
	}
}

func TestFindReducePosition(t *testing.T) {
	pos := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			{Symbol: "AMD", SecType: "STOCK", ConID: 4391, Quantity: 20},
			{Symbol: "IBM", SecType: "STOCK", ConID: 8314, Quantity: 100},
		},
		Options: []rpc.PositionView{
			{Symbol: "SPY", SecType: "OPTION", ConID: 777, Quantity: 30, Right: "P"},
		},
	}

	t.Run("by con_id finds option leg", func(t *testing.T) {
		row, blockers := findReducePosition(pos, 777, "")
		if len(blockers) > 0 {
			t.Fatalf("unexpected blocker: %+v", blockers)
		}
		if row.ConID != 777 {
			t.Fatalf("conID=%d, want 777", row.ConID)
		}
	})
	t.Run("by symbol finds unique stock", func(t *testing.T) {
		row, blockers := findReducePosition(pos, 0, "amd")
		if len(blockers) > 0 {
			t.Fatalf("unexpected blocker: %+v", blockers)
		}
		if row.ConID != 4391 {
			t.Fatalf("conID=%d, want 4391", row.ConID)
		}
	})
	t.Run("unknown con_id blocks", func(t *testing.T) {
		_, blockers := findReducePosition(pos, 999, "")
		if len(blockers) == 0 || blockers[0].Code != "position_not_found" {
			t.Fatalf("want position_not_found, got %+v", blockers)
		}
	})
	t.Run("missing identity blocks", func(t *testing.T) {
		_, blockers := findReducePosition(pos, 0, "")
		if len(blockers) == 0 || blockers[0].Code != "bad_request" {
			t.Fatalf("want bad_request, got %+v", blockers)
		}
	})
	t.Run("ambiguous symbol blocks", func(t *testing.T) {
		dup := &rpc.PositionsResult{Stocks: []rpc.PositionView{
			{Symbol: "BB", SecType: "STOCK", ConID: 1, Quantity: 10},
			{Symbol: "BB", SecType: "STOCK", ConID: 2, Quantity: 20},
		}}
		_, blockers := findReducePosition(dup, 0, "BB")
		if len(blockers) == 0 || blockers[0].Code != "ambiguous_symbol" {
			t.Fatalf("want ambiguous_symbol, got %+v", blockers)
		}
	})
}
