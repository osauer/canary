//go:build trading

package daemon

import (
	"strconv"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A reduction token carries its admission rules through generic order.place,
// including when the first sale is still working and has not changed holdings.
func TestReduceDirectPlaceCannotStackBeyondHeldLine(t *testing.T) {
	t.Parallel()
	for _, firstPercent := range []int{50, 100} {
		t.Run(strconv.Itoa(firstPercent), func(t *testing.T) {
			r := newReduceTokenRig(t, reduceTestStockRow(100))
			first := r.preview(reduceTestStockConID, firstPercent)
			second := r.preview(reduceTestStockConID, 100)
			if res := r.submit(first.PreviewToken, "first"); !res.Accepted {
				t.Fatal("first reduction was refused")
			}
			res, err := r.srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: second.PreviewToken})
			if err == nil || len(r.sends()) != 1 {
				t.Fatalf("direct reduction: accepted=%v error=%v sends=%d; want refusal with only the first order sent", res != nil && res.Accepted, err, len(r.sends()))
			}
		})
	}
}

func TestReduceDirectPlaceKeepsReviewedWorstPrice(t *testing.T) {
	t.Parallel()
	r := newReduceTokenRig(t, reduceTestStockRow(100))
	preview := r.preview(reduceTestStockConID, 50)
	r.quote(24.6, 24.8)
	res, err := r.srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
	if err == nil || len(r.sends()) != 0 {
		t.Fatalf("worse market: accepted=%v error=%v sends=%d; want refusal before sending", res != nil && res.Accepted, err, len(r.sends()))
	}
	// A refusal leaves the token usable when the market returns inside its limit.
	r.quote(26, 26.2)
	res, err = r.srv.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
	if err != nil || res == nil || !res.Accepted || len(r.sends()) != 1 || r.sends()[0].LmtPrice != preview.Terms.LimitPrice {
		t.Fatalf("improved market: accepted=%v error=%v sends=%d; want original reviewed order", res != nil && res.Accepted, err, len(r.sends()))
	}
}
