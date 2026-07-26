//go:build trading

package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// newPaperSmokeTestServer wires the preview test harness with the readiness
// store + signer the smoke needs and an accepted-WhatIf preview path. Broker
// hooks are left to each test.
func newPaperSmokeTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	path := filepath.Join(t.TempDir(), "trading-readiness.json")
	srv.tradingReadiness = newTradingReadinessStore(path, srv.orderTokens)
	authority, err := srv.orderJournal.coreStore()
	if err != nil {
		t.Fatalf("order authority: %v", err)
	}
	if err := srv.tradingReadiness.UseCoreStore(t.Context(), authority); err != nil {
		t.Fatalf("attach readiness authority: %v", err)
	}
	srv.version = "test-version"
	srv.paperSmokeCancelBudgetOverride = 300 * time.Millisecond
	srv.orderPreviewQuote = fixedPreviewQuote(600.10, 600.20)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(0, 1, rpc.OrderPositionEffectOpen)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	return srv, path
}

// ackOnPlace returns an orderPlaceBroker hook that journals the broker
// acknowledgement synchronously (the smoke polls before its first sleep) and
// captures the order ref for later hooks.
func ackOnPlace(srv *Server, brokerStatus string, orderRef *string) func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
	return func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		*orderRef = order.OrderRef
		return srv.orderJournal.Append(paperSmokeTestEvent(srv, orderJournalEventBrokerAcknowledged, order.OrderRef, order.OrderID, brokerStatus, orderSendStateBrokerAcknowledged))
	}
}

func paperSmokeTestEvent(srv *Server, eventType, orderRef string, orderID int, brokerStatus, sendState string) orderJournalEvent {
	return orderJournalEvent{
		At:              srv.orderNow(),
		Type:            eventType,
		OrderRef:        orderRef,
		ReservedOrderID: orderID,
		ClientID:        31,
		Account:         "DU1234567",
		Endpoint:        "127.0.0.1:4002",
		Mode:            "paper",
		Symbol:          "SPY",
		SecType:         "STK",
		Action:          "BUY",
		OrderType:       rpc.OrderTypeLMT,
		TIF:             rpc.OrderTIFDay,
		Quantity:        1,
		Status:          brokerStatus,
		SendState:       sendState,
	}
}

// TestPaperSmokeAcceptsAnyOrigin pins the 2026-06-10 re-gating: the smoke
// is a release-pipeline quality gate (run automatically at version bump),
// not a runtime live precondition, so automated origins must be able to
// run it and produce evidence.
func TestPaperSmokeAcceptsAnyOrigin(t *testing.T) {
	t.Parallel()
	for _, origin := range []string{"", rpc.OrderOriginAgent, "mystery-origin"} {
		srv, _ := newPaperSmokeTestServer(t)
		res, err := srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: origin})
		if err != nil && strings.Contains(err.Error(), "origin") {
			t.Fatalf("origin %q: err = %v, want no origin refusal", origin, err)
		}
		// The bare fixture has no broker, so the run proceeds past the
		// (removed) origin gate and fails at transmit — which still
		// records evidence. The contract under test is only that no
		// origin is refused pre-flight.
		if res == nil {
			t.Fatalf("origin %q: res = nil err = %v, want an attempted run", origin, err)
		}
		if evidence, loadErr := srv.tradingReadiness.Load(); loadErr != nil || evidence.PaperSmoke == nil {
			t.Fatalf("origin %q: readiness evidence missing after attempted run: %+v %v", origin, evidence, loadErr)
		}
	}
}

