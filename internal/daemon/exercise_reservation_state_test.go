package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestExerciseLegacyReservationAllowsUnrelatedCapacity(t *testing.T) {
	legacy := orderJournalEvent{ConID: 101, Symbol: " syn "}
	if exerciseReservationConflicts(legacy, rpc.ContractParams{ConID: 900, Symbol: "OTHER"}, rpc.OrderActionBuy) {
		t.Fatal("historical exercise freezes unrelated contract")
	}
	for _, draft := range []rpc.OrderDraft{{Contract: rpc.ContractParams{ConID: 101, Symbol: "SYN"}, Action: rpc.OrderActionSell}, {Contract: rpc.ContractParams{ConID: 102, Symbol: "SYN"}, Action: rpc.OrderActionBuy}, {Contract: rpc.ContractParams{ConID: 102, Symbol: "SYN"}, Action: rpc.OrderActionSell}} {
		if !exerciseReservationConflicts(legacy, draft.Contract, draft.Action) {
			t.Fatal("legacy affected capacity was released")
		}
	}
}

func TestExerciseReservationsSurviveReloadAndOnlyDefinitelyUnsentReleases(t *testing.T) {
	for _, disposition := range []ibkrlib.SendDisposition{ibkrlib.SendDispositionMayHaveWritten, ibkrlib.SendDispositionUnknown, ibkrlib.SendDispositionDefinitelyUnsent} {
		t.Run(string(disposition), func(t *testing.T) {
			s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
			event := orderJournalEvent{At: time.Now(), Type: orderJournalEventSendAttempted, Endpoint: "127.0.0.1:4002", ClientID: 31, Action: rpc.OpportunityActionExercise, Account: scope.Account, Mode: scope.Mode, PreviewTokenID: "token", OrderRef: "request", AttemptID: "attempt", ActionKind: corestore.ActionExercise}
			if err := s.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			// A new server facade reads only durable journal/core authority, no process cache.
			restarted := &Server{orderJournal: s.orderJournal, coreStore: s.coreStore}
			pending, err := restarted.pendingExercises(t.Context(), scope)
			if err != nil || len(pending) != 1 {
				t.Fatalf("restart pending=%v err=%v", pending, err)
			}
			event.Type = orderJournalEventSendCompleted
			if err := s.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			pending, err = restarted.pendingExercises(t.Context(), scope)
			if err != nil || len(pending) != 1 {
				t.Fatalf("flushed send lost durable reservation: %v %v", pending, err)
			}
			event.Type = orderJournalEventSendError
			event.SendDisposition = disposition
			if err := s.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			pending, err = restarted.pendingExercises(t.Context(), scope)
			want := 1
			if disposition == ibkrlib.SendDispositionDefinitelyUnsent {
				want = 0
			}
			if err != nil || len(pending) != want {
				t.Fatalf("pending=%d want=%d err=%v", len(pending), want, err)
			}
		})
	}
}

func TestExerciseMalformedResolutionCannotReleaseReservation(t *testing.T) {
	s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	raw := json.RawMessage(`{"version":1,"resolutions":{"token":{}}}`)
	if _, err := s.coreStore.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: exerciseResolutionKind, JSON: raw}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.exerciseResolutions(t.Context()); err == nil {
		t.Fatal("malformed resolution silently releases reservation")
	}
}

func TestExerciseReservationsRespectDirectionAndStrategyLegs(t *testing.T) {
	for _, side := range []string{rpc.OrderActionBuy, rpc.OrderActionSell} {
		t.Run(side, func(t *testing.T) {
			s := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
			event := orderJournalEvent{At: s.orderNow(), Type: orderJournalEventSendAttempted, ActionKind: corestore.ActionExercise, Action: rpc.OpportunityActionExercise, Account: "DU1234567", Mode: rpc.AccountModePaper, Endpoint: "127.0.0.1:4002", ClientID: 31, AttemptID: "direction", PreviewTokenID: "token", OrderRef: "request", ExerciseReservation: &exerciseReservation{Option: rpc.ContractParams{ConID: 101}, Underlying: rpc.ContractParams{ConID: 102}, UnderlyingAction: side}}
			if err := s.orderJournal.Append(event); err != nil {
				t.Fatal(err)
			}
			opposite := rpc.OrderActionBuy
			if side == rpc.OrderActionBuy {
				opposite = rpc.OrderActionSell
			}
			status := rpc.TradingStatus{Account: event.Account, Mode: event.Mode}
			bag := rpc.OrderDraft{Contract: rpc.ContractParams{SecType: "BAG"}, Action: opposite, StrategyGroup: &rpc.StrategyOrderDraft{Legs: []rpc.StrategyOrderLeg{{Contract: rpc.ContractParams{ConID: 102}, Action: side, Quantity: 100}}}}
			if err := s.pendingExerciseOrderBlocker(t.Context(), status, bag); err == nil {
				t.Fatal("BAG parent action hid conflicting leg")
			}
			bag.StrategyGroup.Legs[0].Contract.ConID = 103
			if err := s.pendingExerciseOrderBlocker(t.Context(), status, bag); err != nil {
				t.Fatalf("unrelated BAG leg blocked: %v", err)
			}
			for _, tc := range []struct {
				id      int
				action  string
				blocked bool
			}{{102, side, true}, {102, opposite, false}, {101, rpc.OrderActionSell, true}, {101, rpc.OrderActionBuy, false}, {103, side, false}} {
				err := s.pendingExerciseOrderBlocker(t.Context(), status, rpc.OrderDraft{Contract: rpc.ContractParams{ConID: tc.id}, Action: tc.action})
				if (err != nil) != tc.blocked {
					t.Fatalf("side=%s id=%d action=%s blocked=%v err=%v", side, tc.id, tc.action, tc.blocked, err)
				}
			}
		})
	}
}
