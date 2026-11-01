package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestArmedAutomaticLockIsAcceptedByTheAuthoritativeStore(t *testing.T) {
	s, _, _ := watchlistServer(t)
	_, err := s.handleHoldingLocksReplace(t.Context(), &rpc.Request{Params: []byte(`{"automatic":{"action":"lock","lock":{"scope":"position","symbol":"SYNA","con_id":9001,"account_id":"synthetic","account_mode":"paper"}},"expected_revision":0,"request_id":"synthetic-auto-lock"}`)})
	if err != nil {
		t.Fatalf("automatic exclusion cannot be enforced by Canary: %v", err)
	}
}

func TestArmedAutomaticLockAcknowledgementNeedsNoBrokerObservation(t *testing.T) {
	s, _, _ := watchlistServer(t)
	kept, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{Symbol: "SYNB", Role: rpc.HoldingLockRoleCore}}, 0, "synthetic-keep-before-auto")
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	s.reducePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		reads++
		return nil, fmt.Errorf("synthetic broker not responding")
	}
	lock := rpc.AutomaticLock{Scope: "position", Symbol: "SYNA", ConID: 9001, AccountID: "synthetic", AccountMode: "paper"}
	update := rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock}
	accepted := automaticLockUpdate(t, s, kept.Revision, "broker-independent-lock", update)
	if reads != 0 {
		t.Fatalf("automatic exclusion acknowledgement waited for %d unrelated broker reads", reads)
	}
	replayed := automaticLockUpdate(t, s, accepted.Revision, "broker-independent-lock", update)
	if reads != 0 || !replayed.Replay || replayed.Revision != accepted.Revision || replayed.SavedRevision != accepted.Revision || replayed.RequestID != "broker-independent-lock" {
		t.Fatalf("automatic exclusion replay changed its receipt or read the broker: reads=%d receipt=%+v", reads, replayed)
	}
	if len(replayed.Locks) != 1 || replayed.Locks[0].Symbol != "SYNB" || replayed.Locks[0].Held != nil || !replayed.AutomaticMatch("synthetic", "paper", "SYNA", 9001) {
		t.Fatalf("automatic acknowledgement lost accepted terms: %+v", replayed)
	}
	if _, err := replaceHoldingLocks(t, s, kept.Locks, replayed.Revision, "synthetic-ordinary-keep-ack"); err != nil || reads != 1 {
		t.Fatalf("ordinary keep-list acknowledgement changed: reads=%d err=%v", reads, err)
	}
}

func automaticLocksListResponse(t *testing.T, s *Server, params string) rpc.Response {
	t.Helper()
	var output bytes.Buffer
	s.dispatch(t.Context(), &rpc.Request{ID: "automatic-lock-read", Method: rpc.MethodHoldingLocksList, Params: json.RawMessage(params)}, json.NewEncoder(&output), bufio.NewReader(strings.NewReader("")))
	var response rpc.Response
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("decode automatic locks response: %v", err)
	}
	return response
}

