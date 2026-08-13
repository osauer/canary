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

func TestFormatProposalOptionTrailSizingSaysNativePercentage(t *testing.T) {
	text := formatProposalTrailSizing(&rpc.TradeProposalTrailSizing{
		Method: "option-profit-lock-v1", SelectedBy: "policy_default",
		PolicyMinPct: 20, PolicyMaxPct: 50, ChosenPct: 30,
	})
	if !strings.Contains(text, "native 30.0% premium trail") || strings.Contains(text, "fixed 30.0%") {
		t.Fatalf("sizing output = %q", text)
	}
}
