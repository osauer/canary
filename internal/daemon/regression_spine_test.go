package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// TestRegressionSpineRejectsUnsupportedReduction preserves the fail-closed
// boundary fixed by 37a5b805. A broad smoke cannot safely ask the broker to
// preview deliberately misclassified holdings, so this negative contract stays
// local and deterministic.
func TestRegressionSpineRejectsUnsupportedReduction(t *testing.T) {
	t.Parallel()
	for _, secType := range []string{"BOND", rpc.SecTypeFuture, rpc.SecTypeIndex, ""} {
		name := secType
		if name == "" {
			name = "unknown"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if reduceEligible(rpc.PositionView{SecType: secType, Quantity: 1}) {
				t.Fatalf("%q holding admitted to the stock/option reduction path", secType)
			}
		})
	}
}

// TestRegressionSpineSeparatesPositionIdentity preserves the identity boundary
// fixed by 205318b5. IBKR reuses symbols across bond issues and futures
// expiries; ConID and contract fields must therefore participate in the key.
func TestRegressionSpineSeparatesPositionIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		left  rpc.PositionView
		right rpc.PositionView
	}{
		{
			name:  "bond_contracts",
			left:  rpc.PositionView{Symbol: "T", SecType: "BOND", ConID: 500001},
			right: rpc.PositionView{Symbol: "T", SecType: "BOND", ConID: 500002},
		},
		{
			name:  "future_expiries",
			left:  rpc.PositionView{Symbol: "ES", SecType: rpc.SecTypeFuture, ConID: 600001, Expiry: "20260320"},
			right: rpc.PositionView{Symbol: "ES", SecType: rpc.SecTypeFuture, ConID: 600002, Expiry: "20260619"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if positionViewKey(test.left) == positionViewKey(test.right) {
				t.Fatalf("distinct %s share position key %q", test.name, positionViewKey(test.left))
			}
		})
	}
}
