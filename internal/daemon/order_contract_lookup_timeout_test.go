package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// A broker that does not answer the exact-contract lookup in time is a
// retryable timeout, not disabled trading: the call plan surfaced
// "Trading disabled: exact contract resolution failed: context deadline
// exceeded" while a retry succeeded. A lookup that fails for any other
// reason still refuses as trading disabled.
func TestOrderContractLookupTimeoutIsRetryableNotTradingDisabled(t *testing.T) {
	plan := rpc.AddCallPlanParams{Contract: rpc.ContractParams{Symbol: "SYNX", SecType: "OPT", Currency: "USD", Exchange: "SMART", Right: "C", Multiplier: 100, ConID: 4242, TradingClass: "SYNX", Expiry: "20991215", Strike: 50},
		Budget: 5000, LimitPrice: 2}
	viaCallPlan := func(cause error) error {
		preview := func(context.Context, rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
			return nil, previewStageRefusal(previewContractUnresolvedCode, orderContractResolutionError(cause))
		}
		_, err := planCallQuantity(t.Context(), plan, 0, preview)
		return err
	}
	for name, cause := range map[string]error{
		"request deadline":       context.DeadlineExceeded,
		"wrapped deadline":       fmt.Errorf("resolve: %w", context.DeadlineExceeded),
		"contract details timer": ibkrlib.ErrContractDetailsTimeout,
	} {
		t.Run(name, func(t *testing.T) {
			err := viaCallPlan(cause)
			if errors.Is(err, ErrTradingDisabled) {
				t.Fatalf("a lookup timeout reads as trading disabled: %v", err)
			}
			code, msg := classifyError(err)
			if code != rpc.CodeTimeout {
				t.Fatalf("code = %q, want %q (%s)", code, rpc.CodeTimeout, msg)
			}
			if strings.Contains(strings.ToLower(msg), "trading disabled") || !strings.Contains(msg, "nothing was placed") || !strings.Contains(msg, "Try again") {
				t.Fatalf("message %q does not say plainly that the lookup timed out and nothing was placed", msg)
			}
			if blockers := previewFailureBlockers(err); len(blockers) != 1 || blockers[0].Message != msg {
				t.Fatalf("preview blockers %+v do not carry the plain words", blockers)
			}
		})
	}
	t.Run("genuine refusal unchanged", func(t *testing.T) {
		err := viaCallPlan(errors.New("contract details overflow"))
		if code, msg := classifyError(err); code != rpc.CodeTradingDisabled || !strings.Contains(msg, "exact contract resolution failed") {
			t.Fatalf("a non-timeout resolution failure changed: %s %s", code, msg)
		}
	})
}
