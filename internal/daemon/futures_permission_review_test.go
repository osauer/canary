//go:build trading

package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// A direction chosen from one position receipt is no longer proof of an
// opening order after a fill changes the held side. IBKR may accept the new
// closing direction even while opening permission is still absent.
func TestFuturesPermissionReviewDoesNotLearnFromAClosingCheckAfterPositionChange(t *testing.T) {
	s, _, now := brokerPermissionRig(t, nil)
	s.cfg.Gateway.Account = "DU1234567"
	s.endpoint.Account = s.cfg.Gateway.Account
	scope := s.currentBrokerStateScope()
	if err := s.brokerPermissions.observe(t.Context(), scope, rpc.BrokerPermissionFutures, brokerPermissionSourcePreview,
		rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "No trading permissions"}, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	before, generation := 0.0, uint64(1)
	s.orderRiskAuthorityForTest = func(_ context.Context, status rpc.TradingStatus, _ rpc.ContractParams, action string, qty int) (orderPositionAuthority, error) {
		delta := float64(qty)
		if action == rpc.OrderActionSell {
			delta = -delta
		}
		return orderPositionAuthority{
			Impact:     rpc.OrderPositionImpact{Before: before, After: before + delta, Effect: classifyPositionEffect(before, before+delta)},
			Generation: generation, BaseCurrency: "USD", BaseCurrencyProvenance: ibkrlib.AccountBaseCurrencyExplicitTag, EvidenceAt: *now, TestOnly: true,
			Health: ibkrlib.PortfolioStreamHealth{Account: status.Account, InitialCompletedAt: *now, LastUpdateAt: *now, ProjectionGeneration: generation},
		}, nil
	}
	calls := 0
	s.orderPreviewWhatIf = func(_ context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		calls++
		if calls == 1 {
			if draft.Action != rpc.OrderActionBuy {
				t.Fatalf("flat-position check chose %s, want BUY", draft.Action)
			}
			// Another client's sale is reflected before the broker evaluates
			// the request; buying one now closes instead of opening a long.
			before, generation = -1, generation+1
		} else if draft.Action != rpc.OrderActionSell {
			t.Fatalf("stable short-position retry chose %s, want SELL", draft.Action)
		}
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	status := rpc.TradingStatus{Account: scope.Account, Mode: scope.Mode, CanPreview: true}
	if s.checkBrokerPermissions(t.Context(), status) {
		t.Error("a check with changed opening-position authority counted as answered")
	}
	if got := s.brokerPermissions.state(scope, rpc.BrokerPermissionFutures); got.State != rpc.BrokerPermissionNotEnabled {
		t.Fatalf("closing acceptance after portfolio generation changed replaced the opening-permission denial: %+v", got)
	}
	if s.checkBrokerPermissions(t.Context(), status) || calls != 1 {
		t.Fatal("position drift retried before the existing retry interval")
	}
	*now = now.Add(brokerPermissionRetryAfter)
	if !s.checkBrokerPermissions(t.Context(), status) || calls != 2 || s.brokerPermissions.state(scope, rpc.BrokerPermissionFutures).State != rpc.BrokerPermissionEnabled {
		t.Fatal("a stable opening sale did not recover permission on the next retry")
	}
}

// The daily request can remain in flight while a requested opening preview
// learns a newer denial. Finishing the older request must not erase it.
func TestFuturesPermissionReviewOlderDailyReplyCannotEraseNewerDenial(t *testing.T) {
	s, _, now := brokerPermissionRig(t, nil)
	s.cfg.Gateway.Account = "DU1234567"
	s.endpoint.Account = s.cfg.Gateway.Account
	scope := s.currentBrokerStateScope()
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = core.Close() })
	if err := s.brokerPermissions.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	newerAt := now.Add(time.Second)
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		*now = newerAt
		s.noteBrokerPermissionPreview(t.Context(), scope, rpc.OrderDraft{Contract: testMESContract}, rpc.OrderPositionEffectOpen,
			rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "No trading permissions"}, s.nowUTC())
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{Account: scope.Account, Mode: scope.Mode, CanPreview: true})
	if got := s.brokerPermissions.state(scope, rpc.BrokerPermissionFutures); got.State != rpc.BrokerPermissionNotEnabled || !got.CheckedAt.Equal(newerAt) {
		t.Fatalf("an older daily request erased a newer opening-permission refusal: %+v", got)
	}
	var restarted brokerPermissionStore
	if err := restarted.bindCore(t.Context(), core); err != nil {
		t.Fatal(err)
	}
	if got := restarted.state(scope, rpc.BrokerPermissionFutures); got.State != rpc.BrokerPermissionNotEnabled || !got.CheckedAt.Equal(newerAt) {
		t.Fatalf("restart lost the newer opening-permission refusal: %+v", got)
	}
}

