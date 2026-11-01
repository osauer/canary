package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestArmedSelectionOrderLimitKeepsBindingZero(t *testing.T) {
	t.Parallel()
	row := rpc.PositionView{SecType: rpc.SecTypeOption, Currency: "EUR", Mark: 150, Multiplier: 100}
	for _, tc := range []struct {
		name         string
		native, base float64
		want         int
	}{
		{"native cap below one unit", 10000, 100000, 1},
		{"base cap below one unit", 100000, 10000, 1},
		{"both caps below one unit", 10000, 5000, 1},
		{"native cap tighter", 31000, 100000, 2},
		{"base cap tighter", 100000, 31000, 2},
		{"native cap only", 31000, 0, 2},
		{"base cap only", 0, 31000, 2},
		{"no cap", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mandateOrderLimit(row, "EUR", tc.native, tc.base); got != tc.want {
				t.Fatalf("limit = %d contracts at 15000 per contract; native cap %.0f, base cap %.0f: want %d", got, tc.native, tc.base, tc.want)
			}
		})
	}
}
