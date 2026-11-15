//go:build trading

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// twsControlRig is a paper daemon with the control enabled, one manual TWS
// order working at the broker, the ordinary cancel path fenced off, and the
// client-0 run replaced by a seam that records what it was asked.
type twsControlRig struct {
	srv           *Server
	runs          []twsControlRun
	ordinary      int
	receipt       ibkrlib.TWSOrderControlReceipt
	runErr        error
	inventoryDown bool
}

type twsControlRun struct {
	cfg         ibkrlib.TWSOrderControlConfig
	action      string
	target      ibkrlib.TWSOrderControlTarget
	replacement ibkrlib.TWSOrderControlReplacement
}

func newTWSControlRig(t *testing.T) *twsControlRig {
	t.Helper()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.platformSettings = store
	if err := enableTWSOrderControl(t, srv, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatalf("enable: %v", err)
	}
	rig := &twsControlRig{srv: srv, receipt: ibkrlib.TWSOrderControlReceipt{Outcome: ibkrlib.TWSOrderControlConfirmed, BoundOrderID: 7, BrokerStatus: "Cancelled"}}
	srv.orderCancelBroker = func(context.Context, int) error {
		rig.ordinary++
		return errors.New("the ordinary cancel path must not carry a TWS order")
	}
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
		if rig.inventoryDown {
			return ibkrlib.OpenOrderSnapshot{}, scope, errBrokerOpenOrderInventoryUnavailable
		}
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: srv.orderNow(), Orders: []ibkrlib.OrderLifecycleEvent{{
			Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 0, PermID: 31, ClientID: 0, ClientIDPresent: true, Status: "Submitted",
			Symbol: "AAPL", SecType: "STK", ConID: 265598, Exchange: "SMART", Currency: "USD", LocalSymbol: "AAPL", TradingClass: "NMS",
			Action: rpc.OrderActionBuy, TotalQuantity: 100, OrderType: rpc.OrderTypeLMT, LimitPrice: 150, TIF: rpc.OrderTIFDay, Account: "DU1234567",
		}}}, scope, nil
	}
	srv.twsOrderControlRun = func(_ context.Context, cfg ibkrlib.TWSOrderControlConfig, action string, target ibkrlib.TWSOrderControlTarget, replacement ibkrlib.TWSOrderControlReplacement) (ibkrlib.TWSOrderControlReceipt, error) {
		rig.runs = append(rig.runs, twsControlRun{cfg, action, target, replacement})
		receipt := rig.receipt
		receipt.Action, receipt.PermID = action, target.PermID
		receipt.Bound = ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: receipt.BoundOrderID, PermID: target.PermID, ClientIDPresent: true,
			Symbol: target.Symbol, SecType: target.SecType, Action: target.Action, TotalQuantity: target.Quantity, OrderType: target.OrderType, LimitPrice: target.LimitPrice, TIF: target.TIF, Status: receipt.BrokerStatus}
		return receipt, rig.runErr
	}
	return rig
}

func twsControlTestParams(requestID string) rpc.OrderTWSControlParams {
	return rpc.OrderTWSControlParams{RequestID: requestID, PermID: 31, Origin: rpc.OrderOriginPairedDevice,
		Terms: rpc.OrderTWSControlTerms{ConID: 265598, Symbol: "AAPL", SecType: "STK", Action: rpc.OrderActionBuy, Quantity: 100, OrderType: rpc.OrderTypeLMT, LimitPrice: 150, TIF: rpc.OrderTIFDay}}
}

