package daemon

import (
	"testing"

	"github.com/osauer/canary/v3/internal/risk"
)

// The queue compares a warning's identity, not its figure: a figure that
// moved since signing passes, a code the owner did not sign holds, fewer
// warnings pass, and a record without signed warnings covers none.
func TestQueuedAddWarningsCompareIdentity(t *testing.T) {
	w := func(code, msg string) risk.StockAddBlocker {
		return risk.StockAddBlocker{Kind: "warning", Code: code, Message: msg}
	}
	signed := []risk.StockAddBlocker{w("net_exposure", "Net exposure: 71.2 %; warning level 70.0."), w("hedge_integrity", "fixed")}
	for _, tc := range []struct {
		name          string
		signed, fresh []risk.StockAddBlocker
		want          bool
	}{
		{"equal", signed, signed, true},
		{"figure drift", signed, []risk.StockAddBlocker{w("net_exposure", "Net exposure: 74.9 %; warning level 70.0."), w("hedge_integrity", "fixed")}, true},
		{"fewer", signed, signed[:1], true},
		{"new code", signed, append([]risk.StockAddBlocker{w("margin_headroom", "x")}, signed...), false},
		{"legacy record", nil, signed[:1], false},
		{"other kind", signed, []risk.StockAddBlocker{{Kind: "capacity", Code: "net_exposure"}}, false},
	} {
		if got := addWarningsSigned(tc.signed, tc.fresh); got != tc.want {
			t.Errorf("%s: signed %v, want %v", tc.name, got, tc.want)
		}
	}
}
