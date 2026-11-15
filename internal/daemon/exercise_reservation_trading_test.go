//go:build trading

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestAuditExerciseReservesUnderlyingAcrossDistinctPuts(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.orderNow()}, brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}, nil
	}
	opp := testOptionExerciseOpportunity()
	opp.Contract.Right = "P"
	opp.Contract.LocalSymbol = "SYN PUT A"
	opp.UnderlyingQuantityBefore, opp.UnderlyingQuantityAfter = 100, 0
	first := testOptionExercisePayload(t, srv, opp, "audit-exercise-first")
	calls := 0
	srv.optionExerciseBroker = func(context.Context, ibkrlib.OptionExerciseRequest) error { calls++; return nil }
	if err := srv.submitOptionExercise(t.Context(), first, opp, 1, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatalf("first exercise: %v", err)
	}
	other := opp
	other.Contract.ConID++
	other.Contract.Strike++
	other.Contract.LocalSymbol = "SYN PUT B"
	second := testOptionExercisePayload(t, srv, other, "audit-exercise-second")
	err := srv.submitOptionExercise(t.Context(), second, other, 1, rpc.OrderOriginHumanTTY)
	if err == nil || !strings.Contains(err.Error(), "unresolved") || calls != 1 {
		t.Fatalf("pending exercise spent same 100 shares twice: error=%v broker_calls=%d; require refusal and one call", err, calls)
	}
}
func TestReviewPendingExerciseReservesUnderlyingAgainstOrdinarySale(t *testing.T) {
	r := newCloseLinkRig(t, false, "")
	r.stopGone(t)
	r.position = 100
	token := r.previewSell(t, 100)
	opp := testOptionExerciseOpportunity()
	opp.Contract.Right = "P"
	opp.Contract.Symbol = "SYW"
	opp.Contract.Expiry = "20261218"
	opp.Contract.LocalSymbol = ""
	opp.Contract.TradingClass = "SYW"
	opp.UnderlyingContract = closeLinkContract(false)
	opp.UnderlyingQuantityBefore = 100
	opp.UnderlyingQuantityAfter = 0
	opp.PortfolioGeneration = 1
	payload := testOptionExercisePayload(t, r.srv, opp, "synthetic-pending-exercise")
	calls := 0
	r.srv.optionExerciseBroker = func(context.Context, ibkrlib.OptionExerciseRequest) error { calls++; return nil }
	if err := r.srv.submitOptionExercise(t.Context(), payload, opp, 1, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatalf("exercise setup refused: %v", err)
	}
	result, err := r.place(t, token)
	t.Logf("exercise_frames=%d ordinary_sale_accepted=%v error=%v sale_frames=%d", calls, result != nil && result.Accepted, err, len(r.placed))
	if err == nil || !strings.Contains(err.Error(), "unresolved") || len(r.placed) > 0 {
		t.Fatal("ordinary sale consumed shares already committed to pending put exercise")
	}
}

func installExerciseSnapshotFixture(s *Server, opp rpc.Opportunity) {
	if s.openOrderInventoryForTest == nil {
		s.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
			return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: s.orderNow()}, brokerStateScope{Account: opp.PortfolioAccount, Mode: rpc.AccountModePaper}, nil
		}
	}
	if s.reduceFillSnapshotForTest == nil {
		s.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) {
			return []*ibkrlib.RawPosition{{Account: opp.PortfolioAccount, Contract: *previewIBKRContract(opp.Contract), Position: opp.PositionQuantity}, {Account: opp.PortfolioAccount, Contract: *previewIBKRContract(opp.UnderlyingContract), Position: opp.UnderlyingQuantityBefore}}, nil
		}
	}
}

func TestReviewExerciseCannotIgnoreUnacknowledgedBuy(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.orderNow()}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, nil
	}
	opp := testOptionExerciseOpportunity()
	if err := srv.orderJournal.Append(orderJournalEvent{At: srv.orderNow().Add(-time.Hour), Type: orderJournalEventSendAttempted, OrderRef: "pending-buy", PreviewTokenID: "pending-buy-token", ReservedOrderID: 1002, ClientID: 31, Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", Symbol: opp.UnderlyingContract.Symbol, SecType: "STK", ConID: opp.UnderlyingContract.ConID, Currency: "USD", Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT, Quantity: 100, SendState: orderSendStateSendAttempted}); err != nil {
		t.Fatal(err)
	}
	payload := testOptionExercisePayload(t, srv, opp, "exercise-versus-pending-buy")
	calls := 0
	srv.optionExerciseBroker = func(context.Context, ibkrlib.OptionExerciseRequest) error { calls++; return nil }
	err := srv.submitOptionExercise(t.Context(), payload, opp, 1, rpc.OrderOriginHumanTTY)
	t.Logf("exercise_error=%v exercise_frames=%d", err, calls)
	if calls > 0 {
		t.Fatal("call exercise adds100 shares while pending BUY100 has already reserved closing the same short100")
	}
}