// A confirmed cancel: the gates pass, the inventory row matches the reviewed
// terms, the client-0 run gets the daemon's endpoint and account and the
// order's exact terms, both journal rows exist, the paper proof is recorded,
// and the ordinary cancel path is never touched.
func TestTWSOrderControlCancelRunsOnceAndRecordsTheProof(t *testing.T) {
	rig := newTWSControlRig(t)
	res, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlCancel, twsControlTestParams("desk-1"))
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !res.Accepted || res.Outcome != rpc.TWSOrderControlOutcomeConfirmed || res.BoundOrderID != 7 || res.ClientID != 0 || !res.BoundFromTWS ||
		res.Origin != rpc.OrderOriginPairedDevice || res.Mode != rpc.AccountModePaper || res.Account != "DU1234567" || res.Order == nil || res.Order.PermID != 31 {
		t.Fatalf("result = %+v, want a confirmed client-0 cancel of perm 31", res)
	}
	if res.PaperProvenAt.IsZero() || !strings.Contains(res.Message, "confirmed the cancel") {
		t.Fatalf("result proof=%v message=%q, want the paper proof and a confirmation", res.PaperProvenAt, res.Message)
	}
	if len(rig.runs) != 1 || rig.ordinary != 0 {
		t.Fatalf("runs=%d ordinary cancels=%d, want one control run and no ordinary cancel", len(rig.runs), rig.ordinary)
	}
	run := rig.runs[0]
	if run.cfg.Host != "127.0.0.1" || run.cfg.Port != 4002 || run.cfg.Mode != rpc.AccountModePaper || run.cfg.Account != "DU1234567" || run.action != ibkrlib.TWSOrderControlCancel {
		t.Fatalf("run cfg=%+v action=%s, want the daemon's paper endpoint and account", run.cfg, run.action)
	}
	if run.target.PermID != 31 || run.target.ConID != 265598 || run.target.Symbol != "AAPL" || run.target.Quantity != 100 || run.target.LimitPrice != 150 || run.target.TIF != rpc.OrderTIFDay {
		t.Fatalf("run target = %+v, want the inventory row's terms", run.target)
	}
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	var request twsOrderControlRequestRow
	if ok, err := rig.srv.twsOrderControlEvent(context.Background(), twsOrderControlKey(scope, "request", "desk-1"), &request); err != nil || !ok ||
		request.Bound != twsOrderControlJournalOrigin || request.Origin != rpc.OrderOriginPairedDevice || request.Digest != res.Digest {
		t.Fatalf("request row ok=%t err=%v row=%+v, want bound_from_tws under the paired-device origin", ok, err, request)
	}
	var outcome twsOrderControlOutcomeRow
	if ok, err := rig.srv.twsOrderControlEvent(context.Background(), twsOrderControlKey(scope, "outcome", "desk-1"), &outcome); err != nil || !ok || outcome.Outcome != rpc.TWSOrderControlOutcomeConfirmed || outcome.BoundOrderID != 7 {
		t.Fatalf("outcome row ok=%t err=%v row=%+v, want confirmed on bound order 7", ok, err, outcome)
	}
	if avail := rig.srv.twsOrderControlAvailability(rpc.AccountModeLive); !avail.Available || avail.PaperProvenAt.IsZero() {
		t.Fatalf("live availability after the paper proof = %+v, want available", avail)
	}
}

// A change carries the replacement to the run and is journaled with it.
func TestTWSOrderControlModifyCarriesTheReplacement(t *testing.T) {
	rig := newTWSControlRig(t)
	rig.receipt.BrokerStatus = "Submitted"
	p := twsControlTestParams("desk-2")
	p.Replacement = &rpc.OrderTWSControlReplacement{Quantity: 60, LimitPrice: 149.5}
	res, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlModify, p)
	if err != nil || !res.Accepted || res.Outcome != rpc.TWSOrderControlOutcomeConfirmed || res.Action != ibkrlib.TWSOrderControlModify {
		t.Fatalf("modify res=%+v err=%v, want a confirmed change", res, err)
	}
	if len(rig.runs) != 1 || rig.runs[0].replacement != (ibkrlib.TWSOrderControlReplacement{Quantity: 60, LimitPrice: 149.5}) {
		t.Fatalf("runs = %+v, want one modify with qty 60 at 149.5", rig.runs)
	}
}

