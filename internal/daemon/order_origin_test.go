package daemon

import (
	"context"
	"encoding/json"
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

// originTestOrder is the journal identity shared by one seeded order.
func originTestOrder(ref string, reservedID int, at time.Time) orderJournalEvent {
	return orderJournalEvent{
		At: at, OrderRef: ref, ReservedOrderID: reservedID, ClientID: 15,
		Account: "DU1234567", Endpoint: "127.0.0.1:4001", Mode: "paper",
		Symbol: "SYN", SecType: "STK", ConID: 101, Action: rpc.OrderActionSell,
		OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFGTC, Quantity: 10, LimitPrice: 24,
	}
}

// seedOriginOrder journals one order the way the write path does: the place
// attempt carries the request origin, the broker acknowledgement none.
func seedOriginOrder(t *testing.T, srv *Server, ref string, reservedID, permID int, origin string, at time.Time) {
	t.Helper()
	attempt := originTestOrder(ref, reservedID, at)
	attempt.Type = orderJournalEventSendAttempted
	attempt.AttemptID = "attempt-" + ref
	attempt.ActionKind = corestore.ActionPlace
	attempt.SendState = orderSendStateSendAttempted
	attempt.Origin = origin
	ack := originTestOrder(ref, reservedID, at.Add(time.Second))
	ack.Type = orderJournalEventBrokerAcknowledged
	ack.PermID = permID
	ack.Status = "Submitted"
	ack.Remaining = 10
	ack.SendState = orderSendStateBrokerAcknowledged
	if err := srv.orderJournal.AppendAll([]orderJournalEvent{attempt, ack}); err != nil {
		t.Fatalf("seed journaled order %s: %v", ref, err)
	}
}

var originTestScope = brokerStateScope{Account: "DU1234567", Mode: "paper"}

func originTestInventory(asOf time.Time, orders ...ibkrlib.OrderLifecycleEvent) func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
	return func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{Complete: true, AsOf: asOf, Orders: orders}, originTestScope, nil
	}
}

