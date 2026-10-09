package daemon

import (
	"context"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestStockAddQuantityDependentFees(t *testing.T) {
	for _, requested := range []int{0, 100} {
		name := "auto"
		if requested > 0 {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			s, in := stockAddTestServer(t)
			in.FreeCash = 1009.995
			s.orderPreviewWhatIf = func(_ context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				return stockAddTestWhatIf(float64(d.Quantity) * 0.01), nil
			}
			p := stockAddTestParams()
			p.LimitPrice = 1
			p.Quantity, p.Max = requested, requested == 0
			got, err := s.planStockAdd(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			// Synthetic fee schedule: 999 shares cost 1008.99; 1000 cost 1010.
			if len(got.Blockers) != 0 {
				t.Fatalf("affordable addition was held: %+v", got.Blockers)
			}
			if requested == 0 {
				if got.Quantity != 999 || got.MaxQuantity != 999 || !got.MaximumKnown {
					t.Fatalf("expected exact maximum 999: %+v", got)
				}
			} else if got.Quantity != requested || got.MaxQuantity != 0 || got.MaximumKnown {
				t.Fatalf("manual check extrapolated a maximum from its fee: %+v", got)
			}
		})
	}
}
