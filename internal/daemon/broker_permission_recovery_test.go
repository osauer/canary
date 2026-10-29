package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	"slices"
	"testing"
	"time"
)

func brokerPermissionRecoveryRig(t *testing.T, sec string) (*Server, rpc.OrderPreviewParams, *int, *bool) {
	t.Helper()
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	s.orderPreviewQuote = fixedPreviewQuote(2, 2.1)
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	allowed := false
	calls := 0
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		calls++
		if allowed {
			return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
		}
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "YOUR ACCOUNT HAS NO TRADING PERMISSIONS FOR THIS INSTRUMENT"}, nil
	}
	c := rpc.ContractParams{ConID: 910001, Symbol: "SYNTH", SecType: sec, Exchange: "SMART", Currency: "USD"}
	if sec == "OPT" {
		c.Expiry = "20260619"
		c.Right = "C"
		c.Strike = 2
		c.Multiplier = 100
	}
	p := rpc.OrderPreviewParams{Action: "BUY", Contract: c, Quantity: 1, OrderType: "LMT", LimitPrice: new(2.05), TIF: "DAY"}
	if err := s.brokerPermissions.bindCore(t.Context(), s.coreStore); err != nil {
		t.Fatal(err)
	}
	return s, p, &calls, &allowed
}

func TestBrokerPermissionOpeningPreviewRecovers(t *testing.T) {
	for _, sec := range []string{"OPT", "STK"} {
		t.Run(sec, func(t *testing.T) {
			s, p, calls, allowed := brokerPermissionRecoveryRig(t, sec)
			_, firstErr := s.previewOrder(t.Context(), p)
			class := brokerPermissionClass(p.Contract)
			scope := s.currentBrokerStateScope()
			if *calls != 1 || s.brokerPermissions.state(scope, class).State != rpc.BrokerPermissionNotEnabled {
				t.Fatalf("first broker denial was not learned: calls=%d err=%v", *calls, firstErr)
			}
			*allowed = true
			result, err := s.previewOrder(t.Context(), p)
			if err != nil || result == nil || !result.SubmitEligible || result.PreviewToken == "" || *calls != 2 || s.brokerPermissions.state(scope, class).State != rpc.BrokerPermissionEnabled {
				t.Fatalf("permission granted but explicit opening preview never relearned it: calls=%d state=%s err=%v", *calls, s.brokerPermissions.state(scope, class).State, err)
			}
		})
	}
}

func TestBrokerPermissionOpeningPreviewRecoversPersistedDenial(t *testing.T) {
	s, p, _, _ := brokerPermissionRecoveryRig(t, "OPT")
	if _, err := s.previewOrder(t.Context(), p); s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionOptions).State != rpc.BrokerPermissionNotEnabled {
		t.Fatalf("initial refusal not learned: %v", err)
	}
	restarted, params, calls, allowed := brokerPermissionRecoveryRig(t, "OPT")
	*allowed = true
	if err := restarted.brokerPermissions.bindCore(t.Context(), s.coreStore); err != nil {
		t.Fatal(err)
	}
	if restarted.brokerPermissions.state(restarted.currentBrokerStateScope(), rpc.BrokerPermissionOptions).State != rpc.BrokerPermissionNotEnabled {
		t.Fatal("restart lost the persistent denial")
	}
	result, err := restarted.previewOrder(t.Context(), params)
	if err != nil || result == nil || !result.SubmitEligible || result.PreviewToken == "" || *calls != 1 || restarted.brokerPermissions.state(restarted.currentBrokerStateScope(), rpc.BrokerPermissionOptions).State != rpc.BrokerPermissionEnabled {
		t.Fatalf("explicit preview did not recover persisted denial: calls=%d err=%v", *calls, err)
	}
}

// A close is never refused for a stored denial, and its acceptance shows
// nothing about opening; the next opening preview asks the broker again.
func TestBrokerPermissionClosingPreviewLeavesTheDenial(t *testing.T) {
	s, p, calls, allowed := brokerPermissionRecoveryRig(t, "OPT")
	s.previewOrder(t.Context(), p)
	*allowed = true
	s.orderPreviewPositionImpact = fixedPreviewPosition(-1, 0, rpc.OrderPositionEffectClose)
	_, err := s.previewOrder(t.Context(), p)
	if err != nil || *calls != 2 || s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionOptions).State != rpc.BrokerPermissionNotEnabled {
		t.Fatalf("closing preview: calls=%d err=%v state=%s", *calls, err, s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionOptions).State)
	}
	s.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	_, err = s.previewOrder(t.Context(), p)
	if err != nil || *calls != 3 || s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionOptions).State != rpc.BrokerPermissionEnabled {
		t.Fatalf("opening preview after the close: calls=%d err=%v", *calls, err)
	}
}

