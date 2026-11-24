//go:build trading

package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// addSpeedDefinitionDelay is how long the simulated broker takes over each
// contract lookup and option-chain answer in the speed scenario: the low end
// of the one to three seconds a horizon the broker took on 2026-11-24.
const addSpeedDefinitionDelay = time.Second

// addLookups counts the contract and option-chain lookups the daemon sent.
func addLookups(d *simDesk) int {
	counts := d.sim.Recorder().Counts()
	return counts["reqContractDetails"] + counts["reqSecDefOptParams"]
}

// addRowsFound reports whether a discovery answered with a contract in every
// horizon it priced.
func addRowsFound(res rpc.AddMarketResult) int {
	found := 0
	for _, row := range res.Calls {
		if row.Contract != nil && row.Contract.ConID > 0 {
			found++
		}
	}
	return found
}

// Option discovery, open and closed, against a broker that takes a second
// over each lookup. A second discovery of the same symbol looks nothing up
// again: a contract's identity holds for the trading day. While the options
// session is closed the broker answers frozen quotes without displayed
// sizes, and discovery prices from them without waiting out the quote
// budget for sizes that never come.
func TestSimulatedTWSAddDiscoverySpeed(t *testing.T) {
	for _, tc := range []struct {
		name string
		open bool
	}{{"open", true}, {"closed", false}} {
		t.Run(tc.name, func(t *testing.T) {
			setSessions(t, tc.open)
			opts := twssim.Options{Book: twssim.OptionsBook(time.Now()), DefinitionDelay: addSpeedDefinitionDelay}
			if !tc.open {
				opts.ClosedMarkets = []string{twssim.MarketUSOptions}
			}
			d := startSimDesk(t, opts, true)

			began := time.Now()
			first := d.market("C")
			firstTook := time.Since(began)
			lookups := addLookups(d)
			began = time.Now()
			second := d.market("C")
			secondTook := time.Since(began)
			again := addLookups(d) - lookups
			t.Logf("%s: first discovery %s (%d lookups, %d rows), second %s (%d lookups, %d rows)", tc.name, firstTook.Round(time.Millisecond), lookups, addRowsFound(first), secondTook.Round(time.Millisecond), again, addRowsFound(second))

			if addRowsFound(first) == 0 || addRowsFound(second) != addRowsFound(first) {
				t.Fatalf("discovery rows: first %d, second %d", addRowsFound(first), addRowsFound(second))
			}
			if again != 0 {
				t.Fatalf("the second discovery looked up %d contracts or chains again", again)
			}
			if !tc.open {
				for _, row := range second.Calls {
					if row.Contract == nil {
						continue
					}
					if row.Planning == nil || row.Planning.Price == nil || row.Quote == nil || row.Quote.DataType != rpc.MarketDataFrozen {
						t.Fatalf("closed row without a frozen planning price: %+v", row)
					}
				}
				// Every quote answered at once; only sizes were missing.
				if secondTook >= setupQuoteBudget {
					t.Fatalf("closed discovery took %s: it waited out the %s quote budget", secondTook, setupQuoteBudget)
				}
			}
		})
	}
}