func TestPaperSmokeRefusesLiveRoute(t *testing.T) {
	t.Parallel()
	srv, _ := newPaperSmokeTestServer(t)
	// A fully-ready live gate: acks set, port/account live, and valid prior
	// paper-smoke evidence so no blocker fires before the mode backstop.
	if err := srv.tradingReadiness.SavePaperSmoke(tradingPaperSmokeEvidence{
		Account:       "DU1234567",
		Endpoint:      "127.0.0.1:4002",
		EndpointClass: tradingPaperSmokeEndpointClassPaper,
		ClientID:      31,
		Version:       "test-version",
		Result:        tradingPaperSmokeResultPassed,
		At:            srv.orderNow().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	seeded, err := srv.tradingReadiness.Load()
	if err != nil {
		t.Fatalf("read seeded evidence: %v", err)
	}
	srv.cfg.Gateway.Port = new(4001)
	srv.cfg.Gateway.Account = "U1234567"
	srv.cfg.Trading = config.Trading{Mode: config.TradingModeLive}.WithDefaults()
	srv.endpoint.Port = 4001
	srv.endpoint.Account = "U1234567"

	_, err = srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: rpc.OrderOriginHumanTTY})
	if err == nil || !strings.Contains(err.Error(), "paper route") {
		t.Fatalf("err = %v, want paper-route refusal", err)
	}
	after, err := srv.tradingReadiness.Load()
	if err != nil || after.PaperSmokeMAC != seeded.PaperSmokeMAC || after.PaperSmoke == nil || seeded.PaperSmoke == nil || !after.PaperSmoke.At.Equal(seeded.PaperSmoke.At) {
		t.Fatalf("live-route refusal must leave evidence untouched (err=%v)", err)
	}
}

func TestPaperSmokeRefusesConcurrentRun(t *testing.T) {
	t.Parallel()
	srv, _ := newPaperSmokeTestServer(t)
	srv.paperSmokeMu.Lock()
	defer srv.paperSmokeMu.Unlock()
	_, err := srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: rpc.OrderOriginHumanTTY})
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("err = %v, want already-in-progress refusal", err)
	}
}

func TestPaperSmokePassesAndWritesSignedEvidence(t *testing.T) {
	t.Parallel()
	srv, _ := newPaperSmokeTestServer(t)
	var orderRef string
	srv.orderPlaceBroker = ackOnPlace(srv, "Submitted", &orderRef)
	var cancelledID int
	srv.orderCancelBroker = func(_ context.Context, orderID int) error {
		cancelledID = orderID
		return srv.orderJournal.Append(paperSmokeTestEvent(srv, orderJournalEventStatusUpdated, orderRef, orderID, "Cancelled", orderSendStateTerminal))
	}

	res, err := srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: rpc.OrderOriginHumanTTY, TimeoutMs: 500})
	if err != nil {
		t.Fatalf("runPaperSmoke: %v", err)
	}
	if !res.Passed || res.Result != tradingPaperSmokeResultPassed || !res.EvidenceSaved {
		t.Fatalf("result = %+v, want passed with saved evidence", res)
	}
	if cancelledID != 1001 || res.ReservedOrderID != 1001 || res.OrderRef != orderRef {
		t.Fatalf("cancelledID=%d res=%+v", cancelledID, res)
	}
	if res.CancelLifecycleStatus != rpc.OrderLifecycleCancelled {
		t.Fatalf("cancel lifecycle = %q, want cancelled", res.CancelLifecycleStatus)
	}
	// The 2% off-market buy must round onto the cent grid below the bid.
	if res.LimitPrice <= 0 || res.LimitPrice >= 600.10 {
		t.Fatalf("limit price = %v, want positive and below bid", res.LimitPrice)
	}

	// Evidence must be MAC'd and satisfy the live gate for the paired live
	// account family on the same host/client/version.
	readiness, err := srv.tradingReadiness.Load()
	if err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	if readiness.PaperSmoke == nil || readiness.PaperSmokeMAC == "" {
		t.Fatalf("readiness authority missing signed smoke evidence: %+v", readiness)
	}
	check := srv.tradingReadiness.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, srv.orderNow())
	if check.Status != tradingPaperSmokeStatusValid {
		t.Fatalf("CheckPaperSmoke = %+v, want valid", check)
	}
}