func TestFuturesPermissionReviewCheckCannotCrossBrokerIdentity(t *testing.T) {
	for _, change := range []string{"account", "mode", "connector publication"} {
		t.Run(change, func(t *testing.T) {
			s, _, _ := brokerPermissionRig(t, nil)
			if change != "mode" {
				s.cfg.Gateway.Account = "DU1234567"
			}
			scope := s.currentBrokerStateScope()
			s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				switch change {
				case "account":
					s.cfg.Gateway.Account = "DU7654321"
				case "mode":
					s.endpoint.Port = 4002
				case "connector publication":
					s.stopConnector()
				}
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			if s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{Account: scope.Account, Mode: scope.Mode, CanPreview: true}) {
				t.Fatal("a check crossing broker identity counted as answered")
			}
			for _, got := range []rpc.BrokerPermission{s.brokerPermissions.state(scope, rpc.BrokerPermissionFutures), s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionFutures)} {
				if got.State == rpc.BrokerPermissionEnabled {
					t.Fatalf("changed %s still learned opening permission: %+v", change, got)
				}
			}
		})
	}
}

func TestFuturesPermissionReviewRequiresCorrelatedOpeningPosition(t *testing.T) {
	for _, phase := range []string{"stream already behind", "fill during WhatIf", "stable flat", "stable short"} {
		t.Run(phase, func(t *testing.T) {
			s, _, _ := brokerPermissionRig(t, nil)
			s.cfg.Gateway.Account = "DU1234567"
			scope := s.currentBrokerStateScope()
			held := 0.0
			if phase == "stable short" {
				held = -2
			}
			s.orderPreviewPositionImpact = fixedPreviewPosition(held, held+1, classifyPositionEffect(held, held+1))
			generation := uint64(1)
			s.reduceFillGenerationForTest = func() uint64 { return generation }
			receipts := 0
			s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) {
				receipts++
				quantity := held
				if phase == "stream already behind" {
					quantity = -1
				}
				return []*ibkrlib.RawPosition{{Account: scope.Account, Contract: *previewIBKRContract(testMESContract), Position: quantity}}, nil
			}
			calls := 0
			s.orderPreviewWhatIf = func(_ context.Context, draft rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				calls++
				want := rpc.OrderActionBuy
				if held < 0 {
					want = rpc.OrderActionSell
				}
				if draft.Action != want {
					t.Fatalf("check action=%s, want %s", draft.Action, want)
				}
				if phase == "fill during WhatIf" {
					// The sale fills before the portfolio stream changes; only
					// the lifecycle frontier has advanced when the BUY is accepted.
					generation++
				}
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			checked := s.checkBrokerPermissions(t.Context(), rpc.TradingStatus{Account: scope.Account, Mode: scope.Mode, CanPreview: true})
			got := s.brokerPermissions.state(scope, rpc.BrokerPermissionFutures)
			if phase == "stable flat" || phase == "stable short" {
				if !checked || got.State != rpc.BrokerPermissionEnabled || receipts != 1 || calls != 1 {
					t.Fatalf("stable opening check failed: checked=%v state=%s receipts=%d WhatIfs=%d", checked, got.State, receipts, calls)
				}
			} else if checked || got.State == rpc.BrokerPermissionEnabled {
				t.Fatalf("opening permission learned without coherent held quantity: checked=%v state=%s receipts=%d WhatIfs=%d", checked, got.State, receipts, calls)
			}
		})
	}
}

