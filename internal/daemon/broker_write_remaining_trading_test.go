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

func TestOptionExercisePreviewMintsExactOneShotAuthority(t *testing.T) {
	t.Parallel()
	now := opportunityTestRTH()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	e := &opportunityEngine{server: srv, now: func() time.Time { return now }}
	opp := testOptionExerciseOpportunity()
	opp.Key, opp.Revision = "option_exercise:test", "sha256:test"
	opp.UnderlyingQuantityBefore, opp.UnderlyingQuantityAfter = -100, 0

	res := e.previewRevalidatedOpportunity(rpc.OpportunityExercisePreviewParams{
		Key: opp.Key, Revision: opp.Revision, Quantity: 1, Origin: rpc.OrderOriginHumanTTY,
	}, opp, nil, now)
	if !res.Accepted || !res.SubmitEligible || !res.TokenMinted || res.PreviewToken == "" || res.PreviewTokenID == "" || res.PreviewTokenExpiresAt.IsZero() {
		t.Fatalf("preview did not mint exercise authority: %+v", res)
	}
	payload, err := srv.orderTokens.verify(res.PreviewToken)
	if err != nil {
		t.Fatalf("verify exercise token: %v", err)
	}
	if payload.Scope != rpc.OrderTokenScopeExercise || payload.ExerciseKey != opp.Key || payload.ExerciseRevision != opp.Revision || payload.Draft.Quantity != 1 || payload.Position.Effect != rpc.ExercisePositionEffectClose {
		t.Fatalf("exercise token payload = %+v", payload)
	}
}

func TestOptionExerciseAuthorityFailureAfterStageMakesZeroBrokerCalls(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	brokerCalls := 0
	srv.optionExerciseBroker = func(context.Context, ibkrlib.OptionExerciseRequest) error {
		brokerCalls++
		return nil
	}
	installAuthorityFailureAfterStage(t, srv)
	opp := testOptionExerciseOpportunity()
	payload := testOptionExercisePayload(t, srv, opp, "exercise-authority-failure")
	err := srv.submitOptionExercise(context.Background(), payload, opp, 1, rpc.OrderOriginHumanTTY)
	if !errors.Is(err, ErrTradingDisabled) || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("submitOptionExercise err = %v, want fresh authority-health refusal", err)
	}
	if brokerCalls != 0 {
		t.Fatalf("broker_calls=%d, want 0", brokerCalls)
	}
	if !journalContainsEventType(t, srv, orderJournalEventSendAttempted) {
		t.Fatal("authority-failure exercise did not reach the durable pre-transmit stage")
	}
}

func TestOptionExerciseConsumesOneShotTokenAndSendsExactInstruction(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	opp := testOptionExerciseOpportunity()
	payload := testOptionExercisePayload(t, srv, opp, "exercise-one-shot")
	var requests []ibkrlib.OptionExerciseRequest
	srv.optionExerciseBroker = func(_ context.Context, req ibkrlib.OptionExerciseRequest) error {
		requests = append(requests, req)
		return nil
	}
	if err := srv.submitOptionExercise(context.Background(), payload, opp, 1, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatalf("first submitOptionExercise: %v", err)
	}
	if err := srv.submitOptionExercise(context.Background(), payload, opp, 1, rpc.OrderOriginHumanTTY); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("second submitOptionExercise err = %v, want consumed-token refusal", err)
	}
	if len(requests) != 1 || requests[0].Contract.ConID != opp.Contract.ConID || requests[0].ExerciseQuantity != 1 || requests[0].Account != "DU1234567" {
		t.Fatalf("broker requests = %+v, want one exact exercise", requests)
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
		Symbol:              "SPY",
		SecType:             "OPT",
		Action:              rpc.OrderActionBuy,
		ExerciseAction:      rpc.ExerciseActionExercise,
		Quantity:            1,
		MaxQuantity:         1,
		PositionEffect:      rpc.ExercisePositionEffectClose,
		PortfolioGeneration: 7,
		PortfolioAccount:    "DU1234567",
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
		UnderlyingContract:       rpc.ContractParams{ConID: 756733, Symbol: "SPY", SecType: "STK", Exchange: "SMART", Currency: "USD", Multiplier: 1},
		UnderlyingQuantityBefore: -100,
		UnderlyingQuantityAfter:  0,
	}
}

func testOptionExercisePayload(t *testing.T, srv *Server, opp rpc.Opportunity, tokenID string) orderPreviewTokenPayload {
	t.Helper()
	head, err := srv.orderJournal.AuthorityHead()
	if err != nil {
		t.Fatalf("authority head: %v", err)
	}
	return orderPreviewTokenPayload{
		AuthorityEpoch: head.AuthorityEpoch, SignerGeneration: head.SignerGeneration,
		TokenID: tokenID, Scope: rpc.OrderTokenScopeExercise,
		Mode: rpc.AccountModePaper, Account: "DU1234567", Endpoint: "127.0.0.1:4002", ClientID: 31,
		Draft:               exerciseOrderDraft(opp, 1, srv.orderNow()),
		Position:            rpc.OrderPositionImpact{Before: opp.UnderlyingQuantityBefore, After: opp.UnderlyingQuantityAfter, Effect: opp.PositionEffect},
		PortfolioGeneration: opp.PortfolioGeneration, PortfolioAccount: opp.PortfolioAccount,
	}
}