// A repeat of the same request answers from the journal and runs nothing;
// the same request id with another intent is refused.
func TestTWSOrderControlReplayAnswersFromTheJournal(t *testing.T) {
	rig := newTWSControlRig(t)
	first, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlCancel, twsControlTestParams("desk-3"))
	if err != nil || !first.Accepted {
		t.Fatalf("first: res=%+v err=%v", first, err)
	}
	again, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlCancel, twsControlTestParams("desk-3"))
	if err != nil || !again.Accepted || again.Outcome != first.Outcome || again.BoundOrderID != first.BoundOrderID || again.Digest != first.Digest {
		t.Fatalf("replay res=%+v err=%v, want the first outcome %+v", again, err, first)
	}
	if len(rig.runs) != 1 {
		t.Fatalf("runs = %d after a replay, want one", len(rig.runs))
	}
	other := twsControlTestParams("desk-3")
	other.Terms.LimitPrice = 151
	conflict, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlCancel, other)
	if err != nil || conflict.Accepted || conflict.Outcome != rpc.TWSOrderControlOutcomeRefused || len(conflict.Blockers) == 0 || conflict.Blockers[0].Code != rpc.TWSOrderControlBlockerRequestReplay {
		t.Fatalf("conflict res=%+v err=%v, want a request-conflict refusal", conflict, err)
	}
	if len(rig.runs) != 1 {
		t.Fatalf("runs = %d after a conflicting replay, want one", len(rig.runs))
	}
}

// An uncertain run is reported uncertain, journaled as such, and a repeat
// says so again without a second run; no paper proof is recorded.
func TestTWSOrderControlUncertainIsNeverRetried(t *testing.T) {
	rig := newTWSControlRig(t)
	rig.receipt = ibkrlib.TWSOrderControlReceipt{Outcome: ibkrlib.TWSOrderControlUncertain, BoundOrderID: 7}
	rig.runErr = ibkrlib.ErrTWSOrderControlUncertain
	res, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlCancel, twsControlTestParams("desk-4"))
	if err != nil || res.Accepted || res.Outcome != rpc.TWSOrderControlOutcomeUncertain || res.BoundOrderID != 7 || len(res.Blockers) == 0 || res.Blockers[0].Code != rpc.TWSOrderControlBlockerUncertain || !res.PaperProvenAt.IsZero() {
		t.Fatalf("res=%+v err=%v, want an uncertain outcome without a proof", res, err)
	}
	again, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlCancel, twsControlTestParams("desk-4"))
	if err != nil || again.Outcome != rpc.TWSOrderControlOutcomeUncertain || len(rig.runs) != 1 {
		t.Fatalf("replay res=%+v err=%v runs=%d, want uncertain again without a run", again, err, len(rig.runs))
	}
	if avail := rig.srv.twsOrderControlAvailability(rpc.AccountModeLive); avail.Available {
		t.Fatalf("live availability after an uncertain paper outcome = %+v, want proof still required", avail)
	}
}