func TestReviewExerciseCannotBypassWorkingProtectiveStop(t *testing.T) {
	for _, group := range []string{"", "synthetic-oca"} {
		t.Run(group, func(t *testing.T) {
			r := newCloseLinkRig(t, false, group)
			r.position = 100
			opp := testOptionExerciseOpportunity()
			opp.Contract.Right = "P"
			opp.Contract.Symbol = "SYW"
			opp.Contract.Expiry = "20261218"
			opp.Contract.LocalSymbol = ""
			opp.Contract.TradingClass = "SYW"
			opp.UnderlyingContract = closeLinkContract(false)
			opp.UnderlyingQuantityBefore = 100
			opp.UnderlyingQuantityAfter = 0
			opp.PortfolioGeneration = 1
			payload := testOptionExercisePayload(t, r.srv, opp, "synthetic-exercise-versus-stop")
			calls := 0
			r.srv.optionExerciseBroker = func(context.Context, ibkrlib.OptionExerciseRequest) error { calls++; return nil }
			err := r.srv.submitOptionExercise(t.Context(), payload, opp, 1, rpc.OrderOriginHumanTTY)
			t.Logf("exercise_error=%v exercise_frames=%d still_working_stop=%g oca=%q", err, calls, r.stopRemaining, group)
			if calls > 0 {
				t.Fatal("exercise delivers all100 shares while independent SELL70 protective stop remains active")
			}
		})
	}
}

func TestReviewExerciseCannotIgnoreFilledBuyBeforePortfolioUpdate(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.orderNow()}, brokerStateScope{Account: "DU1234567", Mode: "paper"}, nil
	}
	opp := testOptionExerciseOpportunity()
	if err := srv.orderJournal.Append(orderJournalEvent{At: srv.orderNow().Add(-time.Second), Type: orderJournalEventStatusUpdated, ReservedOrderID: 1002, ClientID: 31, Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: "paper", Symbol: opp.UnderlyingContract.Symbol, SecType: "STK", ConID: opp.UnderlyingContract.ConID, Currency: "USD", Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT, Quantity: 100, Filled: 100, Remaining: 0, Status: "Filled", SendState: orderSendStateBrokerAcknowledged}); err != nil {
		t.Fatal(err)
	}
	srv.reduceFillSnapshotForTest = func(context.Context, time.Duration) ([]*ibkrlib.RawPosition, error) {
		return []*ibkrlib.RawPosition{{Account: opp.PortfolioAccount, Contract: *previewIBKRContract(opp.Contract), Position: 1}}, nil
	}
	payload := testOptionExercisePayload(t, srv, opp, "exercise-versus-pending-buy")
	calls := 0
	srv.optionExerciseBroker = func(context.Context, ibkrlib.OptionExerciseRequest) error { calls++; return nil }
	err := srv.submitOptionExercise(t.Context(), payload, opp, 1, rpc.OrderOriginHumanTTY)
	t.Logf("exercise_error=%v exercise_frames=%d", err, calls)
	if calls > 0 {
		t.Fatal("call exercise adds100 shares while pending BUY100 has already reserved closing the same short100")
	}
}

func TestExerciseLifecycleChangeBeforeFirstByteRefusesAndReleasesUnsent(t *testing.T) {
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	opp := testOptionExerciseOpportunity()
	payload := testOptionExercisePayload(t, s, opp, "exercise-lifecycle-race")
	var generation uint64 = 1
	s.reduceFillGenerationForTest = func() uint64 { return generation }
	s.orderWriteBeforeBrokerSend = func() { generation++ }
	calls := 0
	s.optionExerciseBroker = func(context.Context, ibkrlib.OptionExerciseRequest) error { calls++; return nil }
	err := s.submitOptionExercise(t.Context(), payload, opp, 1, rpc.OrderOriginHumanTTY)
	if err == nil || !strings.Contains(err.Error(), "before reduction transmission") || calls != 0 {
		t.Fatalf("lifecycle race err=%v frames=%d", err, calls)
	}
	pending, err := s.pendingExercises(t.Context(), s.currentBrokerStateScope())
	if err != nil || len(pending) != 0 {
		t.Fatalf("definitely-unsent persisted reservation=%v err=%v", pending, err)
	}
}
