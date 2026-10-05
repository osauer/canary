package daemon

import (
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestTechnicalCurrencyFollowsTheFetchedRoute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		symbols []string
		route   rpc.ContractParams
		routed  bool
		want    []string
		common  string
	}{
		{name: "default stocks", symbols: []string{"SPY", "SYNTH"}, want: []string{"USD", "USD"}, common: "USD"},
		{name: "FX mixed with stock", symbols: []string{"SPY", "USD.JPY", "EUR.USD"}, want: []string{"USD", "JPY", "USD"}},
		{name: "explicit foreign route retains benchmark currency", symbols: []string{"SPY", "SYNTH"}, route: rpc.ContractParams{Currency: "EUR"}, routed: true, want: []string{"USD", "EUR"}},
		{name: "foreign stocks", symbols: []string{"SYNTH", "SYNTH2"}, route: rpc.ContractParams{Currency: "EUR"}, routed: true, want: []string{"EUR", "EUR"}, common: "EUR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rpc.TechnicalResult{Currency: tc.route.Currency}
			for _, symbol := range tc.symbols {
				r.Rows = append(r.Rows, rpc.TechnicalRow{Symbol: symbol})
			}
			setTechnicalCurrencies(r, tc.route, tc.routed, "SPY")
			for i, row := range r.Rows {
				if row.Currency != tc.want[i] {
					t.Fatalf("%s got %q want %q", row.Symbol, row.Currency, tc.want[i])
				}
			}
			if r.Currency != tc.common {
				t.Fatalf("mixed-currency screen mislabeled %q, want %q", r.Currency, tc.common)
			}
		})
	}
}
