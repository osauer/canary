//go:build trading

package daemon

import (
	"context"
	"testing"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A price change to a working buy that increases the held position is a fresh
// permission preview too. An accepted
// modify WhatIf must replace the earlier denial before a submit-eligible
// replacement token is minted; an unavailable answer must not clear it.
func TestBrokerPermissionRecoveryIncreasingModify(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		name := "unavailable keeps denial"
		if accepted {
			name = "accepted clears denial"
		}
		t.Run(name, func(t *testing.T) {
			s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			contract := rpc.ContractParams{ConID: 910091, Symbol: "SYNTH", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20260619", Right: "C", Strike: 2, Multiplier: 100}
			draft := rpc.OrderDraft{Contract: contract, OrderRef: "synthetic-permission-modify", Action: rpc.OrderActionBuy, Quantity: 2, OrderType: rpc.OrderTypeLMT, LimitPrice: 2.0, TIF: rpc.OrderTIFDay, OpenClose: "O"}
			event := orderJournalEventForDraft(draft, orderJournalEventBrokerAcknowledged, s.currentTradingStatus(), "synthetic-original-preview", 1002, s.orderNow())
			event.Remaining, event.Status, event.SendState = 2, "Submitted", orderSendStateBrokerAcknowledged
			if err := s.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			scope := s.currentBrokerStateScope()
			denied := rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "YOUR ACCOUNT HAS NO TRADING PERMISSIONS FOR THIS INSTRUMENT"}
			if err := s.brokerPermissions.observe(t.Context(), scope, rpc.BrokerPermissionOptions, brokerPermissionSourcePreview, denied, s.orderNow()); err != nil {
				t.Fatal(err)
			}
			s.orderPreviewQuote = fixedPreviewQuote(2, 2.1)
			s.orderPreviewPositionImpact = fixedPreviewPosition(1, 3, rpc.OrderPositionEffectIncrease)
			calls := 0
			s.orderPreviewWhatIf = func(_ context.Context, got rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				calls++
				if got.Contract.ConID != contract.ConID || got.Action != rpc.OrderActionBuy || got.Quantity != 2 {
					t.Fatalf("modify WhatIf lost reviewed identity/quantity: %+v", got)
				}
				if accepted {
					return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
				}
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusUnavailable, Message: "synthetic broker unavailable"}, nil
			}
			preview, err := s.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: rpc.OrderActionBuy, Contract: contract, Quantity: 2, OrderType: rpc.OrderTypeLMT, LimitPrice: new(2.05), TIF: rpc.OrderTIFDay, ReplaceID: draft.OrderRef})
			state := s.brokerPermissions.state(scope, rpc.BrokerPermissionOptions).State
			if calls != 1 {
				t.Fatalf("increasing modify never reached fresh WhatIf: calls=%d state=%s err=%v", calls, state, err)
			}
			if accepted {
				if err != nil || preview == nil || !preview.SubmitEligible || preview.PreviewToken == "" || preview.PreviewTokenScope != rpc.OrderTokenScopeModify || state != rpc.BrokerPermissionEnabled {
					t.Fatalf("accepted modify did not recover permission and eligible token: state=%s err=%v preview=%+v", state, err, preview)
				}
			} else if err == nil || state != rpc.BrokerPermissionNotEnabled || preview != nil && preview.SubmitEligible {
				t.Fatalf("unavailable modify cleared denial or became eligible: state=%s err=%v preview=%+v", state, err, preview)
			}
		})
	}
}