func TestArmedAutomaticLockListNeedsNoBrokerObservation(t *testing.T) {
	s, _, _ := watchlistServer(t)
	response := automaticLocksListResponse(t, s, `{"automatic_only":true}`)
	var out rpc.HoldingLocks
	if !response.Ok || json.Unmarshal(response.Result, &out) != nil || out.Version != 1 || out.Revision != 0 || out.AutomaticLocksInitialized || len(out.AutomaticLocks) != 0 || out.AsOf.IsZero() {
		t.Fatalf("pristine automatic authority: response=%+v result=%+v", response, out)
	}
	kept, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{Symbol: "SYNB", Role: rpc.HoldingLockRoleCore}}, 0, "synthetic-kept-for-read")
	if err != nil {
		t.Fatal(err)
	}
	lock := rpc.AutomaticLock{Scope: "position", Symbol: "SYNA", ConID: 9001, AccountID: "synthetic", AccountMode: "paper"}
	accepted := automaticLockUpdate(t, s, kept.Revision, "synthetic-lock-for-read", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
	reads := 0
	s.reducePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		reads++
		return &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "SYNB", ConID: 9002}}}, nil
	}
	response = automaticLocksListResponse(t, s, `{"automatic_only":true}`)
	if !response.Ok || json.Unmarshal(response.Result, &out) != nil {
		t.Fatalf("automatic list: %+v", response)
	}
	if reads != 0 || out.Version != 1 || out.Revision != accepted.Revision || !out.AutomaticLocksInitialized || !out.AutomaticMatch("synthetic", "paper", "SYNA", 9001) || len(out.Locks) != 0 || out.AsOf.IsZero() {
		t.Fatalf("automatic read requires the book or lost accepted authority: reads=%d result=%+v", reads, out)
	}
	// The ordinary read still resolves the keep list against the current book.
	for _, params := range []string{"", "null", "{}", `{"automatic_only":false}`} {
		reads = 0
		response := automaticLocksListResponse(t, s, params)
		if !response.Ok || json.Unmarshal(response.Result, &out) != nil || reads != 1 || len(out.Locks) != 1 || out.Locks[0].Held == nil || !*out.Locks[0].Held || len(out.Locks[0].Contracts) != 1 || out.Locks[0].Contracts[0] != 9002 {
			t.Fatalf("ordinary list changed for %q: reads=%d response=%+v result=%+v", params, reads, response, out)
		}
	}
}

func TestArmedAutomaticLockListRejectsInvalidOptionsBeforeReadingBroker(t *testing.T) {
	s, _, _ := watchlistServer(t)
	if _, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{Symbol: "SYNB", Role: rpc.HoldingLockRoleCore}}, 0, "synthetic-kept-invalid-read"); err != nil {
		t.Fatal(err)
	}
	reads := 0
	s.reducePositionsForTest = func(context.Context) (*rpc.PositionsResult, error) {
		reads++
		return nil, fmt.Errorf("synthetic broker unavailable")
	}
	for _, params := range []string{`{"automatic_only":"true"}`, `{"automatic_only":1}`, `{"automatic_only":true,"unknown":true}`, `{"automatic_only":true} {}`, `[]`, `"automatic"`} {
		response := automaticLocksListResponse(t, s, params)
		if response.Ok || response.Error == nil || response.Error.Code != rpc.CodeBadRequest || reads != 0 {
			t.Fatalf("invalid read %q reached authority or broker: reads=%d response=%+v", params, reads, response)
		}
	}
}

func TestArmedAutomaticLockValidationIsIndependentOfKeepList(t *testing.T) {
	s, core, _ := watchlistServer(t)
	raw := []byte(`{"version":1,"locks":null,"automatic_locks_initialized":true,"automatic_locks":[{"scope":"position","symbol":"SYNA","con_id":9001,"account_id":"synthetic","account_mode":"paper"}]}`)
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindHoldingLocks, JSON: raw}); err != nil {
		t.Fatal(err)
	}
	if len(s.automaticLockBlockers(t.Context(), "synthetic", "paper", "SYNA", 9001)) == 0 {
		t.Fatal("keep-list corruption bypassed the independent automatic exclusion")
	}
	if len(s.automaticLockBlockers(t.Context(), "synthetic", "paper", "SYNB", 9002)) != 0 {
		t.Fatal("keep-list corruption disabled an unlocked protective stop")
	}
	response := automaticLocksListResponse(t, s, `{"automatic_only":true}`)
	var out rpc.HoldingLocks
	if !response.Ok || json.Unmarshal(response.Result, &out) != nil || out.Version != 1 || out.Revision != 1 || !out.AutomaticLocksInitialized || !out.AutomaticMatch("synthetic", "paper", "SYNA", 9001) {
		t.Fatalf("keep-list corruption hid independent automatic authority: response=%+v result=%+v", response, out)
	}
	if response := automaticLocksListResponse(t, s, "{}"); response.Ok {
		t.Fatal("ordinary read accepted an unreadable keep list")
	}
	raw = []byte(`{"version":1,"locks":[],"automatic_locks_initialized":true,"automatic_locks":[{"scope":"invalid"}]}`)
	if _, err := core.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindHoldingLocks, ExpectedRevision: 1, JSON: raw}); err != nil {
		t.Fatal(err)
	}
	if got := s.automaticLockBlockers(t.Context(), "synthetic", "paper", "SYNB", 9002); len(got) == 0 || got[0].Code != "automatic_locks_unavailable" {
		t.Fatal("unreadable automatic exclusions did not fail closed")
	}
	if response := automaticLocksListResponse(t, s, `{"automatic_only":true}`); response.Ok {
		t.Fatal("automatic read accepted unreadable automatic exclusions")
	}
}