// Refusals before the run: the control off, a broker refusal, an inventory
// that cannot be read, an order that changed since review, and an order
// that is not there. None runs, none journals a proof.
func TestTWSOrderControlRefusesBeforeTheRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*twsControlRig, *rpc.OrderTWSControlParams)
		code    string
		runs    int
	}{
		{"control off", func(rig *twsControlRig, _ *rpc.OrderTWSControlParams) {
			patch := map[string]json.RawMessage{"trading": json.RawMessage(`{"tws_order_control":{"enabled":false}}`)}
			if err := rig.srv.applyPlatformSettingsPatch(context.Background(), patch, rpc.OrderOriginHumanTTY); err != nil {
				t.Fatal(err)
			}
		}, rpc.TWSOrderControlBlockerOff, 0},
		{"inventory unavailable", func(rig *twsControlRig, _ *rpc.OrderTWSControlParams) { rig.inventoryDown = true }, rpc.TWSOrderControlBlockerInventory, 0},
		{"terms changed", func(_ *twsControlRig, p *rpc.OrderTWSControlParams) { p.Terms.LimitPrice = 151 }, rpc.TWSOrderControlBlockerChanged, 0},
		{"not found", func(_ *twsControlRig, p *rpc.OrderTWSControlParams) { p.PermID = 99 }, rpc.TWSOrderControlBlockerNotFound, 0},
		{"broker refused", func(rig *twsControlRig, _ *rpc.OrderTWSControlParams) {
			rig.receipt = ibkrlib.TWSOrderControlReceipt{Outcome: ibkrlib.TWSOrderControlRefused, BoundOrderID: 7, BrokerCode: 161}
			rig.runErr = ibkrlib.ErrTWSOrderControlRefused
		}, rpc.TWSOrderControlBlockerBrokerRefused, 1},
		{"slot busy", func(rig *twsControlRig, _ *rpc.OrderTWSControlParams) {
			rig.receipt = ibkrlib.TWSOrderControlReceipt{Outcome: ibkrlib.TWSOrderControlRefused}
			rig.runErr = ibkrlib.ErrTWSOrderControlSlotBusy
		}, rpc.TWSOrderControlBlockerSlotBusy, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newTWSControlRig(t)
			p := twsControlTestParams("desk-" + tc.name)
			tc.arrange(rig, &p)
			res, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlCancel, p)
			if err != nil || res.Accepted || res.Outcome != rpc.TWSOrderControlOutcomeRefused {
				t.Fatalf("res=%+v err=%v, want a refusal", res, err)
			}
			if len(res.Blockers) == 0 || res.Blockers[0].Code != tc.code {
				t.Fatalf("blockers = %+v, want %s first", res.Blockers, tc.code)
			}
			if len(rig.runs) != tc.runs || rig.ordinary != 0 || !res.PaperProvenAt.IsZero() {
				t.Fatalf("runs=%d ordinary=%d proof=%v, want %d runs, no ordinary cancel, no proof", len(rig.runs), rig.ordinary, res.PaperProvenAt, tc.runs)
			}
			if avail := rig.srv.twsOrderControlAvailability(rpc.AccountModeLive); avail.Available {
				t.Fatalf("live availability after a refusal = %+v, want proof still required", avail)
			}
		})
	}
}

// The freeze blocks a change and lets a cancel through, as on the ordinary
// path.
func TestTWSOrderControlHonoursTheFreeze(t *testing.T) {
	rig := newTWSControlRig(t)
	patch := map[string]json.RawMessage{"trading": json.RawMessage(`{"freeze":true}`)}
	if err := rig.srv.applyPlatformSettingsPatch(context.Background(), patch, rpc.OrderOriginHumanTTY); err != nil {
		t.Fatal(err)
	}
	p := twsControlTestParams("desk-frozen-modify")
	p.Replacement = &rpc.OrderTWSControlReplacement{Quantity: 60, LimitPrice: 149.5}
	res, err := rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlModify, p)
	if err != nil || res.Accepted || len(rig.runs) != 0 || !strings.Contains(strings.ToLower(res.Message), "frozen") && !strings.Contains(strings.ToLower(res.Message), "freeze") {
		t.Fatalf("frozen modify res=%+v err=%v runs=%d, want a freeze refusal without a run", res, err, len(rig.runs))
	}
	res, err = rig.srv.twsOrderControl(context.Background(), ibkrlib.TWSOrderControlCancel, twsControlTestParams("desk-frozen-cancel"))
	if err != nil || !res.Accepted || len(rig.runs) != 1 {
		t.Fatalf("frozen cancel res=%+v err=%v runs=%d, want the cancel to run", res, err, len(rig.runs))
	}
}

// Untracked rows in orders.open carry the eligibility and the control status.
func TestTWSOrderControlOrdersOpenShowsEligibility(t *testing.T) {
	rig := newTWSControlRig(t)
	res, err := rig.srv.handleOrdersOpen(context.Background(), &rpc.Request{Params: []byte(`{}`)})
	if err != nil {
		t.Fatalf("orders open: %v", err)
	}
	if res.UntrackedControl != rpc.TWSOrderControlStatusAvailable || len(res.Untracked) != 1 || !res.Untracked[0].CancelEligible || !res.Untracked[0].ModifyEligible {
		t.Fatalf("orders open = control %q untracked %+v, want an eligible row under an available control", res.UntrackedControl, res.Untracked)
	}
}
