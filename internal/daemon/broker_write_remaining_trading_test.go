//go:build trading

package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestPaperSmokePreAckCleanupConnectorSwapAfterStageMakesZeroCancelCalls(t *testing.T) {
	t.Parallel()
	srv, _ := newPaperSmokeTestServer(t)
	first := &ibkrlib.Connector{}
	second := &ibkrlib.Connector{}
	const firstEpoch uint64 = 61
	srv.mu.Lock()
	srv.connector = first
	srv.connectorEpoch = firstEpoch
	srv.mu.Unlock()
	srv.orderWriteBindingForTest = func(status rpc.TradingStatus) (*ibkrlib.Connector, uint64, ibkrlib.ConnectorSessionBinding, brokerStateScope) {
		return first, firstEpoch, ibkrlib.ConnectorSessionBinding{}, brokerStateScope{Account: status.Account, Mode: status.Mode}
	}
	preSendCalls := 0
	srv.orderWriteBeforeBrokerSend = func() {
		preSendCalls++
		if preSendCalls != 2 {
			return
		}
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if srv.connector != first || srv.connectorEpoch != firstEpoch {
			t.Fatalf("pre-cancel connector = %p epoch=%d, want captured connector %p epoch=%d", srv.connector, srv.connectorEpoch, first, firstEpoch)
		}
		srv.connector = second
		srv.connectorEpoch++
	}
	placeCalls := 0
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		placeCalls++
		return nil // no acknowledgement, forcing the reserved-ID cleanup path
	}
	cancelCalls := 0
	srv.orderCancelBroker = func(context.Context, int) error {
		cancelCalls++
		return nil
	}

	res, err := srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: rpc.OrderOriginHumanTTY, TimeoutMs: 1})
	if err != nil {
		t.Fatalf("runPaperSmoke: %v", err)
	}
	if res.Passed || res.Result != tradingPaperSmokeResultFailed {
		t.Fatalf("smoke result = %+v, want failed evidence", res)
	}
	if preSendCalls != 2 || placeCalls != 1 || cancelCalls != 0 {
		t.Fatalf("pre_send=%d place=%d cancel=%d, want 2/1/0", preSendCalls, placeCalls, cancelCalls)
	}
	if !strings.Contains(res.Message, "connection authority changed") {
		t.Fatalf("smoke message = %q, want cleanup transaction-binding refusal", res.Message)
	}
	if !journalContainsEventType(t, srv, orderJournalEventCancelRequested) {
		t.Fatal("paper-smoke cleanup swap did not reach the durable cancel-requested stage")
	}
}

func TestOptionExerciseFailsClosedBeforeStageOrBrokerCall(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	brokerCalls := 0
	srv.optionExerciseBroker = func(context.Context, ibkrlib.OptionExerciseRequest) error {
		brokerCalls++
		return nil
	}
	err := srv.submitOptionExercise(context.Background(), testOptionExerciseOpportunity(), 1, rpc.OrderOriginHumanTTY, "exercise-blocked")
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "risk policy") || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("submitOptionExercise err = %v, want explicit fail-closed policy blocker", err)
	}
	if brokerCalls != 0 {
		t.Fatalf("broker_calls=%d, want 0", brokerCalls)
	}
	if journalContainsEventType(t, srv, orderJournalEventSendAttempted) {
		t.Fatal("fail-closed exercise reached the durable pre-transmit stage")
	}
}

func TestOrderPlaceAuthorityFailureAfterStageMakesZeroBrokerCalls(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	brokerCalls := 0
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		brokerCalls++
		return nil
	}
	installAuthorityFailureAfterStage(t, srv)
	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true})

	_, err := srv.placeOrder(context.Background(), rpc.OrderPlaceParams{PreviewToken: token})
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("placeOrder err = %v, want fresh authority-health refusal", err)
	}
	if brokerCalls != 0 {
		t.Fatalf("broker calls = %d, want zero after authority failure", brokerCalls)
	}
	if !journalContainsEventType(t, srv, orderJournalEventSendAttempted) {
		t.Fatal("authority-failure place did not reach the durable pre-transmit stage")
	}
}

func TestOrderCancelAuthorityFailureAfterStageMakesZeroBrokerCalls(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	if err := srv.orderJournal.Append(orderJournalEvent{
		At: srv.orderNow().Add(-time.Minute), Type: orderJournalEventBrokerAcknowledged,
		OrderRef: "authority-cancel", ReservedOrderID: 1001, ClientID: 31,
		Account: "DU1234567", Endpoint: "127.0.0.1:4002", Mode: rpc.AccountModePaper,
		Symbol: "AAPL", SecType: "STK", Action: rpc.OrderActionBuy,
		OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Quantity: 1,
		LimitPrice: 100, Status: "Submitted", SendState: orderSendStateBrokerAcknowledged,
	}); err != nil {
		t.Fatalf("seed cancel row: %v", err)
	}
	brokerCalls := 0
	srv.orderCancelBroker = func(context.Context, int) error {
		brokerCalls++
		return nil
	}
	installAuthorityFailureAfterStage(t, srv)

	_, err := srv.cancelOrder(context.Background(), rpc.OrderCancelParams{ID: "authority-cancel", Origin: rpc.OrderOriginHumanTTY})
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("cancelOrder err = %v, want storage-health refusal (freeze-only cancel exception must not strip storage)", err)
	}
	if brokerCalls != 0 {
		t.Fatalf("cancel broker calls = %d, want zero after authority failure", brokerCalls)
	}
	if !journalContainsEventType(t, srv, orderJournalEventCancelRequested) {
		t.Fatal("authority-failure cancel did not reach the durable cancel-requested stage")
	}
}

func installAuthorityFailureAfterStage(t *testing.T, srv *Server) {
	t.Helper()
	authority, err := corestore.Open(t.Context(), corestore.Options{
		Path: filepath.Join(privateTestDir(t), "failing-daemon.db"),
		CommitObserver: func(corestore.AuthorityHead) error {
			return errors.New("injected authority watermark failure")
		},
	})
	if err != nil {
		t.Fatalf("open injected authority: %v", err)
	}
	if health := authority.Health(); !health.Ready {
		t.Fatalf("injected authority precondition is unhealthy: %+v", health)
	}
	t.Cleanup(func() { _ = authority.Close() })
	srv.coreStore = authority
	srv.orderWriteBeforeBrokerSend = func() {
		_, mutateErr := authority.CompareAndSwapStateDocument(context.Background(), corestore.StateDocumentCAS{
			ScopeKey: "test", Kind: "post-stage-authority-failure", JSON: []byte(`{"version":1}`),
		})
		if mutateErr == nil {
			t.Fatal("injected authority mutation unexpectedly succeeded")
		}
		if health := authority.Health(); health.Ready {
			t.Fatalf("authority remained ready after injected failure: %+v", health)
		}
	}
}

func testOptionExerciseOpportunity() rpc.Opportunity {
	return rpc.Opportunity{
		Symbol:         "SPY",
		SecType:        "OPT",
		Action:         rpc.OrderActionBuy,
		ExerciseAction: rpc.ExerciseActionExercise,
		Quantity:       1,
		MaxQuantity:    1,
		Contract: rpc.ContractParams{
			ConID:        756733611,
			Symbol:       "SPY",
			SecType:      "OPT",
			Exchange:     "SMART",
			Currency:     "USD",
			LocalSymbol:  "SPY   260821C00600000",
			TradingClass: "SPY",
			Expiry:       "20260821",
			Strike:       600,
			Right:        "C",
			Multiplier:   100,
		},
	}
}
