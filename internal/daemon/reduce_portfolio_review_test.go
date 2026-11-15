package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestReducePortfolioReview(t *testing.T) {
	put := reduceTestOptionRow(1)
	put.Right, put.Delta, put.LocalSymbol, put.TradingClass = "P", new(-0.5), "SYN  261218P00030000", "SYN"
	shortOption := reduceTestOptionRow(-1)
	small := reduceTestStockRow(1)
	small.ConID, small.Symbol = 303, "SMALL"
	opposite := reduceTestStockRow(20)
	opposite.ConID, opposite.Symbol = 404, "OTHER"
	second := reduceTestStockRow(100)
	second.ConID, second.Symbol = 505, "SECOND"
	for _, tc := range []struct {
		name          string
		rows          []rpc.PositionView
		block         int
		before, after float64
		untouched     int
		code          string
		quantity      int
	}{
		{"long stock leaves put", []rpc.PositionView{reduceTestStockRow(100), put}, 0, 1250, 625, put.ConID, "opposite_delta", 25},
		{"net short leaves long", []rpc.PositionView{reduceTestStockRow(-100), opposite}, 0, -2000, -1000, opposite.ConID, "opposite_delta", 40},
		{"whole unit rounding", []rpc.PositionView{reduceTestStockRow(100), small}, 0, 2525, 1275, small.ConID, "below_whole_unit", 50},
		{"short options unsupported", []rpc.PositionView{reduceTestStockRow(100), shortOption}, 0, 1250, 625, shortOption.ConID, "unsupported", 25},
		{"blocked preview stays untouched", []rpc.PositionView{reduceTestStockRow(100), second}, second.ConID, 5000, 3750, second.ConID, "blocked", 50},
		{"all selected", []rpc.PositionView{reduceTestStockRow(100)}, 0, 2500, 1250, 0, "", 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newReducePreviewTestServer(t, tc.rows...)
			if tc.block != 0 {
				srv.orderPreviewWhatIf = func(_ context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
					if draft.Contract.ConID == tc.block {
						return rpc.OrderWhatIfResult{Available: false}, nil
					}
					return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
				}
			}
			res, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Percent: 50})
			if err != nil {
				t.Fatal(err)
			}
			if res.NetDollarDeltaBefore != tc.before || res.NetDollarDeltaAfter == nil || *res.NetDollarDeltaAfter != tc.after {
				t.Fatalf("unexpected before/after: %+v", res)
			}
			if len(res.Legs) == 0 || res.Legs[0].ReduceQuantity != tc.quantity {
				t.Fatalf("sizing changed: %+v", res.Legs)
			}
			if res.UntouchedPositions == nil {
				t.Fatal("complete review must provide a list, even when empty")
			}
			if tc.untouched == 0 {
				if len(res.UntouchedPositions) != 0 {
					t.Fatalf("unexpected untouched positions: %+v", res.UntouchedPositions)
				}
				encoded, err := json.Marshal(res)
				if err != nil || !strings.Contains(string(encoded), `"untouched_positions":[]`) {
					t.Fatalf("missing empty list: %s %v", encoded, err)
				}
				return
			}
			if len(res.UntouchedPositions) != 1 {
				t.Fatalf("untouched positions: %+v", res.UntouchedPositions)
			}
			got := res.UntouchedPositions[0]
			if got.Contract.ConID != tc.untouched || got.Code != tc.code || got.Reason == "" {
				t.Fatalf("wrong disclosure: %+v", got)
			}
			want := tc.rows[1]
			if got.Contract != reduceReviewContract(want) || got.PositionQuantity != want.Quantity {
				t.Fatalf("lost exact held contract or quantity: %+v", got)
			}
		})
	}
}

func TestReducePortfolioReviewIncompleteHasNoAfterEstimate(t *testing.T) {
	row := reduceTestStockRow(100)
	row.Stale = true
	srv, _ := newReducePreviewTestServer(t, row)
	res, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NetDeltaIncomplete || res.NetDollarDeltaAfter != nil || res.UntouchedPositions != nil || len(res.Legs) != 0 {
		t.Fatalf("incomplete snapshot acquired a review: %+v", res)
	}
}

// A locked line is named once, in the untouched list, as locked (owner,
// 2026-10-22: one list of what the reduction keeps). The put is on another
// name than the stock: a lock covers every contract of its name.
func TestReducePortfolioReviewNamesALockedLineOnce(t *testing.T) {
	put := reduceTestOptionRow(1)
	put.Symbol, put.Right, put.Delta = "SYNP", "P", new(-0.5)
	srv, _ := newReducePreviewTestServer(t, reduceTestStockRow(100), put)
	if _, err := replaceHoldingLocks(t, srv, []rpc.HoldingLock{{ConID: put.ConID, Symbol: "SYNP", Role: rpc.HoldingLockRoleHedge}}, 0, "lock-put"); err != nil {
		t.Fatal(err)
	}
	res, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if res.NetDollarDeltaAfter == nil || len(res.UntouchedPositions) != 1 {
		t.Fatalf("review = %+v", res)
	}
	if got := res.UntouchedPositions[0]; got.Contract.ConID != put.ConID || got.Code != reduceKeptLocked {
		t.Fatalf("locked put = %+v, want it untouched as locked", got)
	}
	scoped, err := srv.reducePortfolioPreview(t.Context(), rpc.TradeProposalReducePortfolioParams{Mode: rpc.ReduceModeLongs, Percent: 50})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.UntouchedPositions != nil || scoped.NetDollarDeltaAfter != nil {
		t.Fatalf("a scoped sweep reports Kept, not the net review: %+v", scoped)
	}
}