func TestBrokerPermissionRecoveryKeepsDenialWithoutAcceptance(t *testing.T) {
	for _, outcome := range []string{"permission_rejection", "other_rejection", "unavailable", "transport_error"} {
		t.Run(outcome, func(t *testing.T) {
			s, p, calls, _ := brokerPermissionRecoveryRig(t, "OPT")
			s.previewOrder(t.Context(), p)
			s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				*calls++
				switch outcome {
				case "permission_rejection":
					return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Message: "No trading permissions"}, nil
				case "other_rejection":
					return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Message: "insufficient margin"}, nil
				case "transport_error":
					return rpc.OrderWhatIfResult{}, errors.New("synthetic broker unavailable")
				default:
					return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusUnavailable}, nil
				}
			}
			result, err := s.previewOrder(t.Context(), p)
			if *calls != 2 || err == nil || result != nil || s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionOptions).State != rpc.BrokerPermissionNotEnabled {
				t.Fatalf("denial changed or retry did not reach broker: calls=%d result=%v err=%v", *calls, result != nil, err)
			}
		})
	}
}

func TestBrokerPermissionRecoveryRetainsRiskAdmission(t *testing.T) {
	s, p, calls, _ := brokerPermissionRecoveryRig(t, "OPT")
	s.previewOrder(t.Context(), p)
	installTestOrderLimits(s, testOrderLimitsFromTrading(config.Trading{MaxNotional: new(1.0)}))
	_, err := s.previewOrder(t.Context(), p)
	if err == nil || *calls != 1 || !slices.Contains(refusalCodes(err), "order_risk_limit") {
		t.Fatalf("risk admission did not stop retry before broker: calls=%d codes=%v err=%v", *calls, refusalCodes(err), err)
	}
}

func TestBrokerPermissionRecoveryEvidenceKeepsOriginalAccount(t *testing.T) {
	for _, changeDuringQuote := range []bool{false, true} {
		t.Run(fmt.Sprint("changeDuringQuote=", changeDuringQuote), func(t *testing.T) {
			s, p, calls, _ := brokerPermissionRecoveryRig(t, "OPT")
			original := s.currentBrokerStateScope()
			if changeDuringQuote {
				quote := s.orderPreviewQuote
				s.orderPreviewQuote = func(ctx context.Context, c rpc.ContractParams, wait time.Duration) (rpc.OrderQuoteSnapshot, error) {
					s.cfg.Gateway.Account = "DU0000000"
					return quote(ctx, c, wait)
				}
			}
			s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				*calls++
				s.cfg.Gateway.Account = "DU0000000"
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			s.previewOrder(t.Context(), p)
			if *calls != 1 || s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionOptions).State == rpc.BrokerPermissionEnabled || s.brokerPermissions.state(original, rpc.BrokerPermissionOptions).State != rpc.BrokerPermissionEnabled {
				t.Fatal("permission reply was attributed to another account")
			}
		})
	}
}

func TestBrokerPermissionBondPreviewRecovers(t *testing.T) {
	rig := newBondRequestRig(t)
	cusip := syntheticCUSIP(t, "91282CZZ")
	rig.line(cusip, "USD", 8101)
	rig.treasury[cusip] = []treasuryDirectSecurity{{CUSIP: cusip, SecurityType: "Note", Type: "Note", MaturityDate: cashSweepDay(rig.now).AddDate(7, 0, 0).Format(time.DateOnly) + "T00:00:00", InterestRate: "5.000000"}}
	accepted := rig.srv.orderPreviewWhatIf
	calls := 0
	allowed := false
	rig.srv.orderPreviewWhatIf = func(ctx context.Context, d rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		calls++
		if !allowed {
			return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Message: "No trading permissions"}, nil
		}
		return accepted(ctx, d)
	}
	rig.preview(rpc.OrderActionBuy, cusip, "BOND", "USD", 25000)
	if calls != 1 || rig.srv.brokerPermissions.state(rig.srv.currentBrokerStateScope(), rpc.BrokerPermissionBonds).State != rpc.BrokerPermissionNotEnabled {
		t.Fatal("first bond denial not learned")
	}
	allowed = true
	result, err := rig.preview(rpc.OrderActionBuy, cusip, "BOND", "USD", 25000)
	if err != nil || calls != 2 || result == nil || !result.SubmitEligible || rig.srv.brokerPermissions.state(rig.srv.currentBrokerStateScope(), rpc.BrokerPermissionBonds).State != rpc.BrokerPermissionEnabled {
		t.Fatalf("bond recovery refused: calls=%d err=%v", calls, err)
	}
}