// C0: a journaled order reports the origin of the request that placed it,
// on the open-orders RPC, history and status alike; an order Canary never
// placed reports none, whether it is a hand order still working in TWS or a
// fill the lifecycle journal passed through.
func TestOrdersReportTheJournaledOriginAndNoneForAHandOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	srv := newOrderReconcileTestServer(t, now)
	seedOriginOrder(t, srv, "canary-agent", 101, 9001, rpc.OrderOriginAgent, now.Add(-time.Hour))
	seedOriginOrder(t, srv, "canary-daemon", 102, 9002, rpc.OrderOriginDaemonPreAuthorised, now.Add(-time.Hour))
	seedOriginOrder(t, srv, "canary-legacy", 103, 9003, "robot", now.Add(-time.Hour))

	// A modify from a terminal keeps its own origin on its event and never
	// rewrites who placed the order.
	modify := originTestOrder("canary-agent", 101, now.Add(-30*time.Minute))
	modify.Type = orderJournalEventModifyRequested
	modify.PermID = 9001
	modify.AttemptID = "modify-canary-agent"
	modify.ActionKind = corestore.ActionModify
	modify.Origin = rpc.OrderOriginHumanTTY
	modify.LimitPrice = 23
	modifyAck := originTestOrder("canary-agent", 101, now.Add(-29*time.Minute))
	modifyAck.Type = orderJournalEventBrokerAcknowledged
	modifyAck.PermID = 9001
	modifyAck.LimitPrice = 23
	modifyAck.Status = "Submitted"
	modifyAck.Remaining = 10
	modifyAck.SendState = orderSendStateBrokerAcknowledged
	// A fill of an order placed by hand in TWS reaches the journal only as a
	// passed-through execution callback: no request, so no origin.
	handFill := orderJournalEvent{
		At: now.Add(-10 * time.Minute), Type: orderJournalEventStatusUpdated, PermID: 7001, ReservedOrderID: 0,
		ClientID: 15, Account: "DU1234567", Endpoint: "127.0.0.1:4001", Mode: "paper",
		Symbol: "HND", SecType: "STK", Action: rpc.OrderActionBuy, Quantity: 5, Status: "Filled", Filled: 5,
		SendState: orderSendStateTerminal,
	}
	if err := srv.orderJournal.AppendAll([]orderJournalEvent{modify, modifyAck, handFill}); err != nil {
		t.Fatalf("seed modify and hand fill: %v", err)
	}

	// The broker inventory holds the journaled orders plus a hand order that
	// is still working in TWS, one already filled and a what-if probe.
	srv.openOrderInventoryForTest = originTestInventory(now,
		ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 101, PermID: 9001, ClientID: 15, ClientIDPresent: true, Account: "DU1234567", Status: "Submitted", TotalQuantity: 10},
		ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 102, PermID: 9002, ClientID: 15, ClientIDPresent: true, Account: "DU1234567", Status: "Submitted", TotalQuantity: 10},
		ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, OrderID: 103, PermID: 9003, ClientID: 15, ClientIDPresent: true, Account: "DU1234567", Status: "Submitted", TotalQuantity: 10},
		ibkrlib.OrderLifecycleEvent{
			Type: ibkrlib.OrderLifecycleEventOpenOrder, PermID: 7777, ClientID: 0, ClientIDPresent: true, Account: "DU1234567",
			Symbol: "HND", SecType: "STK", ConID: 202, Action: rpc.OrderActionBuy, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
			TotalQuantity: 5, LimitPrice: 12.5, Status: "Submitted", OrderRef: "SYSTEM: transmit the order", WhyHeld: "locate",
		},
		ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, PermID: 7778, Account: "DU1234567", Status: "Filled", TotalQuantity: 5, Filled: 5},
		ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, PermID: 7779, Account: "DU1234567", Status: "PreSubmitted", TotalQuantity: 5, WhatIf: true},
		ibkrlib.OrderLifecycleEvent{Type: ibkrlib.OrderLifecycleEventOpenOrder, PermID: 7780, Account: "DU7654321", Status: "Submitted", TotalQuantity: 5},
	)

	open, err := srv.handleOrdersOpen(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatalf("orders.open: %v", err)
	}
	origins := map[string]string{}
	for _, order := range open.Orders {
		origins[order.OrderRef] = order.Origin
	}
	want := map[string]string{"canary-agent": rpc.OrderOriginAgent, "canary-daemon": rpc.OrderOriginDaemonPreAuthorised, "canary-legacy": ""}
	for ref, origin := range want {
		got, ok := origins[ref]
		if !ok || got != origin {
			t.Fatalf("open order %s origin = %q (present %v), want %q; rows %+v", ref, got, ok, origin, origins)
		}
	}
	if open.UntrackedStatus != rpc.OrdersUntrackedCurrent || !open.UntrackedAsOf.Equal(now) {
		t.Fatalf("untracked status = %q as of %s, want current as of %s", open.UntrackedStatus, open.UntrackedAsOf, now)
	}
	if len(open.Untracked) != 1 {
		t.Fatalf("untracked = %+v, want only the working hand order", open.Untracked)
	}
	hand := open.Untracked[0]
	if hand.PermID != 7777 || hand.Origin != "" || !hand.Open || hand.ModifyEligible || hand.CancelEligible ||
		hand.Symbol != "HND" || hand.Quantity != 5 || hand.LifecycleStatus != rpc.OrderLifecycleSubmitted {
		t.Fatalf("untracked hand order = %+v", hand)
	}
	raw, err := json.Marshal(open)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "SYSTEM") || strings.Contains(string(raw), "locate") {
		t.Fatalf("untracked row carried broker free text: %s", raw)
	}
	var wire struct {
		Untracked []map[string]any `json:"untracked"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil || len(wire.Untracked) != 1 {
		t.Fatalf("decode untracked: %v %s", err, raw)
	}
	if _, ok := wire.Untracked[0]["origin"]; ok {
		t.Fatalf("hand order reports an origin on the wire: %v", wire.Untracked[0])
	}

	// History and status carry the same order origin, and each event keeps
	// the origin of the request that journaled it.
	history, err := srv.handleOrdersHistory(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatalf("orders.history: %v", err)
	}
	sawHandFill := false
	for _, row := range history.Orders {
		switch row.Order.OrderRef {
		case "canary-agent":
			if row.Order.Origin != rpc.OrderOriginAgent {
				t.Fatalf("history order origin = %q", row.Order.Origin)
			}
			eventOrigins := map[string]string{}
			for _, ev := range row.Events {
				eventOrigins[ev.Type] = ev.Origin
			}
			if eventOrigins[orderJournalEventSendAttempted] != rpc.OrderOriginAgent || eventOrigins[orderJournalEventModifyRequested] != rpc.OrderOriginHumanTTY || eventOrigins[orderJournalEventBrokerAcknowledged] != "" {
				t.Fatalf("event origins = %+v", eventOrigins)
			}
		case "":
			if row.Order.PermID == 7001 {
				sawHandFill = true
				if row.Order.Origin != "" {
					t.Fatalf("hand fill reports origin %q", row.Order.Origin)
				}
			}
		}
	}
	if !sawHandFill {
		t.Fatalf("history lacks the passed-through hand fill: %+v", history.Orders)
	}
	status, err := srv.handleOrderStatus(context.Background(), &rpc.Request{Params: []byte(`{"id":"canary-daemon"}`)})
	if err != nil || !status.Found || status.Order.Origin != rpc.OrderOriginDaemonPreAuthorised {
		t.Fatalf("order status = %+v err = %v", status, err)
	}

	// No complete inventory: the untracked list is unavailable, never an
	// empty all-clear, and the journaled rows are unaffected.
	srv.openOrderInventoryForTest = func(context.Context, bool) (ibkrlib.OpenOrderSnapshot, brokerStateScope, error) {
		return ibkrlib.OpenOrderSnapshot{}, originTestScope, errors.New("gateway down")
	}
	unavailable, err := srv.handleOrdersOpen(context.Background(), &rpc.Request{})
	if err != nil {
		t.Fatalf("orders.open without inventory: %v", err)
	}
	if unavailable.UntrackedStatus != rpc.OrdersUntrackedUnavailable || len(unavailable.Untracked) != 0 || len(unavailable.Orders) != len(open.Orders) {
		t.Fatalf("orders.open without inventory = status %q untracked %d orders %d", unavailable.UntrackedStatus, len(unavailable.Untracked), len(unavailable.Orders))
	}
}

// C0: trading status reports trading.freeze and the platform-settings
// trading-control generation in every mode, including disabled, where the
// status otherwise returns before any write gate is evaluated.
func TestTradingStatusReportsFreezeAndControlGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mode := range []string{config.TradingModeDisabled, config.TradingModePaper} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			srv := newTestServer(t)
			srv.cfg.Gateway.Account = "DU1234567"
			srv.cfg.Trading.Mode = mode
			store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
			if err != nil {
				t.Fatal(err)
			}
			srv.platformSettings = store
			if st := srv.handleTradingStatus(); st.Freeze || st.TradingControlGeneration != 0 {
				t.Fatalf("fresh status freeze=%v generation=%d", st.Freeze, st.TradingControlGeneration)
			}
			set := func(key string, mutate func(*platformSettingsData)) {
				t.Helper()
				if err := store.updateWithAudit(ctx, time.Now(), rpc.OrderOriginHumanTTY, []string{key}, func(d *platformSettingsData) error {
					mutate(d)
					return nil
				}); err != nil {
					t.Fatalf("set %s: %v", key, err)
				}
			}
			set("trading.freeze", func(d *platformSettingsData) { d.Trading.Freeze = new(true) })
			frozen := srv.handleTradingStatus()
			if !frozen.Freeze || frozen.TradingControlGeneration != 1 {
				t.Fatalf("frozen status freeze=%v generation=%d", frozen.Freeze, frozen.TradingControlGeneration)
			}
			if mode == config.TradingModePaper && frozen.CanWrite {
				t.Fatal("a frozen paper desk reports can_write")
			}
			raw, err := json.Marshal(frozen)
			if err != nil || !strings.Contains(string(raw), `"freeze":true`) || !strings.Contains(string(raw), `"trading_control_generation":1`) {
				t.Fatalf("trading status wire = %s err = %v", raw, err)
			}
			// Lifting the freeze reads unfrozen again, but the generation shows
			// that it moved; a setting outside the trading controls does not.
			set("trading.freeze", func(d *platformSettingsData) { d.Trading.Freeze = new(false) })
			set("display.date_format", func(d *platformSettingsData) { d.Display.DateFormat = new(rpc.DisplayDateFormatEU) })
			lifted := srv.handleTradingStatus()
			if lifted.Freeze || lifted.TradingControlGeneration != 2 {
				t.Fatalf("lifted status freeze=%v generation=%d", lifted.Freeze, lifted.TradingControlGeneration)
			}
		})
	}
}

// C0: the brief's drift row names the risk constitution in force, so a
// reader comparing two briefs sees a policy_version bump.
func TestBriefPolicyDriftRowNamesTheConstitutionVersion(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 25, 14, 0, 0, 0, time.UTC)
	active := composeBriefRisk(&rpc.RiskPolicyResult{Status: rpc.RiskPolicyStatusActive, PolicyID: "risk-constitution", PolicyVersion: 4}, nil, now)
	if active.PolicyDrift.PolicyID != "risk-constitution" || active.PolicyDrift.PolicyVersion != 4 {
		t.Fatalf("drift row = %+v", active.PolicyDrift)
	}
	absent := composeBriefRisk(&rpc.RiskPolicyResult{Status: rpc.RiskPolicyStatusAbsent}, nil, now)
	if absent.PolicyDrift.PolicyID != "" || absent.PolicyDrift.PolicyVersion != 0 {
		t.Fatalf("absent drift row = %+v", absent.PolicyDrift)
	}
	ready := composeBriefReady(rpc.BriefMarketSection{}, rpc.BriefCalendarSection{}, active, rpc.BriefPortfolioSection{}, rpc.BriefProcessSection{}, rpc.BriefReadyProposalsRow{})
	if ready.PolicyDrift.PolicyVersion != 4 {
		t.Fatalf("ready drift row = %+v", ready.PolicyDrift)
	}
}
