package cli

import (
	"math"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

// IBKR's averageCost convention differs by SecType: per-share for STOCK,
// per-contract (multiplier-inclusive) for OPTION. The CLI renders AvgCost
// alongside a per-share Mark, so OPTION rows need normalisation to read
// correctly. JSON output stays IBKR-faithful — only the renderer changes.
//
// SecType strings must match the daemon's wire shape exactly. The daemon
// fills PositionView.SecType with string(pkg/ibkr.AssetTypeOption) = "OPTION"
// (not "OPT"); the short form was a v0.12.4 implementation error that
// kept the normalisation from firing in production while these tests
// passed. The literal strings here are the contract.
func TestAvgCostPerShare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   rpc.PositionView
		want float64
	}{
		{
			name: "stock returns raw avg cost (already per-share)",
			in:   rpc.PositionView{SecType: rpc.SecTypeStock, Multiplier: 1, AvgCost: 192.10},
			want: 192.10,
		},
		{
			name: "option with mult=100 divides by multiplier",
			in:   rpc.PositionView{SecType: rpc.SecTypeOption, Multiplier: 100, AvgCost: 510.00},
			want: 5.10,
		},
		{
			name: "option with mult=1000 (some index options) divides correctly",
			in:   rpc.PositionView{SecType: rpc.SecTypeOption, Multiplier: 1000, AvgCost: 4200.00},
			want: 4.20,
		},
		{
			name: "option with unknown multiplier (0) returns raw — better than div-by-zero",
			in:   rpc.PositionView{SecType: rpc.SecTypeOption, Multiplier: 0, AvgCost: 510.00},
			want: 510.00,
		},
		{
			name: "stock with omitted multiplier returns raw",
			in:   rpc.PositionView{SecType: rpc.SecTypeStock, AvgCost: 192.10},
			want: 192.10,
		},
		{
			name: "empty SecType (defensive) returns raw — no OPTION assumption",
			in:   rpc.PositionView{Multiplier: 100, AvgCost: 510.00},
			want: 510.00,
		},
		{
			name: "short three-letter SecType (legacy) returns raw — wire shape is the full word",
			in:   rpc.PositionView{SecType: "OPT", Multiplier: 100, AvgCost: 510.00},
			want: 510.00,
		},
		{
			name: "negative avg cost (theoretically possible on short premium) is preserved through division",
			in:   rpc.PositionView{SecType: rpc.SecTypeOption, Multiplier: 100, AvgCost: -300.00},
			want: -3.00,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := avgCostPerShare(tc.in)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("avgCostPerShare = %v, want %v", got, tc.want)
			}
		})
	}
}
