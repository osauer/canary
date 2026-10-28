package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestReducePortfolioTextRetainsReviewProjection(t *testing.T) {
	res := rpc.TradeProposalReducePortfolioResult{
		Mode: rpc.ReduceModeNet, BaseCurrency: "USD", NetDollarDeltaBefore: 1000, TargetDollarDelta: 500,
		AchievedDollarDelta: 400, NetDollarDeltaAfter: new(-321.25),
		UntouchedPositions: []rpc.TradeProposalReduceUntouchedPosition{{
			Contract:         rpc.ContractParams{ConID: 700003, Symbol: "SYNC", SecType: "OPT", Expiry: "20261218", Strike: 35, Right: "P"},
			PositionQuantity: -2, Code: "opposite_delta", Reason: "Opposite-direction exposure stays unchanged.",
		}},
	}
	var out bytes.Buffer
	renderProposalReducePortfolioText(&Env{Stdout: &out}, &res, false)
	text := strings.Join(strings.Fields(out.String()), " ")
	// The after field is deliberately different from before minus achieved:
	// adapters must retain the daemon's typed projection, not recompute it.
	for _, value := range []string{formatProposalMoney(-321.25, "USD"), "without locked hedges", "locked holdings are never traded", "SYNC 20261218 35 P", "700003", "-2", "opposite_delta", res.UntouchedPositions[0].Reason} {
		if !strings.Contains(text, value) {
			t.Errorf("rendered review lost %q:\n%s", value, out.String())
		}
	}
}

func TestReducePortfolioTextDistinguishesMissingAndZeroProjection(t *testing.T) {
	for _, tc := range []struct {
		name      string
		after     *float64
		untouched []rpc.TradeProposalReduceUntouchedPosition
	}{
		{name: "unavailable"},
		{name: "zero and none", after: new(0.0), untouched: []rpc.TradeProposalReduceUntouchedPosition{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			res := rpc.TradeProposalReducePortfolioResult{Mode: rpc.ReduceModeNet, BaseCurrency: "USD", NetDollarDeltaAfter: tc.after, UntouchedPositions: tc.untouched}
			renderProposalReducePortfolioText(&Env{Stdout: &out}, &res, false)
			text := strings.Join(strings.Fields(out.String()), " ")
			if got := strings.Contains(text, "Net delta (after)"); got != (tc.after != nil) {
				t.Errorf("after visibility=%v, supplied=%v: %s", got, tc.after != nil, text)
			}
			if got := strings.Contains(text, "Untouched"); got != (tc.untouched != nil) {
				t.Errorf("untouched visibility=%v, supplied=%v: %s", got, tc.untouched != nil, text)
			}
			if tc.after != nil && (!strings.Contains(text, formatMoneyCcyForPnL(0, "USD")) || !strings.Contains(text, "none")) {
				t.Errorf("zero projection or complete empty review lost: %s", text)
			}
		})
	}
}