// Requested previews learn class permission too. Their signed position
// generation prevents a later send after drift, but must also prevent the
// changed opening direction from becoming durable readiness evidence.
func TestFuturesPermissionReviewRequestedPreviewDoesNotLearnAfterPositionChange(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	sent := newHedgeSendRig(t, s)
	position := s.orderRiskAuthorityForTest
	changed := false
	s.orderRiskAuthorityForTest = func(ctx context.Context, status rpc.TradingStatus, contract rpc.ContractParams, action string, quantity int) (orderPositionAuthority, error) {
		authority, err := position(ctx, status, contract, action, quantity)
		if changed {
			// An external buy leaves exactly the contracts this SELL would
			// close; accepted closing permission says nothing about opening.
			authority.Impact = rpc.OrderPositionImpact{Before: float64(quantity), After: 0, Effect: rpc.OrderPositionEffectClose}
			authority.Generation++
			authority.Health.ProjectionGeneration = authority.Generation
		}
		return authority, err
	}
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		changed = true
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
	if len(preview.Legs) == 1 && preview.Legs[0].PreviewToken != "" {
		result, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: preview.Legs[0].PreviewToken})
		if err == nil || len(sent.sends()) != 0 {
			t.Fatalf("changed position unexpectedly sent: accepted=%v err=%v sends=%d", result != nil && result.Accepted, err, len(sent.sends()))
		}
	}
	if got := s.brokerPermissions.state(s.currentBrokerStateScope(), rpc.BrokerPermissionFutures); got.State == rpc.BrokerPermissionEnabled {
		t.Fatalf("requested WhatIf learned opening permission from a close after position changed: %+v", got)
	}
}

func TestFuturesPermissionReviewOlderRequestedPreviewCannotEraseNewerDenial(t *testing.T) {
	s := hedgeOrderServer(t, 25, mandateReductionStock("SPY", 9101, 3000, 50))
	scope, started := s.currentBrokerStateScope(), s.nowUTC()
	newer := started.Add(time.Second)
	s.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		s.now = func() time.Time { return newer }
		s.noteBrokerPermissionPreview(t.Context(), scope, rpc.OrderDraft{Contract: testMESContract}, rpc.OrderPositionEffectOpen,
			rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "No trading permissions"}, s.nowUTC())
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
	if got := s.brokerPermissions.state(scope, rpc.BrokerPermissionFutures); got.State != rpc.BrokerPermissionNotEnabled || !got.CheckedAt.Equal(newer) || preview.Accepted {
		t.Fatalf("older requested preview replaced newer permission evidence: state=%+v previewAccepted=%v", got, preview.Accepted)
	}
}

// A daily denial can arrive after admission while the transport is waiting.
// It must stop an opening sale, while a deliberate buyback remains available.
func TestFuturesPermissionReviewDenialLearnedAtWire(t *testing.T) {
	for _, closing := range []bool{false, true} {
		name := "opening hedge"
		if closing {
			name = "closing hedge"
		}
		t.Run(name, func(t *testing.T) {
			rows := []rpc.PositionView{mandateReductionStock("SPY", 9101, 3000, 50)}
			if closing {
				rows = append(rows, mandateReductionFuture(hedgeTestConID, -2, 6000))
			}
			s := hedgeOrderServer(t, 25, rows...)
			sent := newHedgeSendRig(t, s)
			var token string
			if closing {
				preview, err := s.tradeProposalReducePreview(t.Context(), rpc.TradeProposalReduceParams{ConID: hedgeTestConID, Percent: 100, IncludeHedges: true})
				if err != nil || preview.PreviewToken == "" {
					t.Fatalf("close preview = %+v, %v", preview, err)
				}
				token = preview.PreviewToken
			} else {
				preview := hedgePlan(t, s, rpc.TradeProposalReducePortfolioParams{})
				if len(preview.Legs) != 1 || preview.Legs[0].PreviewToken == "" {
					t.Fatalf("opening preview = %+v", preview)
				}
				token = preview.Legs[0].PreviewToken
			}
			s.orderWriteBeforeBrokerSend = func() {
				s.noteBrokerPermissionPreview(t.Context(), s.currentBrokerStateScope(), rpc.OrderDraft{Contract: testMESContract}, rpc.OrderPositionEffectOpen,
					rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "No trading permissions"}, s.nowUTC())
			}
			result, err := s.placeOrder(t.Context(), rpc.OrderPlaceParams{PreviewToken: token})
			if closing {
				if err != nil || result == nil || !result.Accepted || len(sent.sends()) != 1 {
					t.Fatalf("opening-permission denial blocked a held-short buyback: result=%+v err=%v sends=%d", result, err, len(sent.sends()))
				}
			} else if err == nil || len(sent.sends()) != 0 || firstBlockerCodeOf(previewFailureBlockers(err)) != rpc.BrokerPermissionNotEnabledCode {
				t.Fatalf("known denial at wire did not refuse the opening hedge with its permission reason: accepted=%v err=%v sends=%d", result != nil && result.Accepted, err, len(sent.sends()))
			}
		})
	}
}