func TestPaperSmokeAckTimeoutFailsClosedAndStillCancels(t *testing.T) {
	t.Parallel()
	srv, _ := newPaperSmokeTestServer(t)
	var placedRef string
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		placedRef = order.OrderRef // broker stays silent: no ack event
		return nil
	}
	var cancelledID int
	srv.orderCancelBroker = func(_ context.Context, orderID int) error {
		cancelledID = orderID
		return srv.orderJournal.Append(paperSmokeTestEvent(srv, orderJournalEventStatusUpdated, placedRef, orderID, "Cancelled", orderSendStateTerminal))
	}

	res, err := srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: rpc.OrderOriginHumanTTY, TimeoutMs: 300})
	if err != nil {
		t.Fatalf("runPaperSmoke: %v", err)
	}
	if res.Passed || res.Result != tradingPaperSmokeResultFailed || !res.EvidenceSaved {
		t.Fatalf("result = %+v, want failed with saved evidence", res)
	}
	if cancelledID != 1001 {
		t.Fatalf("cleanup cancel must still fire on ack timeout, got order %d", cancelledID)
	}
	events, loadErr := srv.orderJournal.LoadEvents(0)
	if loadErr != nil {
		t.Fatalf("load paper-smoke order events: %v", loadErr)
	}
	var cleanupAttempt, cleanupOutcome orderJournalEvent
	for _, ev := range events {
		if ev.ActionKind != corestore.ActionSmokeCleanup {
			continue
		}
		switch ev.Type {
		case orderJournalEventCancelRequested:
			cleanupAttempt = ev
		case orderJournalEventSendCompleted:
			cleanupOutcome = ev
		}
	}
	if cleanupAttempt.AttemptID == "" || cleanupOutcome.AttemptID != cleanupAttempt.AttemptID {
		t.Fatalf("paper-smoke cleanup outcome is not correlated: attempt=%+v outcome=%+v", cleanupAttempt, cleanupOutcome)
	}
	check := srv.tradingReadiness.CheckPaperSmoke("U1234567", "127.0.0.1:4001", 31, "test-version", 168*time.Hour, srv.orderNow())
	if check.Status != tradingPaperSmokeStatusFailed {
		t.Fatalf("CheckPaperSmoke = %+v, want failed (live stays blocked)", check)
	}
}

func TestPaperSmokeCancelConfirmTimeoutFails(t *testing.T) {
	t.Parallel()
	srv, _ := newPaperSmokeTestServer(t)
	var orderRef string
	srv.orderPlaceBroker = ackOnPlace(srv, "Submitted", &orderRef)
	srv.orderCancelBroker = func(context.Context, int) error {
		return nil // cancel sent, broker never confirms
	}

	res, err := srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: rpc.OrderOriginHumanTTY, TimeoutMs: 500})
	if err != nil {
		t.Fatalf("runPaperSmoke: %v", err)
	}
	if res.Passed || res.Result != tradingPaperSmokeResultFailed {
		t.Fatalf("result = %+v, want failed on unconfirmed cancel", res)
	}
	if !strings.Contains(res.Message, "did not confirm the cancel") {
		t.Fatalf("message = %q", res.Message)
	}
}

func TestPaperSmokeFillFailsWithCleanupGuidance(t *testing.T) {
	t.Parallel()
	srv, _ := newPaperSmokeTestServer(t)
	var orderRef string
	srv.orderPlaceBroker = ackOnPlace(srv, "Filled", &orderRef)
	srv.orderCancelBroker = func(context.Context, int) error { return nil }

	res, err := srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: rpc.OrderOriginHumanTTY, TimeoutMs: 500})
	if err != nil {
		t.Fatalf("runPaperSmoke: %v", err)
	}
	if res.Passed || !strings.Contains(res.Message, "filled unexpectedly") {
		t.Fatalf("result = %+v, want fill failure with cleanup guidance", res)
	}
}

func TestPaperSmokePreTransmitFailureLeavesEvidenceUntouched(t *testing.T) {
	t.Parallel()
	srv, _ := newPaperSmokeTestServer(t)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, Message: "rejected by broker"}, nil
	}
	_, err := srv.runPaperSmoke(context.Background(), rpc.TradingPaperSmokeParams{Origin: rpc.OrderOriginHumanTTY})
	if err == nil || !strings.Contains(err.Error(), "not submit-eligible") {
		t.Fatalf("err = %v, want submit-eligibility refusal", err)
	}
	readiness, loadErr := srv.tradingReadiness.Load()
	if loadErr != nil || readiness.PaperSmoke != nil {
		t.Fatalf("pre-transmit failure must leave empty readiness authority: %+v err=%v", readiness, loadErr)
	}
}
