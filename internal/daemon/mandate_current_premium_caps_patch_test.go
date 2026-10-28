//go:build trading

package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestPostReleasePremiumCapReloadInvalidatesAdmission(t *testing.T) {
	s, _ := mandateTestOverLossServer(t)
	manager, path, _ := rulebookTestManager(t)
	manager.reload()
	s.rulebookPolicies = manager
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 5, rpc.OrderPositionEffectOpen)
	s.orderPreviewQuote = fixedPreviewQuote(7.9, 8.1)
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	sent := 0
	s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
	p := rpc.OrderPreviewParams{Action: "BUY", Quantity: 5, LimitPrice: new(8.0), Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
	preview, err := s.previewOrder(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	writeRulebookTestFile(t, path, "policy_version = 4\noption_name_act_pct = 10\n")
	manager.reload()
	if got := s.rulebookPolicy().OptionNameActPct; got != 10 {
		t.Fatalf("fixture reload=%g", got)
	}
	// Held SYNA premium is already above 10% of 300000; no further buy fits.
	result, _, _, _ := s.currentMandateVerdict()
	t.Logf("active cap=%g retained cap=%g held=%g", s.rulebookPolicy().OptionNameActPct, result.Exposure.NamePremiumCapPct, *result.Exposure.Names[0].PremiumAtRiskBase)
	if fresh, err := s.previewOrder(t.Context(), p); err == nil && fresh != nil {
		t.Errorf("new preview after reload also admitted under the obsolete cap")
	}
	placed, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
	if err == nil || sent != 0 {
		t.Fatalf("reloaded cap did not bind admission: accepted=%v err=%v sends=%d", placed != nil && placed.Accepted, err, sent)
	}
}

func TestPostReleaseBookPremiumCapContextInvalidatesAdmission(t *testing.T) {
	for _, kind := range []string{"policy reload", "regime transition"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := mandateTestOverLossServer(t)
			manager, path, _ := rulebookTestManager(t)
			manager.reload()
			s.rulebookPolicies = manager
			text := "policy_version = 4\n[regime_calm]\npremium_budget_watch_pct = 10\npremium_budget_act_pct = 12\n"
			if kind == "regime transition" {
				text = "policy_version = 4\n[regime_confirmed]\npremium_budget_watch_pct = 10\npremium_budget_act_pct = 12\n"
			}
			writeRulebookTestFile(t, path, text)
			manager.reload()
			if kind == "regime transition" {
				s.rulesRegimeStageLoaded = true
				s.rulesRegimeStage = rulesRegimeStageState{Version: 1, Bucket: "confirmed", Stage: "PANIC", AsOf: s.orderNow()}
			}
			pol := s.rulebookPolicy()
			stage, carried := s.rulebookRegimeStage(pol, s.orderNow())
			_, cap, _ := pol.PremiumBudgetInForce(stage.Bucket, carried)
			if cap != 12 {
				t.Fatalf("fixture active cap=%g bucket=%s", cap, stage.Bucket)
			}
			result, _, _, _ := s.currentMandateVerdict()
			t.Logf("active book cap=%g retained cap=%g", cap, result.Exposure.BookPremiumCapPct)
			draft := rpc.OrderDraft{Action: "BUY", Quantity: 5, LimitPrice: 8, Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
			if blocker := s.mandateBudgetGate(draft, mandateOpen); blocker == nil {
				t.Fatal("current book cap bypassed")
			}
		})
	}
}

func TestCurrentPremiumCapsRecheckedAtWire(t *testing.T) {
	for _, kind := range []string{"name", "book", "regime"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := mandateTestOverLossServer(t)
			manager, path, _ := rulebookTestManager(t)
			manager.reload()
			s.rulebookPolicies = manager
			s.rulesRegimeStageLoaded = true
			s.rulesRegimeStage = rulesRegimeStageState{}
			if kind == "regime" {
				writeRulebookTestFile(t, path, "policy_version = 4\n[regime_confirmed]\npremium_budget_watch_pct = 10\npremium_budget_act_pct = 12\n")
				manager.reload()
			}
			s.orderPreviewPositionImpact = fixedPreviewPosition(0, 5, rpc.OrderPositionEffectOpen)
			s.orderPreviewQuote = fixedPreviewQuote(7.9, 8.1)
			s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			s.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
			sent := 0
			s.orderPlaceBroker = func(context.Context, *ibkr.Contract, *ibkr.RawOrder) error { sent++; return nil }
			p := rpc.OrderPreviewParams{Action: "BUY", Quantity: 5, LimitPrice: new(8.0), Contract: rpc.ContractParams{ConID: 9009, Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Right: "C", Strike: 100, Expiry: "20270115", Multiplier: 100}}
			preview, err := s.previewOrder(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			s.orderWriteBeforeBrokerSend = func() {
				switch kind {
				case "name":
					writeRulebookTestFile(t, path, "policy_version = 4\noption_name_act_pct = 10\n")
					manager.reload()
				case "book":
					writeRulebookTestFile(t, path, "policy_version = 4\n[regime_calm]\npremium_budget_watch_pct = 10\npremium_budget_act_pct = 12\n")
					manager.reload()
				case "regime":
					s.rulesRegimeStage = rulesRegimeStageState{Version: 1, Bucket: "confirmed", Stage: "PANIC", AsOf: s.orderNow()}
				}
			}
			_, err = s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken})
			if err == nil || !strings.Contains(err.Error(), "Mandate premium") || sent != 0 {
				t.Fatalf("%s changed before wire: err=%v sends=%d", kind, err, sent)
			}
		})
	}
}

func TestCurrentPremiumCapRetainsAcceptedLimitOnBadReload(t *testing.T) {
	s, _ := mandateTestOverLossServer(t)
	manager, path, _ := rulebookTestManager(t)
	manager.reload()
	s.rulebookPolicies = manager
	writeRulebookTestFile(t, path, "policy_version = 4\noption_name_act_pct = 10\n")
	manager.reload()
	writeRulebookTestFile(t, path, "policy_version = 5\noption_name_act_pct = nonsense\n")
	manager.reload()
	if got := s.rulebookPolicy().OptionNameActPct; got != 10 {
		t.Fatalf("accepted limit lost after failed read: %g", got)
	}
	draft := rpc.OrderDraft{Action: "BUY", Quantity: 1, LimitPrice: 8, Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "OPT", Currency: "EUR", Multiplier: 100}}
	if blocker := s.mandateBudgetGate(draft, mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "10.0% limit") {
		t.Fatalf("last accepted cap bypassed: %+v", blocker)
	}
	// Zero is an owner-supported cap, not missing evidence or permission to
	// fall back to a baseline. Every positive addition must be held.
	writeRulebookTestFile(t, path, "policy_version = 6\noption_name_act_pct = 0\n")
	manager.reload()
	if blocker := s.mandateBudgetGate(draft, mandateOpen); blocker == nil || !strings.Contains(blocker.Message, "0.0% limit") {
		t.Fatalf("zero cap bypassed: %+v", blocker)
	}
}
