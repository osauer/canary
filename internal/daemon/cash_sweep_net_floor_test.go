package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// A sweep buy that earns nothing over the cash it replaces is blocked (owner
// decision 2026-10-23 13:16 CEST); a positive estimate below min_net_gain
// stays advisory, and an unavailable estimate never blocks.
func TestCashSweepBuyMustEarnMoreThanCash(t *testing.T) {
	prop, preview := sweepFeePreview(30000, 990, 1)
	prop.CashSweep.MinTranche = 0
	prop.CashSweep.MinNetGainBase = 25
	prop.CashSweep.Bill.Maturity = cashSweepDay(preview.AsOf).AddDate(0, 0, 90).Format(time.DateOnly)
	prop.CashSweep.CashInterest = cashInterestTestSide(0.04)
	preview.Quote.DataType = rpc.MarketDataLive
	preview.Draft.Quantity = 10
	// Ten units, 10000 face at an ask of 99.9: discount 10, fee 1, about 98
	// of cash interest forgone.
	preview.Quote.Ask = new(99.9)
	if got := cashSweepEconomicsBlockers(prop, preview); !hasTradingBlocker(got, rpc.CashSweepBlockerNoNetGain) {
		t.Fatalf("a sweep worse than cash passed: %v", got)
	}
	// At an ask of 99: discount 100 less fee and interest, about +1: below
	// the benchmark, still allowed.
	preview.Quote.Ask = new(99.0)
	if got := cashSweepEconomicsBlockers(prop, preview); len(got) != 0 {
		t.Fatalf("a positive estimate below the benchmark blocked: %v", got)
	}
	// No cash rate: the estimate is unavailable and stays advisory.
	preview.Quote.Ask = new(99.9)
	prop.CashSweep.CashInterest = nil
	if got := cashSweepEconomicsBlockers(prop, preview); len(got) != 0 {
		t.Fatalf("an unavailable estimate blocked: %v", got)
	}
}