func automaticLockUpdate(t *testing.T, s *Server, revision int64, id string, in rpc.AutomaticLocksUpdate) *rpc.HoldingLocks {
	t.Helper()
	raw, _ := json.Marshal(rpc.HoldingLocksReplaceRequest{Automatic: &in, ExpectedRevision: revision, RequestID: id})
	out, err := s.handleHoldingLocksReplace(t.Context(), &rpc.Request{Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestArmedAutomaticLocksScopeReplayAndKeepList(t *testing.T) {
	s, _, _ := watchlistServer(t)
	lock := rpc.AutomaticLock{Scope: "position", Symbol: "SYNA", ConID: 9001, AccountID: "synthetic", AccountMode: "paper"}
	out := automaticLockUpdate(t, s, 0, "synthetic-lock", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
	for _, tc := range []struct {
		account, mode, symbol string
		conID                 int
		blocked               bool
	}{
		{"synthetic", "paper", "SYNA", 9001, true},
		{"synthetic", "paper", "SYNA", 9002, false},
		{"other", "paper", "SYNA", 9001, false},
		{"synthetic", "live", "SYNA", 9001, false},
	} {
		if got := out.AutomaticMatch(tc.account, tc.mode, tc.symbol, tc.conID); got != tc.blocked {
			t.Fatalf("scope %+v: %v", tc, got)
		}
	}
	if _, manual := out.Match("SYNA", 9001); manual {
		t.Fatal("automatic exclusion changed manual keep-list semantics")
	}
	// A lost acknowledgement is replayed even when the caller has re-read the
	// accepted revision; request identity still binds the exact delta intent.
	out = automaticLockUpdate(t, s, out.Revision, "synthetic-lock", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
	if !out.Replay || len(out.AutomaticLocks) != 1 {
		t.Fatal("lost acknowledgement duplicated or lost the lock")
	}
	out, err := replaceHoldingLocks(t, s, []rpc.HoldingLock{{Symbol: "SYNB", Role: rpc.HoldingLockRoleCore}}, out.Revision, "synthetic-keep")
	if err != nil || !out.AutomaticMatch("synthetic", "paper", "SYNA", 9001) {
		t.Fatalf("ordinary keep-list update erased automatic exclusions: %v", err)
	}
	out = automaticLockUpdate(t, s, out.Revision, "synthetic-unlock", rpc.AutomaticLocksUpdate{Action: "unlock", Lock: &lock})
	out = automaticLockUpdate(t, s, out.Revision, "synthetic-legacy-import", rpc.AutomaticLocksUpdate{Action: "initialize", Locks: []rpc.AutomaticLock{lock}})
	if len(out.AutomaticLocks) != 0 || len(out.Locks) != 1 {
		t.Fatal("legacy migration restored an unlocked holding or erased the keep list")
	}
	lock.Scope, lock.ConID = "underlying", 0
	out = automaticLockUpdate(t, s, out.Revision, "synthetic-underlying", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
	if !out.AutomaticMatch("synthetic", "paper", "SYNA", 9001) || !out.AutomaticMatch("synthetic", "paper", "SYNA", 9002) {
		t.Fatal("underlying lock missed a contract")
	}
}

func TestArmedAutomaticLockFenceCoversBothAutomaticPaths(t *testing.T) {
	s, _, _ := watchlistServer(t)
	lock := rpc.AutomaticLock{Scope: "position", Symbol: "SYNA", ConID: 9001, AccountID: "synthetic", AccountMode: "paper"}
	automaticLockUpdate(t, s, 0, "wire-lock", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
	for _, origin := range []string{rpc.OrderOriginDaemonPreAuthorised, rpc.OrderOriginDaemonProtectiveGuard, rpc.OrderOriginDeskAutomatic, rpc.OrderOriginDaemonOwnerQueued, rpc.OrderOriginHumanTTY} {
		binding := brokerWriteTransactionBinding{origin: origin, scope: brokerStateScope{Account: "synthetic", Mode: "paper"}, riskBound: true, riskDraft: rpc.OrderDraft{Contract: rpc.ContractParams{Symbol: "SYNA", ConID: 9001}}}
		blocked := len(s.automaticWireLockBlockers(t.Context(), binding, false)) > 0
		want := origin != rpc.OrderOriginDaemonOwnerQueued && origin != rpc.OrderOriginHumanTTY
		if blocked != want {
			t.Fatalf("origin %s blocked=%v", origin, blocked)
		}
		if len(s.automaticWireLockBlockers(t.Context(), binding, true)) != 0 {
			t.Fatal("automatic lock blocked cancellation")
		}
	}
}

func TestArmedAutomaticLockAcknowledgementOrdersAfterInFlightWrite(t *testing.T) {
	s, _, _ := watchlistServer(t)
	s.brokerWriteMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.brokerWriteMu.Unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := s.handleHoldingLocksReplace(t.Context(), &rpc.Request{Params: []byte(`{"automatic":{"action":"lock","lock":{"scope":"position","symbol":"SYNA","con_id":9001,"account_id":"synthetic","account_mode":"paper"}},"expected_revision":0,"request_id":"write-in-flight"}`)})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("acknowledged before write completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	s.brokerWriteMu.Unlock()
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lock did not finish")
	}
	if got := s.automaticLockBlockers(t.Context(), "synthetic", "paper", "SYNA", 9001); len(got) != 1 || !strings.Contains(got[0].Code, "locked") {
		t.Fatal("next automatic write was not fenced")
	}
}

func TestArmedAutomaticLockPreservesExistingStopSafetyShrink(t *testing.T) {
	s, _, _ := watchlistServer(t)
	lock := rpc.AutomaticLock{Scope: "position", Symbol: "SYNA", ConID: 9001, AccountID: "synthetic", AccountMode: "paper"}
	automaticLockUpdate(t, s, 0, "shrink-lock", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
	binding := brokerWriteTransactionBinding{origin: rpc.OrderOriginDaemonProtectiveGuard, scope: brokerStateScope{Account: "synthetic", Mode: "paper"}, riskBound: true, orderID: 42, orderEndpoint: "synthetic", riskDraft: rpc.OrderDraft{OrderRef: "synthetic-stop", Contract: rpc.ContractParams{Symbol: "SYNA", ConID: 9001}, Quantity: 20}}
	for _, tc := range []struct {
		name    string
		grant   protectiveStopGuardWriteGrant
		blocked bool
	}{
		{"strict shrink", protectiveStopGuardWriteGrant{Kind: protectiveStopGuardShrink, OrderRef: "synthetic-stop", ReservedOrderID: 42, Quantity: 20, Remaining: 40}, false},
		{"full-position shrink", protectiveStopGuardWriteGrant{Kind: protectiveStopGuardResize, OrderRef: "synthetic-stop", ReservedOrderID: 42, Quantity: 20, Remaining: 40}, false},
		{"same quantity", protectiveStopGuardWriteGrant{Kind: protectiveStopGuardResize, OrderRef: "synthetic-stop", ReservedOrderID: 42, Quantity: 20, Remaining: 20}, true},
		{"growth", protectiveStopGuardWriteGrant{Kind: protectiveStopGuardResize, OrderRef: "synthetic-stop", ReservedOrderID: 42, Quantity: 20, Remaining: 10}, true},
		{"missing remaining", protectiveStopGuardWriteGrant{Kind: protectiveStopGuardShrink, OrderRef: "synthetic-stop", ReservedOrderID: 42, Quantity: 20}, true},
		{"different order", protectiveStopGuardWriteGrant{Kind: protectiveStopGuardShrink, OrderRef: "other-stop", ReservedOrderID: 42, Quantity: 20, Remaining: 40}, true},
		{"different broker id", protectiveStopGuardWriteGrant{Kind: protectiveStopGuardShrink, OrderRef: "synthetic-stop", ReservedOrderID: 43, Quantity: 20, Remaining: 40}, true},
		{"different quantity", protectiveStopGuardWriteGrant{Kind: protectiveStopGuardShrink, OrderRef: "synthetic-stop", ReservedOrderID: 42, Quantity: 21, Remaining: 40}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.protectiveStopGuardGrant.Store(&tc.grant)
			if got := len(s.automaticWireLockBlockers(t.Context(), binding, false)) > 0; got != tc.blocked {
				t.Fatalf("automatic exclusion=%v, want %v", got, tc.blocked)
			}
		})
	}
}

func TestArmedAutomaticLockAllowsPlannedFullPositionReduction(t *testing.T) {
	s, _, _ := watchlistServer(t)
	scope := brokerStateScope{Account: "synthetic", Mode: "paper"}
	lock := rpc.AutomaticLock{Scope: "position", Symbol: "SYNA", ConID: 9001, AccountID: scope.Account, AccountMode: scope.Mode}
	automaticLockUpdate(t, s, 0, "full-position-shrink-lock", rpc.AutomaticLocksUpdate{Action: "lock", Lock: &lock})
	view := rpc.OrderView{OrderRef: "synthetic-stop", PreviewTokenID: "synthetic-signed", ReservedOrderID: 42, Account: scope.Account, Mode: scope.Mode,
		Open: true, ConID: 9001, Symbol: "SYNA", SecType: "STK", Currency: "USD", Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL,
		Quantity: 40, Remaining: 40, TIF: rpc.OrderTIFGTC, Trail: &rpc.OrderTrailSpec{TrailingPercent: new(2.0), InitialStopPrice: 95}}
	order := ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, ConID: 9001, Symbol: "SYNA", SecType: "STK", Currency: "USD",
		Action: rpc.OrderActionSell, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC, TrailingPercent: 2, TotalQuantity: 40, Remaining: 40,
		TrailStopPrice: 98, Status: "Submitted"}
	positions := []*ibkrlib.RawPosition{{Account: scope.Account, Position: 20, Contract: ibkrlib.Contract{ConID: 9001, Symbol: "SYNA", SecType: "STK", Currency: "USD"}}}
	steps := planProtectiveStopGuard([]brokerWorkingOrder{{Order: order, Journal: &view}}, positions, scope, map[string]bool{view.OrderRef: true})
	if len(steps) != 1 || steps[0].Kind != protectiveStopGuardResize || steps[0].Target != 20 {
		t.Fatalf("full-position safety fixture: %+v", steps)
	}
	step := steps[0]
	s.protectiveStopGuardGrant.Store(&protectiveStopGuardWriteGrant{Kind: step.Kind, OrderRef: step.View.OrderRef, ReservedOrderID: step.View.ReservedOrderID,
		Quantity: step.Target, Filled: step.View.Filled, Position: step.Position, Remaining: step.Remaining})
	binding := brokerWriteTransactionBinding{origin: rpc.OrderOriginDaemonProtectiveGuard, scope: scope, riskBound: true, orderID: 42,
		orderEndpoint: "synthetic", riskDraft: rpc.OrderDraft{OrderRef: view.OrderRef, Contract: rpc.ContractParams{Symbol: "SYNA", ConID: 9001}, Quantity: 20}}
	if blockers := s.automaticWireLockBlockers(t.Context(), binding, false); len(blockers) != 0 {
		t.Fatalf("locked full-position stop cannot safely follow holding40->20: %+v", blockers)
	}
}
