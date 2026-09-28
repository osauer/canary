package cli

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFormatProposalOptionExitKeepsMechanismsExplicit(t *testing.T) {
	lossReturn := -62.0
	loss := formatProposalOptionExit(&rpc.TradeProposalOptionExit{
		Kind: "loss_exit", ReturnPct: &lossReturn, LossExitPct: 60, DTE: 31,
	})
	for _, want := range []string{"premium -62.0% vs cost", "full-close line -60.0%", "may remain unfilled", "no resting loss stop", "31 DTE"} {
		if !strings.Contains(loss, want) {
			t.Fatalf("loss output %q missing %q", loss, want)
		}
	}

	profitReturn, initialLock := 55.0, 7.0
	profit := formatProposalOptionExit(&rpc.TradeProposalOptionExit{
		Kind: "profit_trail", ReturnPct: &profitReturn, ProfitArmGainPct: 50,
		LockedGainPct: 5, InitialLockedGainPct: &initialLock, DTE: 31,
	})
	for _, want := range []string{"premium +55.0% vs cost", "armed at +50.0%", "initial lock +7.0%", "DAY TRAIL LIMIT", "31 DTE"} {
		if !strings.Contains(profit, want) {
			t.Fatalf("profit output %q missing %q", profit, want)
		}
	}
}

// Owner decisions of 2026-09-28: an in-the-money long option is closed in the
// Rulebook's expiry window, and a single option's profit trail keeps its high
// water. Each mechanism names its order; a held row does not claim missing
// evidence.
func TestFormatProposalOptionExitNamesExpiryCloseTakeAndHeldTrail(t *testing.T) {
	underlying, gain := 105.0, 20.0
	expiry := formatProposalOptionExit(&rpc.TradeProposalOptionExit{
		Kind: "expiry_close", ReturnPct: &gain, UnderlyingPrice: &underlying, ExpiryCloseDTE: 7, DTE: 5,
	})
	for _, want := range []string{"in the money at underlying 105.00", "expiry window 7 DTE", "avoids exercise at expiry", "may remain unfilled", "5 DTE"} {
		if !strings.Contains(expiry, want) {
			t.Fatalf("expiry output %q missing %q", expiry, want)
		}
	}
	highWater := 2.00
	trail := formatProposalOptionExit(&rpc.TradeProposalOptionExit{Kind: "profit_trail", ReturnPct: &gain, ProfitArmGainPct: 50, HighWaterPerShare: &highWater, DTE: 30})
	take := formatProposalOptionExit(&rpc.TradeProposalOptionExit{Kind: "profit_take", ReturnPct: &gain, HighWaterPerShare: &highWater, DTE: 30})
	if !strings.Contains(trail, "trails from high water 2.00") || !strings.Contains(take, "trail from high water 2.00 hit") || !strings.Contains(take, "patient midpoint limit close") || strings.Contains(take, "TRAIL LIMIT") {
		t.Fatalf("high-water output: trail %q, take %q", trail, take)
	}
	held := formatProposalOptionExit(&rpc.TradeProposalOptionExit{Kind: "review", ReturnPct: &gain, DTE: 10})
	unmeasured := formatProposalOptionExit(&rpc.TradeProposalOptionExit{Kind: "review", DTE: 10})
	if strings.Contains(held, "evidence unavailable") || !strings.Contains(unmeasured, "evidence unavailable") {
		t.Fatalf("review output: held %q, unmeasured %q", held, unmeasured)
	}
}

func TestFormatProposalOptionTrailSizingSaysNativePercentage(t *testing.T) {
	text := formatProposalTrailSizing(&rpc.TradeProposalTrailSizing{
		Method: "option-profit-lock-v1", SelectedBy: "policy_default",
		PolicyMinPct: 20, PolicyMaxPct: 50, ChosenPct: 30,
	})
	if !strings.Contains(text, "native 30.0% premium trail") || strings.Contains(text, "fixed 30.0%") {
		t.Fatalf("sizing output = %q", text)
	}
}
